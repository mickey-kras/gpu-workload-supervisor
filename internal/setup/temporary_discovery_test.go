package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

type cleanupRuntime struct {
	idleRuntime
	fail  bool
	stops int
}

func (r *cleanupRuntime) StopTemporaryDiscovery(_ context.Context, _ control.TemporaryDiscoveryCandidate, invocation string) error {
	if invocation != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		return errors.New("wrong recorded invocation")
	}
	r.stops++
	if r.fail {
		return errors.New("cgroup still populated")
	}
	return nil
}

func TestUpgradeTemporarySessionBlocksSetupUntilVerifiedCleanup(t *testing.T) {
	b, home, r := fixture(t)
	release := deployment.Release
	t.Cleanup(func() { deployment.Release = release })
	deployment.Release = "0.1.8"
	ctx := t.Context()
	if err := b.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	s := openStoreAt(t, r.Profile.StatePath)
	snapshot, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := "legacy-temporary"
	next, err := s.StartTransition(ctx, current.Version, store.Transition{ID: id, Source: current, Target: current, Previous: current, Initiator: "temporary-native-discovery", Deadline: time.Now().Add(time.Minute), ConfigurationRevision: snapshot.Revision})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	record := control.TemporaryDiscoverySession{ID: id, Token: "private-token", ConfigurationRevision: snapshot.Revision, Fence: next.LeaseFence, Version: next.Version, PriorStopped: true, Status: "running", InvocationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", LaunchEvidence: control.TemporaryDiscoveryLaunchEvidence{InvocationID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", JobID: "123", ActivationTimestamp: "456"}, Candidate: control.TemporaryDiscoveryCandidate{Unit: "ollama.service"}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO temporary_discovery_sessions(id,payload,status) VALUES(?,?,?)`, id, raw, record.Status); err != nil {
		t.Fatal(err)
	}
	legacy := snapshot.Catalog.Clone()
	legacy.Profiles[0].NativeModel = &control.NativeModel{Runtime: "ollama", Instance: "old", Model: "legacy", Endpoint: "http://127.0.0.1:11434", LaunchFile: "/home/test/ollama.service", LaunchSHA256: strings.Repeat("a", 64)}
	catalogRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workload_catalog SET catalog=?`, catalogRaw); err != nil {
		t.Fatal(err)
	}
	db.Close()
	deployment.Release = "0.1.9"
	r.ExpectedRevision = snapshot.Revision
	if err := b.Apply(ctx, home, r); !errors.Is(err, store.ErrTemporaryCleanupRequired) {
		t.Fatal("setup bypassed unfinished session", err)
	}
	status, err := TemporaryStatus(ctx, home)
	if err != nil || status.ID != id {
		t.Fatal(status, err)
	}
	runtime := &cleanupRuntime{fail: true}
	b.makeRuntime = func(request Request) (gpuruntime.Manager, error) {
		if err := request.Catalog.Validate(); err != nil {
			return nil, err
		}
		return runtime, nil
	}
	cleanup := TemporaryCleanupRequest{ID: id, Token: record.Token, ExternalControlPaused: true}
	for _, suffix := range []string{".lock", ".proxy.lock"} {
		held, err := lock.TryAcquire(r.Profile.StatePath + suffix)
		if err != nil {
			t.Fatal(err)
		}
		cleanupErr := b.TemporaryCleanup(ctx, home, cleanup)
		held.Close()
		if cleanupErr == nil || runtime.stops != 0 {
			t.Fatal("cleanup bypassed lifetime gate", suffix, cleanupErr)
		}
	}
	if err := b.TemporaryCleanup(ctx, home, cleanup); err == nil {
		t.Fatal("unreleased session completed")
	}
	if err := checkNoTemporaryDiscovery(ctx, r.Profile.StatePath); !errors.Is(err, store.ErrTemporaryCleanupRequired) {
		t.Fatal(err)
	}
	cleanup.Token = "wrong"
	if err := b.TemporaryCleanup(ctx, home, cleanup); !errors.Is(err, store.ErrStaleFence) || runtime.stops != 1 {
		t.Fatal("stale request reached runtime", err)
	}
	cleanup.Token = record.Token
	runtime.fail = false
	if err := b.TemporaryCleanup(ctx, home, cleanup); err != nil {
		t.Fatal(err)
	}
	if err := checkNoTemporaryDiscovery(ctx, r.Profile.StatePath); err != nil {
		t.Fatal(err)
	}
	if status, err := TemporaryStatus(ctx, home); err != nil || status != nil {
		t.Fatal(status, err)
	}
	if err := b.Apply(ctx, home, r); err != nil {
		t.Fatal("cleanup did not restore setup", err)
	}
}
