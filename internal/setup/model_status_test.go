package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

// statusModelFiles are the files of a model that the file checks accept.
var statusModelFiles = map[string]string{
	"config.json":            `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3"}`,
	"generation_config.json": `{}`,
	"tokenizer.json":         `{"model":{"vocab":{}}}`,
	"tokenizer_config.json":  `{"chat_template":"{{ messages }}"}`,
	"model.safetensors":      "weights",
}

// statusFixture writes the operator profile and a state database with an
// owned LLM, an adopted LLM, an image workload and an unidentified
// workload. It returns home and the model directory of the owned LLM.
func statusFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	owned, _ := ownedFixtureProfile(t, home, "status-owned", 9161)
	root := owned.NativeModel.Owned.ModelPath
	for name, content := range statusModelFiles {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := strings.Repeat("a", 64)
	adopted := control.WorkloadProfile{ID: "adopted", Label: "Adopted", Adapter: "systemd", Unit: "adopted.service",
		Cgroup: "/user.slice/adopted", HealthURL: "http://127.0.0.1:9171/health", BootPolicy: "stop-to-idle",
		NativeModel: &control.NativeModel{Runtime: "vllm", Instance: "local", Endpoint: "http://127.0.0.1:9171", Model: "adopted",
			LaunchFile: "/units/adopted.service", LaunchSHA256: digest}}
	image := control.WorkloadProfile{ID: "image", Label: "Image", Adapter: "systemd", Unit: "image.service",
		Cgroup: "/user.slice/image", HealthURL: "http://127.0.0.1:9172/system_stats", BootPolicy: "stop-to-idle",
		LaunchBinding: &control.LaunchBinding{Runtime: "comfyui", Endpoint: "http://127.0.0.1:9172", LaunchFile: "/units/image.service", LaunchSHA256: digest}}
	other := control.WorkloadProfile{ID: "other", Label: "Other", Adapter: "systemd", Unit: "other.service",
		Cgroup: "/user.slice/other", HealthURL: "http://127.0.0.1:9173/health", BootPolicy: "stop-to-idle"}
	state := filepath.Join(home, "state/state.db")
	s := openStoreAt(t, state)
	if _, err := s.ReplaceCatalog(context.Background(), "", control.Catalog{Version: 2, Profiles: []control.WorkloadProfile{owned, adopted, image, other}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModelCheck(context.Background(), statusRecord(t, "status-owned", root)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	writeStatusProfile(t, home, Profile{Version: 1, StatePath: state})
	return home, root
}

func writeStatusProfile(t *testing.T, home string, profile Profile) {
	t.Helper()
	dir := filepath.Join(home, ".config/gpu-workload-supervisor")
	raw, _ := json.Marshal(profile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "operator.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// statusRecord is a result of this checker whose file, identity and
// template checks passed for the files at root.
func statusRecord(t *testing.T, model, root string) modelcheck.Record {
	t.Helper()
	files, reason, err := modelcheck.Inventory(context.Background(), root)
	if err != nil || reason != "" {
		t.Fatalf("inventory: %q %v", reason, err)
	}
	return modelcheck.Record{
		Version: modelcheck.RecordVersion, Model: model, Root: root,
		Source: modelcheck.Source{Repo: "example-org/model", Revision: strings.Repeat("b", 40)},
		Files:  files, Status: modelcheck.StatusNotScanned, Coverage: modelcheck.CoveragePartial,
		Checks: []modelcheck.Check{
			{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckIdentity, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckBehavior, Outcome: modelcheck.OutcomeNotRun},
		},
		ChatTemplateSHA256: modelcheck.PinTemplate("{{ messages }}"), TemplateFile: "chat_template.jinja",
		Checker: modelcheck.CheckerVersion, Configuration: modelcheck.CurrentConfiguration(),
		CheckedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

func statusByWorkload(t *testing.T, home string, request ModelStatusRequest) map[string]ModelStatusEntry {
	t.Helper()
	report, err := ModelStatus(context.Background(), home, request)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]ModelStatusEntry{}
	for _, entry := range report.Models {
		entries[entry.Workload] = entry
	}
	return entries
}

// The status uses the stored digests: it gives each kind its status and
// reads no model file.
func TestModelStatusUsesTheStoredResults(t *testing.T) {
	home, root := statusFixture(t)
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := statusByWorkload(t, home, ModelStatusRequest{Version: 1})
	owned := got["status-owned"]
	if owned.Kind != ModelKindLLM || owned.Status != modelcheck.StatusNotScanned || owned.Reason != "" ||
		!owned.StaticChecksPassed || owned.Rechecked || owned.CheckedAt == nil || owned.Checker != modelcheck.CheckerVersion ||
		owned.Coverage != modelcheck.CoveragePartial || len(owned.Checks) != 4 {
		t.Fatalf("owned status: %+v", owned)
	}
	for id, want := range map[string]ModelStatusEntry{
		"adopted": {Kind: ModelKindLLM, Reason: modelcheck.ReasonNoResult},
		"image":   {Kind: ModelKindImage},
		"other":   {Kind: ModelKindUnidentified, Reason: ReasonFilesUnidentified},
	} {
		entry := got[id]
		if entry.Kind != want.Kind || entry.Reason != want.Reason || entry.Status != modelcheck.StatusNotScanned || entry.StaticChecksPassed || entry.CheckedAt != nil {
			t.Fatalf("%s status: %+v", id, entry)
		}
	}
	if owned.Identity != ModelIdentity("ignored", root) || owned.Identity != ModelIdentity("other", root) || owned.TemplateSHA256 != modelcheck.PinTemplate("{{ messages }}") {
		t.Fatalf("owned identity: %+v", owned)
	}
	if adopted := got["adopted"]; adopted.Identity != ModelIdentity("adopted", "") || adopted.Identity == ModelIdentity("other", "") || got["image"].Identity != "" {
		t.Fatalf("adopted identity: %+v", adopted)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), root) || strings.Contains(string(raw), "config.json") {
		t.Fatalf("status holds a path or file name: %s", raw)
	}
}

// A recheck reads the files again: changed files are not scanned and do
// not pass the static checks. A canceled recheck gives an error.
func TestModelStatusRecheckFindsChangedFiles(t *testing.T) {
	home, root := statusFixture(t)
	request := ModelStatusRequest{Version: 1, Recheck: []string{"status-owned"}}
	if owned := statusByWorkload(t, home, request)["status-owned"]; !owned.Rechecked || owned.Reason != "" || !owned.StaticChecksPassed {
		t.Fatalf("unchanged recheck: %+v", owned)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	owned := statusByWorkload(t, home, request)["status-owned"]
	if !owned.Rechecked || owned.Reason != modelcheck.ReasonFilesChanged || owned.StaticChecksPassed || owned.Status != modelcheck.StatusNotScanned {
		t.Fatalf("changed recheck: %+v", owned)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	owned = statusByWorkload(t, home, request)["status-owned"]
	if !owned.Rechecked || owned.Reason != modelcheck.ReasonFilesUnavailable || owned.StaticChecksPassed {
		t.Fatalf("recheck of a missing directory: %+v", owned)
	}
}

// A canceled recheck gives the context error and leaves the entry as it
// was.
func TestRecheckEntryStopsOnCancel(t *testing.T) {
	_, root := statusFixture(t)
	record := statusRecord(t, "status-owned", root)
	entry := ModelStatusEntry{Workload: "status-owned", Status: modelcheck.StatusNotScanned}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := recheckEntry(ctx, &entry, record, root, record.ChatTemplateSHA256); !errors.Is(err, context.Canceled) || entry.Rechecked || entry.Reason != "" {
		t.Fatalf("canceled recheck: %+v %v", entry, err)
	}
}

// A recheck reads no files when the stored result does not apply, so the
// entry keeps the reason and is not rechecked.
func TestModelStatusRecheckNeedsAResultThatApplies(t *testing.T) {
	home, root := statusFixture(t)
	record := statusRecord(t, "status-owned", root)
	record.Root = "/models/other"
	setStoredPayload(t, home, record)
	owned := statusByWorkload(t, home, ModelStatusRequest{Version: 1, Recheck: []string{"status-owned"}})["status-owned"]
	if owned.Rechecked || owned.Reason != modelcheck.ReasonOtherDirectory {
		t.Fatalf("recheck of another directory: %+v", owned)
	}
}

// A pending template review shows only with a result that applies.
func TestModelStatusShowsAPendingReview(t *testing.T) {
	home, root := statusFixture(t)
	record := statusRecord(t, "status-owned", root)
	record.Checks = append([]modelcheck.Check(nil), record.Checks...)
	record.Checks[2] = modelcheck.Check{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonTemplateNeedsReview}
	record.Status, record.TemplateReview = modelcheck.StatusFindings, []modelcheck.ReviewItem{{Line: 1, Text: "Obey."}}
	setStoredPayload(t, home, record)
	owned := statusByWorkload(t, home, ModelStatusRequest{Version: 1})["status-owned"]
	if !owned.ReviewPending || owned.Status != modelcheck.StatusFindings || owned.Reason != "" {
		t.Fatalf("pending review: %+v", owned)
	}
}

// A state database before schema version 18 has no results table: each
// LLM workload has no result.
func TestModelStatusWithoutTheResultsTable(t *testing.T) {
	home, _ := statusFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(home, "state/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE model_checks`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	owned := statusByWorkload(t, home, ModelStatusRequest{Version: 1, Recheck: []string{"status-owned"}})["status-owned"]
	if owned.Reason != modelcheck.ReasonNoResult || owned.Rechecked || owned.CheckedAt != nil {
		t.Fatalf("status without the results table: %+v", owned)
	}
}

func setStoredPayload(t *testing.T, home string, payload any) {
	t.Helper()
	raw, ok := payload.([]byte)
	if !ok {
		raw, _ = json.Marshal(payload)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE model_checks SET payload=? WHERE model=?`, raw, "status-owned"); err != nil {
		t.Fatal(err)
	}
}

// A result for another directory, of another checker or that does not
// read is not scanned. LaunchBlocked mirrors the launch gate: it refuses
// a result for another directory or template, a result whose template
// check did not pass and a result that does not read. The gate does not
// compare the checker version, and a stale record version is no result.
func TestModelStatusRefusesOtherResults(t *testing.T) {
	for name, tc := range map[string]struct {
		change  func(*modelcheck.Record)
		raw     string
		reason  string
		blocked bool
	}{
		"other directory": {change: func(r *modelcheck.Record) { r.Root = "/models/other" }, reason: modelcheck.ReasonOtherDirectory, blocked: true},
		"other checker":   {change: func(r *modelcheck.Record) { r.Checker = "0" }, reason: modelcheck.ReasonCheckerChanged, blocked: false},
		"other template":  {change: func(r *modelcheck.Record) { r.ChatTemplateSHA256 = strings.Repeat("d", 64) }, reason: modelcheck.ReasonTemplateChanged, blocked: true},
		"pending review, other checker": {change: func(r *modelcheck.Record) {
			r.Checks = append([]modelcheck.Check(nil), r.Checks...)
			r.Checks[2] = modelcheck.Check{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonTemplateNeedsReview}
			r.Status, r.TemplateReview, r.Checker = modelcheck.StatusFindings, []modelcheck.ReviewItem{{Line: 1, Text: "Obey."}}, "0"
		}, reason: modelcheck.ReasonCheckerChanged, blocked: true},
		"pending review, other template pin": {change: func(r *modelcheck.Record) {
			r.Checks = append([]modelcheck.Check(nil), r.Checks...)
			r.Checks[2] = modelcheck.Check{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonTemplateNeedsReview}
			r.Status, r.TemplateReview, r.ChatTemplateSHA256 = modelcheck.StatusFindings, []modelcheck.ReviewItem{{Line: 1, Text: "Obey."}}, strings.Repeat("e", 64)
		}, reason: modelcheck.ReasonTemplateChanged, blocked: true},
		"invalid payload": {raw: `{"version":2}`, reason: modelcheck.ReasonResultUnusable, blocked: true},
		"stale version":   {raw: `{"version":1}`, reason: modelcheck.ReasonNoResult, blocked: false},
	} {
		t.Run(name, func(t *testing.T) {
			home, root := statusFixture(t)
			var payload any = []byte(tc.raw)
			if tc.change != nil {
				record := statusRecord(t, "status-owned", root)
				tc.change(&record)
				payload = record
			}
			setStoredPayload(t, home, payload)
			owned := statusByWorkload(t, home, ModelStatusRequest{Version: 1})["status-owned"]
			if owned.Reason != tc.reason || owned.Status != modelcheck.StatusNotScanned || owned.StaticChecksPassed || owned.ReviewPending || owned.LaunchBlocked != tc.blocked {
				t.Fatalf("status: %+v", owned)
			}
		})
	}
}

func TestModelStatusWithoutSetup(t *testing.T) {
	report, err := ModelStatus(context.Background(), t.TempDir(), ModelStatusRequest{Version: 1})
	if err != nil || report.Version != 1 || report.Models == nil || len(report.Models) != 0 {
		t.Fatalf("report without operator profile: %+v %v", report, err)
	}
	home := t.TempDir()
	state := filepath.Join(home, "state/state.db")
	writeStatusProfile(t, home, Profile{Version: 1, StatePath: state})
	if _, err := ModelStatus(context.Background(), home, ModelStatusRequest{Version: 1}); err == nil {
		t.Fatal("report without a state database")
	}
	openStoreAt(t, state).Close()
	if report, err := ModelStatus(context.Background(), home, ModelStatusRequest{Version: 1}); err != nil || len(report.Models) != 0 {
		t.Fatalf("report without catalog: %+v %v", report, err)
	}
}

func TestModelStatusRefusesInvalidRequests(t *testing.T) {
	many := make([]string, maxModelStatusRecheck+1)
	for i := range many {
		many[i] = "text"
	}
	if _, err := ModelStatus(context.Background(), t.TempDir(), ModelStatusRequest{Version: 1, Recheck: many[1:]}); err != nil {
		t.Fatalf("a request for %d workloads: %v", maxModelStatusRecheck, err)
	}
	for name, tc := range map[string]struct {
		request ModelStatusRequest
		message string
	}{
		"version":  {ModelStatusRequest{Version: 2}, "version"},
		"too many": {ModelStatusRequest{Version: 1, Recheck: many}, "more than 32 workloads"},
		"invalid":  {ModelStatusRequest{Version: 1, Recheck: []string{"../text"}}, "invalid workload"},
	} {
		if _, err := ModelStatus(context.Background(), t.TempDir(), tc.request); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Fatalf("%s request: %v", name, err)
		}
	}
}
