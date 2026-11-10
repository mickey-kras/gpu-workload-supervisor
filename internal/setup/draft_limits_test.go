package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/strictjson"
)

// Owned draft limits are optional: a draft without them keeps its exact
// bytes, and a draft with them survives the strict decoder.
func TestDraftOwnedLimitsRoundTrip(t *testing.T) {
	plain := Draft{ID: "vision", App: "vllm", Binding: &DraftBinding{Instance: "owned", Owned: &DraftOwnedLaunch{ModelPath: "/m", Port: 9100}}}
	data, err := json.Marshal(plain)
	if err != nil || strings.Contains(string(data), "limits") {
		t.Fatalf("draft without limits = %s, %v", data, err)
	}
	limited := plain
	owned := *plain.Binding.Owned
	owned.Limits = control.ResourceLimits{Version: 1, MemoryHighMiB: 8192, MemoryMaxMiB: 10240, TasksMax: 4096, CPUQuotaPercent: 400}
	limited.Binding = &DraftBinding{Instance: "owned", Owned: &owned}
	if data, err = json.Marshal(limited); err != nil {
		t.Fatal(err)
	}
	var decoded Draft
	if err := strictjson.DecodeLimited(bytes.NewReader(data), 65536, &decoded); err != nil || decoded.Binding.Owned.Limits != owned.Limits {
		t.Fatalf("decoded draft = %+v, %v", decoded.Binding.Owned, err)
	}
	empty := strings.Replace(string(data), `"limits":{"version":1,"memoryHighMiB":8192,"memoryMaxMiB":10240,"tasksMax":4096,"cpuQuotaPercent":400}`, `"limits":{}`, 1)
	if err := strictjson.DecodeLimited(strings.NewReader(empty), 65536, &decoded); err == nil {
		t.Fatal("an empty limits object was accepted")
	}
}

// A drafts file with limits is version 2, and a file without limits stays
// version 1 with the bytes of a save without them. A version that does not
// fit the drafts is refused.
func TestDraftsVersionFollowsLimits(t *testing.T) {
	home := t.TempDir()
	plain := Draft{ID: "vision", Label: "Vision", App: "vllm", Binding: &DraftBinding{Instance: "owned", Owned: &DraftOwnedLaunch{ModelPath: "/m", Port: 9100}}}
	first, err := SaveDrafts(home, DraftRequest{Version: 1, Drafts: []Draft{plain}})
	if err != nil || first.Version != 1 {
		t.Fatalf("save without limits = %+v, %v", first, err)
	}
	path := filepath.Join(home, ".config/gpu-workload-supervisor/drafts.json")
	plainBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	limited := plain
	owned := *plain.Binding.Owned
	owned.Limits = control.ResourceLimits{Version: 1, MemoryHighMiB: 8192, MemoryMaxMiB: 10240, TasksMax: 4096, CPUQuotaPercent: 400}
	limited.Binding = &DraftBinding{Instance: "owned", Owned: &owned}
	unchanged := func(want []byte) {
		t.Helper()
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("a refused save changed the file: %s, %v", got, err)
		}
	}
	if _, err := SaveDrafts(home, DraftRequest{Version: 1, ExpectedRevision: first.Revision, Drafts: []Draft{limited}}); err == nil {
		t.Fatal("version 1 with limits accepted")
	}
	unchanged(plainBytes)
	second, err := SaveDrafts(home, DraftRequest{Version: 2, ExpectedRevision: first.Revision, Drafts: []Draft{limited}})
	if err != nil || second.Version != 2 || second.Drafts[0].Binding.Owned.Limits != owned.Limits {
		t.Fatalf("save with limits = %+v, %v", second, err)
	}
	limitedBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SaveDrafts(home, DraftRequest{Version: 2, ExpectedRevision: second.Revision, Drafts: []Draft{plain}}); err == nil {
		t.Fatal("version 2 without limits accepted")
	}
	unchanged(limitedBytes)
	third, err := SaveDrafts(home, DraftRequest{Version: 1, ExpectedRevision: second.Revision, Drafts: []Draft{plain}})
	if err != nil || third.Version != 1 {
		t.Fatalf("save after removing limits = %+v, %v", third, err)
	}
	if back, err := os.ReadFile(path); err != nil || !bytes.Equal(back, plainBytes) {
		t.Fatalf("bytes after removing limits differ: %s", back)
	}
	bad := limited
	badOwned := owned
	badOwned.Limits.MemoryHighMiB = badOwned.Limits.MemoryMaxMiB
	bad.Binding = &DraftBinding{Instance: "owned", Owned: &badOwned}
	if err := validateDrafts(2, []Draft{bad}); err == nil || !strings.Contains(err.Error(), "draft resource limits") {
		t.Fatalf("invalid limits = %v", err)
	}
}

