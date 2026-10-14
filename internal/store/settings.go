package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

var (
	// ErrSettingsConflict reports a stale opaque settings revision.
	ErrSettingsConflict = errors.New("operator settings revision conflict")
	// ErrInvalidIdleTimeout rejects an out-of-bounds idle timeout.
	ErrInvalidIdleTimeout = errors.New("idle timeout must be 0 (off) or between 5 and 1440 minutes")
	// ErrEvidenceUnavailable rejects enabling the idle policy while the
	// hosting process has no qualified evidence provider; never latched.
	ErrEvidenceUnavailable = errors.New("qualified idle evidence provider is unavailable")
)

// Settings reads the committed idle policy and its durable bookkeeping.
func (s *Store) Settings(ctx context.Context) (control.PolicyState, error) {
	return readPolicyState(ctx, s.db)
}

// SetIdlePolicy commits a new idle policy behind the full settings
// precondition. The writer transaction re-validates the incarnation, state
// version, owner, catalog revision, and settings revision; settings never
// interrupt an unstable state or a running transition. Every write rotates
// the opaque settings revision and clears the armed deadline, so a later
// policy tick must re-verify before idling.
func (s *Store) SetIdlePolicy(ctx context.Context, e control.SettingsPrecondition, p control.IdlePolicy, evidenceAvailable bool) (control.PolicyState, error) {
	if err := p.Validate(); err != nil {
		return control.PolicyState{}, errors.Join(ErrInvalidIdleTimeout, err)
	}
	// Enabling is gated on the hosting process carrying a qualified evidence
	// provider; the store defends the invariant so no caller can bypass it.
	if p.TimeoutMinutes != control.IdlePolicyOff && !evidenceAvailable {
		return control.PolicyState{}, ErrEvidenceUnavailable
	}
	revision, err := s.uuid()
	if err != nil {
		return control.PolicyState{}, err
	}
	var result control.PolicyState
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.setIdlePolicy(ctx, tx, e, p, revision)
		return err
	})
	if err != nil {
		return control.PolicyState{}, err
	}
	return result, nil
}

func (s *Store) setIdlePolicy(ctx context.Context, tx *sql.Tx, e control.SettingsPrecondition, p control.IdlePolicy, revision string) (control.PolicyState, error) {
	state, err := readState(ctx, tx)
	if err != nil {
		return control.PolicyState{}, err
	}
	// A live transition persists a non-stable phase, which would shadow
	// the transition check inside operatorSource with ErrUnstableState.
	// Settings writes must report the running transition as busy, so the
	// in-progress check runs first.
	running, err := transitionRunning(ctx, tx)
	if err != nil {
		return control.PolicyState{}, err
	}
	if running {
		return control.PolicyState{}, ErrTransitionRunning
	}
	if err := operatorSource(ctx, tx, state, e.OperatorPrecondition()); err != nil {
		return control.PolicyState{}, err
	}
	current, err := readPolicyState(ctx, tx)
	if err != nil {
		return control.PolicyState{}, err
	}
	if e.SettingsRevision == "" || current.SettingsRevision != e.SettingsRevision {
		return control.PolicyState{}, ErrSettingsConflict
	}
	now := formatTime(s.now())
	if err := updateSingleton(ctx, tx, `UPDATE operator_settings
			SET revision = ?, idle_timeout_minutes = ?, updated_at = ?
			WHERE singleton = 1`, revision, p.TimeoutMinutes, now); err != nil {
		return control.PolicyState{}, err
	}
	if err := s.disarmIdleDeadline(ctx, tx); err != nil {
		return control.PolicyState{}, err
	}
	return readPolicyState(ctx, tx)
}

// disarmIdleDeadline clears the verified armed deadline (and the attestation
// it was armed from) inside the caller's writer transaction. The next policy
// tick must re-attest before a deadline can be armed again.
func (s *Store) disarmIdleDeadline(ctx context.Context, tx *sql.Tx) error {
	return updateSingleton(ctx, tx, `UPDATE idle_policy_state
		SET armed_deadline = NULL, attestation_at = NULL, updated_at = ?
		WHERE singleton = 1`, formatTime(s.now()))
}

func updateSingleton(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func readPolicyState(ctx context.Context, q querier) (control.PolicyState, error) {
	var row policyStateRow
	// Settings and deadline bookkeeping must come from one committed snapshot.
	if err := q.QueryRowContext(ctx, `SELECT s.revision, s.idle_timeout_minutes, s.updated_at,
		p.last_activity_at, p.armed_deadline, p.attestation_at, p.updated_at
		FROM operator_settings AS s
		JOIN idle_policy_state AS p ON p.singleton = s.singleton
		WHERE s.singleton = 1`).Scan(
		&row.state.SettingsRevision, &row.state.Policy.TimeoutMinutes, &row.settingsUpdated,
		&row.lastActivity, &row.armed, &row.attested, &row.stateUpdated); err != nil {
		return row.state, err
	}
	return row.decode()
}

type policyStateRow struct {
	state                         control.PolicyState
	settingsUpdated, stateUpdated string
	lastActivity, armed, attested sql.NullString
}

func (row policyStateRow) decode() (control.PolicyState, error) {
	state := row.state
	if state.SettingsRevision == "" {
		return state, errors.New("operator settings revision is empty")
	}
	if err := state.Policy.Validate(); err != nil {
		return state, err
	}
	var err error
	if state.LastActivityAt, err = parseNullableTime(row.lastActivity); err != nil {
		return state, err
	}
	if state.ArmedDeadline, err = parseNullableTime(row.armed); err != nil {
		return state, err
	}
	if state.AttestationAt, err = parseNullableTime(row.attested); err != nil {
		return state, err
	}
	if _, err := parseTime(row.settingsUpdated); err != nil {
		return state, err
	}
	if _, err := parseTime(row.stateUpdated); err != nil {
		return state, err
	}
	return state, nil
}

func parseNullableTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	parsed, err := parseTime(v.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
