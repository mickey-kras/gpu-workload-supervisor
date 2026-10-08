import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
const endpoint = 'http://127.0.0.1:11434';
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
    for (const label of ['ComfyUI', 'Ollama', 'llama.cpp', 'vLLM']) {
        assert.equal(ui.by(`Use ${label}`).active, false);
        assert.equal(ui.by(`Configure ${label}`).accessibleProperties.label, `Settings for ${label}`);
        assert.ok(ui.by(`Configure ${label}`).tooltip_text);
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
    await ui.by('Continue').emit('clicked');
    assert.equal(heading(ui).label, 'Ready to finish');
    assert.deepEqual(primary(ui).map(widget => widget.label), ['Finish setup']);
    assert.equal(ui.by('Model'), undefined);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('one workload at a time')));
    assert.ok(ui.widgets.some(widget => widget.label?.includes('closes its editor')));
    await ui.by('Finish setup').emit('clicked');
    const applied = JSON.parse(ui.calls.find(call => call.argv[1] === 'apply').input);
    assert.equal(applied.confirmQuiesced, true);
    assert.equal(applied.catalog.profiles.length, 1);
});

test('Ollama is selected once; several existing models become separately named workloads, and Back preserves selections', async () => {
    const ui = await launch(options([installation('ollama', [{id: 'a', label: 'Model A'}, {id: 'b', label: 'Model B'}])]));
    ui.edit(ui.by('Use Ollama'), 'active', true);
    await ui.by('Continue').emit('clicked');
    assert.equal(heading(ui).label, 'Choose models');
    ui.edit(ui.by('Model A'), 'active', true); ui.edit(ui.by('Model B'), 'active', true);
    await ui.by('Continue').emit('clicked');
    let checked = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input);
    assert.deepEqual(checked.catalog.profiles.map(profile => profile.label), ['Ollama - Model A', 'Ollama - Model B']);
    assert.deepEqual(checked.catalog.profiles.map(profile => profile.unit), ['ollama.service', 'ollama.service']);
    await ui.by('Back').emit('clicked');
    assert.equal(heading(ui).label, 'Choose models');
    assert.equal(ui.by('Model A').active, true); assert.equal(ui.by('Model B').active, true);
    assert.equal(ui.by('Finish setup').sensitive, false);
    await ui.by('Continue').emit('clicked');
    checked = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input);
    assert.equal(checked.catalog.profiles.length, 2, 'Back then Continue replaces the staged models rather than duplicating them');
    await ui.by('Back').emit('clicked'); ui.edit(ui.by('Model B'), 'active', false);
    await ui.by('Continue').emit('clicked');
    checked = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input);
    assert.equal(checked.catalog.profiles.length, 1);
});

test('Back permits changing application without duplicate drafts or applying saved selections', async () => {
    const ui = await launch(options([installation(), installation('ollama', [{id: 'a'}, {id: 'b'}])]));
    ui.edit(ui.by('Use Ollama'), 'active', true);
    await ui.by('Continue').emit('clicked'); await ui.by('Back').emit('clicked');
    assert.equal(heading(ui).label, 'Choose your applications');
    ui.edit(ui.by('Use Ollama'), 'active', false); ui.edit(ui.by('Use ComfyUI'), 'active', true);
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
        const checking = ui.by('Continue').emit('clicked');
        await new Promise(resolve => setImmediate(resolve));
        await ui.by('Back').emit('clicked'); ui.finish(); await checking;
        assert.equal(ui.by('Finish setup').sensitive, false);
        await ui.by('Finish setup').emit('clicked');
        assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    });
}

test('duplicate installation and model replies are rejected before activation review', async () => {
    const duplicate = prepared({draft: {id: 'same', label: 'Same', app: 'ollama', model: 'same', binding: {unit: 'ollama.service'}}});
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])], {prepare: duplicate}));
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    ui.edit(ui.by('a'), 'active', true); ui.edit(ui.by('b'), 'active', true);
    await ui.by('Continue').emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('already configured')));
});

