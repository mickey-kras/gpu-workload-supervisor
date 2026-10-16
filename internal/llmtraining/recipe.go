// Package llmtraining prepares local, unqualified Axolotl candidates. It never
// starts a trainer or establishes runtime, GPU or serving-import qualification.
package llmtraining

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// SourceRevision identifies the inspected configuration API, not a supported runtime.
const SourceRevision = "1f1eb05a391c43e65c1c5ee7ec6ea1b8a841d5d1"

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Inventory binds every regular file under a local directory. Matching these
// bytes does not approve their contents, plugins or external dependencies.
type Inventory struct {
	Root  string            `json:"root"`
	Files map[string]string `json:"files"`
}

type Snapshot struct {
	Revision  string    `json:"revision"`
	Inventory Inventory `json:"inventory"`
}

type Dataset struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Runtime struct {
	Inventory  Inventory `json:"inventory"`
	Entrypoint string    `json:"entrypoint"`
}

// Budget is an operator-supplied conservative estimate, never measured capacity.
// Storage includes all retained checkpoints plus one in-progress checkpoint.
type Budget struct {
	RAMBytes            uint64 `json:"ramBytes"`
	WeightsBytes        uint64 `json:"weightsBytes"`
	CacheBytes          uint64 `json:"cacheBytes"`
	CheckpointBytes     uint64 `json:"checkpointBytes"`
	RetainedCheckpoints uint64 `json:"retainedCheckpoints"`
	ExportBytes         uint64 `json:"exportBytes"`
	ReserveBytes        uint64 `json:"reserveBytes"`
}

type Recipe struct {
	Version              int      `json:"version"`
	Model                Snapshot `json:"model"`
	Tokenizer            Snapshot `json:"tokenizer"`
	Runtime              Runtime  `json:"runtime"`
	Train                Dataset  `json:"train"`
	HeldOut              Dataset  `json:"heldOut"`
	OutputDirectory      string   `json:"outputDirectory"`
	Adapter              string   `json:"adapter"`
	Rank                 int      `json:"rank"`
	Alpha                int      `json:"alpha"`
	SequenceLength       int      `json:"sequenceLength"`
	MicroBatchSize       int      `json:"microBatchSize"`
	GradientAccumulation int      `json:"gradientAccumulation"`
	MaxSteps             int      `json:"maxSteps"`
	LearningRate         float64  `json:"learningRate"`
	Seed                 int      `json:"seed"`
	Budget               Budget   `json:"budget"`
}

func ReadRecipe(r io.Reader) (Recipe, error) {
	var recipe Recipe
	raw, err := io.ReadAll(io.LimitReader(r, (8<<20)+1))
	if err != nil || !utf8.Valid(raw) || strictjson.DecodeLimited(bytes.NewReader(raw), 8<<20, &recipe) != nil {
		return Recipe{}, errors.New("invalid candidate recipe JSON")
	}
	if !canonicalRecipeKeys(raw, recipe) {
		return Recipe{}, errors.New("invalid candidate recipe JSON")
	}
	return recipe, recipe.Validate()
}

// Compare against the encoded typed recipe so JSON tags define exact field
// names. Inventory file maps retain their operator-selected keys in both trees.
func canonicalRecipeKeys(raw []byte, recipe Recipe) bool {
	canonical, err := json.Marshal(recipe)
	if err != nil {
		return false
	}
	return matchingObjectKeys(raw, canonical)
}

func matchingObjectKeys(supplied, expected json.RawMessage) bool {
	// Expected nodes come from json.Marshal: scalar and null leaves have no
	// field names. The typed decoder has already checked their value types.
	if len(expected) == 0 || expected[0] != '{' {
		return true
	}
	var got, want map[string]json.RawMessage
	if json.Unmarshal(expected, &want) != nil || json.Unmarshal(supplied, &got) != nil || got == nil {
		return false
	}
	for key, value := range got {
		next, exists := want[key]
		if !exists || !matchingObjectKeys(value, next) {
			return false
		}
	}
	return true
}

func (r Recipe) Validate() error {
	if r.Version != 1 || (r.Adapter != "lora" && r.Adapter != "qlora") {
		return errors.New("unsupported candidate recipe version or adapter")
	}
	if r.Rank < 1 || r.Rank > 64 || r.Alpha < 1 || r.Alpha > 128 || r.SequenceLength < 128 || r.SequenceLength > 4096 || r.MicroBatchSize < 1 || r.MicroBatchSize > 4 || r.GradientAccumulation < 1 || r.GradientAccumulation > 64 || r.MaxSteps < 1 || r.MaxSteps > 10000 || r.Seed < 0 || r.Seed > 2147483647 {
		return errors.New("candidate training parameters exceed supported bounds")
	}
	if !validLearningRate(r.LearningRate) {
		return errors.New("candidate learning rate exceeds supported bounds")
	}
	if !revisionPattern.MatchString(r.Model.Revision) || !revisionPattern.MatchString(r.Tokenizer.Revision) {
		return errors.New("model and tokenizer require exact revision identities")
	}
	if !absolute(r.OutputDirectory) || !absolute(r.Train.Path) || !absolute(r.HeldOut.Path) || r.Train.Path == r.HeldOut.Path || !digestPattern.MatchString(r.Train.SHA256) || !digestPattern.MatchString(r.HeldOut.SHA256) {
		return errors.New("invalid candidate output or dataset identity")
	}
	for _, inventory := range []Inventory{r.Model.Inventory, r.Tokenizer.Inventory, r.Runtime.Inventory} {
		if err := inventory.validate(); err != nil {
			return err
		}
	}
	if !relative(r.Runtime.Entrypoint) || r.Runtime.Inventory.Files[r.Runtime.Entrypoint] == "" {
		return errors.New("runtime entrypoint must be inventoried")
	}
	_, err := r.Budget.StorageBytes()
	return err
}

func validLearningRate(rate float64) bool {
	return !math.IsNaN(rate) && !math.IsInf(rate, 0) && rate >= 0.000001 && rate <= 0.001
}

func (i Inventory) validate() error {
	if !absolute(i.Root) || len(i.Files) == 0 || len(i.Files) > 32768 {
		return errors.New("invalid candidate inventory")
	}
	for path, digest := range i.Files {
		if !relative(path) || !digestPattern.MatchString(digest) {
			return errors.New("invalid candidate inventory entry")
		}
	}
	return nil
}

func absolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}

func relative(path string) bool {
	return filepath.IsLocal(path) && path != "." && filepath.Clean(path) == path && !strings.ContainsAny(path, "\\\x00\r\n")
}

func (b Budget) StorageBytes() (uint64, error) {
	if b.RAMBytes == 0 || b.WeightsBytes == 0 || b.CacheBytes == 0 || b.CheckpointBytes == 0 || b.ExportBytes == 0 || b.ReserveBytes == 0 || b.RetainedCheckpoints < 1 || b.RetainedCheckpoints > 8 {
		return 0, errors.New("candidate requires nonzero RAM and complete storage estimates; retain 1 to 8 checkpoints")
	}
	var total uint64
	for _, size := range []uint64{b.WeightsBytes, b.CacheBytes, b.ExportBytes, b.ReserveBytes} {
		if size > math.MaxUint64-total {
			return 0, errors.New("candidate storage estimate overflows")
		}
		total += size
	}
	if b.CheckpointBytes > (math.MaxUint64-total)/(b.RetainedCheckpoints+1) {
		return 0, errors.New("candidate checkpoint estimate overflows")
	}
	return total + b.CheckpointBytes*(b.RetainedCheckpoints+1), nil
}
