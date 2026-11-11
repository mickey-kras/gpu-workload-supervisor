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
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/lock"
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
	// onRequest, when set, runs once at the first request, for example to
	// change setup while a run is in progress.
	onRequest func()
}

func (h *fakeHub) RoundTrip(r *http.Request) (*http.Response, error) {
	h.requests.Add(1)
	if h.onRequest != nil {
		h.onRequest()
		h.onRequest = nil
	}
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
	writeStatusProfile(t, home, Profile{Version: 1, StatePath: state, ActivatedRelease: deployment.Release})
	if err := deployment.Write(state, deployment.Marker{Version: 1, Release: deployment.Release}); err != nil {
		t.Fatal(err)
	}
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
	requests := hub.requests.Load()
	started := make(chan struct{})
	hub.started = started
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	if _, err := ModelCheck(ctx, home, ModelCheckRequest{Version: 1, Workload: "run-cancel", Source: runSource}, hub.client()); !errors.Is(err, modelcheck.ErrCheckCanceled) || hub.requests.Load() != requests+1 {
		t.Fatalf("a run canceled during the listing: %v, %d hub requests", err, hub.requests.Load())
	}
	after, err := storedResult(t, home, "run-cancel")
	if err != nil || after.CheckedAt != before.CheckedAt || after.Status != before.Status {
		t.Fatalf("the canceled run changed the result: %+v %v", after, err)
	}
	// A run canceled before it starts is refused early and stores nothing.
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
	// The new template is not the template that the launch pins, so the new
	// review is not current either.
	if _, err := ModelApprove(context.Background(), home, request, 1000, now); !errors.Is(err, ErrResultNotCurrent) {
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

func writeRunModel(t *testing.T, id string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(testModelRoot, id, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A hub failure keeps the earlier result only for the same files. Files
// that changed after the stored check give a stored error that blocks the
// launch. A run without an earlier result stores nothing: the workload
// stays without a result.
func TestModelCheckStoresAHubFailureForChangedFiles(t *testing.T) {
	home, _, hub := runFixture(t, "run-changed", "{{ messages }}")
	runCheck(t, home, "run-changed", hub)
	files := runModelFiles("{{ messages }}")
	files["generation_config.json"] = `{"temperature":0.5}`
	writeRunModel(t, "run-changed", files)
	hub.status = http.StatusServiceUnavailable
	entry := runCheck(t, home, "run-changed", hub)
	if entry.NotStored || entry.Status != modelcheck.StatusError || !entry.LaunchBlocked {
		t.Fatalf("entry %+v", entry)
	}
	record, err := storedResult(t, home, "run-changed")
	if err != nil || record.Status != modelcheck.StatusError || record.Checks[1].Reason != modelcheck.ReasonUpstreamUnreachable {
		t.Fatalf("stored %+v %v", record, err)
	}
	fresh, _, hub := runFixture(t, "run-first", "{{ messages }}")
	hub.status = http.StatusServiceUnavailable
	if entry := runCheck(t, fresh, "run-first", hub); !entry.NotStored || entry.LaunchBlocked {
		t.Fatalf("a first run with a hub failure: %+v", entry)
	}
	if _, err := storedResult(t, fresh, "run-first"); !errors.Is(err, store.ErrNoModelCheck) {
		t.Fatalf("a first run with a hub failure stored a result: %v", err)
	}
}

// A kept hub failure reports the launch block of the stored result.
func TestModelCheckKeptHubFailureReportsTheStoredBlock(t *testing.T) {
	home, _, hub := runFixture(t, "run-kept", "{{ messages }}")
	runCheck(t, home, "run-kept", hub)
	hub.status = http.StatusNotFound
	entry := runCheck(t, home, "run-kept", hub)
	if !entry.NotStored || entry.LaunchBlocked {
		t.Fatalf("a kept passing result does not block the launch: %+v", entry)
	}
}

// An unusable hub token and an identity mismatch are stored, never kept.
func TestModelCheckStoresTokenAndMismatchErrors(t *testing.T) {
	home, _, hub := runFixture(t, "run-token", reviewTemplate)
	runCheck(t, home, "run-token", hub)
	approveReview(t, home, "run-token")
	t.Setenv("HF_TOKEN", "bad token")
	entry := runCheck(t, home, "run-token", hub)
	record, err := storedResult(t, home, "run-token")
	if entry.NotStored || !entry.LaunchBlocked || err != nil || record.Checks[1].Reason != modelcheck.ReasonTokenUnusable || record.TemplateApproval != nil {
		t.Fatalf("token error: %+v %+v %v", entry, record.Checks, err)
	}
	t.Setenv("HF_TOKEN", "")
	hub.mismatch = "config.json"
	entry = runCheck(t, home, "run-token", hub)
	if entry.NotStored || entry.Status != modelcheck.StatusFindings || entry.Checks[1].Reason != modelcheck.ReasonIdentityMismatch {
		t.Fatalf("mismatch: %+v", entry)
	}
}

// The actions that write need an activated setup outside maintenance, with
// the installed release. They check this before the run and again before
// the write.
func TestModelCheckNeedsAnActivatedDeployment(t *testing.T) {
	home, _, hub := runFixture(t, "run-deploy", reviewTemplate)
	runCheck(t, home, "run-deploy", hub)
	review, _ := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-deploy"})
	state := filepath.Join(home, "state/state.db")
	before, _ := storedResult(t, home, "run-deploy")
	approve := ModelApproveRequest{Version: 1, Workload: "run-deploy", ChatTemplateSHA256: review.ChatTemplateSHA256, ReviewSHA256: review.ReviewSHA256}
	check := ModelCheckRequest{Version: 1, Workload: "run-deploy", Source: runSource}
	for name, marker := range map[string]deployment.Marker{
		"maintenance":   {Version: 1, Release: deployment.Release, Maintenance: true},
		"other release": {Version: 1, Release: "v0.0.1"},
	} {
		if err := deployment.Write(state, marker); err != nil {
			t.Fatal(err)
		}
		requests := hub.requests.Load()
		want := map[string]string{"maintenance": "maintenance", "other release": "not activated"}[name]
		if _, err := ModelCheck(context.Background(), home, check, hub.client()); err == nil || !strings.Contains(err.Error(), want) || hub.requests.Load() != requests {
			t.Fatalf("%s: check %v", name, err)
		}
		if _, err := ModelApprove(context.Background(), home, approve, 1000, time.Now()); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: approval %v", name, err)
		}
	}
	if err := deployment.Write(state, deployment.Marker{Version: 1, Release: deployment.Release}); err != nil {
		t.Fatal(err)
	}
	// Maintenance that starts during the run refuses the write.
	hub.mismatch = "config.json"
	hub.onRequest = func() {
		if err := deployment.Write(state, deployment.Marker{Version: 1, Release: deployment.Release, Maintenance: true}); err != nil {
			t.Error(err)
		}
	}
	if _, err := ModelCheck(context.Background(), home, check, hub.client()); err == nil {
		t.Fatal("a run stored its result during maintenance")
	}
	if after, _ := storedResult(t, home, "run-deploy"); after.CheckedAt != before.CheckedAt || after.Status != before.Status {
		t.Fatalf("maintenance did not stop the write: %+v", after)
	}
	// A profile without an activated release is refused.
	writeStatusProfile(t, home, Profile{Version: 1, StatePath: state})
	if _, err := ModelCheck(context.Background(), home, check, hub.client()); !errors.Is(err, ErrModelNotActivated) {
		t.Fatalf("not activated: %v", err)
	}
}

// A run stores its result only for the catalog that it read at the start.
func TestModelCheckRefusesAResultForAChangedCatalog(t *testing.T) {
	home, _, hub := runFixture(t, "run-catalog", "{{ messages }}")
	runCheck(t, home, "run-catalog", hub)
	before, _ := storedResult(t, home, "run-catalog")
	state := filepath.Join(home, "state/state.db")
	draft := ownedDraft("run-catalog", 9181)
	draft.Binding.Owned.ModelPath = testModelPath("run-catalog-moved")
	moved, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	hub.mismatch = "config.json"
	hub.onRequest = func() {
		s, err := store.Open(context.Background(), state)
		if err != nil {
			t.Error(err)
			return
		}
		defer s.Close()
		snapshot, err := s.Catalog(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		catalog := snapshot.Catalog.Clone()
		catalog.Profiles[0] = moved
		if _, err := s.ReplaceCatalog(context.Background(), snapshot.Revision, catalog); err != nil {
			t.Error(err)
		}
	}
	if _, err := ModelCheck(context.Background(), home, ModelCheckRequest{Version: 1, Workload: "run-catalog", Source: runSource}, hub.client()); !errors.Is(err, ErrResultNotCurrent) {
		t.Fatalf("a result for the old catalog: %v", err)
	}
	if after, _ := storedResult(t, home, "run-catalog"); after.CheckedAt != before.CheckedAt || after.Status != before.Status {
		t.Fatalf("a result for the old catalog was stored: %+v", after)
	}
}

// A review of another template than the launch pins is not current.
func TestModelReviewNeedsThePinnedTemplate(t *testing.T) {
	home, _, hub := runFixture(t, "run-pin", reviewTemplate)
	files := runModelFiles("{{ messages }}Answer in French.")
	writeRunModel(t, "run-pin", files)
	hub.files = files
	// The review is for another template than the launch pins: the status
	// says so, and no review is pending.
	if entry := runCheck(t, home, "run-pin", hub); entry.ReviewPending || entry.Reason != modelcheck.ReasonTemplateChanged || !entry.LaunchBlocked {
		t.Fatalf("entry %+v", entry)
	}
	review, err := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-pin"})
	if err != nil || review.Pending {
		t.Fatalf("a review of another template: %+v %v", review, err)
	}
	record, _ := storedResult(t, home, "run-pin")
	pending, _ := record.PendingReview()
	request := ModelApproveRequest{Version: 1, Workload: "run-pin", ChatTemplateSHA256: pending.ChatTemplateSHA256, ReviewSHA256: pending.ReviewSHA256}
	if _, err := ModelApprove(context.Background(), home, request, 1000, time.Now()); !errors.Is(err, ErrResultNotCurrent) {
		t.Fatalf("approval of another template: %v", err)
	}
}

// The write waits for the state gate until the time limit of the action,
// as the gpu-setup actions set it. A gate that stays busy until then gives
// ErrStateBusy, for a check and for an approval. A cancel during the wait
// gives the cancel error of the action. Nothing is stored in these cases. A
// gate that frees during the wait lets the write go on.
func TestModelCheckWaitsForTheStateGate(t *testing.T) {
	home, _, hub := runFixture(t, "run-gate", reviewTemplate)
	runCheck(t, home, "run-gate", hub)
	before, _ := storedResult(t, home, "run-gate")
	state := filepath.Join(home, "state/state.db")
	held, err := lock.TryAcquire(state + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	hub.mismatch = "config.json"
	check := ModelCheckRequest{Version: 1, Workload: "run-gate", Source: runSource}
	limited, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if _, err := ModelCheck(limited, home, check, hub.client()); !errors.Is(err, ErrStateBusy) {
		t.Fatalf("busy gate until the time limit of a check: %v", err)
	}
	review, _ := ModelReviewList(context.Background(), home, ModelReviewRequest{Version: 1, Workload: "run-gate"})
	approve := ModelApproveRequest{Version: 1, Workload: "run-gate", ChatTemplateSHA256: review.ChatTemplateSHA256, ReviewSHA256: review.ReviewSHA256}
	approveLimit, stopApprove := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stopApprove()
	if _, err := ModelApprove(approveLimit, home, approve, 1000, time.Now()); !errors.Is(err, ErrStateBusy) {
		t.Fatalf("busy gate until the time limit of an approval: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	hub.onRequest = func() { go func() { time.Sleep(300 * time.Millisecond); cancel() }() }
	if _, err := ModelCheck(canceled, home, check, hub.client()); !errors.Is(err, modelcheck.ErrCheckCanceled) {
		t.Fatalf("canceled during the wait: %v", err)
	}
	canceledApproval, cancelApproval := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancelApproval() }()
	if _, err := ModelApprove(canceledApproval, home, approve, 1000, time.Now()); !errors.Is(err, ErrApprovalCanceled) {
		t.Fatalf("approval canceled during the wait: %v", err)
	}
	if after, _ := storedResult(t, home, "run-gate"); after.CheckedAt != before.CheckedAt || after.Status != before.Status || after.TemplateApproval != nil {
		t.Fatalf("a refused write changed the result: %+v", after)
	}
	hub.onRequest = func() { go func() { time.Sleep(100 * time.Millisecond); held.Close() }() }
	if entry, err := ModelCheck(context.Background(), home, check, hub.client()); err != nil || entry.Status != modelcheck.StatusFindings {
		t.Fatalf("a gate that frees during the wait: %+v %v", entry, err)
	}
}

// acquireGate maps the end of the wait by its cause, and passes other lock
// errors through.
func TestAcquireGateMapsTheEndOfTheWait(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.db")
	held, err := lock.TryAcquire(state + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	deadline, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, err := acquireGate(deadline, state, checkStops); !errors.Is(err, ErrStateBusy) {
		t.Fatalf("deadline during the wait: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := acquireGate(canceled, state, approvalStops); !errors.Is(err, ErrApprovalCanceled) {
		t.Fatalf("cancel during the wait: %v", err)
	}
	if _, err := acquireGate(deadline, state, checkStops); !errors.Is(err, modelcheck.ErrCheckTimedOut) {
		t.Fatalf("deadline before the wait: %v", err)
	}
	if _, err := acquireGate(canceled, state, checkStops); !errors.Is(err, modelcheck.ErrCheckCanceled) {
		t.Fatalf("cancel before the wait: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = acquireGate(context.Background(), filepath.Join(file, "state.db"), checkStops)
	if err == nil || errors.Is(err, ErrStateBusy) || errors.Is(err, modelcheck.ErrCheckCanceled) || errors.Is(err, modelcheck.ErrCheckTimedOut) {
		t.Fatalf("a lock error must pass through: %v", err)
	}
}

// A hub failure for another model directory than the stored result is
// stored. A kept hub failure reports the block of the result that stays.
func TestModelCheckHubFailureForAnotherRootIsStored(t *testing.T) {
	home, _, hub := runFixture(t, "run-root", reviewTemplate)
	runCheck(t, home, "run-root", hub)
	hub.status = http.StatusNotFound
	if entry := runCheck(t, home, "run-root", hub); !entry.NotStored || !entry.LaunchBlocked {
		t.Fatalf("a kept result with a pending review blocks the launch: %+v", entry)
	}
	state := filepath.Join(home, "state/state.db")
	s := openStoreAt(t, state)
	record, _ := s.ModelCheck(context.Background(), "run-root")
	record.Root = testModelPath("run-root-old")
	if err := s.PutModelCheck(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	s.Close()
	entry := runCheck(t, home, "run-root", hub)
	stored, err := storedResult(t, home, "run-root")
	if entry.NotStored || !entry.LaunchBlocked || err != nil || stored.Root != testModelPath("run-root") || stored.Checks[1].Reason != modelcheck.ReasonUpstreamNotFound {
		t.Fatalf("hub failure for another root: %+v %+v %v", entry, stored.Checks, err)
	}
}

// Each error fragment that the desktop maps to a cause is in the text of
// an error that the model check actions give.
func TestDesktopFailureCausesMatchTheErrors(t *testing.T) {
	raw, err := os.ReadFile("../../clients/setup/model-reasons.mjs")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(raw), "const failureCauses = [")
	end := strings.Index(string(raw)[start:], "];")
	fragments := regexp.MustCompile(`\['([^']+)', '`).FindAllStringSubmatch(string(raw)[start:start+end], -1)
	state := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(state, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	texts := []string{ErrStateBusy.Error(), ErrModelNotActivated.Error(), ErrResultNotCurrent.Error(), ErrApprovalCanceled.Error(),
		modelcheck.ErrCheckTimedOut.Error(), modelcheck.ErrApprovalMismatch.Error(), store.ErrModelCheckConflict.Error()}
	for _, marker := range []deployment.Marker{{Version: 1, Release: deployment.Release, Maintenance: true}, {Version: 1, Release: "v0.0.1"}} {
		if err := deployment.Write(state, marker); err != nil {
			t.Fatal(err)
		}
		if err := deployment.Check(state, marker.Release); err != nil {
			texts = append(texts, err.Error())
		}
	}
	if len(fragments) < 8 {
		t.Fatalf("fragments %v", fragments)
	}
	for _, fragment := range fragments {
		if !slices.ContainsFunc(texts, func(text string) bool { return strings.Contains(text, fragment[1]) }) {
			t.Errorf("no error has the text %q", fragment[1])
		}
	}
}
