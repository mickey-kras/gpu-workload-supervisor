package llmtraining

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

//go:embed worker.py
var workerSource []byte

type workerReceipt struct {
	Version  int     `json:"version"`
	Phase    string  `json:"phase"`
	Step     int     `json:"step"`
	EvalLoss float64 `json:"evalLoss"`
	Reloaded bool    `json:"reloaded"`
}

// RunJob is an experimental adapter, not runtime or GPU qualification. The
// catalog's trusted unit must contain it and all of its children for every phase.
func RunJob(ctx context.Context, jobPath, invocationID, gpuUUID string) error {
	if !absolute(jobPath) || !control.ValidGPUUUID(gpuUUID) {
		return errors.New("invalid training execution identity")
	}
	if err := privateExecutionDirectory(filepath.Dir(jobPath)); err != nil {
		return err
	}
	invocation, err := hex.DecodeString(invocationID)
	if err != nil || len(invocation) != 16 || hex.EncodeToString(invocation) != invocationID || invocationID == "00000000000000000000000000000000" {
		return errors.New("invalid training invocation")
	}
	e, err := readLaunchEnvelope(jobPath)
	if err != nil {
		return err
	}
	if err := privateExecutionDirectory(filepath.Dir(e.TerminalFile)); err != nil {
		return err
	}
	work := filepath.Join(filepath.Dir(jobPath), ".llm-"+invocationID)
	r, err := executionRecipe(e, jobPath, work)
	if err != nil {
		return err
	}
	if os.Mkdir(work, 0700) != nil {
		return errors.New("training execution directory already exists or cannot be created")
	}
	if err := prepareExecution(work, r); err != nil {
		return err
	}
	train, err := executeWorker(ctx, r, work, gpuUUID, "train")
	if err != nil {
		return err
	}
	adapter, err := adapterDigest(r)
	if err != nil {
		return err
	}
	if _, err := executeWorker(ctx, r, work, gpuUUID, "reload"); err != nil {
		return err
	}
	if err := unchangedExecutionInputs(jobPath, e, r); err != nil {
		return err
	}
	if err := publishProvenance(r, e.Job, invocationID, train.EvalLoss, adapter); err != nil {
		return err
	}
	receipt := control.TrainingTerminal{Version: 1, ID: e.Job.ID, Token: e.Job.Token, Fence: e.Job.Fence, RecipeSHA256: e.Job.RecipeSHA256, InvocationID: invocationID, Outcome: "completed"}
	return replaceEvidence(e.TerminalFile, receipt)
}

