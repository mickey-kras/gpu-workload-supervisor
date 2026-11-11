package modelcheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statusRecord checks a model on disk and gives its record as this
// checker version with the default hub would store it.
func statusRecord(t *testing.T) (Record, string) {
	t.Helper()
	files := modelFiles()
	root := writeModel(t, files)
	record := run(t, newHub(t, upstreamOf(files)).checker(), root)
	record.Checker, record.Configuration = CheckerVersion, currentConfiguration(DefaultHub)
	return record, root
}

// The stored status needs the same model, root, checker version and
// configuration. It reads no files.
func TestStoredStatusUsesTheStoredDigests(t *testing.T) {
	record, root := statusRecord(t)
	pin := record.ChatTemplateSHA256
	if pin == "" {
		t.Fatal("the clean record pins no template")
	}
	for name, tc := range map[string]struct {
		edit             func(*Record)
		model, root, pin string
		status           Status
		reason           string
	}{
		"same":              {nil, "example", root, pin, StatusNotScanned, ""},
		"no root":           {nil, "example", "", pin, StatusNotScanned, ""},
		"other model":       {nil, "other", root, pin, StatusNotScanned, ReasonNoResult},
		"other root":        {nil, "example", filepath.Join(root, "x"), pin, StatusNotScanned, ReasonOtherDirectory},
		"other checker":     {func(r *Record) { r.Checker = "0" }, "example", root, pin, StatusNotScanned, ReasonCheckerChanged},
		"other hub":         {func(r *Record) { r.Configuration = currentConfiguration("https://hub.example") }, "example", root, pin, StatusNotScanned, ReasonCheckerChanged},
		"other template":    {nil, "example", root, strings.Repeat("d", 64), StatusNotScanned, ReasonTemplateChanged},
		"no template pin":   {nil, "example", root, "", StatusNotScanned, ReasonTemplateChanged},
		"stored findings":   {func(r *Record) { r.Status = StatusFindings }, "example", root, pin, StatusFindings, ""},
		"findings, no pin":  {func(r *Record) { r.Status, r.ChatTemplateSHA256 = StatusFindings, "" }, "example", root, "", StatusFindings, ""},
		"review, other pin": {func(r *Record) { *r = needsReview(t, *r) }, "example", root, strings.Repeat("d", 64), StatusFindings, ""},
	} {
		r := record
		if tc.edit != nil {
			tc.edit(&r)
		}
		if status, reason := StoredStatus(r, tc.model, tc.root, tc.pin); status != tc.status || reason != tc.reason {
			t.Errorf("%s: status %s, reason %q", name, status, reason)
		}
	}
	if !record.StaticChecksPassed() {
		t.Fatal("static checks of a clean model did not pass")
	}
	failed := record
	failed.Checks = append([]Check(nil), record.Checks...)
	failed.Checks[1].Outcome = OutcomeFailed
	if failed.StaticChecksPassed() || (Record{}).StaticChecksPassed() {
		t.Fatal("a failed or empty record passed the static checks")
	}
}

// A recheck reads the files again: changed files are not scanned, also
// when the stored status is not_scanned. Files that cannot be read are
// not scanned with their own reason. A canceled recheck gives no status.
func TestRecheckStatusFindsChangedFiles(t *testing.T) {
	record, root := statusRecord(t)
	pin := record.ChatTemplateSHA256
	ctx := context.Background()
	for _, launchRoot := range []string{root, ""} {
		if status, reason, err := RecheckStatus(ctx, record, "example", launchRoot, pin); err != nil || status != record.Status || reason != "" {
			t.Fatalf("recheck of unchanged files at %q = %s, %q, %v", launchRoot, status, reason, err)
		}
	}
	if status, reason, err := RecheckStatus(ctx, record, "other", root, pin); err != nil || reason != ReasonNoResult || status != StatusNotScanned {
		t.Fatalf("recheck of another model = %s, %q, %v", status, reason, err)
	}
	write(t, filepath.Join(root, "config.json"), `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","x":1}`)
	for _, launchRoot := range []string{root, ""} {
		if status, reason, err := RecheckStatus(ctx, record, "example", launchRoot, pin); err != nil || status != StatusNotScanned || reason != ReasonFilesChanged {
			t.Fatalf("recheck of changed files at %q = %s, %q, %v", launchRoot, status, reason, err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := RecheckStatus(cancelled, record, "example", root, pin); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled recheck gave %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if status, reason, err := RecheckStatus(ctx, record, "example", root, pin); err != nil || status != StatusNotScanned || reason != ReasonFilesUnavailable {
		t.Fatalf("recheck of a missing directory = %s, %q, %v", status, reason, err)
	}
}
