package store

import (
	"context"
	"errors"
	"testing"
)

func TestComfyUIAdmissionFailsClosedForBoundsAndChangedCatalog(t *testing.T) {
	s := testStore(t)
	a := admittedComfyUIFixture(t, s)
	ctx := context.Background()
	for _, mode := range []string{"attempt", "prompt", "contract", "upstream", "revision", "fence"} {
		b := a
		revision := ""
		switch mode {
		case "attempt":
			b.AttemptID = "bad"
		case "prompt":
			b.PromptID = "bad"
		case "contract":
			b.Contract = ""
		case "upstream":
			b.Upstream = "http://user:secret@localhost:8188"
		case "revision":
			revision = "stale"
		case "fence":
			b.Fence.Epoch++
		}
		if _, err := s.AdmitComfyUIWork(ctx, b, revision); err == nil {
			t.Fatalf("invalid %s accepted", mode)
		}
	}
	if _, err := s.PendingComfyUIAttempts(ctx, a.Workload, a.Upstream, a.Contract, "", 65); err == nil {
		t.Fatal("poll limit not enforced")
	}
	// Pressure fixture uses metadata only; no graph/payload is needed to hit
	// the durable unfinished-attempt cap.
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<4096)
	 INSERT INTO registered_work(request_id,workload,lease_incarnation,lease_epoch,registered_at,registration_token)
	 SELECT 'pressure-'||x,'media',?,?,'2026-10-12T00:00:00Z','fixture-token' FROM n`, a.Fence.Incarnation, a.Fence.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO comfyui_attempts SELECT request_id,request_id,request_id,?,? FROM registered_work`, a.Upstream, a.Contract); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitComfyUIWork(ctx, a, ""); !errors.Is(err, ErrComfyUILimit) {
		t.Fatalf("attempt pressure: %v", err)
	}
	if list, err := s.PendingComfyUIAttempts(ctx, a.Workload, a.Upstream, a.Contract, "", 64); err != nil || len(list) != 64 {
		t.Fatalf("unbounded poll: %d %v", len(list), err)
	}
}
