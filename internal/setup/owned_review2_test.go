package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	gpuruntime "github.com/mickey-kras/gpu-workload-supervisor/internal/runtime"
)

// TestApplyRollsBackOwnedWritesOnFailedPrecheck reproduces the mutation leak:
// an owned update whose workload is not quiescent must not leave the
// overwritten unit behind, and the committed request must still apply cleanly.
func TestApplyRollsBackOwnedWritesOnFailedPrecheck(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Update the owned profile; make the workload non-quiescent.
	updated, _ := ownedFixtureProfile(t, home, "vision", 9101)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{updated}}
	r.ExpectedRevision = currentRevision(t, r)
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{err: errors.New("busy")}, nil }
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("non-quiescent workload applied")
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	data, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if err != nil || string(data) != string(raw) {
		t.Fatalf("committed unit not restored: %v", err)
	}
	if _, present, _ := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); present {
		t.Fatal("pending journal left behind after rollback")
	}
	// The committed catalog still applies without a collision.
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatalf("committed request rejected after rollback: %v", err)
	}
}

// TestRollbackRefusesForeignModifiedUnit never destroys content the supervisor
// did not write, even while rolling back.
func TestRollbackRefusesForeignModifiedUnit(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	plan := unitPlan{Writes: map[string][]byte{profile.Unit: raw}, proven: map[string]string{}, written: map[string]bool{}, prior: map[string][]byte{}, absent: map[string]bool{profile.Unit: true}}
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, newOwnedUnitJournal(plan, r.Profile.StatePath)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, profile.Unit), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.rollbackOwnedUnitWrites(context.Background(), home, r.Profile.SystemctlPath, plan); !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("foreign content destroyed or ignored: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, profile.Unit))
	if string(data) != "foreign" {
		t.Fatal("foreign content overwritten")
	}
}

func currentRevision(t *testing.T, r Request) string {
	t.Helper()
	s := openStoreAt(t, r.Profile.StatePath)
	snap, err := s.Catalog(context.Background())
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	return snap.Revision
}

// TestPlanOwnedUnitsPreservesAdoptedReference keeps the unit file when the
// requested catalog still claims it with the proven digest (owned converted
// to adopted), instead of deleting it after commit.
func TestPlanOwnedUnitsPreservesAdoptedReference(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, profile.Unit), raw, 0600); err != nil {
		t.Fatal(err)
	}
	accepted := control.CatalogSnapshot{Revision: "r1", Catalog: control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}}
	adopted := profile
	nativeCopy := *profile.NativeModel
	nativeCopy.Owned = nil
	nativeCopy.LaunchFile = filepath.Join(dir, profile.Unit)
	adopted.NativeModel = &nativeCopy
	adopted.NativeModel.LaunchSHA256 = digest(raw)
	req := ownedFixtureRequest(t, home)
	req.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	_ = r
	plan, err := backend.planOwnedUnits(req, accepted, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deletes) != 0 {
		t.Fatalf("adopted reference scheduled for deletion: %v", plan.Deletes)
	}
}

// TestPlanPreviewReportsOwnedUnitChanges makes the activation preview honest:
// owned writes and removals are listed instead of "user units unchanged".
func TestPlanPreviewReportsOwnedUnitChanges(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	preview, err := backend.Plan(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if !changesContain(preview.Changes, "Write supervisor-owned unit "+profile.Unit) {
		t.Fatalf("write missing from preview: %v", preview.Changes)
	}
	if changesContain(preview.Changes, "unchanged") {
		t.Fatalf("preview claims units unchanged: %v", preview.Changes)
	}
	s := openStoreAt(t, r.Profile.StatePath)
	if _, err := s.ReplaceCatalog(ctx, "", r.Catalog); err != nil {
		t.Fatal(err)
	}
	s.Close()
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	_, raw := ownedFixtureProfile(t, home, "vision", 9100)
	if err := os.WriteFile(filepath.Join(dir, profile.Unit), raw, 0600); err != nil {
		t.Fatal(err)
	}
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	preview, err = backend.Plan(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if !changesContain(preview.Changes, "Remove supervisor-owned unit "+profile.Unit) {
		t.Fatalf("removal missing from preview: %v", preview.Changes)
	}
}

func changesContain(changes []string, needle string) bool {
	for _, c := range changes {
		if strings.Contains(c, needle) {
			return true
		}
	}
	return false
}

// TestResumeReloadsAfterCompletedDelete refreshes systemd even when the
// journaled delete already removed the file, so the unit is not left cached
// and startable.
func TestResumeReloadsAfterCompletedDelete(t *testing.T) {
	backend, home, r := fixture(t)
	var verified []string
	backend.runCommand = recordingOwnedCommand(&verified, home)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{profile.Unit: digest(raw)}, Phase: ownedJournalCommitted}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(context.Background(), home, r); err != nil {
		t.Fatal(err)
	}
	if len(verified) != 1 || verified[0] != profile.Unit {
		t.Fatalf("no reload for completed delete: %v", verified)
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal not cleared")
	}
}

// TestPlanOwnedUnitsQualifiesOwnedLaunches fails the plan loudly when the host
// cannot run the owned launch, before anything becomes durable.
func TestPlanOwnedUnitsQualifiesOwnedLaunches(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.qualifyOwned = func(control.WorkloadProfile) error { return errors.New("untrusted executable") }
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	req := ownedFixtureRequest(t, home, profile)
	if _, err := backend.planOwnedUnits(req, control.CatalogSnapshot{}, home); err == nil || !strings.Contains(err.Error(), "untrusted executable") {
		t.Fatalf("unqualified owned launch planned: %v", err)
	}
}

// TestOwnedProfileRejectsGrammarUnsafeModelPath surfaces the catalog grammar
// rule at draft synthesis: whitespace in a rendered field fails loudly.
func TestOwnedProfileRejectsGrammarUnsafeModelPath(t *testing.T) {
	draft := Draft{ID: "vision", Label: "Vision", App: "llama.cpp", Model: "vision", Binding: &DraftBinding{Instance: "owned", Owned: &DraftOwnedLaunch{ModelPath: "/models/my model.gguf", Port: 9100}}}
	if _, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", t.TempDir()); err == nil {
		t.Fatal("whitespace model path synthesized")
	}
}

