import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

test('refreshed inventory preserves selected model', async () => {
    const ui = await launch({responses: {probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'one'}, {id: 'two'}]}}});
    ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
    await ui.by('Refresh discovery').emit('clicked');
    ui.edit(ui.by('Model'), 'selected', 2);
    await ui.by('Save drafts').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'two');
    await ui.by('Refresh discovery').emit('clicked');
    await ui.by('Save drafts').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'two');
});

test('explicit empty inventory is distinguished from unsupported inventory', async () => {
    const labels = [];
    for (const inventoryStatus of ['available', 'unsupported']) {
        const ui = await launch({responses: {probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus, models: []}}});
        ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
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
    ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    ui.edit(ui.by('Detected instance'), 'selected', 2);
    await ui.by('Save drafts').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].endpoint, undefined);
});

test('late discovery cannot overwrite edited endpoint', async () => {
    const ui = await launch({deferAction: 'probe', responses: {probe: {app: 'ollama', instanceStatus: 'available', models: [{id: 'stale'}]}}});
    ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
    const checking = ui.by('Refresh discovery').emit('clicked');
    ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:2222');
    ui.finish(); await checking;
    await ui.by('Save drafts').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.endpoint, 'http://127.0.0.1:2222'); assert.equal(saved.model, undefined);
});
