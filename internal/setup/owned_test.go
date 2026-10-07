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
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

func ownedDraft(id string, port uint16) Draft {
	return Draft{
		ID:    id,
		Label: "Owned " + id,
		App:   appLlamaCPP,
		Binding: &DraftBinding{
			Instance: "owned",
			Owned:    &DraftOwnedLaunch{ModelPath: "/models/" + id + ".gguf", Port: port, CtxSize: 8192},
		},
	}
}

func ownedFixtureProfile(t *testing.T, home, id string, port uint16) (control.WorkloadProfile, []byte) {
	t.Helper()
	profile, raw, err := OwnedProfile(ownedDraft(id, port), "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	return profile, raw
}

func ownedFixtureRequest(t *testing.T, home string, profiles ...control.WorkloadProfile) Request {
	t.Helper()
	_, _, r := fixture(t)
	r.Profile.StatePath = filepath.Join(home, "state/state.db")
	r.Catalog = control.Catalog{Version: 2, Profiles: profiles}
	return r
}

func TestOwnedProfileSynthesis(t *testing.T) {
	home := t.TempDir()
	profile, raw, err := OwnedProfile(ownedDraft("vision", 9100), "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Unit != "gws-owned-vision.service" {
		t.Fatalf("unit %q", profile.Unit)
	}
	if profile.Cgroup != "/user.slice/user-1000.slice/user@1000.service/app.slice/gws-owned-vision.service" {
		t.Fatalf("cgroup %q", profile.Cgroup)
	}
	if profile.NativeModel.LaunchFile != filepath.Join(home, ".config/systemd/user", profile.Unit) {
		t.Fatalf("launch file %q", profile.NativeModel.LaunchFile)
	}
	if profile.NativeModel.LaunchSHA256 != digest(raw) || profile.NativeModel.Endpoint != "http://127.0.0.1:9100" {
		t.Fatal("fingerprint or endpoint wrong")
	}
	if profile.NativeModel.Model != "/models/vision.gguf" {
		t.Fatalf("model identity %q", profile.NativeModel.Model)
	}
	ollama := ownedDraft("chat", 11434)
	ollama.App = "ollama"
	ollama.Model = "qwen3:latest"
	ollama.Binding.Owned = &DraftOwnedLaunch{Port: 11434}
	profile, _, err = OwnedProfile(ollama, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Unit != "gws-owned-ollama-owned.service" || profile.NativeModel.Model != "qwen3:latest" || profile.HealthURL != "http://127.0.0.1:11434/api/tags" {
		t.Fatalf("ollama profile %+v", profile)
	}
	bad := ownedDraft("vision", 80)
	if _, _, err := OwnedProfile(bad, "/user.slice", home); err == nil {
		t.Fatal("privileged port accepted")
	}
	noOwned := ownedDraft("vision", 9100)
	noOwned.Binding.Owned = nil
	if _, _, err := OwnedProfile(noOwned, "/user.slice", home); err == nil {
		t.Fatal("missing owned spec accepted")
	}
	comfy := ownedDraft("vision", 9100)
	comfy.App = "comfyui"
	if _, _, err := OwnedProfile(comfy, "/user.slice", home); err == nil {
		t.Fatal("comfyui owned accepted")
	}
}

func TestPlanOwnedUnitsDiffs(t *testing.T) {
	home := t.TempDir()
	backend, _, _ := fixture(t)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	accepted := control.CatalogSnapshot{Revision: "r1", Catalog: control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}}

	t.Run("add", func(t *testing.T) {
		req := ownedFixtureRequest(t, home, profile)
		plan, err := backend.planOwnedUnits(req, control.CatalogSnapshot{}, home)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Writes) != 1 || len(plan.Deletes) != 0 || string(plan.Writes[profile.Unit]) != string(raw) {
			t.Fatalf("plan %+v", plan)
		}
	})
	t.Run("no-op keeps idempotent write", func(t *testing.T) {
		req := ownedFixtureRequest(t, home, profile)
		plan, err := backend.planOwnedUnits(req, accepted, home)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Writes) != 1 || len(plan.Deletes) != 0 {
			t.Fatalf("plan %+v", plan)
		}
	})
	t.Run("remove plans proven delete", func(t *testing.T) {
		dir := ownedUnitDirectory(home)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, profile.Unit), raw, 0600); err != nil {
			t.Fatal(err)
		}
		req := ownedFixtureRequest(t, home)
		req.Catalog.Profiles = nil
		req.Catalog.Version = 1
		req.Catalog.Profiles = []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}
		plan, err := backend.planOwnedUnits(req, accepted, home)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Deletes) != 1 || plan.Deletes[0] != profile.Unit || len(plan.Writes) != 0 {
			t.Fatalf("plan %+v", plan)
		}
		if err := os.WriteFile(filepath.Join(dir, profile.Unit), []byte("tampered"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.planOwnedUnits(req, accepted, home); !errors.Is(err, ErrOwnedUnitModified) {
			t.Fatalf("plan-time proof mismatch = %v", err)
		}
	})
	t.Run("absent file delete is a no-op", func(t *testing.T) {
		req := ownedFixtureRequest(t, home)
		req.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
		other, _ := ownedFixtureProfile(t, home, "code", 9200)
		snapshot := control.CatalogSnapshot{Revision: "r2", Catalog: control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{other}}}
		plan, err := backend.planOwnedUnits(req, snapshot, home)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Deletes) != 0 {
			t.Fatalf("absent file planned for delete: %+v", plan)
		}
	})
}

