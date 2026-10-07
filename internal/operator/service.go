package operator

import (
	"context"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"golang.org/x/sys/unix"
	"path/filepath"
	"strconv"
	"time"
)

type Backend interface {
	Status(context.Context) (control.State, error)
	OperatorTransition(context.Context, string, control.Workload, control.OperatorPrecondition) (control.State, error)
}

// PolicyStore serves the typed settings surface straight from durable state:
// settings actions never observe or drive the runtime.
type PolicyStore interface {
	State(context.Context) (control.State, error)
	Settings(context.Context) (control.PolicyState, error)
	SetIdlePolicy(context.Context, control.SettingsPrecondition, control.IdlePolicy, bool) (control.PolicyState, error)
}
type Session struct {
	Backend   Backend
	Revision  string
	Workloads []Workload
	Close     func() error
	// PolicyStore is nil only in tests; settings actions then fail closed.
	PolicyStore PolicyStore
	// IdlePolicyConfigurable is false until the hosting process carries a
	// qualified evidence provider; it gates set-idle-policy enable writes and
	// is never advertised on the version-1 status shape.
	IdlePolicyConfigurable bool
}

// Service opens state only after nonblocking gate acquisition. Its operation
// context is independent of client streams. Cleanup/finalization may add the
// controller's separately bounded budgets after the operation deadline.
type Service struct {
	StatePath                       string
	StatusTimeout, OperationTimeout time.Duration
	Open                            func(context.Context) (Session, error)
}

