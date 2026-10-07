import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

const saved = {id: 'draft-one', app: 'ollama', label: 'Qwen', model: 'qwen:latest'};

for (const [name, probe] of [
    ['missing from the refreshed inventory', {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'other'}]}],
    ['an empty refreshed inventory', {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: []}],
    ['an unchecked inventory', {app: 'ollama', instanceStatus: 'not-running', inventoryStatus: 'not-checked'}],
]) {
    test(`refresh retains the saved model when it is ${name}`, async () => {
        const ui = await launch({responses: {drafts: {revision: 'r1', drafts: [saved]}, probe}});
        await ui.by('Refresh discovery').emit('clicked');
        const model = ui.by('Model');
        assert.equal(model.model.get_string(0), 'Saved model unavailable: qwen:latest');
        assert.equal(model.selected, 0);
        await ui.by('Save drafts').emit('clicked');
        assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'qwen:latest');
    });
}

test('refresh keeps the saved model selected when inventory order changes', async () => {
    const ui = await launch({responses: {drafts: {revision: 'r1', drafts: [saved]},
        probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'other'}, {id: 'qwen:latest'}]}}});
    await ui.by('Refresh discovery').emit('clicked');
    const model = ui.by('Model');
    assert.equal(model.selected, 2);
    assert.equal(model.model.get_string(model.selected), 'qwen:latest');
    await ui.by('Save drafts').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'qwen:latest');
});

test('unavailable placeholder never rewrites the draft model', async () => {
    const ui = await launch({responses: {drafts: {revision: 'r1', drafts: [saved]},
        probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'other'}]}}});
    await ui.by('Refresh discovery').emit('clicked');
    const model = ui.by('Model');
    ui.edit(model, 'selected', 1);
    ui.edit(model, 'selected', 0);
    await ui.by('Save drafts').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'other');
});
