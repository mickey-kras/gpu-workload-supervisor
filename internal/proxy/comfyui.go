package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// This contract is source-qualified, not a promise about arbitrary ComfyUI
// releases or custom nodes. Deployment must verify its runtime against it.
const ComfyUIContract = "comfyui-a7169322485d0049380fb207fa17e9fb3ec40486"
const comfyUICorrelationKey = "gpu_workload_supervisor"
const comfyUIBodyLimit = 16 << 20

type ComfyUIStore interface {
	AdmitComfyUIWork(context.Context, store.ComfyUIAttempt, string) (string, error)
	PendingComfyUIAttempts(context.Context, control.Workload, string, string, string, int) ([]store.ComfyUIAttempt, error)
	FinishWorkToken(context.Context, string, control.Workload, control.Fence, string, store.WorkOutcome) error
}

type comfyUIAdapter struct {
	store     ComfyUIStore
	upstream  *url.URL
	transport http.RoundTripper
	workload  control.Workload
	cursor    string // owned by the single reconciliation loop
}

func (h *Handler) configureComfyUI(config Config) error {
	if config.ComfyUIContract == "" {
		return nil
	}
	if config.ComfyUIContract != ComfyUIContract || config.IngressMode != IngressAdapter || h.nativeModel != nil {
		return errors.New("ComfyUI requires the qualified source contract, adapter ingress, and a non-native workload")
	}
	u := config.Upstream
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || len(u.String()) > 2048 {
		return errors.New("ComfyUI requires a plain loopback runtime root URL")
	}
	if len(config.ExecutionRoutes) != 1 || config.ExecutionRoutes[0] != (Route{Method: http.MethodPost, Path: "/prompt"}) || len(config.PassthroughRoutes) != 0 || config.JobIDHeader != "" {
		return errors.New("ComfyUI requires only POST:/prompt execution, no passthrough, and no job ID header")
	}
	for _, route := range config.ReadOnlyRoutes {
		if route.Method != http.MethodGet || (route.Path != "/prompt" && route.Path != "/queue" && route.Path != "/history") {
			return errors.New("unsupported ComfyUI monitoring route")
		}
	}
	stateStore, ok := h.store.(ComfyUIStore)
	if !ok {
		return errors.New("ComfyUI requires durable attempt storage")
	}
	h.comfyUI = &comfyUIAdapter{store: stateStore, upstream: u, transport: h.proxy.Transport, workload: h.workload}
	return nil
}

func (h *Handler) executeComfyUI(response http.ResponseWriter, request *http.Request, requestID string, fence control.Fence) {
	body, attemptID, promptID, err := prepareComfyUIPrompt(request.Body)
	if err != nil {
		writeError(response, http.StatusBadRequest, "comfyui_request_invalid")
		return
	}
	attempt := store.ComfyUIAttempt{RequestID: requestID, Workload: h.workload, Fence: fence, AttemptID: attemptID, PromptID: promptID, Upstream: h.comfyUI.upstream.String(), Contract: ComfyUIContract}
	if _, err := h.comfyUI.store.AdmitComfyUIWork(request.Context(), attempt, h.catalogRevision); err != nil {
		h.writeWorkError(response, err)
		return
	}
	h.proxy.ServeHTTP(response, h.comfyUIForwardRequest(request, body))
}

func (h *Handler) executeComfyUIUser(response http.ResponseWriter, request *http.Request) {
	body, _, _, err := prepareComfyUIPrompt(request.Body)
	if err != nil {
		writeError(response, http.StatusBadRequest, "comfyui_request_invalid")
		return
	}
	h.proxy.ServeHTTP(response, h.comfyUIForwardRequest(request, body))
}

func (h *Handler) comfyUIForwardRequest(request *http.Request, body []byte) *http.Request {
	forwarded := h.withoutControlHeaders(request)
	forwarded.Body = io.NopCloser(bytes.NewReader(body))
	forwarded.ContentLength = int64(len(body))
	// Clear replay support even when an in-process caller supplied GetBody.
	// A transport failure after commit leaves the attempt unresolved, forever
	// avoiding blind /prompt replay across request retry or process restart.
	forwarded.GetBody = nil
	forwarded.Header.Del("Idempotency-Key")
	forwarded.Header.Del("X-Idempotency-Key")
	return forwarded
}