const session = {id: 'session-id', token: 'secret', status: 'completed'};
const expected = {incarnation: 'incarnation', version: 2, owner: 'supervisor', configurationRevision: 7};
function temporary(extra = {}) { return options([installation('ollama')], {'temporary-status': {available: true, expected}, 'temporary-discover': {session, models: [{id: 'existing', label: 'Existing model'}]}, ...extra}); }

test('temporary discovery is a separate consented action and does not replace ordinary model-name entry', async () => {
    const ui = await launch(temporary()); ui.edit(ui.by('Use Ollama'), 'active', true);
    await ui.by('Continue').emit('clicked');
    const start = ui.by('Start Ollama briefly to list models');
    assert.equal(start.visible, true); assert.equal(start.sensitive, false);
    assert.equal(ui.by('Existing model name').visible, true);
    await start.emit('clicked'); assert.equal(ui.calls.some(call => call.argv[1] === 'temporary-discover'), false);
    ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
    await start.emit('clicked');
    const input = JSON.parse(ui.calls.find(call => call.argv[1] === 'temporary-discover').input);
    assert.deepEqual(input, {unit: 'ollama.service', expected, consent: true, externalControlPaused: true});
    assert.equal(ui.by('Existing model').active, true);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    await ui.by('Continue').emit('clicked'); assert.equal(heading(ui).label, 'Ready to finish');
});

for (const available of [false, undefined]) {
    test(`temporary start is unavailable without authoritative capability: ${available}`, async () => {
        const ui = await launch(temporary({'temporary-status': {available}})); ui.edit(ui.by('Use Ollama'), 'active', true);
        assert.equal(ui.by('Start Ollama briefly to list models').visible, false);
        ui.edit(ui.by('Existing model name'), 'text', 'known');
        await ui.by('Continue').emit('clicked'); assert.equal(heading(ui).label, 'Ready to finish');
        assert.equal(ui.calls.some(call => call.argv[1] === 'temporary-discover'), false);
    });
}

test('temporary cleanup failure is reported, blocks Continue, and retry uses the durable token', async () => {
    const ui = await launch(temporary({'temporary-discover': {session: {...session, status: 'cleanup_required'}, models: [], error: 'stop failed'}, 'temporary-cleanup': {session}}));
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
    await ui.by('Start Ollama briefly to list models').emit('clicked');
    assert.ok(ui.widgets.some(widget => widget.label?.includes('stop failed')));
    await ui.by('Continue').emit('clicked'); assert.equal(ui.by('Finish setup').sensitive, false);
    await ui.by('Retry Ollama cleanup').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.find(call => call.argv[1] === 'temporary-cleanup').input), {id: session.id, token: session.token, externalControlPaused: true});
});

test('cancelling an in-flight temporary check waits for cleanup and never imports late inventory', async () => {
    const ui = await launch({...temporary(), deferAction: 'temporary-discover'});
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
    const checking = ui.by('Start Ollama briefly to list models').emit('clicked');
    const closing = ui.by('Set up later').emit('clicked');
    assert.equal(ui.widgets.some(widget => widget.closed), false);
    assert.deepEqual(ui.signals.map(item => item.signal), [2], 'cancel sends SIGINT to the helper rather than killing it or only discarding its reply');
    ui.finish(); await checking; await closing;
    assert.equal(ui.by('Existing model'), undefined, 'late model inventory cannot replace cancelled selections');
    assert.ok(ui.widgets.some(widget => widget.closed));
});