// TestMaintenanceResumeCompletesCrashWindowApply proves the full Apply resume
// succeeds after the post-commit crash window and clears the fence.
func TestMaintenanceResumeCompletesCrashWindowApply(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	s := openStoreAt(t, r.Profile.StatePath)
	if _, err := s.ReplaceCatalog(ctx, "", r.Catalog); err != nil {
		t.Fatal(err)
	}
	s.Close()
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stale.Unit), staleRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{stale.Unit: digest(staleRaw)}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.Profile.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
		t.Fatal(err)
	}
	fence, err := json.Marshal(activation{Request: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "activation.json"), fence, 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, stale.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("crash-window delete dropped during resume apply")
	}
	marker, err := deployment.Read(r.Profile.StatePath)
	if err != nil || marker.Maintenance {
		t.Fatalf("fence not cleared: %+v %v", marker, err)
	}
}

// TestManagerCgroupQueryRequiresConcreteAnswer rejects empty or malformed
// manager answers instead of deriving an uncontrollable cgroup.
func TestManagerCgroupQueryRequiresConcreteAnswer(t *testing.T) {
	ok := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("ControlGroup=/user.slice/user-1000.slice/user@1000.service\n"), nil
	}
	got, err := managerCgroup(context.Background(), ok, "systemctl")
	if err != nil || got != "/user.slice/user-1000.slice/user@1000.service" {
		t.Fatalf("%q %v", got, err)
	}
	empty := func(context.Context, string, ...string) ([]byte, error) { return []byte("ControlGroup=\n"), nil }
	if _, err := managerCgroup(context.Background(), empty, "systemctl"); !errors.Is(err, ErrManagerCgroupMismatch) {
		t.Fatalf("empty cgroup accepted: %v", err)
	}
	duplicate := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("ControlGroup=/user.slice/a\nControlGroup=/user.slice/evil\n"), nil
	}
	if _, err := managerCgroup(context.Background(), duplicate, "systemctl"); !errors.Is(err, ErrManagerCgroupMismatch) {
		t.Fatalf("duplicate cgroup accepted: %v", err)
	}
	broken := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no bus") }
	if _, err := managerCgroup(context.Background(), broken, "systemctl"); err == nil {
		t.Fatal("query failure accepted")
	}
}

// TestRollbackRestoresOverwrittenUnit restores snapshotted content when a
// pre-commit failure follows an overwrite of an existing owned unit.
func TestRollbackRestoresOverwrittenUnit(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, profile.Unit), []byte("old render"), 0600); err != nil {
		t.Fatal(err)
	}
	plan := unitPlan{Writes: map[string][]byte{profile.Unit: raw}, proven: map[string]string{profile.Unit: digest([]byte("old render"))}, prior: map[string][]byte{}, absent: map[string]bool{}, written: map[string]bool{}}
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, newOwnedUnitJournal(plan, r.Profile.StatePath)); err != nil {
		t.Fatal(err)
	}
	if err := backend.rollbackOwnedUnitWrites(context.Background(), home, r.Profile.SystemctlPath, plan); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, profile.Unit))
	if string(data) != "old render" {
		t.Fatalf("prior content not restored: %q", data)
	}
}

// TestPlanOwnedUnitsDeletesWhenAdoptedReferenceDrifts keeps the fail-closed
// delete when the requested catalog references the unit with a different
// fingerprint.
func TestPlanOwnedUnitsDeletesWhenAdoptedReferenceDrifts(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, profile.Unit), raw, 0600); err != nil {
		t.Fatal(err)
	}
	accepted := control.CatalogSnapshot{Revision: "r1", Catalog: control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}}
	adopted := profile
	nativeCopy := *profile.NativeModel
	nativeCopy.Owned = nil
	nativeCopy.LaunchFile = filepath.Join(dir, profile.Unit)
	adopted.NativeModel = &nativeCopy
	adopted.NativeModel.LaunchSHA256 = digest([]byte("different render"))
	req := ownedFixtureRequest(t, home)
	req.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	plan, err := backend.planOwnedUnits(req, accepted, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deletes) != 1 {
		t.Fatalf("drifted reference suppressed the delete: %v", plan.Deletes)
	}
}

// TestManagerCgroupWrapsSystemBackend exercises the production wrapper's error
// propagation with a systemctl binary that cannot exist.
func TestManagerCgroupWrapsSystemBackend(t *testing.T) {
	if _, err := ManagerCgroup(context.Background(), "/nonexistent/systemctl"); err == nil {
		t.Fatal("missing systemctl accepted")
	}
}

// TestRollbackReportsReloadFailure surfaces a failed daemon-reload during
// rollback instead of pretending the installation was restored.
func TestRollbackReportsReloadFailure(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("reload failed") }
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	plan := unitPlan{Writes: map[string][]byte{profile.Unit: raw}, proven: map[string]string{}, written: map[string]bool{}, prior: map[string][]byte{}, absent: map[string]bool{profile.Unit: true}}
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, newOwnedUnitJournal(plan, r.Profile.StatePath)); err != nil {
		t.Fatal(err)
	}
	if err := backend.rollbackOwnedUnitWrites(context.Background(), home, r.Profile.SystemctlPath, plan); err == nil {
		t.Fatal("reload failure swallowed")
	}
}

// TestClearOwnedUnitJournalWithoutJournal is the idempotent no-op path.
func TestClearOwnedUnitJournalWithoutJournal(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := clearOwnedUnitJournal(root); err != nil {
		t.Fatal(err)
	}
}