// RunComfyUIReconciler is owned by the proxy process lifetime. It never
// dispatches work and stops before the state store/lifetime lock closes.
func (h *Handler) RunComfyUIReconciler(ctx context.Context, report func(error)) {
	if h.comfyUI == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	diagnostics := comfyUIDiagnostics{}
	for {
		h.comfyUI.reconcileWithDiagnostics(ctx, func(id, reason string) {
			if ctx.Err() == nil {
				diagnostics.observe(id, reason, report)
			}
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ComfyUIUnresolvedError intentionally contains no graph, runtime reply,
// request ID, or completion token. Acceptance/errors never release admission.
type ComfyUIUnresolvedError struct{ AttemptID, Reason string }

func (e *ComfyUIUnresolvedError) Error() string {
	if e.AttemptID == "" {
		return "ComfyUI reconciliation unresolved: " + e.Reason
	}
	return "ComfyUI attempt " + e.AttemptID + " unresolved: " + e.Reason
}

type ComfyUIRecovery struct{ AttemptID string }

func (e *ComfyUIRecovery) Error() string {
	if e.AttemptID == "" {
		return "ComfyUI reconciliation pass recovered"
	}
	return "ComfyUI attempt " + e.AttemptID + " reconciliation recovered"
}

const comfyUIDiagnosticLimit = store.MaxComfyUIAttempts + 1

type comfyUIDiagnosticState struct {
	identity [sha256.Size]byte
	reason   string
}

type comfyUIDiagnostics struct {
	states [comfyUIDiagnosticLimit]comfyUIDiagnosticState
	next   int
}

func (d *comfyUIDiagnostics) observe(id, reason string, report func(error)) {
	identity := sha256.Sum256([]byte(id))
	free := -1
	for i := range d.states {
		state := &d.states[i]
		if state.reason == "" {
			if free < 0 {
				free = i
			}
			continue
		}
		if state.identity != identity {
			continue
		}
		if state.reason == reason {
			return
		}
		state.reason = reason
		if reason == "" {
			report(&ComfyUIRecovery{AttemptID: id})
		} else {
			report(&ComfyUIUnresolvedError{AttemptID: id, Reason: reason})
		}
		return
	}
	if reason == "" {
		return
	}
	slot := d.next
	if free >= 0 {
		slot = free
	}
	d.states[slot] = comfyUIDiagnosticState{identity: identity, reason: reason}
	d.next = (slot + 1) % len(d.states)
	report(&ComfyUIUnresolvedError{AttemptID: id, Reason: reason})
}

func decodeComfyUI(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := strictjson.Check(d); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	var object map[string]json.RawMessage
	err := json.Unmarshal(raw, &object)
	return object, err
}

// prepareComfyUIPrompt changes only lifecycle fields. It validates the envelope,
// not graph safety: the trusted gateway must validate graphs/custom nodes.
func prepareComfyUIPrompt(body io.Reader) ([]byte, string, string, error) {
	raw, err := io.ReadAll(io.LimitReader(body, comfyUIBodyLimit+1))
	if err != nil || len(raw) > comfyUIBodyLimit {
		return nil, "", "", errors.New("ComfyUI request unreadable or too large")
	}
	envelope, err := decodeComfyUI(raw)
	if err != nil || envelope == nil || !comfyUIExactKeys(envelope, "prompt", "prompt_id", "extra_data") {
		return nil, "", "", errors.New("ComfyUI envelope invalid")
	}
	var prompt map[string]json.RawMessage
	if err := json.Unmarshal(envelope["prompt"], &prompt); err != nil || len(prompt) == 0 {
		return nil, "", "", errors.New("ComfyUI prompt object required")
	}
	var extra map[string]json.RawMessage
	if value, ok := envelope["extra_data"]; ok {
		if err := json.Unmarshal(value, &extra); err != nil || extra == nil || !comfyUIExactKeys(extra, comfyUICorrelationKey) {
			return nil, "", "", errors.New("ComfyUI extra_data object required")
		}
	}
	if extra == nil {
		extra = make(map[string]json.RawMessage)
	}
	attemptID, err := control.NewUUID()
	if err != nil {
		return nil, "", "", err
	}
	promptID, err := control.NewUUID()
	if err != nil {
		return nil, "", "", err
	}
	// Caller-supplied IDs/correlation cannot alias another admitted execution.
	envelope["prompt_id"], _ = json.Marshal(promptID)
	extra[comfyUICorrelationKey], _ = json.Marshal(map[string]string{"attemptId": attemptID})
	envelope["extra_data"], _ = json.Marshal(extra)
	raw, err = json.Marshal(envelope)
	if len(raw) > comfyUIBodyLimit {
		return nil, "", "", errors.New("ComfyUI request too large after correlation")
	}
	return raw, attemptID, promptID, err
}

func (a *comfyUIAdapter) get(ctx context.Context, path string) (map[string]json.RawMessage, error) {
	u := *a.upstream
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	// The qualified evidence contract uses unencoded JSON. An explicit header
	// prevents net/http from silently negotiating and decoding gzip.
	req.Header.Set("Accept-Encoding", "identity")
	// RoundTrip deliberately never follows redirects and never replays enqueue.
	response, err := a.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Encoding") != "" || response.Uncompressed {
		return nil, errors.New("unexpected runtime response")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, comfyUIBodyLimit+1))
	if err != nil || len(raw) > comfyUIBodyLimit {
		return nil, errors.New("runtime response unreadable or too large")
	}
	return decodeComfyUI(raw)
}

func comfyUIExactKeys(object map[string]json.RawMessage, protected ...string) bool {
	for key := range object {
		for _, exact := range protected {
			if key != exact && strings.EqualFold(key, exact) {
				return false
			}
		}
	}
	return true
}

func comfyUIObject(raw json.RawMessage, protected ...string) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || !comfyUIExactKeys(object, protected...) {
		return nil
	}
	return object
}

// A task_done history entry contains the original prompt tuple. Do not infer
// completion from outputs, WebSocket messages, EOF, or an absent queue alone.
func comfyUIHistoryTerminal(history map[string]json.RawMessage, attempt store.ComfyUIAttempt) bool {
	entry := comfyUIObject(history[attempt.PromptID], "prompt", "status")
	status := comfyUIObject(entry["status"], "completed", "status_str")
	var completed bool
	var statusStr string
	if json.Unmarshal(status["completed"], &completed) != nil || !completed || json.Unmarshal(status["status_str"], &statusStr) != nil || statusStr != "success" {
		return false
	}
	var prompt []json.RawMessage
	if json.Unmarshal(entry["prompt"], &prompt) != nil {
		return false
	}
	id, extra := comfyUITupleIdentity(prompt)
	if id != attempt.PromptID {
		return false
	}
	return comfyUICorrelationMatches(extra, attempt.AttemptID)
}

func comfyUICorrelationMatches(extra map[string]json.RawMessage, attemptID string) bool {
	correlation := comfyUIObject(extra[comfyUICorrelationKey], "attemptId")
	var id string
	return json.Unmarshal(correlation["attemptId"], &id) == nil && id == attemptID
}

func comfyUITupleIdentity(tuple []json.RawMessage) (string, map[string]json.RawMessage) {
	if len(tuple) != 5 {
		return "", nil
	}
	var number *float64
	var id string
	var outputs []string
	if json.Unmarshal(tuple[0], &number) != nil || number == nil || json.Unmarshal(tuple[1], &id) != nil {
		return "", nil
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return "", nil
	}
	prompt := comfyUIObject(tuple[2])
	extra := comfyUIObject(tuple[3], comfyUICorrelationKey)
	if prompt == nil || extra == nil || json.Unmarshal(tuple[4], &outputs) != nil || outputs == nil {
		return "", nil
	}
	return id, extra
}

func comfyUIQueueEntries(queue map[string]json.RawMessage) [][]json.RawMessage {
	if !comfyUIExactKeys(queue, "queue_running", "queue_pending") {
		return nil
	}
	result := make([][]json.RawMessage, 0)
	for _, key := range []string{"queue_running", "queue_pending"} {
		raw, exists := queue[key]
		if !exists {
			return nil
		}
		var entries [][]json.RawMessage
		if json.Unmarshal(raw, &entries) != nil || entries == nil {
			return nil
		}
		for _, entry := range entries {
			id, _ := comfyUITupleIdentity(entry)
			if id == "" {
				return nil
			}
		}
		result = append(result, entries...)
	}
	return result
}

func comfyUIQueueAbsent(entries [][]json.RawMessage, promptID string) bool {
	for _, entry := range entries {
		id, _ := comfyUITupleIdentity(entry)
		if id == promptID {
			return false
		}
	}
	return true
}

func comfyUIQueuePending(entries [][]json.RawMessage, attempt store.ComfyUIAttempt) bool {
	matched := false
	for _, entry := range entries {
		id, extra := comfyUITupleIdentity(entry)
		if id != attempt.PromptID {
			continue
		}
		if matched || !comfyUICorrelationMatches(extra, attempt.AttemptID) {
			return false
		}
		matched = true
	}
	return matched
}

func (a *comfyUIAdapter) reconcileAttempt(ctx context.Context, attempt store.ComfyUIAttempt) error {
	unresolved := func(reason string) error { return &ComfyUIUnresolvedError{attempt.AttemptID, reason} }
	history, err := a.get(ctx, "/history/"+attempt.PromptID)
	if err != nil {
		return unresolved("history_unavailable")
	}
	terminal := comfyUIHistoryTerminal(history, attempt)
	if !terminal && (history == nil || len(history) != 0) {
		return unresolved("terminal_evidence_missing")
	}
	queue, err := a.get(ctx, "/queue")
	if err != nil {
		return unresolved("queue_unavailable")
	}
	entries := comfyUIQueueEntries(queue)
	if entries == nil {
		return unresolved("queue_not_proven_absent")
	}
	if !terminal {
		// Qualified ComfyUI history is empty until task_done. Only matching
		// queued/running identity makes that absence an ordinary pending poll.
		if comfyUIQueuePending(entries, attempt) {
			return nil
		}
		return unresolved("terminal_evidence_missing")
	}
	if !comfyUIQueueAbsent(entries, attempt.PromptID) {
		return unresolved("queue_not_proven_absent")
	}
	if err := a.store.FinishWorkToken(ctx, attempt.RequestID, attempt.Workload, attempt.Fence, attempt.RegistrationToken, store.WorkCompleted); err != nil && !errors.Is(err, store.ErrWorkAlreadyCompleted) {
		return unresolved("completion_commit_failed")
	}
	return nil
}

// reconcile is one bounded pass; it only polls, including after restart. A
// caller invokes it serially and reports its typed unresolved errors.
func (a *comfyUIAdapter) reconcile(ctx context.Context) error {
	return a.reconcileWithDiagnostics(ctx, func(string, string) {
		// This wrapper intentionally ignores diagnostics while propagating errors.
	})
}

func (a *comfyUIAdapter) reconcileWithDiagnostics(ctx context.Context, observe func(string, string)) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	attempts, err := a.store.PendingComfyUIAttempts(ctx, a.workload, a.upstream.String(), ComfyUIContract, a.cursor, 64)
	if err != nil {
		observe("", "pending_metadata_unavailable")
		return fmt.Errorf("ComfyUI pending metadata unavailable: %w", err)
	}
	if len(attempts) == 0 {
		a.cursor = ""
	}
	var errs []error
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			break
		}
		a.cursor = attempt.AttemptID
		attemptCtx, stop := context.WithTimeout(ctx, 2*time.Second)
		err := a.reconcileAttempt(attemptCtx, attempt)
		stop()
		reason := ""
		if err != nil {
			errs = append(errs, err)
			reason = "reconciliation_failed"
			var unresolved *ComfyUIUnresolvedError
			if errors.As(err, &unresolved) {
				reason = unresolved.Reason
			}
		}
		observe(attempt.AttemptID, reason)
	}
	if ctx.Err() != nil {
		errs = append(errs, ctx.Err())
		observe("", "reconciliation_timeout")
	} else {
		observe("", "")
	}
	return errors.Join(errs...)
}
