import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

test('refreshed inventory preserves selected model', async () => {
    const ui = await launch({responses: {probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'one'}, {id: 'two'}]}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:11434');
    await ui.by('Refresh discovery').emit('clicked');
    ui.edit(ui.by('Model'), 'selected', 2);
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'two');
    await ui.by('Refresh discovery').emit('clicked');
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'two');
});

test('explicit empty inventory is distinguished from unsupported inventory', async () => {
    const labels = [];
    for (const inventoryStatus of ['available', 'unsupported']) {
        const ui = await launch({responses: {probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus, models: []}}});
        ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
        ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:11434');
        await ui.by('Refresh discovery').emit('clicked');
        labels.push(ui.widgets.filter(widget => widget.label || widget.title).map(widget => widget.label || widget.title).join('\n') + '\n' + ui.by('Model').model.get_string(0));
    }
    assert.notEqual(labels[0], labels[1]);
});

test('selecting stopped unit clears prior endpoint evidence', async () => {
    const profiles = [];
    const request = {profile: {statePath: '/state.db', gpuIndex: 2}, catalog: {version: 1, profiles}, expectedRevision: 7};
    const ui = await launch({responses: {discover: {request, units: [], applications: [
        {app: 'ollama', label: 'Endpoint A', endpoint: 'http://127.0.0.1:11434', instanceStatus: 'available', models: [{id: 'one'}]},
        {app: 'ollama', label: 'Stopped unit B', unit: 'ollama-b.service', cgroup: '/b', instanceStatus: 'not-running', models: [{id: 'two'}]},
    ]}, fingerprint: {sha256: 'fingerprint'}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    ui.edit(ui.by('Detected instance'), 'selected', 2);
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].endpoint, undefined);
});

test('late discovery cannot overwrite edited endpoint', async () => {
    const ui = await launch({deferAction: 'probe', responses: {probe: {app: 'ollama', instanceStatus: 'available', models: [{id: 'stale'}]}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:1111');
    const checking = ui.by('Refresh discovery').emit('clicked');
    ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:2222');
    ui.finish(); await checking;
    await ui.by('Save selections for later').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.endpoint, 'http://127.0.0.1:2222'); assert.equal(saved.model, undefined);
});

test('unit-only instance guards refresh with its own guidance until a probeable target exists', async () => {
    const request = {profile: {statePath: '/state.db', gpuIndex: 2}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
    const ui = await launch({responses: {discover: {request, units: ['ollama.service'], applications: [
        {app: 'ollama', label: 'Ollama - ollama.service', unit: 'ollama.service', cgroup: '/user.slice/ollama.service', instanceStatus: 'active', models: [],
            nextStep: "Select this instance's endpoint or existing launch configuration. Lifecycle control is unverified."}]}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    await ui.by('Refresh discovery').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'probe').length, 0);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('existing launch configuration')));
    ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:11434');
    await ui.by('Refresh discovery').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'probe').length, 1);
});
