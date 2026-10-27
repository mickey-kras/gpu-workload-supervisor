import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
const endpoint = 'http://127.0.0.1:8000';
function installation(app = 'comfyui', models = []) {
    return {app, label: app, unit: `${app}.service`, endpoint, recognized: true, configurationStatus: models.length ? 'ready' : app === 'comfyui' ? 'ready' : 'model-required', instanceStatus: 'not-running', models,
        binding: {unit: `${app}.service`, cgroup: '/trusted', healthURL: `${endpoint}/health`, instance: 'local', model: models.length === 1 ? models[0].id : '', launchFile: '/trusted/unit'}};
}
function prepared({draft}) {
    const profile = {id: draft.id, label: draft.label, unit: draft.binding.unit, cgroup: '/trusted', healthURL: `${endpoint}/health`, adapter: 'systemd', bootPolicy: 'stop-to-idle'};
    if (draft.app === 'comfyui') profile.launchBinding = {runtime: draft.app, endpoint, launchFile: '/trusted/unit'};
    else profile.nativeModel = {runtime: draft.app, instance: 'local', model: draft.model, endpoint, launchFile: '/trusted/unit'};
    return {profile};
}
function options(candidates, extra = {}) {
    return {deferAction: 'unused', responses: {discover: {request: structuredClone(request), units: [], applications: candidates}, prepare: prepared, fingerprint: {sha256: 'fresh'}, ...extra}};
}
const heading = ui => ui.widgets.find(widget => widget.cssClasses?.includes('title-1'));
const primary = ui => ui.widgets.filter(widget => widget.cssClasses?.includes('suggested-action') && widget.visible !== false);

test('application cards precede configuration, labeled gears and one primary action stay in reserved toolbar', async () => {
    const ui = await launch(options([installation()]));
    assert.equal(heading(ui).label, 'Choose your applications');
    for (const label of ['ComfyUI', 'vLLM']) {
        assert.equal(ui.by(`Use ${label}`).active, false);
        assert.equal(ui.by(`Settings for ${label}`).accessibleProperties.label, `Settings for ${label}`);
        assert.ok(ui.by(`Settings for ${label}`).tooltip_text);
        assert.equal(ui.by(`Settings for ${label}`).label, undefined, 'icon-only gears keep explicit accessible names');
    }
    assert.equal(ui.by('Application'), undefined);
    assert.equal(ui.by('Add workload'), undefined);
    assert.equal(ui.by('Apply configuration'), undefined);
    assert.equal(ui.by('Back').visible, false);
    assert.deepEqual(primary(ui).map(widget => widget.label), ['Continue']);
    const footer = ui.widgets.find(widget => widget.cssClasses?.includes('toolbar'));
    const toolbar = ui.widgets.find(widget => widget.children.includes(footer));
    const scroll = ui.widgets.find(widget => widget.hscrollbar_policy !== undefined);
    assert.ok(toolbar.children.includes(scroll), 'content and footer are separate native ToolbarView children');
    assert.ok(!scroll.children.includes(footer), 'footer never overlays scroll content');
});

test('ComfyUI skips models; Finish setup explicitly activates only the freshly checked snapshot', async () => {
    const ui = await launch(options([installation()]));
    ui.edit(ui.by('Use ComfyUI'), 'active', true);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.equal(heading(ui).label, 'Ready to finish');
    assert.deepEqual(primary(ui).map(widget => widget.label), ['Finish setup']);
    assert.equal(ui.by('Model'), undefined);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    assert.ok(ui.widgets.some(widget => widget.label === 'Only one workload runs at a time.'));
    assert.ok(ui.widgets.some(widget => widget.label === 'ComfyUI closes when switching away.'));
    await ui.by('Finish setup').emit('clicked');
    const applied = JSON.parse(ui.calls.find(call => call.argv[1] === 'apply').input);
    assert.equal(applied.confirmQuiesced, true);
    assert.equal(applied.catalog.profiles.length, 1);
});

test('an application is selected once; the chosen model becomes a named workload, and Back preserves the selection', async () => {
    const ui = await launch(options([installation('vllm', [{id: 'a', label: 'Model A'}, {id: 'b', label: 'Model B'}])]));
    ui.edit(ui.by('Use vLLM'), 'active', true);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.equal(heading(ui).label, 'Choose models');
    ui.edit(ui.by('Model'), 'selected', 1);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    let checked = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input);
    assert.deepEqual(checked.catalog.profiles.map(profile => profile.label), ['vLLM - Model A']);
    assert.deepEqual(checked.catalog.profiles.map(profile => profile.unit), ['vllm.service']);
    await ui.by('Back').emit('clicked');
    assert.equal(heading(ui).label, 'Choose models');
    assert.equal(ui.by('Model').selected, 1);
    assert.equal(ui.by('Finish setup').sensitive, false);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    checked = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input);
    assert.equal(checked.catalog.profiles.length, 1, 'Back then Continue replaces the staged workload rather than duplicating it');
    await ui.by('Back').emit('clicked'); ui.edit(ui.by('Model'), 'selected', 2);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    checked = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input);
    assert.deepEqual(checked.catalog.profiles.map(profile => profile.nativeModel.model), ['b']);
});

