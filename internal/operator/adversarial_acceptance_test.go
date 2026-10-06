package operator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"golang.org/x/sys/unix"
)

const acceptanceProfile = `{"version":1,"statePath":"/var/state.db","activatedRelease":"v1","systemctlPath":"/usr/bin/systemctl","nvidiaSMIPath":"/usr/bin/nvidia-smi","gpuIndex":0}`

func TestAcceptanceProfileExactSchema(t *testing.T) {
	for name, body := range map[string]string{
		"case variant":      strings.Replace(acceptanceProfile, `"version"`, `"Version"`, 1),
		"case duplicate":    strings.Replace(acceptanceProfile, `"version":1`, `"version":2,"Version":1`, 1),
		"trailing document": acceptanceProfile + ` {}`,
		"escaped duplicate": strings.Replace(acceptanceProfile, `"version":1`, `"version":1,"\u0076ersion":1`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".config", "gpu-workload-supervisor")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "operator.json"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadProfileHome(t, home, os.Geteuid()); err == nil {
				t.Fatal("ambiguous profile schema accepted")
			}
		})
	}
}

func TestAcceptanceProfileSpecialFilesAndAncestorLinks(t *testing.T) {
	for _, kind := range []string{"fifo", "directory-link", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".config", "gpu-workload-supervisor")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "operator.json")
			switch kind {
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory-link":
				if err := os.Rename(dir, dir+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-real", dir); err != nil {
					t.Fatal(err)
				}
			case "wrong-owner":
				if err := os.WriteFile(path, []byte(acceptanceProfile), 0600); err != nil {
					t.Fatal(err)
				}
			}
			uid := os.Geteuid()
			if kind == "wrong-owner" {
				uid++
			}
			start := time.Now()
			if _, err := loadProfileHome(t, home, uid); err == nil {
				t.Fatal("untrusted path accepted")
			}
			if time.Since(start) > time.Second {
				t.Fatal("special profile file blocked")
			}
		})
	}
}

