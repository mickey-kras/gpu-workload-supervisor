package modelcheck

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
)

// Request selects one local model directory and its upstream source.
type Request struct {
	Model  string
	Root   string
	Source Source
}

// Checker runs the checks. Listing reads the upstream tree. Version names
// this checker in each record.
type Checker struct {
	Listing Listing
	Version string
	Now     func() time.Time
}

// Inventory walks the model root with the shared training inventory rules:
// links, files that are not regular and invalid shard indexes fail it. The
// supervisor computes these digests itself.
func Inventory(root string) (map[string]string, error) {
	inventory, err := llmtraining.InventoryDirectory(root)
	if err != nil {
		return nil, err
	}
	if len(inventory.Files) > MaxModelFiles {
		return nil, errors.New("model has more than 4096 files")
	}
	return inventory.Files, nil
}

// Run checks one model and gives a record bound to its file digests. A
// check that fails stops the checks after it. The chat template check and
// the behavioral scan are not available yet, so a model that passes the
// first checks stays not scanned.
func (c Checker) Run(ctx context.Context, r Request) (Record, error) {
	if !modelPattern.MatchString(r.Model) || !cleanAbsolute(r.Root) || !r.Source.valid() {
		return Record{}, errors.New("model check needs a model ID, a clean absolute root and a pinned source")
	}
	files, err := Inventory(r.Root)
	if err != nil {
		return Record{}, errors.New("model files cannot be inventoried: remove links and files that are not regular files")
	}
	record := Record{Version: RecordVersion, Model: r.Model, Root: r.Root, Source: r.Source, Files: files, Checker: c.Version, CheckedAt: c.now()}
	results := []Check{c.allowlist(r.Root, files)}
	if results[0].Outcome == OutcomePassed {
		results = append(results, c.identity(ctx, r, files))
	}
	for _, name := range checkOrder[len(results):] {
		results = append(results, Check{Name: name, Outcome: OutcomeNotRun})
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return Record{}, errors.New("model check was cancelled")
	}
	record.Checks, record.Status = results, statusOf(results)
	return record, record.Validate()
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Checker) allowlist(root string, files map[string]string) Check {
	reason, err := checkAllowlist(root, files)
	return outcome(CheckAllowlist, reason, err)
}

func (c Checker) identity(ctx context.Context, r Request, files map[string]string) Check {
	upstream, err := c.Listing(ctx, r.Source)
	if err != nil {
		return Check{Name: CheckIdentity, Outcome: OutcomeError, Reason: "upstream-unavailable"}
	}
	reason, err := checkIdentity(r.Root, files, upstream)
	return outcome(CheckIdentity, reason, err)
}

// outcome maps a reason to a failed check and an error to an error check.
// A file that changes during the check is an error, not a finding.
func outcome(name, reason string, err error) Check {
	switch {
	case errors.Is(err, errChanged):
		return Check{Name: name, Outcome: OutcomeError, Reason: "files-changed"}
	case err != nil:
		return Check{Name: name, Outcome: OutcomeError, Reason: "unreadable"}
	case reason == ReasonNoWeights || reason == ReasonNoConfiguration:
		return Check{Name: name, Outcome: OutcomeUnsupported, Reason: reason}
	case reason != "":
		return Check{Name: name, Outcome: OutcomeFailed, Reason: reason}
	}
	return Check{Name: name, Outcome: OutcomePassed}
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}
