package control

import (
	"errors"
	"fmt"
	"time"
)

type Owner string

const (
	OwnerSupervisor Owner = "supervisor"
	OwnerUser       Owner = "user"
)

type Workload string

const (
	WorkloadText    Workload = "text"
	WorkloadMedia   Workload = "media"
	WorkloadIdle    Workload = "idle"
	WorkloadUnknown Workload = "unknown"
)

type Phase string

const (
	PhaseStable      Phase = "stable"
	PhaseDraining    Phase = "draining"
	PhaseUnloading   Phase = "unloading"
	PhaseLoading     Phase = "loading"
	PhaseVerifying   Phase = "verifying"
	PhaseReconciling Phase = "reconciling"
)

type Health string

const (
	HealthHealthy  Health = "healthy"
	HealthDegraded Health = "degraded"
	HealthError    Health = "error"
)

type Admission string

const (
	AdmissionOpen   Admission = "open"
	AdmissionClosed Admission = "closed"
)

type Fence struct {
	Incarnation string `json:"incarnation"`
	Epoch       uint64 `json:"epoch"`
}

func (f Fence) Validate() error {
	if f.Incarnation == "" {
		return errors.New("fence incarnation is empty")
	}
	if f.Epoch == 0 {
		return errors.New("fence epoch must be greater than zero")
	}
	return nil
}

type State struct {
	Owner           Owner     `json:"owner"`
	DesiredWorkload Workload  `json:"desiredWorkload"`
	ActiveWorkload  Workload  `json:"activeWorkload"`
	Phase           Phase     `json:"phase"`
	Health          Health    `json:"health"`
	Admission       Admission `json:"admission"`
	LeaseFence      Fence     `json:"leaseFence"`
	Version         uint64    `json:"version"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func InitialState(incarnation string, now time.Time) State {
	return State{
		Owner:           OwnerSupervisor,
		DesiredWorkload: WorkloadIdle,
		ActiveWorkload:  WorkloadUnknown,
		Phase:           PhaseReconciling,
		Health:          HealthHealthy,
		Admission:       AdmissionClosed,
		LeaseFence:      Fence{Incarnation: incarnation, Epoch: 1},
		Version:         1,
		UpdatedAt:       now.UTC(),
	}
}

func (s State) Validate() error {
	if s.Owner != OwnerSupervisor && s.Owner != OwnerUser {
		return fmt.Errorf("invalid owner %q", s.Owner)
	}
	if s.DesiredWorkload != WorkloadText && s.DesiredWorkload != WorkloadMedia && s.DesiredWorkload != WorkloadIdle {
		return fmt.Errorf("invalid desired workload %q", s.DesiredWorkload)
	}
	if s.ActiveWorkload != WorkloadText && s.ActiveWorkload != WorkloadMedia && s.ActiveWorkload != WorkloadIdle && s.ActiveWorkload != WorkloadUnknown {
		return fmt.Errorf("invalid active workload %q", s.ActiveWorkload)
	}
	switch s.Phase {
	case PhaseStable, PhaseDraining, PhaseUnloading, PhaseLoading, PhaseVerifying, PhaseReconciling:
	default:
		return fmt.Errorf("invalid phase %q", s.Phase)
	}
	if s.Health != HealthHealthy && s.Health != HealthDegraded && s.Health != HealthError {
		return fmt.Errorf("invalid health %q", s.Health)
	}
	if s.Admission != AdmissionOpen && s.Admission != AdmissionClosed {
		return fmt.Errorf("invalid admission %q", s.Admission)
	}
	if err := s.LeaseFence.Validate(); err != nil {
		return err
	}
	if s.Version == 0 {
		return errors.New("state version must be greater than zero")
	}
	if s.UpdatedAt.IsZero() {
		return errors.New("updated timestamp is empty")
	}
	if s.Owner == OwnerUser && s.Admission != AdmissionClosed {
		return errors.New("supervisor admission must be closed during user ownership")
	}
	if s.Phase != PhaseStable && s.Admission != AdmissionClosed {
		return errors.New("admission must be closed outside stable phase")
	}
	if s.Health == HealthError && s.Admission != AdmissionClosed {
		return errors.New("admission must be closed while health is error")
	}
	return nil
}
