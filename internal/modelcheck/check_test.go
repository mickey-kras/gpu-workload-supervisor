package modelcheck

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/llmtraining"
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

// hub serves the refs, a branch history and a tree listing in pages of two
// entries, with next links. Overrides apply to the tree listing only.
type hub struct {
	server    *httptest.Server
	entries   []UpstreamFile
	requests  atomic.Int32
	strays    atomic.Int32
	status    int
	redirect  bool
	errorCode string
	link      string
	body      string
	branches  []hubRef
	tags      []hubRef
	histories map[string][]string
	stalled   chan struct{}
	mu        sync.Mutex
	auth      []string
}

func newHub(t *testing.T, entries []UpstreamFile) *hub {
	t.Helper()
	h := &hub{entries: entries, status: http.StatusOK, branches: []hubRef{{Name: "main", TargetCommit: testSource.Revision}}, tags: []hubRef{}}
	h.server = httptest.NewTLSServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.server.Close)
	return h
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	h.requests.Add(1)
	h.mu.Lock()
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	h.mu.Unlock()
	base := "/api/models/" + testSource.Repo
	switch {
	case r.URL.Path == base+"/refs":
		raw, _ := json.Marshal(map[string]any{"branches": h.branches, "tags": h.tags})
		_, _ = w.Write(raw)
	case strings.HasPrefix(r.URL.Path, base+"/commits/"):
		history := h.histories[strings.TrimPrefix(r.URL.Path, base+"/commits/")]
		h.page(w, r, len(history), func(start, end int) any {
			commits := []map[string]string{}
			for _, id := range history[start:end] {
				commits = append(commits, map[string]string{"id": id})
			}
			return commits
		})
	case r.URL.Path == base+"/tree/"+testSource.Revision && r.URL.Query().Get("recursive") == "true":
		h.tree(w, r)
	default:
		h.strays.Add(1)
		http.NotFound(w, r)
	}
}

func (h *hub) tree(w http.ResponseWriter, r *http.Request) {
	if h.stalled != nil {
		close(h.stalled)
		<-r.Context().Done()
		return
	}
	if h.body != "" || h.status != http.StatusOK {
		if h.redirect {
			w.Header().Set("Location", "/elsewhere")
		}
		if h.errorCode != "" {
			w.Header().Set("X-Error-Code", h.errorCode)
		}
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(h.body))
		return
	}
	h.page(w, r, len(h.entries), func(start, end int) any {
		return append([]UpstreamFile{{Type: "directory", Path: "dir", OID: strings.Repeat("0", 40)}}, h.entries[start:end]...)
	})
}

func (h *hub) page(w http.ResponseWriter, r *http.Request, total int, items func(start, end int) any) {
	start := 0
	fmt.Sscan(r.URL.Query().Get("cursor"), &start)
	end := min(start+2, total)
	if end < total {
		next := fmt.Sprintf("<%s%s?recursive=true&cursor=%d>; rel=\"next\"", h.server.URL, r.URL.Path, end)
		if h.link != "" && strings.Contains(r.URL.Path, "/tree/") {
			next = h.link
		}
		w.Header().Set("Link", next)
	}
	raw, _ := json.Marshal(items(start, end))
	_, _ = w.Write(raw)
}

func (h *hub) checker() Checker {
	return Checker{Client: h.server.Client(), Hub: h.server.URL, Version: "test-1", Now: testNow}
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
	h := newHub(t, append(upstreamOf(files), UpstreamFile{Type: "file", Path: "extra.md", OID: blobID("extra"), Size: 5}))
	record := run(t, h.checker(), root)
	want := []Check{{CheckAllowlist, OutcomePassed, ""}, {CheckIdentity, OutcomePassed, ""}, {CheckTemplate, OutcomeNotRun, ""}, {CheckBehavior, OutcomeNotRun, ""}}
	if !slices.Equal(record.Checks, want) || record.Status != StatusNotScanned || record.Coverage != CoveragePartial {
		t.Fatalf("checks = %+v, status %s, coverage %s", record.Checks, record.Status, record.Coverage)
	}
	if len(record.Files) != len(files) || record.Files["config.json"] != sha256Hex(files["config.json"]) {
		t.Fatal("record must bind the SHA-256 of every model file")
	}
	if record.CheckedAt != testNow() || record.Checker != "test-1" || record.Configuration != currentConfiguration(h.server.URL) || h.requests.Load() < 4 {
		t.Fatalf("unexpected record metadata or paging: %+v, %d requests", record, h.requests.Load())
	}
}

func TestAllowlistRejectsFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		file, content string
		reason        string
	}{
		"python code":          {"modeling.py", "import os", ReasonCodeOrPickleFile},
		"pickle weights":       {"pytorch_model.bin", "pickle", ReasonCodeOrPickleFile},
		"torch weights":        {"model.pt", "pickle", ReasonCodeOrPickleFile},
		"torch state":          {"model.pth", "pickle", ReasonCodeOrPickleFile},
		"pickle":               {"model.pkl", "pickle", ReasonCodeOrPickleFile},
		"checkpoint":           {"model.ckpt", "pickle", ReasonCodeOrPickleFile},
		"unlisted file":        {"notes.txt", "text", ReasonUnlistedFile},
		"nested file":          {"original/config.json", "{}", ReasonNestedFile},
		"other cache file":     {".cache/other/file", "x", ReasonUnlistedFile},
		"adapter redirect":     {"adapter_config.json", `{"base_model_name_or_path":"other/base"}`, ReasonAdapterRedirect},
		"auto_map":             {"config.json", `{"auto_map":{"AutoModel":"x.Model"}}`, ReasonRemoteCode},
		"nested auto_map":      {"tokenizer_config.json", `{"a":[{"b":{"auto_map":1}}]}`, ReasonRemoteCode},
		"auto_mapping":         {"generation_config.json", `{"auto_mapping":{}}`, ReasonRemoteCode},
		"custom_pipelines":     {"config.json", `{"custom_pipelines":{"x":{}}}`, ReasonRemoteCode},
		"duplicate JSON key":   {"generation_config.json", `{"a":1,"a":2}`, ReasonInvalidJSON},
		"trailing value":       {"generation_config.json", `{}{}`, ReasonInvalidJSON},
		"malformed JSON":       {"generation_config.json", `{"a":`, ReasonInvalidJSON},
		"speculators config":   {"config.json", `{"speculators_config":{"verifier":{"name_or_path":"other/model"}}}`, ReasonModelRedirect},
		"tokenizer file path":  {"tokenizer_config.json", `{"tokenizer_file":"/elsewhere/tokenizer.json"}`, ReasonFilePathRedirect},
		"absent vocab file":    {"tokenizer_config.json", `{"vocab_file":"absent.json"}`, ReasonFilePathRedirect},
		"numeric merges file":  {"tokenizer_config.json", `{"merges_file":1}`, ReasonFilePathRedirect},
		"near-miss license":    {"LICENSE.txt", "text", ReasonUnlistedFile},
		"near-miss readme":     {"readme.md", "text", ReasonUnlistedFile},
		"other readme":         {"README.rst", "text", ReasonUnlistedFile},
		"license variant":      {"LICENSE-APACHE", "text", ReasonUnlistedFile},
		"fast tokenizer files": {"tokenizer_config.json", `{"fast_tokenizer_files":["x.json"]}`, ReasonFilePathRedirect},
		"weights override":     {"config.json", `{"transformers_weights":"other.safetensors"}`, ReasonFilePathRedirect},
		"configuration files":  {"config.json", `{"configuration_files":["other.json"]}`, ReasonModelRedirect},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			files[tc.file] = tc.content
			root := writeModel(t, files)
			h := newHub(t, upstreamOf(files))
			record := run(t, h.checker(), root)
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, tc.reason}) || record.Status != StatusFindings {
				t.Fatalf("allowlist = %+v, status %s", record.Checks[0], record.Status)
			}
			if record.Checks[1].Outcome != OutcomeNotRun || h.requests.Load() != 0 {
				t.Fatal("a failed check must stop the later checks")
			}
		})
	}
}

func TestAllowlistAcceptsLargeValidJSONAndUnloadedFiles(t *testing.T) {
	files := modelFiles()
	files["tokenizer.json"] = `{"a":"` + strings.Repeat("x", 2<<20) + `"}`
	for _, name := range []string{"README", "README.md", "LICENSE", "LICENSE.md", ".gitattributes"} {
		files[name] = "text"
	}
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed || record.Checks[1].Outcome != OutcomePassed || len(record.Files) != len(files) {
		t.Fatalf("checks = %+v", record.Checks)
	}
}

func TestAllowlistNeedsASafetensorsModel(t *testing.T) {
	for name, tc := range map[string]struct{ remove, reason string }{
		"no weights":       {".safetensors", ReasonNoWeights},
		"no configuration": {"config.json", ReasonNoConfiguration},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			for file := range files {
				if strings.HasSuffix(file, tc.remove) || file == "model.safetensors.index.json" {
					delete(files, file)
				}
			}
			record := run(t, newHub(t, nil).checker(), writeModel(t, files))
			if record.Status != StatusUnsupported || record.Checks[0] != (Check{CheckAllowlist, OutcomeUnsupported, tc.reason}) {
				t.Fatalf("allowlist = %+v", record.Checks[0])
			}
		})
	}
}

func TestInventoryRejectionsAreStored(t *testing.T) {
	for name, tc := range map[string]struct {
		change  func(t *testing.T, root string)
		outcome Outcome
		reason  string
	}{
		"symbolic link": {func(t *testing.T, root string) {
			if err := os.Symlink("config.json", filepath.Join(root, "linked.json")); err != nil {
				t.Fatal(err)
			}
		}, OutcomeFailed, ReasonLinkOrNonRegular},
		"shard index names a missing shard": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "model.safetensors.index.json"), `{"weight_map":{"a":"absent.safetensors"}}`)
		}, OutcomeFailed, ReasonInvalidShard},
		"shard index names other weights": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "model.safetensors.index.json"), `{"weight_map":{"a":"config.json"}}`)
		}, OutcomeFailed, ReasonInvalidShard},
		"shard index above the JSON limit": {func(t *testing.T, root string) {
			write(t, filepath.Join(root, "model.safetensors.index.json"), `{"weight_map":{"a":"model-00001-of-00002.safetensors"},"m":"`+strings.Repeat("x", MaxJSONFileBytes)+`"}`)
		}, OutcomeUnsupported, ReasonShardTooLarge},
		"too many files": {func(t *testing.T, root string) {
			for i := 0; i <= MaxModelFiles; i++ {
				write(t, filepath.Join(root, fmt.Sprintf("extra-%04d.safetensors", i)), "w")
			}
		}, OutcomeUnsupported, ReasonTooManyFiles},
	} {
		t.Run(name, func(t *testing.T) {
			root := writeModel(t, modelFiles())
			tc.change(t, root)
			h := newHub(t, nil)
			record := run(t, h.checker(), root)
			if record.Checks[0] != (Check{CheckAllowlist, tc.outcome, tc.reason}) || h.requests.Load() != 0 {
				t.Fatalf("allowlist = %+v", record.Checks[0])
			}
			if _, err := MarshalRecord(record); err != nil {
				t.Fatal("an inventory rejection must be a storable record", err)
			}
		})
	}
}

func TestLargeShardIndexIsAccepted(t *testing.T) {
	files := modelFiles()
	files["model.safetensors.index.json"] = `{"metadata":{"pad":"` + strings.Repeat("x", 2<<20) + `"},"weight_map":{"a":"model-00001-of-00002.safetensors"}}`
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("an index above 1 MiB must pass: %+v", record.Checks[0])
	}
}

