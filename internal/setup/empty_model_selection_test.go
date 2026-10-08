package setup

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

func TestEmptyModelSelectionSurvivesSaveAndUIReopen(t *testing.T) {
	_, home, request := fixture(t)
	request.Catalog.Profiles = nil
	script := `
import fs from 'node:fs';
import {launch} from '../../clients/setup/harness.mjs';
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const candidate = {app: 'ollama', label: 'Ollama', recognized: true, unit: 'ollama.service',
    configurationStatus: 'ready', models: [{id: 'a'}], binding: {unit: 'ollama.service', instance: 'local', model: 'a'}};
const ui = await launch({responses: {discover: {request: input.request, units: [], applications: [candidate]},
    drafts: input.saved ?? {drafts: []}}, deferAction: 'unused'});
if (!input.saved) {
    ui.edit(ui.by('Use Ollama'), 'active', true);
    if (!ui.by('a').active) throw new Error('New onboarding lost the single-model default');
    ui.edit(ui.by('a'), 'active', false);
    await ui.by('Save selections for later').emit('clicked');
    process.stdout.write(ui.calls.find(call => call.argv[1] === 'save-drafts').input);
} else {
    if (ui.by('a').active) throw new Error('Explicitly unchecked model was reselected on reopen');
    await ui.by('Continue').emit('clicked');
    await ui.by('Continue').emit('clicked');
    if (ui.by('Finish setup').sensitive || ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])))
        throw new Error('Empty model selection became executable');
}
`
	run := func(value any) []byte {
		t.Helper()
		data, _ := json.Marshal(value)
		command := exec.CommandContext(t.Context(), "node", "--experimental-vm-modules", "--input-type=module", "-e", script)
		command.Stdin = bytes.NewReader(data)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("UI selection failed: %v: %s", err, stderr.String())
		}
		return output
	}
	var saving DraftRequest
	if err := json.Unmarshal(run(map[string]any{"request": request}), &saving); err != nil {
		t.Fatal(err)
	}
	if len(saving.Drafts) != 1 || saving.Drafts[0].Models == nil || len(saving.Drafts[0].Models) != 0 {
		t.Fatalf("UI lost explicit selection: %+v", saving)
	}
	if _, err := SaveDrafts(home, saving); err != nil {
		t.Fatal(err)
	}
	reopened, err := ReadDrafts(home)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Drafts[0].Models == nil {
		t.Fatal("SaveDrafts lost explicit empty model selection")
	}
	run(map[string]any{"request": request, "saved": reopened})
}