func TestOwnedUnitWriteCollisionAndOverwrite(t *testing.T) {
	home := t.TempDir()
	backend, _, _ := fixture(t)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	req := ownedFixtureRequest(t, home, profile)
	plan, err := backend.planOwnedUnits(req, control.CatalogSnapshot{}, home)
	if err != nil {
		t.Fatal(err)
	}
	journal := newOwnedUnitJournal(plan, req.Profile.StatePath)
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, journal); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ownedUnitDirectory(home), profile.Unit)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(raw) {
		t.Fatalf("unit not written: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	// Replay is idempotent.
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, journal); err != nil {
		t.Fatal(err)
	}
	// Foreign content at the path is a hard collision.
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan, journal); !errors.Is(err, ErrOwnedUnitCollision) {
		t.Fatalf("foreign content overwritten: %v", err)
	}
	// Content proven from the accepted catalog may be replaced.
	accepted := control.CatalogSnapshot{Revision: "r1", Catalog: control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}}
	changed, changedRaw := ownedFixtureProfile(t, home, "vision", 9200)
	req2 := ownedFixtureRequest(t, home, changed)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	plan2, err := backend.planOwnedUnits(req2, accepted, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan2, newOwnedUnitJournal(plan2, req2.Profile.StatePath)); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != string(changedRaw) {
		t.Fatal("proven overwrite did not apply")
	}
	// A journal that disagrees with the plan refuses to write.
	if err := backend.applyOwnedUnitWrites(context.Background(), home, plan2, journal); err == nil {
		t.Fatal("stale journal accepted")
	}
}

func TestOwnedUnitDeleteContentProof(t *testing.T) {
	home := t.TempDir()
	backend, _, _ := fixture(t)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, profile.Unit)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	journal := unitJournal{Version: 1, Writes: map[string]string{}, Deletes: map[string]string{profile.Unit: digest(raw)}, Phase: ownedJournalCommitted}
	if err := backend.applyOwnedUnitDeletes(context.Background(), home, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("proven unit not deleted")
	}
	// Replay after a crash mid-delete: absence is done.
	if err := backend.applyOwnedUnitDeletes(context.Background(), home, journal); err != nil {
		t.Fatal(err)
	}
	// Divergent content is retained and reported.
	if err := os.WriteFile(path, []byte("user edited"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := backend.applyOwnedUnitDeletes(context.Background(), home, journal); !errors.Is(err, ErrOwnedUnitModified) {
		t.Fatalf("modified unit deleted: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("modified unit not retained")
	}
}

func fakeOwnedCommandFor(home string) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		fragment := false
		reload := false
		unit := ""
		for i, arg := range args {
			switch arg {
			case "--property=FragmentPath":
				fragment = true
			case "--property=NeedDaemonReload":
				reload = true
			case "--":
				if i+1 < len(args) {
					unit = args[i+1]
				}
			}
		}
		if fragment {
			return []byte("FragmentPath=" + filepath.Join(ownedUnitDirectory(home), unit) + "\nDropInPaths=\n"), nil
		}
		if reload {
			return []byte("NeedDaemonReload=no\n"), nil
		}
		return []byte("ok\n"), nil
	}
}