test('reopening a durable temporary session requires explicit stopped-state restoration before review', async () => {
    const ui = await launch(options([], {'temporary-status': (_input, calls) => calls.some(call => call.argv[1] === 'temporary-cleanup') ? {available: true, expected, session} : {available: false, session: {...session, status: 'cleanup_required'}}, 'temporary-cleanup': {session}}));
    await ui.by('Continue').emit('clicked'); assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
    await ui.by('Restore stopped application').emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'temporary-cleanup'), false, 'an unacknowledged click cannot assert paused external controls');
    ui.edit(ui.by('I have paused external application controls and finished any resumed work.'), 'active', true);
    await ui.by('Restore stopped application').emit('clicked');
    assert.equal(ui.by('Restore stopped application').sensitive, false);
    await ui.by('Continue').emit('clicked'); assert.equal(heading(ui).label, 'Ready to finish');
});

test('deselecting an existing application reviews only Supervisor removal and preserves its files', async () => {
    const profile = prepared({draft: {id: 'comfyui', label: 'ComfyUI', app: 'comfyui', binding: {unit: 'comfyui.service'}}}).profile;
    const ui = await launch({...options([installation()]), profiles: [profile], responses: {discover: {request: {...request, catalog: {version: 1, profiles: [profile]}}, units: [], applications: [installation()]}}});
    ui.by('Configure ComfyUI').emit('clicked');
    ui.edit(ui.by('Use ComfyUI'), 'active', false);
    await ui.by('Continue').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles, []);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Their applications and files are preserved')));
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

for (const action of ['close', 'defer', 'back']) {
    test(`recovered session ${action} leaves its durable block intact without claiming paused external controls`, async () => {
        const ui = await launch(options([], {'temporary-status': {session: {...session, status: 'cleanup_required'}}}));
        if (action === 'close') assert.equal(ui.widgets.find(widget => widget.title === 'Manage applications').emit('close-request'), false);
        else if (action === 'defer') { await ui.by('Set up later').emit('clicked'); assert.ok(ui.widgets.some(widget => widget.closed)); }
        else { await ui.by('Back').emit('clicked'); await ui.by('Continue').emit('clicked'); }
        assert.equal(ui.calls.some(call => call.argv[1] === 'temporary-cleanup'), false);
        assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
    });
}

for (const failure of [{error: 'cleanup refused', session: {...session, status: 'cleanup_required'}}, {}]) {
    test(`recovery cleanup never treats an incomplete reply as success: ${failure.error ?? 'missing session'}`, async () => {
        const ui = await launch(options([], {'temporary-status': {session: {...session, status: 'cleanup_required'}}, 'temporary-cleanup': failure}));
        ui.edit(ui.by('I have paused external application controls and finished any resumed work.'), 'active', true);
        await ui.by('Restore stopped application').emit('clicked');
        assert.equal(ui.by('Restore stopped application').sensitive, false, 'retry requires a fresh acknowledgement');
        await ui.by('Restore stopped application').emit('clicked');
        assert.equal(ui.calls.filter(call => call.argv[1] === 'temporary-cleanup').length, 1);
        await ui.by('Continue').emit('clicked'); assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
        await ui.by('Set up later').emit('clicked'); assert.equal(ui.widgets.some(widget => widget.closed), true);
    });
}

test('explicit cancellation signals the helper and reports failed safe cleanup without closing', async () => {
    const ui = await launch({...temporary({'temporary-discover': {session: {...session, status: 'cleanup_required'}, error: 'cancelled'}, 'temporary-cleanup': {session: {...session, status: 'cleanup_required'}, error: 'stop failed'}}), deferAction: 'temporary-discover'});
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
    const detecting = ui.by('Start Ollama briefly to list models').emit('clicked');
    const cancelling = ui.by('Cancel model detection').emit('clicked');
    ui.finish(); await detecting; await cancelling;
    assert.equal(ui.signals[0].signal, 2);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Cancellation needs cleanup')));
    await ui.by('Back').emit('clicked');
    assert.equal(heading(ui).label, 'Choose models');
    await ui.by('Remove draft from supervisor').emit('clicked');
    assert.ok(ui.widgets.some(widget => widget.label?.includes('before removing this selection')));
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
    await ui.by('Refresh discovery').emit('clicked');
    assert.ok(ui.widgets.some(widget => widget.label?.startsWith('Discovery failed.')));
    assert.equal(ui.by('Refresh discovery').sensitive, true);
});

test('model-screen gear opens the same application settings and invalidates ready confirmation', async () => {
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])]));
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    ui.by('Application settings for Ollama').emit('clicked');
    assert.equal(heading(ui).label, 'Choose your applications');
    assert.equal(ui.by('Advanced').visible, true);
    assert.equal(ui.by('Advanced').expanded, true);
    assert.equal(ui.by('Finish setup').sensitive, false);
});