// TestRollbackLeavesUnchangedUnitByteIdentical reproduces the truncation
// blocker: a mixed plan where one unit's render already matches disk must not
// be touched by rollback.
func TestRollbackLeavesUnchangedUnitByteIdentical(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	vision, visionRaw := ownedFixtureProfile(t, home, "vision", 9100)
	code, codeRaw := ownedFixtureProfile(t, home, "code", 9102)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, vision.Unit), visionRaw, 0600); err != nil {
		t.Fatal(err)
	}
	plan := unitPlan{Writes: map[string][]byte{vision.Unit: visionRaw, code.Unit: codeRaw}, proven: map[string]string{}, written: map[string]bool{}, prior: map[string][]byte{}, absent: map[string]bool{}}
	if err := backend.applyOwnedUnitWrites(ctx, home, plan, newOwnedUnitJournal(plan, r.Profile.StatePath)); err != nil {
		t.Fatal(err)
	}
	if err := backend.rollbackOwnedUnitWrites(ctx, home, r.Profile.SystemctlPath, plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, vision.Unit))
	if err != nil || string(data) != string(visionRaw) {
		t.Fatalf("unchanged unit mutated by rollback: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, code.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("added unit not removed by rollback")
	}
}

// TestResumeRejectsMismatchedRecoveryRequest blocks a different request from
// consuming the interrupted activation's journal (the relocation crash case).
func TestResumeRejectsMismatchedRecoveryRequest(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stale.Unit), staleRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{stale.Unit: digest(staleRaw)}, Phase: ownedJournalCommitted}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	original := ownedFixtureRequest(t, home)
	original.Profile.StatePath = filepath.Join(home, "other/state.db")
	fence, err := json.Marshal(activation{Request: original})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "activation.json"), fence, 0600); err != nil {
		t.Fatal(err)
	}
	// The fence proves the record current: a different request is rejected.
	if err := os.MkdirAll(filepath.Dir(r.Profile.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(context.Background(), home, r); err == nil {
		t.Fatal("mismatched recovery request consumed the journal")
	}
	if _, present, _ := readOwnedUnitJournal(root); !present {
		t.Fatal("journal consumed by mismatched request")
	}
	if _, err := os.Lstat(filepath.Join(dir, stale.Unit)); err != nil {
		t.Fatal("proven unit deleted by mismatched request")
	}
}

// TestSuccessfulApplyRetiresActivationRecord leaves no activation.json behind,
// so only genuinely interrupted activations guard the journal.
func TestSuccessfulApplyRetiresActivationRecord(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(context.Background(), home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".config/gpu-workload-supervisor/activation.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("activation record not retired")
	}
}

// TestJournalRetireSyncsDirectories makes unit removal and journal retirement
// durable in order: unit directory first, journal directory last.
func TestJournalRetireSyncsDirectories(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	var synced []string
	restore := syncDir
	syncDir = func(path string) error { synced = append(synced, path); return nil }
	defer func() { syncDir = restore }()
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stale.Unit), staleRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{stale.Unit: digest(staleRaw)}, Phase: ownedJournalCommitted}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(context.Background(), home, r); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 2 || synced[0] != dir || synced[1] != root {
		t.Fatalf("sync order %v", synced)
	}
}

// TestPlanOwnedUnitsRejectsForeignHomeLaunchFile refuses to write or commit an
// owned profile whose launch file escapes the setup home.
func TestPlanOwnedUnitsRejectsForeignHomeLaunchFile(t *testing.T) {
	backend, home, _ := fixture(t)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	profile.NativeModel.LaunchFile = "/home/other/.config/systemd/user/" + profile.Unit
	req := ownedFixtureRequest(t, home, profile)
	if _, err := backend.planOwnedUnits(req, control.CatalogSnapshot{}, home); !errors.Is(err, ErrOwnedLaunchFileOutsideHome) {
		t.Fatalf("foreign-home launch file planned: %v", err)
	}
}

