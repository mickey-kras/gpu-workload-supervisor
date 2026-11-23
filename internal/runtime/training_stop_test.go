package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSignalTrainingIsFencedByInvocation(t *testing.T) {
	m, r, p, job, command := trainerContractFixture(t)
	ctx := context.Background()
	if err := m.SignalTraining(ctx, p, job, "SIGHUP"); err == nil {
		t.Fatal("unsupported signal accepted")
	}
	for _, signal := range []string{TrainingStopSignal, TrainingKillSignal} {
		if err := m.SignalTraining(ctx, p, job, signal); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(r.calls, "/usr/bin/true --user kill --signal="+signal+" -- "+p.Unit) {
			t.Fatalf("missing %s kill in %v", signal, r.calls)
		}
	}
	calls := len(r.calls)
	r.outputs[command] = []byte(strings.Replace(string(r.outputs[command]), "InvocationID="+job.InvocationID, "InvocationID="+strings.Repeat("b", 32), 1))
	if err := m.SignalTraining(ctx, p, job, TrainingStopSignal); !errors.Is(err, ErrLaunchChanged) {
		t.Fatalf("signal to a later invocation: %v", err)
	}
	if slices.ContainsFunc(r.calls[calls:], func(call string) bool { return strings.Contains(call, " kill ") }) {
		t.Fatal("signal reached a later invocation")
	}
}

func TestTrainingExitedReadsTrainerState(t *testing.T) {
	m, r, p, job, command := trainerContractFixture(t)
	ctx := context.Background()
	exited := string(r.outputs[command])
	for state, want := range map[string]bool{
		"ActiveState=active\nSubState=exited":             true,
		"ActiveState=failed\nSubState=failed":             true,
		"ActiveState=inactive\nSubState=dead":             true,
		"ActiveState=active\nSubState=running":            false,
		"ActiveState=deactivating\nSubState=stop-sigterm": false,
	} {
		r.outputs[command] = []byte(strings.Replace(exited, "ActiveState=active\nSubState=exited", state, 1))
		if err := m.TrainingExited(ctx, p, job); (err == nil) != want {
			t.Fatalf("%q: %v", state, err)
		}
	}
	r.outputs[command] = []byte(strings.Replace(exited, "InvocationID="+job.InvocationID, "InvocationID=", 1))
	if err := m.TrainingExited(ctx, p, job); !errors.Is(err, ErrLaunchChanged) {
		t.Fatalf("missing invocation: %v", err)
	}
}

// A trainer whose main process exited while a child still saves its
// checkpoint has not exited: the grace window must continue.
func TestTrainingExitedWaitsForTrainerChildren(t *testing.T) {
	m, _, p, job, _ := trainerContractFixture(t)
	writeEvents(t, m.cgroups.root, strings.TrimPrefix(p.Cgroup, "/"), "populated 1\n")
	if err := m.TrainingExited(context.Background(), p, job); !errors.Is(err, errCgroupPopulated) {
		t.Fatalf("populated trainer cgroup = %v", err)
	}
	writeEvents(t, m.cgroups.root, strings.TrimPrefix(p.Cgroup, "/"), "populated 0\n")
	if err := m.TrainingExited(context.Background(), p, job); err != nil {
		t.Fatalf("empty trainer cgroup = %v", err)
	}
}
