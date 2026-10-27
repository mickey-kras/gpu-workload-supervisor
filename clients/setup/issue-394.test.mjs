import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
const discover = applications => ({request: structuredClone(request), units: [], applications});
const heading = ui => ui.widgets.find(widget => widget.cssClasses?.includes('title') && ui.visible(widget)) ?? ui.widgets.find(widget => widget.cssClasses?.includes('title-1'));
function service(unit) {
    return {app: 'vllm', label: 'vLLM', sourceKind: 'configuration', unit, location: '/same/location.conf', endpoint: 'http://127.0.0.1:8000',
        recognized: true, instanceStatus: 'not-running', configurationStatus: 'model-required', models: [],
        binding: {unit, cgroup: `/trusted/${unit}`, healthURL: 'http://127.0.0.1:8000/health', instance: unit, launchFile: '/same/location.conf'}};
}
function ownedProfile({draft}) {
    return {profile: {id: draft.id, label: draft.label, adapter: 'systemd', bootPolicy: 'stop-to-idle', unit: `gws-${draft.app}.service`,
        cgroup: '/owned', healthURL: 'http://127.0.0.1:9000/health', nativeModel: {runtime: draft.app, instance: draft.binding.instance,
            model: draft.model || draft.binding.owned.modelPath, endpoint: 'http://127.0.0.1:9000', launchFile: '/owned.service',
            launchSHA256: 'fixture', owned: draft.binding.owned}}};
}

test('beginning gear opens compact settings; Back restores originating scroll, focus and selections', async () => {
    const ui = await launch({responses: {discover: discover([service('one.service')])}});
    const gear = ui.by('Settings for vLLM');
    const save = ui.by('Save selections for later');
    const saveParent = ui.widgets.find(widget => widget.children.includes(save));
    const scroll = ui.widgets.find(widget => widget.hscrollbar_policy !== undefined);
    scroll.get_vadjustment().value = 75;
    await ui.click('Settings for vLLM');
    assert.ok(saveParent.children.includes(save), 'opening settings retains the rooted save action');
    assert.equal(heading(ui).label, 'vLLM');
    assert.equal(ui.by('Back').focused, true);
    assert.equal(heading(ui).selectable, false);
    assert.equal(scroll.get_vadjustment().value, 0);
    assert.equal(ui.visible(ui.by('Use ComfyUI')), false);
    assert.equal(ui.visible(ui.by('Installed executable')), false);
    assert.equal(ui.visible(ui.by('Existing user service')), false);
    assert.equal(ui.visible(ui.by('Choose installed executable…')), false);
    await ui.click('Advanced settings');
    ui.edit(ui.by('Installation display name'), 'text', 'My models');
    await ui.click('Back'); await ui.click('Back');
    assert.ok(saveParent.children.includes(save), 'Back hides the save action without moving its native parent');
    assert.equal(ui.visible(save), false);
    assert.equal(heading(ui).label, 'Choose your applications');
    assert.equal(gear.focused, true);
    assert.equal(scroll.get_vadjustment().value, 75);
    assert.equal(ui.by('Use vLLM').active, true);
    await ui.click('Settings for vLLM');
    assert.equal(ui.by('Installation display name').text, 'My models');
    await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].label, 'My models');
    assert.equal(ui.calls.some(call => ['render-owned', 'apply'].includes(call.argv[1])), false);
});

