package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
)

func modelCheckRecord(model string) modelcheck.Record {
	checks := []modelcheck.Check{
		{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomePassed},
		{Name: modelcheck.CheckIdentity, Outcome: modelcheck.OutcomePassed},
		{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeNotRun},
		{Name: modelcheck.CheckBehavior, Outcome: modelcheck.OutcomeNotRun},
	}
	return modelcheck.Record{
		Version: modelcheck.RecordVersion, Model: model, Root: "/models/example",
		Source: modelcheck.Source{Repo: "example-org/example-model", Revision: strings.Repeat("a", 40)},
		Files:  map[string]string{"config.json": strings.Repeat("b", 64)},
		Status: modelcheck.StatusNotScanned, Checks: checks, Checker: "test-1",
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
		"duplicate": `{"version":1,"version":1}`,
		"unknown":   `{"version":1,"extra":true}`,
		"truncated": `{"version":`,
	} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, model, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ModelCheck(ctx, model); err == nil || errors.Is(err, ErrNoModelCheck) {
			t.Fatalf("tampered row %s was read", model)
		}
	}
	raw, err := modelcheck.MarshalRecord(modelCheckRecord("other"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, "moved", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ModelCheck(ctx, "moved"); err == nil {
		t.Fatal("a result stored under another model was read")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, "big", strings.Repeat("x", modelcheck.MaxRecordBytes+1)); err == nil {
		t.Fatal("the table accepted an oversized payload")
	}
}
