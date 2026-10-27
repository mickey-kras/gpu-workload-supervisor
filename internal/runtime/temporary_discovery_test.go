package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
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
	r := &temporaryUnitRunner{file: file, timestamp: "0"}
	m := &SystemdManager{config: SystemdConfig{Catalog: &control.Catalog{Version: 1}, SystemctlPath: "/usr/bin/true"}, runner: r, nativeExecutableValidator: func(string) error { return nil }}
	root := t.TempDir()
	m.cgroups = cgroupFS{root: root, verify: func(int) error { return nil }}
	writeEvents(t, root, "user.test", "populated 0\n")
	v := control.TemporaryDiscoveryCandidate{Unit: "ollama.service", LaunchFile: file, LaunchSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Endpoint: "http://127.0.0.1:11434", Cgroup: "/user.test/app.slice/ollama.service", SystemdSlice: "app.slice", SystemdVersion: 252}
	return m, r, v
}
func TestLegacyTemporaryCleanupStopsOnlyRecordedInvocation(t *testing.T) {
	for _, mode := range []string{"recorded", "unknown", "replaced", "changed-file", "changed-source", "populated", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			m, r, v := temporaryManagerFixture(t)
			r.active = true
			r.invocation = strings.Repeat("a", 32)
			invocation := r.invocation
			switch mode {
			case "unknown":
				invocation = ""
			case "replaced":
				invocation = strings.Repeat("b", 32)
			case "changed-file":
				if err := os.WriteFile(v.LaunchFile, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-source":
				r.changed = true
			case "populated":
				writeEvents(t, m.cgroups.root, "user.test/app.slice/ollama.service", "populated 1\n")
			case "stopped":
				r.active = false
				invocation = ""
			}
			err := m.StopTemporaryDiscovery(t.Context(), v, invocation)
			if mode == "recorded" || mode == "stopped" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			expectedStops := 0
			if mode == "recorded" || mode == "populated" {
				expectedStops = 1
			}
			if r.stops != expectedStops || r.starts != 0 {
				t.Fatal("unexpected lifecycle command", r.stops, r.starts)
			}
		})
	}
}
