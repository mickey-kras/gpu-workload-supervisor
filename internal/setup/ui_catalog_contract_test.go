package setup

import (
	"bytes"
	"encoding/json"
	"os/exec"
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
await ui.by('Preview managed launch and add for review').emit('clicked');
await ui.by('Review configuration').emit('clicked');
const confirm = ui.widgets.find(widget => widget.children.some(child => child.label?.startsWith('I have paused')));
ui.edit(confirm, 'active', true);
await ui.by('Apply configuration').emit('clicked');
process.stdout.write(JSON.stringify(ui.calls.filter(call => ['verify-bindings', 'validate', 'apply'].includes(call.argv[1])).map(call => JSON.parse(call.input))));
`
	command := exec.CommandContext(t.Context(), "node", "--experimental-vm-modules", "--input-type=module", "-e", script)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("UI fixture failed: %v: %s", err, stderr.String())
	}
	var requests []Request
	if err := json.Unmarshal(output, &requests); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 {
		t.Fatalf("missing preview/review/apply requests: %d", len(requests))
	}
	for _, candidate := range requests {
		if err := Validate(candidate); err != nil {
			t.Fatalf("UI emitted a backend-rejected catalog: %v", err)
		}
		if candidate.Catalog.Version != 2 {
			t.Fatalf("owned catalog version = %d", candidate.Catalog.Version)
		}
	}
}
