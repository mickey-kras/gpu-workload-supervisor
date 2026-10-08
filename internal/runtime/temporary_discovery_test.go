package runtime

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type temporaryUnitRunner struct {
	file          string
	active        bool
	invocation    string
	timestamp     string
	changed       bool
	outputMissing bool
	startErr      error
	mutations     map[string]string
	failCommand   string
	starts, stops int
}

func (r *temporaryUnitRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	unit := args[len(args)-1]
	if r.failCommand != "" && strings.Contains(command, r.failCommand) {
		return nil, errors.New("host command unavailable")
	}
	if command == "--version" {
		return []byte("systemd 252\n"), nil
	}
	if strings.Contains(command, " start ") {
		r.starts++
		r.active = true
		r.invocation = "12345678901234567890123456789012"
		r.timestamp = "1234"
		if r.outputMissing {
			return nil, r.startErr
		}
		return []byte("Enqueued anchor job 91 ollama.service/start.\n"), r.startErr
	}
	if strings.Contains(command, " stop ") {
		r.stops++
		r.active = false
		return nil, nil
	}
	if unit == "-.slice" {
		return []byte("Id=-.slice\nLoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/user.test\n"), nil
	}
	if unit == "app.slice" {
		return []byte("Id=app.slice\nLoadState=loaded\nActiveState=active\nSubState=active\nControlGroup=/user.test/app.slice\n"), nil
	}
	state, sub, group := "inactive", "dead", ""
	if r.active {
		state, sub, group = "active", "running", "/user.test/app.slice/ollama.service"
	}
	drop := ""
	if r.changed {
		drop = "/changed.conf"
	}
	raw := []byte("ExecStart={ path=/usr/bin/ollama ; argv[]=/usr/bin/ollama serve ; }\nExecStartPre=\nId=ollama.service\nLoadState=loaded\nActiveState=" + state + "\nSubState=" + sub + "\nControlGroup=" + group + "\nSlice=app.slice\nFragmentPath=" + r.file + "\nDropInPaths=" + drop + "\nNeedDaemonReload=no\nInvocationID=" + r.invocation + "\nActiveEnterTimestampMonotonic=" + r.timestamp + "\n")
	props, err := ParseUnitProperties(raw)
	if err != nil {
		return nil, err
	}
	for key, value := range r.mutations {
		props[key] = value
	}
	var out strings.Builder
	for key, value := range props {
		out.WriteString(key + "=" + value + "\n")
	}
	return []byte(out.String()), nil
}
func temporaryManagerFixture(t *testing.T) (*SystemdManager, *temporaryUnitRunner, control.TemporaryDiscoveryCandidate) {
	t.Helper()
	raw := []byte("[Service]\nType=exec\nRestart=no\nEnvironment=OLLAMA_NO_CLOUD=1\nEnvironment=OLLAMA_HOST=127.0.0.1:11434\nExecStart=/usr/bin/ollama serve\n")
	file := filepath.Join(t.TempDir(), "ollama.service")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	launch, err := inspectAutomaticLaunch(raw, "ollama", func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r := &temporaryUnitRunner{file: file, timestamp: "0"}
	m := &SystemdManager{config: SystemdConfig{Catalog: &control.Catalog{Version: 1}, SystemctlPath: "/usr/bin/true"}, runner: r, nativeExecutableValidator: func(string) error { return nil }}
	root := t.TempDir()
	m.cgroups = cgroupFS{root: root, verify: func(int) error { return nil }}
	writeEvents(t, root, "user.test", "populated 0\n")
	v := control.TemporaryDiscoveryCandidate{Unit: "ollama.service", LaunchFile: file, LaunchSHA256: launch.SHA256, Endpoint: launch.Endpoint, Cgroup: "/user.test/app.slice/ollama.service", SystemdSlice: "app.slice", SystemdVersion: 252}
	return m, r, v
}
func TestTemporaryUnitStartAndStopVerifiedInvocation(t *testing.T) {
	m, r, v := temporaryManagerFixture(t)
	ctx := context.Background()
	if err := m.PrepareTemporaryDiscovery(ctx, v); err != nil {
		t.Fatal(err)
	}
	evidence, err := m.StartTemporaryDiscovery(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.JobID != "91" || evidence.ActivationTimestamp != "1234" || evidence.InvocationID == "" {
		t.Fatalf("missing launch evidence %+v", evidence)
	}
	if err = m.StopTemporaryDiscovery(ctx, v, "ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrTemporaryInvocationChanged) || r.stops != 0 {
		t.Fatalf("foreign invocation stopped: %v", err)
	}
	if err = m.StopTemporaryDiscovery(ctx, v, evidence.InvocationID); err != nil || r.stops != 1 || r.active {
		t.Fatalf("cleanup %v %+v", err, r)
	}
	if err = m.StopTemporaryDiscovery(ctx, v, ""); err != nil || r.stops != 1 {
		t.Fatalf("stopped retry mutated service %v %+v", err, r)
	}
}
func TestTemporaryUnitRejectsRunningChangedAndUnverifiedRelease(t *testing.T) {
	for _, mode := range []string{"already-running", "file-change", "loaded-change", "populated", "missing-job", "invalid-unit", "changed-endpoint", "changed-placement-version", "start-cancel"} {
		t.Run(mode, func(t *testing.T) {
			m, r, v := temporaryManagerFixture(t)
			switch mode {
			case "already-running":
				r.active = true
			case "file-change":
				os.WriteFile(v.LaunchFile, []byte("changed"), 0600)
			case "loaded-change":
				r.changed = true
			case "populated":
				writeEvents(t, m.cgroups.root, "user.test/app.slice/ollama.service", "populated 1\n")
			case "missing-job":
				r.outputMissing = true
			case "invalid-unit":
				v.Unit = "foreign@x.service"
			case "changed-endpoint":
				v.Endpoint = "http://127.0.0.1:9999"
			case "changed-placement-version":
				v.SystemdVersion = 255
			case "start-cancel":
				r.startErr = context.Canceled
			}
			evidence, err := m.StartTemporaryDiscovery(context.Background(), v)
			if err == nil {
				t.Fatal("unsafe start accepted")
			}
			if mode == "start-cancel" {
				if evidence.InvocationID == "" {
					t.Fatal("canceled accepted job lost invocation evidence")
				}
				if err = m.StopTemporaryDiscovery(context.Background(), v, evidence.InvocationID); err != nil {
					t.Fatal(err)
				}
				return
			}
			if mode == "missing-job" {
				if evidence.InvocationID != "" {
					t.Fatal("ambiguous start claimed invocation")
				}
				if err = m.StopTemporaryDiscovery(context.Background(), v, ""); !errors.Is(err, ErrTemporaryInvocationChanged) || r.stops != 0 {
					t.Fatal("ambiguous invocation stopped")
				}
				return
			}
			if r.starts != 0 || r.stops != 0 {
				t.Fatalf("effect before proof: %+v", r)
			}
		})
	}
}
func TestTemporaryCleanupRefusesBindingDriftAndSurvivingDescendants(t *testing.T) {
	for _, mode := range []string{"binding-change", "populated", "restarted"} {
		t.Run(mode, func(t *testing.T) {
			m, r, v := temporaryManagerFixture(t)
			e, err := m.StartTemporaryDiscovery(context.Background(), v)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "binding-change":
				r.changed = true
			case "populated":
				writeEvents(t, m.cgroups.root, "user.test/app.slice/ollama.service", "populated 1\n")
			case "restarted":
				r.invocation = "ffffffffffffffffffffffffffffffff"
			}
			if err = m.StopTemporaryDiscovery(context.Background(), v, e.InvocationID); err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			if mode != "populated" && r.stops != 0 {
				t.Fatal("stopped changed installation")
			}
		})
	}
}

func TestTemporaryDiscoveryRejectsIncompleteLoadedEvidence(t *testing.T) {
	for _, field := range []string{"ExecStart", "ExecStartPre", "NeedDaemonReload", "Id", "LoadState", "Slice", "FragmentPath", "ControlGroup"} {
		t.Run(field, func(t *testing.T) {
			m, r, v := temporaryManagerFixture(t)
			r.mutations = map[string]string{field: "foreign"}
			if _, err := m.StartTemporaryDiscovery(context.Background(), v); err == nil || r.starts != 0 {
				t.Fatalf("started with invalid %s evidence: %v", field, err)
			}
		})
	}
	for _, command := range []string{"--version", "-- -.slice", "-- app.slice", "-- ollama.service"} {
		t.Run(command, func(t *testing.T) {
			m, r, v := temporaryManagerFixture(t)
			r.failCommand = command
			if _, err := m.StartTemporaryDiscovery(context.Background(), v); err == nil || r.starts != 0 {
				t.Fatalf("started after unavailable %s: %v", command, err)
			}
		})
	}
	m, r, v := temporaryManagerFixture(t)
	os.Remove(v.LaunchFile)
	if _, err := m.StartTemporaryDiscovery(context.Background(), v); err == nil || r.starts != 0 {
		t.Fatal("missing launch file started")
	}
}
func TestTemporaryStopCommandFailureAndUnknownInvocation(t *testing.T) {
	m, r, v := temporaryManagerFixture(t)
	e, err := m.StartTemporaryDiscovery(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	r.failCommand = " stop "
	if err := m.StopTemporaryDiscovery(context.Background(), v, e.InvocationID); err == nil || !r.active {
		t.Fatal("stop command failure ignored")
	}
	r.failCommand = ""
	for _, id := range []string{"", strings.Repeat("0", 32), strings.Repeat("g", 32), "123"} {
		if err := m.StopTemporaryDiscovery(context.Background(), v, id); !errors.Is(err, ErrTemporaryInvocationChanged) || r.stops != 0 {
			t.Fatalf("invalid invocation %q stopped: %v", id, err)
		}
	}
	r.changed = true
	if err := m.StopTemporaryDiscovery(context.Background(), v, e.InvocationID); err == nil || r.stops != 0 {
		t.Fatal("changed launch stopped")
	}
}

func TestTemporaryDiscoveryPreservesQualifiedDropInAndRefusesSourceDrift(t *testing.T) {
	for _, mode := range []string{"complete", "file-change", "loaded-order", "unqualified"} {
		t.Run(mode, func(t *testing.T) {
			m, r, v := temporaryManagerFixture(t)
			path := filepath.Join(filepath.Dir(v.LaunchFile), "10-options.conf")
			raw := []byte("[Service]\nWorkingDirectory=/opt/models\nLimitNOFILE=65536\nExecStartPre=/usr/bin/true\n")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			launch, err := inspectAutomaticLaunchSourcesWithValidator(v.LaunchFile, "ollama", []string{path}, func(string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			v.DropIns = launch.DropIns
			r.mutations = map[string]string{"DropInPaths": path, "ExecStartPre": "{ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; }"}
			if mode == "unqualified" {
				os.WriteFile(path, []byte("[Service]\nEnvironmentFile=/tmp/injected\n"), 0600)
				if _, err := m.StartTemporaryDiscovery(context.Background(), v); err == nil || r.starts != 0 {
					t.Fatal("unsupported drop-in started")
				}
				return
			}
			evidence, err := m.StartTemporaryDiscovery(context.Background(), v)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "file-change" {
				os.WriteFile(path, []byte("[Service]\nLimitNOFILE=32768\n"), 0600)
			}
			if mode == "loaded-order" {
				r.mutations["DropInPaths"] = "/other.conf " + path
			}
			err = m.StopTemporaryDiscovery(context.Background(), v, evidence.InvocationID)
			if mode != "complete" {
				if err == nil || r.stops != 0 {
					t.Fatal("changed external sources stopped")
				}
				return
			}
			if err != nil || r.stops != 1 {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(raw) {
				t.Fatal("drop-in rewritten")
			}
		})
	}
}
