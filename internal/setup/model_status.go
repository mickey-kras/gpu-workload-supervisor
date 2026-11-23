package setup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

// ModelStatusRequest asks for the model check status of the accepted
// workloads. Recheck names the workloads whose model files setup reads
// again, when their stored result applies; other workloads use the stored
// digests.
type ModelStatusRequest struct {
	Version int      `json:"version"`
	Recheck []string `json:"recheck,omitempty"`
}

// The kinds of a status entry. An image workload has no backdoor scan. An
// unidentified workload names no model files that setup can check.
const (
	ModelKindLLM          = "llm"
	ModelKindImage        = "image"
	ModelKindUnidentified = "unidentified"
)

// ReasonFilesUnidentified marks a workload whose model files setup cannot
// identify.
const ReasonFilesUnidentified = "files-unidentified"

// ReasonAdoptedLaunch marks an adopted vLLM launch: it names no model
// directory, so setup cannot check it and the launch gate refuses it.
const ReasonAdoptedLaunch = "adopted-launch"

// A status request names at most the 32 workloads of a catalog. Its JSON
// is at most MaxModelStatusRequestBytes (8 KiB).
const (
	maxModelStatusRecheck      = 32
	MaxModelStatusRequestBytes = 8192
)

// ModelStatusEntry is the read-only check status of one workload. It holds
// no path, file name or template text: the desktop shows it as is.
// Identity is the SHA-256 of the model directory path or the model ID of
// the accepted launch, see ModelIdentity. It is not a digest of the files.
// TemplateSHA256 is the chat template pin of the accepted launch. The
// desktop compares both with the launch that it shows.
type ModelStatusEntry struct {
	Workload           string              `json:"workload"`
	Label              string              `json:"label"`
	Kind               string              `json:"kind"`
	Status             modelcheck.Status   `json:"status"`
	Reason             string              `json:"reason,omitempty"`
	StaticChecksPassed bool                `json:"staticChecksPassed"`
	Rechecked          bool                `json:"rechecked"`
	Coverage           modelcheck.Coverage `json:"coverage,omitempty"`
	Checker            string              `json:"checker,omitempty"`
	CheckedAt          *time.Time          `json:"checkedAt,omitempty"`
	Checks             []modelcheck.Check  `json:"checks,omitempty"`
	Identity           string              `json:"identity,omitempty"`
	TemplateSHA256     string              `json:"templateSHA256,omitempty"`
	// CheckAvailable marks an LLM workload with a model directory, which
	// gpu-setup model-check can check.
	CheckAvailable bool `json:"checkAvailable"`
	// LaunchBlocked marks an LLM workload that the launch gate refuses: no
	// stored result, a result that cannot be read, or a result that the gate
	// refuses.
	LaunchBlocked bool `json:"launchBlocked"`
}

// ModelIdentity returns the SHA-256 of "path:" plus the model directory of a
// managed launch, or of "model:" plus the model ID. It reads no file, and the
// result does not expose the path in clear text.
func ModelIdentity(model, root string) string {
	name := "model:" + model
	if root != "" {
		name = "path:" + root
	}
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// ModelStatusReport lists the status of each accepted workload in catalog
// order.
type ModelStatusReport struct {
	Version int                `json:"version"`
	Models  []ModelStatusEntry `json:"models"`
}

// validate refuses an unknown version and recheck names that are not
// workload IDs.
func (r ModelStatusRequest) validate() error {
	if r.Version != 1 {
		return errors.New("unsupported model status request version")
	}
	if len(r.Recheck) > maxModelStatusRecheck {
		return fmt.Errorf("model status request names more than %d workloads", maxModelStatusRecheck)
	}
	for _, id := range r.Recheck {
		if !control.ValidWorkloadID(control.Workload(id)) {
			return errors.New("model status request names an invalid workload")
		}
	}
	return nil
}

// ModelStatus reports the check status of each accepted workload. It is
// read-only: it reads the operator profile, the accepted catalog and the
// stored results. Only workloads in request.Recheck have their model files
// reread; canceling ctx stops that read.
func ModelStatus(ctx context.Context, home string, request ModelStatusRequest) (ModelStatusReport, error) {
	report := ModelStatusReport{Version: 1, Models: []ModelStatusEntry{}}
	if err := request.validate(); err != nil {
		return report, err
	}
	profile, found, err := readOperatorProfile(home)
	if err != nil || !found {
		return report, err
	}
	snapshot, err := ReadCatalog(ctx, profile.StatePath)
	if err != nil || len(snapshot.Catalog.Profiles) == 0 {
		return report, err
	}
	db, err := readOnlyDB(profile.StatePath)
	if err != nil {
		return report, err
	}
	defer db.Close()
	// A state database before schema version 18 has no stored results.
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='model_checks'").Scan(&tables); err != nil {
		return report, err
	}
	for _, p := range snapshot.Catalog.Profiles {
		entry, err := modelStatusEntry(ctx, db, tables != 0, p, slices.Contains(request.Recheck, string(p.ID)))
		if err != nil {
			return report, err
		}
		report.Models = append(report.Models, entry)
	}
	return report, nil
}

