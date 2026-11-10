package setup

import (
	"bytes"
	"encoding/json"
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
	if _, err := SaveDrafts(home, DraftRequest{Version: 1, ExpectedRevision: first.Revision, Drafts: []Draft{limited}}); err == nil {
		t.Fatal("version 1 with limits accepted")
	}
	second, err := SaveDrafts(home, DraftRequest{Version: 2, ExpectedRevision: first.Revision, Drafts: []Draft{limited}})
	if err != nil || second.Version != 2 || second.Drafts[0].Binding.Owned.Limits != owned.Limits {
		t.Fatalf("save with limits = %+v, %v", second, err)
	}
	if _, err := SaveDrafts(home, DraftRequest{Version: 2, ExpectedRevision: second.Revision, Drafts: []Draft{plain}}); err == nil {
		t.Fatal("version 2 without limits accepted")
	}
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
