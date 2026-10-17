package llmtraining

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"golang.org/x/sys/unix"
)

const executionGPU = "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const executionInvocation = "1234567890abcdef1234567890abcdef"

// This worker fixture tests the real process/receipt boundary, not Axolotl or GPU support.
const executionScript = `#!/bin/sh
set -eu
[ "$1" = -I ] && [ "$2" = -S ] && [ "$3" = -B ]
[ "${HF_TOKEN-unset}" = unset ] && [ "${PYTHONPATH-unset}" = unset ] && [ "${INVOCATION_ID-unset}" = unset ]
[ "$HF_HUB_OFFLINE" = 1 ] && [ "$AXOLOTL_DO_NOT_TRACK" = 1 ]
[ "$CUDA_VISIBLE_DEVICES" = GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee ]
if [ "$5" = train ]; then
 printf '{"version":1,"phase":"train","step":2,"evalLoss":1.5,"reloaded":false}' > "$7"
else
 printf '{"version":1,"phase":"reload","step":2,"evalLoss":0,"reloaded":true}' > "$7"
fi
chmod 600 "$7"
`

func executionFixture(t *testing.T, script string) (Recipe, jobEnvelope, string) {
	t.Helper()
	r, e, jobPath, _ := jobFixture(t)
	bin := filepath.Join(r.Runtime.Inventory.Root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	r.Runtime.Entrypoint = "bin/python3"
	r.Runtime.Inventory.Files[r.Runtime.Entrypoint] = writeFixture(t, filepath.Join(bin, "python3"), script)
	if err := os.Chmod(filepath.Join(bin, "python3"), 0700); err != nil {
		t.Fatal(err)
	}
	r.Budget.ExportBytes = 100
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	e.Job.RecipeSHA256 = writeFixture(t, e.RecipeFile, string(raw))
	writeJobFixture(t, jobPath, e)
	return r, e, jobPath
}

func TestRunJobRequiresBothPhasesAndBindsCompletion(t *testing.T) {
	r, e, jobPath := executionFixture(t, executionScript)
	// The fake worker creates its output only after the empty-output preflight.
	script := executionScript
	script += "printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'\n"
	updateExecutionScript(t, r, &e, jobPath, script)
	t.Setenv("HF_TOKEN", "must-not-reach-the-worker")
	t.Setenv("PYTHONPATH", "/untrusted")
	t.Setenv("INVOCATION_ID", "must-not-reach-the-worker")
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(e.TerminalFile)
	if err != nil {
		t.Fatal(err)
	}
	var terminal control.TrainingTerminal
	if err := json.Unmarshal(raw, &terminal); err != nil {
		t.Fatal(err)
	}
	e.Job.InvocationID = executionInvocation
	if !terminal.Matches(e.Job) || terminal.Outcome != "completed" {
		t.Fatal("completion is not bound to this execution")
	}
	provenance, err := os.ReadFile(filepath.Join(r.OutputDirectory, "supervisor-provenance.json"))
	if err != nil || strings.Contains(string(provenance), e.Job.Token) || !strings.Contains(string(provenance), e.Job.RecipeSHA256) {
		t.Fatal("provenance missing identity or leaked a credential", err)
	}
	if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err == nil {
		t.Fatal("replayed the execution")
	}
}

func updateExecutionScript(t *testing.T, r Recipe, e *jobEnvelope, jobPath, script string) {
	t.Helper()
	r.Runtime.Inventory.Files[r.Runtime.Entrypoint] = writeFixture(t, filepath.Join(r.Runtime.Inventory.Root, r.Runtime.Entrypoint), script)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	e.Job.RecipeSHA256 = writeFixture(t, e.RecipeFile, string(raw))
	writeJobFixture(t, jobPath, *e)
}

func TestRunJobFailureNeverAcknowledgesCompletion(t *testing.T) {
	for name, script := range map[string]string{
		"process failure":  "#!/bin/sh\nexit 1\n",
		"missing receipt":  "#!/bin/sh\nexit 0\n",
		"incomplete steps": strings.ReplaceAll(executionScript, `"step":2`, `"step":1`),
		"negative loss":    strings.ReplaceAll(executionScript, `"evalLoss":1.5`, `"evalLoss":-1`),
		"false reload":     strings.ReplaceAll(executionScript, `"reloaded":true`, `"reloaded":false`),
		"reload failed":    strings.ReplaceAll(executionScript, "else\n", "else\n exit 2\n"),
		"adapter missing":  executionScript,
	} {
		t.Run(name, func(t *testing.T) {
			r, e, jobPath := executionFixture(t, script)
			wantPhases := ""
			switch name {
			case "incomplete steps", "negative loss":
				wantPhases = "train\n"
			case "false reload", "reload failed":
				wantPhases = "train\nreload\n"
			}
			marker := filepath.Join(t.TempDir(), "phases")
			if wantPhases != "" {
				// Receipt failures otherwise have a complete worker result,
				// including a bounded adapter, so later failures cannot mask them.
				script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\nprintf '%s\\n' \"$5\" >> '"+marker+"'\n", 1)
				script += "if [ \"$5\" = train ]; then printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'; fi\n"
				updateExecutionScript(t, r, &e, jobPath, script)
			}
			err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU)
			if err == nil || strings.Contains(err.Error(), e.Job.Token) {
				t.Fatal("accepted incomplete execution or disclosed completion token", err)
			}
			if _, err := os.Lstat(e.TerminalFile); !os.IsNotExist(err) {
				t.Fatal("failed execution published terminal completion", err)
			}
			if wantPhases != "" {
				if name != "reload failed" && err.Error() != "training worker acknowledgement is incomplete or invalid" {
					t.Fatal("invalid receipt failed at a different boundary", err)
				}
				phases, err := os.ReadFile(marker)
				if err != nil || string(phases) != wantPhases {
					t.Fatalf("failure reached wrong phases: got=%q, want=%q, err=%v", phases, wantPhases, err)
				}
			}
		})
	}
}

