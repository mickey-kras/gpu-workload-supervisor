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
	strays   atomic.Int32
	status   int
	redirect bool
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
		h.strays.Add(1)
		http.NotFound(w, r)
		return
	}
	if h.body != "" || h.status != http.StatusOK {
		if h.redirect {
			w.Header().Set("Location", "/elsewhere")
		}
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
		"python code":         {"modeling.py", "import os", ReasonCodeOrPickleFile},
		"pickle weights":      {"pytorch_model.bin", "pickle", ReasonCodeOrPickleFile},
		"torch weights":       {"model.pt", "pickle", ReasonCodeOrPickleFile},
		"torch state":         {"model.pth", "pickle", ReasonCodeOrPickleFile},
		"pickle":              {"model.pkl", "pickle", ReasonCodeOrPickleFile},
		"checkpoint":          {"model.ckpt", "pickle", ReasonCodeOrPickleFile},
		"unlisted file":       {"notes.txt", "text", ReasonUnlistedFile},
		"nested file":         {"original/config.json", "{}", ReasonNestedFile},
		"other cache file":    {".cache/other/file", "x", ReasonUnlistedFile},
		"adapter redirect":    {"adapter_config.json", `{"base_model_name_or_path":"other/base"}`, ReasonAdapterRedirect},
		"auto_map":            {"config.json", `{"auto_map":{"AutoModel":"x.Model"}}`, ReasonRemoteCode},
		"nested auto_map":     {"tokenizer_config.json", `{"a":[{"b":{"auto_map":1}}]}`, ReasonRemoteCode},
		"auto_mapping":        {"generation_config.json", `{"auto_mapping":{}}`, ReasonRemoteCode},
		"custom_pipelines":    {"config.json", `{"custom_pipelines":{"x":{}}}`, ReasonRemoteCode},
		"duplicate JSON key":  {"generation_config.json", `{"a":1,"a":2}`, ReasonInvalidJSON},
		"trailing value":      {"generation_config.json", `{}{}`, ReasonInvalidJSON},
		"malformed JSON":      {"generation_config.json", `{"a":`, ReasonInvalidJSON},
		"speculators config":  {"config.json", `{"speculators_config":{"verifier":{"name_or_path":"other/model"}}}`, ReasonModelRedirect},
		"tokenizer file path": {"tokenizer_config.json", `{"tokenizer_file":"/elsewhere/tokenizer.json"}`, ReasonFilePathRedirect},
		"absent vocab file":   {"tokenizer_config.json", `{"vocab_file":"absent.json"}`, ReasonFilePathRedirect},
		"numeric merges file": {"tokenizer_config.json", `{"merges_file":1}`, ReasonFilePathRedirect},
		"near-miss license":   {"LICENSE.txt", "text", ReasonUnlistedFile},
		"near-miss readme":    {"readme.md", "text", ReasonUnlistedFile},
		"other readme":        {"README.rst", "text", ReasonUnlistedFile},
		"license variant":     {"LICENSE-APACHE", "text", ReasonUnlistedFile},
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
	original := MaxGitBlobBytes
	MaxGitBlobBytes = 4
	t.Cleanup(func() { MaxGitBlobBytes = original })
	content := "a matching blob above the limit"
	root := t.TempDir()
	write(t, filepath.Join(root, "notes.json"), content)
	upstream := map[string]UpstreamFile{"notes.json": {Type: "file", Path: "notes.json", OID: blobID(content), Size: int64(len(content))}}
	if err := os.Remove(filepath.Join(root, "notes.json")); err != nil {
		t.Fatal(err)
	}
	reason, err := checkIdentity(root, map[string]string{"notes.json": sha256Hex(content)}, upstream)
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
	plain := httptest.NewServer(http.HandlerFunc(other.serve))
	t.Cleanup(plain.Close)
	tree := "/api/models/" + testSource.Repo + "/tree/" + testSource.Revision
	for name, link := range map[string]string{
		"other host":   "<" + other.server.URL + tree + "?recursive=true&cursor=2>; rel=\"next\"",
		"other path":   "<%s/api/models/" + testSource.Repo + "/tree/main?recursive=true&cursor=2>; rel=\"next\"",
		"other scheme": "<" + plain.URL + tree + "?recursive=true&cursor=2>; rel=\"next\"",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHub(t, entries)
			h.link = strings.Replace(link, "%s", h.server.URL, 1)
			before := other.requests.Load()
			_, err := HubListing(trustBoth(h.server, other.server), h.server.URL)(context.Background(), testSource)
			if !errors.Is(err, upstreamError{reason: ReasonUpstreamInvalid}) || other.requests.Load() != before || h.strays.Load() != 0 {
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
	files := modelFiles()
	root := writeModel(t, files)
	checker := newHub(t, upstreamOf(files)).checker()
	record := run(t, checker, root)
	record.Checks[2].Outcome, record.Checks[3].Outcome = OutcomePassed, OutcomePassed
	record.Status, record.Coverage = statusOf(record.Checks), coverageOf(record.Checks)
	if record.Status != StatusPassed || record.Coverage != CoverageComplete {
		t.Fatal("all checks passing must give passed and complete coverage")
	}
	fresh, reason, err := Inventory(context.Background(), root)
	if err != nil || reason != "" || Current(&record, "example", root, testSource, checker, fresh) != StatusPassed {
		t.Fatal("an unchanged model must keep its status", err)
	}
	write(t, filepath.Join(root, "generation_config.json"), `{"changed":true}`)
	changed, _, err := Inventory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	other := testSource
	other.Revision = strings.Repeat("b", 40)
	newer, otherHub := checker, checker
	newer.Version, otherHub.Hub = "test-2", "https://mirror.invalid"
	for name, status := range map[string]Status{
		"changed file":  Current(&record, "example", root, testSource, checker, changed),
		"other model":   Current(&record, "other", root, testSource, checker, fresh),
		"other root":    Current(&record, "example", root+"2", testSource, checker, fresh),
		"other source":  Current(&record, "example", root, other, checker, fresh),
		"other checker": Current(&record, "example", root, testSource, newer, fresh),
		"other hub":     Current(&record, "example", root, testSource, otherHub, fresh),
		"no record":     Current(nil, "example", root, testSource, checker, fresh),
	} {
		if status != StatusNotScanned {
			t.Fatalf("%s: status = %s, want not_scanned", name, status)
		}
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
	reason, err := checkAllowlist(root, digests)
	if got := outcome(CheckAllowlist, reason, err); got.Reason != "files-changed" || got.Outcome != OutcomeError {
		t.Fatalf("allowlist after change = %+v", got)
	}
	upstream := map[string]UpstreamFile{}
	for _, entry := range upstreamOf(files) {
		upstream[entry.Path] = entry
	}
	upstream["config.json"] = UpstreamFile{Type: "file", Path: "config.json", OID: blobID(`{"architectures":["Other"]}`), Size: int64(len(`{"architectures":["Other"]}`))}
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
	write(t, filepath.Join(root, "model.safetensors.index.json"), indexJSON+" ")
	if _, err := checkShardIndexes(root, digests); !errors.Is(err, errChanged) {
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
