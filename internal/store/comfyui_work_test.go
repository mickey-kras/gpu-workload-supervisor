package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

func admittedComfyUIFixture(t *testing.T, s *Store) ComfyUIAttempt {
	t.Helper()
	ctx := context.Background()
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.ActiveWorkload = control.WorkloadMedia
	state.DesiredWorkload = control.WorkloadMedia
	state.Phase = control.PhaseStable
	state.Health = control.HealthHealthy
	state.Admission = control.AdmissionOpen
	state, err = s.UpdateState(ctx, state.Version, state)
	if err != nil {
		t.Fatal(err)
	}
	return ComfyUIAttempt{RequestID: "request", Workload: control.WorkloadMedia, Fence: state.LeaseFence, AttemptID: "11111111-1111-4111-8111-111111111111", PromptID: "22222222-2222-4222-8222-222222222222", Upstream: "http://127.0.0.1:8188", Contract: "fixture"}
}

func TestComfyUIDurableIdentityAtomicRollbackAndRetention(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := admittedComfyUIFixture(t, s)
	if _, err = s.db.Exec(`CREATE TRIGGER reject_attempt BEFORE INSERT ON comfyui_attempts BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitComfyUIWork(ctx, a, ""); err == nil {
		t.Fatal("metadata failure accepted")
	}
	if count, err := s.PendingWork(ctx); err != nil || count != 0 {
		t.Fatalf("partial admission survived: %d %v", count, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_attempt`); err != nil {
		t.Fatal(err)
	}
	token, err := s.AdmitComfyUIWork(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitComfyUIWork(ctx, a, ""); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("duplicate %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.PendingComfyUIAttempts(ctx, a.Workload, a.Upstream, a.Contract, "", 64)
	if err != nil || len(list) != 1 || list[0].PromptID != a.PromptID || list[0].RegistrationToken != token {
		t.Fatalf("lost durable identity: %#v %v", list, err)
	}
	state, _ := s.State(ctx)
	if _, err = s.RotateFenceAndCloseAdmission(ctx, state.Version); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishWorkToken(ctx, a.RequestID, a.Workload, a.Fence, token, WorkCompleted); err != nil {
		t.Fatal(err)
	}
	if list, err = s.PendingComfyUIAttempts(ctx, a.Workload, a.Upstream, a.Contract, "", 64); err != nil || len(list) != 0 {
		t.Fatal("terminal attempt still polled")
	}
	if n, err := s.PruneCompletedWork(ctx, time.Now().Add(time.Hour), 64); err != nil || n != 1 {
		t.Fatalf("retention blocked: %d %v", n, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM comfyui_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan metadata retained")
	}
}

func TestComfyUIMigrationPreservesLegacyAndDoesNotInventAttempts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	a := admittedComfyUIFixture(t, s)
	token, err := s.AdmitWorkToken(ctx, a.RequestID, "job", a.Workload, a.Fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE training_jobs; DROP TABLE broker_demands; DROP TABLE comfyui_attempts; DELETE FROM schema_migrations WHERE version>=14`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.PendingComfyUIAttempts(ctx, a.Workload, a.Upstream, a.Contract, "", 64)
	if err != nil || len(list) != 0 {
		t.Fatal("invented correlation")
	}
	if err = s.FinishWorkToken(ctx, a.RequestID, a.Workload, a.Fence, token, WorkCompleted); err != nil {
		t.Fatal(err)
	}
}