func TestApplyOwnedLifecycle(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	ctx := context.Background()
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), profile.Unit))
	if err != nil || string(data) != string(raw) {
		t.Fatalf("owned unit not applied: %v", err)
	}
	if _, present, err := readOwnedUnitJournal(filepath.Join(home, ".config/gpu-workload-supervisor")); err != nil || present {
		t.Fatalf("journal not cleared: %v %v", present, err)
	}
	// Re-apply unchanged: idempotent.
	s := openStoreAt(t, r.Profile.StatePath)
	snap, _ := s.Catalog(ctx)
	s.Close()
	r.ExpectedRevision = snap.Revision
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	// Remove the owned profile: post-commit delete.
	r.Catalog = control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{{ID: "text", Label: "Text", Adapter: "systemd", Unit: "text.service", Cgroup: "/user.slice/text", HealthURL: "http://127.0.0.1:8000/health"}}}
	s = openStoreAt(t, r.Profile.StatePath)
	snap, _ = s.Catalog(ctx)
	s.Close()
	r.ExpectedRevision = snap.Revision
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ownedUnitDirectory(home), profile.Unit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned unit not removed post-commit")
	}
	s = openStoreAt(t, r.Profile.StatePath)
	snap, _ = s.Catalog(ctx)
	s.Close()
	if snap.Catalog.Version != 1 {
		t.Fatal("clean downgrade to v1 rejected")
	}
}