test('helper subprocess failure rechecks durable status before allowing review', async () => {
    const ui = await launch({...temporary({'temporary-status': (_input, calls) => calls.some(call => call.argv[1] === 'temporary-cleanup') ? {available: true, expected, session} : calls.filter(call => call.argv[1] === 'temporary-status').length === 1 ? {available: true, expected} : {available: false, session: {...session, status: 'cleanup_required'}}, 'temporary-cleanup': {session}}), fail: 'temporary-discover'});
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
    await ui.by('Start Ollama briefly to list models').emit('clicked');
    await ui.by('Continue').emit('clicked'); assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.calls.filter(call => call.argv[1] === 'temporary-status').length, 2);
    await ui.by('Set up later').emit('clicked'); assert.ok(ui.widgets.some(widget => widget.closed));
});

test('post-install selection adds a model beneath the existing runtime while retaining stable ID and resources', async () => {
    const original = {...prepared({draft: {id: 'stable', label: 'Ollama', app: 'ollama', model: 'a', binding: {unit: 'ollama.service'}}}).profile, requiredMiB: 9000, bootPolicy: 'retain', systemdSlice: 'app.slice'};
    const ui = await launch(options([installation('ollama', [{id: 'b', label: 'B'}, {id: 'a', label: 'A'}])], {discover: {request: {...request, catalog: {version: 1, profiles: [original]}}, units: [], applications: [installation('ollama', [{id: 'b', label: 'B'}, {id: 'a', label: 'A'}])]}}));
    ui.by('Configure Ollama').emit('clicked'); ui.by('Edit application').emit('clicked');
    await ui.by('Continue').emit('clicked'); ui.edit(ui.by('B'), 'active', true);
    await ui.by('Continue').emit('clicked');
    const checked = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles;
    assert.equal(checked.length, 2);
    const retained = checked.find(profile => profile.nativeModel.model === 'a');
    assert.equal(retained.id, 'stable'); assert.equal(retained.requiredMiB, 9000); assert.equal(retained.bootPolicy, 'retain'); assert.equal(retained.systemdSlice, 'app.slice');
    assert.notEqual(checked.find(profile => profile.nativeModel.model === 'b').id, 'stable');
});

test('saved multi-model choices reopen together beneath a single runtime', async () => {
    const initial = {id: 'draft-saved', app: 'ollama', label: 'Ollama', model: 'b', models: ['a', 'b'], endpoint, binding: installation('ollama').binding};
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])], {drafts: {drafts: [initial]}}));
    assert.equal(ui.by('Use Ollama').active, true); await ui.by('Continue').emit('clicked');
    assert.equal(ui.by('a').active, true); assert.equal(ui.by('b').active, true);
    await ui.by('Save selections for later').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts[0].models, ['a', 'b']);
});

test('legacy ComfyUI is selected without a new draft and remains removable from the same app card', async () => {
    const profile = {id: 'legacy', label: 'ComfyUI', adapter: 'comfyui', unit: 'comfyui.service', cgroup: '/trusted', healthURL: `${endpoint}/health`};
    const ui = await launch(options([installation()], {discover: {request: {...request, catalog: {version: 1, profiles: [profile]}}, units: [], applications: [installation()]}}));
    assert.equal(ui.by('Use ComfyUI').active, true);
    await ui.by('Continue').emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles.length, 1);
    await ui.by('Back').emit('clicked'); ui.edit(ui.by('Use ComfyUI'), 'active', false);
    await ui.by('Continue').emit('clicked');
    assert.equal(JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input).catalog.profiles.length, 0);
});