func TestDownloadToolMetadataIsSkipped(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	write(t, filepath.Join(root, ".cache", "huggingface", ".gitignore"), "*")
	for name := range files {
		write(t, filepath.Join(root, ".cache", "huggingface", "download", name+".metadata"), "commit\netag\n")
	}
	write(t, filepath.Join(root, ".cache", "huggingface", "download", "config.json.lock"), "")
	record := run(t, newHub(t, upstreamOf(files)).checker(), root)
	if record.Checks[0].Outcome != OutcomePassed || record.Checks[1].Outcome != OutcomePassed || len(record.Files) != len(files) {
		t.Fatalf("the download metadata must be skipped and never bound: %+v", record)
	}
	for name, path := range map[string]string{
		"code in download": filepath.Join(".cache", "huggingface", "download", "evil.py"),
		"nested download":  filepath.Join(".cache", "huggingface", "download", "sub", "x.metadata"),
		"other tool cache": filepath.Join(".cache", "torch", "x"),
		"other file":       filepath.Join(".cache", "huggingface", "token"),
	} {
		t.Run(name, func(t *testing.T) {
			other := writeModel(t, files)
			write(t, filepath.Join(other, path), "x")
			record := run(t, newHub(t, upstreamOf(files)).checker(), other)
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonUnlistedFile}) {
				t.Fatalf("allowlist = %+v", record.Checks[0])
			}
		})
	}
}

func TestChecksAreDeterministic(t *testing.T) {
	files := modelFiles()
	files["a.py"], files["b.bin"], files["notes.txt"] = "x", "x", "x"
	files["config.json"] = `{"auto_map":{}}`
	root := writeModel(t, files)
	h := newHub(t, upstreamOf(files))
	first := run(t, h.checker(), root)
	for i := 0; i < 20; i++ {
		if again := run(t, h.checker(), root); !slices.Equal(again.Checks, first.Checks) {
			t.Fatalf("run %d gave %+v, first %+v", i, again.Checks, first.Checks)
		}
	}
	if first.Checks[0].Reason != ReasonCodeOrPickleFile {
		t.Fatalf("the first problem in name order must win: %+v", first.Checks[0])
	}
}

func TestIdentityMismatchIsAFinding(t *testing.T) {
	setSize := func(path string, size int64) func(*testing.T, string, []UpstreamFile) []UpstreamFile {
		return func(_ *testing.T, _ string, entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].Path == path {
					entries[i].Size = size
				}
			}
			return entries
		}
	}
	for name, tc := range map[string]struct {
		change func(t *testing.T, root string, entries []UpstreamFile) []UpstreamFile
		reason string
	}{
		"changed weights": {func(_ *testing.T, _ string, entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].LFS != nil {
					entries[i].LFS.OID = sha256Hex("other weights")
				}
			}
			return entries
		}, ReasonIdentityMismatch},
		"changed small file": {func(_ *testing.T, _ string, entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].Path == "chat_template.jinja" {
					entries[i].OID = blobID("{{ poisoned }}")
				}
			}
			return entries
		}, ReasonIdentityMismatch},
		"other size":                {setSize("config.json", int64(len(modelFiles()["config.json"])+1)), ReasonIdentityMismatch},
		"upstream blob above limit": {setSize("tokenizer.json", MaxGitBlobBytes+1), ReasonIdentityMismatch},
		"local file larger than blob": {func(t *testing.T, root string, entries []UpstreamFile) []UpstreamFile {
			write(t, filepath.Join(root, "tokenizer.json"), `{"model":{"vocab":{"a":1,"b":2}}}`)
			return entries
		}, ReasonIdentityMismatch},
		"file not upstream": {func(_ *testing.T, _ string, entries []UpstreamFile) []UpstreamFile {
			for i := range entries {
				if entries[i].Path == "generation_config.json" {
					return append(entries[:i], entries[i+1:]...)
				}
			}
			return entries
		}, ReasonNotUpstream},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			root := writeModel(t, files)
			entries := tc.change(t, root, upstreamOf(files))
			record := run(t, newHub(t, entries).checker(), root)
			if record.Status != StatusFindings || record.Checks[1] != (Check{CheckIdentity, OutcomeFailed, tc.reason}) || record.Checks[2].Outcome != OutcomeNotRun {
				t.Fatalf("identity = %+v", record.Checks[1])
			}
		})
	}
}

func TestIdentityRefusesBlobsAboveTheLimitBeforeReading(t *testing.T) {
	original := maxGitBlobBytes
	maxGitBlobBytes = 4
	t.Cleanup(func() { maxGitBlobBytes = original })
	content := "a matching blob above the limit"
	root := t.TempDir()
	write(t, filepath.Join(root, "notes.json"), content)
	upstream := map[string]UpstreamFile{"notes.json": {Type: "file", Path: "notes.json", OID: blobID(content), Size: int64(len(content))}}
	if err := os.Remove(filepath.Join(root, "notes.json")); err != nil {
		t.Fatal(err)
	}
	reason, err := checkIdentity(context.Background(), root, map[string]string{"notes.json": sha256Hex(content)}, upstream)
	if reason != ReasonIdentityMismatch || err != nil {
		t.Fatalf("a matching blob above the limit = %q, %v; want a mismatch without a read", reason, err)
	}
}

func TestUpstreamFailuresAreErrors(t *testing.T) {
	zero := strings.Repeat("0", 40)
	for name, tc := range map[string]struct {
		change func(h *hub)
		reason string
	}{
		"not found":     {func(h *hub) { h.status = http.StatusNotFound }, ReasonUpstreamNotFound},
		"denied":        {func(h *hub) { h.status = http.StatusForbidden }, ReasonUpstreamDenied},
		"server error":  {func(h *hub) { h.status = http.StatusBadGateway }, ReasonUpstreamUnreachable},
		"redirect":      {func(h *hub) { h.status = http.StatusFound; h.redirect = true }, ReasonUpstreamMoved},
		"rate limited":  {func(h *hub) { h.status = http.StatusTooManyRequests }, ReasonUpstreamUnreachable},
		"bad request":   {func(h *hub) { h.status = http.StatusBadRequest }, ReasonUpstreamInvalid},
		"malformed":     {func(h *hub) { h.body = `[{"type":` }, ReasonUpstreamInvalid},
		"invalid entry": {func(h *hub) { h.body = `[{"type":"file","path":"../x","oid":"` + zero + `","size":1}]` }, ReasonUpstreamInvalid},
		"invalid LFS digest": {func(h *hub) {
			h.body = `[{"type":"file","path":"x","oid":"` + zero + `","size":1,"lfs":{"oid":"zz","size":1}}]`
		}, ReasonUpstreamInvalid},
		"duplicate path":      {func(h *hub) { h.entries = append(h.entries, h.entries[0], h.entries[0]) }, ReasonUpstreamInvalid},
		"oversized page":      {func(h *hub) { h.body = "[" + strings.Repeat(" ", maxListingPageBytes) + "]" }, ReasonUpstreamInvalid},
		"unknown entry type":  {func(h *hub) { h.body = `[{"type":"symlink","path":"x","oid":"` + zero + `","size":1}]` }, ReasonUpstreamInvalid},
		"negative entry size": {func(h *hub) { h.body = `[{"type":"file","path":"x","oid":"` + zero + `","size":-1}]` }, ReasonUpstreamInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			h := newHub(t, upstreamOf(files))
			tc.change(h)
			record := run(t, h.checker(), writeModel(t, files))
			if record.Status != StatusError || record.Checks[1] != (Check{CheckIdentity, OutcomeError, tc.reason}) {
				t.Fatalf("identity = %+v", record.Checks[1])
			}
			if h.strays.Load() != 0 {
				t.Fatal("the listing followed a redirect")
			}
		})
	}
}

