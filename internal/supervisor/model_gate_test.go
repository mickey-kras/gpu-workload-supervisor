package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/store"
)

var gatePin = strings.Repeat("c", 64)

// gateRecord is a stored model check result for speech whose file and
// template checks passed with the template gatePin.
func gateRecord() modelcheck.Record {
	return modelcheck.Record{
		Version: modelcheck.RecordVersion, Model: "speech", Root: "/models/speech",
		Files:     map[string]string{"model.safetensors": strings.Repeat("b", 64)},
		FileStats: map[string]modelcheck.FileStat{"model.safetensors": {Size: 1, Inode: 1}},
		Status:    modelcheck.StatusNotScanned, Coverage: modelcheck.CoveragePartial,
		Checks: []modelcheck.Check{
			{Name: modelcheck.CheckAllowlist, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomePassed},
			{Name: modelcheck.CheckBehavior, Outcome: modelcheck.OutcomeNotRun},
		},
		ChatTemplateSHA256: gatePin, TemplateFile: "chat_template.jinja", Checker: "test-1",
		Configuration: modelcheck.Configuration{MaxJSONFileBytes: 1, MaxModelFiles: 1, Template: modelcheck.TemplateLimits{MaxBytes: 1}},
		CheckedAt:     time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
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
	failed.Checks[1] = modelcheck.Check{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonTemplateConflict}
	for name, tc := range map[string]struct {
		record   *modelcheck.Record
		template string
		files    error
		refused  string
	}{
		"adopted launch refuses":       {nil, gatePin, nil, "adopted vLLM service"},
		"passing result launches":      {&passing, gatePin, nil, ""},
		"failed result refuses":        {&failed, gatePin, nil, modelcheck.ReasonTemplateConflict},
		"changed template SHA refuses": {&passing, strings.Repeat("d", 64), nil, "chat template changed"},
		"launch without a pin refuses": {&passing, "", nil, "pins no chat template"},
		"changed model files refuse":   {&passing, gatePin, fmt.Errorf("%w: files changed", modelcheck.ErrLaunchRefused), "files changed"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c, s, r := gateController(t, tc.template)
			c.modelFilesUnchanged = func(modelcheck.Record) error { return tc.files }
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

// Taking control of a running workload starts nothing, so a result that
// refuses launches must not block the ownership transfer.
func TestModelCheckGateSkipsPreservingTakeover(t *testing.T) {
	ctx := context.Background()
	c, s, r := gateController(t, gatePin)
	c.modelFilesUnchanged = func(modelcheck.Record) error { return nil }
	if err := s.PutModelCheck(ctx, gateRecord()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "speech", "test"); err != nil {
		t.Fatal(err)
	}
	failed := gateRecord()
	failed.Status, failed.ChatTemplateSHA256, failed.TemplateFile = modelcheck.StatusFindings, "", ""
	failed.Checks[1] = modelcheck.Check{Name: modelcheck.CheckTemplate, Outcome: modelcheck.OutcomeFailed, Reason: modelcheck.ReasonTemplateConflict}
	if err := s.PutModelCheck(ctx, failed); err != nil {
		t.Fatal(err)
	}
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	e := control.OperatorPrecondition{Incarnation: state.LeaseFence.Incarnation, Version: state.Version, Owner: state.Owner, ConfigurationRevision: c.config.Catalog.Revision}
	next, err := c.OperatorTransition(ctx, "take-control", "", e)
	if err != nil || next.Owner != control.OwnerUser || next.ActiveWorkload != "speech" || slices.Contains(r.calls, "start speech") {
		t.Fatalf("take-control = %+v, calls %v: %v", next, r.calls, err)
	}
}

// A switch to the workload that already runs starts nothing, so it does not
// need a result that allows a launch.
func TestModelCheckGateSkipsSwitchToRunningWorkload(t *testing.T) {
	ctx := context.Background()
	c, s, r := gateController(t, gatePin)
	c.modelFilesUnchanged = func(modelcheck.Record) error { return nil }
	if err := s.PutModelCheck(ctx, gateRecord()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Switch(ctx, "speech", "test"); err != nil {
		t.Fatal(err)
	}
	c.modelFilesUnchanged = func(modelcheck.Record) error {
		return fmt.Errorf("%w: files changed", modelcheck.ErrLaunchRefused)
	}
	r.calls = nil
	state, err := c.Switch(ctx, "speech", "test")
	if err != nil || state.ActiveWorkload != "speech" || slices.Contains(r.calls, "start speech") {
		t.Fatalf("same-workload switch = %+v, calls %v: %v", state, r.calls, err)
	}
}

// A user-owned source stops every user runtime, so a switch to the running
// user workload restarts it. The gate must refuse it before any stop.
func TestModelCheckGateRefusesUserRestartBeforeStop(t *testing.T) {
	ctx := context.Background()
	c, s, r := gateController(t, gatePin)
	state, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Owner, state.ActiveWorkload, state.DesiredWorkload = control.OwnerUser, "speech", "speech"
	if state, err = s.UpdateState(ctx, state.Version, state); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	if _, err := c.SwitchUser(ctx, "speech", "test"); !errors.Is(err, modelcheck.ErrLaunchRefused) {
		t.Fatalf("user restart without a result = %v", err)
	}
	after, err := s.State(ctx)
	if err != nil || after != state || len(r.calls) != 0 {
		t.Fatalf("a refused restart had effects: %+v %v %v", after, err, r.calls)
	}
}

// A managed launch without a result can be checked in setup, so its refusal
// says to run the checks.
func TestModelCheckGateRefusesManagedLaunchWithoutResult(t *testing.T) {
	c, r, s := limitedController(t, limitedCatalog())
	c.modelFilesUnchanged = modelcheck.Record.FilesUnchanged
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModelCheck(context.Background(), "vision"); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	_, err := c.Switch(context.Background(), "vision", "test")
	if !errors.Is(err, modelcheck.ErrLaunchRefused) || !strings.Contains(err.Error(), "run the model checks in setup") || slices.Contains(r.calls, "start vision") {
		t.Fatalf("managed launch without a result = %v, calls %v", err, r.calls)
	}
}