func TestResumeOwnedUnitJournalAtApplyEntry(t *testing.T) {
	backend, home, r := fixture(t)
	reloads := 0
	backend.runCommand = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		for _, arg := range args {
			if arg == "daemon-reload" {
				reloads++
			}
			if arg == "--property=NeedDaemonReload" {
				return []byte("NeedDaemonReload=no\n"), nil
			}
		}
		return []byte("ok\n"), nil
	}
	ctx := context.Background()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	stale, staleRaw := ownedFixtureProfile(t, home, "stale", 9300)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".config/gpu-workload-supervisor")

	t.Run("committed journal deletes replayed", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, stale.Unit), staleRaw, 0600); err != nil {
			t.Fatal(err)
		}
		journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{stale.Unit: digest(staleRaw)}, Phase: ownedJournalCommitted}
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeOwnedUnitJournal(root, journal); err != nil {
			t.Fatal(err)
		}
		if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(dir, stale.Unit)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("stale delete not replayed")
		}
		if _, present, _ := readOwnedUnitJournal(root); present {
			t.Fatal("journal not cleared")
		}
		if reloads == 0 {
			t.Fatal("no daemon-reload after replayed delete")
		}
	})

	t.Run("re-added unit still finishes the committed delete", func(t *testing.T) {
		// The accepted catalog already dropped the unit, so the file is
		// removed even though the request re-adds it identically; the resumed
		// plan recreates it. Preserving it would leave an unjournaled orphan
		// if the resumed attempt then failed.
		if err := os.WriteFile(filepath.Join(dir, profile.Unit), raw, 0600); err != nil {
			t.Fatal(err)
		}
		journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{profile.Unit: digest(raw)}, Phase: ownedJournalCommitted}
		if err := writeOwnedUnitJournal(root, journal); err != nil {
			t.Fatal(err)
		}
		req := ownedFixtureRequest(t, home, profile)
		if err := backend.resumeOwnedUnitJournal(ctx, home, req); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(dir, profile.Unit)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("committed delete not finished")
		}
		if _, present, _ := readOwnedUnitJournal(root); present {
			t.Fatal("journal not retired after finishing the delete")
		}
	})

	t.Run("divergent digest rewrite refuses", func(t *testing.T) {
		// Content the journal never proved is fail-closed: the delete is
		// refused and the journal survives for inspection.
		if err := os.WriteFile(filepath.Join(dir, profile.Unit), raw, 0600); err != nil {
			t.Fatal(err)
		}
		journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{profile.Unit: digest([]byte("older render"))}, Phase: ownedJournalCommitted}
		if err := writeOwnedUnitJournal(root, journal); err != nil {
			t.Fatal(err)
		}
		req := ownedFixtureRequest(t, home, profile)
		if err := backend.resumeOwnedUnitJournal(ctx, home, req); !errors.Is(err, ErrOwnedUnitModified) {
			t.Fatalf("divergent rewrite = %v", err)
		}
		if _, present, _ := readOwnedUnitJournal(root); !present {
			t.Fatal("journal consumed despite divergent content")
		}
		if err := clearOwnedUnitJournal(root); err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(dir, profile.Unit))
	})

	t.Run("writes-pending journal without fence is cleared", func(t *testing.T) {
		journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{stale.Unit: digest(staleRaw)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
		if err := writeOwnedUnitJournal(root, journal); err != nil {
			t.Fatal(err)
		}
		if err := backend.resumeOwnedUnitJournal(ctx, home, r); err != nil {
			t.Fatal(err)
		}
		if _, present, _ := readOwnedUnitJournal(root); present {
			t.Fatal("pending journal not cleared")
		}
	})

	t.Run("maintenance fence recovers pending writes before activation resume", func(t *testing.T) {
		journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{stale.Unit: digest(staleRaw)}, Deletes: map[string]string{}, Phase: ownedJournalPending}
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
		if _, present, _ := readOwnedUnitJournal(root); present {
			t.Fatal("pending journal not recovered under maintenance fence")
		}
		os.Remove(r.Profile.StatePath + deployment.Suffix)
	})

	t.Run("tampered journaled file retained", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, stale.Unit), []byte("edited"), 0600); err != nil {
			t.Fatal(err)
		}
		journal := unitJournal{Version: 1, StatePath: r.Profile.StatePath, Writes: map[string]string{}, Deletes: map[string]string{stale.Unit: digest(staleRaw)}, Phase: ownedJournalCommitted}
		if err := writeOwnedUnitJournal(root, journal); err != nil {
			t.Fatal(err)
		}
		if err := backend.resumeOwnedUnitJournal(ctx, home, r); !errors.Is(err, ErrOwnedUnitModified) {
			t.Fatalf("tampered file = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(dir, stale.Unit)); err != nil {
			t.Fatal("tampered file not retained")
		}
	})
}

func TestApplyCarriedSharedPairVerbatim(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
	ctx := context.Background()
	native := func(model string) *control.NativeModel {
		return &control.NativeModel{Runtime: "ollama", Instance: "local", Model: model, Endpoint: "http://127.0.0.1:11434", LaunchFile: "/etc/systemd/user/ollama.service", LaunchSHA256: strings.Repeat("0", 64)}
	}
	shared := func(id, model string) control.WorkloadProfile {
		return control.WorkloadProfile{ID: control.Workload(id), Label: id, Adapter: "systemd", Unit: "ollama.service", Cgroup: "/user.slice/ollama.service", HealthURL: "http://127.0.0.1:11434/health", NativeModel: native(model)}
	}
	// Setup applies a plain catalog first; the adopted shared pair is then
	// committed through the configure path (store-level equivalent).
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatal(err)
	}
	s := openStoreAt(t, r.Profile.StatePath)
	snap, _ := s.Catalog(ctx)
	pair := control.Catalog{Version: 1, Profiles: []control.WorkloadProfile{r.Catalog.Profiles[0], shared("alpha", "a"), shared("beta", "b")}}
	if _, err := s.ReplaceCatalog(ctx, snap.Revision, pair); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Catalog(ctx)
	s.Close()
	// A verbatim carried pair rides along with a new owned profile.
	profile, _ := ownedFixtureProfile(t, home, "vision", 9100)
	r.ExpectedRevision = snap.Revision
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{shared("alpha", "a"), shared("beta", "b"), profile}}
	if err := backend.Apply(ctx, home, r); err != nil {
		t.Fatalf("verbatim carried pair rejected: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ownedUnitDirectory(home), profile.Unit)); err != nil {
		t.Fatal("owned unit not written")
	}
	// A changed adopted shared pair is still configure-only.
	s = openStoreAt(t, r.Profile.StatePath)
	snap, _ = s.Catalog(ctx)
	r.ExpectedRevision = snap.Revision
	s.Close()
	changed := shared("beta", "b")
	changed.Label = "Renamed"
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{shared("alpha", "a"), changed, profile}}
	if err := backend.Apply(ctx, home, r); err == nil || !strings.Contains(err.Error(), "catalog-only") {
		t.Fatalf("changed adopted shared pair accepted: %v", err)
	}
}