test('identical candidate labels retain complete distinct identities and authoritative selected binding', async () => {
    const first = service(`first-${'long-'.repeat(12)}instance.service`);
    const second = service(`second-${'long-'.repeat(12)}instance.service`);
    const address = {app: 'vllm', label: 'vLLM', sourceKind: 'endpoint', endpoint: first.endpoint, recognized: false, instanceStatus: 'not-running', models: []};
    const ui = await launch({responses: {discover: discover([first, second, address])}});
    await ui.click('Settings for vLLM');
    const choices = ui.by('Detected instance');
    assert.notEqual(choices.model.get_string(1), choices.model.get_string(2));
    assert.ok(choices.model.get_string(1).includes(first.unit));
    assert.ok(choices.model.get_string(3).includes('Address:'));
    ui.edit(choices, 'selected', 2);
    assert.ok(ui.widgets.some(widget => widget.selectable && widget.label?.includes(second.unit)));
    await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.deepEqual(saved.binding, second.binding);
    assert.equal(saved.endpoint, second.endpoint);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('inspection failure, unverified default address, missing location and unrelated errors stay distinct', async () => {
    const failure = {...service('broken.service'), recognized: false, instanceStatus: 'inspection-failed', configurationStatus: 'inspection-failed'};
    const ui = await launch({responses: {discover: {...discover([failure,
        {app: 'comfyui', endpoint: 'http://127.0.0.1:8188', sourceKind: 'endpoint', instanceStatus: 'not-running'}]), errors: ['unrelated.service: inspection failed']}}});
    for (const label of ['Inspection failed · Open settings', 'Default address unavailable · Installation unverified'])
        assert.ok(ui.widgets.some(widget => widget.label === label));
    assert.equal(ui.widgets.filter(widget => widget.label === 'Detection failed · Open settings').length, 0);
    const missing = await launch({responses: {discover: {...discover([
        {app: 'vllm', reference: '/missing/server', referenceKind: 'application', instanceStatus: 'missing'}]), errors: ['unrelated.service: inspection failed']}}});
    assert.ok(missing.widgets.some(widget => widget.label === 'Location missing · Open settings'));
    assert.equal(missing.widgets.filter(widget => widget.label === 'Detection failed · Open settings').length, 0);
});

for (const app of ['vllm']) {
    test(`${app}: executable selection validates a model, previews owned launch, then requires explicit consent`, async () => {
        const executable = `/installed/${app}`;
        const paths = [executable, '/models/existing'];
        const ui = await launch({deferAction: 'unused', filePath: () => paths.shift(), responses: {discover: discover([]), 'render-owned': ownedProfile}});
        const label = 'vLLM';
        await ui.click(`Settings for ${label}`); await ui.click('Change installation…'); await ui.click('Choose installed executable…');
        assert.equal(ui.calls.some(call => call.argv[1] === 'render-owned'), false);
        await ui.click('Back'); await ui.click('Advanced settings'); await ui.click('Review changes');
        assert.equal(heading(ui).label, 'Choose models');
        await ui.click('Choose another model…');
        await ui.click('Choose model folder...');
        await ui.click('Continue');
        assert.equal(heading(ui).label, 'Ready to finish');
        const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft;
        assert.equal(rendered.binding.owned.executable, executable);
        assert.equal(rendered.binding.unit, undefined);
        assert.equal(rendered.binding.owned.port, 8000);
        assert.ok(rendered.model || rendered.binding.owned.modelPath);
        assert.ok(ui.calls.some(call => call.argv[1] === 'verify-bindings'));
        assert.ok(ui.calls.some(call => call.argv[1] === 'validate'));
        assert.equal(ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])), false);
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
    await ui.click('Settings for vLLM'); await ui.click('Change installation…'); await ui.click('Choose installed executable…'); await ui.click('Back'); await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.model, 'existing:latest');
    assert.deepEqual(saved.binding, selected.binding);
    assert.equal(ui.calls.some(call => ['render-owned', 'apply'].includes(call.argv[1])), false);
});

test('failed executable validation retains selections without authorizing startup', async () => {
    const paths = ['/installed/vllm', '/models/existing'];
    const ui = await launch({fail: 'render-owned', deferAction: 'unused', filePath: () => paths.shift(), responses: {discover: discover([])}});
    await ui.click('Settings for vLLM'); await ui.click('Change installation…'); await ui.click('Choose installed executable…'); await ui.click('Back'); await ui.click('Advanced settings'); await ui.click('Review changes');
    await ui.click('Choose another model…'); await ui.click('Choose model folder...'); await ui.click('Continue');
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    assert.ok(ui.widgets.some(widget => (widget.title === 'Technical details' && widget.visible && !widget.expanded) || (widget.accessibleProperties?.label === 'View details' && widget.visible)));
    await ui.click('Back'); await ui.click('Settings for vLLM'); await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.binding.owned.executable, '/installed/vllm');
    assert.equal(saved.binding.owned.modelPath, '/models/existing');
});

