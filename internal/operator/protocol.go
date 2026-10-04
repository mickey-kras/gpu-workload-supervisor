// Package operator implements the bounded local desktop control contract.
package operator

import (
	"bytes"
	"encoding/json"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"io"
	"strconv"
	"time"
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

// uniqueObject rejects duplicate keys rather than accepting encoding/json's last value.
func uniqueObject(d *json.Decoder) bool {
	tok, e := d.Token()
	if e != nil {
		return false
	}
	if delim, ok := tok.(json.Delim); ok {
		if delim != '{' {
			return false
		}
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return false
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return false
			}
			seen[s] = true
			var raw json.RawMessage
			if d.Decode(&raw) != nil {
				return false
			}
			if len(raw) > 0 && raw[0] == '{' {
				if !uniqueObject(json.NewDecoder(bytes.NewReader(raw))) {
					return false
				}
			}
		}
		_, e = d.Token()
		return e == nil
	}
	return false
}
func Decode(body []byte) (Request, Code) {
	var r Request
	if len(body) > MaxRequestBytes || !uniqueObject(json.NewDecoder(bytes.NewReader(body))) {
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
	if r.ProtocolVersion != 1 {
		return Request{}, UnsupportedVersion
	}
	if !token(r.RequestID, 64) {
		return Request{}, InvalidRequest
	}
	if r.Action == "status" {
		if r.Expected != nil || r.Target != "" {
			return Request{}, InvalidRequest
		}
		return r, OK
	}
	if r.Action != "take-control" && r.Action != "user-switch" && r.Action != "return-control" {
		return Request{}, InvalidRequest
	}
	if (r.Action == "user-switch" && !workloadID(string(r.Target))) || (r.Action != "user-switch" && r.Target != "") {
		return Request{}, InvalidRequest
	}
	e := r.Expected
	if e == nil || !token(e.Incarnation, 128) || !token(e.ConfigurationRevision, 128) || (e.Owner != control.OwnerUser && e.Owner != control.OwnerSupervisor) {
		return Request{}, InvalidRequest
	}
	v, err := strconv.ParseUint(e.Version, 10, 64)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != e.Version {
		return Request{}, InvalidRequest
	}
	return r, OK
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
	if action == "status" {
		_, e := m["expected"]
		_, t := m["target"]
		return !e && !t
	}
	if action != "user-switch" {
		if _, ok := m["target"]; ok {
			return false
		}
	}
	e, ok := fields(m["expected"], "incarnation", "version", "owner", "configurationRevision")
	return ok && len(e) == 4
}
