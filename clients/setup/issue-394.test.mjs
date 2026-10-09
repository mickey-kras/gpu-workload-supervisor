import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
const discover = applications => ({request: structuredClone(request), units: [], applications});
const heading = ui => ui.widgets.find(widget => widget.cssClasses?.includes('title-1'));
function service(unit) {
    return {app: 'ollama', label: 'Ollama', sourceKind: 'configuration', unit, location: '/same/location.conf', endpoint: 'http://127.0.0.1:11434',
        recognized: true, instanceStatus: 'not-running', configurationStatus: 'model-required', models: [],
        binding: {unit, cgroup: `/trusted/${unit}`, healthURL: 'http://127.0.0.1:11434/api/tags', instance: unit, launchFile: '/same/location.conf'}};
}
function ownedProfile({draft}) {
    return {profile: {id: draft.id, label: draft.label, adapter: 'systemd', bootPolicy: 'stop-to-idle', unit: `gws-${draft.app}.service`,
        cgroup: '/owned', healthURL: 'http://127.0.0.1:9000/health', nativeModel: {runtime: draft.app, instance: draft.binding.instance,
            model: draft.model || draft.binding.owned.modelPath, endpoint: 'http://127.0.0.1:9000', launchFile: '/owned.service',
            launchSHA256: 'fixture', owned: draft.binding.owned}}};
}

test('beginning gear opens compact settings; Back restores originating scroll, focus and selections', async () => {
    const ui = await launch({responses: {discover: discover([service('one.service')])}});
    const gear = ui.by('Settings for Ollama');
    const scroll = ui.widgets.find(widget => widget.hscrollbar_policy !== undefined);
    scroll.get_vadjustment().value = 75;
    await ui.click('Settings for Ollama');
    assert.equal(heading(ui).label, 'Ollama settings');
    assert.equal(heading(ui).focused, true);
    assert.equal(scroll.get_vadjustment().value, 0);
    assert.equal(ui.visible(ui.by('Use ComfyUI')), false);
    assert.equal(ui.visible(ui.by('Installed executable')), false);
    assert.equal(ui.visible(ui.by('Existing user service')), false);
    assert.equal(ui.visible(ui.by('Choose installed executable…')), true);
    ui.edit(ui.by('Friendly name'), 'text', 'My models');
    await ui.click('Back');
    assert.equal(heading(ui).label, 'Choose your applications');
    assert.equal(gear.focused, true);
    assert.equal(scroll.get_vadjustment().value, 75);
    assert.equal(ui.by('Use Ollama').active, true);
    await ui.click('Settings for Ollama');
    assert.equal(ui.by('Friendly name').text, 'My models');
    await ui.click('Save selections for later');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].label, 'My models');
    assert.equal(ui.calls.some(call => ['render-owned', 'apply', 'temporary-start'].includes(call.argv[1])), false);
});

test('identical candidate labels retain complete distinct identities and authoritative selected binding', async () => {
    const first = service(`first-${'long-'.repeat(12)}instance.service`);
    const second = service(`second-${'long-'.repeat(12)}instance.service`);
    const address = {app: 'ollama', label: 'Ollama', sourceKind: 'endpoint', endpoint: first.endpoint, recognized: false, instanceStatus: 'not-running', models: []};
    const ui = await launch({responses: {discover: discover([first, second, address])}});
    await ui.click('Settings for Ollama');
    const choices = ui.by('Detected instance');
    assert.notEqual(choices.model.get_string(1), choices.model.get_string(2));
    assert.ok(choices.model.get_string(1).includes(first.unit));
    assert.ok(choices.model.get_string(3).includes('Address:'));
    ui.edit(choices, 'selected', 2);
    assert.ok(ui.widgets.some(widget => widget.selectable && widget.label?.includes(second.unit)));
    await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.deepEqual(saved.binding, second.binding);
    assert.equal(saved.endpoint, second.endpoint);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('inspection failure, unverified default address, missing location and unrelated errors stay distinct', async () => {
    const failure = {...service('broken.service'), recognized: false, instanceStatus: 'inspection-failed', configurationStatus: 'inspection-failed'};
    const ui = await launch({responses: {discover: {...discover([failure,
        {app: 'vllm', endpoint: 'http://127.0.0.1:8000', sourceKind: 'endpoint', instanceStatus: 'not-running'},
        {app: 'llama.cpp', reference: '/missing/server', referenceKind: 'application', instanceStatus: 'missing'}]), errors: ['unrelated.service: inspection failed']}}});
    for (const label of ['Inspection failed · Open settings', 'Default address unavailable · Installation unverified', 'Location missing · Open settings', 'Not detected'])
        assert.ok(ui.widgets.some(widget => widget.label === label));
    assert.equal(ui.widgets.filter(widget => widget.label === 'Detection failed · Open settings').length, 0);
});

for (const app of ['ollama', 'vllm', 'llama.cpp']) {
    test(`${app}: executable selection validates a model, previews owned launch, then requires explicit consent`, async () => {
        const executable = `/installed/${app === 'llama.cpp' ? 'llama-server' : app}`;
        const paths = [executable, app === 'llama.cpp' ? '/models/existing.gguf' : '/models/existing'];
        const ui = await launch({deferAction: 'unused', filePath: () => paths.shift(), responses: {discover: discover([]), 'render-owned': ownedProfile}});
        const label = {ollama: 'Ollama', vllm: 'vLLM', 'llama.cpp': 'llama.cpp'}[app];
        await ui.click(`Settings for ${label}`); await ui.click('Choose installed executable…');
        assert.equal(ui.calls.some(call => call.argv[1] === 'render-owned'), false);
        await ui.click('Continue');
        assert.equal(heading(ui).label, 'Choose models');
        if (app === 'ollama') ui.edit(ui.by('Existing model name'), 'text', 'existing:latest');
        else {
            await ui.click('Choose another model…');
            await ui.click(app === 'llama.cpp' ? 'Choose model file...' : 'Choose model folder...');
        }
        await ui.click('Continue');
        assert.equal(heading(ui).label, 'Ready to finish');
        const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft;
        assert.equal(rendered.binding.owned.executable, executable);
        assert.equal(rendered.binding.unit, undefined);
        assert.equal(rendered.binding.owned.port, {ollama: 11434, vllm: 8000, 'llama.cpp': 8080}[app]);
        assert.ok(rendered.model || rendered.binding.owned.modelPath);
        assert.ok(ui.calls.some(call => call.argv[1] === 'verify-bindings'));
        assert.ok(ui.calls.some(call => call.argv[1] === 'validate'));
        assert.equal(ui.calls.some(call => ['prepare', 'apply', 'temporary-start', 'temporary-discover'].includes(call.argv[1])), false);
        await ui.click('Back');
        assert.equal(ui.by('Finish setup').sensitive, false);
        await ui.click('Continue'); await ui.click('Finish setup');
        const applied = JSON.parse(ui.calls.find(call => call.argv[1] === 'apply').input);
        assert.equal(applied.confirmQuiesced, true);
        assert.equal(applied.catalog.profiles[0].nativeModel.owned.executable, executable);
    });
}

test('cancelled executable picker preserves the selected service and model', async () => {
    const selected = {...service('existing.service'), configurationStatus: 'ready', models: [{id: 'existing:latest'}], binding: {...service('existing.service').binding, model: 'existing:latest'}};
    const ui = await launch({fileError: {message: 'dismissed', matches: () => true}, responses: {discover: discover([selected])}});
    await ui.click('Settings for Ollama'); await ui.click('Choose installed executable…'); await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.model, 'existing:latest');
    assert.deepEqual(saved.binding, selected.binding);
    assert.equal(ui.calls.some(call => ['render-owned', 'apply'].includes(call.argv[1])), false);
});

test('failed executable validation retains selections without authorizing startup', async () => {
    const ui = await launch({fail: 'render-owned', deferAction: 'unused', filePath: '/installed/ollama', responses: {discover: discover([])}});
    await ui.click('Settings for Ollama'); await ui.click('Choose installed executable…'); await ui.click('Continue');
    ui.edit(ui.by('Existing model name'), 'text', 'existing:latest'); await ui.click('Continue');
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    assert.ok(ui.widgets.some(widget => widget.title === 'Technical details' && widget.visible && !widget.expanded));
    await ui.click('Back'); await ui.click('Settings for Ollama'); await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.binding.owned.executable, '/installed/ollama');
    assert.equal(saved.model, 'existing:latest');
});

