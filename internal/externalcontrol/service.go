package externalcontrol

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
	"golang.org/x/sys/unix"
)

const MaxTimeout = 5 * time.Minute

// Backend must truthfully bind its operations to this exact durable state file.
// Embeddings must stop and reopen adapters when that file is restored/replaced.
type Backend interface {
	DurableStatePath() string
	Status(context.Context) (control.State, error)
	SwitchConditional(context.Context, control.Workload, string, control.Precondition) (control.State, error)
}

// Resolver is trusted adapter code that returns an authenticated principal.
// Request bodies and unverified caller-supplied identity headers are not sources.
type Resolver interface {
	Resolve(context.Context) (string, error)
}
type Grant struct {
	AuditID   string
	Status    bool
	Workloads []control.Workload
}
type Config struct {
	Timeout time.Duration
	Grants  map[string]Grant
}
type AuditSink interface {
	Record(context.Context, AuditEvent) error
}

type Service struct {
	backend  Backend
	resolver Resolver
	audit    AuditSink
	gatePath string
	timeout  time.Duration
	grants   map[string]Grant
	now      func() time.Time
	id       func() (string, error)
}

// New copies the policy and binds the immutable process gate to the backend.
// A backend or sink must honor contexts; Handle never detaches their work.
func New(backend Backend, resolver Resolver, audit AuditSink, config Config) (*Service, error) {
	if nilInterface(backend) || nilInterface(resolver) || nilInterface(audit) {
		return nil, errors.New("backend, resolver and audit sink are required")
	}
	path := backend.DurableStatePath()
	if !filepath.IsAbs(path) || path != filepath.Clean(path) || path == string(filepath.Separator) {
		return nil, errors.New("backend durable state path must be an absolute file path")
	}
	if config.Timeout <= 0 || config.Timeout > MaxTimeout {
		return nil, errors.New("timeout is outside the supported budget")
	}
	grants, err := copyGrants(config.Grants)
	if err != nil {
		return nil, err
	}
	return &Service{backend: backend, resolver: resolver, audit: audit, gatePath: path + ".lock", timeout: config.Timeout, grants: grants, now: time.Now, id: randomID}, nil
}

func copyGrants(source map[string]Grant) (map[string]Grant, error) {
	grants := make(map[string]Grant, len(source))
	for principal, grant := range source {
		if principal == "" || !opaqueID(grant.AuditID, 64) || grant.AuditID == "anonymous" {
			return nil, errors.New("principal and opaque audit id are required")
		}
		for _, workload := range grant.Workloads {
			if !validWorkload(workload) {
				return nil, errors.New("invalid workload grant")
			}
		}
		grant.Workloads = append([]control.Workload(nil), grant.Workloads...)
		grants[principal] = grant
	}
	return grants, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(value).IsNil()
	default:
		return false
	}
}
func randomID() (string, error) { id, err := uuid.NewRandom(); return id.String(), err }

// Handle executes synchronously. Cancellation can add the controller's bounded
// CleanupTimeout and FinalizeTimeout to the budget. No work is queued/retried.
// An unavailable outcome after execution or audit failure can have side effects;
// callers must refresh status before deciding whether to submit another request.
func (s *Service) Handle(ctx context.Context, body []byte) Response {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	id, err := s.id()
	if err != nil {
		return response(CodeUnavailable)
	}
	event := AuditEvent{CorrelationID: id, AuditID: "anonymous", Operation: OperationInvalid, Target: "invalid", Stage: "outcome"}
	if err := ctx.Err(); err != nil {
		return s.finish(ctx, event, response(errorCode(err)))
	}
	grant, code := s.resolve(ctx)
	if code != CodeOK {
		return s.finish(ctx, event, response(code))
	}
	event.AuditID = grant.AuditID
	req, code := Decode(body)
	if code != CodeOK {
		return s.finish(ctx, event, response(code))
	}
	event.Operation = req.Operation
	event.Target = "none"
	if req.Operation == OperationRequestWorkload {
		event.Target = string(req.Target)
	}
	if !authorized(grant, req) {
		return s.finish(ctx, event, response(CodeForbidden))
	}
	result := s.execute(ctx, req, event)
	return s.finish(ctx, event, result)
}

