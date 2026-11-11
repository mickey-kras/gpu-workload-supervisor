package setup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
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
// one workload. Pending is false when the result applies to the accepted
// launch and asks for no review, or when no such result exists. Items
// hold the literals of the template: the desktop shows them to the
// operator. The setup log does not.
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
	ErrModelNotLLM        = errors.New("model checks apply only to LLM workloads")
	ErrModelNoDirectory   = errors.New("the workload names no model folder, so setup cannot check its files")
	ErrResultNotCurrent   = errors.New("the stored result does not apply to the accepted workload; run the checks again")
)

// digestPattern matches a SHA-256 as 64 lowercase hex characters.
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DecodeModelCheckRequest reads one strict request of at most
// MaxModelCheckRequestBytes and validates it.
func DecodeModelCheckRequest(r io.Reader) (ModelCheckRequest, error) {
	var request ModelCheckRequest
	if err := strictjson.DecodeLimited(r, MaxModelCheckRequestBytes, &request); err != nil {
		return request, err
	}
	if request.Version != 1 || !control.ValidWorkloadID(control.Workload(request.Workload)) || !request.Source.Valid() {
		return request, ErrModelCheckRequest
	}
	return request, nil
}

// DecodeModelReviewRequest reads one strict review request.
func DecodeModelReviewRequest(r io.Reader) (ModelReviewRequest, error) {
	var request ModelReviewRequest
	if err := strictjson.DecodeLimited(r, MaxModelCheckRequestBytes, &request); err != nil {
		return request, err
	}
	if request.Version != 1 || !control.ValidWorkloadID(control.Workload(request.Workload)) {
		return request, ErrModelCheckRequest
	}
	return request, nil
}

// DecodeModelApproveRequest reads one strict approval request.
func DecodeModelApproveRequest(r io.Reader) (ModelApproveRequest, error) {
	var request ModelApproveRequest
	if err := strictjson.DecodeLimited(r, MaxModelCheckRequestBytes, &request); err != nil {
		return request, err
	}
	if request.Version != 1 || !control.ValidWorkloadID(control.Workload(request.Workload)) ||
		!digestPattern.MatchString(request.ChatTemplateSHA256) || !digestPattern.MatchString(request.ReviewSHA256) {
		return request, ErrModelCheckRequest
	}
	return request, nil
}

// checkTarget is an accepted LLM workload with a model directory.
type checkTarget struct {
	statePath string
	profile   control.WorkloadProfile
	root, pin string
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
	return checkTarget{statePath: profile.StatePath, profile: p, root: p.NativeModel.Owned.ModelPath, pin: p.NativeModel.ChatTemplateSHA256}, nil
}

// openCheckStore opens the existing state database for a write.
func openCheckStore(ctx context.Context, path string) (*store.Store, error) {
	file, err := deployment.OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	file.Close()
	return store.Open(ctx, path)
}

// ModelCheck runs the checks of one accepted LLM workload and stores the
// result, keyed by the workload ID. client reads the hub. The run reuses a
// stored template approval for the same files; it never approves by
// itself. A canceled or failed run stores nothing. A run whose hub
// listing failed keeps the earlier stored result: it stores nothing and
// gives the entry of the run with NotStored. Each other run replaces the
// stored result, also with findings or an error. The entry describes the
// stored result.
func ModelCheck(ctx context.Context, home string, request ModelCheckRequest, client *http.Client) (ModelStatusEntry, error) {
	target, err := loadCheckTarget(ctx, home, request.Workload)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	st, err := openCheckStore(ctx, target.statePath)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	defer st.Close()
	checker := modelcheck.Checker{Client: client, Hub: modelcheck.DefaultHub, Version: modelcheck.CheckerVersion, Approver: st.TemplateApprover(nil, nil)}
	record, err := checker.Run(ctx, modelcheck.Request{Model: request.Workload, Root: target.root, Source: request.Source})
	if err != nil {
		return ModelStatusEntry{}, err
	}
	if record.HubFailed() {
		entry := recordEntry(target, record)
		entry.NotStored = true
		return entry, nil
	}
	if err := st.PutModelCheck(ctx, record); err != nil {
		return ModelStatusEntry{}, err
	}
	return storedEntry(ctx, st, target)
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
	entry, root, pin := baseEntry(target.profile)
	entry.Status, entry.Reason = modelcheck.StoredStatus(record, string(target.profile.ID), root, pin)
	describeRecord(&entry, record, root, pin)
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
	db, err := readOnlyDB(target.statePath)
	if err != nil {
		return review, err
	}
	defer db.Close()
	record, found, err := storedModelCheck(ctx, db, request.Workload)
	if err != nil || !found || record == nil {
		return review, err
	}
	pending, ok := currentReview(*record, target)
	if !ok {
		return review, nil
	}
	review.Pending, review.TemplateFile, review.Items = true, pending.TemplateFile, pending.Review
	review.ChatTemplateSHA256, review.ReviewSHA256 = pending.ChatTemplateSHA256, pending.ReviewSHA256
	return review, nil
}

// currentReview gives the pending review of a result that applies to the
// target.
func currentReview(record modelcheck.Record, target checkTarget) (modelcheck.ReviewRequest, bool) {
	if _, reason := modelcheck.StoredStatus(record, string(target.profile.ID), target.root, target.pin); reason != "" {
		return modelcheck.ReviewRequest{}, false
	}
	return record.PendingReview()
}

// ModelApprove stores the approval of the operator with uid for the
// review list of the stored result. The request must name the template
// and review list SHA-256 values of the pending review, else it gives
// modelcheck.ErrApprovalMismatch. A result that changed after the read
// gives store.ErrModelCheckConflict: the desktop reads the list again.
func ModelApprove(ctx context.Context, home string, request ModelApproveRequest, uid int, now time.Time) (ModelStatusEntry, error) {
	target, err := loadCheckTarget(ctx, home, request.Workload)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	st, err := openCheckStore(ctx, target.statePath)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	defer st.Close()
	record, err := st.ModelCheck(ctx, request.Workload)
	if err != nil {
		return ModelStatusEntry{}, err
	}
	if _, reason := modelcheck.StoredStatus(record, request.Workload, target.root, target.pin); reason != "" {
		return ModelStatusEntry{}, ErrResultNotCurrent
	}
	pending, ok := record.PendingReview()
	switch {
	case !ok:
		return ModelStatusEntry{}, modelcheck.ErrNoPendingReview
	case pending.ChatTemplateSHA256 != request.ChatTemplateSHA256 || pending.ReviewSHA256 != request.ReviewSHA256:
		return ModelStatusEntry{}, modelcheck.ErrApprovalMismatch
	}
	if err := st.ApproveTemplate(ctx, request.Workload, modelcheck.NewApproval(pending, uid, now)); err != nil {
		return ModelStatusEntry{}, err
	}
	return storedEntry(ctx, st, target)
}
