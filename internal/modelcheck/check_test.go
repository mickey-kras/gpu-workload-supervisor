package modelcheck

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var (
	testSource = Source{Repo: "example-org/example-model", Revision: strings.Repeat("a", 40)}
	testNow    = func() time.Time { return time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC) }
)

const indexJSON = `{"metadata":{},"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors"}}`

func modelFiles() map[string]string {
	return map[string]string{
		"config.json":                      `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3"}`,
		"generation_config.json":           `{}`,
		"tokenizer.json":                   `{"model":{"vocab":{}}}`,
		"tokenizer_config.json":            `{"chat_template":"{{ messages }}"}`,
		"chat_template.jinja":              "{{ messages }}",
		"model-00001-of-00002.safetensors": "weights one",
		"model-00002-of-00002.safetensors": "weights two",
		"model.safetensors.index.json":     indexJSON,
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeModel(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "model")
	for name, content := range files {
		write(t, filepath.Join(root, name), content)
	}
	return root
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func blobID(content string) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

// upstreamOf lists files as the hub does: weights in LFS, others as blobs.
func upstreamOf(files map[string]string) []UpstreamFile {
	var entries []UpstreamFile
	for name, content := range files {
		entry := UpstreamFile{Type: "file", Path: name, OID: blobID(content), Size: int64(len(content))}
		if strings.HasSuffix(name, ".safetensors") {
			entry.OID = blobID("version https://git-lfs.github.com/spec/v1\n")
			entry.LFS = &LFSEntry{OID: sha256Hex(content), Size: int64(len(content))}
		}
		entries = append(entries, entry)
	}
	return entries
}

// hub serves a listing in pages of two entries, with next links.
type hub struct {
	server   *httptest.Server
	entries  []UpstreamFile
	requests atomic.Int32
	status   int
	link     string
	body     string
}

func newHub(t *testing.T, entries []UpstreamFile) *hub {
	t.Helper()
	h := &hub{entries: entries, status: http.StatusOK}
	h.server = httptest.NewTLSServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	h.requests.Add(1)
	if r.URL.Path != "/api/models/"+testSource.Repo+"/tree/"+testSource.Revision || r.URL.Query().Get("recursive") != "true" {
		http.NotFound(w, r)
		return
	}
	if h.body != "" || h.status != http.StatusOK {
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(h.body))
		return
	}
	page := 0
	fmt.Sscan(r.URL.Query().Get("cursor"), &page)
	end := min(page+2, len(h.entries))
	if end < len(h.entries) {
		next := fmt.Sprintf("<%s%s?recursive=true&cursor=%d>; rel=\"next\"", h.server.URL, r.URL.Path, end)
		if h.link != "" {
			next = h.link
		}
		w.Header().Set("Link", next)
	}
	raw, _ := json.Marshal(append([]UpstreamFile{{Type: "directory", Path: "dir", OID: strings.Repeat("0", 40)}}, h.entries[page:end]...))
	_, _ = w.Write(raw)
}

func (h *hub) checker() Checker {
	return Checker{Listing: HubListing(h.server.Client(), h.server.URL), Version: "test-1", Now: testNow}
}

func run(t *testing.T, c Checker, root string) Record {
	t.Helper()
	record, err := c.Run(context.Background(), Request{Model: "example", Root: root, Source: testSource})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRunBindsPassingFileChecksToDigests(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	h := newHub(t, append(upstreamOf(files), UpstreamFile{Type: "file", Path: "README.md", OID: blobID("readme"), Size: 6}))
	record := run(t, h.checker(), root)
	if record.Status != StatusNotScanned {
		t.Fatalf("status = %s; the later checks did not run", record.Status)
	}
	want := []Check{{CheckAllowlist, OutcomePassed, ""}, {CheckIdentity, OutcomePassed, ""}, {CheckTemplate, OutcomeNotRun, ""}, {CheckBehavior, OutcomeNotRun, ""}}
	for i, check := range want {
		if record.Checks[i] != check {
			t.Fatalf("check %d = %+v, want %+v", i, record.Checks[i], check)
		}
	}
	if len(record.Files) != len(files) || record.Files["config.json"] != sha256Hex(files["config.json"]) {
		t.Fatal("record must bind the SHA-256 of every model file")
	}
	if record.CheckedAt != testNow() || record.Checker != "test-1" || h.requests.Load() < 4 {
		t.Fatalf("unexpected record metadata or paging: %+v, %d requests", record, h.requests.Load())
	}
	if Current(&record, "example", root, testSource, record.Files) != StatusNotScanned {
		t.Fatal("current status must follow the record")
	}
}

func TestAllowlistRejectsFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		file, content string
		outcome       Outcome
		reason        string
	}{
		"python code":         {"modeling.py", "import os", OutcomeFailed, ReasonCodeOrPickleFile},
		"pickle weights":      {"pytorch_model.bin", "pickle", OutcomeFailed, ReasonCodeOrPickleFile},
		"torch weights":       {"model.pt", "pickle", OutcomeFailed, ReasonCodeOrPickleFile},
		"checkpoint":          {"model.ckpt", "pickle", OutcomeFailed, ReasonCodeOrPickleFile},
		"unlisted file":       {"README.md", "text", OutcomeFailed, ReasonUnlistedFile},
		"nested file":         {"original/config.json", "{}", OutcomeFailed, ReasonNestedFile},
		"auto_map in config":  {"config.json", `{"auto_map":{"AutoModel":"x.Model"}}`, OutcomeFailed, ReasonRemoteCode},
		"nested auto_map":     {"tokenizer_config.json", `{"a":[{"b":{"auto_map":1}}]}`, OutcomeFailed, ReasonRemoteCode},
		"duplicate JSON key":  {"generation_config.json", `{"a":1,"a":2}`, OutcomeFailed, ReasonInvalidJSON},
		"trailing JSON value": {"generation_config.json", `{}{}`, OutcomeFailed, ReasonInvalidJSON},
		"malformed JSON":      {"generation_config.json", `{"a":`, OutcomeFailed, ReasonInvalidJSON},
		"oversized JSON":      {"tokenizer.json", strings.Repeat(" ", MaxJSONFileBytes+1), OutcomeFailed, ReasonInvalidJSON},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			files[tc.file] = tc.content
			root := writeModel(t, files)
			h := newHub(t, upstreamOf(files))
			record := run(t, h.checker(), root)
			if record.Checks[0] != (Check{CheckAllowlist, tc.outcome, tc.reason}) || record.Status != StatusFindings {
				t.Fatalf("allowlist = %+v, status %s", record.Checks[0], record.Status)
			}
			if record.Checks[1].Outcome != OutcomeNotRun || h.requests.Load() != 0 {
				t.Fatal("a failed check must stop the later checks")
			}
		})
	}
}

