package setup

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type setupDiscoveryRuntime struct {
	candidate control.TemporaryDiscoveryCandidate
	idleRuntime
	starts, stops int
	stopErr       error
}

func (r *setupDiscoveryRuntime) PrepareTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate) error {
	return nil
}
func (r *setupDiscoveryRuntime) StartTemporaryDiscovery(_ context.Context, v control.TemporaryDiscoveryCandidate) (control.TemporaryDiscoveryLaunchEvidence, error) {
	r.candidate = v
	r.starts++
	return control.TemporaryDiscoveryLaunchEvidence{InvocationID: strings.Repeat("a", 32), JobID: "2", ActivationTimestamp: "3"}, nil
}
func (r *setupDiscoveryRuntime) StopTemporaryDiscovery(context.Context, control.TemporaryDiscoveryCandidate, string) error {
	r.stops++
	return r.stopErr
}
func activatedTemporaryFixture(t *testing.T) (Backend, string, Profile, *setupDiscoveryRuntime) {
	t.Helper()
	b, home, request := fixture(t)
	if err := b.Apply(context.Background(), home, request); err != nil {
		t.Fatal(err)
	}
	raw, err := privateRead(filepath.Join(home, ".config/gpu-workload-supervisor/operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p Profile
	if err = json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	b.temporaryProfile = func() (Profile, error) { return p, nil }
	r := &setupDiscoveryRuntime{}
	b.makeRuntime = func(Request) (gpuruntime.Manager, error) { return r, nil }
	automatic, _, _ := automaticFixture(t, "ollama")
	b.runCommand = automatic.runCommand
	b.inspectAutomatic = automatic.inspectAutomatic
	b.probeApplication = automatic.probeApplication
	return b, home, p, r
}
func TestTemporarySetupInventoryCleanupAndExplicitRetry(t *testing.T) {
	b, home, p, r := activatedTemporaryFixture(t)
	ctx := context.Background()
	status, err := b.TemporaryStatus(ctx, home)
	if err != nil || !status.Available || status.Expected == nil {
		t.Fatalf("%+v %v", status, err)
	}
	snapStore, err := store.Open(ctx, p.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := snapStore.Catalog(ctx)
	snapStore.Close()
	result, err := b.TemporaryDiscover(ctx, home, TemporaryDiscoveryRequest{Unit: "ollama.service", Expected: *status.Expected, Consent: true, ExternalControlPaused: true})
	if err != nil || result.Error != "" || len(result.Models) != 1 || result.Session == nil || result.Session.Status != "completed" || r.starts != 1 || r.stops != 1 {
		t.Fatalf("%+v %v %+v", result, err, r)
	}
	snapStore, err = store.Open(ctx, p.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := snapStore.Catalog(ctx)
	snapStore.Close()
	if after.Revision != before.Revision {
		t.Fatal("temporary candidate replaced catalog")
	}
	status, err = b.TemporaryStatus(ctx, home)
	if err != nil || !status.Available {
		t.Fatal(err)
	}
	r.stopErr = errors.New("descendant remains")
	failed, err := b.TemporaryDiscover(ctx, home, TemporaryDiscoveryRequest{Unit: "ollama.service", Expected: *status.Expected, Consent: true, ExternalControlPaused: true})
	if err != nil || failed.Error == "" || failed.Session == nil || failed.Session.Status != "cleanup_required" || len(failed.Models) != 0 {
		t.Fatalf("%+v %v", failed, err)
	}
	status, err = b.TemporaryStatus(ctx, home)
	if err != nil || status.Available || status.Session == nil {
		t.Fatalf("%+v %v", status, err)
	}
	if err = checkNoTemporaryDiscovery(ctx, p.StatePath); !errors.Is(err, store.ErrTemporaryCleanupRequired) {
		t.Fatal(err)
	}
	r.stopErr = nil
	cleaned, err := b.TemporaryCleanup(ctx, home, TemporaryCleanupRequest{ID: failed.Session.ID, Token: failed.Session.Token, ExternalControlPaused: true})
	if err != nil || cleaned.Error != "" || cleaned.Session.Status != "completed" {
		t.Fatalf("%+v %v", cleaned, err)
	}
	if err = checkNoTemporaryDiscovery(ctx, p.StatePath); err != nil {
		t.Fatal(err)
	}
}
func TestTemporarySetupPrerequisitesConsentAndLocks(t *testing.T) {
	b, home, p, r := activatedTemporaryFixture(t)
	ctx := context.Background()
	status, err := b.TemporaryStatus(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	req := TemporaryDiscoveryRequest{Unit: "ollama.service", Expected: *status.Expected, Consent: true, ExternalControlPaused: true}
	for _, mode := range []string{"no-consent", "external-control", "bad-unit", "bad-version", "stale-version", "proxy-busy", "state-busy", "profile-unreadable", "profile-invalid", "bad-source"} {
		t.Run(mode, func(t *testing.T) {
			clone := b
			request := req
			var gate *lock.File
			switch mode {
			case "no-consent":
				request.Consent = false
			case "external-control":
				request.ExternalControlPaused = false
			case "bad-unit":
				request.Unit = "../foreign.service"
			case "bad-version":
				request.Expected.Version = "01"
			case "stale-version":
				request.Expected.Version = "9999"
			case "proxy-busy":
				gate, err = lock.TryAcquire(p.StatePath + ".proxy.lock")
			case "state-busy":
				gate, err = lock.TryAcquire(p.StatePath + ".lock")
			case "profile-unreadable":
				clone.temporaryProfile = func() (Profile, error) { return Profile{}, errors.New("broken profile") }
			case "profile-invalid":
				clone.temporaryProfile = func() (Profile, error) { return Profile{}, nil }
			case "bad-source":
				clone.inspectAutomatic = func(string, string) (gpuruntime.AutomaticLaunch, error) {
					return gpuruntime.AutomaticLaunch{}, errors.New("binding changed")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if gate != nil {
				defer gate.Close()
			}
			if result, err := clone.TemporaryDiscover(ctx, home, request); err == nil && result.Error == "" {
				t.Fatal("unsafe temporary start accepted")
			}
			if r.starts != 0 || r.stops != 0 {
				t.Fatal("runtime effect before prerequisite")
			}
		})
	}
	if _, err = b.TemporaryCleanup(ctx, home, TemporaryCleanupRequest{}); err == nil {
		t.Fatal("cleanup consent missing accepted")
	}
	blank := t.TempDir()
	missing, err := b.TemporaryStatus(ctx, blank)
	if err != nil || missing.Available || missing.Reason == "" {
		t.Fatalf("%+v %v", missing, err)
	}
	if err = os.MkdirAll(filepath.Join(blank, ".config/gpu-workload-supervisor"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(blank, ".config/gpu-workload-supervisor/operator.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = b.TemporaryStatus(ctx, blank); err == nil {
		t.Fatal("broken deployment treated as uninitialized")
	}
}

func TestTemporarySetupTransportsOrderedExternalSources(t *testing.T) {
	b, home, _, runtime := activatedTemporaryFixture(t)
	automatic, _, metadata := automaticFixture(t, "ollama")
	drop := control.LaunchSource{Path: "/opt/launch/10-options.conf", SHA256: strings.Repeat("c", 64)}
	metadata["ollama.service"]["DropInPaths"] = drop.Path
	original := automatic.inspectAutomatic
	b.runCommand = automatic.runCommand
	b.inspectAutomatic = func(path, app string) (gpuruntime.AutomaticLaunch, error) {
		launch, err := original(path, app)
		launch.DropIns = []control.LaunchSource{drop}
		launch.GPUUUID = "GPU-01234567-89ab-cdef-0123-456789abcdef"
		return launch, err
	}
	status, err := b.TemporaryStatus(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.TemporaryDiscover(context.Background(), home, TemporaryDiscoveryRequest{Unit: "ollama.service", Expected: *status.Expected, Consent: true, ExternalControlPaused: true})
	if err != nil || result.Error != "" || result.Session == nil || runtime.candidate.GPUUUID != "GPU-01234567-89ab-cdef-0123-456789abcdef" || result.Session.Candidate.GPUUUID != runtime.candidate.GPUUUID || !control.EqualLaunchSources(runtime.candidate.DropIns, []control.LaunchSource{drop}) || !control.EqualLaunchSources(result.Session.Candidate.DropIns, []control.LaunchSource{drop}) {
		t.Fatalf("lost source binding %+v %+v %v", runtime.candidate, result, err)
	}
}

func TestTemporaryStatusDoesNotHideBrokenInitializedDeployment(t *testing.T) {
	b, home, p, _ := activatedTemporaryFixture(t)
	if err := os.Remove(p.StatePath + ".deployment.json"); err != nil {
		t.Fatal(err)
	}
	if status, err := b.TemporaryStatus(context.Background(), home); err == nil || status.Available || strings.Contains(status.Reason, "initialized") {
		t.Fatalf("missing initialized marker hidden as fresh setup: %+v %v", status, err)
	}
}

func TestApplyRefusesUnfinishedTemporarySession(t *testing.T) {
	backend, home, r := fixture(t)
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, r.Profile.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: snap.Revision}
	tr := store.Transition{ID: "temporary", Source: state, Target: state, Previous: state, Initiator: "temporary-native-discovery", Phase: control.PhaseDraining, Deadline: time.Now().Add(time.Minute), ConfigurationRevision: snap.Revision}
	session := control.TemporaryDiscoverySession{ID: "temporary", Token: "token", Status: "starting", PriorStopped: true}
	if _, err := s.StartTemporaryDiscovery(ctx, e, tr, session); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := backend.Apply(ctx, home, r); !errors.Is(err, store.ErrTemporaryCleanupRequired) {
		t.Fatalf("apply during unfinished temporary session: %v", err)
	}
}