test('removing an existing profile during batch preparation cannot replace a different profile', async () => {
    const original = prepared({draft: {id: 'stable', label: 'ComfyUI', app: 'comfyui', binding: {unit: 'comfyui.service'}}}).profile;
    const ui = await launch({...options([installation()], {discover: {request: {...request, catalog: {version: 1, profiles: [original]}}, units: [], applications: [installation()]}}), deferAction: 'prepare'});
    ui.by('Configure ComfyUI').emit('clicked'); ui.by('Edit application').emit('clicked');
    const checking = ui.by('Continue').emit('clicked');
    ui.by('Remove from supervisor').emit('clicked'); ui.finish(); await checking;
    assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
    assert.equal(ui.by('Finish setup').sensitive, false);
});

test('configured Ollama presents its model group in the ordinary journey without requiring a gear', async () => {
    const original = {...prepared({draft: {id: 'stable', label: 'Custom A', app: 'ollama', model: 'a', binding: {unit: 'ollama.service'}}}).profile, requiredMiB: 4000};
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])], {discover: {request: {...request, catalog: {version: 1, profiles: [original]}}, units: [], applications: [installation('ollama', [{id: 'a'}, {id: 'b'}])]}}));
    await ui.by('Continue').emit('clicked'); assert.equal(heading(ui).label, 'Choose models');
    assert.equal(ui.by('a').active, true); ui.edit(ui.by('b'), 'active', true);
    await ui.by('Continue').emit('clicked');
    const checked = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles;
    assert.deepEqual(checked.map(profile => profile.label), ['Custom A', 'Ollama - b']);
    assert.equal(checked[0].id, 'stable'); assert.equal(checked[0].requiredMiB, 4000);
});

for (const next of ['continue and close', 'another temporary check']) {
    test(`same-process failed check cleanup refreshes its shared fence before ${next}`, async () => {
        const freshExpected = {...expected, version: 4};
        const ui = await launch(temporary({
            'temporary-discover': (_input, calls) => calls.filter(call => call.argv[1] === 'temporary-discover').length === 1 ? {session: {...session, status: 'cleanup_required'}, error: 'start cancelled'} : {session, models: [{id: 'existing'}]},
            'temporary-cleanup': {session},
            'temporary-status': (_input, calls) => calls.some(call => call.argv[1] === 'temporary-cleanup') ? {available: true, expected: freshExpected, session} : calls.some(call => call.argv[1] === 'temporary-discover') ? {available: false, session: {...session, status: 'cleanup_required'}} : {available: true, expected},
        }));
        ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
        ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
        await ui.by('Start Ollama briefly to list models').emit('clicked');
        await ui.by('Retry Ollama cleanup').emit('clicked');
        assert.equal(ui.calls.filter(call => call.argv[1] === 'temporary-cleanup').length, 1);
        if (next === 'another temporary check') {
            ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
            await ui.by('Start Ollama briefly to list models').emit('clicked');
            const inputs = ui.calls.filter(call => call.argv[1] === 'temporary-discover').map(call => JSON.parse(call.input));
            assert.equal(inputs.length, 2); assert.deepEqual(inputs[1].expected, freshExpected);
        } else ui.edit(ui.by('Existing model name'), 'text', 'known');
        await ui.by('Continue').emit('clicked');
        assert.equal(heading(ui).label, 'Ready to finish');
        await ui.by('Set up later').emit('clicked'); assert.ok(ui.widgets.some(widget => widget.closed));
        assert.equal(ui.calls.filter(call => call.argv[1] === 'temporary-cleanup').length, 1, 'completion is synchronized globally and never cleaned a second time');
    });
}

