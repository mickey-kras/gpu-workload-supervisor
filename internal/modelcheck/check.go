package modelcheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Request selects one local model directory and its recorded provenance source.
type Request struct {
	Model  string
	Root   string
	Source Source
}

// Checker validates local safetensors artifacts, JSON redirects, and chat template digests.
type Checker struct {
	Client  *http.Client
	Hub     string
	Version string
	Now     func() time.Time
}

var (
	ErrCheckCanceled = errors.New("model check was canceled")
	ErrCheckTimedOut = errors.New("model check stopped at its time limit")
)

// Stopped maps context cancellation or deadline expiry to typed check errors.
func Stopped(ctx context.Context) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return ErrCheckTimedOut
	case ctx.Err() != nil:
		return ErrCheckCanceled
	}
	return nil
}

// Run inventories the model directory, verifies the safetensors/JSON allowlist, and records any chat template pin.
func (c Checker) Run(ctx context.Context, r Request) (Record, error) {
	if !checkerPattern.MatchString(c.Version) || !currentConfiguration(c.effectiveHub()).valid() {
		return Record{}, errors.New("model checker needs a valid HTTPS hub and version")
	}
	if !modelPattern.MatchString(r.Model) || !cleanAbsolute(r.Root) || !r.Source.Valid() {
		return Record{}, errors.New("model check needs a model ID, a clean absolute root and a pinned source")
	}
	files, reason, err := Inventory(ctx, r.Root)
	if stopErr := Stopped(ctx); stopErr != nil {
		return Record{}, stopErr
	}
	if err != nil && !errors.Is(err, errChanged) && !errors.Is(err, errUnreadable) {
		return Record{}, fmt.Errorf("model files cannot be inventoried: %w", err)
	}
	record := Record{
		Version:       RecordVersion,
		Model:         r.Model,
		Root:          r.Root,
		Source:        r.Source,
		Files:         files,
		Checker:       c.Version,
		Configuration: currentConfiguration(c.effectiveHub()),
		CheckedAt:     c.now(),
	}
	allow := outcome(CheckAllowlist, reason, err)
	if allow.Outcome == OutcomePassed {
		rAllow, allowErr := checkAllowlist(ctx, r.Root, files)
		allow = outcome(CheckAllowlist, rAllow, allowErr)
	}
	tplCheck := Check{Name: CheckTemplate, Outcome: OutcomeNotRun}
	if allow.Outcome == OutcomePassed {
		pin, tplFile, tplReason, tplErr := resolveTemplatePin(ctx, r.Root, files)
		tplCheck = outcome(CheckTemplate, tplReason, tplErr)
		if tplCheck.Outcome == OutcomePassed {
			record.ChatTemplateSHA256 = pin
			record.TemplateFile = tplFile
		}
	}
	if stopErr := Stopped(ctx); stopErr != nil {
		return Record{}, stopErr
	}
	results := []Check{allow, tplCheck}
	record.Checks, record.Status, record.Coverage = results, statusOf(results), coverageOf(results)
	return record, record.Validate()
}

func (c Checker) effectiveHub() string {
	if c.Hub == "" {
		return DefaultHub
	}
	return c.Hub
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func outcome(name, reason string, err error) Check {
	switch {
	case errors.Is(err, errChanged):
		return Check{Name: name, Outcome: OutcomeError, Reason: ReasonFilesChanged}
	case err != nil:
		return Check{Name: name, Outcome: OutcomeError, Reason: ReasonUnreadable}
	case reason != "":
		return Check{Name: name, Outcome: OutcomeFailed, Reason: reason}
	default:
		return Check{Name: name, Outcome: OutcomePassed}
	}
}