// trustBoth gives a client that trusts both TLS test servers, so a refused
// link fails because of the same-origin rule, not because of TLS.
func trustBoth(a, b *httptest.Server) *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(a.Certificate())
	pool.AddCert(b.Certificate())
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
}

func TestNextLinksStayOnTheSameTree(t *testing.T) {
	files := modelFiles()
	entries := upstreamOf(files)
	other := newHub(t, entries)
	tree := "/api/models/" + testSource.Repo + "/tree/" + testSource.Revision
	for name, link := range map[string]string{
		"other host":   "<" + other.server.URL + tree + "?recursive=true&cursor=2>; rel=\"next\"",
		"other path":   "<%s/api/models/" + testSource.Repo + "/tree/main?recursive=true&cursor=2>; rel=\"next\"",
		"other scheme": "<http://%h" + tree + "?recursive=true&cursor=2>; rel=\"next\"",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHub(t, entries)
			h.link = strings.NewReplacer("%s", h.server.URL, "%h", h.server.Listener.Addr().String()).Replace(link)
			before := other.requests.Load()
			_, err := HubListing(trustBoth(h.server, other.server), h.server.URL)(context.Background(), testSource)
			// The hub must see only the refs and the first tree page.
			if !errors.Is(err, upstreamError{reason: ReasonUpstreamInvalid}) || other.requests.Load() != before || h.requests.Load() != 2 || h.strays.Load() != 0 {
				t.Fatalf("a next link to another location was followed or misreported: %v", err)
			}
		})
	}
	h := newHub(t, entries)
	if listing, err := HubListing(trustBoth(h.server, other.server), h.server.URL)(context.Background(), testSource); err != nil || len(listing) != len(entries) {
		t.Fatal("same-tree next links must be followed", err)
	}
}

func TestHubListingNeedsHTTPS(t *testing.T) {
	files := modelFiles()
	h := newHub(t, upstreamOf(files))
	plain := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(plain.Close)
	if _, err := HubListing(plain.Client(), plain.URL)(context.Background(), testSource); err == nil || h.requests.Load() != 0 {
		t.Fatal("a plain HTTP hub was used")
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
	if _, err := HubListing(h.server.Client(), "https://127.0.0.1:1")(context.Background(), testSource); !errors.Is(err, upstreamError{reason: ReasonUpstreamUnreachable}) {
		t.Fatal("an unreachable hub must report unreachable", err)
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
		{Model: "example", Root: filepath.Join(root, "absent"), Source: testSource},
	} {
		if _, err := h.checker().Run(context.Background(), request); err == nil {
			t.Fatalf("request %+v was accepted", request)
		}
	}
	for _, checker := range []Checker{{Hub: h.server.URL, Version: "test-1"}, {Client: h.server.Client(), Hub: "http://hub", Version: "test-1"}, {Client: h.server.Client(), Hub: h.server.URL}} {
		if _, err := checker.Run(context.Background(), Request{Model: "example", Root: root, Source: testSource}); err == nil {
			t.Fatalf("checker %+v was accepted", checker)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.checker().Run(ctx, Request{Model: "example", Root: root, Source: testSource}); err == nil {
		t.Fatal("a canceled check gave a result; the model must stay not scanned")
	}
	if _, _, err := Inventory(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatal("a canceled inventory must stop before hashing", err)
	}
}

func TestFileChangeResetsToNotScanned(t *testing.T) {
	ctx := context.Background()
	files := modelFiles()
	root := writeModel(t, files)
	checker := newHub(t, upstreamOf(files)).checker()
	record := run(t, checker, root)
	record.Checks[2].Outcome, record.Checks[3].Outcome = OutcomePassed, OutcomePassed
	record.Status, record.Coverage = statusOf(record.Checks), coverageOf(record.Checks)
	if record.Status != StatusPassed || record.Coverage != CoverageComplete {
		t.Fatal("all checks passing must give passed and complete coverage")
	}
	if Current(ctx, &record, "example", root, testSource, checker) != StatusPassed {
		t.Fatal("an unchanged model must keep its status")
	}
	other := testSource
	other.Revision = strings.Repeat("b", 40)
	newer, otherHub := checker, checker
	newer.Version, otherHub.Hub = "test-2", "https://mirror.invalid"
	for name, status := range map[string]Status{
		"other model":   Current(ctx, &record, "other", root, testSource, checker),
		"other root":    Current(ctx, &record, "example", root+"2", testSource, checker),
		"other source":  Current(ctx, &record, "example", root, other, checker),
		"other checker": Current(ctx, &record, "example", root, testSource, newer),
		"other hub":     Current(ctx, &record, "example", root, testSource, otherHub),
		"no record":     Current(ctx, nil, "example", root, testSource, checker),
	} {
		if status != StatusNotScanned {
			t.Fatalf("%s: status = %s, want not_scanned", name, status)
		}
	}
	if err := os.Symlink("config.json", filepath.Join(root, "zz.safetensors")); err != nil {
		t.Fatal(err)
	}
	if got := Current(ctx, &record, "example", root, testSource, checker); got != StatusNotScanned {
		t.Fatalf("a late-sorting link must not keep the old status: %s", got)
	}
	if err := os.Remove(filepath.Join(root, "zz.safetensors")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "generation_config.json"), `{"changed":true}`)
	if got := Current(ctx, &record, "example", root, testSource, checker); got != StatusNotScanned {
		t.Fatalf("a changed file must reset the status: %s", got)
	}
	linked := writeModel(t, files)
	if err := os.Symlink("config.json", filepath.Join(linked, "zz.safetensors")); err != nil {
		t.Fatal(err)
	}
	finding := run(t, checker, linked)
	if finding.Status != StatusFindings || Current(ctx, &finding, "example", linked, testSource, checker) != StatusFindings {
		t.Fatal("a stored inventory finding must still apply to the same files")
	}
}

func TestFilesChangedDuringTheCheckAreErrors(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	digests, _, err := Inventory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "config.json"), `{"architectures":["Other"]}`)
	reason, err := checkAllowlist(context.Background(), root, digests)
	if got := outcome(CheckAllowlist, reason, err); got.Reason != "files-changed" || got.Outcome != OutcomeError {
		t.Fatalf("allowlist after change = %+v", got)
	}
	upstream := map[string]UpstreamFile{}
	for _, entry := range upstreamOf(files) {
		upstream[entry.Path] = entry
	}
	upstream["config.json"] = UpstreamFile{Type: "file", Path: "config.json", OID: blobID(`{"architectures":["Other"]}`), Size: int64(len(`{"architectures":["Other"]}`))}
	only := map[string]string{"config.json": digests["config.json"]}
	reason, err = checkIdentity(context.Background(), root, only, upstream)
	if got := outcome(CheckIdentity, reason, err); got.Reason != "files-changed" {
		t.Fatalf("identity after change = %+v", got)
	}
	if err := os.Remove(filepath.Join(root, "config.json")); err != nil {
		t.Fatal(err)
	}
	reason, err = checkIdentity(context.Background(), root, only, upstream)
	if got := outcome(CheckIdentity, reason, err); got.Reason != "unreadable" {
		t.Fatalf("identity after removal = %+v", got)
	}
	write(t, filepath.Join(root, "model.safetensors.index.json"), indexJSON+" ")
	if _, err := checkShardIndexes(context.Background(), root, digests); !errors.Is(err, errChanged) {
		t.Fatal("a shard index changed after the inventory must be an error", err)
	}
}