// TestPlanPreviewMatchesPlanSemantics omits idempotent writes and referenced
// deletions from the user-facing preview.
func TestPlanPreviewMatchesPlanSemantics(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	r.ExpectedRevision = currentRevision(t, r)
	preview, err := backend.Plan(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if changesContain(preview.Changes, "Write supervisor-owned unit") || changesContain(preview.Changes, "Remove supervisor-owned unit") {
		t.Fatalf("idempotent apply misreported: %v", preview.Changes)
	}
	if !changesContain(preview.Changes, "unchanged") {
		t.Fatalf("unchanged line missing: %v", preview.Changes)
	}
	// Adopted conversion keeps the file: no removal in the preview.
	adopted := profile
	nativeCopy := *profile.NativeModel
	nativeCopy.Owned = nil
	adopted.NativeModel = &nativeCopy
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	preview, err = backend.Plan(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if changesContain(preview.Changes, "Remove supervisor-owned unit") {
		t.Fatalf("preserved unit listed for removal: %v", preview.Changes)
	}
	_ = raw
}

// TestSyncDirDurability covers the real directory fsync helper directly.
func TestSyncDirDurability(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing directory synced")
	}
}

// TestRequireMatchingActivationRejectsCorruptRecord fails loudly on an
// unreadable activation record instead of consuming the journal.
func TestRequireMatchingActivationRejectsCorruptRecord(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "activation.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, r := fixture(t)
	if err := requireMatchingActivation(root, r); err == nil {
		t.Fatal("corrupt activation record accepted")
	}
}

// TestPlanPreviewFailsOnUnrenderableOwnedProfile surfaces a fingerprint/render
// disagreement instead of printing a false preview.
func TestPlanPreviewFailsOnUnrenderableOwnedProfile(t *testing.T) {
	backend, home, r := fixture(t)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	profile.NativeModel.LaunchSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if _, err := backend.Plan(home, r); err == nil {
		t.Fatal("unrenderable owned profile previewed")
	}
}

// failReloadCommand answers everything except daemon-reload, which fails.
func failReloadCommand(_ context.Context, _ string, args ...string) ([]byte, error) {
	for _, arg := range args {
		if arg == "--property=NeedDaemonReload" {
			return []byte("NeedDaemonReload=no\n"), nil
		}
		if arg == "daemon-reload" {
			return nil, errors.New("reload failed")
		}
	}
	return []byte("ok\n"), nil
}

// TestApplyRollsBackWhenReloadFails covers the post-write reload failure: the
// freshly written unit is removed and the pending journal dropped.
func TestApplyRollsBackWhenReloadFails(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = failReloadCommand
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(context.Background(), home, r); err == nil {
		t.Fatal("failed reload applied")
	}
	if _, err := os.Lstat(filepath.Join(ownedUnitDirectory(home), profile.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unit left behind after failed reload")
	}
	// The rollback reload fails too, so the pending journal must survive for
	// the next apply to finish recovery.
	if _, present, _ := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); !present {
		t.Fatal("journal dropped while rollback could not complete")
	}
}

// TestApplyRollsBackPartialWriteOnCollision covers a mid-loop collision: the
// unit written before the collision is restored to the committed render, and
// the foreign file is untouched.
func TestApplyRollsBackPartialWriteOnCollision(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	code, codeRaw := ownedFixtureProfile(t, home, "code", 9102)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{code}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Update code and add vision (separate instance); plant foreign content at
	// vision's path so the write loop fails after rewriting code.
	updated, _ := ownedFixtureProfile(t, home, "code", 9103)
	draft := ownedDraft("vision", 9100)
	draft.Binding.Instance = "second"
	vision, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	dir := ownedUnitDirectory(home)
	if err := os.WriteFile(filepath.Join(dir, vision.Unit), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{updated, vision}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("collision not surfaced: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, code.Unit))
	if err != nil || string(data) != string(codeRaw) {
		t.Fatalf("committed unit not restored after partial write: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, vision.Unit))
	if string(data) != "foreign" {
		t.Fatal("foreign content destroyed")
	}
	if _, present, _ := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); present {
		t.Fatal("journal left behind after collision rollback")
	}
}

// TestResumeRecoversPendingWritesWithoutFence reproduces the crash between a
// unit write and maintenance entry: the journaled write is removed before the
// journal is retired, so the unit cannot become a permanent orphan.
func TestResumeRecoversPendingWritesWithoutFence(t *testing.T) {
	backend, home, r := fixture(t)
	var verified []string
	backend.runCommand = recordingOwnedCommand(&verified, home)
	ctx := context.Background()
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stale.Unit), staleRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{stale.Unit: digest(staleRaw)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, stale.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uncommitted write not recovered")
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal not retired")
	}
	if len(verified) != 1 || verified[0] != stale.Unit {
		t.Fatalf("recovery skipped reload: %v", verified)
	}
}

// TestResumeRestoresOverwrittenPendingWrite covers the update variant of the
// same crash window: the committed render is restored from the accepted
// catalog, not deleted.
func TestResumeRestoresOverwrittenPendingWrite(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	updated, updatedRaw := ownedFixtureProfile(t, home, "vision", 9101)
	dir := ownedUnitDirectory(home)
	if err := os.WriteFile(filepath.Join(dir, profile.Unit), updatedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{profile.Unit: digest(updatedRaw)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, profile.Unit))
	if string(data) != string(raw) {
		t.Fatal("committed render not restored")
	}
	_ = updated
}

// TestResumeRefusesTamperedPendingWrite never recovers over foreign content.
func TestResumeRefusesTamperedPendingWrite(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	stale, _ := ownedFixtureProfile(t, home, "stale", 9300)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stale.Unit), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{stale.Unit: digest([]byte("journaled render"))}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(context.Background(), home, r); !errors.Is(err, ErrOwnedUnitModified) {
		t.Fatalf("tampered write recovered over: %v", err)
	}
	if _, present, _ := readOwnedUnitJournal(root); !present {
		t.Fatal("journal consumed despite tamper")
	}
}

// TestPlanPreviewKeepsStateDatabaseReadOnly proves the preview never writes to
// the state database: a read-only database file still previews fine.
func TestPlanPreviewKeepsStateDatabaseReadOnly(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.Profile.StatePath, 0400); err != nil {
		t.Fatal(err)
	}
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	preview, err := backend.Plan(home, r)
	if err != nil {
		t.Fatalf("preview wrote to a read-only database: %v", err)
	}
	if !changesContain(preview.Changes, "Remove supervisor-owned unit "+profile.Unit) {
		t.Fatalf("removal missing: %v", preview.Changes)
	}
}

// TestProvenReplaceRejectsConcurrentDisplacement injects a replace between the
// ownership re-check and the rename: the foreign inode is detected and left
// intact, and the write fails loudly.
func TestProvenReplaceRejectsConcurrentDisplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gws-owned-vision.service")
	if err := os.WriteFile(path, []byte("proven render"), 0600); err != nil {
		t.Fatal(err)
	}
	// Pin the verified inode so the displacement cannot reuse its number.
	pin, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	restore := ownedStat
	ownedStat = func(p string) (os.FileInfo, error) {
		// A concurrent writer atomically replaces the file after our digest
		// proof but before our rename.
		if err := os.Remove(p); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte("concurrent render"), 0600); err != nil {
			return nil, err
		}
		return os.Stat(p)
	}
	defer func() { ownedStat = restore }()
	err = provenReplace(path, []byte("new render"), digest([]byte("proven render")))
	if !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("concurrent displacement not detected: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "concurrent render" {
		t.Fatal("foreign content destroyed")
	}
}

// TestProvenReplaceFailsOnProofMismatch never replaces content the proof does
// not cover.
func TestProvenReplaceFailsOnProofMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gws-owned-vision.service")
	if err := os.WriteFile(path, []byte("unexpected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := provenReplace(path, []byte("new render"), digest([]byte("proven render"))); !errors.Is(err, ErrOwnedUnitModified) {
		t.Fatalf("unproven content replaced: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "unexpected" {
		t.Fatal("content modified")
	}
}

// TestDiscoveryReportsUntrustedOwnedUnit surfaces a gws-owned file that fails
// the private-file check instead of silently omitting it.
func TestDiscoveryReportsUntrustedOwnedUnit(t *testing.T) {
	home := t.TempDir()
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gws-owned-vision.service"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "gws-owned-dir.service"), 0700); err != nil {
		t.Fatal(err)
	}
	owned := discoverOwnedUnits(home, control.Catalog{})
	if len(owned) != 2 {
		t.Fatalf("untrusted entries omitted: %+v", owned)
	}
	for _, status := range owned {
		if status.State != "modified" || status.Digest != "" {
			t.Fatalf("untrusted entry misclassified: %+v", status)
		}
	}
}