test('selecting a recognized external service replaces a previous owned-launch draft with its authoritative binding', async () => {
    const selected = service('external.service');
    const ui = await launch({responses: {discover: discover([selected]), drafts: {drafts: [{id: 'draft-one', label: 'Ollama', app: 'ollama', reference: '/installed/ollama', referenceKind: 'application', binding: {instance: 'local', owned: {executable: '/installed/ollama', port: 12345}}}]}}});
    await ui.click('Settings for Ollama');
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    await ui.click('Save selections for later');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts[0].binding, selected.binding);
});

test('review serialization preserves physical GPU evidence in prepared native and external application profiles', async () => {
    const gpuUUID = 'GPU-11111111-2222-3333-4444-555555555555';
    const profiles = [
        {id: 'native', label: 'Native', adapter: 'systemd', unit: 'native.service', cgroup: '/native', bootPolicy: 'stop-to-idle', nativeModel: {runtime: 'vllm', model: '/model', instance: 'local', endpoint: 'http://127.0.0.1:8000', launchFile: '/native.service', gpuUUID}},
        {id: 'comfy', label: 'Images', adapter: 'systemd', unit: 'comfy.service', cgroup: '/comfy', bootPolicy: 'stop-to-idle', launchBinding: {runtime: 'comfyui', launchFile: '/comfy.service', gpuUUID}},
    ];
    const ui = await launch({deferAction: 'unused', profiles, responses: {fingerprint: {sha256: 'fresh'}}});
    await ui.click('Continue');
    const reviewed = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input);
    assert.equal(reviewed.catalog.profiles[0].nativeModel.gpuUUID, gpuUUID);
    assert.equal(reviewed.catalog.profiles[1].launchBinding.gpuUUID, gpuUUID);
    assert.equal(ui.by('GPU UUID'), undefined);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('configured application gear opens its own settings; removal and Back do not disturb another application', async () => {
    const profiles = [
        {id: 'images', label: 'Images', adapter: 'comfyui', unit: 'images.service', cgroup: '/images', bootPolicy: 'stop-to-idle'},
        {id: 'native', label: 'Native', adapter: 'systemd', unit: 'native.service', cgroup: '/native', bootPolicy: 'stop-to-idle', nativeModel: {runtime: 'vllm', model: '/model', instance: 'local', endpoint: 'http://127.0.0.1:8000', launchFile: '/native.service'}},
    ];
    const ui = await launch({deferAction: 'unused', profiles, responses: {fingerprint: {sha256: 'fresh'}}});
    await ui.click('Settings for ComfyUI');
    assert.equal(heading(ui).label, 'ComfyUI settings');
    assert.equal(ui.visible(ui.by('Display name')), true);
    await ui.click('Remove from supervisor'); await ui.click('Back'); await ui.click('Continue');
    const reviewed = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input);
    assert.deepEqual(reviewed.catalog.profiles.map(profile => profile.id), ['native']);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});