func TestRunJobRejectsUnsafeInputBeforeExecution(t *testing.T) {
	for _, name := range []string{"invocation", "gpu", "job path", "job permissions", "runtime permissions", "existing execution", "recipe changed", "invalid envelope", "output occupied", "output permissions", "runtime entrypoint", "adapter redirect", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "worker-started")
			script := strings.Replace(executionScript, "#!/bin/sh\n", "#!/bin/sh\nprintf started > '"+marker+"'\n", 1)
			r, e, jobPath := executionFixture(t, script)
			invocation, gpu := executionInvocation, executionGPU
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "invocation":
				invocation = strings.ToUpper(invocation)
			case "gpu":
				gpu = "0"
			case "job path":
				jobPath = "relative"
			case "job permissions":
				if err := os.Chmod(filepath.Dir(jobPath), 0755); err != nil {
					t.Fatal(err)
				}
			case "runtime permissions":
				if err := os.Chmod(r.Runtime.Inventory.Root, 0777); err != nil {
					t.Fatal(err)
				}
			case "existing execution":
				if err := os.Mkdir(filepath.Join(filepath.Dir(jobPath), ".llm-"+invocation), 0700); err != nil {
					t.Fatal(err)
				}
			case "recipe changed":
				writeFixture(t, e.RecipeFile, "{}")
			case "invalid envelope":
				writeFixture(t, jobPath, "{}")
			case "output occupied":
				writeFixture(t, filepath.Join(r.OutputDirectory, "existing"), "preserve")
			case "output permissions":
				if err := os.Chmod(r.OutputDirectory, 0755); err != nil {
					t.Fatal(err)
				}
			case "adapter redirect":
				r.Model.Inventory.Files["adapter_config.json"] = writeFixture(t, filepath.Join(r.Model.Inventory.Root, "adapter_config.json"), `{"base_model_name_or_path":"outside"}`)
				raw, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				e.Job.RecipeSHA256 = writeFixture(t, e.RecipeFile, string(raw))
				writeJobFixture(t, jobPath, e)
			case "runtime entrypoint":
				r.Runtime.Entrypoint = "axolotl"
				entrypoint := filepath.Join(r.Runtime.Inventory.Root, r.Runtime.Entrypoint)
				r.Runtime.Inventory.Files[r.Runtime.Entrypoint] = writeFixture(t, entrypoint, script)
				if err := os.Chmod(entrypoint, 0700); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				e.Job.RecipeSHA256 = writeFixture(t, e.RecipeFile, string(raw))
				writeJobFixture(t, jobPath, e)
			case "cancelled":
				cancel()
			}
			if err := RunJob(ctx, jobPath, invocation, gpu); err == nil {
				t.Fatal("accepted unsafe execution")
			}
			if _, err := os.Lstat(marker); !os.IsNotExist(err) {
				t.Fatal("preflight rejection executed the worker", err)
			}
		})
	}
}

