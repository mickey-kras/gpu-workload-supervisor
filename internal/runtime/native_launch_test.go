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

func TestNativeLaunchQualificationRejectsIndirectionAndMutation(t *testing.T) {
	n := control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://localhost:9000", Model: "selected"}
	good := string(nativeLaunchFixture(t, n.Runtime, n.Endpoint, n.Model))
	cases := []string{
		"garbage", "[Other]\nKey=value", "[Service]\nExecStart=/bin/sh -c true", "[Service]\nExecStart=llama-server",
		good + "ExecStopPost=/usr/bin/true\n", good + "EnvironmentFile=/tmp/config\n", good + "Environment=LD_PRELOAD=/tmp/lib.so\n",
		good + "Restart=always\n", good + "Type=forking\n", good + "ExecStart=/usr/bin/true\n", good + "BindPaths=/tmp\n", good + "ExecSearchPath=/tmp\n",
		strings.Replace(good, " --model ", " --hf-repo ", 1), strings.Replace(good, " --alias selected", " --alias other", 1), strings.Replace(good, " --host localhost", " --host 0.0.0.0", 1), strings.Replace(good, " --port 9000", " --port 9001", 1),
		strings.TrimSpace(good) + " --port 9000", strings.TrimSpace(good) + " --unknown x", strings.TrimSpace(good) + " --ctx-size 0", strings.TrimSpace(good) + " --ctx-size nope", strings.TrimSpace(good) + " --max-model-len 1024", strings.TrimSpace(good) + " --alias another", strings.TrimSpace(good) + " --flag", strings.TrimSpace(good) + " --ctx-size $CTX", strings.TrimSpace(good) + " --ctx-size %i", strings.TrimSpace(good) + " --ctx-size '12'",
		strings.Replace(good, " --model ", " --model relative --model ", 1), strings.Replace(good, " --alias selected", " --served-model-name selected", 1), strings.Replace(good, "llama-server ", "missing ", 1), strings.Replace(good, "[Service]", "[Unit]", 1),
	}
	for _, raw := range cases {
		if err := qualifyFixtureLaunch([]byte(raw), n); err == nil {
			t.Errorf("unsafe launch accepted: %s", raw)
		}
	}
	valid := "# comment\n; comment\n[Unit]\nDescription=Native model\n" + good + "Type=exec\nRestart=no\n[Install]\nWantedBy=default.target\n"
	if err := qualifyFixtureLaunch([]byte(valid), n); err != nil {
		t.Fatal(err)
	}
	if err := qualifyFixtureLaunch([]byte(strings.TrimSpace(good)+" --ctx-size 8192 --n-gpu-layers 99"), n); err != nil {
		t.Fatal(err)
	}
	n.Runtime = "unknown"
	if qualifyFixtureLaunch([]byte(good), n) == nil {
		t.Fatal("unknown runtime accepted")
	}
	n.Runtime = "vllm"
	if qualifyFixtureLaunch([]byte(good), n) == nil {
		t.Fatal("wrong executable accepted")
	}
	n.Runtime = "ollama"
	if qualifyFixtureLaunch([]byte(good), n) == nil {
		t.Fatal("wrong executable accepted")
	}
	n.Runtime = "llama.cpp"
	n.Endpoint = "://bad"
	if qualifyFixtureLaunch([]byte(good), n) == nil {
		t.Fatal("bad endpoint accepted")
	}
}

