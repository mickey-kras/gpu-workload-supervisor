package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

func sampleModelCheckRecord(model string) modelcheck.Record {
	pin := modelcheck.PinTemplate("template")
	return modelcheck.Record{
		Version: modelcheck.RecordVersion,
		Model:   model,
		Root:    "/models/" + model,
		Source:  modelcheck.Source{Repo: "org/" + model, Revision: strings.Repeat("a", 40)},
		Files: map[string]string{
			"config.json":         strings.Repeat("1", 64),
			"model.safetensors":   strings.Repeat("2", 64),
			"chat_template.jinja": strings.Repeat("3", 64),
		},
		Status:             modelcheck.StatusPassed,
		Coverage:           modelcheck.CoverageComplete,
		Checks:             []modelcheck.Check{{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomePassed}, {Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomePassed}},
		ChatTemplateSHA256: pin,
		TemplateFile:       "chat_template.jinja",
		Checker:            modelcheck.CheckerVersion,
		Configuration:      modelcheck.CurrentConfiguration(),
		CheckedAt:          time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestModelCheckPutGetDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.ModelCheck(ctx, "chat"); !errors.Is(err, ErrNoModelCheck) {
		t.Fatalf("expected ErrNoModelCheck, got %v", err)
	}
	rec := sampleModelCheckRecord("chat")
	if err := s.PutModelCheck(ctx, rec); err != nil {
		t.Fatalf("PutModelCheck: %v", err)
	}
	got, err := s.ModelCheck(ctx, "chat")
	if err != nil || got.ChatTemplateSHA256 != rec.ChatTemplateSHA256 {
		t.Fatalf("ModelCheck = %+v, %v", got, err)
	}
	if err := s.DeleteModelCheck(ctx, "chat"); err != nil {
		t.Fatalf("DeleteModelCheck: %v", err)
	}
	if _, err := s.ModelCheck(ctx, "chat"); !errors.Is(err, ErrNoModelCheck) {
		t.Fatalf("expected ErrNoModelCheck after delete, got %v", err)
	}
}