func (s *Service) resolve(ctx context.Context) (Grant, Code) {
	principal, err := s.resolver.Resolve(ctx)
	if ctx.Err() != nil {
		return Grant{}, errorCode(ctx.Err())
	}
	if err != nil || principal == "" {
		return Grant{}, CodeUnauthenticated
	}
	grant, ok := s.grants[principal]
	if !ok {
		return Grant{}, CodeForbidden
	}
	return grant, CodeOK
}

func authorized(grant Grant, req Request) bool {
	if req.Operation == OperationStatus {
		return grant.Status
	}
	for _, target := range grant.Workloads {
		if req.Target == target {
			return true
		}
	}
	return false
}

func (s *Service) execute(ctx context.Context, req Request, event AuditEvent) Response {
	if err := ctx.Err(); err != nil {
		return response(errorCode(err))
	}
	gate, err := lock.TryAcquire(s.gatePath)
	if err != nil {
		return response(errorCode(err))
	}
	defer gate.Close()
	event.Stage = "admission"
	event.Outcome = CodeOK
	if err := s.record(ctx, event); err != nil {
		return response(CodeUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return response(errorCode(err))
	}
	state, err := s.call(ctx, req, event.AuditID)
	if err != nil {
		return response(errorCode(err))
	}
	if err := ctx.Err(); err != nil {
		return response(errorCode(err))
	}
	if state.Validate() != nil || !opaqueID(state.LeaseFence.Incarnation, 128) {
		return response(CodeUnavailable)
	}
	result := response(CodeOK)
	result.Status = statusDTO(state, s.now())
	return result
}

func (s *Service) call(ctx context.Context, req Request, auditID string) (control.State, error) {
	if req.Operation == OperationStatus {
		return s.backend.Status(ctx)
	}
	return s.backend.SwitchConditional(ctx, req.Target, auditID, *req.Expected)
}

func (s *Service) finish(ctx context.Context, event AuditEvent, result Response) Response {
	event.Outcome = result.Code
	if err := s.record(ctx, event); err != nil {
		return response(CodeUnavailable)
	}
	return result
}
func (s *Service) record(ctx context.Context, event AuditEvent) error {
	event.Timestamp = s.now().UTC()
	return s.audit.Record(ctx, event)
}
func response(code Code) Response { return Response{APIVersion: APIVersion, Code: code} }
func statusDTO(state control.State, observed time.Time) *Status {
	return &Status{Owner: state.Owner, DesiredWorkload: state.DesiredWorkload, ActiveWorkload: state.ActiveWorkload, Phase: state.Phase, Health: state.Health, Admission: state.Admission, Expected: control.Precondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version}, UpdatedAt: state.UpdatedAt.UTC(), ObservedAt: observed.UTC()}
}

func errorCode(err error) Code {
	switch {
	case err == nil:
		return CodeOK
	case errors.Is(err, context.Canceled):
		return CodeCanceled
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, supervisor.ErrDrainTimeout) || errors.Is(err, supervisor.ErrVerifyTimeout):
		return CodeTimeout
	case errors.Is(err, supervisor.ErrUserOwned) || errors.Is(err, store.ErrUserOwned):
		return CodeUserOwned
	case errors.Is(err, store.ErrVersionConflict) || errors.Is(err, store.ErrStaleFence):
		return CodeStaleState
	case errors.Is(err, supervisor.ErrTransitionRunning) || errors.Is(err, store.ErrTransitionRunning) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK):
		return CodeBusy
	case errors.Is(err, supervisor.ErrRecoveryRequired) || errors.Is(err, supervisor.ErrReconcileRequired) || errors.Is(err, store.ErrRecoveryRequired):
		return CodeRecoveryRequired
	default:
		return CodeUnavailable
	}
}
