import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

const request = {profile: {statePath: '/state.db', gpuIndex: 2}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
function installation(app, unit = `${app}.service`) {
    const endpoint = 'http://127.0.0.1:9000';
    return {app, label: app, unit, endpoint, cgroup: '/trusted/service', location: '/installed/service.conf',
        recognized: true, configurationStatus: 'ready', instanceStatus: 'not-running', models: app === 'comfyui' ? [] : [{id: 'existing'}],
        binding: {unit, cgroup: '/trusted/service', healthURL: `${endpoint}/health`, model: app === 'comfyui' ? '' : 'existing', launchFile: '/installed/service.conf'}};
}
function profileFor(app) {
    const profile = {id: `${app}-existing`, label: app, adapter: 'systemd', unit: `${app}.service`,
        cgroup: '/trusted/service', healthURL: 'http://127.0.0.1:9000/health', bootPolicy: 'stop-to-idle'};
    if (app !== 'comfyui') profile.nativeModel = {runtime: app, instance: 'local', model: 'existing', endpoint: 'http://127.0.0.1:9000', launchFile: '/installed/service.conf'};
    else profile.launchBinding = {runtime: app, endpoint: 'http://127.0.0.1:9000', launchFile: '/installed/service.conf', launchSHA256: 'original'};
    return profile;
}

for (const [index, app] of ['comfyui', 'ollama', 'llama.cpp', 'vllm'].entries()) {
    test(`${app}: one recognized stopped installation finishes with no technical entry or startup`, async () => {
        const prepared = profileFor(app);
        const ui = await launch({deferAction: 'unused', responses: {
            discover: {request, units: [], applications: [installation(app)]}, prepare: {profile: prepared}, fingerprint: {sha256: 'fresh'},
        }});
        ui.edit(ui.by(`Use ${['ComfyUI', 'Ollama', 'llama.cpp', 'vLLM'][index]}`), 'active', true);
        assert.equal(ui.by('Detected instance').visible, false);
        assert.ok(ui.widgets.some(widget => widget.label?.includes('Installed and stopped. Ready to configure.')));
        if (app === 'comfyui') assert.equal(ui.by('Model'), undefined);
        else assert.equal(ui.by('Model').visible, false, 'existing launch model needs no new decision');
        const draftAdvanced = ui.by('Advanced');
        assert.notEqual(draftAdvanced.expanded, true);
        assert.ok(draftAdvanced.children.some(widget => widget.title === 'Existing service'));
        assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
        await ui.by('Check application').emit('clicked');
        const input = JSON.parse(ui.calls.find(call => call.argv[1] === 'prepare').input);
        assert.equal(input.draft.binding.unit, `${app}.service`);
        assert.ok(ui.calls.some(call => call.argv[1] === 'validate'));
        assert.ok(ui.calls.some(call => call.argv[1] === 'verify-bindings'));
        assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
        if (app === 'comfyui') assert.ok(ui.widgets.some(widget => widget.label?.includes('ComfyUI closes when switching')));

        assert.equal(ui.by('Finish setup').sensitive, true);

        await ui.by('Finish setup').emit('clicked');
        const applied = JSON.parse(ui.calls.find(call => call.argv[1] === 'apply').input);
        assert.equal(applied.confirmQuiesced, true);
        assert.equal(applied.catalog.profiles.length, 1);
        assert.ok(ui.calls.every(call => ['--version', 'discover', 'temporary-status', 'drafts', 'prepare', 'fingerprint', 'validate', 'verify-bindings', 'apply'].includes(call.argv[1])));
    });
}

test('multiple recognized installations ask for a friendly location without adoption', async () => {
    const ui = await launch({responses: {discover: {request, units: [], applications: [installation('comfyui', 'one.service'), installation('comfyui', 'two.service')]}}});
    ui.selectApplication(ui.applicationIndex ?? 0);
    assert.equal(ui.by('Detected instance').selected, 0);
    assert.ok(ui.by('Detected instance').model.get_string(1).includes('/installed/service.conf'));
    assert.ok(!ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])));
    ui.edit(ui.by('Detected instance'), 'selected', 2);
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].binding.unit, 'two.service');
});

for (const cancel of ['edit', 'remove', 'close']) {
    test(`late prepared installation cannot be added after ${cancel}`, async () => {
        const ui = await launch({deferAction: 'prepare', responses: {discover: {request, units: [], applications: [installation('comfyui')]}, prepare: {profile: profileFor('comfyui')}}});
        ui.selectApplication(ui.applicationIndex ?? 0);
        const preparing = ui.by('Check application').emit('clicked');
        if (cancel === 'edit') ui.edit(ui.by('Friendly name'), 'text', 'Changed');
        else if (cancel === 'remove') ui.by('Remove this application').emit('clicked');
        else ui.widgets.find(widget => widget.title === 'Manage applications').emit('close-request');
        ui.finish(); await preparing;
        assert.ok(!ui.calls.some(call => ['verify-bindings', 'validate', 'apply'].includes(call.argv[1])));
        assert.equal(ui.by('Finish setup').sensitive, false);
    });
}

