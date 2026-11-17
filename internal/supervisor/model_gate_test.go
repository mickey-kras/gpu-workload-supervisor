package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var gatePin = strings.Repeat("c", 64)

func gateRecord() modelcheck.Record {
	return modelcheck.Record{
		Version: modelcheck.RecordVersion,
		Model:   "speech",
		Root:    "/models/speech",
		Source:  modelcheck.Source{Repo: "example-org/speech", Revision: strings.Repeat("a", 40)},
		Files: map[string]string{
			"config.json":         strings.Repeat("1", 64),
			"model.safetensors":   strings.Repeat("b", 64),
			"chat_template.jinja": strings.Repeat("2", 64),
		},
		Status:   modelcheck.StatusPassed,
		Coverage: modelcheck.CoverageComplete,
		Checks: []modelcheck.Check{
			{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomePassed},
		},
		ChatTemplateSHA256: gatePin,
		TemplateFile:       "chat_template.jinja",
		Checker:            modelcheck.CheckerVersion,
		Configuration:      modelcheck.CurrentConfiguration(),
		CheckedAt:          time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

func gateController(t *testing.T, template string) (*Controller, *store.Store, *acceptanceRuntime) {
	t.Helper()
	ctx := context.Background()
	c, s, r, _ := acceptanceController(t)
	cat := c.config.Catalog.Catalog.Clone()
	for i := range cat.Profiles {
		if cat.Profiles[i].ID == "speech" {
			n := &control.NativeModel{
				Runtime: "vllm", Instance: "local", Endpoint: "http://localhost:9000", Model: "speech",
				LaunchFile: "/units/speech.service", LaunchSHA256: strings.Repeat("a", 64),
			}
			if template != "" {
				n.ChatTemplate = "/models/speech/chat_template.jinja"
				n.ChatTemplateSHA256 = template
				n.EnvironmentFile = "/home/u/vllm.env"
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
	failed.Status = modelcheck.StatusFindings
	failed.Coverage = modelcheck.CoveragePartial
	failed.ChatTemplateSHA256 = ""
	failed.TemplateFile = ""
	failed.Checks[0] = modelcheck.Check{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonRemoteCode}
	failed.Checks[1] = modelcheck.Check{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeNotRun}

	for name, tc := range map[string]struct {
		record   *modelcheck.Record
		template string
		refused  string
	}{
		"no stored result launches":    {nil, gatePin, ""},
		"passing result launches":      {&passing, gatePin, ""},
		"failed result refuses":        {&failed, gatePin, modelcheck.ReasonRemoteCode},
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
					t.Fatalf("Switch = %+v, %v", state, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Fatalf("Switch error = %v, want %q", err, tc.refused)
			}
			after, readErr := s.State(ctx)
			if readErr != nil || after != before || len(r.calls) != 0 {
				t.Fatalf("refused launch mutated state or runtime: after=%+v calls=%v", after, r.calls)
			}
		})
	}
}
