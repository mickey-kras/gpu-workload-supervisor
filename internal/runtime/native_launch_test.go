package runtime

import (
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
		good + "ExecStartPre=/usr/bin/true\n", good + "ExecStopPost=/usr/bin/true\n", good + "EnvironmentFile=/tmp/config\n", good + "Environment=LD_PRELOAD=/tmp/lib.so\n",
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
			hash, err := InspectNativeLaunch(path)
			if err != nil || hash != qualified {
				t.Fatal("fingerprint mismatch", err)
			}
			if _, err := inspectFixtureLaunch(path+"missing", n); err == nil {
				t.Fatal("missing launch accepted")
			}
			if _, err := InspectNativeLaunch(path + "missing"); err == nil {
				t.Fatal("missing launch hashed")
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
	if qualifyNativeLaunch(raw, n) == nil {
		t.Fatal("untrusted executable qualified")
	}
}