test('selecting a recognized external service replaces a previous owned-launch draft with its authoritative binding', async () => {
    const selected = service('external.service');
    const ui = await launch({responses: {discover: discover([selected]), drafts: {drafts: [{id: 'draft-one', label: 'vLLM', app: 'vllm', reference: '/installed/vllm', referenceKind: 'application', binding: {instance: 'local', owned: {executable: '/installed/vllm', port: 12345}}}]}}});
    await ui.click('Settings for vLLM');
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
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
    assert.equal(heading(ui).label, 'ComfyUI');
    assert.equal(ui.visible(ui.by('Installation display name')), false);
    await ui.click('Cancel'); ui.edit(ui.by('Use ComfyUI'), 'active', false); await ui.click('Continue');
    const reviewed = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input);
    assert.deepEqual(reviewed.catalog.profiles.map(profile => profile.id), ['native']);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

for (const [configurationStatus, expected, choiceStatus] of [
    ['inspection-failed', 'Inspection failed · Open settings', 'inspection failed'],
    ['model-missing', 'Configured model missing · Open settings', 'model missing'],
]) {
    test(`endpoint-first production discovery preserves ${configurationStatus} service evidence`, async () => {
        const endpoint = {app: 'vllm', sourceKind: 'endpoint', label: 'vLLM', endpoint: 'http://127.0.0.1:8000', instanceStatus: 'not-running', inventoryStatus: 'not-checked', models: []};
        const configuration = {...service('selected.service'), recognized: false, configurationStatus};
        const ui = await launch({responses: {discover: discover([endpoint, configuration])}});
        assert.ok(ui.widgets.some(widget => widget.label === expected));
        await ui.click('Settings for vLLM');
        const choices = ui.by('Detected instance');
        assert.ok(choices.model.get_string(2).endsWith(choiceStatus));
        ui.edit(choices, 'selected', 2);
        assert.ok(ui.widgets.some(widget => widget.selectable && widget.label?.includes(configurationStatus === 'model-missing' ? 'Configured model is missing' : 'Installation inspection failed')));
        assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    });
}

test('removing a configured edit and reopening it uses the newly attached live editor', async () => {
    const profile = {id: 'native', label: 'Native', adapter: 'systemd', unit: 'native.service', cgroup: '/native', bootPolicy: 'stop-to-idle', nativeModel: {runtime: 'vllm', model: '/model', instance: 'local', endpoint: 'http://127.0.0.1:8000', launchFile: '/native.service'}};
    const ui = await launch({profiles: [profile]});
    await ui.click('Settings for vLLM');
    const oldCard = ui.widgets.find(widget => widget.children.some(child => child.children.includes(ui.by('Change installation…'))));
    const parent = ui.widgets.find(widget => widget.children.includes(oldCard));
    await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Remove this application');
    assert.equal(parent.children.includes(oldCard), false);
    await ui.click('Cancel'); await ui.click('Settings for vLLM');
    const newCard = parent.children.find(widget => widget !== oldCard);
    assert.ok(newCard && newCard !== oldCard, 'new edit belongs to a newly attached editor');
    assert.equal(ui.visible(newCard), true);
    assert.equal(oldCard.visible, false);
    const friendlyName = ui.by('Installation display name');
    ui.edit(friendlyName, 'text', 'Live editor'); await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].label, 'Live editor');
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('CSS fixture retains the actual stylesheet passed to the native provider', async () => {
    const ui = await launch();
    assert.equal(ui.styleProviders.size, 1);
    assert.ok([...ui.styleProviders][0].data.includes('.setup-card'));
});

test('an authoritative managed candidate prefills executable and launch options through ordinary model preview', async () => {
    const binding = {instance: 'installed-vllm-identity', owned: {executable: '/venv/bin/vllm', port: 9003, maxModelLen: 8192, alias: 'local-model'}};
    const candidate = {app: 'vllm', label: 'vLLM', sourceKind: 'owned', recognized: true, configurationStatus: 'model-required', instanceStatus: 'installed',
        reference: binding.owned.executable, referenceKind: 'application', binding, models: []};
    const ui = await launch({deferAction: 'unused', filePath: '/models/existing-vllm', responses: {discover: discover([candidate]), 'render-owned': ownedProfile}});
    await ui.click('Settings for vLLM');
    for (const [title, value] of [['Installed executable', binding.owned.executable], ['Instance name', binding.instance], ['Launch port', '9003'], ['Maximum model length', '8192'], ['Served model name', 'local-model']])
        assert.equal(ui.by(title).text, value);
    await ui.click('Use installation'); await ui.click('Choose another model…'); await ui.click('Choose model folder...'); await ui.click('Continue');
    const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft;
    assert.deepEqual(rendered.binding, {instance: binding.instance, owned: {...binding.owned, modelPath: '/models/existing-vllm'}});
    assert.equal(ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])), false);
    assert.equal(heading(ui).label, 'Ready to finish');
    assert.equal(ui.by('Finish setup').sensitive, true);
});