// TestFinalizeDeletesSyncUnitDir makes the finalize path's unlinks durable
// before the journal is retired.
func TestFinalizeDeletesSyncUnitDir(t *testing.T) {
	backend, home, r := fixture(t)
	var synced []string
	restore := syncDir
	syncDir = func(path string) error { synced = append(synced, path); return nil }
	defer func() { syncDir = restore }()
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stale.Unit), staleRaw, 0600); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{stale.Unit: digest(staleRaw)}, Phase: ownedJournalCommitted}
	if err := backend.applyOwnedUnitDeletes(context.Background(), home, journal); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("unit dir not synced after delete: %v", synced)
	}
}

// TestRollbackSyncsUnitDirBeforeJournalClear makes rollback removals durable
// before abortOwnedUnitWrites retires the pending journal.
func TestRollbackSyncsUnitDirBeforeJournalClear(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	var synced []string
	restore := syncDir
	syncDir = func(path string) error { synced = append(synced, path); return nil }
	defer func() { syncDir = restore }()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	plan := unitPlan{Writes: map[string][]byte{profile.Unit: raw}, proven: map[string]string{}, prior: map[string][]byte{}, absent: map[string]bool{}, written: map[string]bool{}}
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, newOwnedUnitJournal(plan, r.Profile.StatePath)); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnedUnitJournal(root, newOwnedUnitJournal(plan, r.Profile.StatePath)); err != nil {
		t.Fatal(err)
	}
	if err := backend.abortOwnedUnitWrites(context.Background(), home, root, r, plan, errors.New("precheck failed")); err == nil {
		t.Fatal("cause swallowed")
	}
	if len(synced) != 2 || synced[0] != ownedUnitDirectory(home) || synced[1] != root {
		t.Fatalf("sync order %v", synced)
	}
}

// TestProvenReplaceErrorPaths fails loudly on missing files, write faults, and
// post-rename content disagreement.
func TestProvenReplaceErrorPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gws-owned-vision.service")
	if err := provenReplace(path, []byte("x"), digest([]byte("x"))); err == nil {
		t.Fatal("missing file replaced")
	}
	if err := os.WriteFile(path, []byte("proven render"), 0600); err != nil {
		t.Fatal(err)
	}
	proof := digest([]byte("proven render"))
	restoreWrite := ownedAtomicWrite
	ownedAtomicWrite = func(string, []byte) error { return errors.New("write fault") }
	if err := provenReplace(path, []byte("new render"), proof); err == nil {
		t.Fatal("write fault swallowed")
	}
	ownedAtomicWrite = func(p string, _ []byte) error { return os.WriteFile(p, []byte("wrong"), 0600) }
	if err := provenReplace(path, []byte("new render"), proof); !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("landed content disagreement accepted: %v", err)
	}
	ownedAtomicWrite = restoreWrite
	if err := provenReplace(path, []byte("new render"), digest([]byte("wrong"))); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new render" {
		t.Fatal("replace did not land")
	}
}

// TestPlanPreviewFailsOnUnreadableUnitFile surfaces unit-file read faults in
// the preview instead of pretending the write is planned.
func TestPlanPreviewFailsOnUnreadableUnitFile(t *testing.T) {
	backend, home, r := fixture(t)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(filepath.Join(dir, profile.Unit), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Plan(home, r); err == nil {
		t.Fatal("unreadable unit file previewed")
	}
}

// TestResumeRejectsForeignStatePathJournal reproduces the pre-maintenance
// crash followed by a relocated request: the journal is bound to its original
// state database and the mismatched request fails without consuming anything.
func TestResumeRejectsForeignStatePathJournal(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stale.Unit), staleRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: filepath.Join(home, "original/state.db"), Writes: map[string]string{stale.Unit: digest(staleRaw)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(context.Background(), home, r); !errors.Is(err, ErrOwnedJournalStateMismatch) {
		t.Fatalf("foreign state path consumed journal: %v", err)
	}
	if _, present, _ := readOwnedUnitJournal(root); !present {
		t.Fatal("journal consumed by mismatched request")
	}
	data, _ := os.ReadFile(filepath.Join(dir, stale.Unit))
	if string(data) != string(staleRaw) {
		t.Fatal("journaled unit mutated by mismatched request")
	}
}

// TestJournalWithoutStatePathRejected fails closed on journals that predate
// the state-path binding.
func TestJournalWithoutStatePathRejected(t *testing.T) {
	root := t.TempDir()
	journal := unitJournal{Version: 1, Writes: map[string]string{}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readOwnedUnitJournal(root); err == nil {
		t.Fatal("journal without statePath accepted")
	}
}

// TestRollbackFailureRetainsPendingJournal injects a restore fault: the abort
// fails loudly, the journal survives, and the next apply replays recovery.
func TestRollbackFailureRetainsPendingJournal(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Fault: restoring the committed render fails transiently.
	restore := ownedAtomicWrite
	ownedAtomicWrite = func(path string, data []byte) error {
		if string(data) == string(raw) {
			return errors.New("transient write fault")
		}
		return restore(path, data)
	}
	defer func() { ownedAtomicWrite = restore }()
	updated, updatedRaw := ownedFixtureProfile(t, home, "vision", 9101)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{updated}}
	r.ExpectedRevision = currentRevision(t, r)
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{err: errors.New("busy")}, nil }
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("apply succeeded despite rollback fault")
	}
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	ownedAtomicWrite = restore
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if _, present, _ := readOwnedUnitJournal(root); !present {
		t.Fatal("journal dropped while rollback failed")
	}
	// Recovery replays: the committed render is restored and the journal retired.
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != string(raw) {
		t.Fatal("committed render not restored by recovery")
	}
	_ = updatedRaw
}

