package modelcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Request selects one local model directory and its upstream source.
type Request struct {
	Model  string
	Root   string
	Source Source
}

// Checker runs the checks. It reads the upstream tree from Hub with Client.
// Version names this checker in each record; a new version checks again.
type Checker struct {
	Client  *http.Client
	Hub     string
	Version string
	Now     func() time.Time
	// Approver asks an operator to approve the literals of a chat template.
	// Without it, a template with literals to review does not pass.
	Approver Approver
	// hub tunes the hub client: its listing timeout and retries. The zero
	// value gives the defaults. Tests in this package set it.
	hub hubOptions
}

// Run checks one model and gives a record bound to its file digests. A
// check that does not pass stops the checks after it. The behavioral scan
// is not available yet, so a model that passes the other checks stays not
// scanned. A canceled run gives no record, so the model stays not scanned.
func (c Checker) Run(ctx context.Context, r Request) (Record, error) {
	if c.Client == nil || !checkerPattern.MatchString(c.Version) || !currentConfiguration(c.Hub).valid() {
		return Record{}, errors.New("model checker needs a client, an HTTPS hub and a version")
	}
	if !modelPattern.MatchString(r.Model) || !cleanAbsolute(r.Root) || !r.Source.Valid() {
		return Record{}, errors.New("model check needs a model ID, a clean absolute root and a pinned source")
	}
	files, reason, err := Inventory(ctx, r.Root)
	if err := Stopped(ctx); err != nil {
		return Record{}, err
	}
	if err != nil && !errors.Is(err, errChanged) && !errors.Is(err, errUnreadable) {
		return Record{}, fmt.Errorf("model files cannot be inventoried: %w", err)
	}
	record := Record{
		Version: RecordVersion, Model: r.Model, Root: r.Root, Source: r.Source, Files: files,
		Checker: c.Version, Configuration: currentConfiguration(c.Hub), CheckedAt: c.now(),
	}
	allow := outcome(CheckAllowlist, reason, err)
	if allow.Outcome == OutcomePassed {
		reason, err := checkAllowlist(ctx, r.Root, files)
		allow = outcome(CheckAllowlist, reason, err)
	}
	results := []Check{allow}
	if allow.Outcome == OutcomePassed {
		results = append(results, c.identity(ctx, r, files))
	}
	if results[len(results)-1].Outcome == OutcomePassed {
		check, fields := c.template(ctx, record, files)
		record.ChatTemplateSHA256, record.TemplateFile = fields.pin, fields.file
		record.TemplateReview, record.TemplateApproval = fields.review, fields.approval
		results = append(results, check)
	}
	for _, name := range checkOrder[len(results):] {
		results = append(results, Check{Name: name, Outcome: OutcomeNotRun})
	}
	if err := Stopped(ctx); err != nil {
		return Record{}, err
	}
	record.Checks, record.Status, record.Coverage = results, statusOf(results), coverageOf(results)
	return record, record.Validate()
}

// Errors of a run that stopped before its end. Neither gives a record.
var (
	ErrCheckCanceled = errors.New("model check was canceled")
	ErrCheckTimedOut = errors.New("model check stopped at its time limit")
)

// Stopped gives the error of a run whose context ended: a time limit is
// not a cancel. It gives nil while ctx is live.
func Stopped(ctx context.Context) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ErrCheckTimedOut
	case ctx.Err() != nil:
		return ErrCheckCanceled
	}
	return nil
}

// templateFields are the record fields that the template check gives.
type templateFields struct {
	pin, file string
	review    []ReviewItem
	approval  *TemplateApproval
}

// template runs the chat template check on the model of record. A template
// with literals to review passes only with the approval of an operator. A
// check that does not pass the structure, or an approval error, gives no
// fields.
func (c Checker) template(ctx context.Context, record Record, files map[string]string) (Check, templateFields) {
	result, reason, err := checkTemplate(ctx, record.Root, files)
	check := outcome(CheckTemplate, reason, err)
	if check.Outcome != OutcomePassed {
		return check, templateFields{}
	}
	fields := templateFields{pin: result.pin, file: result.file, review: result.review}
	if len(result.review) == 0 {
		return check, fields
	}
	record.ChatTemplateSHA256, record.TemplateFile, record.TemplateReview = fields.pin, fields.file, fields.review
	check, fields.approval = c.askApproval(ctx, record.buildReviewRequest())
	if check.Outcome == OutcomeError {
		return check, templateFields{}
	}
	return check, fields
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Checker) identity(ctx context.Context, r Request, files map[string]string) Check {
	upstream, err := hubListing(c.Client, c.Hub, c.hub)(ctx, r.Source)
	if err != nil {
		return outcome(CheckIdentity, "", err)
	}
	reason, err := checkIdentity(ctx, r.Root, files, upstream, MaxGitBlobBytes)
	return outcome(CheckIdentity, reason, err)
}

// unsupportedReasons are the reasons that describe a model that the checks
// do not support, not a problem in its files.
var unsupportedReasons = map[string]bool{
	ReasonNoWeights: true, ReasonNoConfiguration: true, ReasonTooManyFiles: true, ReasonShardTooLarge: true, ReasonJSONTooLarge: true,
	ReasonNoTemplate: true, ReasonNamedTemplates: true, ReasonTemplateTooLarge: true, ReasonTemplateTooComplex: true,
	ReasonTemplateTooManyLiterals: true, ReasonTemplateReviewTooLarge: true,
}

// outcome maps a reason to a failed or unsupported check and an error to an
// error check. The error reason keeps its cause as a fixed code. A file that
// changes during the check is an error, not a finding.
func outcome(name, reason string, err error) Check {
	var upstream upstreamError
	switch {
	case errors.Is(err, errChanged):
		return Check{Name: name, Outcome: OutcomeError, Reason: ReasonFilesChanged}
	case errors.Is(err, errNotOnBranch):
		return Check{Name: name, Outcome: OutcomeFailed, Reason: ReasonNotOnBranch}
	case errors.As(err, &upstream):
		return Check{Name: name, Outcome: OutcomeError, Reason: upstream.reason}
	case err != nil:
		return Check{Name: name, Outcome: OutcomeError, Reason: ReasonUnreadable}
	case unsupportedReasons[reason]:
		return Check{Name: name, Outcome: OutcomeUnsupported, Reason: reason}
	case reason == ReasonTemplateCheckFailed:
		// A failure of the checker, not a problem in the files.
		return Check{Name: name, Outcome: OutcomeError, Reason: reason}
	case reason != "":
		return Check{Name: name, Outcome: OutcomeFailed, Reason: reason}
	}
	return Check{Name: name, Outcome: OutcomePassed}
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}
