package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// ErrPolicyPreempted reports that the armed idle deadline was invalidated by
// concurrent activity or a state change before the idle transition committed;
// the next policy tick re-verifies from fresh evidence.
var ErrPolicyPreempted = errors.New("idle policy precondition preempted")

// touchActivity records activity time monotonically: concurrent finishers can
// never move last_activity_at backwards. String timestamps compare incorrectly
// at subsecond precision, so the comparison goes through julianday. Activity
// invalidates any armed deadline computed from the previous activity marker,
// so the armed fields are cleared in the same update; the read model then
// never reports a stale verified pending deadline, even if ticks stop.
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

// DisarmIdleDeadline clears the verified armed deadline (and the attestation
// it was armed from) outside any transition; the next policy tick must
// re-verify before a deadline can be armed again. An already-disarmed state
// is read-only: the timer ticks every minute with the policy Off by default,
// and a no-op must not churn updated_at and the WAL forever.
func (s *Store) DisarmIdleDeadline(ctx context.Context) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var armed int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM idle_policy_state
			WHERE singleton = 1 AND (armed_deadline IS NOT NULL OR attestation_at IS NOT NULL))`).Scan(&armed); err != nil {
			return err
		}
		if armed == 0 {
			return nil
		}
		return s.disarmIdleDeadline(ctx, tx)
	})
}

// ArmIdleDeadline durably records a verified inactivity deadline together with
// the attestation it was armed from. Arming is conditioned on the settings
// revision the evaluator observed (the policy fingerprint) and is only legal
// on an open, stable, supervisor-owned workload.
func (s *Store) ArmIdleDeadline(ctx context.Context, expectedSettingsRevision string, deadline, attestationAt time.Time) error {
	if expectedSettingsRevision == "" || deadline.IsZero() || attestationAt.IsZero() {
		return errors.New("arm requires a settings revision, deadline and attestation time")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		policy, err := readPolicyState(ctx, tx)
		if err != nil {
			return err
		}
		if policy.SettingsRevision != expectedSettingsRevision {
			return ErrSettingsConflict
		}
		if policy.Policy.TimeoutMinutes == control.IdlePolicyOff {
			return errors.Join(ErrInvalidIdleTimeout, errors.New("cannot arm a deadline while the policy is off"))
		}
		state, err := readControlState(ctx, tx)
		if err != nil {
			return err
		}
		// Healthy-only, consistent with idleSource: arming a deadline on a
		// degraded workload would advertise a deadline that can never fire.
		if state.Owner != control.OwnerSupervisor || state.Phase != control.PhaseStable || state.Health != control.HealthHealthy || state.Admission != control.AdmissionOpen {
			return ErrPolicyPreempted
		}
		// TOCTOU: an admission or completion may have committed after the
		// evaluator read the settings; revalidate the supplied deadline against
		// the current activity marker and require no pending work, so a stale
		// computed deadline is never armed. Mismatch is a clean preemption,
		// not a latch: the next tick re-verifies from fresh evidence.
		pending, err := pendingWorkTx(ctx, tx)
		if err != nil {
			return err
		}
		if pending > 0 {
			return ErrPolicyPreempted
		}
		if policy.LastActivityAt == nil ||
			!deadline.Equal(policy.LastActivityAt.Add(time.Duration(policy.Policy.TimeoutMinutes)*time.Minute)) {
			return ErrPolicyPreempted
		}
		return updateSingleton(ctx, tx, `UPDATE idle_policy_state
			SET armed_deadline = ?, attestation_at = ?, updated_at = ?
			WHERE singleton = 1`, formatTime(deadline), formatTime(attestationAt), formatTime(s.now()))
	})
}

// StartIdleTransition starts the inactivity drain only while the armed,
// verified deadline it was read from is still intact: the supervisor owns an
// open, stable, healthy, non-idle workload, no transition is running, no
// admitted work is pending, the armed deadline matches and has elapsed, and no
// activity was recorded since the arm. Any mismatch is a clean preemption, not
// an error, and never latches state.
//
// acquire, when non-nil, atomically validates and holds the external evidence
// generation inside the writer transaction. Its release callback runs only
// after that transaction commits or rolls back, so external queue mutations
// cannot slip between validation and the durable admission closure. The
// provider must return a non-nil release on success and must not call back into
// the Store while acquiring: the store is single-writer (SetMaxOpenConns(1)).
func (s *Store) StartIdleTransition(ctx context.Context, armed time.Time, tr Transition, acquire func(context.Context) (func(), error)) (control.State, error) {
	if tr.ID == "" {
		return control.State{}, errors.New("transition id is empty")
	}
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	return s.stateTx(ctx, func(tx *sql.Tx, current control.State) (control.State, error) {
		if err := s.idleSource(ctx, tx, current, armed, tr); err != nil {
			return control.State{}, err
		}
		if err := validateTransitionCatalog(ctx, tx, tr); err != nil {
			return control.State{}, err
		}
		if acquire != nil {
			held, err := acquire(ctx)
			release = held
			if err != nil {
				return control.State{}, err
			}
			if held == nil {
				return control.State{}, ErrEvidenceUnavailable
			}
		}
		return s.beginTransitionTx(ctx, tx, current, tr)
	})
}

func (s *Store) idleSource(ctx context.Context, tx *sql.Tx, current control.State, armed time.Time, tr Transition) error {
	if current.Owner != control.OwnerSupervisor || current.Phase != control.PhaseStable ||
		current.Health != control.HealthHealthy || current.Admission != control.AdmissionOpen ||
		current.ActiveWorkload == control.WorkloadIdle || current.ActiveWorkload == control.WorkloadUnknown ||
		current.ActiveWorkload != tr.Source.ActiveWorkload {
		return ErrPolicyPreempted
	}
	running, err := transitionRunning(ctx, tx)
	if err != nil {
		return err
	}
	if running {
		return ErrPolicyPreempted
	}
	pending, err := pendingWorkTx(ctx, tx)
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
	if policy.ArmedDeadline == nil || !policy.ArmedDeadline.Equal(armed) || s.now().Before(*policy.ArmedDeadline) {
		return ErrPolicyPreempted
	}
	if policy.Policy.TimeoutMinutes == control.IdlePolicyOff {
		return ErrPolicyPreempted
	}
	// Activity recorded after the arm moves the computed deadline, so any
	// drift between the armed deadline and last_activity_at + timeout means
	// the deadline no longer reflects the newest activity.
	if policy.LastActivityAt == nil || !policy.ArmedDeadline.Equal(policy.LastActivityAt.Add(time.Duration(policy.Policy.TimeoutMinutes)*time.Minute)) {
		return ErrPolicyPreempted
	}
	return nil
}

func pendingWorkTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var pending int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM registered_work WHERE completed_at IS NULL)`).Scan(&pending)
	return pending, err
}

