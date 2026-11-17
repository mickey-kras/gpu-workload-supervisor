package modelcheck

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSource() Source {
	return Source{Repo: "org/model", Revision: strings.Repeat("a", 40)}
}

func writeModelFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheckerPassesCleanSafetensorsModel(t *testing.T) {
	root := writeModelFixture(t, map[string]string{
		"config.json":         `{"architectures":["Qwen3ForCausalLM"]}`,
		"model.safetensors":   "weights",
		"chat_template.jinja": "{{ messages[0]['content'] }}",
	})
	c := Checker{Hub: DefaultHub, Version: CheckerVersion, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }}
	rec, err := c.Run(context.Background(), Request{Model: "chat", Root: root, Source: testSource()})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if rec.Status != StatusPassed || rec.Coverage != CoverageComplete || !rec.StaticChecksPassed() {
		t.Fatalf("unexpected status: %+v", rec)
	}
	wantPin := PinTemplate("{{ messages[0]['content'] }}")
	if rec.ChatTemplateSHA256 != wantPin || rec.TemplateFile != "chat_template.jinja" {
		t.Fatalf("unexpected template pin: %+v", rec)
	}
	if err := rec.AllowLaunch(root, wantPin); err != nil {
		t.Fatalf("AllowLaunch rejected valid record: %v", err)
	}
}

func TestCheckerRejectsPicklesRedirectsAndInvalidShards(t *testing.T) {
	c := Checker{Version: CheckerVersion}
	cases := map[string]struct {
		files  map[string]string
		reason string
	}{
		"pickle file": {
			files:  map[string]string{"config.json": `{}`, "pytorch_model.bin": "bad"},
			reason: ReasonCodeOrPickleFile,
		},
		"remote code": {
			files:  map[string]string{"config.json": `{"auto_map":{"AutoModel":"evil.py"}}`, "model.safetensors": "w"},
			reason: ReasonRemoteCode,
		},
		"model redirect": {
			files:  map[string]string{"config.json": `{"speculators_config":"other/repo"}`, "model.safetensors": "w"},
			reason: ReasonModelRedirect,
		},
		"missing shard": {
			files: map[string]string{
				"config.json":                  `{}`,
				"model.safetensors":            "w",
				"model.safetensors.index.json": `{"weight_map":{"layer.0":"missing.safetensors"}}`,
			},
			reason: ReasonInvalidShard,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeModelFixture(t, tc.files)
			rec, err := c.Run(context.Background(), Request{Model: "chat", Root: root, Source: testSource()})
			if err != nil {
				t.Fatalf("Run error: %v", err)
			}
			if rec.Status != StatusFindings || rec.Checks[0].Reason != tc.reason {
				t.Fatalf("got %+v, want reason %q", rec, tc.reason)
			}
		})
	}
}

func TestStoppedClassifiesContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(Stopped(ctx), ErrCheckCanceled) {
		t.Fatalf("expected ErrCheckCanceled, got %v", Stopped(ctx))
	}
	dCtx, dCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer dCancel()
	<-dCtx.Done()
	if !errors.Is(Stopped(dCtx), ErrCheckTimedOut) {
		t.Fatalf("expected ErrCheckTimedOut, got %v", Stopped(dCtx))
	}
}

func TestCheckerExtractsJSONTemplatePinAndValidatesShards(t *testing.T) {
	root := writeModelFixture(t, map[string]string{
		"config.json":                  `{"architectures":["Qwen3ForCausalLM"],"vocab_file":"vocab.txt"}`,
		"vocab.txt":                    "hello\n",
		"model.safetensors":            "weights",
		"model.safetensors.index.json": `{"weight_map":{"layer.0":"model.safetensors"}}`,
		"tokenizer_config.json":        `{"chat_template":"{{ messages[0].content }}"}`,
	})
	mkdirCache := filepath.Join(root, ".cache", "huggingface", "download")
	if err := os.MkdirAll(mkdirCache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mkdirCache, "model.safetensors.metadata"), []byte("meta"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Checker{Version: CheckerVersion}
	rec, err := c.Run(context.Background(), Request{Model: "chat", Root: root, Source: testSource()})
	if err != nil || rec.Status != StatusPassed || rec.TemplateFile != "tokenizer_config.json" {
		t.Fatalf("Run = %+v, %v", rec, err)
	}
	// Named templates array in tokenizer_config.json fails template stage cleanly
	namedRoot := writeModelFixture(t, map[string]string{
		"config.json":           `{}`,
		"model.safetensors":     "weights",
		"tokenizer_config.json": `{"chat_template":[{"name":"default","template":"hi"}]}`,
	})
	namedRec, err := c.Run(context.Background(), Request{Model: "chat", Root: namedRoot, Source: testSource()})
	if err != nil || namedRec.Status != StatusFindings || namedRec.Checks[1].Reason != "named-chat-templates" {
		t.Fatalf("named templates = %+v, %v", namedRec, err)
	}
	// Invalid Checker or Request arguments fail closed
	if _, err := (Checker{}).Run(context.Background(), Request{}); err == nil {
		t.Fatal("expected invalid checker error")
	}
	if _, err := c.Run(context.Background(), Request{Model: "../bad", Root: root, Source: testSource()}); err == nil {
		t.Fatal("expected invalid request error")
	}
}
