package modelseal

import (
	"errors"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/attestation"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

// Target names exactly one inventory root or one artifact file. The
// deployment selects the form; the envelope never selects it.
type Target struct {
	Root string
	File string
}

// inventory is the on-disk identity of one target.
type inventory struct {
	artifact attestation.Artifact
	// directory is set only for the files form, for later re-inspection.
	directory *llmtraining.Inventory
	digest    string
}

// path returns the root or the file of the target.
func (t Target) path() string {
	if t.Root != "" {
		return t.Root
	}
	return t.File
}

// take builds the gate inventory from a full walk of the effective artifact,
// never from evidence. It uses the shared training inventory rules, including
// shard index binding, then the narrower attestation path rules.
func (t Target) take() (inventory, error) {
	if (t.Root == "") == (t.File == "") {
		return inventory{}, errors.New("select exactly one inventory root or artifact file")
	}
	var result inventory
	if t.Root != "" {
		walked, err := llmtraining.InventoryDirectory(t.Root)
		if err != nil {
			return inventory{}, err
		}
		result.directory = &walked
		result.artifact = attestation.Artifact{Files: walked.Files}
	} else {
		digest, err := llmtraining.FileDigest(t.File)
		if err != nil {
			return inventory{}, err
		}
		result.artifact = attestation.Artifact{SHA256: digest}
	}
	digest, err := result.artifact.Digest()
	if err != nil {
		return inventory{}, errors.New("artifact paths are outside the attestation alphabet; the artifact cannot be sealed")
	}
	result.digest = digest
	return result, nil
}

// unchanged repeats the byte comparison of a directory inventory after code
// that could change files. Only the files form can be scanned.
func (i inventory) unchanged() bool {
	return i.directory != nil && i.directory.Inspect() == nil
}
