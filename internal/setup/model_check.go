package setup

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"golang.org/x/sys/unix"
)

// MaxModelCheckRequestBytes bounds the JSON of a model check, review or
// approval request (4 KiB).
const MaxModelCheckRequestBytes = 4096

// ModelCheckRequest asks setup to run the model checks of one accepted
// LLM workload. Source is the hub repository and the pinned revision of
// the model files. Setup stores it only in the check result.
type ModelCheckRequest struct {
	Version  int               `json:"version"`
	Workload string            `json:"workload"`
	Source   modelcheck.Source `json:"source"`
}

// ModelReviewRequest asks for the chat template review list of the stored
// result of one workload.
type ModelReviewRequest struct {
	Version  int    `json:"version"`
	Workload string `json:"workload"`
}

// ModelApproveRequest gives the approval of the operator for the review
// list that the operator read. The two SHA-256 values identify that list.
type ModelApproveRequest struct {
	Version            int    `json:"version"`
	Workload           string `json:"workload"`
	ChatTemplateSHA256 string `json:"chatTemplateSHA256"`
	ReviewSHA256       string `json:"reviewSHA256"`
}

// ModelReview is the chat template review list of the stored result of
// one workload. Pending is false when no stored result exists, when the
// result does not apply to the accepted launch, or when it asks for no
// review. Items hold the literals of the template: the desktop shows them
// to the operator. The setup log does not.
type ModelReview struct {
	Version            int                     `json:"version"`
	Workload           string                  `json:"workload"`
	Pending            bool                    `json:"pending"`
	TemplateFile       string                  `json:"templateFile,omitempty"`
	ChatTemplateSHA256 string                  `json:"chatTemplateSHA256,omitempty"`
	ReviewSHA256       string                  `json:"reviewSHA256,omitempty"`
	Items              []modelcheck.ReviewItem `json:"items"`
}

// Errors of the model check actions. They hold no path.
var (
	ErrModelCheckRequest  = errors.New("the model check request is not valid")
	ErrModelNotConfigured = errors.New("the workload is not in the accepted configuration")
	ErrModelNotActivated  = errors.New("model checks need an activated setup; finish setup first")
	ErrModelNotLLM        = errors.New("model checks apply only to LLM workloads")
	ErrModelNoDirectory   = errors.New("the workload names no model folder, so setup cannot check its files")
	ErrResultNotCurrent   = errors.New("the stored result does not apply to the accepted workload; run the checks again")
	// ErrStateBusy tells that another setup change held the state lock
	// until the time limit of the action. Nothing was stored.
	ErrStateBusy = errors.New("setup was busy with another change until the time limit, so nothing was stored; try again")
	// ErrApprovalCanceled tells that the operator canceled an approval
	// before setup stored it.
	ErrApprovalCanceled = errors.New("the approval was canceled; nothing was stored")
)

// digestPattern matches a SHA-256 as 64 lowercase hex characters.
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// modelRequest is a request of a model check action.
type modelRequest interface {
	ModelCheckRequest | ModelReviewRequest | ModelApproveRequest
	valid() bool
}

func (r ModelCheckRequest) valid() bool {
	return r.Version == 1 && control.ValidWorkloadID(control.Workload(r.Workload)) && r.Source.Valid()
}

func (r ModelReviewRequest) valid() bool {
	return r.Version == 1 && control.ValidWorkloadID(control.Workload(r.Workload))
}

func (r ModelApproveRequest) valid() bool {
	return r.Version == 1 && control.ValidWorkloadID(control.Workload(r.Workload)) &&
		digestPattern.MatchString(r.ChatTemplateSHA256) && digestPattern.MatchString(r.ReviewSHA256)
}

// decodeModelRequest reads one strict request of at most
// MaxModelCheckRequestBytes and validates it.
func decodeModelRequest[T modelRequest](r io.Reader) (T, error) {
	var request T
	if err := strictjson.DecodeLimited(r, MaxModelCheckRequestBytes, &request); err != nil {
		return request, err
	}
	if !request.valid() {
		return request, ErrModelCheckRequest
	}
	return request, nil
}