func TestAllowlistNeedsASafetensorsModel(t *testing.T) {
	for name, remove := range map[string]string{"no weights": ".safetensors", "no configuration": "config.json"} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			for file := range files {
				if strings.HasSuffix(file, remove) || file == "model.safetensors.index.json" {
					delete(files, file)
				}
			}
			record := run(t, newHub(t, nil).checker(), writeModel(t, files))
			if record.Status != StatusUnsupported || record.Checks[0].Outcome != OutcomeUnsupported {
				t.Fatalf("status = %s", record.Status)
			}
		})
	}
}

func TestRunRefusesInvalidInventories(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, root string){
		"symbolic link": func(t *testing.T, root string) {
			if err := os.Symlink("config.json", filepath.Join(root, "linked.json")); err != nil {
				t.Fatal(err)
			}
		},
		"shard index names a missing shard": func(t *testing.T, root string) {
			write(t, filepath.Join(root, "model.safetensors.index.json"), `{"weight_map":{"a":"absent.safetensors"}}`)
		},
		"shard index names other weights": func(t *testing.T, root string) {
			write(t, filepath.Join(root, "model.safetensors.index.json"), `{"weight_map":{"a":"config.json"}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeModel(t, modelFiles())
			change(t, root)
			if _, err := newHub(t, nil).checker().Run(context.Background(), Request{Model: "example", Root: root, Source: testSource}); err == nil {
				t.Fatal("an invalid inventory was checked")
			}
		})
	}
}

func TestIdentityMismatchIsAFinding(t *testing.T) {
	for name, change := range map[string]func(entries []UpstreamFile) []UpstreamFile{
		"changed weights": func(entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].LFS != nil {
					entries[i].LFS.OID = sha256Hex("other weights")
				}
			}
			return entries
		},
		"changed small file": func(entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].Path == "chat_template.jinja" {
					entries[i].OID = blobID("{{ poisoned }}")
				}
			}
			return entries
		},
		"other size": func(entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].Path == "config.json" {
					entries[i].Size++
				}
			}
			return entries
		},
		"file not upstream": func(entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].Path == "generation_config.json" {
					return append(entries[:i], entries[i+1:]...)
				}
			}
			return entries
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			record := run(t, newHub(t, change(upstreamOf(files))).checker(), writeModel(t, files))
			if record.Status != StatusFindings || record.Checks[1].Outcome != OutcomeFailed || record.Checks[2].Outcome != OutcomeNotRun {
				t.Fatalf("identity = %+v", record.Checks[1])
			}
		})
	}
}

func TestUpstreamFailuresAreErrors(t *testing.T) {
	for name, change := range map[string]func(h *hub){
		"not found": func(h *hub) { h.status = http.StatusNotFound },
		"malformed": func(h *hub) { h.body = `[{"type":` },
		"invalid entry": func(h *hub) {
			h.body = `[{"type":"file","path":"../x","oid":"` + strings.Repeat("0", 40) + `","size":1}]`
		},
		"invalid LFS digest": func(h *hub) {
			h.body = `[{"type":"file","path":"x","oid":"` + strings.Repeat("0", 40) + `","size":1,"lfs":{"oid":"zz","size":1}}]`
		},
		"duplicate path":  func(h *hub) { h.entries = append(h.entries, h.entries[0], h.entries[0]) },
		"other-host link": func(h *hub) { h.link = `<https://example.invalid/api/models/x/tree/y>; rel="next"` },
		"oversized page":  func(h *hub) { h.body = "[" + strings.Repeat(" ", maxListingPageBytes) + "]" },
		"unknown entry type": func(h *hub) {
			h.body = `[{"type":"symlink","path":"x","oid":"` + strings.Repeat("0", 40) + `","size":1}]`
		},
		"negative entry size": func(h *hub) {
			h.body = `[{"type":"file","path":"x","oid":"` + strings.Repeat("0", 40) + `","size":-1}]`
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			h := newHub(t, upstreamOf(files))
			change(h)
			record := run(t, h.checker(), writeModel(t, files))
			if record.Status != StatusError || record.Checks[1] != (Check{CheckIdentity, OutcomeError, "upstream-unavailable"}) {
				t.Fatalf("identity = %+v", record.Checks[1])
			}
		})
	}
}

func TestHubListingBoundsPagesAndSource(t *testing.T) {
	entries := make([]UpstreamFile, 2*maxListingPages+2)
	for i := range entries {
		entries[i] = UpstreamFile{Type: "file", Path: fmt.Sprintf("f%d", i), OID: strings.Repeat("0", 40)}
	}
	h := newHub(t, entries)
	if _, err := HubListing(h.server.Client(), h.server.URL)(context.Background(), testSource); err == nil {
		t.Fatal("an unbounded listing was accepted")
	}
	for _, source := range []Source{{Repo: "one", Revision: testSource.Revision}, {Repo: "a/../b", Revision: testSource.Revision}, {Repo: testSource.Repo, Revision: "main"}} {
		if _, err := HubListing(h.server.Client(), h.server.URL)(context.Background(), source); err == nil {
			t.Fatalf("source %+v was accepted", source)
		}
	}
	if _, err := HubListing(http.DefaultClient, "http://127.0.0.1:1")(context.Background(), testSource); err == nil {
		t.Fatal("a plain HTTP hub was accepted")
	}
	if _, err := HubListing(h.server.Client(), "https://127.0.0.1:1")(context.Background(), testSource); err == nil {
		t.Fatal("an unreachable hub was accepted")
	}
}

func TestRunRefusesBadRequestsAndCancel(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	h := newHub(t, upstreamOf(files))
	for _, request := range []Request{
		{Model: "", Root: root, Source: testSource},
		{Model: "example", Root: "model", Source: testSource},
		{Model: "example", Root: root, Source: Source{Repo: testSource.Repo}},
	} {
		if _, err := h.checker().Run(context.Background(), request); err == nil {
			t.Fatalf("request %+v was accepted", request)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.checker().Run(ctx, Request{Model: "example", Root: root, Source: testSource}); err == nil {
		t.Fatal("a cancelled check gave a result; the model must stay not scanned")
	}
}

func TestFileChangeResetsToNotScanned(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	record := run(t, newHub(t, upstreamOf(files)).checker(), root)
	record.Checks[2].Outcome, record.Checks[3].Outcome = OutcomePassed, OutcomePassed
	record.Status = statusOf(record.Checks)
	if record.Status != StatusPassed {
		t.Fatal("all checks passing must give passed")
	}
	fresh, err := Inventory(root)
	if err != nil || Current(&record, "example", root, testSource, fresh) != StatusPassed {
		t.Fatal("an unchanged model must keep its status", err)
	}
	write(t, filepath.Join(root, "generation_config.json"), `{"changed":true}`)
	changed, err := Inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	other := testSource
	other.Revision = strings.Repeat("b", 40)
	for name, status := range map[string]Status{
		"changed file": Current(&record, "example", root, testSource, changed),
		"other model":  Current(&record, "other", root, testSource, fresh),
		"other root":   Current(&record, "example", root+"2", testSource, fresh),
		"other source": Current(&record, "example", root, other, fresh),
		"no record":    Current(nil, "example", root, testSource, fresh),
	} {
		if status != StatusNotScanned {
			t.Fatalf("%s: status = %s, want not_scanned", name, status)
		}
	}
}

func TestFilesChangedDuringTheCheckAreErrors(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	digests, err := Inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "config.json"), `{"architectures":["Other"]}`)
	reason, err := checkAllowlist(root, digests)
	if got := outcome(CheckAllowlist, reason, err); got.Reason != "files-changed" || got.Outcome != OutcomeError {
		t.Fatalf("allowlist after change = %+v", got)
	}
	upstream := map[string]UpstreamFile{}
	for _, entry := range upstreamOf(files) {
		upstream[entry.Path] = entry
	}
	only := map[string]string{"config.json": digests["config.json"]}
	reason, err = checkIdentity(root, only, upstream)
	if got := outcome(CheckIdentity, reason, err); got.Reason != "files-changed" {
		t.Fatalf("identity after change = %+v", got)
	}
	if err := os.Remove(filepath.Join(root, "config.json")); err != nil {
		t.Fatal(err)
	}
	reason, err = checkIdentity(root, only, upstream)
	if got := outcome(CheckIdentity, reason, err); got.Reason != "unreadable" {
		t.Fatalf("identity after removal = %+v", got)
	}
}