func TestOversizedJSONIsUnsupported(t *testing.T) {
	files := modelFiles()
	files["tokenizer.json"] = `{"a":"` + strings.Repeat("x", MaxJSONFileBytes) + `"}`
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0] != (Check{CheckAllowlist, OutcomeUnsupported, ReasonJSONTooLarge}) || record.Status != StatusUnsupported {
		t.Fatalf("allowlist = %+v", record.Checks[0])
	}
}

func TestFilePathKeysAcceptNullAndInventoriedNames(t *testing.T) {
	files := modelFiles()
	files["tokenizer_config.json"] = `{"tokenizer_file":null,"vocab_file":"vocab.json"}`
	files["vocab.json"] = `{}`
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("allowlist = %+v", record.Checks[0])
	}
}

func TestRevisionMustBeOnABranch(t *testing.T) {
	files := modelFiles()
	other := func(i int) string { return fmt.Sprintf("%040x", i+1) }
	long := make([]string, 2*maxHistoryPages+1)
	for i := range long {
		long[i] = other(i)
	}
	main := hubRef{Name: "main", TargetCommit: other(99)}
	for name, tc := range map[string]struct {
		branches  []hubRef
		tags      []hubRef
		histories map[string][]string
		check     Check
	}{
		"branch head":                {[]hubRef{{Name: "main", TargetCommit: testSource.Revision}}, nil, nil, Check{CheckIdentity, OutcomePassed, ""}},
		"tag head":                   {[]hubRef{main}, []hubRef{{Name: "v1", TargetCommit: testSource.Revision}}, map[string][]string{"main": {other(1)}}, Check{CheckIdentity, OutcomePassed, ""}},
		"in branch history":          {[]hubRef{main}, nil, map[string][]string{"main": {other(0), other(1), testSource.Revision}}, Check{CheckIdentity, OutcomePassed, ""}},
		"in a second branch":         {[]hubRef{main, {Name: "release/1", TargetCommit: other(98)}}, nil, map[string][]string{"main": {other(1)}, "release/1": {testSource.Revision}}, Check{CheckIdentity, OutcomePassed, ""}},
		"only on a pull request ref": {[]hubRef{main}, nil, map[string][]string{"main": {other(0), other(1), other(2)}}, Check{CheckIdentity, OutcomeFailed, ReasonNotOnBranch}},
		"history above the bound":    {[]hubRef{main}, nil, map[string][]string{"main": long}, Check{CheckIdentity, OutcomeError, ReasonHistoryTooLong}},
		"budget across branches":     {[]hubRef{main, {Name: "dev", TargetCommit: other(97)}}, nil, map[string][]string{"main": long[:2*maxHistoryPages-1], "dev": {other(5), testSource.Revision}}, Check{CheckIdentity, OutcomeError, ReasonHistoryTooLong}},
		"no branches":                {[]hubRef{}, nil, nil, Check{CheckIdentity, OutcomeError, ReasonUpstreamInvalid}},
		"branch without a name":      {[]hubRef{{Name: "", TargetCommit: other(1)}}, nil, nil, Check{CheckIdentity, OutcomeError, ReasonUpstreamInvalid}},
		"branch without a target":    {[]hubRef{{Name: "main"}}, nil, nil, Check{CheckIdentity, OutcomeError, ReasonUpstreamInvalid}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHub(t, upstreamOf(files))
			h.branches, h.histories = tc.branches, tc.histories
			if tc.tags != nil {
				h.tags = tc.tags
			}
			record := run(t, h.checker(), writeModel(t, files))
			if record.Checks[1] != tc.check {
				t.Fatalf("identity = %+v, want %+v", record.Checks[1], tc.check)
			}
		})
	}
}