// modelStatusEntry returns the status of one workload. An unreadable stored
// result leaves the workload not_scanned, and the launch gate refuses it.
// Without a results table, no workload has a result.
func modelStatusEntry(ctx context.Context, db *sql.DB, results bool, p control.WorkloadProfile, recheck bool) (ModelStatusEntry, error) {
	entry, root, pin := baseEntry(p)
	if entry.Kind != ModelKindLLM || !results {
		return entry, nil
	}
	record, found, err := storedModelCheck(ctx, db, string(p.ID))
	if err != nil || !found {
		return entry, err
	}
	entry.Reason, entry.LaunchBlocked = modelcheck.ReasonResultUnusable, true
	if record == nil {
		return entry, nil
	}
	entry.Status, entry.Reason = modelcheck.StoredStatus(*record, string(p.ID), root, pin)
	if recheck && entry.Reason == "" {
		if err := recheckEntry(ctx, &entry, *record, root, pin); err != nil {
			return entry, fmt.Errorf("recheck %s: %w", p.ID, err)
		}
	}
	describeRecord(&entry, *record, root, pin)
	return entry, nil
}

// baseEntry returns the entry of a workload without a stored result, plus
// the model directory and chat template pin of its launch.
func baseEntry(p control.WorkloadProfile) (entry ModelStatusEntry, root, pin string) {
	entry = ModelStatusEntry{Workload: string(p.ID), Label: p.Label, Kind: modelKind(p), Status: modelcheck.StatusNotScanned}
	if entry.Kind != ModelKindLLM {
		if entry.Kind == ModelKindUnidentified {
			entry.Reason = ReasonFilesUnidentified
		}
		return entry, "", ""
	}
	if p.NativeModel.Owned != nil {
		root = p.NativeModel.Owned.ModelPath
	}
	pin = p.NativeModel.ChatTemplateSHA256
	entry.Identity, entry.TemplateSHA256 = ModelIdentity(p.NativeModel.Model, root), pin
	// The launch gate refuses an LLM workload without a stored result.
	entry.Reason, entry.CheckAvailable, entry.LaunchBlocked = modelcheck.ReasonNoResult, root != "", true
	if root == "" {
		entry.Reason = ReasonAdoptedLaunch
	}
	return entry, root, pin
}

// describeRecord adds the fields of a result to entry, after its status
// and reason are set. The static checks count only for a result that
// applies.
func describeRecord(entry *ModelStatusEntry, record modelcheck.Record, root, pin string) {
	if entry.Reason == "" {
		entry.StaticChecksPassed = record.StaticChecksPassed()
	}
	checkedAt := record.CheckedAt
	entry.Coverage, entry.Checker, entry.CheckedAt, entry.Checks = record.Coverage, record.Checker, &checkedAt, record.Checks
	entry.LaunchBlocked = record.AllowLaunch(root, pin) != nil
}

// recheckEntry reads the model files of a stored result that applies, and
// sets the status of entry. A canceled read leaves entry as it was.
func recheckEntry(ctx context.Context, entry *ModelStatusEntry, record modelcheck.Record, root, pin string) error {
	status, reason, err := modelcheck.RecheckStatus(ctx, record, entry.Workload, root, pin)
	if err != nil {
		return err
	}
	entry.Status, entry.Reason, entry.Rechecked = status, reason, true
	return nil
}

// modelKind sorts a workload by the model files that it names.
func modelKind(p control.WorkloadProfile) string {
	switch {
	case p.NativeModel != nil:
		return ModelKindLLM
	case p.LaunchBinding != nil && p.LaunchBinding.Runtime == "comfyui":
		return ModelKindImage
	}
	return ModelKindUnidentified
}

// storedModelCheck reads the stored result of one model from the state
// database. found is false without a result, or with a result of another
// record version. A result that does not read as valid gives a nil record.
func storedModelCheck(ctx context.Context, db *sql.DB, model string) (record *modelcheck.Record, found bool, err error) {
	var raw []byte
	err = db.QueryRowContext(ctx, `SELECT payload FROM model_checks WHERE model=?`, model).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	stored, err := modelcheck.ReadRecord(raw)
	if errors.Is(err, modelcheck.ErrStaleRecord) {
		return nil, false, nil
	}
	if err != nil || stored.Model != model {
		return nil, true, nil
	}
	return &stored, true, nil
}

// readOperatorProfile reads the operator profile of setup. found is false
// when setup has no profile yet.
func readOperatorProfile(home string) (profile Profile, found bool, err error) {
	data, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if errors.Is(err, os.ErrNotExist) {
		return profile, false, nil
	}
	if err != nil {
		return profile, false, err
	}
	if err := json.Unmarshal(data, &profile); err != nil {
		return profile, false, err
	}
	return profile, true, nil
}
