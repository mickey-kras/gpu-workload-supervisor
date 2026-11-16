package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/supervisor"
)

func gpuService(t *testing.T) (Service, *backendFixture) {
	t.Helper()
	b := &backendFixture{state: control.InitialState("inc", time.Now())}
	b.state.Phase = control.PhaseStable
	b.state.ActiveWorkload = control.WorkloadIdle
	s := Service{StatePath: filepath.Join(t.TempDir(), "state"), Open: func(context.Context) (Session, error) {
		return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}, {"third", "Third"}}}, nil
	}}
	return s, b
}

func refused(err error) error { return fmt.Errorf("%w: %w", supervisor.ErrGPURefused, err) }

var userProcess = gpuruntime.GPUProcess{PID: 4242, Name: "python3", UsedMiB: 2048, MemoryKnown: true, UID: 1000, User: "alice", Closable: true}
var otherProcess = gpuruntime.GPUProcess{PID: 77, Name: "render" + string(rune(0x202E)) + "\x07farm", UID: 1001, User: "bob"}

func switchRequest() Request {
	return Request{ProtocolVersion: 1, RequestID: "r", Action: "user-switch", Target: "third", Expected: &Expected{"inc", "1", control.OwnerUser, "rev"}}
}

func closeRequest(pid int) Request {
	return Request{ProtocolVersion: 1, RequestID: "r", Action: actionCloseGPUProcess, Target: "third", PID: pid}
}

func TestDecodeCloseGPUProcess(t *testing.T) {
	good := `{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":4242}`
	r, code := Decode([]byte(good))
	if code != OK || r.PID != 4242 || r.Target != "third" {
		t.Fatalf("decode = %+v, %s", r, code)
	}
	for _, body := range []string{
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third"}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":null}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":0}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":-1}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":4194305}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":1.5}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"idle","pid":5}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","pid":5}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":null,"pid":5}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":5,"expected":{"incarnation":"i","version":"1","owner":"user","configurationRevision":"r"}}`,
		`{"protocolVersion":1,"requestId":"r","action":"close-gpu-process","target":"third","pid":5,"settings":{"timeoutMinutes":0,"settingsRevision":"s"}}`,
		`{"protocolVersion":1,"requestId":"r","action":"status","pid":5}`,
		`{"protocolVersion":1,"requestId":"r","action":"user-switch","target":"third","pid":5,"expected":{"incarnation":"i","version":"1","owner":"user","configurationRevision":"r"}}`,
	} {
		if _, code := Decode([]byte(body)); code != InvalidRequest {
			t.Errorf("%s: %s", body, code)
		}
	}
	if code := validateTransitionRequest(Request{Action: actionUserSwitch, Target: "third", PID: 5, Expected: &Expected{"i", "1", control.OwnerUser, "r"}}); code != InvalidRequest {
		t.Fatalf("PID on a transition = %s", code)
	}
}

