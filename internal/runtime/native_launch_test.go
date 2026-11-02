package runtime

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func TestNativeLaunchFingerprintAndModelPath(t *testing.T) {
	for _, family := range []string{"vllm"} {
		t.Run(family, func(t *testing.T) {
			n := control.NativeModel{Runtime: family, Endpoint: "http://127.0.0.1:9000", Model: "selected"}
			raw := nativeLaunchFixture(t, &n)
			path := filepath.Join(t.TempDir(), "model.service")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			qualified, err := inspectFixtureLaunch(path, n)
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("%x", sha256.Sum256(raw)); qualified != want {
				t.Fatal("fingerprint mismatch")
			}
			if _, err := inspectFixtureLaunch(path+"missing", n); err == nil {
				t.Fatal("missing launch accepted")
			}
			if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectFixtureLaunch(path, n); err == nil {
				t.Fatal("invalid launch qualified")
			}
			if qualifyFixtureLaunch([]byte(strings.Replace(string(raw), "--port 9000", "--port 9001", 1)), n) == nil {
				t.Fatal("launch on another endpoint accepted")
			}
			if family == "vllm" {
				if err := qualifyFixtureLaunch([]byte(strings.TrimSpace(string(raw))+" --max-model-len 8192"), n); err != nil {
					t.Fatal(err)
				}
			}
			args := strings.Fields(strings.Split(string(raw), "ExecStart=")[1])
			local := args[2]
			withoutAlias := strings.Replace(string(raw), " --served-model-name selected", "", 1)
			withoutAlias = strings.Replace(withoutAlias, " --served-model-name selected", "", 1)
			n.Model = local
			if err := qualifyFixtureLaunch([]byte(withoutAlias), n); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(local); err != nil {
				t.Fatal(err)
			}
			if qualifyFixtureLaunch(raw, n) == nil {
				t.Fatal("missing model accepted")
			}
		})
	}
}

// vllmLaunchFixture returns a qualifying hardened direct vLLM launch on
// 127.0.0.1:9000 and its binding. The model directory exists.
func vllmLaunchFixture(t *testing.T) (string, control.NativeModel) {
	t.Helper()
	model := filepath.Join(t.TempDir(), "selected")
	if err := os.Mkdir(model, 0755); err != nil {
		t.Fatal(err)
	}
	hardened := newHardenedFixture(t)
	return "[Service]\n" + hardened.lines() + "ExecStart=/usr/bin/vllm serve " + model + " --host 127.0.0.1 --port 9000" + hardened.args() + "\n", hardened.bind(control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:9000", Model: model})
}

func qualifyFixtureLaunch(raw []byte, n control.NativeModel) error {
	return qualifyNativeLaunchWithValidator(raw, n, fixtureExecutableValidator)
}
func inspectFixtureLaunch(path string, n control.NativeModel) (string, error) {
	return inspectQualifiedNativeLaunch(path, n, fixtureExecutableValidator)
}

func TestNativeLaunchPreservesDocumentedOptionsAndPreparation(t *testing.T) {
	n := control.NativeModel{Runtime: "vllm", Endpoint: "http://127.0.0.1:9000", Model: "selected"}
	good := strings.TrimSpace(string(nativeLaunchFixture(t, &n)))
	options := "--max-model-len 4096"
	model := strings.Fields(strings.Split(good, "ExecStart=")[1])[2]
	raw := []byte(good + " " + options + "\nExecStartPre=/usr/bin/test -d " + model + "\nExecStartPre=-/usr/bin/true\n")
	launch, err := inspectAutomaticLaunch(raw, n.Runtime, fixtureExecutableValidator)
	if err != nil {
		t.Fatal(err)
	}
	if len(launch.PreCommands) != 2 || launch.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) || !strings.HasSuffix(launch.Command, options) {
		t.Fatalf("preparation or options lost: %+v", launch)
	}
	for _, options := range []string{"--max-model-len 0", "--max-model-len nope", "--max-model-len 1 --max-model-len 2", "--served-model-name other", "--model /other", "--parallel 4", "--unknown x", "--max-model-len"} {
		if qualifyFixtureLaunch([]byte(good+" "+options), n) == nil {
			t.Errorf("unsupported options accepted: %s", options)
		}
	}
	for _, command := range []string{"/bin/sh -c true", "env true", "+/usr/bin/true", "/usr/bin/env true", "/usr/bin/systemctl start other.service", "/missing/helper", "/usr/bin/true $ARGS", "/usr/bin/true ; /usr/bin/true"} {
		if qualifyFixtureLaunch([]byte(good+"\nExecStartPre="+command+"\n"), n) == nil {
			t.Errorf("unsafe preparation accepted: %s", command)
		}
	}
}

func TestLoadedStartupPreparationMustMatchBoundCommands(t *testing.T) {
	output := "{ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; start_time=[n/a] ; } { path=/usr/bin/sleep ; argv[]=/usr/bin/sleep 1 ; ignore_errors=yes ; start_time=[n/a] ; }"
	commands := []string{"/usr/bin/true", "-/usr/bin/sleep 1"}
	if err := CheckLoadedPreCommands(output, commands); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", output + " garbage", strings.Replace(output, "sleep 1", "sleep 2", 1), strings.Replace(output, "ignore_errors=yes", "ignore_errors=no", 1), strings.Replace(output, "path=/usr/bin/true", "path=/usr/bin/false", 1)} {
		if CheckLoadedPreCommands(bad, commands) == nil {
			t.Errorf("changed loaded preparation accepted: %s", bad)
		}
	}
	if CheckLoadedPreCommands(output, nil) == nil {
		t.Fatal("unbound loaded preparation accepted")
	}
}