test('window cancellation of a locally authorized temporary check still waits for supervised cleanup', async () => {
    const ui = await launch({...temporary(), deferAction: 'temporary-discover'});
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    ui.edit(ui.by('I allow this brief start and will keep other application controls paused.'), 'active', true);
    const detecting = ui.by('Start Ollama briefly to list models').emit('clicked');
    assert.equal(ui.widgets.find(widget => widget.title === 'Manage applications').emit('close-request'), true);
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(ui.widgets.some(widget => widget.closed), false); assert.equal(ui.signals[0].signal, 2);
    ui.finish(); await detecting; await new Promise(resolve => setImmediate(resolve));
    assert.ok(ui.widgets.some(widget => widget.closed));
});

test('deselect/reselect restores original model IDs and deliberately edited resources without fresh defaults', async () => {
    const model = (id, nativeModel, label, requiredMiB) => ({...prepared({draft: {id, label, app: 'ollama', model: nativeModel, binding: {unit: 'ollama.service'}}}).profile, requiredMiB, bootPolicy: 'retain', systemdSlice: 'app.slice'});
    const originals = [model('stable-a', 'a', 'Custom A', 7777), model('stable-b', 'b', 'Custom B', 8888)];
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])], {discover: {request: {...request, catalog: {version: 1, profiles: originals}}, units: [], applications: [installation('ollama', [{id: 'a'}, {id: 'b'}])]}}));
    ui.edit(ui.by('Measured VRAM requirement (MiB; optional)'), 'text', '7999');
    ui.edit(ui.by('Use Ollama'), 'active', false); ui.edit(ui.by('Use Ollama'), 'active', true);
    await ui.by('Continue').emit('clicked');
    assert.equal(ui.by('a').active, true); assert.equal(ui.by('b').active, true);
    await ui.by('Continue').emit('clicked');
    await ui.by('Back').emit('clicked'); await ui.by('Continue').emit('clicked');
    const checked = JSON.parse(ui.calls.filter(call => call.argv[1] === 'validate').at(-1).input).catalog.profiles;
    assert.deepEqual(checked.map(profile => ({id: profile.id, label: profile.label, requiredMiB: profile.requiredMiB, bootPolicy: profile.bootPolicy, systemdSlice: profile.systemdSlice})), [
        {id: 'stable-a', label: 'Custom A', requiredMiB: 7999, bootPolicy: 'retain', systemdSlice: 'app.slice'},
        {id: 'stable-b', label: 'Custom B', requiredMiB: 8888, bootPolicy: 'retain', systemdSlice: 'app.slice'},
    ]);
});

test('deselect/reselect restores a single configured model without a duplicate draft', async () => {
    const original = {...prepared({draft: {id: 'stable', label: 'Custom', app: 'ollama', model: 'a', binding: {unit: 'ollama.service'}}}).profile, requiredMiB: 7777, bootPolicy: 'retain', systemdSlice: 'app.slice'};
    const ui = await launch(options([installation('ollama', [{id: 'a'}])], {discover: {request: {...request, catalog: {version: 1, profiles: [original]}}, units: [], applications: [installation('ollama', [{id: 'a'}])]}}));
    ui.edit(ui.by('Use Ollama'), 'active', false); ui.edit(ui.by('Use Ollama'), 'active', true);
    await ui.by('Continue').emit('clicked');
    const checked = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles;
    assert.deepEqual(checked, [{...original, nativeModel: {...original.nativeModel, launchSHA256: 'fresh'}}]);
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
});

function twoInstallations() {
    return [
        {...installation('ollama', [{id: 'a1'}, {id: 'a2'}]), label: 'Installation A', unit: 'a.service', binding: {...installation('ollama').binding, unit: 'a.service'}},
        {...installation('ollama', [{id: 'b1'}, {id: 'b2'}]), label: 'Installation B', unit: 'b.service', binding: {...installation('ollama').binding, unit: 'b.service'}},
    ];
}

