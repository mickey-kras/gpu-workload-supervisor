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
	actionStatus        = "status"
	actionTakeControl   = "take-control"
	actionUserSwitch    = "user-switch"
	actionReturnControl = "return-control"
	actionGetSettings   = "get-settings"
	actionSetIdlePolicy = "set-idle-policy"
)

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
	TakeControl            bool `json:"takeControl"`
	UserSwitch             bool `json:"userSwitch"`
	ReturnControl          bool `json:"returnControl"`
	IdlePolicyConfigurable bool `json:"idlePolicyConfigurable"`
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
	IdlePolicy      IdlePolicyStatus  `json:"idlePolicy"`
}
type Response struct {
	ProtocolVersion int               `json:"protocolVersion"`
	RequestID       string            `json:"requestId"`
	Code            Code              `json:"code"`
	Status          *Status           `json:"status,omitempty"`
	Settings        *SettingsResponse `json:"settings,omitempty"`
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
	if !requestFields(body, r.Action) {
		return Request{}, InvalidRequest
	}
	if code := validateRequest(r); code != OK {
		return Request{}, code
	}
	return r, OK
}

func validateRequest(r Request) Code {
	if r.ProtocolVersion != 1 {
		return UnsupportedVersion
	}
	if !token(r.RequestID, 64) {
		return InvalidRequest
	}
	if r.Action == actionStatus || r.Action == actionGetSettings {
		if r.Expected != nil || r.Target != "" || r.Settings != nil {
			return InvalidRequest
		}
		return OK
	}
	if r.Action == actionSetIdlePolicy {
		if r.Target != "" {
			return InvalidRequest
		}
		if code := validateSettings(r.Settings); code != OK {
			return code
		}
		return validateExpected(r.Expected)
	}
	if r.Action != actionTakeControl && r.Action != actionUserSwitch && r.Action != actionReturnControl {
		return InvalidRequest
	}
	if r.Settings != nil || (r.Action == actionUserSwitch && !workloadID(string(r.Target))) || (r.Action != actionUserSwitch && r.Target != "") {
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
func requestFields(body []byte, action string) bool {
	m, ok := fields(body, "protocolVersion", "requestId", "action", "expected", "target", "settings")
	if !ok {
		return false
	}
	for _, key := range []string{"protocolVersion", "requestId", "action"} {
		if raw, present := m[key]; !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
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
	if action != actionUserSwitch {
		if _, ok := m["target"]; ok {
			return false
		}
	}
	e, ok := fields(m["expected"], "incarnation", "version", "owner", "configurationRevision")
	if !ok || len(e) != 4 {
		return false
	}
	if action == actionSetIdlePolicy {
		s, ok := fields(m["settings"], "timeoutMinutes", "settingsRevision")
		return ok && len(s) == 2
	}
	return true
}
