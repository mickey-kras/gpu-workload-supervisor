package modelcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"unicode/utf8"
)

// TemplateApproval records that an operator read the review list of a
// chat template and accepted it. It binds the SHA-256 of the template text
// and the SHA-256 of the review list. Another template asks again.
type TemplateApproval struct {
	ChatTemplateSHA256 string    `json:"chatTemplateSHA256"`
	ReviewSHA256       string    `json:"reviewSHA256"`
	ApprovedAt         time.Time `json:"approvedAt"`
	ApproverUID        int       `json:"approverUID"`
}

// ReviewRequest asks an operator to read and approve the literals of a
// chat template. Review is in source order, with the line of each literal.
type ReviewRequest struct {
	Model  string
	Root   string
	Source Source
	// TemplateFile names the file that holds the template. The line of a
	// review item counts lines of the decoded template text, not lines of
	// a JSON file.
	TemplateFile       string
	ChatTemplateSHA256 string
	ReviewSHA256       string
	Review             []ReviewItem
}

// Approver gives the approval of an operator for a review request. It gives
// an error when no operator approved. It never approves by itself.
type Approver func(ctx context.Context, request ReviewRequest) (TemplateApproval, error)

// ErrApprovalNeeded tells that a chat template has literals that no
// operator approved. The model stays without a clean result until an
// operator approves them.
var ErrApprovalNeeded = errors.New("the chat template has literals that no operator approved")

// ErrApprovalRefused tells that the operator did not approve the review
// list.
var ErrApprovalRefused = errors.New("the operator did not approve the chat template literals")

// ErrNoPendingReview tells that a result does not ask for a template
// review, for example because an operator already approved it or a newer
// run replaced it. Read the result again before you approve.
var ErrNoPendingReview = errors.New("the result does not ask for a chat template review")

// ErrApprovalMismatch tells that an approval is not valid or is for another
// template or review list. The operator must read the review list again.
var ErrApprovalMismatch = errors.New("the approval does not match the chat template review of this result")

