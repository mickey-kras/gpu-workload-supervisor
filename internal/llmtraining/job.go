package llmtraining

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

type jobEnvelope struct {
	Version      int                 `json:"version"`
	Job          control.TrainingJob `json:"job"`
	RecipeFile   string              `json:"recipeFile"`
	TerminalFile string              `json:"terminalFile"`
}

// JobPlan binds preparation to a launch envelope without exposing its completion
// credential. It is neither terminal evidence nor permission to execute a runtime.
type JobPlan struct {
	Version         int              `json:"version"`
	ID              string           `json:"id"`
	Fence           control.Fence    `json:"fence"`
	Workload        control.Workload `json:"workload"`
	CatalogRevision string           `json:"catalogRevision"`
	RecipeSHA256    string           `json:"recipeSHA256"`
	InvocationID    string           `json:"invocationId"`
	Candidate       Plan             `json:"candidate"`
}

// PrepareJob audits the recipe from a launch-state supervisor envelope and writes
// a private, unqualified plan. The supplied invocation identifies preparation
// only: this offline check cannot prove a live systemd invocation or current fence.
// Inputs are inspected at a point in time; no files are locked or executed.
func PrepareJob(jobPath, planPath, invocationID string) error {
	invocation, err := hex.DecodeString(invocationID)
	if err != nil || len(invocation) != 16 || !absolute(jobPath) || !absolute(planPath) || jobPath == planPath {
		return errors.New("invalid job preparation paths or invocation identity")
	}
	if err := privateJobDirectory(filepath.Dir(jobPath)); err != nil {
		return err
	}
	if err := privateJobDirectory(filepath.Dir(planPath)); err != nil {
		return err
	}
	envelope, err := readLaunchEnvelope(jobPath)
	if err != nil {
		return err
	}
	candidate, err := compileJobRecipe(envelope, jobPath, planPath)
	if err != nil {
		return err
	}
	job := envelope.Job
	plan := JobPlan{Version: 1, ID: job.ID, Fence: job.Fence, Workload: job.Workload, CatalogRevision: job.CatalogRevision, RecipeSHA256: job.RecipeSHA256, InvocationID: invocationID, Candidate: candidate}
	output, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return errors.New("cannot encode job preparation plan")
	}
	return publishPlan(planPath, append(output, '\n'))
}

func readLaunchEnvelope(jobPath string) (jobEnvelope, error) {
	var envelope jobEnvelope
	raw, err := readJobInput(jobPath, 64<<10, true)
	if err != nil {
		return envelope, err
	}
	if !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 64<<10, &envelope) != nil {
		return jobEnvelope{}, errors.New("invalid training job envelope")
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !matchingObjectKeys(raw, canonical) || !validLaunchEnvelope(envelope) {
		return jobEnvelope{}, errors.New("invalid training job envelope")
	}
	return envelope, nil
}

func compileJobRecipe(envelope jobEnvelope, jobPath, planPath string) (Plan, error) {
	if planPath == envelope.RecipeFile || planPath == envelope.TerminalFile || jobPath == envelope.RecipeFile || jobPath == envelope.TerminalFile {
		return Plan{}, errors.New("job preparation paths overlap lifecycle files")
	}
	recipeRaw, err := readJobInput(envelope.RecipeFile, 8<<20, false)
	if err != nil {
		return Plan{}, err
	}
	digest := sha256.Sum256(recipeRaw)
	if hex.EncodeToString(digest[:]) != envelope.Job.RecipeSHA256 {
		return Plan{}, errors.New("training job recipe digest mismatch")
	}
	recipe, err := ReadRecipe(bytes.NewReader(recipeRaw))
	if err != nil {
		return Plan{}, err
	}
	if jobPathsOverlap(recipe, jobPath, planPath, envelope.RecipeFile, envelope.TerminalFile) {
		return Plan{}, errors.New("job lifecycle paths overlap training inputs or output")
	}
	ram, storage, err := available(recipe.OutputDirectory)
	if err != nil {
		return Plan{}, err
	}
	return Compile(recipe, ram, storage)
}

func jobPathsOverlap(recipe Recipe, paths ...string) bool {
	for _, source := range []string{recipe.OutputDirectory, recipe.Model.Inventory.Root, recipe.Tokenizer.Inventory.Root, recipe.Runtime.Inventory.Root, recipe.Train.Path, recipe.HeldOut.Path} {
		for _, path := range paths {
			if within(path, source) || within(source, path) {
				return true
			}
		}
	}
	return false
}

func validLaunchEnvelope(e jobEnvelope) bool {
	j := e.Job
	return e.Version == 1 && absolute(e.RecipeFile) && absolute(e.TerminalFile) && e.RecipeFile != e.TerminalFile &&
		control.ValidateRequestID(j.ID) == nil && control.ValidateRequestID(j.Token) == nil &&
		control.ValidateRequestID(j.CatalogRevision) == nil && control.ValidateRequestID(j.Fence.Incarnation) == nil &&
		j.Fence.Validate() == nil && control.ValidWorkloadID(j.Workload) && digestPattern.MatchString(j.RecipeSHA256) &&
		j.Status == "launching" && j.InvocationID == "" && j.TerminalOutcome == "" && !j.UpdatedAt.IsZero()
}

func privateJobDirectory(path string) error {
	if err := noLinks(path); err != nil {
		return err
	}
	var stat unix.Stat_t
	if unix.Lstat(path, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0077 != 0 || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return errors.New("job preparation directory must be trusted and private")
	}
	return nil
}

func readJobInput(path string, limit int64, private bool) ([]byte, error) {
	if err := noLinks(path); err != nil {
		return nil, err
	}
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var stat unix.Stat_t
	forbidden := uint32(0022)
	if private {
		forbidden = 0077
	}
	if unix.Fstat(int(f.Fd()), &stat) != nil || stat.Mode&forbidden != 0 || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return nil, errors.New("training job input ownership or permissions are unsafe")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("training job input is unreadable or exceeds its limit")
	}
	return raw, nil
}
