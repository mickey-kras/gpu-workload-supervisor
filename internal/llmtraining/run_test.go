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
			_, e, jobPath := executionFixture(t, script)
			err := RunJob(context.Background(), jobPath, executionInvocation, executionGPU)
			if err == nil || strings.Contains(err.Error(), e.Job.Token) {
				t.Fatal("accepted incomplete execution or disclosed completion token", err)
			}
			if _, err := os.Lstat(e.TerminalFile); !os.IsNotExist(err) {
				t.Fatal("failed execution published terminal completion", err)
			}
		})
	}
}

func TestRunJobRejectsUnsafeInputBeforeExecution(t *testing.T) {
	for _, name := range []string{"invocation", "gpu", "job path", "job permissions", "runtime permissions", "existing execution", "recipe changed", "invalid envelope", "output occupied", "output permissions", "runtime entrypoint", "adapter redirect", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			r, e, jobPath := executionFixture(t, executionScript)
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