// DecodeModelCheckRequest reads one strict model check request.
func DecodeModelCheckRequest(r io.Reader) (ModelCheckRequest, error) {
	return decodeModelRequest[ModelCheckRequest](r)
}

// DecodeModelReviewRequest reads one strict review request.
func DecodeModelReviewRequest(r io.Reader) (ModelReviewRequest, error) {
	return decodeModelRequest[ModelReviewRequest](r)
}

// DecodeModelApproveRequest reads one strict approval request.
func DecodeModelApproveRequest(r io.Reader) (ModelApproveRequest, error) {
	return decodeModelRequest[ModelApproveRequest](r)
}

// checkTarget is an accepted LLM workload with a model directory.
type checkTarget struct {
	statePath, release string
	profile            control.WorkloadProfile
	root, pin          string
}

// sameLaunch tells whether two targets name the same state, release, model
// directory and chat template pin.
func (t checkTarget) sameLaunch(other checkTarget) bool {
	return t.statePath == other.statePath && t.release == other.release && t.root == other.root && t.pin == other.pin
}

// loadCheckTarget finds an accepted LLM workload that has a model
// directory. It refuses an image or unidentified workload and an LLM
// workload that names only a model ID, such as an adopted service.
func loadCheckTarget(ctx context.Context, home, workload string) (checkTarget, error) {
	profile, found, err := readOperatorProfile(home)
	if err != nil {
		return checkTarget{}, err
	}
	if !found {
		return checkTarget{}, ErrModelNotConfigured
	}
	snapshot, err := ReadCatalog(ctx, profile.StatePath)
	if err != nil {
		return checkTarget{}, err
	}
	p, ok := snapshot.Catalog.Profile(control.Workload(workload))
	switch {
	case !ok:
		return checkTarget{}, ErrModelNotConfigured
	case p.NativeModel == nil:
		return checkTarget{}, ErrModelNotLLM
	case p.NativeModel.Owned == nil || p.NativeModel.Owned.ModelPath == "":
		return checkTarget{}, ErrModelNoDirectory
	}
	return checkTarget{statePath: profile.StatePath, release: profile.ActivatedRelease, profile: p, root: p.NativeModel.Owned.ModelPath, pin: p.NativeModel.ChatTemplateSHA256}, nil
}

// loadWriteTarget is loadCheckTarget for an action that writes: setup must
// be activated, and the deployment must not be in maintenance or carry
// another release.
func loadWriteTarget(ctx context.Context, home, workload string) (checkTarget, error) {
	target, err := loadCheckTarget(ctx, home, workload)
	if err != nil {
		return target, err
	}
	if target.release == "" {
		return target, ErrModelNotActivated
	}
	return target, deployment.Check(target.statePath, target.release)
}

// gatePoll bounds the wait between two tries for the state gate. The wait
// starts at 10 ms and doubles up to this bound.
const gatePoll = time.Second

// stops are the errors of an action that stops before its write: a
// cancel, and the time limit of the action before the wait for the gate.
type stops struct{ canceled, timedOut error }

// checkStops are the stops of a run of the checks. approvalStops are the
// stops of an approval, which has no time limit of its own.
var (
	checkStops    = stops{canceled: modelcheck.ErrCheckCanceled, timedOut: modelcheck.ErrCheckTimedOut}
	approvalStops = stops{canceled: ErrApprovalCanceled, timedOut: ErrStateBusy}
)

// acquireGate waits for the state gate until the action context ends, so a
// finished run waits as long as its time limit allows. The time limit of
// the action during the wait means that another change held the gate:
// it gives ErrStateBusy. A cancel gives the cancel error of the action. A
// context that ended before the wait gives the stop of the action. Other
// errors pass through.
func acquireGate(ctx context.Context, statePath string, stop stops) (*lock.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, stop.of(err)
	}
	delay := 10 * time.Millisecond
	for {
		gate, err := lock.TryAcquire(statePath + ".lock")
		if err == nil {
			return gate, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrStateBusy
			}
			return nil, stop.canceled
		case <-timer.C:
		}
		delay = min(2*delay, gatePoll)
	}
}