func TestMissingRepositoryWithoutTokenIsNotFound(t *testing.T) {
	files := modelFiles()
	for _, code := range []string{"RepoNotFound", "RevisionNotFound"} {
		h := newHub(t, upstreamOf(files))
		h.status, h.errorCode = http.StatusUnauthorized, code
		record := run(t, h.checker(), writeModel(t, files))
		if record.Checks[1] != (Check{CheckIdentity, OutcomeError, ReasonUpstreamNotFound}) {
			t.Fatalf("%s: identity = %+v", code, record.Checks[1])
		}
	}
}

func TestHubLoginTokenGoesOnlyToTheHub(t *testing.T) {
	files := modelFiles()
	home := t.TempDir()
	write(t, filepath.Join(home, "token"), "hf_secrettoken\n")
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", "")
	t.Setenv("HF_HOME", home)
	h := newHub(t, upstreamOf(files))
	record := run(t, h.checker(), writeModel(t, files))
	raw, err := MarshalRecord(record)
	if err != nil || strings.Contains(string(raw), "secret") || record.Checks[1].Outcome != OutcomePassed {
		t.Fatal("the token must reach the hub and never the record", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.auth) < 2 {
		t.Fatal("the hub got no requests")
	}
	for _, auth := range h.auth {
		if auth != "Bearer hf_secrettoken" {
			t.Fatalf("request authorization = %q", auth)
		}
	}
}

func TestHubTokenLookupOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "path-token"), "hf_path")
	write(t, filepath.Join(dir, "home", "token"), "hf_home")
	write(t, filepath.Join(dir, "cache", "huggingface", "token"), "hf_cache")
	write(t, filepath.Join(dir, "large"), strings.Repeat("a", maxTokenBytes+1))
	write(t, filepath.Join(dir, "spaced"), "hf bad")
	write(t, filepath.Join(dir, "user", ".cache", "huggingface", "token"), "hf_default")
	if err := os.Symlink(filepath.Join(dir, "path-token"), filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		env   map[string]string
		token string
		err   bool
	}{
		"HF_TOKEN first":            {map[string]string{"HF_TOKEN": "hf_env", "HF_TOKEN_PATH": filepath.Join(dir, "path-token")}, "hf_env", false},
		"unusable HF_TOKEN":         {map[string]string{"HF_TOKEN": "hf bad", "HF_HOME": filepath.Join(dir, "home")}, "", true},
		"HF_TOKEN_PATH second":      {map[string]string{"HF_TOKEN_PATH": filepath.Join(dir, "path-token"), "HF_HOME": filepath.Join(dir, "home")}, "hf_path", false},
		"relative HF_TOKEN_PATH":    {map[string]string{"HF_TOKEN_PATH": "path-token"}, "", true},
		"missing HF_TOKEN_PATH":     {map[string]string{"HF_TOKEN_PATH": filepath.Join(dir, "absent")}, "", true},
		"linked HF_TOKEN_PATH":      {map[string]string{"HF_TOKEN_PATH": filepath.Join(dir, "linked")}, "", true},
		"token above the limit":     {map[string]string{"HF_TOKEN_PATH": filepath.Join(dir, "large")}, "", true},
		"token with a space":        {map[string]string{"HF_TOKEN_PATH": filepath.Join(dir, "spaced")}, "", true},
		"HF_HOME third":             {map[string]string{"HF_HOME": filepath.Join(dir, "home"), "XDG_CACHE_HOME": filepath.Join(dir, "cache")}, "hf_home", false},
		"HF_HOME without a login":   {map[string]string{"HF_HOME": filepath.Join(dir, "absent"), "XDG_CACHE_HOME": filepath.Join(dir, "cache")}, "", false},
		"relative HF_HOME":          {map[string]string{"HF_HOME": "home"}, "", true},
		"cache fourth":              {map[string]string{"XDG_CACHE_HOME": filepath.Join(dir, "cache")}, "hf_cache", false},
		"default cache under HOME":  {map[string]string{"XDG_CACHE_HOME": "relative", "HOME": filepath.Join(dir, "user")}, "hf_default", false},
		"no default cache file":     {map[string]string{"XDG_CACHE_HOME": "", "HOME": dir}, "", false},
		"no login in the cache dir": {map[string]string{"XDG_CACHE_HOME": filepath.Join(dir, "absent")}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"HF_TOKEN", "HF_TOKEN_PATH", "HF_HOME", "XDG_CACHE_HOME"} {
				t.Setenv(key, "")
			}
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			token, err := hubToken()
			if token != tc.token || (err != nil) != tc.err {
				t.Fatalf("token = %q, err = %v", token, err)
			}
			if err != nil && (strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "hf_")) {
				t.Fatal("the error must hold no path or token")
			}
		})
	}
}

func TestUnusableTokenIsAnError(t *testing.T) {
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", "relative")
	files := modelFiles()
	h := newHub(t, upstreamOf(files))
	record := run(t, h.checker(), writeModel(t, files))
	if record.Checks[1] != (Check{CheckIdentity, OutcomeError, ReasonTokenUnusable}) || h.requests.Load() != 0 {
		t.Fatalf("identity = %+v; an unusable token must not fall back to an anonymous request", record.Checks[1])
	}
}

func TestListingHasADefaultDeadline(t *testing.T) {
	original := listingTimeout
	listingTimeout = 200 * time.Millisecond
	t.Cleanup(func() { listingTimeout = original })
	files := modelFiles()
	h := newHub(t, upstreamOf(files))
	h.stalled = make(chan struct{})
	checker := h.checker()
	checker.Client = &http.Client{Transport: h.server.Client().Transport}
	start := time.Now()
	record := run(t, checker, writeModel(t, files))
	if record.Checks[1] != (Check{CheckIdentity, OutcomeError, ReasonUpstreamUnreachable}) || time.Since(start) > 10*time.Second {
		t.Fatalf("a stalled hub must time out as unreachable: %+v", record.Checks[1])
	}
}

func TestCancelDuringListingStoresNoResult(t *testing.T) {
	files := modelFiles()
	h := newHub(t, upstreamOf(files))
	h.stalled = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-h.stalled
		cancel()
	}()
	record, err := h.checker().Run(ctx, Request{Model: "example", Root: writeModel(t, files), Source: testSource})
	if err == nil || record.Model != "" {
		t.Fatalf("a check canceled during the listing gave a result: %+v", record)
	}
}

