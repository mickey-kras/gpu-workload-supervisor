package setup

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var runSource = modelcheck.Source{Repo: "example-org/example-model", Revision: strings.Repeat("c", 40)}

// reviewTemplate has a literal that the operator must approve.
const reviewTemplate = "{{ messages }}Answer in English."

// runModelFiles are the files of a model that all static checks accept,
// with the template text.
func runModelFiles(template string) map[string]string {
	raw, _ := json.Marshal(map[string]string{"chat_template": template})
	return map[string]string{
		"config.json":            `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3"}`,
		"generation_config.json": `{}`,
		"tokenizer.json":         `{"model":{"vocab":{}}}`,
		"tokenizer_config.json":  string(raw),
		"chat_template.jinja":    template,
		"model.safetensors":      "weights",
	}
}

// fakeHub answers the hub API requests of one source from the files of
// the model, as the hub does: weights in LFS, other files as git blobs.
// It never opens a connection.
type fakeHub struct {
	files    map[string]string
	status   int
	mismatch string
	requests atomic.Int32
	// started, when set, gets one value at the first request, and the
	// request then waits for its context to end.
	started chan struct{}
}

func (h *fakeHub) RoundTrip(r *http.Request) (*http.Response, error) {
	h.requests.Add(1)
	if h.started != nil {
		close(h.started)
		h.started = nil
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	base := "/api/models/" + runSource.Repo
	reply := func(status int, body any) (*http.Response, error) {
		raw, _ := json.Marshal(body)
		header := http.Header{"Content-Type": {"application/json"}, "Retry-After": {"0"}}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
	}
	switch {
	case r.URL.Host != "huggingface.co":
		return nil, errors.New("unexpected host")
	case h.status != 0:
		return reply(h.status, map[string]string{"error": "fake"})
	case r.URL.Path == base+"/refs":
		return reply(http.StatusOK, map[string]any{"branches": []map[string]string{{"name": "main", "targetCommit": runSource.Revision}}, "tags": []any{}})
	case r.URL.Path == base+"/tree/"+runSource.Revision && r.URL.Query().Get("recursive") == "true":
		return reply(http.StatusOK, h.tree())
	}
	return reply(http.StatusNotFound, map[string]string{"error": "fake"})
}

func (h *fakeHub) tree() []modelcheck.UpstreamFile {
	entries := []modelcheck.UpstreamFile{}
	for name, content := range h.files {
		if name == h.mismatch {
			content += " upstream"
		}
		blob := sha1.New()
		fmt.Fprintf(blob, "blob %d\x00%s", len(content), content)
		entry := modelcheck.UpstreamFile{Type: "file", Path: name, OID: hex.EncodeToString(blob.Sum(nil)), Size: int64(len(content))}
		if strings.HasSuffix(name, ".safetensors") {
			sum := sha256.Sum256([]byte(content))
			entry.LFS = &modelcheck.LFSEntry{OID: hex.EncodeToString(sum[:]), Size: int64(len(content))}
		}
		entries = append(entries, entry)
	}
	return entries
}

func (h *fakeHub) client() *http.Client { return &http.Client{Transport: h} }

// runFixture writes the operator profile and a state database with one
// owned LLM whose model folder holds files with template, an adopted LLM,
// an image workload and an unidentified workload. It gives home, the
// model folder and a hub for the files.
func runFixture(t *testing.T, id, template string) (string, string, *fakeHub) {
	t.Helper()
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", "")
	t.Setenv("HF_HOME", t.TempDir())
	home := t.TempDir()
	files := runModelFiles(template)
	root := filepath.Join(testModelRoot, id)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	owned, _ := ownedFixtureProfile(t, home, id, 9181)
	statusHome, _ := statusFixture(t)
	others := statusProfiles(t, statusHome)
	state := filepath.Join(home, "state/state.db")
	s := openStoreAt(t, state)
	if _, err := s.ReplaceCatalog(context.Background(), "", control.Catalog{Version: 2, Profiles: append([]control.WorkloadProfile{owned}, others...)}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	writeStatusProfile(t, home, Profile{Version: 1, StatePath: state})
	return home, root, &fakeHub{files: files}
}

// statusProfiles gives the adopted, image and unidentified workloads of
// the status fixture at home.
func statusProfiles(t *testing.T, home string) []control.WorkloadProfile {
	t.Helper()
	snapshot, err := ReadCatalog(context.Background(), filepath.Join(home, "state/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Catalog.Profiles[1:]
}

func runCheck(t *testing.T, home, id string, hub *fakeHub) ModelStatusEntry {
	t.Helper()
	entry, err := ModelCheck(context.Background(), home, ModelCheckRequest{Version: 1, Workload: id, Source: runSource}, hub.client())
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func storedResult(t *testing.T, home, id string) (modelcheck.Record, error) {
	t.Helper()
	s := openStoreAt(t, filepath.Join(home, "state/state.db"))
	defer s.Close()
	return s.ModelCheck(context.Background(), id)
}

// A run that passes the static checks stores a result for the files, and
// the status reads it.
func TestModelCheckStoresTheResult(t *testing.T) {
	home, root, hub := runFixture(t, "run-pass", "{{ messages }}")
	entry := runCheck(t, home, "run-pass", hub)
	if entry.Status != modelcheck.StatusNotScanned || entry.Reason != "" || !entry.StaticChecksPassed || entry.NotStored || entry.LaunchBlocked || !entry.CheckAvailable {
		t.Fatalf("entry %+v", entry)
	}
	if entry.Source == nil || *entry.Source != runSource {
		t.Fatalf("source %+v", entry.Source)
	}
	record, err := storedResult(t, home, "run-pass")
	if err != nil || record.Root != root || record.Source != runSource || record.Checker != modelcheck.CheckerVersion {
		t.Fatalf("stored %+v %v", record, err)
	}
	status := statusByWorkload(t, home, ModelStatusRequest{Version: 1})["run-pass"]
	if !status.StaticChecksPassed || status.Reason != "" || status.LaunchBlocked {
		t.Fatalf("status %+v", status)
	}
}

// An identity mismatch is a finding: the run stores it over the earlier
// result, and the launch gate refuses it.
func TestModelCheckStoresFindings(t *testing.T) {
	home, _, hub := runFixture(t, "run-mismatch", "{{ messages }}")
	runCheck(t, home, "run-mismatch", hub)
	hub.mismatch = "config.json"
	entry := runCheck(t, home, "run-mismatch", hub)
	if entry.Status != modelcheck.StatusFindings || entry.NotStored || !entry.LaunchBlocked || entry.StaticChecksPassed {
		t.Fatalf("entry %+v", entry)
	}
	record, err := storedResult(t, home, "run-mismatch")
	if err != nil || record.Status != modelcheck.StatusFindings || record.Checks[1].Reason != modelcheck.ReasonIdentityMismatch {
		t.Fatalf("stored %+v %v", record, err)
	}
}

// A run whose hub listing fails gives the error of the run, and keeps the
// earlier stored result, also its approval.
func TestModelCheckKeepsTheResultWhenTheHubFails(t *testing.T) {
	for name, status := range map[string]int{"unreachable": http.StatusServiceUnavailable, "not found": http.StatusNotFound, "denied": http.StatusForbidden} {
		t.Run(name, func(t *testing.T) {
			home, _, hub := runFixture(t, "run-hub", reviewTemplate)
			runCheck(t, home, "run-hub", hub)
			approveReview(t, home, "run-hub")
			before, err := storedResult(t, home, "run-hub")
			if err != nil || before.TemplateApproval == nil {
				t.Fatalf("approved %+v %v", before, err)
			}
			hub.status = status
			entry := runCheck(t, home, "run-hub", hub)
			if !entry.NotStored || entry.Status != modelcheck.StatusError || !strings.HasPrefix(entry.Checks[1].Reason, "upstream-") {
				t.Fatalf("entry %+v", entry)
			}
			after, err := storedResult(t, home, "run-hub")
			if err != nil || after.CheckedAt != before.CheckedAt || after.TemplateApproval == nil || after.Status != before.Status {
				t.Fatalf("the earlier result changed: %+v %v", after, err)
			}
		})
	}
}

// A canceled run stores nothing: the earlier result stays.
func TestModelCheckCancelStoresNothing(t *testing.T) {
	home, _, hub := runFixture(t, "run-cancel", "{{ messages }}")
	runCheck(t, home, "run-cancel", hub)
	before, err := storedResult(t, home, "run-cancel")
	if err != nil {
		t.Fatal(err)
	}
	hub.mismatch = "config.json"
	started := make(chan struct{})
	hub.started = started
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	if _, err := ModelCheck(ctx, home, ModelCheckRequest{Version: 1, Workload: "run-cancel", Source: runSource}, hub.client()); err == nil {
		t.Fatal("a canceled run gave a result")
	}
	after, err := storedResult(t, home, "run-cancel")
	if err != nil || after.CheckedAt != before.CheckedAt || after.Status != before.Status {
		t.Fatalf("the canceled run changed the result: %+v %v", after, err)
	}
	// A run canceled before it starts stores nothing either.
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := ModelCheck(done, home, ModelCheckRequest{Version: 1, Workload: "run-cancel", Source: runSource}, hub.client()); err == nil {
		t.Fatal("a canceled run gave a result")
	}
}

func approveReview(t *testing.T, home, id string) ModelStatusEntry {
	t.Helper()
	review, err := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: id})
	if err != nil || !review.Pending {
		t.Fatalf("review %+v %v", review, err)
	}
	entry, err := ModelApprove(context.Background(), home, ModelApproveRequest{Version: 1, Workload: id, ChatTemplateSHA256: review.ChatTemplateSHA256, ReviewSHA256: review.ReviewSHA256}, 1000, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// A template with literals asks for review. The operator reads the list,
// approves it, and the static checks then pass. A later run of the same
// files keeps the approval.
func TestModelReviewThenApprovePassesTheStaticChecks(t *testing.T) {
	home, _, hub := runFixture(t, "run-review", reviewTemplate)
	entry := runCheck(t, home, "run-review", hub)
	if !entry.ReviewPending || entry.StaticChecksPassed || !entry.LaunchBlocked || entry.Status != modelcheck.StatusFindings {
		t.Fatalf("entry %+v", entry)
	}
	review, err := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-review"})
	if err != nil {
		t.Fatal(err)
	}
	if review.TemplateFile != "chat_template.jinja" || review.ChatTemplateSHA256 != modelcheck.PinTemplate(reviewTemplate) || len(review.Items) != 1 || review.Items[0].Text != "Answer in English." || review.Items[0].Line != 1 {
		t.Fatalf("review %+v", review)
	}
	if review.ReviewSHA256 != modelcheck.ReviewDigest(review.Items) {
		t.Fatal("review digest")
	}
	approved := approveReview(t, home, "run-review")
	if approved.ReviewPending || !approved.StaticChecksPassed || approved.LaunchBlocked || approved.Reason != "" {
		t.Fatalf("approved %+v", approved)
	}
	status := statusByWorkload(t, home, ModelStatusRequest{Version: 1})["run-review"]
	if !status.StaticChecksPassed || status.ReviewPending {
		t.Fatalf("status %+v", status)
	}
	again := runCheck(t, home, "run-review", hub)
	if !again.StaticChecksPassed || again.ReviewPending {
		t.Fatalf("a new run dropped the approval: %+v", again)
	}
	record, err := storedResult(t, home, "run-review")
	if err != nil || record.TemplateApproval == nil || record.TemplateApproval.ApproverUID != 1000 {
		t.Fatalf("approval %+v %v", record.TemplateApproval, err)
	}
	none, err := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-review"})
	if err != nil || none.Pending || len(none.Items) != 0 {
		t.Fatalf("an approved result has no review: %+v %v", none, err)
	}
}

// An approval for other SHA-256 values, of a result that asks for no
// review, or of a result that changed after the read, is refused.
func TestModelApproveRefusesMismatchAndConflict(t *testing.T) {
	home, _, hub := runFixture(t, "run-approve", reviewTemplate)
	runCheck(t, home, "run-approve", hub)
	review, err := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-approve"})
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("f", 64)
	now := time.Now()
	for name, request := range map[string]ModelApproveRequest{
		"template": {Version: 1, Workload: "run-approve", ChatTemplateSHA256: other, ReviewSHA256: review.ReviewSHA256},
		"review":   {Version: 1, Workload: "run-approve", ChatTemplateSHA256: review.ChatTemplateSHA256, ReviewSHA256: other},
	} {
		if _, err := ModelApprove(context.Background(), home, request, 1000, now); !errors.Is(err, modelcheck.ErrApprovalMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if record, _ := storedResult(t, home, "run-approve"); record.TemplateApproval != nil {
		t.Fatal("a refused approval was stored")
	}
	// A row that another write changed between the read and the update is
	// a conflict. A test cannot run a write there, so the row holds the
	// same bytes as TEXT, which the update never finds equal to the BLOB
	// that it read (see the store tests).
	textPayload(t, home, "run-approve")
	request := ModelApproveRequest{Version: 1, Workload: "run-approve", ChatTemplateSHA256: review.ChatTemplateSHA256, ReviewSHA256: review.ReviewSHA256}
	if _, err := ModelApprove(context.Background(), home, request, 1000, now); !errors.Is(err, store.ErrModelCheckConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if record, _ := storedResult(t, home, "run-approve"); record.TemplateApproval != nil {
		t.Fatal("a conflict stored the approval")
	}
	// A new run of another template replaces the review list that the
	// operator read: its approval does not match.
	hub.files = runModelFiles("{{ messages }}Answer in French.")
	for name, content := range hub.files {
		if err := os.WriteFile(filepath.Join(testModelRoot, "run-approve", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runCheck(t, home, "run-approve", hub)
	if _, err := ModelApprove(context.Background(), home, request, 1000, now); !errors.Is(err, modelcheck.ErrApprovalMismatch) {
		t.Fatalf("approval of a replaced review: %v", err)
	}
	hub.files = runModelFiles(reviewTemplate)
	for name, content := range hub.files {
		if err := os.WriteFile(filepath.Join(testModelRoot, "run-approve", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runCheck(t, home, "run-approve", hub)
	if _, err := ModelApprove(context.Background(), home, request, 1000, now); err != nil {
		t.Fatal(err)
	}
	if _, err := ModelApprove(context.Background(), home, request, 1000, now); !errors.Is(err, modelcheck.ErrNoPendingReview) {
		t.Fatalf("second approval: %v", err)
	}
	// A result that does not apply to the accepted launch is not approved.
	home, _, hub = runFixture(t, "run-stale", reviewTemplate)
	runCheck(t, home, "run-stale", hub)
	review, _ = ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-stale"})
	s := openStoreAt(t, filepath.Join(home, "state/state.db"))
	record, _ := s.ModelCheck(context.Background(), "run-stale")
	record.Root = filepath.Join(record.Root, "elsewhere")
	if err := s.PutModelCheck(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	s.Close()
	stale := ModelApproveRequest{Version: 1, Workload: "run-stale", ChatTemplateSHA256: review.ChatTemplateSHA256, ReviewSHA256: review.ReviewSHA256}
	if _, err := ModelApprove(context.Background(), home, stale, 1000, now); !errors.Is(err, ErrResultNotCurrent) {
		t.Fatalf("stale approval: %v", err)
	}
	if list, err := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-stale"}); err != nil || list.Pending {
		t.Fatalf("stale review: %+v %v", list, err)
	}
}

// textPayload stores the payload of the result of model as TEXT, with the
// same bytes.
func textPayload(t *testing.T, home, model string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE model_checks SET payload=CAST(payload AS TEXT) WHERE model=?`, model); err != nil {
		t.Fatal(err)
	}
}

// Setup checks only accepted LLM workloads with a model folder, and only
// for valid requests.
func TestModelCheckRefusesOtherWorkloads(t *testing.T) {
	home, _, hub := runFixture(t, "run-refuse", "{{ messages }}")
	for workload, want := range map[string]error{"adopted": ErrModelNoDirectory, "image": ErrModelNotLLM, "other": ErrModelNotLLM, "missing": ErrModelNotConfigured} {
		request := ModelCheckRequest{Version: 1, Workload: workload, Source: runSource}
		if _, err := ModelCheck(context.Background(), home, request, hub.client()); !errors.Is(err, want) {
			t.Fatalf("check %s: %v", workload, err)
		}
		if _, err := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: workload}); !errors.Is(err, want) {
			t.Fatalf("review %s: %v", workload, err)
		}
		approve := ModelApproveRequest{Version: 1, Workload: workload, ChatTemplateSHA256: strings.Repeat("a", 64), ReviewSHA256: strings.Repeat("b", 64)}
		if _, err := ModelApprove(context.Background(), home, approve, 1000, time.Now()); !errors.Is(err, want) {
			t.Fatalf("approve %s: %v", workload, err)
		}
	}
	if hub.requests.Load() != 0 {
		t.Fatal("a refused request read the hub")
	}
	if _, err := ModelCheck(context.Background(), t.TempDir(), ModelCheckRequest{Version: 1, Workload: "run-refuse", Source: runSource}, hub.client()); !errors.Is(err, ErrModelNotConfigured) {
		t.Fatalf("no profile: %v", err)
	}
	status := statusByWorkload(t, home, ModelStatusRequest{Version: 1})
	if !status["run-refuse"].CheckAvailable || status["adopted"].CheckAvailable || status["image"].CheckAvailable {
		t.Fatalf("check availability %+v", status)
	}
}

// The requests are strict JSON of at most 4 KiB.
func TestModelCheckRequestsAreStrict(t *testing.T) {
	valid := `{"version":1,"workload":"chat","source":{"repo":"example-org/model","revision":"` + strings.Repeat("a", 40) + `"}}`
	if request, err := DecodeModelCheckRequest(strings.NewReader(valid)); err != nil || request.Source.Repo != "example-org/model" {
		t.Fatalf("valid: %+v %v", request, err)
	}
	for name, raw := range map[string]string{
		"unknown":        strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1),
		"nested unknown": strings.Replace(valid, `"repo"`, `"path":"x","repo"`, 1),
		"duplicate":      strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"trailing":       valid + `{}`,
		"too large":      strings.Replace(valid, `"chat"`, `"chat"`+strings.Repeat(" ", MaxModelCheckRequestBytes), 1),
		"version":        strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"workload":       strings.Replace(valid, `"chat"`, `"../chat"`, 1),
		"idle":           strings.Replace(valid, `"chat"`, `"idle"`, 1),
		"branch":         strings.Replace(valid, strings.Repeat("a", 40), "main", 1),
		"upper revision": strings.Replace(valid, strings.Repeat("a", 40), strings.Repeat("A", 40), 1),
		"repo":           strings.Replace(valid, "example-org/model", "model", 1),
		"dots":           strings.Replace(valid, "example-org/model", "example-org/..", 1),
		"no source":      `{"version":1,"workload":"chat"}`,
	} {
		if _, err := DecodeModelCheckRequest(strings.NewReader(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	review := `{"version":1,"workload":"chat"}`
	if _, err := DecodeModelReviewRequest(strings.NewReader(review)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"version":1}`, `{"version":1,"workload":"chat","extra":1}`, `{"version":2,"workload":"chat"}`, review + strings.Repeat(" ", MaxModelCheckRequestBytes)} {
		if _, err := DecodeModelReviewRequest(strings.NewReader(raw)); err == nil {
			t.Errorf("review %s accepted", raw)
		}
	}
	approve := `{"version":1,"workload":"chat","chatTemplateSHA256":"` + strings.Repeat("a", 64) + `","reviewSHA256":"` + strings.Repeat("b", 64) + `"}`
	if _, err := DecodeModelApproveRequest(strings.NewReader(approve)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(approve, strings.Repeat("a", 64), strings.Repeat("a", 63), 1),
		strings.Replace(approve, strings.Repeat("b", 64), strings.Repeat("B", 64), 1),
		strings.Replace(approve, `"chat"`, `""`, 1),
		strings.Replace(approve, `"version":1`, `"version":1,"approverUID":0`, 1),
		approve + strings.Repeat(" ", MaxModelCheckRequestBytes),
	} {
		if _, err := DecodeModelApproveRequest(strings.NewReader(raw)); err == nil {
			t.Errorf("approve %s accepted", raw)
		}
	}
}