// TestResumeReloadsWhenPendingRecoveryMatchesDisk refreshes systemd even when
// recovery finds nothing to mutate: the journaled write may already have been
// loaded.
func TestResumeReloadsWhenPendingRecoveryMatchesDisk(t *testing.T) {
	backend, home, r := fixture(t)
	var verified []string
	backend.runCommand = recordingOwnedCommand(&verified, home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Crash window: journaled write landed but the file already matches the
	// accepted render (rollback restored bytes, reload never ran).
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{profile.Unit: digest([]byte("uncommitted render"))}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	r.ExpectedRevision = currentRevision(t, r)
	verified = nil
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if len(verified) != 1 || verified[0] != profile.Unit {
		t.Fatalf("matching recovery skipped reload: %v", verified)
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal not retired")
	}
	data, _ := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != string(raw) {
		t.Fatal("accepted render mutated")
	}
}

// TestProvenDeleteRejectsConcurrentDisplacement injects a replace between the
// delete proof and the unlink: the foreign inode survives.
func TestProvenDeleteRejectsConcurrentDisplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gws-owned-vision.service")
	if err := os.WriteFile(path, []byte("proven render"), 0600); err != nil {
		t.Fatal(err)
	}
	pin, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	restore := ownedStat
	ownedStat = func(p string) (os.FileInfo, error) {
		if err := os.Remove(p); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte("concurrent render"), 0600); err != nil {
			return nil, err
		}
		return os.Stat(p)
	}
	defer func() { ownedStat = restore }()
	if err := provenDelete(path, digest([]byte("proven render"))); !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("concurrent displacement deleted: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "concurrent render" {
		t.Fatal("foreign content destroyed")
	}
}

// TestProvenDeleteFailsOnProofMismatch never unlinks unproven content.
func TestProvenDeleteFailsOnProofMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gws-owned-vision.service")
	if err := os.WriteFile(path, []byte("unexpected"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := provenDelete(path, digest([]byte("proven render"))); !errors.Is(err, ErrOwnedUnitModified) {
		t.Fatalf("unproven content deleted: %v", err)
	}
}

// TestDiscoveryAccountsAdoptedOwnedBinding mirrors the runtime orphan scan: an
// exact adopted binding manages the file, a drifted one does not.
func TestDiscoveryAccountsAdoptedOwnedBinding(t *testing.T) {
	home := t.TempDir()
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("[Unit]\nDescription=converted\n")
	if err := os.WriteFile(filepath.Join(dir, "gws-owned-vision.service"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	adopted := control.WorkloadProfile{
		ID: "vision", Label: "Vision", Adapter: "systemd", Unit: "gws-owned-vision.service",
		Cgroup: "/user.slice/x", HealthURL: "http://127.0.0.1:9100/health",
		NativeModel: &control.NativeModel{Runtime: "llama.cpp", Instance: "second", Model: "vision", Endpoint: "http://127.0.0.1:9100", LaunchFile: filepath.Join(dir, "gws-owned-vision.service"), LaunchSHA256: digest(raw)},
	}
	owned := discoverOwnedUnits(home, control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}})
	if len(owned) != 1 || owned[0].State != "managed" {
		t.Fatalf("adopted binding not accounted: %+v", owned)
	}
	adopted.NativeModel.LaunchSHA256 = digest([]byte("drifted"))
	owned = discoverOwnedUnits(home, control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}})
	if len(owned) != 1 || owned[0].State != "orphaned" {
		t.Fatalf("drifted binding misclassified: %+v", owned)
	}
}

// TestApplyDeletesDroppedAdoptedOwnedBinding covers the conversion lifecycle:
// owned -> adopted keeping the file -> binding dropped. The dropped binding
// schedules the file for proven deletion so preflight cannot latch an orphan.
func TestApplyDeletesDroppedAdoptedOwnedBinding(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Convert to adopted, keeping the exact binding.
	adopted := profile
	nativeCopy := *profile.NativeModel
	nativeCopy.Owned = nil
	adopted.NativeModel = &nativeCopy
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ownedUnitDirectory(home), profile.Unit)
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("conversion dropped the preserved file")
	}
	// Drop the binding entirely: the file must be deleted with content proof.
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dropped adopted binding left the owned file behind")
	}
}

// TestApplyRefusesDriftedAdoptedOwnedBinding fails closed when the file behind
// a dropped adopted binding no longer matches its proven fingerprint.
func TestApplyRefusesDriftedAdoptedOwnedBinding(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	adopted := profile
	nativeCopy := *profile.NativeModel
	nativeCopy.Owned = nil
	adopted.NativeModel = &nativeCopy
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ownedUnitDirectory(home), profile.Unit)
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); !errors.Is(err, ErrOwnedUnitModified) {
		t.Fatalf("drifted adopted binding deleted: %v", err)
	}
}

// TestResumeKeepsAdoptedBindingDuringPendingRecovery never deletes a file the
// committed catalog still binds exactly, even when a pending journal names it.
func TestResumeKeepsAdoptedBindingDuringPendingRecovery(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	adopted := profile
	nativeCopy := *profile.NativeModel
	nativeCopy.Owned = nil
	adopted.NativeModel = &nativeCopy
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{profile.Unit: digest([]byte("uncommitted render"))}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != string(raw) {
		t.Fatal("adopted-bound file deleted during recovery")
	}
	// Drifted bytes fail loudly instead of being deleted.
	if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), profile.Unit), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	journal2 := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{profile.Unit: digest([]byte("uncommitted render"))}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal2); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); !errors.Is(err, ErrOwnedUnitModified) {
		t.Fatalf("drifted adopted binding recovered over: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != "tampered" {
		t.Fatal("drifted file destroyed")
	}
}

