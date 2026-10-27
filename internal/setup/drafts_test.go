package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDraftsPersistOutsideCatalogAndRejectStaleWrite(t *testing.T) {
	home := t.TempDir()
	first, err := ReadDrafts(home)
	if err != nil || len(first.Drafts) != 0 {
		t.Fatalf("initial: %+v %v", first, err)
	}
	request := DraftRequest{Version: 1, Drafts: []Draft{{ID: "draft-one", Label: "My model", App: "vllm", Model: "model-one"}}}
	saved, err := SaveDrafts(home, request)
	if err != nil || saved.Revision == "" {
		t.Fatalf("save: %+v %v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/gpu-workload-supervisor/catalog.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("draft created catalog")
	}
	if _, err := SaveDrafts(home, request); err == nil {
		t.Fatal("accepted stale draft revision")
	}
	request.ExpectedRevision = saved.Revision
	request.Drafts = nil
	if _, err := SaveDrafts(home, request); err != nil {
		t.Fatal(err)
	}
}

func TestDraftRejectsUnsupportedAppsAndConflictingLocations(t *testing.T) {
	for _, draft := range []Draft{{ID: "d", Label: "D", App: "other"}, {ID: "d", Label: "D", App: "vllm", Endpoint: "http://localhost:1", Reference: "/file", ReferenceKind: "model-file"}} {
		if _, err := SaveDrafts(t.TempDir(), DraftRequest{Version: 1, Drafts: []Draft{draft}}); err == nil {
			t.Fatal("invalid draft accepted")
		}
	}
}

func TestDraftValidationAndCorruptStorage(t *testing.T) {
	valid := Draft{ID: "d", Label: "D", App: "vllm", Reference: "/models/a", ReferenceKind: "model-directory"}
	if err := validateDrafts(1, []Draft{valid}); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Draft){
		func(d *Draft) { d.ID = "" }, func(d *Draft) { d.Label = "" }, func(d *Draft) { d.App = "bad" },
		func(d *Draft) { d.Reference = "relative" }, func(d *Draft) { d.ReferenceKind = "bad" },
		func(d *Draft) { d.Reference = "" }, func(d *Draft) { d.Endpoint = strings.Repeat("x", 2049); d.Reference = ""; d.ReferenceKind = "" },
		func(d *Draft) { d.App = "comfyui"; d.Model = "model" },
	}
	for _, mutate := range mutations {
		d := valid
		mutate(&d)
		if err := validateDrafts(1, []Draft{d}); err == nil {
			t.Fatalf("accepted %+v", d)
		}
	}
	if err := validateDrafts(1, []Draft{valid, valid}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := validateDrafts(2, nil); err == nil {
		t.Fatal("version accepted")
	}
	home := t.TempDir()
	root := filepath.Join(home, ".config/gpu-workload-supervisor")
	if err := mkdirTrusted(root); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"invalid", `{"version":2}`} {
		if err := os.WriteFile(filepath.Join(root, "drafts.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadDrafts(home); err == nil {
			t.Fatal("corrupt accepted")
		}
		if _, err := SaveDrafts(home, DraftRequest{Version: 1}); err == nil {
			t.Fatal("corrupt overwritten")
		}
	}
}

func TestDraftWithRemovedApplicationIsReplaceable(t *testing.T) {
	for _, app := range []string{"ollama", "llama.cpp"} {
		t.Run(app, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".config/gpu-workload-supervisor")
			if err := mkdirTrusted(root); err != nil {
				t.Fatal(err)
			}
			stored := []byte(`{"version":1,"drafts":[{"id":"old-chat","label":"Old chat","app":"` + app + `"}]}`)
			if err := os.WriteFile(filepath.Join(root, "drafts.json"), stored, 0600); err != nil {
				t.Fatal(err)
			}
			current, err := ReadDrafts(home)
			if err == nil {
				t.Fatal("draft with removed application accepted")
			}
			for _, want := range []string{"draft old-chat", `application "` + app + `"`, draftsDisplayPath, "delete the file"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
			if current.Revision != digest(stored) {
				t.Fatal("unusable drafts lost their raw revision")
			}
			if _, err := SaveDrafts(home, DraftRequest{Version: 1}); err == nil {
				t.Fatal("unusable drafts replaced without their revision")
			}
			replacement := Draft{ID: "chat", Label: "Chat", App: "vllm"}
			saved, err := SaveDrafts(home, DraftRequest{Version: 1, ExpectedRevision: current.Revision, Drafts: []Draft{replacement}})
			if err != nil || len(saved.Drafts) != 1 || saved.Drafts[0].ID != "chat" {
				t.Fatalf("unusable drafts were not replaced: %+v %v", saved, err)
			}
		})
	}
}