// of gives the stop error for the end of a context.
func (s stops) of(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return s.timedOut
	}
	return s.canceled
}

// writeTarget runs write with the writable store, as the other setup
// write paths do. It holds the state gate, so no apply or activation runs
// during the write. Under the gate it checks the deployment again, and
// reads the target again: a workload whose state path, release, model
// directory or chat template pin changed after target was read gives
// ErrResultNotCurrent, and nothing is written.
func writeTarget(ctx context.Context, home string, target checkTarget, stop stops, write func(*store.Store, checkTarget) (ModelStatusEntry, error)) (ModelStatusEntry, error) {
	gate, err := acquireGate(ctx, target.statePath, stop)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	defer gate.Close()
	fresh, err := loadWriteTarget(ctx, home, string(target.profile.ID))
	if err != nil {
		return ModelStatusEntry{}, err
	}
	if !target.sameLaunch(fresh) {
		return ModelStatusEntry{}, ErrResultNotCurrent
	}
	st, err := store.Open(ctx, fresh.statePath)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	defer st.Close()
	return write(st, fresh)
}

// ModelCheck runs the checks of one accepted LLM workload and stores the
// result, keyed by the workload ID. client reads the hub. The run reuses a
// stored template approval for the same files; it never approves by
// itself. A run that is canceled, stops at its time limit, or whose
// checker cannot make a record, stores nothing. Else there are two cases:
//
//  1. The hub listing failed, and no result is stored, or the stored
//     result is for the same model directory and file digests. Setup
//     keeps that state and gives the entry of the run with NotStored.
//  2. Any other run replaces the stored result, also with findings or an
//     error. The entry describes the stored result.
func ModelCheck(ctx context.Context, home string, request ModelCheckRequest, client *http.Client) (ModelStatusEntry, error) {
	target, err := loadWriteTarget(ctx, home, request.Workload)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	prior, err := readStoredResult(ctx, target)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	checker := modelcheck.Checker{Client: client, Hub: modelcheck.DefaultHub, Version: modelcheck.CheckerVersion, Approver: storedApprover(prior)}
	record, err := checker.Run(ctx, modelcheck.Request{Model: request.Workload, Root: target.root, Source: request.Source})
	if err != nil {
		return ModelStatusEntry{}, err
	}
	return writeTarget(ctx, home, target, checkStops, func(st *store.Store, fresh checkTarget) (ModelStatusEntry, error) {
		if record.HubFailed() {
			stored, err := st.ModelCheck(ctx, request.Workload)
			kept := errors.Is(err, store.ErrNoModelCheck) || (err == nil && stored.Root == record.Root && maps.Equal(stored.Files, record.Files))
			if kept {
				entry := recordEntry(fresh, record)
				// The launch block is the block of the state that stays.
				entry.NotStored, entry.LaunchBlocked = true, err == nil && stored.AllowLaunch(fresh.root, fresh.pin) != nil
				return entry, nil
			}
		}
		if err := st.PutModelCheck(ctx, record); err != nil {
			return ModelStatusEntry{}, err
		}
		return storedEntry(ctx, st, fresh)
	})
}

