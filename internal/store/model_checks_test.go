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
		Status: modelcheck.StatusNotScanned, Coverage: modelcheck.CoveragePartial,
		Configuration: modelcheck.Configuration{Hub: "https://huggingface.co", MaxJSONFileBytes: modelcheck.MaxJSONFileBytes, MaxGitBlobBytes: modelcheck.MaxGitBlobBytes, MaxModelFiles: modelcheck.MaxModelFiles, Template: modelcheck.TemplateLimits{MaxBytes: 1, MaxDepth: 1, MaxTagTokens: 1, MaxReviewItems: 1, MaxReviewBytes: 1}},
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
		"duplicate": `{"version":2,"version":2}`,
		"unknown":   `{"version":2,"extra":true}`,
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
	other := reviewRecord("other", "Be brief.")
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
	request, _ := other.PendingReview()
	if err := s.ApproveTemplate(ctx, "moved", modelcheck.NewApproval(request, 1000, time.Now())); err == nil {
		t.Fatal("an approval of a result stored under another model was stored")
	}
	var stored []byte
	if err := s.db.QueryRowContext(ctx, `SELECT payload FROM model_checks WHERE model=?`, "moved").Scan(&stored); err != nil || string(stored) != string(raw) {
		t.Fatal("a refused approval changed the row", err)
	}
	for model, payload := range map[string]string{"newer": `{"version":3,"future":true}`, "older": `{"version":1,"configuration":{}}`} {
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

// reviewRecord gives a result whose template check asks for review.
func reviewRecord(model, template string) modelcheck.Record {
	record := modelCheckRecord(model)
	record.Checks[2] = modelcheck.Check{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonTemplateNeedsReview}
	record.Status = modelcheck.StatusFindings
	record.ChatTemplateSHA256, record.TemplateFile = modelcheck.PinTemplate(template), "chat_template.jinja"
	record.TemplateReview = []modelcheck.ReviewItem{{Line: 1, Text: template}}
	return record
}

func TestTemplateApprovalIsStoredAndReused(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	record := reviewRecord("example", "Be brief.")
	if err := s.PutModelCheck(ctx, record); err != nil {
		t.Fatal(err)
	}
	request, _ := record.PendingReview()
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	var shown []modelcheck.ReviewRequest
	confirm := func(_ context.Context, r modelcheck.ReviewRequest) (int, error) {
		shown = append(shown, r)
		return 1000, nil
	}
	approval, err := s.TemplateApprover(confirm, func() time.Time { return at })(ctx, request)
	if err != nil || len(shown) != 1 || !approval.Matches(request) || approval.ApproverUID != 1000 || !approval.ApprovedAt.Equal(at) {
		t.Fatalf("the approver must ask once and approve: %v %+v", err, approval)
	}
	if err := s.ApproveTemplate(ctx, "example", approval); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ModelCheck(ctx, "example")
	if err != nil || stored.Checks[2].Outcome != modelcheck.OutcomePassed || stored.TemplateApproval == nil {
		t.Fatalf("the stored result must hold the approval: %v %+v", err, stored)
	}
	// The same template and review list reuse the stored approval.
	again, err := s.TemplateApprover(nil, time.Now)(ctx, request)
	if err != nil || again != approval || len(shown) != 1 {
		t.Fatalf("a stored approval must be reused: %v", err)
	}
	// A changed template asks again.
	changed, _ := reviewRecord("example", "Be brief!").PendingReview()
	if _, err := s.TemplateApprover(nil, time.Now)(ctx, changed); !errors.Is(err, modelcheck.ErrApprovalNeeded) {
		t.Fatal("a changed template must ask again", err)
	}
	if _, err := s.TemplateApprover(confirm, time.Now)(ctx, changed); err != nil || len(shown) != 2 {
		t.Fatal("a changed template must show the review list again", err)
	}
	// A deleted result drops its approval.
	if err := s.DeleteModelCheck(ctx, "example"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TemplateApprover(nil, time.Now)(ctx, request); !errors.Is(err, modelcheck.ErrApprovalNeeded) {
		t.Fatal("a deleted result must drop its approval", err)
	}
}

// TestRunKeepsAnApprovalStoredDuringTheRun checks a run that read the
// stored result before an approval and writes after it. The run asks for
// review of the same template and review list, so it keeps the approval.
// A run with a changed review list, or of another source, drops it.
func TestRunKeepsAnApprovalStoredDuringTheRun(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pending := reviewRecord("example", "Be brief.")
	if err := s.PutModelCheck(ctx, pending); err != nil {
		t.Fatal(err)
	}
	request, _ := pending.PendingReview()
	approval := modelcheck.NewApproval(request, 1000, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	if err := s.ApproveTemplate(ctx, "example", approval); err != nil {
		t.Fatal(err)
	}
	if err := s.PutModelCheck(ctx, pending); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ModelCheck(ctx, "example")
	if err != nil || stored.TemplateApproval == nil || *stored.TemplateApproval != approval || stored.Checks[2].Outcome != modelcheck.OutcomePassed {
		t.Fatalf("a run of the same review list must keep the approval: %v %+v", err, stored)
	}
	moved := reviewRecord("example", "Be brief.")
	moved.Source.Revision = strings.Repeat("b", 40)
	for name, record := range map[string]modelcheck.Record{"changed": reviewRecord("example", "Be brief!"), "source": moved} {
		if err := s.DeleteModelCheck(ctx, "example"); err != nil {
			t.Fatal(err)
		}
		if err := s.PutModelCheck(ctx, pending); err != nil {
			t.Fatal(err)
		}
		if err := s.ApproveTemplate(ctx, "example", approval); err != nil {
			t.Fatal(err)
		}
		if err := s.PutModelCheck(ctx, record); err != nil {
			t.Fatal(name, err)
		}
		stored, err := s.ModelCheck(ctx, "example")
		if err != nil || stored.TemplateApproval != nil {
			t.Fatalf("%s: a run of another review list or source must drop the approval: %v", name, err)
		}
	}
}

func TestTemplateApprovalNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	record := reviewRecord("example", "Be brief.")
	if err := s.PutModelCheck(ctx, record); err != nil {
		t.Fatal(err)
	}
	request, _ := record.PendingReview()
	if _, err := s.TemplateApprover(nil, time.Now)(ctx, request); !errors.Is(err, modelcheck.ErrApprovalNeeded) {
		t.Fatal("without confirmation the approver must refuse", err)
	}
	refused := errors.New("the operator did not approve")
	if _, err := s.TemplateApprover(func(context.Context, modelcheck.ReviewRequest) (int, error) { return 0, refused }, time.Now)(ctx, request); !errors.Is(err, refused) {
		t.Fatal("a refused confirmation must not approve", err)
	}
	other := modelcheck.NewApproval(request, 1000, time.Now())
	other.ChatTemplateSHA256 = strings.Repeat("c", 64)
	if err := s.ApproveTemplate(ctx, "example", other); !errors.Is(err, modelcheck.ErrApprovalMismatch) {
		t.Fatal("an approval of another template must be refused", err)
	}
	if err := s.ApproveTemplate(ctx, "missing", other); !errors.Is(err, ErrNoModelCheck) {
		t.Fatal("an approval without a result must be refused", err)
	}
}

func TestTemplateApprovalNeedsTheSameSourceAndAnUnchangedResult(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	record := reviewRecord("example", "Be brief.")
	if err := s.PutModelCheck(ctx, record); err != nil {
		t.Fatal(err)
	}
	request, _ := record.PendingReview()
	approval := modelcheck.NewApproval(request, 1000, time.Now())
	if err := s.ApproveTemplate(ctx, "example", approval); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveTemplate(ctx, "example", approval); !errors.Is(err, modelcheck.ErrNoPendingReview) {
		t.Fatal("a second approval must tell that no review is pending", err)
	}
	for name, change := range map[string]func(*modelcheck.ReviewRequest){
		"other root":   func(r *modelcheck.ReviewRequest) { r.Root = "/models/other" },
		"other source": func(r *modelcheck.ReviewRequest) { r.Source.Revision = strings.Repeat("d", 40) },
	} {
		other := request
		change(&other)
		if _, err := s.TemplateApprover(nil, nil)(ctx, other); !errors.Is(err, modelcheck.ErrApprovalNeeded) {
			t.Errorf("%s: a stored approval must not carry over: %v", name, err)
		}
	}
	raw, err := modelcheck.MarshalRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.replaceModelCheck(ctx, "example", raw, raw); !errors.Is(err, ErrModelCheckConflict) {
		t.Fatal("an update of a changed result must give a conflict", err)
	}
}

func TestTemplateApproverReportsStoreErrors(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	request, _ := reviewRecord("example", "Be brief.").PendingReview()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, "example", `{"version":`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TemplateApprover(nil, nil)(ctx, request); err == nil || errors.Is(err, modelcheck.ErrApprovalNeeded) {
		t.Fatal("a store read error must be an error, not a request for review", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE model_checks SET payload=? WHERE model=?`, `{"version":1,"configuration":{}}`, "example"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TemplateApprover(nil, nil)(ctx, request); !errors.Is(err, modelcheck.ErrApprovalNeeded) {
		t.Fatal("a stale result must ask for review", err)
	}
	if err := s.ApproveTemplate(ctx, "example", modelcheck.NewApproval(request, 0, time.Now())); !errors.Is(err, ErrNoModelCheck) {
		t.Fatal("the approval of a stale result must give no result", err)
	}
}

// TestApproveTemplateWritesOnlyTheRowItRead checks that ApproveTemplate
// updates the row only when it still holds the payload that it read. A
// test cannot run a write between the read and the update. So the row
// holds the same bytes as TEXT, which SQLite never finds equal to the BLOB
// that ApproveTemplate read. The row reads the same as a BLOB, but to the
// update it is a row that another write changed.
func TestApproveTemplateWritesOnlyTheRowItRead(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	record := reviewRecord("example", "Be brief.")
	raw, err := modelcheck.MarshalRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO model_checks(model,payload) VALUES(?,?)`, "example", string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ModelCheck(ctx, "example"); err != nil {
		t.Fatal("the row must read as the stored result", err)
	}
	request, _ := record.PendingReview()
	if err := s.ApproveTemplate(ctx, "example", modelcheck.NewApproval(request, 1000, time.Now())); !errors.Is(err, ErrModelCheckConflict) {
		t.Fatal("an approval of a changed row must give a conflict", err)
	}
	var kind string
	if err := s.db.QueryRowContext(ctx, `SELECT typeof(payload) FROM model_checks WHERE model=?`, "example").Scan(&kind); err != nil || kind != "text" {
		t.Fatal("a conflict must leave the row as it was", kind, err)
	}
	if got, err := s.ModelCheck(ctx, "example"); err != nil || got.TemplateApproval != nil {
		t.Fatal("a conflict must not store the approval", err)
	}
}