test('model chooser disclosure preserves rooted pickers and opens no dialog until a picker is activated', async () => {
    const ui = await launch(options([installation('vllm', [{id: 'a'}, {id: 'b'}])]));
    ui.edit(ui.by('Use vLLM'), 'active', true);
    await ui.click('Continue');
    const chooser = ui.by('Choose another model…');
    const file = ui.by('Choose model folder...');
    const folder = ui.by('Choose model folder...');
    const choices = ui.widgets.find(widget => widget.children.includes(file));
    const parent = ui.widgets.find(widget => widget.children.includes(choices));
    assert.equal(chooser.widgetType, 'Button');
    assert.ok(parent.children.includes(chooser));
    assert.equal(chooser.accessibleStates.expanded, 0);
    assert.equal(ui.visible(file), false); assert.equal(ui.visible(folder), false);
    const callsBefore = ui.calls.length;
    await ui.click('Choose another model…');
    assert.equal(chooser.accessibleStates.expanded, 1);
    assert.equal(ui.visible(file), true); assert.equal(ui.visible(folder), true);
    await ui.click('Choose another model…');
    assert.equal(chooser.accessibleStates.expanded, 0);
    assert.equal(ui.visible(file), false); assert.equal(ui.visible(folder), false);
    assert.ok(parent.children.includes(choices), 'collapsed choices remain attached to the page');
    assert.equal(ui.calls.length, callsBefore, 'disclosure does not start discovery or change configuration');
    await ui.click('Choose another model…');
    await ui.click('Choose model folder...');
    assert.ok(ui.widgets.some(widget => widget.label === '/models/selected'));
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('Back permits changing application without duplicate drafts or applying saved selections', async () => {
    const ui = await launch(options([installation(), installation('vllm', [{id: 'a'}, {id: 'b'}])]));
    ui.edit(ui.by('Use vLLM'), 'active', true);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); await ui.by('Back').emit('clicked');
    assert.equal(heading(ui).label, 'Choose your applications');
    ui.edit(ui.by('Use vLLM'), 'active', false); ui.edit(ui.by('Use ComfyUI'), 'active', true);
    await ui.by('Save selections for later').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts.map(draft => draft.app), ['comfyui']);
    await ui.by('Set up later').emit('clicked');
    assert.ok(ui.widgets.some(widget => widget.closed));
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

for (const phase of ['prepare', 'validate', 'verify-bindings']) {
    test(`Back invalidates an in-flight ${phase} and cannot activate its late reply`, async () => {
        const ui = await launch({...options([installation()]), deferAction: phase});
        ui.edit(ui.by('Use ComfyUI'), 'active', true);
        const checking = (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
        await new Promise(resolve => setImmediate(resolve));
        await ui.by('Back').emit('clicked'); ui.finish(); await checking;
        assert.equal(ui.by('Finish setup').sensitive, false);
        await ui.by('Finish setup').emit('clicked');
        assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    });
}

test('a prepared workload that reuses a configured workload ID is rejected before activation review', async () => {
    const existing = prepared({draft: {id: 'same', label: 'ComfyUI', app: 'comfyui', binding: {unit: 'comfyui.service'}}}).profile;
    const duplicate = prepared({draft: {id: 'same', label: 'Same', app: 'vllm', model: 'a', binding: {unit: 'vllm.service'}}});
    const ui = await launch(options([installation(), installation('vllm', [{id: 'a'}, {id: 'b'}])], {prepare: duplicate, discover: {request: {...request, catalog: {version: 1, profiles: [existing]}}, units: [], applications: [installation(), installation('vllm', [{id: 'a'}, {id: 'b'}])]}}));
    ui.edit(ui.by('Use vLLM'), 'active', true); await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    ui.edit(ui.by('Model'), 'selected', 1);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('already configured')));
});

test('deselecting an existing application reviews only Supervisor removal and preserves its files', async () => {
    const profile = prepared({draft: {id: 'comfyui', label: 'ComfyUI', app: 'comfyui', binding: {unit: 'comfyui.service'}}}).profile;
    const ui = await launch({...options([installation()]), profiles: [profile], responses: {discover: {request: {...request, catalog: {version: 1, profiles: [profile]}}, units: [], applications: [installation()]}}});
    ui.by('Settings for ComfyUI').emit('clicked');
    if (ui.by('Cancel')) await ui.by('Cancel').emit('clicked'); ui.edit(ui.by('Use ComfyUI'), 'active', false);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles, []);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Their applications and files are preserved')));
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('application location picker cancellation and real errors keep the saved selection', async () => {
    for (const fileError of [{message: 'dismissed', matches: () => true}, {message: 'permission denied'}, null]) {
        const ui = await launch({...options([]), fileError, filePath: null}); ui.selectApplication(0);
        ui.by('Choose application location…').emit('clicked');
        await ui.by('Save selections for later').emit('clicked');
        assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].reference, undefined);
    }
});