// ReadDrafts applies the version rule to the file: a version that does not
// fit its drafts, or an empty or unknown limits form, makes it unusable.
func TestReadDraftsChecksTheLimitsVersion(t *testing.T) {
	plain := `{"id":"plain","label":"Plain","app":"vllm","binding":{"instance":"owned","owned":{"modelPath":"/m","port":9100}}}`
	limited := `{"id":"limited","label":"Limited","app":"vllm","binding":{"instance":"owned","owned":{"modelPath":"/m","port":9101,` +
		`"limits":{"version":1,"memoryHighMiB":8192,"memoryMaxMiB":10240,"tasksMax":4096,"cpuQuotaPercent":400}}}}`
	for name, tc := range map[string]struct {
		body string
		ok   bool
	}{
		"v1 plain":               {`{"version":1,"drafts":[` + plain + `]}`, true},
		"v2 mixed":               {`{"version":2,"drafts":[` + plain + `,` + limited + `]}`, true},
		"v1 with limits":         {`{"version":1,"drafts":[` + limited + `]}`, false},
		"v2 without limits":      {`{"version":2,"drafts":[` + plain + `]}`, false},
		"v3":                     {`{"version":3,"drafts":[` + limited + `]}`, false},
		"empty limits":           {`{"version":1,"drafts":[` + strings.Replace(plain, `"port":9100`, `"port":9100,"limits":{}`, 1) + `]}`, false},
		"unknown limits control": {`{"version":2,"drafts":[` + strings.Replace(limited, `"version":1,`, `"version":1,"memoryLowMiB":1,`, 1) + `]}`, false},
	} {
		home := t.TempDir()
		root := filepath.Join(home, ".config/gpu-workload-supervisor")
		if err := mkdirTrusted(root); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "drafts.json"), []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadDrafts(home); (err == nil) != tc.ok {
			t.Errorf("%s: read = %v", name, err)
		}
	}
	// The remedy depends on the failure: setup still opens with an
	// unsupported application, and stops on other failures.
	for body, remedy := range map[string]string{
		`{"version":1,"drafts":[` + limited + `]}`: "Move the file aside or delete it",
		`{"version":1,"drafts":[` + strings.Replace(plain, `"app":"vllm","binding":{"instance":"owned","owned":{"modelPath":"/m","port":9100}}`, `"app":"ollama"`, 1) + `]}`: "remove the draft in setup and save",
	} {
		home := t.TempDir()
		root := filepath.Join(home, ".config/gpu-workload-supervisor")
		if err := mkdirTrusted(root); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "drafts.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ReadDrafts(home)
		other := "remove the draft in setup"
		if remedy == other+" and save" {
			other = "setup cannot use this file"
		}
		if !errors.Is(err, errUnusableDrafts) || !strings.Contains(err.Error(), remedy) || strings.Contains(err.Error(), other) {
			t.Errorf("read of %s = %v; want %q and not %q", body, err, remedy, other)
		}
	}
	mixed := []Draft{{ID: "plain", Label: "Plain", App: "vllm"}, {ID: "limited", Label: "Limited", App: "vllm", Binding: &DraftBinding{Instance: "owned",
		Owned: &DraftOwnedLaunch{ModelPath: "/m", Port: 9101, Limits: control.ResourceLimits{Version: 1, MemoryHighMiB: 8192, MemoryMaxMiB: 10240, TasksMax: 4096, CPUQuotaPercent: 400}}}}}
	if err := validateDrafts(1, mixed); err == nil {
		t.Fatal("version 1 with a limited draft among others accepted")
	}
	if err := validateDrafts(2, mixed); err != nil {
		t.Fatalf("version 2 mixed = %v", err)
	}
}