func TestServiceReportsGPUInUseWithProcesses(t *testing.T) {
	s, b := gpuService(t)
	b.err = refused(&gpuruntime.GPUInUseError{Device: "0", Processes: []gpuruntime.GPUProcess{userProcess, otherProcess}})
	r := s.Handle(switchRequest())
	if r.Code != GPUInUse || r.Status != nil || r.GPUCheck == nil || r.GPUCheck.Failure != "" {
		t.Fatalf("response = %+v", r)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocolVersion":1,"requestId":"r","code":"gpu_in_use","gpuCheck":{"processes":[` +
		`{"pid":4242,"name":"python3","user":"alice","usedMiB":2048,"closable":true},` +
		`{"pid":77,"name":"renderfarm","user":"bob","usedMiB":null,"closable":false}]}}`
	if string(raw) != want {
		t.Fatalf("json = %s", raw)
	}
}

func TestServiceReportsGPUCheckFailure(t *testing.T) {
	s, b := gpuService(t)
	b.err = refused(&gpuruntime.GPUCheckError{Reason: "the nvidia-smi process query timed out"})
	r := s.Handle(switchRequest())
	if r.Code != GPUCheckFailed || r.GPUCheck == nil || r.GPUCheck.Failure != "the nvidia-smi process query timed out" || len(r.GPUCheck.Processes) != 0 {
		t.Fatalf("response = %+v", r)
	}
	// A timed-out check carries the deadline too; it stays a check failure,
	// never a free GPU or an uncertain timeout.
	b.err = refused(errors.Join(&gpuruntime.GPUCheckError{Reason: "timed out"}, context.DeadlineExceeded))
	if r := s.Handle(switchRequest()); r.Code != GPUCheckFailed {
		t.Fatalf("timed-out check = %+v", r)
	}
}

// A GPU error inside a started transition is not a refusal before any
// change, so it keeps the usual code and shows no processes.
func TestServiceGPUErrorAfterEffectKeepsUsualCode(t *testing.T) {
	s, b := gpuService(t)
	b.err = &gpuruntime.GPUInUseError{Processes: []gpuruntime.GPUProcess{userProcess}}
	if r := s.Handle(switchRequest()); r.Code != Unavailable || r.GPUCheck != nil {
		t.Fatalf("response = %+v", r)
	}
}

func TestServiceBoundsGPUProcessList(t *testing.T) {
	processes := make([]gpuruntime.GPUProcess, 100)
	for i := range processes {
		processes[i] = gpuruntime.GPUProcess{PID: i + 1, Name: strings.Repeat("n", 5000), User: "alice", UID: 1000}
	}
	s, b := gpuService(t)
	b.err = refused(&gpuruntime.GPUInUseError{Processes: processes})
	r := s.Handle(switchRequest())
	raw, _ := json.Marshal(r)
	if len(r.GPUCheck.Processes) != maxGPUProcesses || len(r.GPUCheck.Processes[0].Name) != maxProcessNameRunes || len(raw) > MaxResponseBytes {
		t.Fatalf("processes = %d, bytes = %d", len(r.GPUCheck.Processes), len(raw))
	}
}

func TestServiceCloseGPUProcess(t *testing.T) {
	s, b := gpuService(t)
	r := s.Handle(closeRequest(4242))
	if r.Code != OK || r.Status == nil || r.GPUCheck != nil || len(b.closed) != 1 || b.closed[0] != 4242 {
		t.Fatalf("free after close = %+v", r)
	}
	for name, tc := range map[string]struct {
		err  error
		code Code
	}{
		"still in use":   {&gpuruntime.GPUInUseError{Processes: []gpuruntime.GPUProcess{userProcess}}, GPUInUse},
		"check failed":   {&gpuruntime.GPUCheckError{Reason: "the nvidia-smi process query failed"}, GPUCheckFailed},
		"other user":     {gpuruntime.ErrGPUProcessNotOwned, NotPermitted},
		"invalid target": {supervisor.ErrInvalidTarget, InvalidRequest},
		"timeout":        {context.DeadlineExceeded, Timeout},
	} {
		b.closeErr = tc.err
		r := s.Handle(closeRequest(4242))
		if r.Code != tc.code || r.Status != nil {
			t.Errorf("%s: %+v", name, r)
		}
		if (tc.code == GPUInUse || tc.code == GPUCheckFailed) != (r.GPUCheck != nil) {
			t.Errorf("%s: gpuCheck = %+v", name, r.GPUCheck)
		}
	}
	b.closeErr = nil
	b.err = context.DeadlineExceeded
	if r := s.Handle(closeRequest(4242)); r.Code != Timeout {
		t.Fatalf("status after close = %+v", r)
	}
	b.err = nil
	b.closed = nil
	for _, req := range []Request{{ProtocolVersion: 1, RequestID: "r", Action: actionCloseGPUProcess, Target: "missing", PID: 5}, {ProtocolVersion: 1, RequestID: "r", Action: actionCloseGPUProcess, Target: "idle", PID: 5}} {
		if r := s.Handle(req); r.Code != InvalidRequest || len(b.closed) != 0 {
			t.Fatalf("target %q = %+v", req.Target, r)
		}
	}
	b.state.Version = 0
	if r := s.Handle(closeRequest(4242)); r.Code != Unavailable || r.Status != nil {
		t.Fatalf("invalid state = %+v", r)
	}
}

func TestGPUCheckDetailFallbacks(t *testing.T) {
	if got := gpuCheck(GPUInUse, errors.New("x")); got != nil {
		t.Fatalf("in use without processes = %+v", got)
	}
	if got := gpuCheck(GPUCheckFailed, gpuruntime.ErrGPUCheckFailed); got == nil || got.Failure == "" {
		t.Fatalf("failure without reason = %+v", got)
	}
	if got := gpuErrorCode(errors.New("x")); got != Unavailable {
		t.Fatalf("unrelated = %s", got)
	}
	if got := displayText(" a\x00b" + string(rune(0x2066)) + "c" + string([]byte{0xff}) + " "); got != "abc" {
		t.Fatalf("display = %q", got)
	}
}
