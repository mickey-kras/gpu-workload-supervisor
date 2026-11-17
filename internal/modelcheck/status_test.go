package modelcheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoredAndRecheckStatus(t *testing.T) {
	root := writeModelFixture(t, map[string]string{
		"config.json":         `{}`,
		"model.safetensors":   "weights",
		"chat_template.jinja": "hello",
	})
	c := Checker{Version: CheckerVersion}
	rec, err := c.Run(context.Background(), Request{Model: "chat", Root: root, Source: testSource()})
	if err != nil {
		t.Fatal(err)
	}
	if st, reason := StoredStatus(rec, "chat", root, rec.ChatTemplateSHA256); st != StatusPassed || reason != "" {
		t.Fatalf("StoredStatus = %s, %s", st, reason)
	}
	if st, reason := StoredStatus(rec, "other", root, rec.ChatTemplateSHA256); st != StatusNotScanned || reason != ReasonNoResult {
		t.Fatalf("other model = %s, %s", st, reason)
	}
	if st, reason := StoredStatus(rec, "chat", root, strings.Repeat("0", 64)); st != StatusNotScanned || reason != ReasonTemplateChanged {
		t.Fatalf("changed pin = %s, %s", st, reason)
	}
	if st, reason, err := RecheckStatus(context.Background(), rec, "chat", root, rec.ChatTemplateSHA256); err != nil || st != StatusPassed || reason != "" {
		t.Fatalf("RecheckStatus clean = %s, %s, %v", st, reason, err)
	}
	if err := os.WriteFile(filepath.Join(root, "model.safetensors"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, reason, err := RecheckStatus(context.Background(), rec, "chat", root, rec.ChatTemplateSHA256); err != nil || st != StatusNotScanned || reason != ReasonFilesChanged {
		t.Fatalf("RecheckStatus tampered = %s, %s, %v", st, reason, err)
	}
}
