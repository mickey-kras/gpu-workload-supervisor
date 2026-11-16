// Package operator implements the bounded local desktop control contract.
package operator

import (
	"bytes"
	"encoding/json"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
	"io"
	"strconv"
	"time"
)

const (
	actionStatus           = "status"
	actionTakeControl      = "take-control"
	actionUserSwitch       = "user-switch"
	actionReturnControl    = "return-control"
	actionGetSettings      = "get-settings"
	actionSetIdlePolicy    = "set-idle-policy"
	actionActivateWorkload = "activate-workload"
	actionCloseGPUProcess  = "close-gpu-process"
)

// maxPID is the largest PID Linux can assign (PID_MAX_LIMIT).
const maxPID int = 1 << 22

const MaxRequestBytes = 16 * 1024
const MaxResponseBytes = 64 * 1024

type Code string

const (
	OK                        Code = "ok"
	InvalidRequest            Code = "invalid_request"
	UnsupportedVersion        Code = "unsupported_version"
	IncompatibleConfiguration Code = "incompatible_configuration"
	StaleState                Code = "stale_state"
	WrongOwner                Code = "wrong_owner"
	Busy                      Code = "busy"
	RecoveryRequired          Code = "recovery_required"
	Timeout                   Code = "timeout"
	Unavailable               Code = "unavailable"
	// Deferred is only a workload-activation outcome: the precondition was
	// readable, but the workload cannot start now (not idle, or unfinished
	// admitted work). It never latches anything and may be retried.
	Deferred Code = "deferred"
	// GPUInUse refuses a start before any change: another program uses the
	// target GPU. The response lists the processes in gpuCheck.
	GPUInUse Code = "gpu_in_use"
	// GPUCheckFailed refuses a start before any change: the GPU process check
	// failed or timed out. gpuCheck.failure says why. It never means free.
	GPUCheckFailed Code = "gpu_check_failed"
	// NotPermitted refuses to close a process that does not run as the
	// desktop user.
	NotPermitted Code = "not_permitted"
)

type Expected struct {
	Incarnation           string        `json:"incarnation"`
	Version               string        `json:"version"`
	Owner                 control.Owner `json:"owner"`
	ConfigurationRevision string        `json:"configurationRevision"`
}
type Request struct {
	ProtocolVersion int              `json:"protocolVersion"`
	RequestID       string           `json:"requestId"`
	Action          string           `json:"action"`
	Expected        *Expected        `json:"expected,omitempty"`
	Target          control.Workload `json:"target,omitempty"`
	Settings        *SettingsRequest `json:"settings,omitempty"`
	// PID is the process for close-gpu-process, and only for it.
	PID int `json:"pid,omitempty"`
}
type Workload struct {
	ID    control.Workload `json:"id"`
	Label string           `json:"label"`
}

// SettingsRequest carries the desired idle policy plus the opaque settings
// revision copied from a fresh get-settings response.
type SettingsRequest struct {
	TimeoutMinutes   int    `json:"timeoutMinutes"`
	SettingsRevision string `json:"settingsRevision"`
}
type IdlePolicyStatus struct {
	TimeoutMinutes int `json:"timeoutMinutes"`
}

// SettingsResponse reports the committed idle policy and its current opaque
// concurrency token.
type SettingsResponse struct {
	Policy           IdlePolicyStatus `json:"policy"`
	SettingsRevision string           `json:"settingsRevision"`
}
type Capabilities struct {
	TakeControl   bool `json:"takeControl"`
	UserSwitch    bool `json:"userSwitch"`
	ReturnControl bool `json:"returnControl"`
}
type Status struct {
	Owner           control.Owner     `json:"owner"`
	DesiredWorkload control.Workload  `json:"desiredWorkload"`
	ActiveWorkload  control.Workload  `json:"activeWorkload"`
	Phase           control.Phase     `json:"phase"`
	Health          control.Health    `json:"health"`
	Admission       control.Admission `json:"admission"`
	ObservedAt      time.Time         `json:"observedAt"`
	Expected        Expected          `json:"expected"`
	Workloads       []Workload        `json:"workloads"`
	Capabilities    Capabilities      `json:"capabilities"`
}
type Response struct {
	ProtocolVersion int               `json:"protocolVersion"`
	RequestID       string            `json:"requestId"`
	Code            Code              `json:"code"`
	Status          *Status           `json:"status,omitempty"`
	Settings        *SettingsResponse `json:"settings,omitempty"`
	// LeaseFence is present only on a successful activate-workload response:
	// the freshly rotated fence authorizes exactly one execution generation.
	LeaseFence *FenceResponse `json:"leaseFence,omitempty"`
	// GPUCheck is present only with gpu_in_use and gpu_check_failed.
	GPUCheck *GPUCheck `json:"gpuCheck,omitempty"`
}

// GPUProcess is one program that blocks a start. User is empty when the
// process cannot be read. UsedMiB is null when the driver does not report it.
type GPUProcess struct {
	PID      int     `json:"pid"`
	Name     string  `json:"name"`
	User     string  `json:"user"`
	UsedMiB  *uint64 `json:"usedMiB"`
	Closable bool    `json:"closable"`
}

// GPUCheck explains a gpu_in_use or gpu_check_failed code: the processes,
// or the failure of the check.
type GPUCheck struct {
	Processes []GPUProcess `json:"processes,omitempty"`
	Failure   string       `json:"failure,omitempty"`
}

// FenceResponse hands the committed fence to the activating caller.
type FenceResponse struct {
	Incarnation string `json:"incarnation"`
	Epoch       string `json:"epoch"`
}