func TestRunJobRejectsInputsOrAdapterChangedDuringTraining(t *testing.T) {
	for _, name := range []string{"job", "recipe", "model", "runtime", "dataset", "adapter", "empty adapter", "oversized adapter"} {
		t.Run(name, func(t *testing.T) {
			r, e, jobPath := executionFixture(t, executionScript)
			adapter := filepath.Join(r.OutputDirectory, "adapter_model.safetensors")
			script := executionScript + "printf x > '" + adapter + "'\n"
			path := ""
			switch name {
			case "job":
				path = jobPath
			case "recipe":
				path = e.RecipeFile
			case "model":
				path = filepath.Join(r.Model.Inventory.Root, "config.json")
			case "runtime":
				path = filepath.Join(r.Runtime.Inventory.Root, "axolotl")
			case "dataset":
				path = r.Train.Path
			case "adapter":
				script += "if [ \"$5\" = reload ]; then printf y > '" + adapter + "'; fi\n"
			case "empty adapter":
				script += ": > '" + adapter + "'\n"
			case "oversized adapter":
				script += "printf '" + strings.Repeat("x", 101) + "' > '" + adapter + "'\n"
			}
			if path != "" {
				script += "printf changed > '" + path + "'\n"
			}
			updateExecutionScript(t, r, &e, jobPath, script)
			if err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU); err == nil {
				t.Fatal("accepted changed execution inputs or adapter")
			}
			if _, err := os.Lstat(e.TerminalFile); !os.IsNotExist(err) {
				t.Fatal("published completion after input mutation", err)
			}
		})
	}
}

func TestTerminalPublicationPreservesUnsafePreviousDestination(t *testing.T) {
	for _, name := range []string{"public file", "directory", "link"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "terminal.json")
			switch name {
			case "public file":
				writeFixture(t, path, "previous")
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "link":
				target := filepath.Join(root, "preserve")
				writeFixture(t, target, "previous")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := replaceEvidence(path, control.TrainingTerminal{Version: 1, Outcome: "completed"}); err == nil {
				t.Fatal("replaced unsafe terminal destination")
			}
		})
	}
}

func TestWorkerPythonGuards(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for worker boundary tests")
	}
	cmd := exec.Command(python, "-m", "unittest", "discover", "-s", "../../tests/training", "-p", "test_worker.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker guards failed: %v\n%s", err, output)
	}
}

func TestTrustedRuntimeAncestors(t *testing.T) {
	for _, name := range []string{"private", "writable parent", "writable ancestor", "sticky parent", "linked parent", "writable child", "linked child", "relative", "missing"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			parent := filepath.Join(base, "parent")
			root := filepath.Join(parent, "runtime")
			if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			child := filepath.Join(root, "bin", "python3")
			writeFixture(t, child, "harmless runtime fixture")
			switch name {
			case "writable parent":
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
			case "writable ancestor":
				if err := os.Chmod(base, 0777); err != nil {
					t.Fatal(err)
				}
			case "sticky parent":
				if err := os.Chmod(parent, os.ModeSticky|0777); err != nil {
					t.Fatal(err)
				}
			case "linked parent":
				link := filepath.Join(base, "link")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				root = filepath.Join(link, "runtime")
			case "writable child":
				if err := os.Chmod(child, 0666); err != nil {
					t.Fatal(err)
				}
			case "linked child":
				if err := os.Symlink(child, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
			case "relative":
				root = "relative"
			case "missing":
				root = filepath.Join(root, "missing")
			}
			err := trustedRuntime(Inventory{Root: root})
			if (err == nil) != (name == "private") {
				t.Fatalf("unexpected runtime trust result: %v", err)
			}
		})
	}
}

