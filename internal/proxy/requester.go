package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const RequesterCredentialHeader = "X-Workload-Requester-Credential"
const WorkflowHeader = "X-Workload-Workflow"

// RequesterPolicy is an immutable deployment allowlist. Credentials are never
// persisted with demand evidence. Digests bind authorization to the exact body.
type RequesterPolicy struct {
	Requesters []Requester `json:"requesters"`
}

type Requester struct {
	ID         string     `json:"id"`
	Credential string     `json:"credential"`
	Workflows  []Workflow `json:"workflows"`
}

type Workflow struct {
	ID         string `json:"id"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	BodySHA256 string `json:"bodySHA256"`
}

func policyID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func validateRequesterPolicy(config Config) error {
	p := config.RequesterPolicy
	if p == nil {
		return nil
	}
	if config.IngressMode != IngressAdapter || len(config.PassthroughRoutes) != 0 || len(config.ReadOnlyRoutes) != 0 {
		return errors.New("requester policy requires adapter ingress with execution routes only")
	}
	if len(p.Requesters) == 0 || len(p.Requesters) > 128 {
		return errors.New("requester policy requires 1 to 128 requesters")
	}
	ids, credentials := map[string]bool{}, map[string]bool{}
	credentials[config.ExecutionCredential], credentials[config.CompletionCredential] = true, true
	for _, requester := range p.Requesters {
		if !policyID(requester.ID) || ids[requester.ID] || !ValidIngressCredential(requester.Credential) || credentials[requester.Credential] {
			return errors.New("invalid or duplicate requester identity or credential")
		}
		ids[requester.ID], credentials[requester.Credential] = true, true
		if len(requester.Workflows) == 0 || len(requester.Workflows) > 128 {
			return errors.New("requester requires 1 to 128 workflows")
		}
		if err := validateRequesterWorkflows(requester.Workflows, config.ExecutionRoutes); err != nil {
			return err
		}

	}
	return nil
}

func validateRequesterWorkflows(workflows []Workflow, routes []Route) error {
	seen := map[string]bool{}
	for _, workflow := range workflows {
		if workflow.Method == http.MethodDelete || !policyID(workflow.ID) || seen[workflow.ID] || !ValidIngressCredential(workflow.BodySHA256) {
			return errors.New("invalid or duplicate workflow")
		}
		seen[workflow.ID] = true
		if !slices.Contains(routes, Route{Method: workflow.Method, Path: workflow.Path}) {
			return errors.New("workflow must name a configured execution route")
		}
	}
	return nil
}

// authorizeRequester consumes a bounded body only after authenticating the
// requester and selecting an allowed route. It never trusts caller identity.
func (h *Handler) authorizeRequester(w http.ResponseWriter, r *http.Request) (string, Workflow, bool) {
	values := ingressHeaderValues(r.Header, RequesterCredentialHeader)
	if len(values) != 1 || len(ingressHeaderValues(r.Header, ExecutionCredentialHeader)) != 0 || len(ingressHeaderValues(r.Header, CompletionCredentialHeader)) != 0 {
		writeError(w, http.StatusUnauthorized, "requester_unauthorized")
		return "", Workflow{}, false
	}
	var selected *Requester
	for i := range h.requesterPolicy.Requesters {
		candidate := &h.requesterPolicy.Requesters[i]
		if subtle.ConstantTimeCompare([]byte(values[0]), []byte(candidate.Credential)) == 1 {
			selected = candidate
		}
	}
	if selected == nil {
		writeError(w, http.StatusUnauthorized, "requester_unauthorized")
		return "", Workflow{}, false
	}
	workflows := ingressHeaderValues(r.Header, WorkflowHeader)
	if len(workflows) != 1 {
		writeError(w, http.StatusForbidden, "workflow_forbidden")
		return "", Workflow{}, false
	}
	for _, workflow := range selected.Workflows {
		if workflow.ID != workflows[0] || (workflow.Method != r.Method && r.Method != http.MethodDelete) || workflow.Path != r.URL.Path {
			continue
		}
		if !authorizeWorkflowBody(w, r, workflow) {
			return "", Workflow{}, false
		}
		return selected.ID, workflow, true
	}

	writeError(w, http.StatusForbidden, "workflow_forbidden")
	return "", Workflow{}, false
}

func authorizeWorkflowBody(w http.ResponseWriter, r *http.Request, workflow Workflow) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || len(ingressHeaderValues(r.Header, "Upgrade")) != 0 || r.Header.Get("Content-Encoding") != "" || len(r.Trailer) != 0 {
		writeError(w, http.StatusForbidden, "workflow_forbidden")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "workflow_body_invalid")
		return false
	}
	digest := sha256.Sum256(body)
	// Undeclared chunked trailers become visible only after reading to EOF.
	if len(r.Trailer) != 0 || hex.EncodeToString(digest[:]) != workflow.BodySHA256 {
		writeError(w, http.StatusForbidden, "workflow_forbidden")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = nil
	return true
}

func requesterHeader(name string) bool {
	return strings.EqualFold(name, RequesterCredentialHeader) || strings.EqualFold(name, WorkflowHeader)
}

type demandStore interface {
	QueueDemand(context.Context, store.Demand) error
	ReserveDemand(context.Context, store.Demand) (string, error)
	RequeueDemand(context.Context, string, string) error
	CancelDemand(context.Context, store.Demand) error
}

func (h *Handler) executeRequester(w http.ResponseWriter, r *http.Request) {
	requester, workflow, ok := h.authorizeRequester(w, r)
	if !ok {
		return
	}
	ids := ingressHeaderValues(r.Header, h.requestIDHeader)
	if len(ids) != 1 || control.ValidateRequestID(ids[0]) != nil {
		writeError(w, http.StatusBadRequest, "request_id_invalid")
		return
	}
	demands := h.store.(demandStore)
	demand := store.Demand{RequestID: ids[0], Requester: requester, Workflow: workflow.ID, BodySHA256: workflow.BodySHA256, Workload: h.workload}
	if r.Method == http.MethodDelete {
		if err := demands.CancelDemand(r.Context(), demand); err != nil {
			writeDemandError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.dispatchDemand(w, r, demands, demand)
}

func (h *Handler) dispatchDemand(w http.ResponseWriter, r *http.Request, demands demandStore, demand store.Demand) {
	if !h.checkNativeCatalog(w, r) || !h.checkNativeRequest(w, r) {
		return
	}
	state, err := h.store.State(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "state_unavailable")
		return
	}
	if state.Owner != control.OwnerSupervisor || state.Health != control.HealthHealthy {
		writeError(w, http.StatusConflict, "demand_rejected")
		return
	}
	if err := demands.QueueDemand(r.Context(), demand); err != nil {
		writeDemandError(w, err)
		return
	}
	if state.Phase != control.PhaseStable || state.ActiveWorkload != h.workload && state.ActiveWorkload != control.WorkloadIdle {
		writeError(w, http.StatusConflict, "demand_queued")
		return
	}
	claim, fence, ok := h.reserveDemand(w, r, demands, demand, state)
	if !ok {
		return
	}

	// The reservation is durable before activation/dispatch. Admission consumes
	// its claim atomically with the registered work. An ambiguous failure stays
	// unresolved; neither client disconnect nor process restart permits replay.
	r = r.WithContext(context.WithValue(store.WithDemandClaim(r.Context(), claim), demandReservationKey{}, demandReservation{requestID: demand.RequestID, claim: claim}))
	r.Header.Set(h.fenceIDHeader, fence.Incarnation)
	r.Header.Set(h.fenceEpochHeader, strconv.FormatUint(fence.Epoch, 10))
	if h.comfyUI != nil {
		h.executeComfyUI(w, r, demand.RequestID, fence)
		return
	}
	h.executeRegistered(w, r, demand.RequestID, fence)
}

func (h *Handler) reserveDemand(w http.ResponseWriter, r *http.Request, demands demandStore, demand store.Demand, state control.State) (string, control.Fence, bool) {
	claim, err := demands.ReserveDemand(r.Context(), demand)
	if err != nil {
		writeDemandError(w, err)
		return "", control.Fence{}, false
	}
	fence := state.LeaseFence
	if state.ActiveWorkload == control.WorkloadIdle {
		fence, err = h.activateDemand(r.Context(), h.workload, state, h.catalogRevision)
		if err != nil {
			if !requeueUnadmittedDemand(w, r, demands, demand.RequestID, claim) {
				return "", control.Fence{}, false
			}
			writeError(w, http.StatusConflict, "demand_queued")
			return "", control.Fence{}, false
		}
	}
	return claim, fence, true
}

func cloneRequesterPolicy(policy *RequesterPolicy) *RequesterPolicy {
	if policy == nil {
		return nil
	}
	clone := &RequesterPolicy{Requesters: slices.Clone(policy.Requesters)}
	for i := range clone.Requesters {
		clone.Requesters[i].Workflows = slices.Clone(clone.Requesters[i].Workflows)
	}
	return clone
}

func writeDemandError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDemandConflict):
		writeError(w, http.StatusConflict, "demand_conflict")
	case errors.Is(err, store.ErrDemandCapacity):
		writeError(w, http.StatusServiceUnavailable, "demand_capacity")
	default:
		writeError(w, http.StatusServiceUnavailable, "demand_state_failed")
	}
}

func validateDemandDependencies(stateStore StateStore, config Config) error {
	if config.RequesterPolicy == nil {
		return nil
	}
	if _, ok := stateStore.(demandStore); !ok || config.ActivateDemand == nil {
		return errors.New("requester ingress requires durable demand storage and activation")
	}
	return nil
}

func (h *Handler) requesterExecution(r *http.Request) bool {
	return h.requesterPolicy != nil && !(r.Method == http.MethodPost && r.URL.Path == h.completionPath)
}

func requesterSecrets(policy *RequesterPolicy) []string {
	if policy == nil {
		return nil
	}
	secrets := make([]string, 0, len(policy.Requesters))
	for _, requester := range policy.Requesters {
		secrets = append(secrets, requester.Credential)
	}
	return secrets
}

// demandReservation is present only while this handler owns a durable claim.
type demandReservationKey struct{}
type demandReservation struct{ requestID, claim string }

// releaseUnadmittedDemand runs only on rejection paths before dispatch. The
// store's atomic claim check preserves admitted work even after an ambiguous
// admission commit error. Cleanup failures remain visible and fail closed.
func (h *Handler) releaseUnadmittedDemand(w http.ResponseWriter, r *http.Request) bool {
	reservation, ok := r.Context().Value(demandReservationKey{}).(demandReservation)
	if !ok {
		return true
	}
	return requeueUnadmittedDemand(w, r, h.store.(demandStore), reservation.requestID, reservation.claim)
}

func requeueUnadmittedDemand(w http.ResponseWriter, r *http.Request, demands demandStore, requestID, claim string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	if err := demands.RequeueDemand(ctx, requestID, claim); err != nil {
		writeDemandError(w, err)
		return false
	}
	return true
}
