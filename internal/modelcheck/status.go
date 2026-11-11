package modelcheck

import "context"

// CheckerVersion is the checker version that StoredStatus accepts. A record
// of another version reads as not scanned, so a new checker checks each
// model again. The setup action that runs the checks, gpu-setup
// model-check, sets Checker.Version to CheckerVersion.
const CheckerVersion = "1"

// Reasons of a display status that differs from the stored status. They
// never hold paths or file content. See also ReasonFilesChanged.
const (
	ReasonNoResult         = "no-result"
	ReasonOtherDirectory   = "other-directory"
	ReasonCheckerChanged   = "checker-changed"
	ReasonResultUnusable   = "result-unusable"
	ReasonTemplateChanged  = "template-changed"
	ReasonFilesUnavailable = "files-unavailable"
)

// CurrentConfiguration is the configuration that a check of this checker
// records against the default hub.
func CurrentConfiguration() Configuration {
	return currentConfiguration(DefaultHub)
}

// StoredStatus gives the status of a stored record for the model files at
// root, without reading the files. It uses the stored digests: a record for
// another model, another root, another checker version or another
// configuration is not scanned, with its reason. An empty root accepts the
// root of the record, for a launch that names no model directory. It does
// not compare the source. pin is the chat template SHA-256 of the launch.
// As in the launch gate, a record whose template check passed applies only
// to a launch that pins the same SHA-256. Other records, for example
// findings, keep their status.
func StoredStatus(r Record, model, root, pin string) (Status, string) {
	switch {
	case r.Model != model:
		return StatusNotScanned, ReasonNoResult
	case root != "" && r.Root != root:
		return StatusNotScanned, ReasonOtherDirectory
	case r.Checker != CheckerVersion || r.Configuration != CurrentConfiguration():
		return StatusNotScanned, ReasonCheckerChanged
	case len(r.Checks) == len(checkOrder) && r.templateCheck().Outcome == OutcomePassed && r.ChatTemplateSHA256 != pin:
		return StatusNotScanned, ReasonTemplateChanged
	}
	return r.Status, ""
}

// RecheckStatus reads the files of the record root again, and gives their
// status. Files that differ from the stored digests are not scanned. Files
// that cannot be read, for example a missing directory, are not scanned
// with ReasonFilesUnavailable. A canceled read gives the context error,
// and no status.
func RecheckStatus(ctx context.Context, r Record, model, root, pin string) (Status, string, error) {
	status, reason := StoredStatus(r, model, root, pin)
	if reason != "" {
		return status, reason, nil
	}
	match, err := filesMatch(ctx, r, r.Root)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", ctxErr
	}
	switch {
	case err != nil:
		return StatusNotScanned, ReasonFilesUnavailable, nil
	case !match:
		return StatusNotScanned, ReasonFilesChanged, nil
	}
	return r.Status, "", nil
}

// StaticChecksPassed reports a record whose file, identity and template
// checks passed while the behavioral scan did not run.
func (r Record) StaticChecksPassed() bool {
	if len(r.Checks) != len(checkOrder) {
		return false
	}
	for _, check := range r.Checks {
		want := OutcomePassed
		if check.Name == CheckBehavior {
			want = OutcomeNotRun
		}
		if check.Outcome != want {
			return false
		}
	}
	return true
}

// listingFailures are the reasons of a hub listing that failed. An
// identity mismatch and an unusable hub token are not among them.
var listingFailures = map[string]bool{
	ReasonUpstreamNotFound: true, ReasonUpstreamDenied: true, ReasonUpstreamInvalid: true,
	ReasonUpstreamUnreachable: true, ReasonUpstreamMoved: true, ReasonHistoryTooLong: true,
}

// HubFailed reports a record whose upstream identity check did not
// complete because the hub listing failed, for example because the hub is
// not reachable. The files can be correct, so the setup action that runs
// the checks can keep the earlier stored result. An identity mismatch is a
// finding, and an unusable hub token is an error of this computer: neither
// is a hub failure.
func (r Record) HubFailed() bool {
	for _, check := range r.Checks {
		if check.Name == CheckIdentity {
			return check.Outcome == OutcomeError && listingFailures[check.Reason]
		}
	}
	return false
}