test('failed discovery refresh retains editable selections and reports the technical error', async () => {
    const ui = await launch({...options([installation()]), fail: 'probe'}); ui.selectApplication(0);
    await ui.by('Check again').emit('clicked');
    assert.ok(ui.widgets.some(widget => widget.label?.startsWith('Discovery failed.')));
    assert.equal(ui.by('Check again').sensitive, true);
});

test('model-screen gear opens the same application settings and invalidates ready confirmation', async () => {
    const ui = await launch(options([installation('vllm', [{id: 'a'}, {id: 'b'}])]));
    ui.edit(ui.by('Use vLLM'), 'active', true); await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    ui.by('Settings for vLLM').emit('clicked');
    assert.equal(heading(ui).label, 'vLLM');
    assert.equal(ui.visible(ui.by('Advanced settings')), true);
    assert.equal(ui.visible(ui.by('Existing service')), false);
    assert.equal(ui.by('Finish setup').sensitive, false);
});

test('changing the model of a configured runtime keeps its stable ID and resources', async () => {
    const original = {...prepared({draft: {id: 'stable', label: 'vLLM', app: 'vllm', model: 'a', binding: {unit: 'vllm.service'}}}).profile, requiredMiB: 9000, bootPolicy: 'retain', systemdSlice: 'app.slice'};
    const ui = await launch(options([installation('vllm', [{id: 'b', label: 'B'}, {id: 'a', label: 'A'}])], {discover: {request: {...request, catalog: {version: 1, profiles: [original]}}, units: [], applications: [installation('vllm', [{id: 'b', label: 'B'}, {id: 'a', label: 'A'}])]}}));
    ui.by('Settings for vLLM').emit('clicked');
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); ui.edit(ui.by('Model'), 'selected', 1);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    const checked = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles;
    assert.equal(checked.length, 1);
    const [retained] = checked;
    assert.equal(retained.nativeModel.model, 'b');
    assert.equal(retained.id, 'stable'); assert.equal(retained.requiredMiB, 9000); assert.equal(retained.bootPolicy, 'retain'); assert.equal(retained.systemdSlice, 'app.slice');
});

test('legacy ComfyUI is selected without a new draft and remains removable from the same app card', async () => {
    const profile = {id: 'legacy', label: 'ComfyUI', adapter: 'comfyui', unit: 'comfyui.service', cgroup: '/trusted', healthURL: `${endpoint}/health`};
    const ui = await launch(options([installation()], {discover: {request: {...request, catalog: {version: 1, profiles: [profile]}}, units: [], applications: [installation()]}}));
    assert.equal(ui.by('Use ComfyUI').active, true);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles.length, 1);
    await ui.by('Back').emit('clicked'); ui.edit(ui.by('Use ComfyUI'), 'active', false);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.equal(JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input).catalog.profiles.length, 0);
});

