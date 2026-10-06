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
}
type Workload struct {
	ID    control.Workload `json:"id"`
	Label string           `json:"label"`
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
	ProtocolVersion int     `json:"protocolVersion"`
	RequestID       string  `json:"requestId"`
	Code            Code    `json:"code"`
	Status          *Status `json:"status,omitempty"`
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
	if r.Action == actionStatus {
		if r.Expected != nil || r.Target != "" {
			return InvalidRequest
		}
		return OK
	}
	if r.Action != actionTakeControl && r.Action != actionUserSwitch && r.Action != actionReturnControl {
		return InvalidRequest
	}
	if (r.Action == actionUserSwitch && !workloadID(string(r.Target))) || (r.Action != actionUserSwitch && r.Target != "") {
		return InvalidRequest
	}
	return validateExpected(r.Expected)
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
	m, ok := fields(body, "protocolVersion", "requestId", "action", "expected", "target")
	if !ok {
		return false
	}
	for _, key := range []string{"protocolVersion", "requestId", "action"} {
		if raw, present := m[key]; !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
	}
	if action == actionStatus {
		_, e := m["expected"]
		_, t := m["target"]
		return !e && !t
	}
	if action != actionUserSwitch {
		if _, ok := m["target"]; ok {
			return false
		}
	}
	e, ok := fields(m["expected"], "incarnation", "version", "owner", "configurationRevision")
	return ok && len(e) == 4
}