func TestNativeLaunchFingerprintAndModelPath(t *testing.T) {
	for _, family := range []string{"llama.cpp", "vllm", "ollama"} {
		t.Run(family, func(t *testing.T) {
			n := control.NativeModel{Runtime: family, Endpoint: "http://localhost:9000", Model: "selected"}
			raw := nativeLaunchFixture(t, family, n.Endpoint, n.Model)
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
			if family == "ollama" {
				for _, bad := range []string{strings.Replace(string(raw), "OLLAMA_NO_CLOUD=1", "OLLAMA_NO_CLOUD=0", 1), string(raw) + "Environment=OLLAMA_NO_CLOUD=1\n", string(raw) + "Environment=OLLAMA_HOST=localhost:9000\n", strings.Replace(string(raw), " serve", " run selected", 1), strings.Replace(string(raw), "localhost:9000", "localhost:9001", 1)} {
					if qualifyFixtureLaunch([]byte(bad), n) == nil {
						t.Fatal("unsafe ollama launch accepted")
					}
				}
			} else {
				if family == "vllm" {
					if err := qualifyFixtureLaunch([]byte(strings.TrimSpace(string(raw))+" --max-model-len 8192"), n); err != nil {
						t.Fatal(err)
					}
				}
				args := strings.Fields(strings.Split(string(raw), "ExecStart=")[1])
				local := args[2]
				withoutAlias := strings.Replace(string(raw), " --alias selected", "", 1)
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
			}
		})
	}
}

func TestNativeExecutableRequiresTrustedAncestors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(path, []byte("x"), 0700); err != nil {
		t.Fatal(err)
	}
	if validateNativeExecutable(path) == nil {
		t.Fatal("executable in writable temp ancestry accepted")
	}
	if validateNativeExecutable("/missing/llama-server") == nil {
		t.Fatal("missing executable accepted")
	}
}

func qualifyFixtureLaunch(raw []byte, n control.NativeModel) error {
	return qualifyNativeLaunchWithValidator(raw, n, fixtureExecutableValidator)
}
func inspectFixtureLaunch(path string, n control.NativeModel) (string, error) {
	return inspectQualifiedNativeLaunch(path, n, fixtureExecutableValidator)
}
func TestQualifiedFingerprintRejectsUntrustedExecutable(t *testing.T) {
	n := control.NativeModel{Runtime: "ollama", Endpoint: "http://localhost:9000", Model: "selected"}
	raw := nativeLaunchFixture(t, n.Runtime, n.Endpoint, n.Model)
	path := filepath.Join(t.TempDir(), "model.service")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectQualifiedNativeLaunch(path, n); err == nil {
		t.Fatal("untrusted executable qualified")
	}
	if qualifyNativeLaunchWithValidator(raw, n, validateNativeExecutable) == nil {
		t.Fatal("untrusted executable qualified")
	}
}

func TestNativeLaunchPreservesDocumentedLlamaOptionsAndPreparation(t *testing.T) {
	n := control.NativeModel{Runtime: "llama.cpp", Endpoint: "http://localhost:9000", Model: "selected"}
	good := strings.TrimSpace(string(nativeLaunchFixture(t, n.Runtime, n.Endpoint, n.Model)))
	for _, options := range []string{
		"--parallel 4 --cont-batching --flash-attn on --spec-type draft-mtp --spec-draft-n-max 4 --spec-draft-n-min 0",
		"-np -1 -nocb -fa auto",
	} {
		model := strings.Fields(strings.Split(good, "ExecStart=")[1])[2]
		raw := []byte(good + " " + options + "\nExecStartPre=/usr/bin/test -f " + model + "\nExecStartPre=-/usr/bin/true\n")
		launch, err := inspectAutomaticLaunch(raw, n.Runtime, fixtureExecutableValidator)
		if err != nil {
			t.Fatal(err)
		}
		if len(launch.PreCommands) != 2 || launch.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) || !strings.HasSuffix(launch.Command, options) {
			t.Fatalf("preparation or options lost: %+v", launch)
		}
	}
	for _, options := range []string{"--parallel 0", "--parallel nope", "-np 2 --parallel 3", "-cb --no-cont-batching", "--flash-attn yes", "--spec-type draft-simple", "--spec-draft-model /other.gguf", "--spec-draft-n-max -1"} {
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
	m, r, p, cmd := nativeFixture(t, "llama.cpp", "http://localhost:9000")
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