func token(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func workloadID(s string) bool { return s == "idle" || control.ValidWorkloadID(control.Workload(s)) }

func Decode(body []byte) (Request, Code) {
	var r Request
	if len(body) > MaxRequestBytes || strictjson.Check(json.NewDecoder(bytes.NewReader(body))) != nil {
		return r, InvalidRequest
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil {
		return Request{}, InvalidRequest
	}
	if d.Decode(new(any)) != io.EOF {
		return Request{}, InvalidRequest
	}
	m, ok := fields(body, "protocolVersion", "requestId", "action", "expected", "target", "settings", "pid")
	if !ok || !token(r.RequestID, 64) {
		return Request{}, InvalidRequest
	}
	correlation := Request{RequestID: r.RequestID}
	if !requestFields(m, r.Action) {
		return correlation, InvalidRequest
	}
	if code := validateRequest(r); code != OK {
		return correlation, code
	}
	return r, OK
}

func validateRequest(r Request) Code {
	if r.ProtocolVersion != 1 {
		return UnsupportedVersion
	}
	return validateAction(r)
}

func validateAction(r Request) Code {
	switch r.Action {
	case actionStatus, actionGetSettings:
		if r.Expected != nil || r.Target != "" || r.Settings != nil {
			return InvalidRequest
		}
		return OK
	case actionSetIdlePolicy:
		if r.Target != "" {
			return InvalidRequest
		}
		if code := validateSettings(r.Settings); code != OK {
			return code
		}
		return validateExpected(r.Expected)
	case actionActivateWorkload:
		if r.Settings != nil || !workloadID(string(r.Target)) || r.Target == control.WorkloadIdle {
			return InvalidRequest
		}
		return validateExpected(r.Expected)
	case actionTakeControl, actionUserSwitch, actionReturnControl:
		return validateTransitionRequest(r)
	case actionCloseGPUProcess:
		// Close needs no precondition: it changes no supervisor state, and
		// the runtime lists the process again before the signal.
		if r.Settings != nil || r.Expected != nil || !workloadID(string(r.Target)) || r.Target == control.WorkloadIdle || r.PID <= 0 || r.PID > maxPID {
			return InvalidRequest
		}
		return OK
	default:
		return InvalidRequest
	}
}

func validateTransitionRequest(r Request) Code {
	if r.PID != 0 || r.Settings != nil || (r.Action == actionUserSwitch && !workloadID(string(r.Target))) || (r.Action != actionUserSwitch && r.Target != "") {
		return InvalidRequest
	}
	return validateExpected(r.Expected)
}

func validateSettings(s *SettingsRequest) Code {
	if s == nil || !token(s.SettingsRevision, 128) {
		return InvalidRequest
	}
	if (control.IdlePolicy{TimeoutMinutes: s.TimeoutMinutes}).Validate() != nil {
		return InvalidRequest
	}
	return OK
}

func validateExpected(e *Expected) Code {
	if e == nil || !token(e.Incarnation, 128) || !token(e.ConfigurationRevision, 128) || (e.Owner != control.OwnerUser && e.Owner != control.OwnerSupervisor) {
		return InvalidRequest
	}
	v, err := strconv.ParseUint(e.Version, 10, 64)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != e.Version {
		return InvalidRequest
	}
	return OK
}

func fields(body []byte, allowed ...string) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil || m == nil {
		return nil, false
	}
	for k := range m {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok {
			return nil, false
		}
	}
	return m, true
}
func requestFields(m map[string]json.RawMessage, action string) bool {
	for _, key := range []string{"protocolVersion", "requestId", "action"} {
		if raw, present := m[key]; !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
	}
	if ok, decided := closeFields(m, action); decided {
		return ok
	}
	if action == actionStatus || action == actionGetSettings {
		_, e := m["expected"]
		_, t := m["target"]
		_, s := m["settings"]
		return !e && !t && !s
	}
	if _, s := m["settings"]; s && action != actionSetIdlePolicy {
		return false
	}
	if action != actionUserSwitch && action != actionActivateWorkload {
		if _, ok := m["target"]; ok {
			return false
		}
	}
	return mutationFields(m, action)
}

// closeFields decides the fields of close-gpu-process: a target and a PID,
// and no precondition or settings. A PID on any other action is refused.
// decided is false when the other rules apply.
func closeFields(m map[string]json.RawMessage, action string) (ok, decided bool) {
	_, pid := m["pid"]
	if action != actionCloseGPUProcess {
		return false, pid
	}
	_, e := m["expected"]
	_, s := m["settings"]
	return pid && !e && !s && present(m, "target"), true
}

// present reports a field that is set and not null.
func present(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	return ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func mutationFields(m map[string]json.RawMessage, action string) bool {
	e, ok := fields(m["expected"], "incarnation", "version", "owner", "configurationRevision")
	if !ok || len(e) != 4 {
		return false
	}
	if action == actionSetIdlePolicy {
		s, ok := fields(m["settings"], "timeoutMinutes", "settingsRevision")
		if !ok || len(s) != 2 {
			return false
		}
		// encoding/json decodes a raw null into int as 0; reject it here so a
		// malformed timeout cannot silently read as an explicit Off.
		if bytes.Equal(bytes.TrimSpace(s["timeoutMinutes"]), []byte("null")) {
			return false
		}
		return !bytes.Equal(bytes.TrimSpace(s["settingsRevision"]), []byte("null"))
	}
	return true
}
