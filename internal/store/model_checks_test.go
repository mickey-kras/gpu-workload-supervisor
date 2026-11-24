package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/llm-workload-supervisor/internal/modelcheck"
)

func modelCheckRecord(model string) modelcheck.Record {
	checks := []modelcheck.Check{
		{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomePassed},
		{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeNotRun},
		{Name: modelcheck.CheckBehavior, Outcome: modelcheck.OutcomeNotRun},
	}
	return modelcheck.Record{
		Version: modelcheck.RecordVersion, Model: model, Root: "/models/example",
		Files:  map[string]string{"config.json": strings.Repeat("b", 64)},
		Status: modelcheck.StatusNotScanned, Coverage: modelcheck.CoveragePartial,
		Configuration: modelcheck.Configuration{MaxJSONFileBytes: modelcheck.MaxJSONFileBytes, MaxModelFiles: modelcheck.MaxModelFiles, Template: modelcheck.TemplateLimits{MaxBytes: 1}},
		Checks:        checks, Checker: "test-1",
		CheckedAt: time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC),
	}
}

func TestModelCheckStoreKeepsOneValidResultPerModel(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if _, err := s.ModelCheck(ctx, "example"); !errors.Is(err, ErrNoModelCheck) {
		t.Fatal("a missing result must report no result", err)
	}
	record := modelCheckRecord("example")
	if err := s.PutModelCheck(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.Checker = "test-2"
	if err := s.PutModelCheck(ctx, record); err != nil {
		t.Fatal(err)
	}
	got, err := s.ModelCheck(ctx, "example")
	if err != nil || got.Checker != "test-2" || got.Status != modelcheck.StatusNotScanned {
		t.Fatalf("stored result = %+v, %v", got, err)
	}
	invalid := modelCheckRecord("example")
	invalid.Status = modelcheck.StatusPassed
	if err := s.PutModelCheck(ctx, invalid); err == nil {
		t.Fatal("a result whose status does not follow from its checks was stored")
	}
	if err := s.DeleteModelCheck(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ModelCheck(ctx, "example"); !errors.Is(err, ErrNoModelCheck) {
		t.Fatal("a deleted result is still present", err)
	}
}

func TestModelCheckStoreRejectsTamperedRows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for model, payload := range map[string]string{
		"duplicate": `{"version":3,"version":3}`,
		"unknown":   `{"version":3,"extra":true}`,
		"truncated": `{"version":`,
		"old":       `{"version":0}`,
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, model, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ModelCheck(ctx, model); err == nil || errors.Is(err, ErrNoModelCheck) {
			t.Fatalf("tampered row %s was read", model)
		}
	}
	other := modelCheckRecord("other")
	raw, err := modelcheck.MarshalRecord(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, "moved", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ModelCheck(ctx, "moved"); err == nil {
		t.Fatal("a result stored under another model was read")
	}
	for model, payload := range map[string]string{"newer": `{"version":4,"future":true}`, "older": `{"version":2,"source":{},"templateReview":[]}`} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, model, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ModelCheck(ctx, model); !errors.Is(err, ErrNoModelCheck) {
			t.Fatal("a result of another record version must read as no result", model, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, "big", strings.Repeat("x", modelcheck.MaxRecordBytes+1)); err == nil {
		t.Fatal("the table accepted an oversized payload")
	}
}
