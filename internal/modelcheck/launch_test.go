package modelcheck

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validLaunchRecord() Record {
	pin := PinTemplate("hello")
	return Record{
		Version:            RecordVersion,
		Model:              "chat",
		Root:               "/models/chat",
		Source:             testSource(),
		Files:              map[string]string{"config.json": strings.Repeat("a", 64), "chat_template.jinja": strings.Repeat("b", 64)},
		Status:             StatusPassed,
		Coverage:           CoverageComplete,
		Checks:             []Check{{Name: CheckAllowlist, Outcome: OutcomePassed}, {Name: CheckTemplate, Outcome: OutcomePassed}},
		ChatTemplateSHA256: pin,
		TemplateFile:       "chat_template.jinja",
		Checker:            CheckerVersion,
		Configuration:      CurrentConfiguration(),
		CheckedAt:          time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestAllowLaunchEnforcesRootChecksAndPin(t *testing.T) {
	rec := validLaunchRecord()
	if err := rec.AllowLaunch("/models/chat", rec.ChatTemplateSHA256); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
	if err := rec.AllowLaunch("/models/other", rec.ChatTemplateSHA256); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("expected root mismatch refusal, got %v", err)
	}
	if err := rec.AllowLaunch("/models/chat", ""); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("expected missing pin refusal, got %v", err)
	}
	if err := rec.AllowLaunch("/models/chat", strings.Repeat("f", 64)); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("expected mismatched pin refusal, got %v", err)
	}
}
