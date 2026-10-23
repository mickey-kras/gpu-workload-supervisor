package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestTrainerInspectionUsesLaunchChecksWithoutLifecycleActions(t *testing.T) {
	m, runner, profile, _, command := trainerContractFixture(t)
	for _, path := range []string{profile.Trainer.JobFile, profile.Trainer.TerminalFile} {
		if err := os.WriteFile(path, []byte("private lifecycle data is not parsed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	runner.calls = nil
	if err := m.InspectTrainer(context.Background(), profile.ID); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if call != command && !strings.Contains(call, "--query-gpu=uuid") {
			t.Fatalf("inspection performed a lifecycle action: %s", call)
		}
	}
	for _, path := range []string{profile.Trainer.JobFile, profile.Trainer.TerminalFile} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != "private lifecycle data is not parsed" {
			t.Fatalf("inspection changed lifecycle data: %v", err)
		}
	}
	runner.outputs[command] = []byte(strings.Replace(string(runner.outputs[command]), "Restart=no", "Restart=always", 1))
	if err := m.InspectTrainer(context.Background(), profile.ID); err == nil {
		t.Fatal("inspection accepted loaded binding drift")
	}
	if err := m.InspectTrainer(context.Background(), "unknown"); err == nil {
		t.Fatal("unknown trainer accepted")
	}
	if err := m.InspectTrainer(context.Background(), m.config.Catalog.Profiles[0].ID); err == nil {
		t.Fatal("inference workload accepted as a trainer")
	}
}