func TestUnreadableShardIndexIsAnError(t *testing.T) {
	root := writeModel(t, modelFiles())
	digests, _, err := Inventory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(root, "model.safetensors.index.json")
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config.json", index); err != nil {
		t.Fatal(err)
	}
	if _, err := checkShardIndexes(context.Background(), root, digests); !errors.Is(err, errUnreadable) {
		t.Fatal("an index that becomes a link must be an error, not a finding", err)
	}
}

func TestRealModelConfigurationsPass(t *testing.T) {
	models, err := os.ReadDir("testdata/shapes")
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if !model.IsDir() {
			continue
		}
		t.Run(model.Name(), func(t *testing.T) {
			files := map[string]string{"model.safetensors": "weights"}
			entries, err := os.ReadDir(filepath.Join("testdata/shapes", model.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				raw, err := os.ReadFile(filepath.Join("testdata/shapes", model.Name(), entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				files[entry.Name()] = string(raw)
			}
			record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
			if record.Checks[0].Outcome != OutcomePassed || record.Checks[1].Outcome != OutcomePassed {
				t.Fatalf("a real configuration was rejected: %+v", record.Checks[:2])
			}
		})
	}
}

func TestPathValuesAreRejectedAtAnyDepth(t *testing.T) {
	for name, tc := range map[string]struct{ file, content string }{
		"marian source spm":     {"tokenizer_config.json", `{"source_spm":"/etc/model.spm","target_spm":"target.spm"}`},
		"nested absolute path":  {"config.json", `{"text_config":{"weights":"/srv/other.safetensors"}}`},
		"parent path":           {"generation_config.json", `{"a":["x","../other/config.json"]}`},
		"home path":             {"processor_config.json", `{"chat":"~/template.jinja"}`},
		"relative dot path":     {"preprocessor_config.json", `{"file":"./x"}`},
		"backslash path":        {"config.json", `{"file":"\\\\host\\share"}`},
		"unbound file name":     {"config.json", `{"tokenizer":"other.model"}`},
		"file in a folder":      {"config.json", `{"weights":"sub/model.safetensors"}`},
		"top level vocab value": {"tokenizer_config.json", `{"vocab":"absent.json"}`},
		"documentation file":    {"tokenizer_config.json", `{"tokenizer_file":"README.md"}`},
		"documentation value":   {"config.json", `{"notes":"LICENSE.md"}`},
		"top level path object": {"tokenizer_config.json", `{"vocab_file":{"a":1}}`},
		"license in a value":    {"config.json", `{"notes":{"text":"LICENSE"}}`},
		"path list":             {"config.json", `{"files":["ok","../x"]}`},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			// The documentation files are present, so only the rule for
			// unloaded files can reject a value that names them.
			files["README.md"], files["LICENSE.md"] = "text", "text"
			files[tc.file] = tc.content
			record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonFilePathRedirect}) {
				t.Fatalf("allowlist = %+v", record.Checks[0])
			}
		})
	}
	files := modelFiles()
	files["tokenizer_config.json"] = `{"source_spm":"source.spm","name_or_path":"/home/user/model","_name_or_path":"org/name","chat_template":"{{ '../x' }}","eos_token":"..."}`
	files["source.spm"] = "spm"
	delete(files, "tokenizer.json")
	files["tokenizer.json"] = `{"model":{"merges":["Ġ ..","/ ~"]}}`
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomeFailed || record.Checks[0].Reason != ReasonUnlistedFile {
		t.Fatalf("source.spm is not on the allowlist, so it must fail as unlisted: %+v", record.Checks[0])
	}
	delete(files, "source.spm")
	files["tokenizer_config.json"] = `{"vocab_file":"vocab.json","name_or_path":"/home/user/model","_name_or_path":"org/name","chat_template":[{"name":"default","template":"{{ '../x' }}"}],"eos_token":"...","additional_special_tokens":["...","~x","/ok"]}`
	files["vocab.json"] = `{}`
	record = run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("text keys, bound names and token data must pass: %+v", record.Checks[0])
	}
}

func TestEverySharedLoaderKeyIsRejected(t *testing.T) {
	for reason, keys := range map[string][]string{
		ReasonRemoteCode:       llmtraining.RemoteCodeKeys,
		ReasonModelRedirect:    llmtraining.ModelRedirectKeys,
		ReasonFilePathRedirect: llmtraining.FileRedirectKeys,
	} {
		for _, key := range keys {
			t.Run(key, func(t *testing.T) {
				files := modelFiles()
				files["generation_config.json"] = `{"outer":[{"` + key + `":{}}]}`
				record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
				if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, reason}) {
					t.Fatalf("nested %s: allowlist = %+v", key, record.Checks[0])
				}
			})
		}
	}
	for _, key := range llmtraining.FilePathKeys {
		t.Run(key, func(t *testing.T) {
			files := modelFiles()
			files["tokenizer_config.json"] = `{"` + key + `":"absent-name"}`
			record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonFilePathRedirect}) {
				t.Fatalf("%s: allowlist = %+v", key, record.Checks[0])
			}
		})
	}
}

func TestSizeDifferenceIsAMismatchBeforeAnyRead(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	upstream := map[string]UpstreamFile{}
	for _, entry := range upstreamOf(files) {
		upstream[entry.Path] = entry
	}
	entry := upstream["config.json"]
	entry.Size++
	upstream["config.json"] = entry
	// The digest is wrong on purpose: a size difference returns before any read.
	reason, err := checkIdentity(context.Background(), root, map[string]string{"config.json": strings.Repeat("0", 64)}, upstream)
	if reason != ReasonIdentityMismatch || err != nil {
		t.Fatalf("a size difference = %q, %v; want a mismatch", reason, err)
	}
}

func TestInvalidUTF8IsInvalidJSON(t *testing.T) {
	files := modelFiles()
	files["generation_config.json"] = "{\"a\":\"\xff\"}"
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonInvalidJSON}) {
		t.Fatalf("allowlist = %+v", record.Checks[0])
	}
}

