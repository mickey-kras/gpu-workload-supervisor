package control

import (
	"errors"
	"time"
)

// IdlePolicy configures inactivity idling. TimeoutMinutes 0 means Off; an
// enabled policy is bounded to reject typos. No inactivity evaluator ships,
// so the store rejects every enabled policy.
type IdlePolicy struct {
	TimeoutMinutes int `json:"timeoutMinutes"`
}

const (
	IdlePolicyOff        = 0
	IdlePolicyMinMinutes = 5
	IdlePolicyMaxMinutes = 1440
)

func (p IdlePolicy) Validate() error {
	if p.TimeoutMinutes == IdlePolicyOff {
		return nil
	}
	if p.TimeoutMinutes < IdlePolicyMinMinutes || p.TimeoutMinutes > IdlePolicyMaxMinutes {
		return errors.New("idle timeout must be 0 (off) or between 5 and 1440 minutes")
	}
	return nil
}

// SettingsPrecondition binds an operator settings write to an observed state,
// catalog, and settings revision. It is deliberately separate from
// OperatorPrecondition: settings changes never move the workload.
type SettingsPrecondition struct {
	Incarnation           string
	Version               uint64
	Owner                 Owner
	ConfigurationRevision string
	SettingsRevision      string
}

func (e SettingsPrecondition) OperatorPrecondition() OperatorPrecondition {
	return OperatorPrecondition{
		Incarnation:           e.Incarnation,
		Version:               e.Version,
		Owner:                 e.Owner,
		ConfigurationRevision: e.ConfigurationRevision,
	}
}

// PolicyState is the read model for the operator settings surface. The
// settings revision is an opaque concurrency token rotated on every write;
// no production path arms the deadline fields. They stay for databases of
// older releases, and every write path clears them.
type PolicyState struct {
	Policy           IdlePolicy
	SettingsRevision string
	LastActivityAt   *time.Time
	ArmedDeadline    *time.Time
	AttestationAt    *time.Time
}