func TestDraftBindingsRemainUntrustedAndRoundTrip(t *testing.T) {
	binding := &DraftBinding{Unit: "existing.service", Cgroup: "/scope", HealthURL: "http://127.0.0.1:1234", Instance: "runtime", Model: "chosen", LaunchFile: "/trusted/model.service"}
	draft := Draft{ID: "d", Label: "Draft", App: "vllm", Binding: binding}
	result, err := SaveDrafts(t.TempDir(), DraftRequest{Version: 1, Drafts: []Draft{draft}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Drafts[0].Binding == nil || *result.Drafts[0].Binding != *binding {
		t.Fatalf("lost binding: %+v", result)
	}
	draft.Binding.LaunchFile = strings.Repeat("x", 4097)
	if err := validateDrafts(1, []Draft{draft}); err == nil {
		t.Fatal("oversized binding accepted")
	}
}

// TestDraftOwnedRejectsGrammarUnsafeValues fails at save time for model paths
// or aliases that the unit command grammar cannot express, matching catalog
// validation.
func TestDraftOwnedRejectsGrammarUnsafeValues(t *testing.T) {
	owned := func(modelPath, alias string) Draft {
		return Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Instance: "owned", Owned: &DraftOwnedLaunch{ModelPath: modelPath, Port: 9100, Alias: alias}}}
	}
	for name, d := range map[string]Draft{
		"space in model path":     owned("/models/vision v2", ""),
		"quote in model path":     owned(`/models/vis"ion`, ""),
		"percent in model path":   owned("/models/vision%i", ""),
		"dollar in model path":    owned("/models/$vision", ""),
		"backslash in model path": owned(`/models/vis\ion`, ""),
		"semicolon model path":    owned("/models/a;b", ""),
		"space in alias":          owned("/models/vision", "vision v2"),
		"backtick in alias":       owned("/models/vision", "vis`ion"),
		"semicolon alias":         owned("/models/vision", "a;b"),
	} {
		if err := validateDrafts(1, []Draft{d}); err == nil {
			t.Fatalf("%s saved", name)
		}
	}
	if err := validateDrafts(1, []Draft{owned("/models/vision-v2.Q4_K_M", "vision-v2")}); err != nil {
		t.Fatal(err)
	}
}

// TestDraftOwnedRejectsMissingInstance requires a valid instance while allowing
// model selection to remain deferred outside the executable catalog.
func TestDraftOwnedRejectsMissingInstance(t *testing.T) {
	for name, d := range map[string]Draft{
		"missing instance":      {ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Owned: &DraftOwnedLaunch{ModelPath: "/models/vision", Port: 9100}}},
		"vllm without instance": {ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Owned: &DraftOwnedLaunch{ModelPath: "/models/vision", Port: 9100}}},
	} {
		if err := validateDrafts(1, []Draft{d}); err == nil {
			t.Fatalf("%s saved", name)
		}
	}
	incomplete := Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Instance: "owned", Owned: &DraftOwnedLaunch{Port: 9100}}}
	if err := validateDrafts(1, []Draft{incomplete}); err != nil {
		t.Fatalf("deferred model draft rejected: %v", err)
	}
	if _, _, err := OwnedProfile(incomplete, "/user.slice/user-1000.slice/user@1000.service", t.TempDir()); err == nil {
		t.Fatal("deferred model became ready")
	}
	valid := Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Instance: "owned", Owned: &DraftOwnedLaunch{ModelPath: "/models/vision", Port: 9100}}}
	if err := validateDrafts(1, []Draft{valid}); err != nil {
		t.Fatal(err)
	}
}

// TestDraftOwnedRejectsInvalidInstanceSyntax applies the catalog's workload-ID
// grammar to owned instances at save time.
func TestDraftOwnedRejectsInvalidInstanceSyntax(t *testing.T) {
	for _, instance := range []string{"Local", "bad!name", "white space", ""} {
		d := Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Instance: instance, Owned: &DraftOwnedLaunch{ModelPath: "/models/vision", Port: 9100}}}
		if err := validateDrafts(1, []Draft{d}); err == nil {
			t.Fatalf("instance %q saved", instance)
		}
	}
	d := Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Instance: "rig-2", Owned: &DraftOwnedLaunch{ModelPath: "/models/vision", Port: 9100}}}
	if err := validateDrafts(1, []Draft{d}); err != nil {
		t.Fatal(err)
	}
}

func TestDraftComfyUIRejectsModelChoice(t *testing.T) {
	if err := validateDrafts(1, []Draft{{ID: "images", Label: "Images", App: "comfyui", Model: "first"}}); err == nil {
		t.Fatal("ComfyUI accepted a model choice")
	}
}

func TestUnsupportedDraftDoesNotHideOtherInvalidSelections(t *testing.T) {
	for _, drafts := range [][]Draft{
		{{ID: "old", Label: "Old", App: "ollama"}, {ID: "bad", Label: "Bad", App: "comfyui", Model: "invalid"}},
		{{ID: "old", Label: "Old", App: "ollama"}, {ID: "old", Label: "Duplicate", App: "llama.cpp"}},
	} {
		if err := validateDrafts(1, drafts); err == nil || errors.Is(err, ErrUnsupportedDraftApplication) {
			t.Fatalf("invalid selection treated as removable unsupported application: %v", err)
		}
	}
}