func TestApplyOwnedSharedPairRenderedBySetup(t *testing.T) {
	backend, home, r := fixture(t)
	backend.runCommand = fakeOwnedCommandFor(home)
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
	if first.Unit != second.Unit {
		t.Fatal("owned shared pair derived different units")
	}
	r.Catalog = control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{first, second}}
	if err := backend.Apply(context.Background(), home, r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ownedUnitDirectory(home), first.Unit))
	if err != nil || string(data) != string(raw) {
		t.Fatalf("shared owned unit not rendered: %v", err)
	}
}

func TestDraftOwnedValidation(t *testing.T) {
	valid := ownedDraft("vision", 9100)
	if err := validateDrafts(1, []Draft{valid}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Draft){
		"privileged port": func(d *Draft) { d.Binding.Owned.Port = 80 },
		"llama max len":   func(d *Draft) { d.Binding.Owned.MaxModelLen = 1 },
		"llama no path":   func(d *Draft) { d.Binding.Owned.ModelPath = "" },
		"llama rel path":  func(d *Draft) { d.Binding.Owned.ModelPath = "rel.gguf" },
		"ollama alias":    func(d *Draft) { d.App = "ollama"; d.Binding.Owned.Alias = "x" },
		"ollama ctx":      func(d *Draft) { d.App = "ollama"; d.Binding.Owned.CtxSize = 1 },
		"vllm gpu layers": func(d *Draft) { d.App = "vllm"; d.Binding.Owned.GPULayers = 1 },
		"comfyui owned":   func(d *Draft) { d.App = "comfyui" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := ownedDraft("vision", 9100)
			mutate(&d)
			if err := validateDrafts(1, []Draft{d}); err == nil {
				t.Fatal("invalid owned draft accepted")
			}
		})
	}
}

func TestDiscoverOwnedUnitsStates(t *testing.T) {
	home := t.TempDir()
	profile, raw := ownedFixtureProfile(t, home, "vision", 9100)
	dir := ownedUnitDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	catalog := control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{profile}}
	got, err := discoverOwnedUnits(home, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty scan %+v", got)
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(profile.Unit, raw)
	write("gwsowned-not-mine.service", []byte("x"))
	write("gws-owned-stale.service", []byte("y"))
	write("other.service", []byte("z"))
	got, err = discoverOwnedUnits(home, catalog)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, u := range got {
		states[u.Name] = u.State
	}
	if len(got) != 2 || states[profile.Unit] != "managed" || states["gws-owned-stale.service"] != "orphaned" {
		t.Fatalf("scan %+v", got)
	}
	write(profile.Unit, []byte("edited"))
	got, err = discoverOwnedUnits(home, catalog)
	if err != nil {
		t.Fatal(err)
	}
	states = map[string]string{}
	for _, u := range got {
		states[u.Name] = u.State
	}
	if states[profile.Unit] != "modified" {
		t.Fatalf("modified scan %+v", got)
	}
}

func openStoreAt(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