test('changing installations discards the previous multi-model selection before preparing the new service', async () => {
    const ui = await launch(options(twoInstallations())); ui.selectApplication(1);
    ui.edit(ui.by('Detected instance'), 'selected', 1);
    await ui.by('Continue').emit('clicked'); ui.edit(ui.by('a1'), 'active', true); ui.edit(ui.by('a2'), 'active', true);
    ui.by('Application settings for Ollama').emit('clicked'); ui.edit(ui.by('Detected instance'), 'selected', 2);
    await ui.by('Continue').emit('clicked');
    assert.equal(ui.by('b1').active, false); assert.equal(ui.by('b2').active, false);
    await ui.by('Continue').emit('clicked'); assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    ui.edit(ui.by('b2'), 'active', true); await ui.by('Continue').emit('clicked');
    const inputs = ui.calls.filter(call => call.argv[1] === 'prepare').map(call => JSON.parse(call.input));
    assert.deepEqual(inputs.map(input => [input.draft.binding.unit, input.draft.model]), [['b.service', 'b2']]);
});

for (const source of ['address', 'application location', 'model file']) {
    test(`changing ${source} clears old grouped model choices`, async () => {
        const ui = await launch({...options([installation('ollama', [{id: 'a'}, {id: 'b'}])]), filePath: '/new/source'}); ui.selectApplication(1);
        await ui.by('Continue').emit('clicked'); ui.edit(ui.by('a'), 'active', true); ui.edit(ui.by('b'), 'active', true);
        ui.by('Application settings for Ollama').emit('clicked');
        if (source === 'address') ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:9900');
        else ui.by(source === 'application location' ? 'Choose application location…' : 'Choose model file...').emit('clicked');
        await ui.by('Save selections for later').emit('clicked');
        const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
        assert.equal(saved.models, undefined); assert.equal(saved.model, undefined);
        await ui.by('Continue').emit('clicked'); await ui.by('Continue').emit('clicked');
        assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    });
}

for (const selected of [['a', 'b'], []]) {
    test(`ordinary manual model entry replaces saved multi-model choices: ${JSON.stringify(selected)}`, async () => {
        const saved = {id: 'draft-saved', label: 'Ollama', app: 'ollama', model: 'a', models: selected, endpoint, binding: installation('ollama').binding};
        const ui = await launch(options([installation('ollama')], {drafts: {drafts: [saved]}}));
        await ui.by('Continue').emit('clicked');
        ui.edit(ui.by('Existing model name'), 'text', 'c'); await ui.by('Continue').emit('clicked');
        const inputs = ui.calls.filter(call => call.argv[1] === 'prepare').map(call => JSON.parse(call.input));
        assert.deepEqual(inputs.map(input => input.draft.model), ['c']);
    });
}

test('manual owned-model edits replace grouped selections and synchronize the visible inventory checks', async () => {
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])], {'render-owned': prepared})); ui.selectApplication(1);
    await ui.by('Continue').emit('clicked'); ui.edit(ui.by('a'), 'active', true); ui.edit(ui.by('b'), 'active', true);
    ui.by('Application settings for Ollama').emit('clicked'); ui.edit(ui.by('Model name'), 'text', 'c');
    await ui.by('Save selections for later').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0]; assert.equal(saved.models, undefined); assert.equal(saved.model, 'c');
    assert.equal(ui.by('a').active, false); assert.equal(ui.by('b').active, false);
});

test('refreshing the same installation preserves only its available selected models', async () => {
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])], {probe: {...installation('ollama', [{id: 'b'}, {id: 'a'}]), inventoryStatus: 'available'}})); ui.selectApplication(1);
    await ui.by('Continue').emit('clicked'); ui.edit(ui.by('a'), 'active', true); ui.edit(ui.by('b'), 'active', true);
    await ui.by('Refresh discovery').emit('clicked');
    await ui.by('Save selections for later').emit('clicked'); assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts[0].models, ['a', 'b']);
});