// readStoredResult reads the stored result of the target from the state
// database, read-only. It gives nil without a result that reads as valid.
func readStoredResult(ctx context.Context, target checkTarget) (*modelcheck.Record, error) {
	db, err := readOnlyDB(target.statePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	record, _, err := storedModelCheck(ctx, db, string(target.profile.ID))
	return record, err
}

// storedApprover reuses the approval of prior for the same model directory,
// source, template and review list, as store.TemplateApprover does. Else
// it asks for review: setup never approves by itself. An approval that the
// operator stores during the run stays, see store.PutModelCheck.
func storedApprover(prior *modelcheck.Record) modelcheck.Approver {
	return func(_ context.Context, request modelcheck.ReviewRequest) (modelcheck.TemplateApproval, error) {
		if prior != nil && prior.TemplateApproval != nil && prior.Root == request.Root && prior.Source == request.Source && prior.TemplateApproval.Matches(request) {
			return *prior.TemplateApproval, nil
		}
		return modelcheck.TemplateApproval{}, modelcheck.ErrApprovalNeeded
	}
}

// storedEntry reads the stored result of the target again, for example
// with an approval that the store kept.
func storedEntry(ctx context.Context, st *store.Store, target checkTarget) (ModelStatusEntry, error) {
	record, err := st.ModelCheck(ctx, string(target.profile.ID))
	if err != nil {
		return ModelStatusEntry{}, err
	}
	return recordEntry(target, record), nil
}

// recordEntry gives the status entry of a result for the target.
func recordEntry(target checkTarget, record modelcheck.Record) ModelStatusEntry {
	entry, _, _ := baseEntry(target.profile)
	entry.Status, entry.Reason = modelcheck.StoredStatus(record, string(target.profile.ID), target.root, target.pin)
	describeRecord(&entry, record, target.root, target.pin)
	return entry
}

// ModelReviewList gives the chat template review list of the stored
// result of one workload. It changes nothing. A result that does not apply
// to the accepted launch has no pending review.
func ModelReviewList(ctx context.Context, home string, request ModelReviewRequest) (ModelReview, error) {
	review := ModelReview{Version: 1, Workload: request.Workload, Items: []modelcheck.ReviewItem{}}
	target, err := loadCheckTarget(ctx, home, request.Workload)
	if err != nil {
		return review, err
	}
	record, err := readStoredResult(ctx, target)
	if err != nil || record == nil {
		return review, err
	}
	pending, err := currentReview(*record, target)
	if err != nil {
		// A result that is not current, or that asks for no review, has
		// no pending review.
		return review, nil
	}
	review.Pending, review.TemplateFile, review.Items = true, pending.TemplateFile, pending.Review
	review.ChatTemplateSHA256, review.ReviewSHA256 = pending.ChatTemplateSHA256, pending.ReviewSHA256
	return review, nil
}

// currentReview gives the pending review of a result that applies to the
// target. A result that does not apply gives ErrResultNotCurrent; this
// includes a review of another template than the launch pins, see
// modelcheck.StoredStatus. A result without a pending review gives
// modelcheck.ErrNoPendingReview.
func currentReview(record modelcheck.Record, target checkTarget) (modelcheck.ReviewRequest, error) {
	if _, reason := modelcheck.StoredStatus(record, string(target.profile.ID), target.root, target.pin); reason != "" {
		return modelcheck.ReviewRequest{}, ErrResultNotCurrent
	}
	pending, ok := record.PendingReview()
	if !ok {
		return pending, modelcheck.ErrNoPendingReview
	}
	return pending, nil
}

// ModelApprove stores the approval of the operator with uid for the
// review list of the stored result. The request must name the template
// and review list SHA-256 values of the pending review, else it gives
// modelcheck.ErrApprovalMismatch. A result that changed after the read
// gives store.ErrModelCheckConflict: the desktop reads the list again.
func ModelApprove(ctx context.Context, home string, request ModelApproveRequest, uid int, now time.Time) (ModelStatusEntry, error) {
	target, err := loadWriteTarget(ctx, home, request.Workload)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	return writeTarget(ctx, home, target, approvalStops, func(st *store.Store, fresh checkTarget) (ModelStatusEntry, error) {
		record, err := st.ModelCheck(ctx, request.Workload)
		if err != nil {
			return ModelStatusEntry{}, err
		}
		pending, err := currentReview(record, fresh)
		if err != nil {
			return ModelStatusEntry{}, err
		}
		if pending.ChatTemplateSHA256 != request.ChatTemplateSHA256 || pending.ReviewSHA256 != request.ReviewSHA256 {
			return ModelStatusEntry{}, modelcheck.ErrApprovalMismatch
		}
		if err := st.ApproveTemplate(ctx, request.Workload, modelcheck.NewApproval(pending, uid, now)); err != nil {
			return ModelStatusEntry{}, err
		}
		return storedEntry(ctx, st, fresh)
	})
}
