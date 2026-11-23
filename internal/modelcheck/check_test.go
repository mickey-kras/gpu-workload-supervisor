package modelcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var testNow = func() time.Time { return time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC) }

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

// testChecker gives a checker with a fixed version and time.
func testChecker() Checker {
	return Checker{Version: "test-1", Now: testNow}
}

func run(t *testing.T, c Checker, root string) Record {
	t.Helper()
	record, err := c.Run(context.Background(), Request{Model: "example", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRunBindsPassingFileChecksToDigests(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	record := run(t, testChecker(), root)
	want := []Check{{CheckAllowlist, OutcomePassed, ""}, {CheckTemplate, OutcomePassed, ""}, {CheckBehavior, OutcomeNotRun, ""}}
	if !slices.Equal(record.Checks, want) || record.Status != StatusNotScanned || record.Coverage != CoveragePartial || record.ChatTemplateSHA256 != sha256Hex(files["chat_template.jinja"]) {
		t.Fatalf("checks = %+v, status %s, coverage %s, pin %s", record.Checks, record.Status, record.Coverage, record.ChatTemplateSHA256)
	}
	if len(record.Files) != len(files) || record.Files["config.json"] != sha256Hex(files["config.json"]) {
		t.Fatal("record must bind the SHA-256 of every model file")
	}
	if record.CheckedAt != testNow() || record.Checker != "test-1" || record.Configuration != CurrentConfiguration() || record.TemplateFile != "chat_template.jinja" {
		t.Fatalf("unexpected record metadata: %+v", record)
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
		"template elsewhere":   {"preprocessor_config.json", `{"chat_template":"{{ messages }}"}`, ReasonMisplacedTemplate},
		"nested template":      {"tokenizer_config.json", `{"x":{"chat_template":"{{ messages }}"}}`, ReasonMisplacedTemplate},
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
			record := run(t, testChecker(), root)
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, tc.reason}) || record.Status != StatusFindings {
				t.Fatalf("allowlist = %+v, status %s", record.Checks[0], record.Status)
			}
			if record.Checks[1].Outcome != OutcomeNotRun || record.ChatTemplateSHA256 != "" {
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
	record := run(t, testChecker(), writeModel(t, files))
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
			record := run(t, testChecker(), writeModel(t, files))
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
			record := run(t, testChecker(), root)
			if record.Checks[0] != (Check{CheckAllowlist, tc.outcome, tc.reason}) {
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
	record := run(t, testChecker(), writeModel(t, files))
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
	record := run(t, testChecker(), root)
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
			record := run(t, testChecker(), other)
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
	first := run(t, testChecker(), root)
	for i := 0; i < 20; i++ {
		if again := run(t, testChecker(), root); !slices.Equal(again.Checks, first.Checks) {
			t.Fatalf("run %d gave %+v, first %+v", i, again.Checks, first.Checks)
		}
	}
	if first.Checks[0].Reason != ReasonCodeOrPickleFile {
		t.Fatalf("the first problem in name order must win: %+v", first.Checks[0])
	}
}

func TestRunRefusesBadRequestsAndCancel(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	for _, request := range []Request{
		{Model: "", Root: root},
		{Model: "example", Root: "model"},
		{Model: "example", Root: filepath.Join(root, "absent")},
	} {
		if _, err := testChecker().Run(context.Background(), request); err == nil {
			t.Fatalf("request %+v was accepted", request)
		}
	}
	if _, err := (Checker{}).Run(context.Background(), Request{Model: "example", Root: root}); err == nil {
		t.Fatal("a checker without a version was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := testChecker().Run(ctx, Request{Model: "example", Root: root}); err == nil {
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
	checker := testChecker()
	record := run(t, checker, root)
	record.Checks[2].Outcome = OutcomePassed
	record.Status, record.Coverage = statusOf(record.Checks), coverageOf(record.Checks)
	if record.Status != StatusPassed || record.Coverage != CoverageComplete {
		t.Fatal("all checks passing must give passed and complete coverage")
	}
	if Current(ctx, &record, "example", root, checker) != StatusPassed {
		t.Fatal("an unchanged model must keep its status")
	}
	newer := checker
	newer.Version = "test-2"
	for name, status := range map[string]Status{
		"other model":   Current(ctx, &record, "other", root, checker),
		"other root":    Current(ctx, &record, "example", root+"2", checker),
		"other checker": Current(ctx, &record, "example", root, newer),
		"no record":     Current(ctx, nil, "example", root, checker),
	} {
		if status != StatusNotScanned {
			t.Fatalf("%s: status = %s, want not_scanned", name, status)
		}
	}
	if err := os.Symlink("config.json", filepath.Join(root, "zz.safetensors")); err != nil {
		t.Fatal(err)
	}
	if got := Current(ctx, &record, "example", root, checker); got != StatusNotScanned {
		t.Fatalf("a late-sorting link must not keep the old status: %s", got)
	}
	if err := os.Remove(filepath.Join(root, "zz.safetensors")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "generation_config.json"), `{"changed":true}`)
	if got := Current(ctx, &record, "example", root, checker); got != StatusNotScanned {
		t.Fatalf("a changed file must reset the status: %s", got)
	}
	linked := writeModel(t, files)
	if err := os.Symlink("config.json", filepath.Join(linked, "zz.safetensors")); err != nil {
		t.Fatal(err)
	}
	finding := run(t, checker, linked)
	if finding.Status != StatusFindings || Current(ctx, &finding, "example", linked, checker) != StatusFindings {
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
	if err := os.Remove(filepath.Join(root, "config.json")); err != nil {
		t.Fatal(err)
	}
	reason, err = checkAllowlist(context.Background(), root, digests)
	if got := outcome(CheckAllowlist, reason, err); got.Reason != "unreadable" {
		t.Fatalf("allowlist after removal = %+v", got)
	}
	write(t, filepath.Join(root, "model.safetensors.index.json"), indexJSON+" ")
	if _, err := checkShardIndexes(context.Background(), root, digests); !errors.Is(err, errChanged) {
		t.Fatal("a shard index changed after the inventory must be an error", err)
	}
}

func TestOversizedJSONIsUnsupported(t *testing.T) {
	files := modelFiles()
	files["tokenizer.json"] = `{"a":"` + strings.Repeat("x", MaxJSONFileBytes) + `"}`
	record := run(t, testChecker(), writeModel(t, files))
	if record.Checks[0] != (Check{CheckAllowlist, OutcomeUnsupported, ReasonJSONTooLarge}) || record.Status != StatusUnsupported {
		t.Fatalf("allowlist = %+v", record.Checks[0])
	}
}

func TestFilePathKeysAcceptNullAndInventoriedNames(t *testing.T) {
	files := modelFiles()
	files["tokenizer_config.json"] = `{"tokenizer_file":null,"vocab_file":"vocab.json"}`
	files["vocab.json"] = `{}`
	record := run(t, testChecker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("allowlist = %+v", record.Checks[0])
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
			record := run(t, testChecker(), writeModel(t, files))
			if record.Checks[0].Outcome != OutcomePassed {
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
			record := run(t, testChecker(), writeModel(t, files))
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
	record := run(t, testChecker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomeFailed || record.Checks[0].Reason != ReasonUnlistedFile {
		t.Fatalf("source.spm is not on the allowlist, so it must fail as unlisted: %+v", record.Checks[0])
	}
	delete(files, "source.spm")
	files["tokenizer_config.json"] = `{"vocab_file":"vocab.json","name_or_path":"/home/user/model","_name_or_path":"org/name","chat_template":[{"name":"default","template":"{{ '../x' }}"}],"eos_token":"...","additional_special_tokens":["...","~x","/ok"]}`
	files["vocab.json"] = `{}`
	record = run(t, testChecker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("text keys, bound names and token data must pass: %+v", record.Checks[0])
	}
}

func TestEverySharedLoaderKeyIsRejected(t *testing.T) {
	for reason, keys := range map[string][]string{
		ReasonRemoteCode:       RemoteCodeKeys,
		ReasonModelRedirect:    ModelRedirectKeys,
		ReasonFilePathRedirect: FileRedirectKeys,
	} {
		for _, key := range keys {
			t.Run(key, func(t *testing.T) {
				files := modelFiles()
				files["generation_config.json"] = `{"outer":[{"` + key + `":{}}]}`
				record := run(t, testChecker(), writeModel(t, files))
				if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, reason}) {
					t.Fatalf("nested %s: allowlist = %+v", key, record.Checks[0])
				}
			})
		}
	}
	for _, key := range FilePathKeys {
		t.Run(key, func(t *testing.T) {
			files := modelFiles()
			files["tokenizer_config.json"] = `{"` + key + `":"absent-name"}`
			record := run(t, testChecker(), writeModel(t, files))
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonFilePathRedirect}) {
				t.Fatalf("%s: allowlist = %+v", key, record.Checks[0])
			}
		})
	}
}

func TestInvalidUTF8IsInvalidJSON(t *testing.T) {
	files := modelFiles()
	files["generation_config.json"] = "{\"a\":\"\xff\"}"
	record := run(t, testChecker(), writeModel(t, files))
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
	record := run(t, testChecker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("tokens that look like configuration keys must pass: %+v", record.Checks[0])
	}
	files["tokenizer.json"] = `{"model":{"type":"BPE","vocab":{}},"auto_map":{}}`
	record = run(t, testChecker(), writeModel(t, files))
	if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonRemoteCode}) {
		t.Fatalf("a configuration key outside token data must still fail: %+v", record.Checks[0])
	}
}

func TestHubModelIDsAndURLsAreRedirects(t *testing.T) {
	for name, tc := range map[string]struct{ file, content string }{
		"ultravox audio model":  {"config.json", `{"audio_model_id":"openai/whisper-large-v3"}`},
		"ultravox text model":   {"config.json", `{"text_model_id":"org/llm"}`},
		"audio tokenizer":       {"config.json", `{"audio_tokenizer":"tokenizer"}`},
		"audio tokenizer class": {"config.json", `{"audio_tokenizer_class":"Tokenizer"}`},
		"audio tokenizer path":  {"config.json", `{"audio_tokenizer_name_or_path":"tokenizer"}`},
		"repo ID in a value":    {"config.json", `{"text_config":{"backbone":"org/other-model"}}`},
		"URL in a value":        {"generation_config.json", `{"source":"https://example.invalid/weights"}`},
	} {
		t.Run(name, func(t *testing.T) {
			files := modelFiles()
			files[tc.file] = tc.content
			record := run(t, testChecker(), writeModel(t, files))
			if record.Checks[0] != (Check{CheckAllowlist, OutcomeFailed, ReasonModelRedirect}) {
				t.Fatalf("allowlist = %+v", record.Checks[0])
			}
		})
	}
	files := modelFiles()
	files["config.json"] = `{"_name_or_path":"org/name","architectures":["Qwen3ForCausalLM"],"model_type":"qwen3"}`
	record := run(t, testChecker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("a label may hold a hub ID: %+v", record.Checks[0])
	}
}

func TestTokenMapsSkipThePathRules(t *testing.T) {
	files := modelFiles()
	files["vocab.json"] = `{"vocab":0,"vocab_file":1,"../x":2,"/":3,"org/name":4}`
	files["added_tokens.json"] = `{"source_spm":5,"auto_map":6}`
	record := run(t, testChecker(), writeModel(t, files))
	if record.Checks[0].Outcome != OutcomePassed {
		t.Fatalf("token maps must skip the key and value rules: %+v", record.Checks[0])
	}
}

func TestChecksStopWhenCanceled(t *testing.T) {
	files := modelFiles()
	root := writeModel(t, files)
	digests, _, err := Inventory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := checkAllowlist(ctx, root, digests); !errors.Is(err, context.Canceled) {
		t.Fatal("the allowlist must stop when canceled", err)
	}
	if _, err := checkShardIndexes(ctx, root, digests); !errors.Is(err, context.Canceled) {
		t.Fatal("the shard index check must stop when canceled", err)
	}
	if _, err := checkJSON(ctx, filepath.Join(root, "config.json"), digests); !errors.Is(err, context.Canceled) {
		t.Fatal("a single JSON read must stop when canceled", err)
	}
	if _, _, err := checkTemplate(ctx, root, digests); !errors.Is(err, context.Canceled) {
		t.Fatal("the template check must stop when canceled", err)
	}
}