func TestTrustedRuntimeComponentOwnership(t *testing.T) {
	foreign := uint32(os.Geteuid()) + 1
	for _, tc := range []struct {
		name           string
		uid            uint32
		mode           uint32
		ancestor, want bool
	}{
		{"owned directory", uint32(os.Geteuid()), unix.S_IFDIR | 0700, false, true},
		{"root file", 0, unix.S_IFREG | 0755, false, true},
		{"foreign directory", foreign, unix.S_IFDIR | 0700, true, false},
		{"foreign sticky directory", foreign, unix.S_IFDIR | unix.S_ISVTX | 0777, true, false},
		{"root sticky ancestor", 0, unix.S_IFDIR | unix.S_ISVTX | 0777, true, true},
		{"root sticky parent", 0, unix.S_IFDIR | unix.S_ISVTX | 0777, false, false},
		{"writable ancestor", 0, unix.S_IFDIR | 0777, true, false},
		{"link", 0, unix.S_IFLNK | 0700, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trustedExecutionComponent(unix.Stat_t{Uid: tc.uid, Mode: tc.mode}, tc.ancestor); got != tc.want {
				t.Fatalf("trusted=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecuteWorkerRequiresCompleteReceipt(t *testing.T) {
	for _, phase := range []string{"train", "reload"} {
		for _, field := range []string{"version", "phase", "step", "evalLoss", "reloaded"} {
			for _, form := range []string{"omitted", "null"} {
				t.Run(phase+"/"+field+"/"+form, func(t *testing.T) {
					receipt := map[string]any{"version": 1, "phase": phase, "step": 2, "evalLoss": 0, "reloaded": phase == "reload"}
					if form == "omitted" {
						delete(receipt, field)
					} else {
						receipt[field] = nil
					}
					raw, err := json.Marshal(receipt)
					if err != nil {
						t.Fatal(err)
					}
					script := "#!/bin/sh\nprintf '%s' '" + string(raw) + "' > \"$7\"\nchmod 600 \"$7\"\n"
					r, _, jobPath := executionFixture(t, script)
					if _, err := executeWorker(context.Background(), r, filepath.Dir(jobPath), executionGPU, phase); err == nil {
						t.Fatal("accepted incomplete or null receipt")
					}
				})
			}
		}
	}
}

func TestExecuteWorkerAcceptsZeroLoss(t *testing.T) {
	r, _, jobPath := executionFixture(t, strings.ReplaceAll(executionScript, `"evalLoss":1.5`, `"evalLoss":0`))
	for _, phase := range []string{"train", "reload"} {
		receipt, err := executeWorker(context.Background(), r, filepath.Dir(jobPath), executionGPU, phase)
		if err != nil || receipt.EvalLoss != 0 {
			t.Fatalf("zero loss rejected: %v", err)
		}
	}
}

func TestRunJobRequiresTrustedWritableRootAncestors(t *testing.T) {
	for _, destination := range []string{"output", "job", "terminal"} {
		for _, state := range []string{"private", "world writable", "group writable", "changed during execution"} {
			t.Run(destination+"/"+state, func(t *testing.T) {
				r, e, jobPath := executionFixture(t, executionScript)
				parent := filepath.Join(t.TempDir(), "parent")
				leaf := filepath.Join(parent, "private")
				if err := os.MkdirAll(leaf, 0700); err != nil {
					t.Fatal(err)
				}
				switch destination {
				case "output":
					r.OutputDirectory = leaf
				case "job":
					jobPath = filepath.Join(leaf, "job.json")
				case "terminal":
					e.TerminalFile = filepath.Join(leaf, "terminal.json")
				}
				marker := filepath.Join(t.TempDir(), "phases")
				script := strings.Replace(executionScript, "#!/bin/sh\n", "#!/bin/sh\nprintf '%s\\n' \"$5\" >> '"+marker+"'\n", 1)
				script += "if [ \"$5\" = train ]; then printf x > '" + filepath.Join(r.OutputDirectory, "adapter_model.safetensors") + "'; fi\n"
				if state == "changed during execution" {
					script += "if [ \"$5\" = reload ]; then chmod 777 '" + parent + "'; fi\n"
				}
				updateExecutionScript(t, r, &e, jobPath, script)
				switch state {
				case "world writable":
					if err := os.Chmod(parent, 0777); err != nil {
						t.Fatal(err)
					}
				case "group writable":
					if err := os.Chmod(parent, 0770); err != nil {
						t.Fatal(err)
					}
				}
				err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU)
				if state == "private" {
					if err != nil {
						t.Fatal("trusted writable root rejected", err)
					}
					return
				}
				if err == nil {
					t.Fatal("accepted writable root with unsafe ancestor")
				}
				if _, err := os.Lstat(e.TerminalFile); !os.IsNotExist(err) {
					t.Fatal("unsafe ancestry published terminal completion", err)
				}
				if _, err := os.Lstat(filepath.Join(r.OutputDirectory, "supervisor-provenance.json")); !os.IsNotExist(err) {
					t.Fatal("unsafe ancestry published provenance", err)
				}
				if state == "changed during execution" {
					phases, err := os.ReadFile(marker)
					if err != nil || string(phases) != "train\nreload\n" {
						t.Fatalf("final validation did not follow both phases: %q, %v", phases, err)
					}
				} else if _, err := os.Lstat(marker); !os.IsNotExist(err) {
					t.Fatal("unsafe writable root executed the worker", err)
				}
			})
		}
	}
}

func TestPrivateExecutionDirectoryAllowsRootOwnedStickyAncestor(t *testing.T) {
	var stat unix.Stat_t
	if unix.Lstat(os.TempDir(), &stat) != nil || stat.Uid != 0 || stat.Mode&unix.S_ISVTX == 0 {
		t.Skip("temporary directory is not a root-owned sticky ancestor")
	}
	root := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := privateExecutionDirectory(root); err != nil {
		t.Fatal("root-owned sticky ancestor rejected", err)
	}
}