func executionRecipe(e jobEnvelope, jobPath, work string) (Recipe, error) {
	if _, err := compileJobRecipe(e, jobPath, filepath.Join(work, "plan.json")); err != nil {
		return Recipe{}, err
	}
	raw, err := readJobInput(e.RecipeFile, 8<<20, false)
	if err != nil {
		return Recipe{}, err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != e.Job.RecipeSHA256 {
		return Recipe{}, errors.New("training recipe changed")
	}
	r, err := ReadRecipe(bytes.NewReader(raw))
	if err != nil {
		return Recipe{}, err
	}
	if r.Runtime.Entrypoint != "bin/python3" {
		return Recipe{}, errors.New("training runtime requires an inventoried bin/python3")
	}
	if err := privateExecutionDirectory(r.OutputDirectory); err != nil {
		return Recipe{}, err
	}
	if r.Model.Inventory.Files["adapter_config.json"] != "" {
		return Recipe{}, errors.New("training requires a base model rather than an adapter redirect")
	}
	if err := trustedRuntime(r.Runtime.Inventory); err != nil {
		return Recipe{}, err
	}
	return r, nil
}

func trustedRuntime(inventory Inventory) error {
	if err := trustedExecutionAncestors(inventory.Root); err != nil {
		return err
	}
	remaining := 65536
	directories := []string{inventory.Root}
	for len(directories) != 0 {
		last := len(directories) - 1
		path := directories[last]
		directories = directories[:last]
		entries, err := boundedDirectoryEntries(path, remaining)
		if err != nil {
			return err
		}
		remaining -= len(entries)
		for _, entry := range entries {
			child := filepath.Join(path, entry.Name())
			var stat unix.Stat_t
			if unix.Lstat(child, &stat) != nil || !trustedExecutionComponent(stat, false) {
				return errors.New("training runtime ownership or permissions are unsafe")
			}
			if entry.IsDir() {
				directories = append(directories, child)
			}
		}
	}
	return nil
}

// Execution writes and child imports require a private leaf whose entire path
// is trusted; a private leaf alone can be replaced through a writable parent.
func privateExecutionDirectory(path string) error {
	if err := privateJobDirectory(path); err != nil {
		return err
	}
	return trustedExecutionAncestors(path)
}

func trustedExecutionAncestors(root string) error {
	if !absolute(root) {
		return errors.New("training execution path is invalid")
	}
	for path := root; ; path = filepath.Dir(path) {
		var stat unix.Stat_t
		// Like runtime launch-source qualification, only root-owned sticky
		// ancestors above the immediate parent may be writable (e.g. /tmp).
		stickyAncestor := path != root && path != filepath.Dir(root) && path != "/"
		if unix.Lstat(path, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || !trustedExecutionComponent(stat, stickyAncestor) {
			return errors.New("training execution ancestry is unsafe")
		}
		if path == "/" {
			return nil
		}
	}
}

func trustedExecutionComponent(stat unix.Stat_t, stickyAncestor bool) bool {
	writable := stat.Mode&0022 != 0
	if stickyAncestor && stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0 {
		writable = false
	}
	kind := stat.Mode & unix.S_IFMT
	return (stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())) && !writable && (kind == unix.S_IFDIR || kind == unix.S_IFREG)
}

func prepareExecution(work string, r Recipe) error {
	for _, name := range []string{"home", "cache"} {
		if os.Mkdir(filepath.Join(work, name), 0700) != nil {
			return errors.New("training private directories cannot be created")
		}
	}
	if os.WriteFile(filepath.Join(work, "worker.py"), workerSource, 0600) != nil {
		return errors.New("training worker cannot be written")
	}
	raw, err := json.Marshal(compileConfig(r))
	if err != nil || os.WriteFile(filepath.Join(work, "config.json"), raw, 0600) != nil {
		return errors.New("training configuration cannot be written")
	}
	return nil
}

func executionEnvironment(work, runtimeRoot, gpuUUID string) []string {
	return []string{
		"PATH=" + filepath.Join(runtimeRoot, "bin") + ":/usr/bin:/bin",
		"HOME=" + filepath.Join(work, "home"), "XDG_CONFIG_HOME=" + filepath.Join(work, "home"),
		"TMPDIR=" + work, "HF_HOME=" + filepath.Join(work, "cache"), "CUDA_VISIBLE_DEVICES=" + gpuUUID,
		"HF_HUB_OFFLINE=1", "HF_DATASETS_OFFLINE=1", "TRANSFORMERS_OFFLINE=1",
		"HF_HUB_DISABLE_TELEMETRY=1", "AXOLOTL_DO_NOT_TRACK=1", "DO_NOT_TRACK=1",
		"WANDB_MODE=disabled", "TOKENIZERS_PARALLELISM=false", "LANG=C.UTF-8",
	}
}

func executeWorker(ctx context.Context, r Recipe, work, gpuUUID, phase string) (workerReceipt, error) {
	var receipt workerReceipt
	path := filepath.Join(work, phase+".json")
	cmd := exec.CommandContext(ctx, filepath.Join(r.Runtime.Inventory.Root, r.Runtime.Entrypoint), "-I", "-S", "-B", filepath.Join(work, "worker.py"), phase, filepath.Join(work, "config.json"), path, filepath.Join(work, "progress.json"))
	cmd.Env = executionEnvironment(work, r.Runtime.Inventory.Root, gpuUUID)
	cmd.Dir = work
	// Child output may contain private examples, paths or third-party tracebacks.
	// Only the bounded structured receipt crosses the supervisor boundary.
	if cmd.Run() != nil {
		return receipt, errors.New("training worker failed; output is not eligible for import")
	}
	raw, err := readJobInput(path, 4096, true)
	if err != nil {
		return receipt, err
	}
	if strictjson.DecodeLimited(bytes.NewReader(raw), 4096, &receipt) != nil {
		return workerReceipt{}, errors.New("training worker acknowledgement is incomplete or invalid")
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !matchingObjectKeys(raw, canonical) || receipt.Version != 1 || receipt.Phase != phase || receipt.Step != r.MaxSteps || math.IsNaN(receipt.EvalLoss) || math.IsInf(receipt.EvalLoss, 0) || receipt.EvalLoss < 0 || receipt.Reloaded != (phase == "reload") {
		return workerReceipt{}, errors.New("training worker acknowledgement is incomplete or invalid")
	}
	return receipt, nil
}

func unchangedExecutionInputs(jobPath string, expected jobEnvelope, r Recipe) error {
	for _, path := range []string{filepath.Dir(jobPath), filepath.Dir(expected.TerminalFile), r.OutputDirectory} {
		if err := privateExecutionDirectory(path); err != nil {
			return err
		}
	}
	current, err := readLaunchEnvelope(jobPath)
	if err != nil || current != expected {
		return errors.New("training job identity changed during execution")
	}
	if matchFile(expected.RecipeFile, expected.Job.RecipeSHA256, 8<<20) != nil {
		return errors.New("training recipe changed during execution")
	}
	for _, inventory := range []Inventory{r.Model.Inventory, r.Tokenizer.Inventory, r.Runtime.Inventory} {
		if err := inventory.Inspect(); err != nil {
			return err
		}
	}
	if _, _, err := inspectDatasets(r.Train, r.HeldOut); err != nil {
		return err
	}
	return nil
}

func adapterDigest(r Recipe) (string, error) {
	adapter := filepath.Join(r.OutputDirectory, "adapter_model.safetensors")
	f, err := openRegular(adapter)
	if err != nil {
		return "", errors.New("trained adapter weights are missing")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 || uint64(info.Size()) > r.Budget.ExportBytes {
		return "", errors.New("trained adapter exceeds its export budget or is empty")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, int64(min(r.Budget.ExportBytes, 1<<40))+1))
	if err != nil {
		return "", errors.New("trained adapter could not be hashed")
	}
	if uint64(n) > r.Budget.ExportBytes || n > 1<<40 {
		return "", errors.New("trained adapter exceeds its export budget")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func publishProvenance(r Recipe, job control.TrainingJob, invocation string, loss float64, expectedAdapter string) error {
	digest, err := adapterDigest(r)
	if err != nil {
		return err
	}
	if digest != expectedAdapter {
		return errors.New("trained adapter changed during reload")
	}
	provenance := struct {
		Version           int     `json:"version"`
		SourceRevision    string  `json:"sourceRevision"`
		RecipeSHA256      string  `json:"recipeSHA256"`
		ModelRevision     string  `json:"modelRevision"`
		TokenizerRevision string  `json:"tokenizerRevision"`
		InvocationID      string  `json:"invocationId"`
		AdapterSHA256     string  `json:"adapterSHA256"`
		EvalLoss          float64 `json:"evalLoss"`
	}{1, SourceRevision, job.RecipeSHA256, r.Model.Revision, r.Tokenizer.Revision, invocation, digest, loss}
	raw, err := json.Marshal(provenance)
	if err != nil {
		return errors.New("training provenance cannot be encoded")
	}
	return publishPlan(filepath.Join(r.OutputDirectory, "supervisor-provenance.json"), append(raw, '\n'))
}

func replaceEvidence(path string, value control.TrainingTerminal) error {
	if _, err := os.Lstat(path); err == nil {
		if _, err := readJobInput(path, 64<<10, true); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return errors.New("training terminal destination is unreadable")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return errors.New("training terminal evidence cannot be encoded")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".terminal-*")
	if err != nil {
		return errors.New("training terminal staging failed")
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || os.Rename(f.Name(), path) != nil {
		return errors.New("training terminal publication failed")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("training terminal directory is unreadable")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errors.New("training terminal directory sync failed")
	}
	return nil
}