// TestPlanPreviewListsDroppedAdoptedOwnedBindingRemoval keeps preview honest:
// dropping an adopted binding deletes the preserved file, so the preview must
// say so before Apply runs.
func TestPlanPreviewListsDroppedAdoptedOwnedBindingRemoval(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	adopted := profile
	nativeCopy := *profile.NativeModel
	nativeCopy.Owned = nil
	adopted.NativeModel = &nativeCopy
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{adopted}}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Conversion alone must not preview a removal (the binding keeps the file).
	preview, err := backend.Plan(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if changesContain(preview.Changes, "Remove supervisor-owned unit "+profile.Unit) {
		t.Fatalf("kept binding previewed as removal: %v", preview.Changes)
	}
	// Dropping the binding previews the removal Apply will perform.
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	preview, err = backend.Plan(home, r)
	if err != nil {
		t.Fatal(err)
	}
	if !changesContain(preview.Changes, "Remove supervisor-owned unit "+profile.Unit) {
		t.Fatalf("dropped adopted binding missing from preview: %v", preview.Changes)
	}
}

// TestResumeIgnoresStaleActivationRecordWithoutFence completes the crash
// chain: a stale activation.json from a previous successful apply must not
// block recovery of a newer interrupted activation once its fence is gone.
func TestResumeIgnoresStaleActivationRecordWithoutFence(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	// Stale record from an earlier request (crash before its removal).
	stale, _ := json.Marshal(activation{Request: ownedFixtureRequest(t, home)})
	if err := os.WriteFile(filepath.Join(root, "activation.json"), stale, 0600); err != nil {
		t.Fatal(err)
	}
	// Newer activation crashed after journal creation, before enterMaintenance:
	// pending journal, no fence, stale record above.
	stray, strayRaw := ownedFixtureProfile(t, home, "stray", 9400)
	if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), stray.Unit), strayRaw, 0600); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{stray.Unit: digest(strayRaw)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	r.ExpectedRevision = currentRevision(t, r)
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
		t.Fatalf("stale activation record blocked recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ownedUnitDirectory(home), stray.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uncommitted write not recovered")
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal not retired")
	}
}

// dropInCommand answers like a healthy manager except the written unit reports
// a drop-in override (or a foreign fragment), shadowing setup's file.
func dropInCommand(home string, dropIn bool) func(context.Context, string, ...string) ([]byte, error) {
	delegate := fakeOwnedCommandFor(home)
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "--property=FragmentPath" {
				unit := args[len(args)-1]
				fragment := filepath.Join(ownedUnitDirectory(home), unit)
				if !dropIn {
					fragment = "/etc/systemd/user/" + unit
				}
				return []byte("FragmentPath=" + fragment + "\nDropInPaths=/home/u/.config/systemd/user/" + unit + ".d/override.conf\n"), nil
			}
		}
		return delegate(ctx, name, args...)
	}
}

// TestApplyRejectsShadowedOwnedUnitBinding fails the commit when the loaded
// unit does not bind setup's file exactly: a drop-in or foreign fragment that
// systemd accepts silently would wedge the next supervisor preflight.
func TestApplyRejectsShadowedOwnedUnitBinding(t *testing.T) {
	for name, dropIn := range map[string]bool{"drop-in override": true, "foreign fragment": false} {
		t.Run(name, func(t *testing.T) {
			backend, home, r := fixture(t)
			backend.runCommand = dropInCommand(home, dropIn)
			profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
			r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
			err := backend.Apply(context.Background(), home, r)
			if !errors.Is(err, gpuruntime.ErrLaunchChanged) {
				t.Fatalf("shadowed binding committed: %v", err)
			}
			if _, lerr := os.Lstat(filepath.Join(ownedUnitDirectory(home), profile.Unit)); !errors.Is(lerr, os.ErrNotExist) {
				t.Fatal("shadowed unit left behind")
			}
			if _, present, _ := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); present {
				t.Fatal("journal left behind")
			}
		})
	}
}

// TestResumeRestoresAcceptedRenderingBeforeReplay covers the full P1 chain:
// crash after the updated unit was written and maintenance entered but before
// commit; the resumed attempt fails pre-commit; the accepted rendering must be
// what rollback restores, not the uncommitted bytes.
func TestResumeRestoresAcceptedRenderingBeforeReplay(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	updated, updatedRaw := ownedFixtureProfile(t, home, "vision", 9101)
	r2 := r
	r2.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{updated}}
	r2.ExpectedRevision = currentRevision(t, r)
	// Crash window: v2 written, maintenance entered, catalog never committed.
	if err := os.WriteFile(filepath.Join(ownedUnitDirectory(home), profile.Unit), updatedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	record, _ := json.Marshal(activation{Request: r2})
	if err := os.WriteFile(filepath.Join(root, "activation.json"), record, 0600); err != nil {
		t.Fatal(err)
	}
	if err := deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{profile.Unit: digest(updatedRaw)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	// Resumed attempt fails at the pre-commit runtime check.
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{err: errors.New("busy")}, nil }
	if err := backend.Apply(ctx, home, r2); err == nil {
		t.Fatal("apply succeeded despite busy runtime")
	}
	data, _ := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != string(raw) {
		t.Fatal("rollback did not restore the accepted rendering")
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal left behind")
	}
	// The same resume succeeds when the runtime frees up.
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	if err := backend.Apply(ctx, home, r2); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != string(updatedRaw) {
		t.Fatal("successful resume did not apply the update")
	}
}