func TestTokenDataIsNotReadAsConfigurationKeys(t *testing.T) {
	files := modelFiles()
	files["tokenizer.json"] = `{"model":{"type":"BPE","vocab":{"auto_map":1,"__dict__":2,"speculators_config":3},"merges":["auto_map x"]},` +
		`"post_processor":{"special_tokens":{"custom_pipelines":{"id":"custom_pipelines","ids":[4],"tokens":["custom_pipelines"]}}},` +
		`"added_tokens":[{"id":5,"content":"auto_map"}]}`
	files["vocab.json"] = `{"auto_map":1,"__dict__":2,"transformers_weights":3}`
	files["added_tokens.json"] = `{"custom_pipelines":4}`
	files["tokenizer_config.json"] = `{"added_tokens_decoder":{"5":{"content":"auto_map"}}}`
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("tokens that look like configuration keys must pass: %+v", record.Checks[0])
	}
	files["tokenizer.json"] = `{"model":{"type":"BPE","vocab":{}},"auto_map":{}}`
	record = run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonRemoteCode}) {
		t.Fatalf("a configuration key outside token data must still fail: %+v", record.Checks[0])
	}
}

func TestHubModelIDsAndURLsAreRedirects(t *testing.T) {
	for name, tc := range map[string]struct{ file, content string }{
		"ultravox audio model": {"config.json", `{"audio_model_id":"openai/whisper-large-v3"}`},
		"ultravox text model":  {"config.json", `{"text_model_id":"org/llm"}`},
		"repo ID in a value":   {"config.json", `{"text_config":{"backbone":"org/other-model"}}`},
		"URL in a value":       {"generation_config.json", `{"source":"https://example.invalid/weights"}`},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			files[tc.file] = tc.content
			record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonModelRedirect}) {
				t.Fatalf("allowlist = %+v", record.Checks[0])
			}
		})
	}
	files := modelFiles()
	files["config.json"] = `{"_name_or_path":"org/name","architectures":["Qwen3ForCausalLM"],"model_type":"qwen3"}`
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("a label may hold a hub ID: %+v", record.Checks[0])
	}
}

func TestTokenMapsSkipThePathRules(t *testing.T) {
	files := modelFiles()
	files["vocab.json"] = `{"vocab":0,"vocab_file":1,"../x":2,"/":3,"org/name":4}`
	files["added_tokens.json"] = `{"source_spm":5,"auto_map":6}`
	record := run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("token maps must skip the key and value rules: %+v", record.Checks[0])
	}
}

func TestLoadedBinaryFilesNeedAnUpstreamSHA256(t *testing.T) {
	files := modelFiles()
	files["tokenizer.model"] = "binary sentencepiece"
	files["merges.txt"] = "#version: 0.2\nĠ t\n"
	entries := upstreamOf(files)
	record := run(t, newHub(t, entries).checker(), writeModel(t, files))
	if record.Checks[1] != (Check{CheckIdentity, OutcomeFailed, ReasonNoSHA256}) {
		t.Fatalf("a binary file with only a git SHA-1 = %+v", record.Checks[1])
	}
	for i := range entries {
		if entries[i].Path == "tokenizer.model" {
			entries[i].LFS = &LFSEntry{OID: sha256Hex(files["tokenizer.model"]), Size: int64(len(files["tokenizer.model"]))}
		}
	}
	record = run(t, newHub(t, entries).checker(), writeModel(t, files))
	if record.Checks[1].Outcome != OutcomePassed {
		t.Fatalf("an LFS binary and a text file with a git SHA-1 must pass: %+v", record.Checks[1])
	}
	files["merges.txt"] = "a b\x00\x01collision block"
	entries = upstreamOf(files)
	for i := range entries {
		if entries[i].Path == "tokenizer.model" {
			entries[i].LFS = &LFSEntry{OID: sha256Hex(files["tokenizer.model"]), Size: int64(len(files["tokenizer.model"]))}
		}
	}
	record = run(t, newHub(t, entries).checker(), writeModel(t, files))
	if record.Checks[1] != (Check{CheckIdentity, OutcomeFailed, ReasonIdentityMismatch}) {
		t.Fatalf("a text file with control bytes must not match by SHA-1: %+v", record.Checks[1])
	}
}

func TestChecksStopWhenCanceled(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	digests, _, err := Inventory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	upstream := map[string]UpstreamFile{}
	for _, entry := range upstreamOf(files) {
		upstream[entry.Path] = entry
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := checkAllowlist(ctx, root, digests); !errors.Is(err, context.Canceled) {
		t.Fatal("the allowlist must stop when canceled", err)
	}
	if _, err := checkIdentity(ctx, root, digests, upstream); !errors.Is(err, context.Canceled) {
		t.Fatal("the identity check must stop when canceled", err)
	}
	if _, err := checkShardIndexes(ctx, root, digests); !errors.Is(err, context.Canceled) {
		t.Fatal("the shard index check must stop when canceled", err)
	}
	if _, err := checkJSON(ctx, filepath.Join(root, "config.json"), digests); !errors.Is(err, context.Canceled) {
		t.Fatal("a single JSON read must stop when canceled", err)
	}
	if _, err := gitBlobID(ctx, filepath.Join(root, "config.json"), int64(len(files["config.json"])), digests["config.json"], false); !errors.Is(err, context.Canceled) {
		t.Fatal("a single blob read must stop when canceled", err)
	}
}

func TestIdentityNeedsSHA256ForWeightsAndTextForTemplates(t *testing.T) {
	files := modelFiles()
	entries := upstreamOf(files)
	for i := range entries {
		if entries[i].Path == "model-00001-of-00002.safetensors" {
			entries[i].LFS = nil
			entries[i].OID = blobID(files[entries[i].Path])
		}
	}
	record := run(t, newHub(t, entries).checker(), writeModel(t, files))
	if record.Checks[1] != (Check{CheckIdentity, OutcomeFailed, ReasonNoSHA256}) {
		t.Fatalf("weights without an LFS entry = %+v", record.Checks[1])
	}
	files = modelFiles()
	files["chat_template.jinja"] = "{{ messages }}\x07"
	record = run(t, newHub(t, upstreamOf(files)).checker(), writeModel(t, files))
	if record.Checks[1] != (Check{CheckIdentity, OutcomeFailed, ReasonIdentityMismatch}) {
		t.Fatalf("a template with a control byte and a matching blob ID = %+v", record.Checks[1])
	}
}
