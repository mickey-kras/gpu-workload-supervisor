package supervisor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var gatePin = strings.Repeat("c", 64)

// gateRecord is a stored model check result for speech whose file,
// identity and template checks passed with the template gatePin.
func gateRecord() modelcheck.Record {
	return modelcheck.Record{
		Version: modelcheck.RecordVersion, Model: "speech", Root: "/models/speech",
		Source: modelcheck.Source{Repo: "example-org/speech", Revision: strings.Repeat("a", 40)},
		Files:  map[string]string{"model.safetensors": strings.Repeat("b", 64)},
		Status: modelcheck.StatusNotScanned, Coverage: modelcheck.CoveragePartial,
		Checks: []modelcheck.Check{
			{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckIdentity, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckBehavior, Outcome: modelcheck.OutcomeNotRun},
		},
		ChatTemplateSHA256: gatePin, TemplateFile: "chat_template.jinja", Checker: "test-1",
		Configuration: modelcheck.Configuration{Hub: modelcheck.DefaultHub, MaxJSONFileBytes: 1, MaxGitBlobBytes: 1, MaxModelFiles: 1,
			Template: modelcheck.TemplateLimits{MaxBytes: 1, MaxDepth: 1, MaxTagTokens: 1, MaxReviewItems: 1, MaxReviewBytes: 1}},
		CheckedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

// gateController binds speech to a vLLM launch that pins template, or
// to a launch without hardened settings when template is empty.
func gateController(t *testing.T, template string) (*Controller, *store.Store, *acceptanceRuntime) {
	t.Helper()
	ctx := context.Background()
	c, s, r, _ := acceptanceController(t)
	cat := c.config.Catalog.Catalog.Clone()
	for i := range cat.Profiles {
		if cat.Profiles[i].ID == "speech" {
			n := &control.NativeModel{Runtime: "vllm", Instance: "local", Endpoint: "http://localhost:9000", Model: "speech",
				LaunchFile: "/units/speech.service", LaunchSHA256: strings.Repeat("a", 64)}
			if template != "" {
				n.ChatTemplate, n.ChatTemplateSHA256, n.EnvironmentFile = "/models/speech/chat_template.jinja", template, "/home/u/vllm.env"
			}
			cat.Profiles[i].NativeModel = n
		}
	}
	snap, err := s.ReplaceCatalog(ctx, c.config.Catalog.Revision, cat)
	if err != nil {
		t.Fatal(err)
	}
	c.config.Catalog = &snap
	return c, s, r
}

func TestModelCheckGate(t *testing.T) {
	passing := gateRecord()
	failed := gateRecord()
	failed.Status, failed.Coverage, failed.ChatTemplateSHA256, failed.TemplateFile = modelcheck.StatusFindings, modelcheck.CoveragePartial, "", ""
	failed.Checks[1] = modelcheck.Check{Name: modelcheck.CheckIdentity, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonIdentityMismatch}
	failed.Checks[2].Outcome = modelcheck.OutcomeNotRun
	for name, tc := range map[string]struct {
		record   *modelcheck.Record
		template string
		refused  string
	}{
		"no stored result launches":    {nil, gatePin, ""},
		"passing result launches":      {&passing, gatePin, ""},
		"failed result refuses":        {&failed, gatePin, modelcheck.ReasonIdentityMismatch},
		"changed template SHA refuses": {&passing, strings.Repeat("d", 64), "chat template changed"},
		"launch without a pin refuses": {&passing, "", "pins no chat template"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c, s, r := gateController(t, tc.template)
			if tc.record != nil {
				if err := s.PutModelCheck(ctx, *tc.record); err != nil {
					t.Fatal(err)
				}
			}
			r.calls = nil
			before, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			state, err := c.Switch(ctx, "speech", "test")
			if tc.refused == "" {
				if err != nil || state.ActiveWorkload != "speech" {
					t.Fatalf("switch = %+v, %v; want a launch", state, err)
				}
				return
			}
			if !errors.Is(err, modelcheck.ErrLaunchRefused) || !strings.Contains(err.Error(), tc.refused) || !strings.HasPrefix(err.Error(), "speech (speech.service): ") {
				t.Fatalf("switch error = %v; want a refusal that names %q", err, tc.refused)
			}
			after, err := s.State(ctx)
			if err != nil || after != before || slices.Contains(r.calls, "start speech") {
				t.Fatalf("a refused launch changed state or started the unit: %+v %v %v", after, err, r.calls)
			}
		})
	}
}

// A refused model does not stop other launches, and an unreadable result
// refuses the launch.
func TestModelCheckGateOtherWorkloadsAndReadErrors(t *testing.T) {
	ctx := context.Background()
	c, s, _ := gateController(t, strings.Repeat("d", 64))
	if err := s.PutModelCheck(ctx, gateRecord()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "text", "test"); err != nil {
		t.Fatalf("a workload without a native model must launch: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := c.refuseModelCheck(ctx, "speech", false)
	if err == nil || errors.Is(err, modelcheck.ErrLaunchRefused) || !strings.Contains(err.Error(), "read the model check result") {
		t.Fatalf("read error = %v; want a refusal from the failed read", err)
	}
	if err := c.refuseModelCheck(ctx, "speech", true); err != nil {
		t.Fatalf("a verify-only recovery must skip the gate: %v", err)
	}
}

func TestModelCheckGateRefusesActivation(t *testing.T) {
	ctx := context.Background()
	c, s, r := gateController(t, strings.Repeat("d", 64))
	if err := s.PutModelCheck(ctx, gateRecord()); err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: c.config.Catalog.Revision}
	r.calls = nil
	if _, err := c.ActivateWorkload(ctx, "speech", e); !errors.Is(err, modelcheck.ErrLaunchRefused) || slices.Contains(r.calls, "start speech") {
		t.Fatalf("activation = %v, calls %v; want a refusal before any start", err, r.calls)
	}
}