// TestResumeFinishesCommittedDeleteBeforeReAdd covers the re-add wedge: a
// committed delete whose file survived is finished at resume, so a re-add
// attempt that then fails leaves catalog and disk consistent (no orphan).
func TestResumeFinishesCommittedDeleteBeforeReAdd(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Crash after the delete committed but before finalize: committed catalog
	// without the unit, proven delete journaled, file still on disk.
	dropped := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	s := openStoreAt(t, r.Profile.StatePath)
	if _, err := s.ReplaceCatalog(ctx, currentRevision(t, r), dropped); err != nil {
		t.Fatal(err)
	}
	s.Close()
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{profile.Unit: digest(raw)}, Phase: ownedJournalCommitted}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	// Re-add request whose attempt fails before commit: the finished delete
	// plus rollback leave no unjournaled file behind.
	r.ExpectedRevision = currentRevision(t, r)
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{err: errors.New("busy")}, nil }
	if err := backend.Apply(ctx, home, r); err == nil {
		t.Fatal("apply succeeded despite busy runtime")
	}
	if _, err := os.Lstat(filepath.Join(ownedUnitDirectory(home), profile.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed re-add left an unjournaled unit file")
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal left behind")
	}
	// Happy re-add path: the unit is recreated from the request.
	backend.makeRuntime = func(Request) (gpuruntime.Manager, error) { return idleRuntime{}, nil }
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != string(raw) {
		t.Fatal("re-added unit not recreated")
	}
}

// TestAtomicWriteLandedErrorIsRolledBack injects rename-landed-then-failed
// AtomicWrite faults on create and overwrite: the visibly mutated file is
// covered by rollback, not skipped.
func TestAtomicWriteLandedErrorIsRolledBack(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		backend, home, r := fixture(t)
		backend.runCommand = fakeOwnedCommandFor(home)
		profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
		r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
		restore := ownedAtomicWrite
		ownedAtomicWrite = func(path string, data []byte) error {
			// The rename lands, then the parent-dir sync reports failure.
			if err := deployment.AtomicWrite(path, data); err != nil {
				return err
			}
			return errors.New("dir sync fault")
		}
		defer func() { ownedAtomicWrite = restore }()
		if err := backend.Apply(context.Background(), home, r); err == nil {
			t.Fatal("fault swallowed")
		}
		ownedAtomicWrite = restore
		if _, err := os.Lstat(filepath.Join(ownedUnitDirectory(home), profile.Unit)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("landed unit left orphaned after rollback")
		}
		if _, present, _ := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); present {
			t.Fatal("journal left behind after covered rollback")
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		backend, home, r := fixture(t)
		backend.runCommand = fakeOwnedCommandFor(home)
		ctx := context.Background()
		profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
		r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
		if err := backend.Apply(ctx, home, r); err != nil {
			t.Fatal(err)
		}
		updated, updatedRaw := ownedFixtureProfile(t, home, "vision", 9101)
		r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{updated}}
		r.ExpectedRevision = currentRevision(t, r)
		restore := ownedAtomicWrite
		failed := false
		ownedAtomicWrite = func(path string, data []byte) error {
			if !failed && string(data) == string(updatedRaw) {
				failed = true
				if err := deployment.AtomicWrite(path, data); err != nil {
					return err
				}
				return errors.New("dir sync fault")
			}
			return deployment.AtomicWrite(path, data)
		}
		defer func() { ownedAtomicWrite = restore }()
		if err := backend.Apply(ctx, home, r); err == nil {
			t.Fatal("fault swallowed")
		}
		ownedAtomicWrite = restore
		data, _ := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
		if string(data) != string(raw) {
			t.Fatal("accepted rendering not restored after landed overwrite fault")
		}
		if _, present, _ := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); present {
			t.Fatal("journal left behind after covered rollback")
		}
	})
}

// TestUndetectableWriteRetainsJournal injects a write fault whose landing
// cannot be verified: the pending journal must survive for recovery, and the
// next apply recovers cleanly.
func TestUndetectableWriteRetainsJournal(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	restore := ownedAtomicWrite
	ownedAtomicWrite = func(path string, _ []byte) error {
		// Landing state undetectable: world-readable file rejects privateRead.
		if err := os.WriteFile(path, raw, 0644); err != nil {
			return err
		}
		return errors.New("ambiguous write fault")
	}
	if err := backend.Apply(context.Background(), home, r); err == nil {
		t.Fatal("fault swallowed")
	}
	ownedAtomicWrite = restore
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if _, present, _ := readOwnedUnitJournal(root); !present {
		t.Fatal("journal dropped with write outcome undetectable")
	}
	// Recovery: the journaled content is proven, removed, and the retry applies.
	if err := os.Chmod(filepath.Join(ownedUnitDirectory(home), profile.Unit), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.Apply(context.Background(), home, r); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if string(data) != string(raw) {
		t.Fatal("retry did not apply cleanly")
	}
}

// TestRollbackSkipsUntouchedUnitsAndPrunesJournal covers the foreign-file
// collision with an unavailable manager: rollback reloads nothing it never
// wrote, and the retained journal covers only the unit that mutated.
func TestRollbackSkipsUntouchedUnitsAndPrunesJournal(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	code, _ := ownedFixtureProfile(t, home, "code", 9102)
	draft := ownedDraft("vision", 9100)
	draft.Binding.Instance = "second"
	vision, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{code, vision}}
	// Foreign content occupies the second unit; the manager is down.
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, vision.Unit), []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	backend.runCommand = failReloadCommand
	if err := backend.Apply(ctx, home, r); !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("collision not surfaced: %v", err)
	}
	// Rollback removed the written unit; the retained journal covers only it.
	if _, err := os.Lstat(filepath.Join(dir, code.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("written unit not rolled back")
	}
	journal, present, err := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor"))
	if err != nil || !present {
		t.Fatal("journal not retained after rollback reload failure")
	}
	if len(journal.Writes) != 1 || journal.Writes[code.Unit] == "" {
		t.Fatalf("journal not pruned to mutated entries: %+v", journal.Writes)
	}
	// Manager recovers: resume clears the minimal journal without a false
	// proof failure on the foreign file, then the plan reports the collision.
	backend.runCommand = fakeOwnedCommandFor(home)
	if err := backend.Apply(ctx, home, r); !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("retry = %v", err)
	}
	if errors.Is(backend.Apply(ctx, home, r), ErrOwnedUnitModified) {
		t.Fatal("foreign file misreported as modified owned content")
	}
	if _, present, _ := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); present {
		t.Fatal("journal left behind after recovery")
	}
}
