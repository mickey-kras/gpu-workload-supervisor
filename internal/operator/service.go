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
type Session struct {
	Backend   Backend
	Revision  string
	Workloads []Workload
	Close     func() error
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
	state, code := session.execute(ctx, req)
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
	return result
}

func (s Service) requestBudget(action string) (time.Duration, bool) {
	budget, fallback, maximum := s.OperationTimeout, 15*time.Minute, 30*time.Minute
	if action == "status" {
		budget, fallback, maximum = s.StatusTimeout, 30*time.Second, 60*time.Second
	}
	if budget == 0 {
		budget = fallback
	}
	return budget, budget >= 0 && budget <= maximum
}

func (s Session) execute(ctx context.Context, req Request) (control.State, Code) {
	var state control.State
	var err error
	if req.Action == "status" {
		state, err = s.Backend.Status(ctx)
	} else {
		if code := s.validateTransition(req); code != OK {
			return state, code
		}
		e := req.Expected
		version, _ := strconv.ParseUint(e.Version, 10, 64)
		state, err = s.Backend.OperatorTransition(ctx, req.Action, req.Target, control.OperatorPrecondition{Incarnation: e.Incarnation, Version: version, Owner: e.Owner, ConfigurationRevision: e.ConfigurationRevision})
	}
	if err != nil {
		return state, errorCode(err)
	}
	return state, OK
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
	case errors.Is(err, store.ErrVersionConflict) || errors.Is(err, store.ErrStaleFence) || errors.Is(err, store.ErrConfigurationConflict):
		return StaleState
	case errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, store.ErrTransitionRunning) || errors.Is(err, supervisor.ErrTransitionRunning):
		return Busy
	case errors.Is(err, store.ErrRecoveryRequired) || errors.Is(err, supervisor.ErrRecoveryRequired) || errors.Is(err, supervisor.ErrReconcileRequired):
		return RecoveryRequired
	default:
		return Unavailable
	}
}
