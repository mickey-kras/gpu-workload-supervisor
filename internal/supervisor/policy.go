package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

// MaxAttestationAge bounds how old qualified evidence may be at evaluation
// time; anything older is treated as unavailable.
const MaxAttestationAge = 2 * time.Minute

// Attestation is the qualified evidence snapshot: registered proxies/jobs and
// sessions, plus an authoritative observation timestamp and an opaque
// generation token the provider can validate and hold through the drain commit.
type Attestation struct {
	Queued     []string
	Reserved   []string
	Running    []string
	Unresolved []string
	AttestedAt time.Time
	// Token is the provider's evidence generation/fingerprint; empty fails
	// closed. AcquireFence must report any change since the attestation.
	Token string
}

// EvidenceProvider is a qualified external evidence adapter. gpu-operator and
// gpu-mode carry no built-in provider; until a qualified adapter exists, every
// evaluation fails closed.
type EvidenceProvider interface {
	Attest(context.Context) (Attestation, error)
	// AcquireFence atomically validates the attested generation and excludes
	// external queue/reservation changes until release. The store calls release
	// after its idle transition transaction commits or rolls back. Providers
	// must not call into the Store while holding this fence.
	AcquireFence(ctx context.Context, token string) (release func(), err error)
}

func (a Attestation) anyEvidence() bool {
	return len(a.Queued)+len(a.Reserved)+len(a.Running)+len(a.Unresolved) > 0
}

// PolicyTick evaluates the inactivity policy once (oneshot, driven by the
// systemd user timer). Every step either no-ops cleanly or disarms the armed
// deadline; evidence failures return an error (noisy fail-closed) and never
// latch any state.
func (c *Controller) PolicyTick(ctx context.Context, evidence EvidenceProvider) error {
	settings, err := c.store.Settings(ctx)
	if err != nil {
		return err
	}
	if settings.Policy.TimeoutMinutes == control.IdlePolicyOff {
		return c.store.DisarmIdleDeadline(ctx)
	}
	state, err := c.store.State(ctx)
	if err != nil {
		return err
	}
	// Only a healthy, stable, supervisor-owned, running workload is eligible;
	// anything else (including degraded health, which the downstream idle
	// guard also rejects) disarms and no-ops, so no permanently overdue
	// deadline can be advertised or futilely fired.
	if state.Owner == control.OwnerUser || state.Phase != control.PhaseStable || state.Health != control.HealthHealthy ||
		state.ActiveWorkload == control.WorkloadIdle || state.ActiveWorkload == control.WorkloadUnknown {
		return c.store.DisarmIdleDeadline(ctx)
	}
	if evidence == nil {
		return errors.Join(c.store.DisarmIdleDeadline(ctx), store.ErrEvidenceUnavailable)
	}
	attestation, err := evidence.Attest(ctx)
	// Freshness is bounded in both directions: a timestamp ahead of the
	// supervisor clock (frozen or erroneous adapter clock) would otherwise
	// qualify an arbitrarily old empty snapshot forever. A missing generation
	// token cannot be revalidated before the drain commit, so it fails closed.
	if err != nil || attestation.AttestedAt.IsZero() || attestation.Token == "" || attestation.AttestedAt.After(c.now()) ||
		c.now().Sub(attestation.AttestedAt) > MaxAttestationAge {
		return errors.Join(c.store.DisarmIdleDeadline(ctx), store.ErrEvidenceUnavailable, err)
	}
	pending, err := c.store.PendingWork(ctx)
	if err != nil {
		return err
	}
	if attestation.anyEvidence() || pending > 0 {
		return c.store.DisarmIdleDeadline(ctx)
	}
	lastActivity := c.now()
	if settings.LastActivityAt != nil {
		lastActivity = *settings.LastActivityAt
	}
	deadline := lastActivity.Add(time.Duration(settings.Policy.TimeoutMinutes) * time.Minute)
	armed := settings.ArmedDeadline
	if armed == nil || !armed.Equal(deadline) {
		// No armed deadline yet (or activity/policy moved it): commit the
		// verified deadline and let a later tick fire once it elapses.
		return c.armPolicyDeadline(ctx, settings.SettingsRevision, deadline, attestation.AttestedAt)
	}
	if c.now().Before(*armed) {
		return nil
	}
	return c.firePolicyDeadline(ctx, *armed, evidence, attestation.Token)
}

// PolicyIdle drains the active workload into idle behind the armed deadline.
// The deadline is the concurrency token; the store revalidates it together
// with the stability and pending-work guards inside the writer transaction,
// and holds the external evidence fence (when non-nil) through commit.
func (c *Controller) PolicyIdle(ctx context.Context, armed time.Time, acquire func(context.Context) (func(), error)) (control.State, error) {
	if err := c.checkCatalog(ctx); err != nil {
		return control.State{}, err
	}
	current, err := c.store.State(ctx)
	if err != nil {
		return control.State{}, err
	}
	if current.Owner != control.OwnerSupervisor || current.ActiveWorkload == control.WorkloadIdle || current.ActiveWorkload == control.WorkloadUnknown {
		return current, store.ErrPolicyPreempted
	}
	if err := c.preflight(ctx); err != nil {
		return current, err
	}
	transitionID, err := c.id()
	if err != nil {
		return current, err
	}
	transition := store.Transition{
		ID: transitionID, Source: current,
		Target: stableTarget(current, control.OwnerSupervisor, control.WorkloadIdle), Previous: current,
		Initiator: inactivityPolicyInitiator, Phase: control.PhaseDraining,
		Deadline: c.now().Add(c.config.DrainTimeout),
	}
	transition.ConfigurationRevision = c.config.Catalog.Revision
	state, err := c.store.StartIdleTransition(ctx, armed, transition, acquire)
	if err != nil {
		return current, err
	}
	return c.executeTransition(ctx, transition, state, current, control.WorkloadIdle, transitionOptions{
		sourceOwner: control.OwnerSupervisor, targetOwner: control.OwnerSupervisor,
	})
}

const inactivityPolicyInitiator = "inactivity-policy"

func (c *Controller) armPolicyDeadline(ctx context.Context, settingsRevision string, deadline, attestationAt time.Time) error {
	err := c.store.ArmIdleDeadline(ctx, settingsRevision, deadline, attestationAt)
	if errors.Is(err, store.ErrSettingsConflict) || errors.Is(err, store.ErrPolicyPreempted) {
		return nil
	}
	return err
}

func (c *Controller) firePolicyDeadline(ctx context.Context, armed time.Time, evidence EvidenceProvider, token string) error {
	// Acquire inside the store's writer transaction and hold the provider's
	// queue-mutation fence through commit. An error or missing release fails
	// closed; after commit the durable draining state closes admission.
	acquire := func(ctx context.Context) (func(), error) {
		release, err := evidence.AcquireFence(ctx, token)
		if err != nil {
			return release, fmt.Errorf("%w: acquire evidence fence: %w", store.ErrEvidenceUnavailable, err)
		}
		return release, nil
	}
	_, err := c.PolicyIdle(ctx, armed, acquire)
	if errors.Is(err, store.ErrEvidenceUnavailable) {
		return errors.Join(c.store.DisarmIdleDeadline(ctx), err)
	}
	if errors.Is(err, store.ErrPolicyPreempted) {
		return nil
	}
	return err
}
