import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

const draft = {id: 'draft-one', app: 'ollama', label: 'Qwen', model: 'qwen:latest'};
const owned = {id: 'ollama-qwen-latest', label: 'Qwen', adapter: 'systemd', bootPolicy: 'stop-to-idle',
    unit: 'gws-owned-ollama-local.service', cgroup: '/user/app.slice/gws-owned-ollama-local.service', healthURL: 'http://127.0.0.1:11434/api/tags',
    nativeModel: {runtime: 'ollama', instance: 'local', model: 'qwen:latest', endpoint: 'http://127.0.0.1:11434',
        launchFile: '/home/user/.config/systemd/user/gws-owned-ollama-local.service', launchSHA256: 'derived-digest', owned: {port: 11434}}};

test('managed launch is previewed without fingerprinting a nonexistent unit or starting apps', async () => {
    const ui = await launch({responses: {drafts: {drafts: [draft]}, 'render-owned': {profile: owned}}, deferAction: 'unused'});
    await ui.by('Preview managed launch and add for review').emit('clicked');
    const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input);
    assert.deepEqual(rendered.draft.binding, {instance: 'local', owned: {port: 11434}});
    assert.equal(rendered.managerCgroup, undefined);
    await ui.by('Finish setup').emit('clicked');
    const reviewed = JSON.parse(ui.calls.at(-1).input);
    assert.deepEqual(reviewed.catalog.profiles[0], owned);
    assert.equal(reviewed.catalog.version, 2);
    assert.ok(ui.calls.filter(call => ['verify-bindings', 'validate'].includes(call.argv[1])).every(call => JSON.parse(call.input).catalog.version === 2));
    assert.ok(!ui.calls.some(call => ['fingerprint', 'apply'].includes(call.argv[1])));
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Allow GPU Workload Supervisor')));
    const confirm = ui.widgets.find(widget => widget.children.some(child => child.label?.startsWith('I have paused')));
    ui.edit(confirm, 'active', true);
    await ui.by('Apply configuration').emit('clicked');
    const applied = JSON.parse(ui.calls.at(-1).input);
    assert.deepEqual(applied.catalog.profiles[0], owned);
    assert.equal(applied.confirmQuiesced, true);
    assert.equal(applied.catalog.version, 2);
});

test('a second Ollama model preserves the shared instance and derived service', async () => {
    const second = {...owned, id: 'ollama-other', label: 'Other', nativeModel: {...owned.nativeModel, model: 'other:latest'}};
    const ui = await launch({profiles: [owned], responses: {drafts: {drafts: [{...draft, model: 'other:latest'}]}, 'render-owned': {profile: second}}, deferAction: 'unused'});
    await ui.by('Preview managed launch and add for review').emit('clicked');
    await ui.by('Finish setup').emit('clicked');
    const profiles = JSON.parse(ui.calls.at(-1).input).catalog.profiles;
    assert.equal(profiles.length, 2);
    assert.equal(profiles[0].unit, profiles[1].unit);
    assert.notEqual(profiles[0].nativeModel.model, profiles[1].nativeModel.model);
});

test('stale managed launch result cannot promote an edited draft', async () => {
    const ui = await launch({responses: {drafts: {drafts: [draft]}, 'render-owned': {profile: owned}}, deferAction: 'render-owned'});
    const preview = ui.by('Preview managed launch and add for review').emit('clicked');
    ui.edit(ui.by('Friendly name'), 'text', 'Changed');
    ui.finish(); await preview;
    assert.ok(!ui.calls.some(call => call.argv[1] === 'validate'));
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Draft changed during preview')));
});

test('invalid launch numbers fail before render; retry remains available', async () => {
    const ui = await launch({responses: {drafts: {drafts: [draft]}}, deferAction: 'unused'});
    ui.edit(ui.by('Launch port'), 'text', '11434.5');
    await ui.by('Preview managed launch and add for review').emit('clicked');
    assert.ok(!ui.calls.some(call => call.argv[1] === 'render-owned'));
    assert.equal(ui.by('Preview managed launch and add for review').sensitive, true);
});

test('failed managed launch keeps an editable draft and collapsed error details', async () => {
    const ui = await launch({fail: 'render-owned', responses: {drafts: {drafts: [draft]}}, deferAction: 'unused'});
    await ui.by('Preview managed launch and add for review').emit('clicked');
    assert.equal(ui.by('Preview managed launch and add for review').sensitive, true);
    await ui.by('Save drafts').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].id, draft.id);
    assert.ok(ui.widgets.some(widget => widget.title === 'Technical details' && widget.visible && !widget.expanded));
    assert.ok(!ui.calls.some(call => call.argv[1] === 'apply'));
});