test('removing an existing profile during batch preparation cannot replace a different profile', async () => {
    const original = prepared({draft: {id: 'stable', label: 'ComfyUI', app: 'comfyui', binding: {unit: 'comfyui.service'}}}).profile;
    const ui = await launch({...options([installation()], {discover: {request: {...request, catalog: {version: 1, profiles: [original]}}, units: [], applications: [installation()]}}), deferAction: 'prepare'});
    ui.by('Settings for ComfyUI').emit('clicked');
    const checking = (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    ui.by('Remove from supervisor').emit('clicked'); ui.finish(); await checking;
    assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
    assert.equal(ui.by('Finish setup').sensitive, false);
});

test('deselect/reselect restores a single configured model without a duplicate draft', async () => {
    const original = {...prepared({draft: {id: 'stable', label: 'Custom', app: 'vllm', model: 'a', binding: {unit: 'vllm.service'}}}).profile, requiredMiB: 7777, bootPolicy: 'retain', systemdSlice: 'app.slice'};
    const ui = await launch(options([installation('vllm', [{id: 'a'}])], {discover: {request: {...request, catalog: {version: 1, profiles: [original]}}, units: [], applications: [installation('vllm', [{id: 'a'}])]}}));
    ui.edit(ui.by('Use vLLM'), 'active', false); ui.edit(ui.by('Use vLLM'), 'active', true);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    const checked = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles;
    assert.deepEqual(checked, [{...original, nativeModel: {...original.nativeModel, launchSHA256: 'fresh'}}]);
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
});

function twoInstallations() {
    return [
        {...installation('vllm', [{id: 'a1'}, {id: 'a2'}]), label: 'Installation A', unit: 'a.service', binding: {...installation('vllm').binding, unit: 'a.service'}},
        {...installation('vllm', [{id: 'b1'}, {id: 'b2'}]), label: 'Installation B', unit: 'b.service', binding: {...installation('vllm').binding, unit: 'b.service'}},
    ];
}

test('changing installations discards the previous model selection before preparing the new service', async () => {
    const ui = await launch(options(twoInstallations())); ui.selectApplication(1);
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); ui.edit(ui.by('Model'), 'selected', 1);
    ui.by('Settings for vLLM').emit('clicked'); ui.edit(ui.by('Detected instance'), 'selected', 2);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.equal(ui.by('Model').selected, 0);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    ui.edit(ui.by('Model'), 'selected', 2); await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    const inputs = ui.calls.filter(call => call.argv[1] === 'prepare').map(call => JSON.parse(call.input));
    assert.deepEqual(inputs.map(input => [input.draft.binding.unit, input.draft.model]), [['b.service', 'b2']]);
});

for (const source of ['address', 'application location', 'model folder']) {
    test(`changing ${source} clears the old model choice`, async () => {
        const ui = await launch({...options([installation('vllm', [{id: 'a'}, {id: 'b'}])]), filePath: '/new/source'}); ui.selectApplication(1);
        await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); ui.edit(ui.by('Model'), 'selected', 1);
        ui.by('Settings for vLLM').emit('clicked');
        if (source === 'address') ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:9900');
        else ui.by(source === 'application location' ? 'Choose application location…' : 'Choose model folder...').emit('clicked');
        await ui.by('Save selections for later').emit('clicked');
        const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
        assert.equal(saved.model, undefined);
        await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
        assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    });
}

test('manual lifecycle service changes invalidate selections until a model is chosen for the new source', async () => {
    const ui = await launch(options([installation('vllm', [{id: 'a'}, {id: 'b'}])])); ui.selectApplication(1);
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); ui.edit(ui.by('Model'), 'selected', 1);
    ui.by('Settings for vLLM').emit('clicked'); ui.edit(ui.by('Existing user service'), 'text', 'new.service');
    await ui.by('Save selections for later').emit('clicked');
    assert.ok(!JSON.parse(ui.calls.at(-1).input).drafts[0].model, 'no model is selected for the new source');
    await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked'); await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    ui.edit(ui.by('Exact model ID'), 'text', 'new-model'); await (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    assert.deepEqual(ui.calls.filter(call => call.argv[1] === 'prepare').map(call => JSON.parse(call.input).draft.model), ['new-model']);
});

for (const app of ['vllm']) {
    test(`${app}: choosing an application folder clears the managed model widget and cannot become a model after another launch edit`, async () => {
        const oldPath = '/models/old';
        const draft = {id: 'draft-saved', label: app, app, reference: oldPath, referenceKind: 'model-directory', binding: {instance: 'local', owned: {port: 9000, modelPath: oldPath}}};
        const ui = await launch({...options([], {drafts: {drafts: [draft]}}), filePath: '/new/application'});
        ui.by('Settings for vLLM').emit('clicked');
        ui.by('Choose application location…').emit('clicked');
        const modelField = ui.by('Model directory');
        assert.equal(modelField.text, '');
        ui.edit(ui.by('Launch port'), 'text', '9010');
        await ui.by('Save selections for later').emit('clicked');
        const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
        assert.equal(saved.reference, '/new/application'); assert.equal(saved.referenceKind, 'application-directory');
        assert.equal(saved.binding.owned.modelPath, undefined); assert.equal(saved.binding.owned.port, 9010);
        const reopened = await launch(options([], {drafts: {drafts: [saved]}}));
        assert.equal(reopened.by('Model directory').text, '');
        await ui.by('Preview managed launch and add for review').emit('clicked');
        const preview = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input);
        assert.equal(preview.draft.binding.owned.modelPath, undefined);
    });
}