func (s Service) Handle(req Request) Response {
	result := Response{ProtocolVersion: 1, RequestID: req.RequestID, Code: Unavailable}
	if s.Open == nil || !filepath.IsAbs(s.StatePath) || filepath.Clean(s.StatePath) != s.StatePath || s.StatePath == "/" {
		return result
	}
	budget, ok := s.requestBudget(req.Action)
	if !ok {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	gate, err := lock.TryAcquire(s.StatePath + ".lock")
	if err != nil {
		result.Code = errorCode(err)
		return result
	}
	defer gate.Close()
	session, err := s.Open(ctx)
	if err != nil {
		result.Code = errorCode(err)
		return result
	}
	if session.Close != nil {
		defer session.Close()
	}
	if session.Backend == nil || !validCatalog(session) {
		result.Code = IncompatibleConfiguration
		return result
	}
	state, settings, code := session.execute(ctx, req)
	if code != OK {
		result.Code = code
		return result
	}
	if ctx.Err() != nil {
		result.Code = Timeout
		return result
	}
	if state.Validate() != nil || !token(state.LeaseFence.Incarnation, 128) {
		return result
	}
	result.Code = OK
	result.Status = session.status(state)
	result.Settings = settings
	return result
}

func (s Service) requestBudget(action string) (time.Duration, bool) {
	budget, fallback, maximum := s.OperationTimeout, 15*time.Minute, 30*time.Minute
	if action == actionStatus || action == actionGetSettings || action == actionSetIdlePolicy {
		budget, fallback, maximum = s.StatusTimeout, 30*time.Second, 60*time.Second
	}
	if budget == 0 {
		budget = fallback
	}
	return budget, budget >= 0 && budget <= maximum
}

func (s Session) execute(ctx context.Context, req Request) (control.State, *SettingsResponse, Code) {
	switch req.Action {
	case actionGetSettings:
		return s.getSettings(ctx)
	case actionSetIdlePolicy:
		return s.setIdlePolicy(ctx, req)
	case actionStatus:
		state, err := s.Backend.Status(ctx)
		if err != nil {
			return state, nil, errorCode(err)
		}
		return state, nil, OK
	default:
		var state control.State
		if code := s.validateTransition(req); code != OK {
			return state, nil, code
		}
		e := req.Expected
		version, _ := strconv.ParseUint(e.Version, 10, 64)
		state, err := s.Backend.OperatorTransition(ctx, req.Action, req.Target, control.OperatorPrecondition{Incarnation: e.Incarnation, Version: version, Owner: e.Owner, ConfigurationRevision: e.ConfigurationRevision})
		if err != nil {
			return state, nil, errorCode(err)
		}
		return state, nil, OK
	}
}

func (s Session) getSettings(ctx context.Context) (control.State, *SettingsResponse, Code) {
	if s.PolicyStore == nil {
		return control.State{}, nil, Unavailable
	}
	state, err := s.PolicyStore.State(ctx)
	if err != nil {
		return control.State{}, nil, errorCode(err)
	}
	settings, err := s.PolicyStore.Settings(ctx)
	if err != nil {
		return control.State{}, nil, errorCode(err)
	}
	return state, settingsResponse(settings), OK
}

// setIdlePolicy commits only through the typed store surface; enabling is
// rejected fail-closed while the host has no qualified evidence provider.
func (s Session) setIdlePolicy(ctx context.Context, req Request) (control.State, *SettingsResponse, Code) {
	if s.PolicyStore == nil {
		return control.State{}, nil, Unavailable
	}
	if req.Expected == nil || req.Settings == nil {
		return control.State{}, nil, InvalidRequest
	}
	if req.Expected.ConfigurationRevision != s.Revision {
		return control.State{}, nil, StaleState
	}
	if req.Settings.TimeoutMinutes != control.IdlePolicyOff && !s.IdlePolicyConfigurable {
		return control.State{}, nil, errorCode(store.ErrEvidenceUnavailable)
	}
	e := req.Expected
	version, _ := strconv.ParseUint(e.Version, 10, 64)
	precondition := control.SettingsPrecondition{
		Incarnation: e.Incarnation, Version: version, Owner: e.Owner,
		ConfigurationRevision: e.ConfigurationRevision,
		SettingsRevision:      req.Settings.SettingsRevision,
	}
	settings, err := s.PolicyStore.SetIdlePolicy(ctx, precondition, control.IdlePolicy{TimeoutMinutes: req.Settings.TimeoutMinutes}, s.IdlePolicyConfigurable)
	if err != nil {
		return control.State{}, nil, errorCode(err)
	}
	state, err := s.PolicyStore.State(ctx)
	if err != nil {
		return control.State{}, nil, errorCode(err)
	}
	return state, settingsResponse(settings), OK
}

func settingsResponse(s control.PolicyState) *SettingsResponse {
	return &SettingsResponse{
		Policy:           IdlePolicyStatus{TimeoutMinutes: s.Policy.TimeoutMinutes},
		SettingsRevision: s.SettingsRevision,
	}
}

func (s Session) validateTransition(req Request) Code {
	if req.Expected == nil {
		return InvalidRequest
	}
	if req.Expected.ConfigurationRevision != s.Revision {
		return StaleState
	}
	if req.Action == actionUserSwitch {
		for _, w := range s.Workloads {
			if w.ID == req.Target {
				return OK
			}
		}
		return InvalidRequest
	}
	return OK
}

func (session Session) status(state control.State) *Status {
	ready := state.Phase == control.PhaseStable && state.Health != control.HealthError && state.ActiveWorkload != control.WorkloadUnknown
	return &Status{Owner: state.Owner, DesiredWorkload: state.DesiredWorkload, ActiveWorkload: state.ActiveWorkload, Phase: state.Phase, Health: state.Health, Admission: state.Admission, ObservedAt: time.Now().UTC(), Expected: Expected{state.LeaseFence.Incarnation, strconv.FormatUint(state.Version, 10), state.Owner, session.Revision}, Workloads: session.Workloads, Capabilities: Capabilities{ready && state.Owner == control.OwnerSupervisor, ready && state.Owner == control.OwnerUser, ready && state.Owner == control.OwnerUser}}
}
func validCatalog(s Session) bool {
	if !token(s.Revision, 128) || len(s.Workloads) == 0 || len(s.Workloads) > 65 {
		return false
	}
	seen := map[control.Workload]bool{}
	for _, w := range s.Workloads {
		if !workloadID(string(w.ID)) || seen[w.ID] || !control.ValidWorkloadLabel(w.Label) {
			return false
		}
		seen[w.ID] = true

	}
	return seen[control.WorkloadIdle]
}
func errorCode(err error) Code {
	switch {
	case errors.Is(err, ErrIncompatibleConfiguration):
		return IncompatibleConfiguration
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, supervisor.ErrDrainTimeout) || errors.Is(err, supervisor.ErrVerifyTimeout):
		return Timeout
	case errors.Is(err, store.ErrWrongOwner) || errors.Is(err, supervisor.ErrUserOwned) || errors.Is(err, supervisor.ErrSupervisorOwned):
		return WrongOwner
	case errors.Is(err, store.ErrVersionConflict) || errors.Is(err, store.ErrStaleFence) || errors.Is(err, store.ErrConfigurationConflict) || errors.Is(err, store.ErrSettingsConflict):
		return StaleState
	case errors.Is(err, store.ErrInvalidIdleTimeout):
		return InvalidRequest
	case errors.Is(err, store.ErrEvidenceUnavailable):
		return Unavailable
	case errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, store.ErrTransitionRunning) || errors.Is(err, supervisor.ErrTransitionRunning):
		return Busy
	case errors.Is(err, store.ErrUnstableState) || errors.Is(err, supervisor.ErrRecoveryRequired) || errors.Is(err, supervisor.ErrReconcileRequired):
		return RecoveryRequired
	default:
		return Unavailable
	}
}
