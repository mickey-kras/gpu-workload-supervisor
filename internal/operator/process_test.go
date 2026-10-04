package operator

import (
	"context"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type survivingBackend struct {
	backendFixture
	dir string
}

func (b *survivingBackend) OperatorTransition(ctx context.Context, a string, w control.Workload, e control.OperatorPrecondition) (control.State, error) {
	os.WriteFile(filepath.Join(b.dir, "admitted"), nil, 0600)
	time.Sleep(150 * time.Millisecond)
	os.WriteFile(filepath.Join(b.dir, "committed"), nil, 0600)
	return b.state, nil
}
func TestDetachedProcessSurvivesClientDisappearance(t *testing.T) {
	if os.Getenv("OPERATOR_TEST_CHILD") == "1" {
		if e := DetachSession(); e != nil {
			os.Exit(11)
		}
		body, e := ReadRequest(os.Stdin, time.Second)
		if e != nil {
			os.Exit(12)
		}
		req, code := Decode(body)
		if code != OK {
			os.Exit(13)
		}
		dir := os.Getenv("OPERATOR_TEST_DIR")
		b := &survivingBackend{dir: dir}
		b.state = control.InitialState("inc", time.Now())
		b.state.Phase = control.PhaseStable
		b.state.ActiveWorkload = control.WorkloadIdle
		s := Service{StatePath: filepath.Join(dir, "state"), Open: func(context.Context) (Session, error) {
			return Session{Backend: b, Revision: "rev", Workloads: []Workload{{"idle", "Idle"}}}, nil
		}}
		response := s.Handle(req)
		_ = WriteResponse(os.Stdout, response, time.Second)
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedProcessSurvivesClientDisappearance$")
	cmd.Env = append(os.Environ(), "OPERATOR_TEST_CHILD=1", "OPERATOR_TEST_DIR="+dir)
	input, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	output, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	input.Write([]byte(`{"protocolVersion":1,"requestId":"r","action":"return-control","expected":{"incarnation":"inc","version":"1","owner":"user","configurationRevision":"rev"}}` + "\n"))
	input.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, e = os.Stat(filepath.Join(dir, "admitted")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("not admitted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	output.Close()
	if e = cmd.Process.Signal(syscall.SIGHUP); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dir, "committed")); e != nil {
		t.Fatal("lost admitted operation", e)
	}
}
