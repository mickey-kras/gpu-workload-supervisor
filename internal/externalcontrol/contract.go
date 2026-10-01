// Package externalcontrol defines a restricted transport-neutral control
// contract. It does not authenticate a network transport or expose a listener.
package externalcontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"io"
	"time"
)

const APIVersion = "v1"
const MaxBodyBytes = 4096

type Operation string

const (
	OperationStatus          Operation = "status"
	OperationRequestWorkload Operation = "request-workload"
	OperationInvalid         Operation = "invalid"
)

type Code string

const (
	CodeOK                 Code = "ok"
	CodeUnauthenticated    Code = "unauthenticated"
	CodeForbidden          Code = "forbidden"
	CodeInvalidRequest     Code = "invalid_request"
	CodeUnsupportedVersion Code = "unsupported_version"
	CodeUserOwned          Code = "user_owned"
	CodeStaleState         Code = "stale_state"
	CodeBusy               Code = "busy"
	CodeRecoveryRequired   Code = "recovery_required"
	CodeTimeout            Code = "timeout"
	CodeCanceled           Code = "canceled"
	CodeUnavailable        Code = "unavailable"
)

type Request struct {
	APIVersion string                `json:"apiVersion"`
	Operation  Operation             `json:"operation"`
	Target     control.Workload      `json:"target,omitempty"`
	Expected   *control.Precondition `json:"expected,omitempty"`
}

type Response struct {
	APIVersion string  `json:"apiVersion"`
	Code       Code    `json:"code"`
	Status     *Status `json:"status,omitempty"`
}

// Status intentionally omits internal lease epoch, transition journals, and
// runtime errors. ObservedAt identifies when the backend status call returned.
type Status struct {
	Owner           control.Owner        `json:"owner"`
	DesiredWorkload control.Workload     `json:"desiredWorkload"`
	ActiveWorkload  control.Workload     `json:"activeWorkload"`
	Phase           control.Phase        `json:"phase"`
	Health          control.Health       `json:"health"`
	Admission       control.Admission    `json:"admission"`
	Expected        control.Precondition `json:"expected"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	ObservedAt      time.Time            `json:"observedAt"`
}

// AuditEvent has only server-generated/configured values and fixed request
// classifications. Stage distinguishes pre-execution admission from outcome.
type AuditEvent struct {
	Timestamp     time.Time `json:"timestamp"`
	CorrelationID string    `json:"correlationId"`
	AuditID       string    `json:"auditId"`
	Operation     Operation `json:"operation"`
	Target        string    `json:"target"`
	Stage         string    `json:"stage"`
	Outcome       Code      `json:"outcome"`
}

// Decode accepts one bounded object with exact, case-sensitive fields. The
// standard JSON parser reads keys individually so duplicate keys cannot become
// last-value-wins input, including inside the expected-state token.
func Decode(body []byte) (Request, Code) {
	req := Request{}
	if len(body) > MaxBodyBytes {
		return req, CodeInvalidRequest
	}
	fields, err := objectFields(body, "apiVersion", "operation", "target", "expected")
	if err != nil {
		return req, CodeInvalidRequest
	}
	if err := json.Unmarshal(fields["apiVersion"], &req.APIVersion); err != nil || req.APIVersion == "" {
		return Request{}, CodeInvalidRequest
	}
	if req.APIVersion != APIVersion {
		return Request{}, CodeUnsupportedVersion
	}
	if err := json.Unmarshal(fields["operation"], &req.Operation); err != nil {
		return Request{}, CodeInvalidRequest
	}
	if !validRequestFields(fields, &req) {
		return Request{}, CodeInvalidRequest
	}
	return req, CodeOK
}

func validRequestFields(fields map[string]json.RawMessage, req *Request) bool {
	switch req.Operation {
	case OperationStatus:
		return len(fields) == 2
	case OperationRequestWorkload:
		if len(fields) != 4 || json.Unmarshal(fields["target"], &req.Target) != nil || !validWorkload(req.Target) {
			return false
		}
		expected, err := decodeExpected(fields["expected"])
		if err != nil {
			return false
		}
		req.Expected = &expected
		return true
	default:
		return false
	}
}

func decodeExpected(body []byte) (control.Precondition, error) {
	expected := control.Precondition{}
	fields, err := objectFields(body, "incarnation", "version")
	if err != nil {
		return expected, err
	}
	if len(fields) != 2 || json.Unmarshal(fields["incarnation"], &expected.Incarnation) != nil || json.Unmarshal(fields["version"], &expected.Version) != nil {
		return expected, errors.New("invalid expected state")
	}
	if !opaqueID(expected.Incarnation, 128) || expected.Version == 0 {
		return expected, errors.New("invalid expected state")
	}
	return expected, nil
}

func objectFields(body []byte, allowed ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		if err := readField(decoder, fields, allowed); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing input")
	}
	return fields, nil
}

func readField(decoder *json.Decoder, fields map[string]json.RawMessage, allowed []string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	key, ok := token.(string)
	if !ok || !allowedField(key, allowed) {
		return errors.New("unknown field")
	}
	if _, exists := fields[key]; exists {
		return errors.New("duplicate field")
	}
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	fields[key] = value
	return nil
}

func allowedField(key string, allowed []string) bool {
	for _, field := range allowed {
		if key == field {
			return true
		}
	}
	return false
}
func validWorkload(target control.Workload) bool {
	return target == control.WorkloadText || target == control.WorkloadMedia || target == control.WorkloadIdle
}
func opaqueID(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
