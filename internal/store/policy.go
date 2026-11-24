package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
)

// ErrPolicyPreempted reports that the idle transition no longer applies:
// activity, pending work or a state change came first. It never latches;
// the next policy tick checks again from durable state.
var ErrPolicyPreempted = errors.New("idle policy precondition preempted")

// touchActivity records activity time monotonically: concurrent finishers can
// never move last_activity_at backwards. String timestamps compare incorrectly
// at subsecond precision, so the comparison goes through julianday. Activity
// invalidates any armed deadline computed from the previous activity marker,
// so the armed fields are cleared in the same update; the read model then
// never reports a stale pending deadline.
func (s *Store) touchActivity(ctx context.Context, tx *sql.Tx) error {
	now := formatTime(s.now())
	return updateSingleton(ctx, tx, `UPDATE idle_policy_state
		SET last_activity_at = CASE
			WHEN last_activity_at IS NULL OR julianday(?) > julianday(last_activity_at) THEN ?
			ELSE last_activity_at END,
		armed_deadline = NULL, attestation_at = NULL,
		updated_at = ?
		WHERE singleton = 1`, now, now, now)
}

// StartIdleTransition starts the inactivity stop to Idle only while every
// idle precondition holds inside one writer transaction: the supervisor owns
// an open, stable, healthy, non-idle workload at the version tr was built
// from, no transition runs, no work or demand is pending, the policy is on,
// and last activity plus the timeout has elapsed. Admission, demand and
// finish paths write through the same single-writer store. They either
// commit first and preempt the stop, or see the closed admission after it.
// Any mismatch returns ErrPolicyPreempted without writing.
func (s *Store) StartIdleTransition(ctx context.Context, tr Transition) (control.State, error) {
	if tr.ID == "" {
		return control.State{}, errors.New("transition id is empty")
	}
	if tr.Target.DesiredWorkload != control.WorkloadIdle {
		return control.State{}, errors.New("idle transition must target idle")
	}
	return s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		if err := s.idleSource(ctx, tx, current, tr); err != nil {
			return control.State{}, err
		}
		if err := validateTransitionCatalog(ctx, tx, tr); err != nil {
			return control.State{}, err
		}
		return s.beginTransitionTx(ctx, tx, current, tr)
	})
}

func (s *Store) idleSource(ctx context.Context, tx *sql.Tx, current control.State, tr Transition) error {
	if current.Owner != control.OwnerSupervisor || current.Phase != control.PhaseStable ||
		current.Health != control.HealthHealthy || current.Admission != control.AdmissionOpen ||
		current.ActiveWorkload == control.WorkloadIdle || current.ActiveWorkload == control.WorkloadUnknown ||
		current.Version != tr.Source.Version {
		return ErrPolicyPreempted
	}
	running, err := transitionRunning(ctx, tx)
	if err != nil {
		return err
	}
	if running {
		return ErrPolicyPreempted
	}
	pending, err := pendingIdleWorkTx(ctx, tx)
	if err != nil {
		return err
	}
	if pending > 0 {
		return ErrPolicyPreempted
	}
	policy, err := readPolicyState(ctx, tx)
	if err != nil {
		return err
	}
	if policy.Policy.TimeoutMinutes == control.IdlePolicyOff || policy.LastActivityAt == nil {
		return ErrPolicyPreempted
	}
	deadline := policy.LastActivityAt.Add(time.Duration(policy.Policy.TimeoutMinutes) * time.Minute)
	if s.now().Before(deadline) {
		return ErrPolicyPreempted
	}
	return nil
}

func pendingWorkTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var pending int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL)`).Scan(&pending)
	return pending, err
}

// pendingIdleWorkTx reports open admissions and queued or reserved demand.
// Broker arrivals count only here: they must not block their own workload
// activation or count as dispatched work.
func pendingIdleWorkTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var pending int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL)
		OR EXISTS(SELECT 1 FROM broker_demands WHERE status IN ('queued','reserved'))`).Scan(&pending)
	return pending, err
}