for (const app of ['llama.cpp', 'vllm']) {
    test(`${app} model picker feeds the managed launch preview`, async () => {
        const ui = await launch({responses: {drafts: {drafts: [{...draft, app} ]}, 'render-owned': {profile: owned}}, deferAction: 'unused'});
        await ui.by(app === 'llama.cpp' ? 'Choose model file...' : 'Choose model folder...').emit('clicked');
        await ui.by('Preview managed launch and add for review').emit('clicked');
        const request = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input);
        assert.equal(request.draft.binding.owned.modelPath, '/models/selected.gguf');
    });
}

test('editing an owned launch replaces its reviewed catalog entry only after preview', async () => {
    const ui = await launch({profiles: [{...owned, requiredMiB: 12000, bootPolicy: 'retain'}], responses: {'render-owned': {profile: {...owned, label: 'Updated'}}}, deferAction: 'unused'});
    await ui.by('Edit application').emit('clicked');
    ui.edit(ui.by('Model name'), 'text', 'other:latest');
    await ui.by('Preview managed launch and add for review').emit('clicked');
    await ui.by('Finish setup').emit('clicked');
    const profiles = JSON.parse(ui.calls.at(-1).input).catalog.profiles;
    assert.equal(profiles.length, 1);
    assert.equal(profiles[0].label, 'Updated');
    assert.equal(profiles[0].requiredMiB, 12000);
    assert.equal(profiles[0].bootPolicy, 'retain');
    assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft.id, owned.id);
});

test('a saved managed-launch edit reopens and replaces the original stable workload', async () => {
    const savedEdit = {id: owned.id, label: 'Updated', app: 'ollama', model: 'qwen:latest',
        binding: {instance: 'local', owned: {port: 11434}}};
    const ui = await launch({profiles: [{...owned, requiredMiB: 12000, bootPolicy: 'retain'}], responses: {drafts: {drafts: [savedEdit]}, 'render-owned': {profile: {...owned, label: 'Updated'}}}, deferAction: 'unused'});
    await ui.by('Preview managed launch and add for review').emit('clicked');
    await ui.by('Finish setup').emit('clicked');
    const profiles = JSON.parse(ui.calls.at(-1).input).catalog.profiles;
    assert.equal(profiles.length, 1);
    assert.equal(profiles[0].id, owned.id);
    assert.equal(profiles[0].label, 'Updated');
    assert.equal(profiles[0].requiredMiB, 12000);
    assert.equal(profiles[0].bootPolicy, 'retain');
});

test('managed launch editing retains resource/login edits made during binding verification', async () => {
    const ui = await launch({profiles: [{...owned, requiredMiB: 12000, bootPolicy: 'retain'}],
        responses: {'render-owned': {profile: owned}}, deferAction: 'verify-bindings'});
    await ui.by('Edit application').emit('clicked');
    const preview = ui.by('Preview managed launch and add for review').emit('clicked');
    await new Promise(resolve => setImmediate(resolve));
    ui.edit(ui.by('Measured VRAM requirement (MiB; optional)'), 'text', '16000');
    ui.edit(ui.by('Keep this workload running at login if already active'), 'active', false);
    ui.finish(); await preview;
    const review = ui.by('Finish setup').emit('clicked');
    await new Promise(resolve => setImmediate(resolve));
    ui.finish(); await review;
    const profile = JSON.parse(ui.calls.at(-1).input).catalog.profiles[0];
    assert.equal(profile.requiredMiB, 16000);
    assert.equal(profile.bootPolicy, 'stop-to-idle');
});

for (const app of ['llama.cpp', 'vllm']) {
    test(`${app} choosing a replacement model updates a previously populated managed path`, async () => {
        const initial = {...draft, app, binding: {instance: 'local', owned: {modelPath: '/models/old', port: 9000}}};
        const ui = await launch({responses: {drafts: {drafts: [initial]}, 'render-owned': {profile: owned}}, deferAction: 'unused'});
        await ui.by(app === 'llama.cpp' ? 'Choose model file...' : 'Choose model folder...').emit('clicked');
        await ui.by('Preview managed launch and add for review').emit('clicked');
        const request = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input);
        assert.equal(request.draft.binding.owned.modelPath, '/models/selected.gguf');
    });
}

test('a failed mixed-catalog adopted binding check cannot promote a managed preview', async () => {
    const adopted = {...owned, id: 'adopted', unit: 'external.service', nativeModel: {...owned.nativeModel, owned: undefined}};
    const ui = await launch({profiles: [adopted], fail: 'verify-bindings', responses: {drafts: {drafts: [draft]}, 'render-owned': {profile: owned}}, deferAction: 'unused'});
    await ui.by('Preview managed launch and add for review').emit('clicked');
    const request = JSON.parse(ui.calls.at(-1).input);
    assert.equal(ui.calls.at(-1).argv[1], 'verify-bindings');
    assert.equal(request.catalog.profiles.length, 2);
    assert.ok(ui.by('Preview managed launch and add for review'));
    assert.ok(!ui.calls.some(call => call.argv[1] === 'apply'));
});