func TestNativeStartupPreparationRecheckedBeforeStart(t *testing.T) {
	m, r, p, cmd := nativeFixture(t, "vllm", "http://127.0.0.1:9000")
	raw, err := os.ReadFile(p.NativeModel.LaunchFile)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("ExecStartPre=/usr/bin/true\n")...)
	if err := os.WriteFile(p.NativeModel.LaunchFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	p.NativeModel.LaunchSHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	good := string(r.outputs[cmd]) + "ExecStartPre={ path=/usr/bin/true ; argv[]=/usr/bin/true ; ignore_errors=no ; }\n"
	r.outputs[cmd] = []byte(good)
	if err := m.verifyNativeBinding(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	r.outputs[cmd] = []byte(strings.Replace(good, "argv[]=/usr/bin/true", "argv[]=/usr/bin/false", 1))
	if m.verifyNativeBinding(context.Background(), p) == nil {
		t.Fatal("changed preparation accepted")
	}
}

// Trusted interpreter binaries do not make their input code trustworthy. The
// unit fingerprint binds this path, but does not bind a script replaced later.
func TestStartupPreparationRejectsUnboundCodeAndWrappers(t *testing.T) {
	script := filepath.Join(t.TempDir(), "preflight.pl")
	if err := os.WriteFile(script, []byte("exit 0;\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0666); err != nil {
		t.Fatal(err)
	}
	commands := []string{
		"/usr/bin/perl " + script,
		"/usr/bin/python3 " + script,
		"/usr/bin/ruby " + script,
		"/usr/bin/node " + script,
		"/usr/bin/awk -f " + script,
		"/usr/bin/busybox sh " + script,
		"/usr/bin/timeout 1 /usr/bin/perl " + script,
		"/usr/bin/xargs /usr/bin/perl " + script,
		"/opt/trusted/test -f /models/selected",
	}
	good, n := vllmLaunchFixture(t)
	// Trust all executable paths to isolate the missing command-policy check.
	// Script input remains writable by a different principal.
	validate := func(string) error { return nil }
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			raw := []byte(good + "ExecStartPre=" + command + "\n")
			if err := qualifyNativeLaunchWithValidator(raw, n, validate); err == nil {
				t.Fatal("preparation can execute unbound code")
			}
			if _, err := inspectAutomaticLaunch(raw, n.Runtime, validate); err == nil {
				t.Fatal("automatic inspection accepted preparation that can execute unbound code")
			}
		})
	}
}

func TestStartupPreparationRejectsWritableScriptWithTrustedInterpreter(t *testing.T) {
	const interpreter = "/usr/bin/perl"
	if err := validateNativeExecutable(interpreter); err != nil {
		t.Skipf("trusted Perl executable unavailable: %v", err)
	}
	script := filepath.Join(t.TempDir(), "preflight.pl")
	if err := os.WriteFile(script, []byte("exit 0;\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0666); err != nil {
		t.Fatal(err)
	}
	unit := parsedLaunchUnit{preCommands: []string{interpreter + " " + script}}
	if err := unit.validatePreCommands(validateNativeExecutable); err == nil {
		t.Fatal("trusted interpreter accepted a script writable by another principal")
	}
}

func TestStartupPreparationPreservesSupportedChecks(t *testing.T) {
	good, n := vllmLaunchFixture(t)
	commands := []string{"/usr/bin/true", "-/usr/bin/true --", "/bin/false", "-/bin/false --"}
	for _, predicate := range []string{"-e", "-f", "-d", "-r", "-w", "-x", "-s"} {
		commands = append(commands, "/usr/bin/test "+predicate+" /models/selected", "-/bin/test "+predicate+" /models/selected")
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			raw := []byte(good + "ExecStartPre=" + command + "\n")
			validate := func(string) error { return nil }
			launch, err := inspectAutomaticLaunch(raw, n.Runtime, validate)
			if err != nil {
				t.Fatal(err)
			}
			if len(launch.PreCommands) != 1 || launch.PreCommands[0] != command || launch.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
				t.Fatalf("supported preparation changed: %+v", launch)
			}
			unit := parsedLaunchUnit{preCommands: []string{command}}
			if unit.validatePreCommands(func(string) error { return ErrLaunchUnsupported }) == nil {
				t.Fatal("allowed grammar bypassed executable trust")
			}
		})
	}
	for _, command := range []string{
		"/usr/bin/test -f relative", "/usr/bin/test -f /models/../selected",
		"/usr/bin/test -f /models/selected -o -d /models", "/usr/bin/test /models/selected",
		"/usr/bin/test -f /models/selected extra", "/usr/bin/true ignored", "/usr/bin/false -- extra",
		"--/usr/bin/true", "+/usr/bin/test -f /models/selected", "/usr/bin/../bin/true",
	} {
		raw := []byte(good + "ExecStartPre=" + command + "\n")
		if qualifyNativeLaunchWithValidator(raw, n, func(string) error { return nil }) == nil {
			t.Errorf("unsupported preparation grammar accepted: %s", command)
		}
	}
}
