package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mickey-kras/llm-workload-supervisor/internal/control"
	"github.com/mickey-kras/llm-workload-supervisor/internal/deployment"
	"github.com/mickey-kras/llm-workload-supervisor/internal/lock"
	"github.com/mickey-kras/llm-workload-supervisor/internal/modelcheck"
	"github.com/mickey-kras/llm-workload-supervisor/internal/store"
)

// runModelFiles are the files of a model that all static checks accept,
// with the template text.
func runModelFiles(template string) map[string]string {
	raw, _ := json.Marshal(map[string]string{"chat_template": template})
	return map[string]string{
		"config.json":            `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3"}`,
		"generation_config.json": `{}`,
		"tokenizer.json":         `{"model":{"vocab":{}}}`,
		"tokenizer_config.json":  string(raw),
		"chat_template.jinja":    template,
		"model.safetensors":      "weights",
	}
}

// runFixture writes the operator profile and a state database with one
// owned LLM whose model folder holds files with template, an adopted LLM,
// an image workload and an unidentified workload. It gives home and the
// model folder.
func runFixture(t *testing.T, id, template string) (string, string) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(testModelRoot, id)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRunModel(t, id, runModelFiles(template))
	owned, _ := ownedFixtureProfile(t, home, id, 9181)
	statusHome, _ := statusFixture(t)
	others := statusProfiles(t, statusHome)
	state := filepath.Join(home, "state/state.db")
	s := openStoreAt(t, state)
	if _, err := s.ReplaceCatalog(context.Background(), "", control.Catalog{Version: control.CatalogVersionArtifactSandbox, Profiles: append([]control.WorkloadProfile{owned}, others...)}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	writeStatusProfile(t, home, Profile{Version: 1, StatePath: state, ActivatedRelease: deployment.Release})
	if err := deployment.Write(state, deployment.Marker{Version: 1, Release: deployment.Release}); err != nil {
		t.Fatal(err)
	}
	return home, root
}