for (const [instanceStatus, expected] of [['unreachable', 'Address unreachable · Installation unverified'], ['discovery-error', 'Detection failed · Open settings'], ['invalid', 'Configuration unreadable · Open settings']]) {
    test(`compact application card keeps ${instanceStatus} distinct from missing`, async () => {
        const candidate = {...installation(), recognized: false, instanceStatus};
        const ui = await launch(options([candidate]));
        assert.ok(ui.widgets.some(widget => widget.label === expected));
        assert.equal(ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])), false);
    });
}

test('fresh setup exposes global technical settings through an accessible gear without selecting an application', async () => {
    const ui = await launch(options([]));
    for (const label of ['State database path', 'NVIDIA GPU index', 'Add existing service (Advanced)', 'Save selections for later']) assert.equal(ui.visible(ui.by(label)), false, `${label} starts behind settings`);
    assert.equal(ui.visible(ui.by('Setup settings')), true);
    await ui.click('Setup settings');
    for (const label of ['State database path', 'NVIDIA GPU index', 'Add existing service (Advanced)', 'Save selections for later']) assert.equal(ui.visible(ui.by(label)), true, `${label} can be reached through the visible gear`);
    assert.equal(ui.widgets.filter(widget => widget.accessibleProperties?.label?.startsWith('Use ') && widget.active).length, 0);
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '3');
    await ui.click('Continue');
    assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).profile.gpuIndex, 3);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

for (const saved of [false, true]) {
    test(`${saved ? 'recovered draft' : 'fresh selection'} settings make draft saving reachable through visible ancestry`, async () => {
        const initial = {id: 'saved-choice', app: 'comfyui', label: 'ComfyUI'};
        const ui = await launch(options([installation()], saved ? {drafts: {drafts: [initial]}} : {}));
        assert.equal(ui.visible(ui.by('Save selections for later')), false);
        await ui.click('Settings for ComfyUI');
        assert.equal(ui.visible(ui.by('Installation display name')), false);
        await ui.click('Advanced settings');
        await ui.click('Launch details');
        assert.equal(ui.visible(ui.by('Save selections for later')), true);
        ui.edit(ui.by('Installation display name'), 'text', 'My workflow editor');
        await ui.click('Save selections for later');
        assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].label, 'My workflow editor');
        assert.equal(ui.calls.some(call => ['apply'].includes(call.argv[1])), false);
    });
}

test('global settings gear is hidden on model and confirmation pages; Back restores it with invalidated review', async () => {
    const ui = await launch(options([installation('vllm', [{id: 'a', label: 'Model A'}, {id: 'b', label: 'Model B'}])]));
    assert.equal(ui.visible(ui.by('Use vLLM')), true);
    ui.edit(ui.by('Use vLLM'), 'active', true);
    await ui.click('Continue');
    assert.equal(heading(ui).label, 'Choose models');
    assert.equal(ui.visible(ui.by('Setup settings')), false);
    await assert.rejects(ui.click('Setup settings'), /not actionable/);
    assert.equal(ui.visible(ui.by('Model')), true);
    ui.edit(ui.by('Model'), 'selected', 1);
    await ui.click('Continue');
    assert.equal(heading(ui).label, 'Ready to finish');
    assert.equal(ui.visible(ui.by('Setup settings')), false);
    await assert.rejects(ui.click('Setup settings'), /not actionable/);
    assert.equal(ui.by('Finish setup').sensitive, true);
    await ui.click('Back');
    assert.equal(ui.by('Finish setup').sensitive, false);
    await ui.click('Back');
    assert.equal(heading(ui).label, 'Choose your applications');
    await ui.click('Setup settings');
    assert.equal(ui.visible(ui.by('NVIDIA GPU index')), true);
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '3');
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('closing setup releases display styling and singleton appearance subscriptions', async () => {
    const ui = await launch(options([]));
    assert.equal(ui.appearance.handlers.size, 2);
    assert.equal(ui.styleProviders.size, 1);
    ui.widgets.find(widget => widget.widgetType === 'ApplicationWindow').emit('unrealize');
    assert.equal(ui.appearance.handlers.size, 2, 'a window can be hidden and realized again before disposal');
    assert.equal(ui.styleProviders.size, 1);
    await ui.click('Set up later');
    assert.equal(ui.appearance.handlers.size, 0);
    assert.equal(ui.styleProviders.size, 0);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

