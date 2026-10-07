package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
)

// recordingOwnedCommand answers systemctl like a healthy user manager and
// records the units whose NeedDaemonReload property was verified.
func recordingOwnedCommand(verified *[]string, home string) func(context.Context, string, ...string) ([]byte, error) {
	delegate := fakeOwnedCommandFor(home)
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		for i, arg := range args {
			if arg == "--property=NeedDaemonReload" && i+2 < len(args) {
				*verified = append(*verified, args[i+2])
				break
			}
		}
		return delegate(ctx, name, args...)
	}
}

// TestResumeReplaysDeletesFromCrashWindow reproduces the post-commit crash
// window: journal still writes-pending, fence still set, catalog already
// committed. The proven delete must be replayed, never dropped.
func TestResumeReplaysDeletesFromCrashWindow(t *testing.T) {
	backend, home, r := fixture(t)
	var verified []string
	backend.runCommand = recordingOwnedCommand(&verified, home)
	ctx := context.Background()
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	// The committed catalog no longer carries the stale profile.
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
	if err := deployment.Write(r.Profile.StatePath, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, stale.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("crash-window delete was dropped")
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal not cleared after replay")
	}
	if len(verified) != 1 || verified[0] != stale.Unit {
		t.Fatalf("resume reload did not verify %s: %v", stale.Unit, verified)
	}
}

// TestResumeDefersGenuinePreCommitCrash keeps the journal when the fence is
// set but the catalog was never committed: the activation resume replays the
// original request and recomputes the plan.
func TestResumeDefersGenuinePreCommitCrash(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
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
	if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, stale.Unit)); err != nil {
		t.Fatal("pre-commit unit deleted during deferral")
	}
	if _, present, _ := readOwnedUnitJournal(root); !present {
		t.Fatal("journal cleared while activation resume still needs it")
	}
}

// TestResumeSkipsConflictCheckWhenDeleteAlreadyRan covers the false-positive
// latch: a committed journal entry whose file is already gone needs no
// protection, even if the new request re-adds the unit with a divergent spec.
func TestResumeSkipsConflictCheckWhenDeleteAlreadyRan(t *testing.T) {
	backend, home, _ := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	req := ownedFixtureRequest(t, home, profile)
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, StatePath: req.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{profile.Unit: digest([]byte("older render"))}, Phase: ownedJournalCommitted}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.resumeOwnedUnitJournal(context.Background(), home, req); err != nil {
		t.Fatalf("absent unit latched on a stale conflict: %v", err)
	}
	if _, present, _ := readOwnedUnitJournal(root); present {
		t.Fatal("journal not cleared")
	}
}

// TestPlanOwnedUnitsDedupesSharedUnitDeletes covers the shared Ollama pair:
// both profiles removed, one unit file, one delete entry.
func TestPlanOwnedUnitsDedupesSharedUnitDeletes(t *testing.T) {
	home := t.TempDir()
	backend, _, _ := fixture(t)
	draft := func(id, model string) Draft {
		return Draft{ID: id, Label: "Owned " + id, App: "ollama", Model: model, Binding: &DraftBinding{Instance: "local", Owned: &DraftOwnedLaunch{Port: 11434}}}
	}
	first, _, err := OwnedProfile(draft("chat", "qwen3:latest"), "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	second, raw, err := OwnedProfile(draft("code", "qwen3-coder:latest"), "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, first.Unit), raw, 0600); err != nil {
		t.Fatal(err)
	}
	accepted := control.CatalogSnapshot{Revision: "r1", Catalog: control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{first, second}}}
	req := ownedFixtureRequest(t, home)
	req.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	plan, err := backend.planOwnedUnits(req, accepted, home)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deletes) != 1 || plan.Deletes[0] != first.Unit {
		t.Fatalf("shared unit deleted %v", plan.Deletes)
	}
}

// TestCommitOwnedUnitJournalPinning locks the ordering invariant: the journal
// flips to committed only with a present journal, and finalize refuses an
// unpinned journal.
func TestCommitOwnedUnitJournalPinning(t *testing.T) {
	backend, _, r := fixture(t)
	home := t.TempDir()
	backend.runCommand = fakeOwnedCommandFor(home)
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := commitOwnedUnitJournal(root, unitPlan{}); err != nil {
		t.Fatal(err)
	}
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	plan := unitPlan{Writes: map[string][]byte{profile.Unit: raw}, proven: map[string]string{}, prior: map[string][]byte{}, absent: map[string]bool{}, written: map[string]bool{}}
	if err := commitOwnedUnitJournal(root, plan); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing journal pinned: %v", err)
	}
	journal := newOwnedUnitJournal(plan, r.Profile.StatePath)
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := commitOwnedUnitJournal(root, plan); err != nil {
		t.Fatal(err)
	}
	pinned, present, err := readOwnedUnitJournal(root)
	if err != nil || !present || pinned.Phase != ownedJournalCommitted {
		t.Fatalf("journal not pinned: %+v %v %v", pinned, present, err)
	}
	if err := writeOwnedUnitJournal(root, journal); err != nil {
		t.Fatal(err)
	}
	if err := backend.finalizeOwnedUnits(context.Background(), home, root, r, plan); err == nil {
		t.Fatal("finalize accepted an unpinned journal")
	}
}
