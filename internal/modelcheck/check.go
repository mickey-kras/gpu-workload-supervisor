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
}

// Run checks one model and gives a record bound to its file digests. A
// check that does not pass stops the checks after it. The chat template
// check and the behavioral scan are not available yet, so a model that
// passes the first checks stays not scanned. A canceled run gives no
// record, so the model stays not scanned.
func (c Checker) Run(ctx context.Context, r Request) (Record, error) {
	if c.Client == nil || !checkerPattern.MatchString(c.Version) || !currentConfiguration(c.Hub).valid() {
		return Record{}, errors.New("model checker needs a client, an HTTPS hub and a version")
	}
	if !modelPattern.MatchString(r.Model) || !cleanAbsolute(r.Root) || !r.Source.valid() {
		return Record{}, errors.New("model check needs a model ID, a clean absolute root and a pinned source")
	}
	files, reason, err := Inventory(ctx, r.Root)
	if ctx.Err() != nil {
		return Record{}, errors.New("model check was canceled")
	}
	if err != nil && !errors.Is(err, errChanged) {
		return Record{}, fmt.Errorf("model files cannot be inventoried: %w", err)
	}
	record := Record{
		Version: RecordVersion, Model: r.Model, Root: r.Root, Source: r.Source, Files: files,
		Checker: c.Version, Configuration: currentConfiguration(c.Hub), CheckedAt: c.now(),
	}
	allow := outcome(CheckAllowlist, reason, err)
	if allow.Outcome == OutcomePassed {
		reason, err := checkAllowlist(r.Root, files)
		allow = outcome(CheckAllowlist, reason, err)
	}
	results := []Check{allow}
	if allow.Outcome == OutcomePassed {
		results = append(results, c.identity(ctx, r, files))
	}
	for _, name := range checkOrder[len(results):] {
		results = append(results, Check{Name: name, Outcome: OutcomeNotRun})
	}
	if ctx.Err() != nil {
		return Record{}, errors.New("model check was canceled")
	}
	record.Checks, record.Status, record.Coverage = results, statusOf(results), coverageOf(results)
	return record, record.Validate()
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c Checker) identity(ctx context.Context, r Request, files map[string]string) Check {
	upstream, err := HubListing(c.Client, c.Hub)(ctx, r.Source)
	if err != nil {
		return outcome(CheckIdentity, "", err)
	}
	reason, err := checkIdentity(r.Root, files, upstream)
	return outcome(CheckIdentity, reason, err)
}

// unsupportedReasons are the reasons that describe a model that the checks
// do not support, not a problem in its files.
var unsupportedReasons = map[string]bool{ReasonNoWeights: true, ReasonNoConfiguration: true, ReasonTooManyFiles: true, ReasonShardTooLarge: true, ReasonJSONTooLarge: true}

// outcome maps a reason to a failed or unsupported check and an error to an
// error check. The error reason keeps its cause as a fixed code. A file that
// changes during the check is an error, not a finding.
func outcome(name, reason string, err error) Check {
	var upstream upstreamError
	switch {
	case errors.Is(err, errChanged):
		return Check{Name: name, Outcome: OutcomeError, Reason: "files-changed"}
	case errors.As(err, &upstream):
		return Check{Name: name, Outcome: OutcomeError, Reason: upstream.reason}
	case err != nil:
		return Check{Name: name, Outcome: OutcomeError, Reason: "unreadable"}
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
