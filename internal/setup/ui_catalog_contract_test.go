package setup

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"reflect"
	"testing"

	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
)

// Exercise actual UI request serialization against the backend validator;
// widget/subprocess fixtures alone cannot detect incompatible catalog versions.
func TestManagedUIRequestsValidateFromFreshV1Catalog(t *testing.T) {
	_, home, request := fixture(t)
	request.Catalog.Profiles = nil
	draft := ownedDraft("vision", 9100)
	profile, _, err := OwnedProfile(draft, "/user.slice/user-1000.slice/user@1000.service", home)
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(struct {
		Request Request                 `json:"request"`
		Draft   Draft                   `json:"draft"`
		Profile control.WorkloadProfile `json:"profile"`
	}{request, draft, profile})
	if err != nil {
		t.Fatal(err)
	}
	script := `
import fs from 'node:fs';
import {launch} from '../../clients/setup/harness.mjs';
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const ui = await launch({responses: {discover: {request: input.request, units: []},
    drafts: {drafts: [input.draft]}, 'render-owned': {profile: input.profile}}, deferAction: 'unused'});
if (!ui.by('Use llama.cpp').active) throw new Error('Saved application was not selected');
await ui.by('Continue').emit('clicked');
if (ui.calls.some(call => call.argv[1] === 'apply')) throw new Error('Continue silently applied');
if (!ui.by('Finish setup')?.sensitive || !ui.by('Finish setup').visible) throw new Error('Validated configuration did not reach explicit Finish confirmation');
await ui.by('Finish setup').emit('clicked');
process.stdout.write(JSON.stringify(ui.calls.filter(call => ['verify-bindings', 'validate', 'apply'].includes(call.argv[1])).map(call => ({action: call.argv[1], request: JSON.parse(call.input)}))));
`
	command := exec.CommandContext(t.Context(), "node", "--experimental-vm-modules", "--input-type=module", "-e", script)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("UI fixture failed: %v: %s", err, stderr.String())
	}
	var requests []struct {
		Action  string  `json:"action"`
		Request Request `json:"request"`
	}
	if err := json.Unmarshal(output, &requests); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("missing preview/review/apply requests: %d", len(requests))
	}
	for index, phase := range requests {
		expectedAction := []string{"validate", "verify-bindings", "apply"}[index]
		if phase.Action != expectedAction {
			t.Fatalf("activation phase %d = %q, want %q", index, phase.Action, expectedAction)
		}
		candidate := phase.Request
		if candidate.ConfirmQuiesced != (phase.Action == "apply") {
			t.Fatalf("confirmation on phase %q = %v", phase.Action, candidate.ConfirmQuiesced)
		}
		if err := Validate(candidate); err != nil {
			t.Fatalf("UI emitted a backend-rejected catalog: %v", err)
		}
		if candidate.Catalog.Version != 2 {
			t.Fatalf("owned catalog version = %d", candidate.Catalog.Version)
		}
	}
}

func TestIncompleteManagedUIDraftsRoundTripWithoutBecomingReady(t *testing.T) {
	for _, app := range []string{"llama.cpp", "vllm", "ollama"} {
		for _, selection := range []string{"manual", "detected"} {
			t.Run(app+"/"+selection, func(t *testing.T) {
				_, home, request := fixture(t)
				draft := Draft{ID: "draft-model", Label: "Model", App: app, Endpoint: "http://127.0.0.1:9000", Model: "qwen:latest",
					Binding: &DraftBinding{Instance: "custom", Owned: &DraftOwnedLaunch{Port: 12345}}}
				if app != "ollama" {
					draft.Endpoint, draft.Model = "", ""
					draft.Reference, draft.ReferenceKind = "/models/old", "model-file"
					if app == "vllm" {
						draft.ReferenceKind = "model-directory"
					}
					draft.Binding.Owned.ModelPath = draft.Reference
				}
				input, err := json.Marshal(struct {
					Request   Request `json:"request"`
					Draft     Draft   `json:"draft"`
					Selection string  `json:"selection"`
				}{request, draft, selection})
				if err != nil {
					t.Fatal(err)
				}
				script := `
import fs from 'node:fs';
import {launch} from '../../clients/setup/harness.mjs';
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const ui = await launch({responses: {discover: {request: input.request, units: [], applications: [{app: input.draft.app,
    label: 'Other address', instanceStatus: 'not-running', endpoint: 'http://127.0.0.1:9100'}]},
    drafts: {drafts: [input.draft]}}, deferAction: 'unused'});
ui.by('Settings for ' + (input.draft.app === 'vllm' ? 'vLLM' : input.draft.app === 'ollama' ? 'Ollama' : 'llama.cpp')).emit('clicked');
if (input.selection === 'detected') ui.edit(ui.by('Detected instance'), 'selected', 1);
else ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:9100');
await ui.by('Save selections for later').emit('clicked');
process.stdout.write(ui.calls.at(-1).input);
`
				command := exec.CommandContext(t.Context(), "node", "--experimental-vm-modules", "--input-type=module", "-e", script)
				command.Stdin = bytes.NewReader(input)
				var stderr bytes.Buffer
				command.Stderr = &stderr
				output, err := command.Output()
				if err != nil {
					t.Fatalf("UI fixture failed: %v: %s", err, stderr.String())
				}
				var savedRequest DraftRequest
				if err := json.Unmarshal(output, &savedRequest); err != nil {
					t.Fatal(err)
				}
				if _, err := SaveDrafts(home, savedRequest); err != nil {
					t.Fatalf("UI emitted unsavable incomplete draft: %v", err)
				}
				reopened, err := ReadDrafts(home)
				if err != nil {
					t.Fatal(err)
				}
				if len(reopened.Drafts) != 1 {
					t.Fatal("managed draft was lost")
				}
				saved := reopened.Drafts[0]
				expected := &DraftBinding{Instance: "custom", Owned: &DraftOwnedLaunch{Port: 12345}}
				if !reflect.DeepEqual(saved.Binding, expected) {
					t.Fatalf("custom managed choices did not roundtrip: %+v", saved.Binding)
				}
				if saved.Model != "" {
					t.Fatalf("stale model survived address change: %q", saved.Model)
				}
				if _, _, err := OwnedProfile(saved, "/user.slice/user-1000.slice/user@1000.service", home); err == nil {
					t.Fatal("incomplete managed draft became a ready profile")
				}
			})
		}
	}
}