test('preparation errors keep the draft, show collapsed detail and allow retry', async () => {
    const ui = await launch({fail: 'prepare', responses: {discover: {request, units: [], applications: [installation('comfyui')]}}});
    ui.selectApplication(ui.applicationIndex ?? 0);
    await ui.by('Check application').emit('clicked');
    assert.equal(ui.by('Check application').sensitive, true);
    assert.ok(ui.widgets.some(widget => widget.title === 'Technical details' && widget.visible && !widget.expanded));
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].binding.unit, 'comfyui.service');
    assert.ok(!ui.calls.some(call => call.argv[1] === 'apply'));
});

test('ComfyUI explicit application location is a folder, never a model decision', async () => {
    const ui = await launch({filePath: '/selected/ComfyUI'});
    ui.selectApplication(ui.applicationIndex ?? 0);
    ui.by('Choose application location…').emit('clicked');
    await ui.by('Save selections for later').emit('clicked');
    const draft = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(draft.reference, '/selected/ComfyUI');
    assert.equal(draft.referenceKind, 'application-directory');
    assert.equal(ui.by('Model'), undefined);
});

test('stopped Ollama asks only for its existing model name when inventory cannot be read', async () => {
    const candidate = {...installation('ollama'), configurationStatus: 'model-required', models: [], binding: {...installation('ollama').binding, model: ''}};
    const ui = await launch({deferAction: 'unused', responses: {discover: {request, units: [], applications: [candidate]}, prepare: {profile: profileFor('ollama')}, fingerprint: {sha256: 'fresh'}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    assert.equal(ui.by('Existing model name').visible, true);
    assert.equal(ui.by('Model').visible, false);
    ui.edit(ui.by('Existing model name'), 'text', 'existing');
    await ui.by('Check application').emit('clicked');
    assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'prepare').input).draft.model, 'existing');
    assert.ok(!ui.calls.some(call => call.argv[1] === 'apply'));
});

test('failed automatic review retains draft and retry replaces the staged profile', async () => {
    const ui = await launch({deferAction: 'unused', responses: {discover: {request: structuredClone(request), units: [], applications: [installation('comfyui')]}, prepare: {profile: profileFor('comfyui')}}});
    ui.selectApplication(ui.applicationIndex ?? 0);
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '-1');
    await ui.by('Check application').emit('clicked');
    assert.equal(ui.by('Finish setup').sensitive, false);
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts.length, 1);
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '0');
    await ui.by('Check application').emit('clicked');
    const validations = ui.calls.filter(call => call.argv[1] === 'validate');
    assert.equal(JSON.parse(validations.at(-1).input).catalog.profiles.length, 1);
    assert.ok(!ui.calls.some(call => call.argv[1] === 'apply'));
});

test('ordinary application edit preserves stable ID, cgroup evidence, resources and login policy', async () => {
    const profile = {...profileFor('comfyui'), id: 'stable-id', requiredMiB: 9000, bootPolicy: 'retain', systemdSlice: 'app.slice'};
    const ui = await launch({profiles: [profile], deferAction: 'unused', responses: {prepare: {profile: {...profile, id: 'backend-derived', label: 'Updated'}}}});
    ui.by('Edit application').emit('clicked');
    await ui.by('Save selections for later').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.id, 'stable-id');
    assert.equal(saved.binding.unit, 'comfyui.service');
    assert.equal(saved.binding.launchSHA256, undefined, 'saved drafts do not carry proof');
    await ui.by('Check application').emit('clicked');
    const reviewed = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input).catalog.profiles;
    assert.equal(reviewed.length, 1);
    assert.equal(reviewed[0].id, 'stable-id');
    assert.equal(reviewed[0].requiredMiB, 9000);
    assert.equal(reviewed[0].bootPolicy, 'retain');
    assert.equal(reviewed[0].systemdSlice, 'app.slice');
    assert.deepEqual(reviewed[0].launchBinding, profile.launchBinding);
});

test('ordinary Finish of a managed edit retains owned launch options through the existing renderer', async () => {
    const owned = {...profileFor('ollama'), nativeModel: {...profileFor('ollama').nativeModel, owned: {port: 11434}}};
    const ui = await launch({profiles: [owned], deferAction: 'unused', responses: {'render-owned': {profile: owned}}});
    ui.by('Edit application').emit('clicked');
    await ui.by('Check application').emit('clicked');
    const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input);
    assert.deepEqual(rendered.draft.binding.owned, {port: 11434});
    assert.ok(!ui.calls.some(call => call.argv[1] === 'prepare'));
    assert.ok(ui.calls.some(call => call.argv[1] === 'validate'));
    assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
});