// ReviewDigest gives the SHA-256 of a review list.
func ReviewDigest(review []ReviewItem) string {
	raw, _ := json.Marshal(review)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// NewApproval gives the approval of request by the operator with uid at
// the time at.
func NewApproval(request ReviewRequest, uid int, at time.Time) TemplateApproval {
	return TemplateApproval{ChatTemplateSHA256: request.ChatTemplateSHA256, ReviewSHA256: request.ReviewSHA256, ApprovedAt: at.UTC(), ApproverUID: uid}
}

// Matches tells whether the approval is for this template and review list.
func (a TemplateApproval) Matches(request ReviewRequest) bool {
	return a.ChatTemplateSHA256 == request.ChatTemplateSHA256 && a.ReviewSHA256 == request.ReviewSHA256
}

// validFor tells whether the approval is valid and is for this template and
// review list.
func (a TemplateApproval) validFor(request ReviewRequest) bool {
	return a.valid() && a.Matches(request)
}

func (a TemplateApproval) valid() bool {
	return sha256Pattern.MatchString(a.ChatTemplateSHA256) && sha256Pattern.MatchString(a.ReviewSHA256) &&
		!a.ApprovedAt.IsZero() && a.ApprovedAt.Location() == time.UTC && a.ApproverUID >= 0
}

// buildReviewRequest gives the review request of a record, also when its
// template check does not ask for review. PendingReview gives it only then.
func (r Record) buildReviewRequest() ReviewRequest {
	return ReviewRequest{Model: r.Model, Root: r.Root, Source: r.Source, TemplateFile: r.TemplateFile, ChatTemplateSHA256: r.ChatTemplateSHA256, ReviewSHA256: ReviewDigest(r.TemplateReview), Review: r.TemplateReview}
}

// PendingReview gives the review request of a record whose template check
// asks for review, and true. Else it gives false.
func (r Record) PendingReview() (ReviewRequest, bool) {
	if len(r.Checks) != len(checkOrder) || !asksReview(r.templateCheck()) {
		return ReviewRequest{}, false
	}
	return r.buildReviewRequest(), true
}

// Approve gives the record with the approval of its template review. The
// record must ask for review, else it gives ErrNoPendingReview. The
// approval must be valid and match its template and review list, else it
// gives ErrApprovalMismatch.
func Approve(r Record, approval TemplateApproval) (Record, error) {
	request, ok := r.PendingReview()
	switch {
	case !ok:
		return Record{}, ErrNoPendingReview
	case !approval.validFor(request):
		return Record{}, ErrApprovalMismatch
	}
	r.Checks = append([]Check(nil), r.Checks...)
	r.Checks[templateIndex] = Check{Name: CheckTemplate, Outcome: OutcomePassed}
	r.TemplateApproval = &approval
	r.Status, r.Coverage = statusOf(r.Checks), coverageOf(r.Checks)
	return r, r.Validate()
}

// askApproval asks the approver of the checker for a review list. Without an
// approver, or when no operator approved, the check asks for review.
// Another error of the approver, or an approval that does not match, is an
// error.
func (c Checker) askApproval(ctx context.Context, request ReviewRequest) (Check, *TemplateApproval) {
	needsReview := Check{Name: CheckTemplate, Outcome: OutcomeFailed, Reason: ReasonTemplateNeedsReview}
	if c.Approver == nil {
		return needsReview, nil
	}
	approval, err := c.Approver(ctx, request)
	switch {
	case errors.Is(err, ErrApprovalNeeded), errors.Is(err, ErrApprovalRefused):
		return needsReview, nil
	case err != nil || !approval.validFor(request):
		return Check{Name: CheckTemplate, Outcome: OutcomeError, Reason: ReasonTemplateApprovalFailed}, nil
	}
	return Check{Name: CheckTemplate, Outcome: OutcomePassed}, &approval
}

// templateIndex is the position of the template check in checkOrder.
var templateIndex = slices.Index(checkOrder, CheckTemplate)

// templateCheck gives the template check of a record with all checks.
func (r Record) templateCheck() Check {
	return r.Checks[templateIndex]
}

// asksReview tells whether a template check asks for review. Only a failed
// check can ask for review.
func asksReview(check Check) bool {
	return check.Outcome == OutcomeFailed && check.Reason == ReasonTemplateNeedsReview
}

// validTemplate needs the template fields that follow from the template
// check. A check that asks for review needs a pin and a review list, and
// no approval. A passed check needs a pin, has no reason, and has an
// approval exactly when it has a review list. Each other check has no
// pin, no review list, no approval, and not the review reason.
func (r Record) validTemplate() bool {
	check := r.templateCheck()
	switch {
	case asksReview(check):
		return r.pinned() && len(r.TemplateReview) > 0 && validReview(r.TemplateReview) && r.TemplateApproval == nil
	case check.Outcome == OutcomePassed:
		return check.Reason == "" && r.pinned() && validReview(r.TemplateReview) && r.approvalMatchesReview()
	}
	return check.Reason != ReasonTemplateNeedsReview && r.ChatTemplateSHA256 == "" && r.TemplateFile == "" && len(r.TemplateReview) == 0 && r.TemplateApproval == nil
}

// pinned tells whether the record has a template pin and the name of a
// file that holds a template.
func (r Record) pinned() bool {
	return sha256Pattern.MatchString(r.ChatTemplateSHA256) && slices.Contains(templateFiles, r.TemplateFile)
}

// approvalMatchesReview tells whether a passed check has an approval that
// is valid for its review list, or has neither.
func (r Record) approvalMatchesReview() bool {
	if len(r.TemplateReview) == 0 {
		return r.TemplateApproval == nil
	}
	return r.TemplateApproval != nil && r.TemplateApproval.validFor(r.buildReviewRequest())
}

// validReview needs the bounds of the scan: at most maxReviewItems items
// and maxReviewBytes as JSON, each with a line and text.
func validReview(review []ReviewItem) bool {
	if raw, err := json.Marshal(review); err != nil || len(review) > maxReviewItems || len(raw) > maxReviewBytes {
		return false
	}
	for _, item := range review {
		if item.Line < 1 || item.Text == "" || !utf8.ValidString(item.Text) {
			return false
		}
	}
	return true
}
