package modelcheck

import "context"

// CheckerVersion identifies the active checker version accepted by StoredStatus.
const CheckerVersion = "1"

const (
	ReasonNoResult         = "no-result"
	ReasonOtherDirectory   = "other-directory"
	ReasonCheckerChanged   = "checker-changed"
	ReasonResultUnusable   = "result-unusable"
	ReasonTemplateChanged  = "template-changed"
	ReasonFilesUnavailable = "files-unavailable"
)

// CurrentConfiguration returns the active checker configuration for DefaultHub.
func CurrentConfiguration() Configuration {
	return currentConfiguration(DefaultHub)
}

// StoredStatus evaluates a stored record against model, root, and launch template pin without touching disk.
func StoredStatus(r Record, model, root, pin string) (Status, string) {
	switch {
	case r.Model != model:
		return StatusNotScanned, ReasonNoResult
	case root != "" && r.Root != root:
		return StatusNotScanned, ReasonOtherDirectory
	case r.Checker != CheckerVersion || r.Configuration != CurrentConfiguration():
		return StatusNotScanned, ReasonCheckerChanged
	case len(r.Checks) == len(checkOrder) && r.Checks[1].Outcome == OutcomePassed && r.ChatTemplateSHA256 != "" && r.ChatTemplateSHA256 != pin:
		return StatusNotScanned, ReasonTemplateChanged
	}
	return r.Status, ""
}

// RecheckStatus re-inventories root and compares every file digest against r.
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

// StaticChecksPassed reports whether all static checks in r passed.
func (r Record) StaticChecksPassed() bool {
	return r.Status == StatusPassed
}
