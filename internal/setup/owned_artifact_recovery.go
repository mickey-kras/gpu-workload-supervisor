package setup

import (
	"context"
	"maps"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

// observeLegacyRetry keeps a proved historical request for commit and removes
// the old bindings only from the setup preflight and release observation.
func (work activationWork) observeLegacyRetry(plan unitPlan) (Request, unitPlan, error) {
	observed := work.request
	// Preparation enables the sandbox for hardened units in a new apply.
	// An unmarked hardened candidate can only be an exact legacy retry.
	if !legacyArtifactCatalog(observed.Catalog) {
		return observed, plan, nil
	}
	if err := verifyOwnedUnitRenderings(work.home, observed.Catalog); err != nil {
		return observed, plan, err
	}
	observed.Catalog, observed.observation = observed.Catalog.Clone(), true
	proof := unitPlan{Deletes: append([]string(nil), plan.Deletes...), proven: maps.Clone(plan.proven)}
	for i, p := range observed.Catalog.Profiles {
		if legacyArtifactBinding(p) {
			observed.Catalog.Profiles[i].NativeModel = nil
			proof.Deletes = append(proof.Deletes, p.Unit)
			proof.proven[p.Unit] = p.NativeModel.LaunchSHA256
		}
	}
	return observed, proof, nil
}

// activationObservationManager supplies the same legacy unit proofs when the
// transaction repeats preflight immediately before committing.
type activationObservationManager struct {
	supervisor.Runtime
	proof unitPlan
}

func (m activationObservationManager) Preflight(ctx context.Context) error {
	return verifyActivationBindings(ctx, m.Runtime, m.proof)
}

func (m activationObservationManager) ReleasedForSetup(ctx context.Context, target control.Workload) error {
	return releasedForSetup(ctx, m.Runtime, target)
}

func legacyArtifactCatalog(c control.Catalog) bool {
	for _, p := range c.Profiles {
		if legacyArtifactBinding(p) {
			return true
		}
	}
	return false
}

func legacyArtifactBinding(p control.WorkloadProfile) bool {
	n := p.NativeModel
	return n != nil && n.Runtime == "vllm" && n.Owned != nil && n.Hardened() && !p.HasArtifactSandbox()
}