test('a new temporary attempt cannot inherit a completed record when its helper and status both fail', async () => {
    const ui = await launch(temporary({
        'temporary-discover': (_input, calls) => {
            if (calls.filter(call => call.argv[1] === 'temporary-discover').length > 1) throw new Error('helper failed');
            return {session, models: []};
        },
        'temporary-status': (_input, calls) => {
            if (calls.filter(call => call.argv[1] === 'temporary-discover').length > 1) throw new Error('status failed');
            return {available: true, expected, ...(calls.some(call => call.argv[1] === 'temporary-discover') ? {session} : {})};
        },
    }));
    ui.edit(ui.by('Use Ollama'), 'active', true); await ui.by('Continue').emit('clicked');
    const consent = ui.by('I allow this brief start and will keep other application controls paused.');
    ui.edit(consent, 'active', true); await ui.by('Start Ollama briefly to list models').emit('clicked');
    ui.edit(consent, 'active', true); await ui.by('Start Ollama briefly to list models').emit('clicked');
    ui.edit(ui.by('Existing model name'), 'text', 'known'); await ui.by('Continue').emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false); assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.by('Retry Ollama cleanup').sensitive, false);
    await ui.by('Retry Ollama cleanup').emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'temporary-cleanup'), false, 'unknown session IDs and tokens are never guessed');
    await ui.by('Set up later').emit('clicked'); assert.ok(ui.widgets.some(widget => widget.closed));
    assert.ok(ui.widgets.some(widget => widget.label?.includes('preserves its durable recovery block')));
});

test('manual lifecycle service changes invalidate selections until a model is chosen for the new source', async () => {
    const ui = await launch(options([installation('ollama', [{id: 'a'}, {id: 'b'}])])); ui.selectApplication(1);
    await ui.by('Continue').emit('clicked'); ui.edit(ui.by('a'), 'active', true); ui.edit(ui.by('b'), 'active', true);
    ui.by('Application settings for Ollama').emit('clicked'); ui.edit(ui.by('Existing user service'), 'text', 'new.service');
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].models, undefined);
    await ui.by('Continue').emit('clicked'); await ui.by('Continue').emit('clicked');
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    ui.edit(ui.by('Exact model ID'), 'text', 'new-model'); await ui.by('Continue').emit('clicked');
    assert.deepEqual(ui.calls.filter(call => call.argv[1] === 'prepare').map(call => JSON.parse(call.input).draft.model), ['new-model']);
});

for (const app of ['llama.cpp', 'vllm']) {
    test(`${app}: choosing an application folder clears the managed model widget and cannot become a model after another launch edit`, async () => {
        const oldPath = app === 'llama.cpp' ? '/models/old.gguf' : '/models/old';
        const draft = {id: 'draft-saved', label: app, app, reference: oldPath, referenceKind: app === 'llama.cpp' ? 'model-file' : 'model-directory', binding: {instance: 'local', owned: {port: 9000, modelPath: oldPath}}};
        const ui = await launch({...options([], {drafts: {drafts: [draft]}}), filePath: '/new/application'});
        ui.by(`Configure ${app === 'vllm' ? 'vLLM' : 'llama.cpp'}`).emit('clicked');
        ui.by('Choose application location…').emit('clicked');
        const modelField = ui.by(app === 'llama.cpp' ? 'Model file' : 'Model directory');
        assert.equal(modelField.text, '');
        ui.edit(ui.by('Launch port'), 'text', '9010');
        await ui.by('Save selections for later').emit('clicked');
        const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
        assert.equal(saved.reference, '/new/application'); assert.equal(saved.referenceKind, 'application-directory');
        assert.equal(saved.binding.owned.modelPath, undefined); assert.equal(saved.binding.owned.port, 9010);
        const reopened = await launch(options([], {drafts: {drafts: [saved]}}));
        assert.equal(reopened.by(app === 'llama.cpp' ? 'Model file' : 'Model directory').text, '');
        await ui.by('Preview managed launch and add for review').emit('clicked');
        const preview = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input);
        assert.equal(preview.draft.binding.owned.modelPath, undefined);
    });
}
