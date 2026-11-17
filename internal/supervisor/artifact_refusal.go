package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// ErrArtifactTarget refuses a target with artifact protection while its
// unit cannot be proven to pin the approved model artifacts read-only.
var ErrArtifactTarget = errors.New("target refused before any change")

// ErrArtifactUnitRejected refuses to keep a running unit whose artifact
// protection can no longer be proven: the model artifacts changed, or a
// link escapes or aliases them.
var ErrArtifactUnitRejected = errors.New("unit with artifact protection rejected")

// refuseArtifactTarget runs the artifact checks of a protected unit before
// any unload. A runtime that cannot run them refuses the unit. Start
// repeats these checks as the backstop.
func (c *Controller) refuseArtifactTarget(ctx context.Context, target control.Workload) error {
	p, _ := c.config.Catalog.Catalog.Profile(target)
	if !p.HasArtifactProtection() {
		return nil
	}
	err := c.runLimitProbe(ctx, target, limitChecker.VerifyArtifactProtection)
	if err == nil {
		return nil
	}
	if gpuruntime.DefiniteBindingFailure(err) {
		from := c.remedyFromStore(ctx)
		return artifactRemedy(limitedUnitError(ErrArtifactTarget, p, err), from)
	}
	// A command, read or context failure decides nothing. Nothing changed,
	// and the error keeps its usual code. It still names the target.
	return gpuruntime.UnitError(p, err)
}

// checkRetainedArtifacts runs the artifact checks of a protected running
// unit before reconciliation keeps it. It changes nothing.
func (c *Controller) checkRetainedArtifacts(ctx context.Context, p control.WorkloadProfile) error {
	if !p.HasArtifactProtection() {
		return nil
	}
	return c.runLimitProbe(ctx, p.ID, limitChecker.VerifyArtifactProtection)
}

// artifactRemedy adds the next steps to an artifact protection refusal. It
// reuses the recovery steps of a limit refusal; the resolution step names
// the artifact paths and the setup catalog edit instead of limits.
func artifactRemedy(rejected error, from remedyState) error {
	if rejected == nil {
		return nil
	}
	steps := from.recoverySteps()
	if len(steps) == 0 {
		return rejected
	}
	separator := "; next steps: "
	if strings.Contains(rejected.Error(), "\n") {
		separator = "\nnext steps: "
	}
	text := steps[len(steps)-1]
	if len(steps) > 1 {
		text = strings.Join(steps[:len(steps)-1], ", ") + ", then " + text
	}
	return fmt.Errorf("%w%sfirst, %s; then resolve the model artifact paths, or remove the artifact protection through setup", rejected, separator, text)
}