test('failed systemd discovery remains distinct from missing installations and supports explicit location', async () => {
    const ui = await launch({responses: {discover: {request, units: [], applications: [], errors: ['Service manager unavailable']}}});
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Some installations could not be checked')));
    ui.selectApplication(ui.applicationIndex ?? 0);
    assert.ok(ui.widgets.some(widget => widget.label?.startsWith('Installation discovery failed.')));
    assert.ok(!ui.widgets.some(widget => widget.label?.includes('wasn’t detected')));
    assert.ok(ui.by('Choose application location…'));
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.ok(ui.widgets.some(widget => widget.title === 'Technical details' && widget.visible && !widget.expanded));
});

for (const phase of ['validate', 'final verification']) {
    for (const action of ['remove', 'edit', 'close']) {
        for (const existing of [false, true]) {
            test(`${phase}: ${action} keeps ${existing ? 'the original installation' : 'a new draft'} out of the reviewed changes`, async () => {
                const original = {...profileFor('comfyui'), label: 'Original'};
                const prepared = {...original, label: 'Prepared replacement'};
                const ui = await launch({deferAction: phase === 'validate' ? 'validate' : 'verify-bindings', deferOccurrence: phase === 'validate' ? null : 2,
                    responses: {discover: {request: {...request, catalog: {version: 1, profiles: existing ? [original] : []}}, units: [], applications: [installation('comfyui')]}, prepare: {profile: prepared}}});
                if (existing) ui.by('Edit application').emit('clicked');
                else ui.selectApplication(ui.applicationIndex ?? 0);
                const finishing = ui.by('Check application').emit('clicked');
                await new Promise(resolve => setImmediate(resolve));
                assert.equal(ui.widgets.filter(widget => widget.title === 'Display name').length, existing ? 1 : 0, 'a pending draft is never staged in the editable catalog');
                if (existing) assert.equal(ui.by('Display name').text, 'Original');
                if (action === 'remove') ui.by('Remove this application').emit('clicked');
                else if (action === 'edit') ui.edit(ui.by('Friendly name'), 'text', 'Updated draft');
                else ui.widgets.find(widget => widget.title === 'Manage applications').emit('close-request');
                ui.finish(); await finishing;

                assert.equal(ui.by('Finish setup').sensitive, false);
                assert.equal(ui.by('Finish setup').sensitive, false);

                await ui.by('Finish setup').emit('clicked');
                assert.ok(!ui.calls.some(call => call.argv[1] === 'apply'));
                assert.equal(ui.widgets.filter(widget => widget.title === 'Display name').length, existing ? 1 : 0);
                if (existing) assert.equal(ui.by('Display name').text, 'Original');
            });
        }
    }
}

test('inventory refresh preserves stopped Ollama lifecycle identity and ordinary model entry', async () => {
    const candidate = {...installation('ollama'), configurationStatus: 'model-required', models: [], binding: {...installation('ollama').binding, model: ''}};
    const ui = await launch({responses: {discover: {request, units: [], applications: [candidate]}, probe: {app: 'ollama', instanceStatus: 'not-running', inventoryStatus: 'not-checked', models: []}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    assert.equal(ui.by('Existing model name').visible, true);
    await ui.by('Refresh discovery').emit('clicked');
    assert.equal(ui.by('Existing model name').visible, true);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Installed and stopped. Ready to configure.')));
    ui.edit(ui.by('Existing model name'), 'text', 'existing');
    await ui.by('Save selections for later').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.deepEqual(saved.binding, {...candidate.binding, model: 'existing'});
    assert.equal(saved.model, 'existing');
});

for (const advanced of ['existing service', 'managed launch']) {
    test(`successful Advanced ${advanced} completion removes the outer draft card`, async () => {
        const prepared = profileFor('ollama');
        const draft = {id: 'advanced-draft', app: 'ollama', label: 'Existing model', model: 'existing', endpoint: prepared.nativeModel.endpoint,
            binding: advanced === 'managed launch' ? {instance: 'local', owned: {port: 11434}} : {unit: prepared.unit, cgroup: prepared.cgroup, healthURL: prepared.healthURL, instance: 'local', model: 'existing', launchFile: prepared.nativeModel.launchFile}};
        const ui = await launch({deferAction: 'unused', responses: {drafts: {drafts: [draft]}, 'render-owned': {profile: {...prepared, nativeModel: {...prepared.nativeModel, owned: {port: 11434}}}}, fingerprint: {sha256: 'checked'}}});
        const card = ui.widgets.find(widget => widget.description?.startsWith('Choose your existing installation'));
        const parent = ui.widgets.find(widget => widget.children.includes(card));
        assert.ok(parent);
        const action = advanced === 'managed launch' ? 'Preview managed launch and add for review' : 'Verify binding and add for review';
        await ui.by(action).emit('clicked');
        assert.equal(parent.children.includes(card), false, 'completion removes the direct child draft, including its nested Advanced rows');
        await ui.by('Save selections for later').emit('clicked');
        assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts, []);
        assert.equal(ui.widgets.filter(widget => widget.title === 'Display name').length, 1);
        assert.ok(!ui.calls.some(call => call.argv[1] === 'apply'));
    });
}
