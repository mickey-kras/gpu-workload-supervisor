package modelcheck

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Request selects one local model directory.
type Request struct {
	Model string
	Root  string
}

// Checker runs the checks. They read only the local model files.
// Version names this checker in each record; a new version checks again.
type Checker struct {
	Version string
	Now     func() time.Time
}

// Run checks one model and gives a record bound to its file digests. A
// check that does not pass stops the checks after it. The behavioral scan
// is not available yet, so a model that passes the other checks stays not
// scanned. A canceled run gives no record, so the model stays not scanned.
func (c Checker) Run(ctx context.Context, r Request) (Record, error) {
	if !checkerPattern.MatchString(c.Version) {
		return Record{}, errors.New("model checker needs a version")
	}
	if !modelPattern.MatchString(r.Model) || !cleanAbsolute(r.Root) {
		return Record{}, errors.New("model check needs a model ID and a clean absolute root")
	}
	files, stats, reason, err := InventoryWithStats(ctx, r.Root)
	if err := Stopped(ctx); err != nil {
		return Record{}, err
	}
	if err != nil && !errors.Is(err, errChanged) && !errors.Is(err, errUnreadable) {
		return Record{}, fmt.Errorf("model files cannot be inventoried: %w", err)
	}
	record := Record{
		Version: RecordVersion, Model: r.Model, Root: r.Root, Files: files, FileStats: stats,
		Checker: c.Version, Configuration: CurrentConfiguration(), CheckedAt: c.now(),
	}
	allow := outcome(CheckAllowlist, reason, err)
	if allow.Outcome == OutcomePassed {
		reason, err := checkAllowlist(ctx, r.Root, files)
		allow = outcome(CheckAllowlist, reason, err)
	}
	results := []Check{allow}
	if allow.Outcome == OutcomePassed {
		result, reason, err := checkTemplate(ctx, r.Root, files)
		check := outcome(CheckTemplate, reason, err)
		if check.Outcome == OutcomePassed {
			record.ChatTemplateSHA256, record.TemplateFile = result.pin, result.file
		}
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

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// unsupportedReasons are the reasons that describe a model that the checks
// do not support, not a problem in its files.
var unsupportedReasons = map[string]bool{
	ReasonNoWeights: true, ReasonNoConfiguration: true, ReasonTooManyFiles: true, ReasonShardTooLarge: true, ReasonJSONTooLarge: true,
	ReasonNoTemplate: true, ReasonNamedTemplates: true, ReasonTemplateTooLarge: true,
}

// outcome maps a reason to a failed or unsupported check and an error to an
// error check with a fixed reason. A file that changes during the check is
// an error, not a finding.
func outcome(name, reason string, err error) Check {
	switch {
	case errors.Is(err, errChanged):
		return Check{Name: name, Outcome: OutcomeError, Reason: ReasonFilesChanged}
	case err != nil:
		return Check{Name: name, Outcome: OutcomeError, Reason: ReasonUnreadable}
	case unsupportedReasons[reason]:
		return Check{Name: name, Outcome: OutcomeUnsupported, Reason: reason}
	case reason != "":
		return Check{Name: name, Outcome: OutcomeFailed, Reason: reason}
	}
	return Check{Name: name, Outcome: OutcomePassed}
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}
