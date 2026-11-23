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

// StoredStatus returns the status of r for the model files at root using
// only the stored digests; it does not read the files. A record for another
// model, root, checker version or configuration is not_scanned with a
// reason. An empty root accepts the record's root, for a launch that names
// no model directory.
//
// pin is the launch's chat template SHA-256. As in the launch gate, a record
// whose template check passed applies only to a launch with the same pin (an
// unpinned launch pins no template). Other records, such as findings, keep
// their status.
func StoredStatus(r Record, model, root, pin string) (Status, string) {
	switch {
	case r.Model != model:
		return StatusNotScanned, ReasonNoResult
	case root != "" && r.Root != root:
		return StatusNotScanned, ReasonOtherDirectory
	case r.Checker != CheckerVersion || r.Configuration != CurrentConfiguration():
		return StatusNotScanned, ReasonCheckerChanged
	case len(r.Checks) == len(checkOrder) && r.Checks[templateIndex].Outcome == OutcomePassed && r.ChatTemplateSHA256 != pin:
		return StatusNotScanned, ReasonTemplateChanged
	}
	return r.Status, ""
}

// RecheckStatus rereads the files under the record root and returns their
// status. Files that differ from the stored digests are not_scanned; files
// that cannot be read, such as a missing directory, are not_scanned with
// ReasonFilesUnavailable. A canceled read returns the context error and no
// status.
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

// StaticChecksPassed reports a record whose file and template checks
// passed while the behavioral scan did not run.
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