for (const app of ['llama.cpp', 'vllm']) {
    test(`${app} detected references replace a previous picker path and addresses clear it`, async () => {
        const kind = app === 'llama.cpp' ? 'model-file' : 'model-directory';
        const ui = await launch({responses: {drafts: {drafts: [{...draft, app}]}, 'render-owned': {profile: owned},
            discover: {request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}}, units: [], applications: [
                {app, label: 'File setup', instanceStatus: 'candidate', reference: '/models/detected', referenceKind: kind},
                {app, label: 'Address setup', instanceStatus: 'available', endpoint: 'http://127.0.0.1:9000'}]}}, deferAction: 'unused'});
        await ui.by(app === 'llama.cpp' ? 'Choose model file...' : 'Choose model folder...').emit('clicked');
        ui.edit(ui.by('Detected instance'), 'selected', 1);
        await ui.by('Preview managed launch and add for review').emit('clicked');
        const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input);
        assert.equal(rendered.draft.binding.owned.modelPath, '/models/detected');
    });

    test(`${app} detected and manually edited addresses retire old managed model paths`, async () => {
        for (const select of ['detected', 'manual']) {
            const initial = {...draft, app, reference: '/models/old', referenceKind: app === 'llama.cpp' ? 'model-file' : 'model-directory', binding: {instance: 'local', owned: {port: 9000, modelPath: '/models/old'}}};
            const ui = await launch({responses: {drafts: {drafts: [initial]}, discover: {request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}}, units: [], applications: [{app, label: 'Address setup', instanceStatus: 'available', endpoint: 'http://127.0.0.1:9000'}]}}, deferAction: 'unused'});
            if (select === 'detected') ui.edit(ui.by('Detected instance'), 'selected', 1);
            else ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:9000');
            assert.equal(ui.by(app === 'llama.cpp' ? 'Model file' : 'Model directory').text, '');
            await ui.by('Save drafts').emit('clicked');
            assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].binding.owned.modelPath, undefined);
        }
    });

    test(`${app} detected native inventory model prefills the managed path`, async () => {
        const ui = await launch({responses: {drafts: {drafts: [{...draft, app}]}, 'render-owned': {profile: owned}, discover: {request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}}, units: [], applications: [{app, label: 'Running setup', instanceStatus: 'available', endpoint: 'http://127.0.0.1:9000', models: [{id: '/models/discovered'}]}]}}, deferAction: 'unused'});
        ui.edit(ui.by('Detected instance'), 'selected', 1);
        ui.edit(ui.by('Model'), 'selected', 1);
        await ui.by('Preview managed launch and add for review').emit('clicked');
        assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft.binding.owned.modelPath, '/models/discovered');
    });
}

test('changing an Ollama native inventory choice preserves saved managed options on reopen', async () => {
    const initial = {...draft, endpoint: 'http://127.0.0.1:11434', binding: {instance: 'custom', owned: {port: 12345}}};
    const ui = await launch({responses: {drafts: {drafts: [initial]}, probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'other:latest'}]}}, deferAction: 'unused'});
    await ui.by('Refresh discovery').emit('clicked');
    ui.edit(ui.by('Model'), 'selected', 1);
    assert.equal(ui.by('Model name').text, 'other:latest');
    await ui.by('Save drafts').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.deepEqual(saved.binding, {instance: 'custom', owned: {port: 12345}});
    assert.equal(saved.model, 'other:latest');
    const reopened = await launch({responses: {drafts: {drafts: [saved]}}});
    assert.equal(reopened.by('Launch port').text, '12345');
    assert.equal(reopened.by('Instance name').text, 'custom');
    assert.equal(reopened.by('Model name').text, 'other:latest');
});

test('selecting a detected service preserves custom managed options in saved drafts', async () => {
    const initial = {...draft, binding: {instance: 'custom', owned: {port: 12345}}};
    const ui = await launch({responses: {drafts: {drafts: [initial]}, discover: {
        request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}}, units: [],
        applications: [{app: 'ollama', label: 'Existing service', instanceStatus: 'available',
            unit: 'external.service', cgroup: '/user/external.service', endpoint: 'http://127.0.0.1:11434', models: [{id: 'other:latest'}]}]}}, deferAction: 'unused'});
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    ui.edit(ui.by('Model'), 'selected', 1);
    await ui.by('Save drafts').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.deepEqual(saved.binding, {instance: 'custom', owned: {port: 12345}});
    assert.equal(saved.model, 'other:latest');
    const reopened = await launch({responses: {drafts: {drafts: [saved]}}});
    assert.equal(reopened.by('Launch port').text, '12345');
    assert.equal(reopened.by('Instance name').text, 'custom');
});