func TestAcceptanceRequestNestedAmbiguityAndLimits(t *testing.T) {
	base := `{"protocolVersion":1,"requestId":"r","action":"user-switch","target":"idle","expected":{"incarnation":"i","version":"1","owner":"user","configurationRevision":"r"}}`
	for name, body := range map[string]string{
		"nested duplicate":         strings.Replace(base, `"version":"1"`, `"version":"1","version":"2"`, 1),
		"escaped nested duplicate": strings.Replace(base, `"owner":"user"`, `"owner":"user","\u006fwner":"user"`, 1),
		"case alias nested":        strings.Replace(base, `"owner":"user"`, `"owner":"user","Owner":"user"`, 1),
		"truncated value":          `{"protocolVersion":`,
		"long request id":          strings.Replace(base, `"requestId":"r"`, `"requestId":"`+strings.Repeat("x", 65)+`"`, 1),
		"long incarnation":         strings.Replace(base, `"incarnation":"i"`, `"incarnation":"`+strings.Repeat("x", 129)+`"`, 1),
		"long revision":            strings.Replace(base, `"configurationRevision":"r"`, `"configurationRevision":"`+strings.Repeat("x", 129)+`"`, 1),
		"target forbidden":         strings.Replace(base, `"user-switch"`, `"return-control"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, code := Decode([]byte(body)); code != InvalidRequest {
				t.Fatalf("code=%s", code)
			}
		})
	}
	body := base + strings.Repeat(" ", MaxRequestBytes-len(base)-1) + "\n"
	if b, err := ReadRequest(strings.NewReader(body), time.Second); err != nil {
		t.Fatal(err)
	} else if _, code := Decode(b); code != OK {
		t.Fatal(code)
	}
	if _, err := ReadRequest(strings.NewReader(body+" "), time.Second); err == nil {
		t.Fatal("limit plus one accepted")
	}
}

type acceptanceShortWriter struct{ calls int }

func (w *acceptanceShortWriter) Write(b []byte) (int, error) { w.calls++; return len(b) - 1, nil }

type acceptanceErrorReader struct{}

func (acceptanceErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestAcceptanceIOErrorsAndOutputLimit(t *testing.T) {
	if _, err := ReadRequest(acceptanceErrorReader{}, time.Second); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	w := &acceptanceShortWriter{}
	if err := WriteResponse(w, Response{Code: OK}, time.Second); !errors.Is(err, io.ErrShortWrite) || w.calls != 1 {
		t.Fatalf("%v calls=%d", err, w.calls)
	}
	var out bytes.Buffer
	if err := WriteResponse(&out, Response{RequestID: strings.Repeat("x", MaxResponseBytes)}, time.Second); err == nil || out.Len() != 0 {
		t.Fatalf("oversize response emitted: %v bytes=%d", err, out.Len())
	}
}

func TestAcceptanceProcessGatePreventsAnyOpen(t *testing.T) {
	if os.Getenv("GPU_ACCEPTANCE_LOCK_CHILD") == "1" {
		gate, err := lock.TryAcquire(os.Getenv("GPU_ACCEPTANCE_STATE") + ".lock")
		if err != nil {
			os.Exit(21)
		}
		defer gate.Close()
		_, _ = os.Stdout.Write([]byte("ready\n"))
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	path := filepath.Join(t.TempDir(), "state.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestAcceptanceProcessGatePreventsAnyOpen$")
	cmd.Env = append(os.Environ(), "GPU_ACCEPTANCE_LOCK_CHILD=1", "GPU_ACCEPTANCE_STATE="+path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		in.Close()
		if err := cmd.Wait(); err != nil {
			t.Error(err)
		}
	}()
	ready := make(chan error, 1)
	go func() {
		b := make([]byte, 6)
		_, err := io.ReadFull(out, b)
		if err == nil && string(b) != "ready\n" {
			err = errors.New("child not ready")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		t.Fatal("child gate acquisition stalled")
	}
	opened := false
	svc := Service{StatePath: path, Open: func(context.Context) (Session, error) { opened = true; return Session{}, errors.New("must not open") }}
	start := time.Now()
	for _, action := range []string{"status", "take-control", "user-switch", "return-control"} {
		if r := svc.Handle(Request{Action: action}); r.Code != Busy || r.Status != nil {
			t.Fatalf("%s: %+v", action, r)
		}
	}
	if opened || time.Since(start) > time.Second {
		t.Fatal("contended gate was not fail-fast")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state opened despite contention: %v", err)
	}
}

// Embedded nil methods deliberately fail the test if any runtime method is called.
type acceptanceUntouchedRuntime struct{ gpuruntime.Manager }

func TestAcceptanceControllerStalePreconditionsHaveZeroEffects(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap, err := db.ReplaceCatalog(ctx, "", control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "third", Label: "Third", Adapter: "systemd", Unit: "third.service", Cgroup: "/user/third", HealthURL: "http://localhost:9999"}}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := supervisor.New(db, acceptanceUntouchedRuntime{}, supervisor.Config{Catalog: &snap, DrainTimeout: time.Second, VerifyTimeout: time.Second, ActionTimeout: time.Second, CleanupTimeout: time.Second, FinalizeTimeout: time.Second, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{StatePath: path, Open: func(context.Context) (Session, error) {
		return Session{Backend: controller, Revision: snap.Revision, Workloads: []Workload{{"idle", "Idle"}, {"third", "Third"}}}, nil
	}}
	expected := Expected{before.LeaseFence.Incarnation, strconv.FormatUint(before.Version, 10), before.Owner, snap.Revision}
	for _, tc := range []struct {
		name   string
		change func(*Expected)
		code   Code
	}{
		{"restored incarnation", func(e *Expected) { e.Incarnation = "old-restored-incarnation" }, StaleState},
		{"stale version", func(e *Expected) { e.Version = strconv.FormatUint(before.Version+1, 10) }, StaleState},
		{"wrong owner", func(e *Expected) {
			if e.Owner == control.OwnerUser {
				e.Owner = control.OwnerSupervisor
			} else {
				e.Owner = control.OwnerUser
			}
		}, WrongOwner},
		{"changed configuration", func(e *Expected) { e.ConfigurationRevision = "old-revision" }, StaleState},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := expected
			tc.change(&e)
			r := svc.Handle(Request{ProtocolVersion: 1, RequestID: "r", Action: "take-control", Expected: &e})
			if r.Code != tc.code || r.Status != nil {
				t.Fatalf("response %+v", r)
			}
			after, err := db.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("state changed: before=%+v after=%+v", before, after)
			}
			afterCatalog, err := db.Catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(snap, afterCatalog) {
				t.Fatal("catalog changed")
			}
			pending, err := db.InProgressTransition(ctx)
			if err != nil || pending != "" {
				t.Fatalf("transition created: %q %v", pending, err)
			}
		})
	}
}