// statusProfiles gives the adopted, image and unidentified workloads of
// the status fixture at home.
func statusProfiles(t *testing.T, home string) []control.WorkloadProfile {
	t.Helper()
	snapshot, err := ReadCatalog(context.Background(), filepath.Join(home, "state/state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Catalog.Profiles[1:]
}

func runCheck(t *testing.T, home, id string) ModelStatusEntry {
	t.Helper()
	entry, err := ModelCheck(context.Background(), home, ModelCheckRequest{Version: 1, Workload: id})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func storedResult(t *testing.T, home, id string) (modelcheck.Record, error) {
	t.Helper()
	s := openStoreAt(t, filepath.Join(home, "state/state.db"))
	defer s.Close()
	return s.ModelCheck(context.Background(), id)
}

func writeRunModel(t *testing.T, id string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(testModelRoot, id, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeTargetOf reads the write target of a workload, as a run does at
// its start.
func writeTargetOf(t *testing.T, home, id string) checkTarget {
	t.Helper()
	target, err := loadWriteTarget(context.Background(), home, id)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// storeNothing is a write that must not run.
func storeNothing(t *testing.T) func(*store.Store, checkTarget) (ModelStatusEntry, error) {
	return func(*store.Store, checkTarget) (ModelStatusEntry, error) {
		t.Error("the write ran")
		return ModelStatusEntry{}, nil
	}
}

// A run that passes the static checks stores a result for the files, and
// the status reads it.
func TestModelCheckStoresTheResult(t *testing.T) {
	home, root := runFixture(t, "run-pass", "{{ messages }}")
	entry := runCheck(t, home, "run-pass")
	if entry.Status != modelcheck.StatusNotScanned || entry.Reason != "" || !entry.StaticChecksPassed || entry.LaunchBlocked || !entry.CheckAvailable {
		t.Fatalf("entry %+v", entry)
	}
	record, err := storedResult(t, home, "run-pass")
	if err != nil || record.Root != root || record.Checker != modelcheck.CheckerVersion || record.ChatTemplateSHA256 != modelcheck.PinTemplate("{{ messages }}") {
		t.Fatalf("stored %+v %v", record, err)
	}
	status := statusByWorkload(t, home, ModelStatusRequest{Version: 1})["run-pass"]
	if !status.StaticChecksPassed || status.Reason != "" || status.LaunchBlocked {
		t.Fatalf("status %+v", status)
	}
}

// A code file is a finding: the run stores it over the earlier result, and
// the launch gate refuses it.
func TestModelCheckStoresFindings(t *testing.T) {
	home, _ := runFixture(t, "run-finding", "{{ messages }}")
	runCheck(t, home, "run-finding")
	writeRunModel(t, "run-finding", map[string]string{"modeling.py": "import os"})
	entry := runCheck(t, home, "run-finding")
	if entry.Status != modelcheck.StatusFindings || !entry.LaunchBlocked || entry.StaticChecksPassed {
		t.Fatalf("entry %+v", entry)
	}
	record, err := storedResult(t, home, "run-finding")
	if err != nil || record.Status != modelcheck.StatusFindings || record.Checks[0].Reason != modelcheck.ReasonCodeOrPickleFile {
		t.Fatalf("stored %+v %v", record, err)
	}
}

// A run canceled before it starts is refused early and stores nothing:
// the earlier result stays.
func TestModelCheckCancelStoresNothing(t *testing.T) {
	home, _ := runFixture(t, "run-cancel", "{{ messages }}")
	runCheck(t, home, "run-cancel")
	before, err := storedResult(t, home, "run-cancel")
	if err != nil {
		t.Fatal(err)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := ModelCheck(done, home, ModelCheckRequest{Version: 1, Workload: "run-cancel"}); err == nil {
		t.Fatal("a canceled run gave a result")
	}
	after, err := storedResult(t, home, "run-cancel")
	if err != nil || after.CheckedAt != before.CheckedAt || after.Status != before.Status {
		t.Fatalf("the canceled run changed the result: %+v %v", after, err)
	}
}

// Setup checks only accepted LLM workloads with a model folder, and only
// for valid requests.
func TestModelCheckRefusesOtherWorkloads(t *testing.T) {
	home, _ := runFixture(t, "run-refuse", "{{ messages }}")
	for workload, want := range map[string]error{"adopted": ErrModelNoDirectory, "image": ErrModelNotLLM, "other": ErrModelNotLLM, "missing": ErrModelNotConfigured} {
		if _, err := ModelCheck(context.Background(), home, ModelCheckRequest{Version: 1, Workload: workload}); !errors.Is(err, want) {
			t.Fatalf("check %s: %v", workload, err)
		}
	}
	if _, err := ModelCheck(context.Background(), t.TempDir(), ModelCheckRequest{Version: 1, Workload: "run-refuse"}); !errors.Is(err, ErrModelNotConfigured) {
		t.Fatalf("no profile: %v", err)
	}
	status := statusByWorkload(t, home, ModelStatusRequest{Version: 1})
	if !status["run-refuse"].CheckAvailable || status["adopted"].CheckAvailable || status["image"].CheckAvailable {
		t.Fatalf("check availability %+v", status)
	}
}

// The requests are strict JSON of at most 4 KiB.
func TestModelCheckRequestsAreStrict(t *testing.T) {
	valid := `{"version":1,"workload":"chat"}`
	if request, err := DecodeModelCheckRequest(strings.NewReader(valid)); err != nil || request.Workload != "chat" {
		t.Fatalf("valid: %+v %v", request, err)
	}
	for name, raw := range map[string]string{
		"unknown":   strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1),
		"source":    strings.Replace(valid, `"version":1`, `"version":1,"source":{"repo":"example-org/model","revision":"`+strings.Repeat("a", 40)+`"}`, 1),
		"duplicate": strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"trailing":  valid + `{}`,
		"too large": strings.Replace(valid, `"chat"`, `"chat"`+strings.Repeat(" ", MaxModelCheckRequestBytes), 1),
		"version":   strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"workload":  strings.Replace(valid, `"chat"`, `"../chat"`, 1),
		"idle":      strings.Replace(valid, `"chat"`, `"idle"`, 1),
		"none":      `{"version":1}`,
	} {
		if _, err := DecodeModelCheckRequest(strings.NewReader(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// A run needs an activated setup outside maintenance, with the installed
// release. It checks this before the run and again before the write.
func TestModelCheckNeedsAnActivatedDeployment(t *testing.T) {
	home, _ := runFixture(t, "run-deploy", "{{ messages }}")
	runCheck(t, home, "run-deploy")
	state := filepath.Join(home, "state/state.db")
	before, _ := storedResult(t, home, "run-deploy")
	check := ModelCheckRequest{Version: 1, Workload: "run-deploy"}
	target := writeTargetOf(t, home, "run-deploy")
	for name, marker := range map[string]deployment.Marker{
		"maintenance":   {Version: 1, Release: deployment.Release, Maintenance: true},
		"other release": {Version: 1, Release: "v0.0.1"},
	} {
		if err := deployment.Write(state, marker); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"maintenance": "maintenance", "other release": "not activated"}[name]
		if _, err := ModelCheck(context.Background(), home, check); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: check %v", name, err)
		}
		// A change that starts during the run refuses the write.
		if _, err := writeTarget(context.Background(), home, target, storeNothing(t)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: write %v", name, err)
		}
	}
	if after, _ := storedResult(t, home, "run-deploy"); after.CheckedAt != before.CheckedAt || after.Status != before.Status {
		t.Fatalf("a refused write changed the result: %+v", after)
	}
	// A profile without an activated release is refused.
	writeStatusProfile(t, home, Profile{Version: 1, StatePath: state})
	if _, err := ModelCheck(context.Background(), home, check); !errors.Is(err, ErrModelNotActivated) {
		t.Fatalf("not activated: %v", err)
	}
}

// A run stores its result only for the catalog that it read at the start.
func TestModelCheckRefusesAResultForAChangedCatalog(t *testing.T) {
	home, _ := runFixture(t, "run-catalog", "{{ messages }}")
	target := writeTargetOf(t, home, "run-catalog")
	draft := ownedDraft("run-catalog", 9181)
	draft.Binding.Owned.ModelPath = testModelPath("run-catalog-moved")
	moved, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	s := openStoreAt(t, filepath.Join(home, "state/state.db"))
	snapshot, err := s.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	catalog := snapshot.Catalog.Clone()
	catalog.Profiles[0] = moved
	if _, err := s.ReplaceCatalog(context.Background(), snapshot.Revision, catalog); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := writeTarget(context.Background(), home, target, storeNothing(t)); !errors.Is(err, ErrResultNotCurrent) {
		t.Fatalf("a result for the old catalog: %v", err)
	}
}

// A result of another template than the launch pins does not apply.
func TestModelCheckNeedsThePinnedTemplate(t *testing.T) {
	home, _ := runFixture(t, "run-pin", "{{ messages }}")
	writeRunModel(t, "run-pin", runModelFiles("{{ messages }}Answer in French."))
	if entry := runCheck(t, home, "run-pin"); entry.Reason != modelcheck.ReasonTemplateChanged || !entry.LaunchBlocked || entry.StaticChecksPassed {
		t.Fatalf("entry %+v", entry)
	}
}

// The write waits for the state gate until the time limit of the action,
// as the gpu-setup actions set it. A gate that stays busy until then gives
// ErrStateBusy. A cancel during the wait gives modelcheck.ErrCheckCanceled.
// Nothing is stored in these cases. A gate that frees during the wait lets
// the write go on.
func TestModelCheckWaitsForTheStateGate(t *testing.T) {
	home, _ := runFixture(t, "run-gate", "{{ messages }}")
	runCheck(t, home, "run-gate")
	before, _ := storedResult(t, home, "run-gate")
	state := filepath.Join(home, "state/state.db")
	held, err := lock.TryAcquire(state + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	writeRunModel(t, "run-gate", map[string]string{"modeling.py": "import os"})
	check := ModelCheckRequest{Version: 1, Workload: "run-gate"}
	limited, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if _, err := ModelCheck(limited, home, check); !errors.Is(err, ErrStateBusy) {
		t.Fatalf("busy gate until the time limit of a check: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	if _, err := ModelCheck(canceled, home, check); !errors.Is(err, modelcheck.ErrCheckCanceled) {
		t.Fatalf("canceled during the wait: %v", err)
	}
	if after, _ := storedResult(t, home, "run-gate"); after.CheckedAt != before.CheckedAt || after.Status != before.Status {
		t.Fatalf("a refused write changed the result: %+v", after)
	}
	go func() { time.Sleep(300 * time.Millisecond); held.Close() }()
	if entry, err := ModelCheck(context.Background(), home, check); err != nil || entry.Status != modelcheck.StatusFindings {
		t.Fatalf("a gate that frees during the wait: %+v %v", entry, err)
	}
}

// acquireGate maps the end of the wait by its cause, and passes other lock
// errors through.
func TestAcquireGateMapsTheEndOfTheWait(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.db")
	held, err := lock.TryAcquire(state + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	deadline, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, err := acquireGate(deadline, state); !errors.Is(err, ErrStateBusy) {
		t.Fatalf("deadline during the wait: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := acquireGate(canceled, state); !errors.Is(err, modelcheck.ErrCheckCanceled) {
		t.Fatalf("cancel during the wait: %v", err)
	}
	if _, err := acquireGate(deadline, state); !errors.Is(err, modelcheck.ErrCheckTimedOut) {
		t.Fatalf("deadline before the wait: %v", err)
	}
	if _, err := acquireGate(canceled, state); !errors.Is(err, modelcheck.ErrCheckCanceled) {
		t.Fatalf("cancel before the wait: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = acquireGate(context.Background(), filepath.Join(file, "state.db"))
	if err == nil || errors.Is(err, ErrStateBusy) || errors.Is(err, modelcheck.ErrCheckCanceled) || errors.Is(err, modelcheck.ErrCheckTimedOut) {
		t.Fatalf("a lock error must pass through: %v", err)
	}
}

// Each error fragment that the desktop maps to a cause is in the text of
// an error that the model check actions give.
func TestDesktopFailureCausesMatchTheErrors(t *testing.T) {
	raw, err := os.ReadFile("../../clients/setup/model-reasons.mjs")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(raw), "const failureCauses = [")
	end := strings.Index(string(raw)[start:], "];")
	fragments := regexp.MustCompile(`\['([^']+)', '`).FindAllStringSubmatch(string(raw)[start:start+end], -1)
	state := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(state, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	texts := []string{ErrStateBusy.Error(), ErrModelNotActivated.Error(), ErrResultNotCurrent.Error(), modelcheck.ErrCheckTimedOut.Error()}
	for _, marker := range []deployment.Marker{{Version: 1, Release: deployment.Release, Maintenance: true}, {Version: 1, Release: "v0.0.1"}} {
		if err := deployment.Write(state, marker); err != nil {
			t.Fatal(err)
		}
		if err := deployment.Check(state, marker.Release); err != nil {
			texts = append(texts, err.Error())
		}
	}
	if len(fragments) < 6 {
		t.Fatalf("fragments %v", fragments)
	}
	for _, fragment := range fragments {
		if !slices.ContainsFunc(texts, func(text string) bool { return strings.Contains(text, fragment[1]) }) {
			t.Errorf("no error has the text %q", fragment[1])
		}
	}
}
