import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';

test('large discovery stays compact and adopts no service until explicit selection', async () => {
    const ui = await launch({units: Array.from({length: 10000}, (_, i) => `worker-${i}.service`)});
    assert.ok(ui.widgets.every(widget => (widget.label ?? '').length < 1000));
    assert.deepEqual(ui.calls.map(call => call.argv[1]), ['--version', 'discover', 'temporary-status', 'drafts']);
    ui.by('Add existing service (Advanced)').emit('clicked');
    const service = ui.by('Existing user service');
    assert.ok(service, 'service selection is available in each workload card');
    assert.equal(service.enable_search, true);
    assert.ok(service.expression, 'search must extract the model item string');
    const matches = Array.from({length: service.model.get_n_items()}, (_, index) => {
        const [ok, value] = service.expression.evaluate(service.model.get_item(index));
        return ok && value.includes('worker-9999') ? value : null;
    }).filter(Boolean);
    assert.deepEqual(matches, ['worker-9999.service']);
    assert.equal(service.selected, 0);
    ui.edit(service, 'selected', 10000, 'notify::selected');
    const validating = ui.by('Continue').emit('clicked');
    const sent = JSON.parse(ui.calls.at(-1).input);
    assert.equal(sent.catalog.profiles[0].unit, 'worker-9999.service');
    assert.equal(sent.catalog.profiles[0].cgroup, undefined);
    assert.equal(sent.catalog.profiles[0].healthURL, undefined);
    assert.equal(sent.catalog.profiles[0].requiredMiB, undefined);
    ui.finish(); await validating;
});

test('editing invalidates review and requires fresh explicit confirmation', async () => {
    const ui = await launch({profiles: [{id: 'stable', label: 'Existing', unit: 'old.service', cgroup: '/old', healthURL: 'http://localhost:1', custom: 'keep'}]});
    const reviewing = ui.by('Continue').emit('clicked');
    ui.finish(); await reviewing;


    assert.equal(ui.by('Finish setup').sensitive, true);
    ui.edit(ui.by('Display name'), 'text', 'Renamed', 'changed');
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.by('Finish setup').sensitive, false);
    const second = ui.by('Continue').emit('clicked');
    ui.finish(); await second;
    assert.equal(ui.by('Finish setup').sensitive, true, 'Finish setup is the explicit confirmation for the freshly reviewed snapshot');
    await ui.by('Finish setup').emit('clicked');
    const applied = JSON.parse(ui.calls.at(-1).input);
    assert.equal(applied.catalog.profiles[0].id, 'stable');
    assert.equal(applied.catalog.profiles[0].custom, 'keep');
    assert.equal(applied.catalog.profiles[0].label, 'Renamed');
    assert.deepEqual(applied.catalog.extra, {keep: true});
    assert.equal(applied.profile.extra, 'profile');
    assert.equal(applied.extra, 'request');
    assert.equal(applied.confirmQuiesced, true);
});

test('pending activation reviews and resumes its exact original request', async () => {
    const ui = await launch({pending: true, profiles: [{id: 'stable', label: 'Existing', unit: 'missing.service', opaque: {keep: true}}]});
    assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
    const reviewing = ui.by('Continue').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input), ui.request);
    ui.finish(); await reviewing;


    await ui.by('Finish setup').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input), {...ui.request, confirmQuiesced: true});
    assert.equal(ui.calls.filter(call => call.argv[1] === 'verify-bindings').length, 0);
});

test('late validation cannot enable applying edited settings', async () => {
    const ui = await launch();
    const reviewing = ui.by('Continue').emit('clicked');
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '3', 'changed');
    ui.finish(); await reviewing;


    await ui.by('Finish setup').emit('clicked');
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
});


test('technical workload fields stay collapsed and removing a card removes only that profile', async () => {
    const ui = await launch({profiles: [{id: 'keep', unit: 'keep.service', label: 'Keep'}, {id: 'edit', unit: 'edit.service', label: 'Edit'}]});
    const group = ui.by('Edit');
    for (const title of ['Existing user service', 'Cgroup path beneath /sys/fs/cgroup (required)', 'Loopback health URL (required)'])
        assert.ok(group.children.find(widget => widget.title === 'Advanced').children.includes(ui.widgets.filter(widget => widget.title === title)[1]), `technical field stays in Advanced: ${title}`);
    assert.equal(group.children.find(widget => widget.title === 'Advanced').expanded, false);
    group.children.find(widget => widget.label === 'Remove from supervisor').emit('clicked');
    const reviewing = ui.by('Continue').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).catalog.profiles, [{id: 'keep', unit: 'keep.service', label: 'Keep'}]);
    ui.finish(); await reviewing;
});

for (const failure of ['discover', 'validate', 'apply']) {
    test(`${failure} failure cannot enable an unreviewed or repeated apply`, async () => {
        const ui = await launch({fail: failure});
        if (failure === 'discover') {
            assert.equal(ui.by('Continue').sensitive, false);
            assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
        } else {
            const reviewing = ui.by('Continue').emit('clicked');
            ui.finish(); await reviewing;

            if (failure === 'apply') {

                await ui.by('Finish setup').emit('clicked');
                assert.equal(ui.by('Finish setup').sensitive, false);
                assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
                assert.equal(ui.by('Set up later').sensitive, true, 'failed apply must not trap the user');
                assert.equal(ui.by('Continue').sensitive, true, 'recovery requires a fresh review');
            }
            await ui.by('Finish setup').emit('clicked');
            assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, failure === 'apply' ? 1 : 0);
        }
        assert.equal(ui.by('Finish setup').sensitive, false);
        assert.ok(ui.widgets.some(widget => widget.label?.includes('Backend unavailable')));
    });
}

test('apply failure keeps the raw backend error collapsed under the actionable summary', async () => {
    const ui = await launch({fail: 'apply'});
    const reviewing = ui.by('Continue').emit('clicked');
    ui.finish(); await reviewing;


    await ui.by('Finish setup').emit('clicked');
    const details = ui.by('Technical details');
    assert.equal(details.visible, true);
    assert.equal(details.expanded, false);
    assert.ok(details.children.some(child => child.label === 'Backend unavailable'));
    assert.ok(ui.widgets.some(widget => widget.label?.startsWith('Setup needs attention.')));
});

test('validation failure names the next action and collapses the raw error', async () => {
    const ui = await launch({fail: 'validate'});
    const reviewing = ui.by('Continue').emit('clicked');
    ui.finish(); await reviewing;
    const details = ui.by('Technical details');
    assert.equal(details.visible, true);
    assert.ok(details.children.some(child => child.label === 'Backend unavailable'));
    assert.ok(ui.widgets.some(widget => widget.label?.includes('reopen Manage workloads to refresh, then review again')));
    assert.equal(ui.by('Finish setup').sensitive, false);
});

test('invalid numerical input fails before backend validation and can be corrected', async () => {
    const ui = await launch();
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '-1', 'changed');
    await ui.by('Continue').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'validate').length, 0);
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.by('Continue').sensitive, true);
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '0', 'changed');
    ui.by('Add existing service (Advanced)').emit('clicked');
    const vram = ui.by('Measured VRAM requirement (MiB; optional)');
    ui.edit(vram, 'text', '123.5', 'changed');
    await ui.by('Continue').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'validate').length, 0);
    ui.edit(vram, 'text', '', 'changed');
    const reviewing = ui.by('Continue').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].requiredMiB, undefined);
    ui.finish(); await reviewing;
});

test('unsupported desktop never discovers or applies workloads', async () => {
    const ui = await launch({version: 'GNOME Shell 49.0'});
    assert.deepEqual(ui.calls.map(call => call.argv[1]), ['--version']);
    assert.equal(ui.by('Continue').sensitive, false);
    assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
});

test('manual service names preserve template instances and picker updates the same request field', async () => {
    const ui = await launch({units: ['known.service']});
    ui.by('Add existing service (Advanced)').emit('clicked');
    const manual = ui.by('Service name (manual entry)');
    assert.ok(manual, 'undiscovered services remain configurable');
    const service = ui.by('Existing user service');
    ui.edit(service, 'selected', 2, 'notify::selected');
    assert.equal(ui.by('Advanced').expanded, true);
    assert.equal(manual.focused, true);
    ui.edit(manual, 'text', 'worker@model.service', 'changed');
    const reviewing = ui.by('Continue').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].unit, 'worker@model.service');
    ui.finish(); await reviewing;
    ui.edit(service, 'selected', 1, 'notify::selected');
    assert.equal(manual.text, 'known.service');
    assert.equal(ui.by('Finish setup').sensitive, false);
    const second = ui.by('Continue').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].unit, 'known.service');
    ui.finish(); await second;
});


test('workload titles escape markup while reviewed labels retain their exact text', async () => {
    const ui = await launch({profiles: [{id: 'literal', label: '<b>GPU & work</b>', unit: 'literal.service'}]});
    const group = ui.widgets.find(widget => widget.description?.startsWith('Start and stop'));
    assert.equal(group.title, '&lt;b&gt;GPU &amp; work&lt;/b&gt;');
    ui.edit(ui.by('Display name'), 'text', 'Render < & >');
    assert.equal(group.title, 'Render &lt; &amp; &gt;');
    const reviewing = ui.by('Continue').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].label, 'Render < & >');
    ui.finish(); await reviewing;
});


test('app-first draft is separate from catalog and deferral never applies', async () => {
    const ui = await launch();
    ui.selectApplication(ui.applicationIndex ?? 0);
    assert.ok(ui.by('Friendly name'));
    assert.ok(ui.widgets.some(widget => widget.label?.startsWith('ComfyUI workflows')));
    assert.equal(ui.by('Model'), undefined);
    await ui.by('Save selections for later').emit('clicked');
    const saved = ui.calls.find(call => call.argv[1] === 'save-drafts');
    assert.equal(JSON.parse(saved.input).drafts[0].app, 'comfyui');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
    await ui.by('Set up later').emit('clicked');
    assert.ok(ui.widgets.some(widget => widget.closed));
});

for (const [index, app] of ['comfyui', 'ollama', 'llama.cpp', 'vllm'].entries()) {
    test(`${app} discovery remains a draft until explicit binding verification`, async () => {
        const ui = await launch({responses: {probe: {app, instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'one', label: 'One'}, {id: 'two', label: 'Two'}]}}});
        ui.applicationIndex = index;
        ui.selectApplication(ui.applicationIndex ?? 0);
        ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:11434');
        await ui.by('Refresh discovery').emit('clicked');
        assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'probe').input).app, app);
        if (app !== 'comfyui') {
            ui.edit(ui.by('Model'), 'selected', 1);
            ui.by('Choose model file...').emit('clicked');
            await ui.by('Refresh discovery').emit('clicked');
            const probe = JSON.parse(ui.calls.at(-1).input);
            assert.equal(probe.reference, '/models/selected.gguf');
            assert.equal(probe.referenceKind, 'model-file');
        }
        assert.equal(ui.calls.filter(call => ['apply', 'verify-bindings'].includes(call.argv[1])).length, 0);
        await ui.by('Verify binding and add for review').emit('clicked');
        assert.equal(ui.calls.at(-1).argv[1], 'verify-bindings');
        const bound = JSON.parse(ui.calls.at(-1).input).catalog.profiles[0];
        assert.equal(bound.nativeModel?.runtime, app === 'comfyui' ? undefined : app);
        assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
    });
}

test('promoted binding prefills a friendly label and stable ID; application re-selection adds no duplicate', async () => {
    const ui = await launch({responses: {probe: {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'qwen:latest'}]}, fingerprint: {sha256: 'fp'}}});
    const promote = async () => {
        ui.applicationIndex = 1;
        ui.selectApplication(ui.applicationIndex ?? 0);
        ui.edit(ui.widgets.filter(widget => widget.title === 'Application address').at(-1), 'text', 'http://127.0.0.1:11434');
        await ui.widgets.filter(widget => widget.label === 'Refresh discovery').at(-1).emit('clicked');
        ui.edit(ui.widgets.filter(widget => widget.title === 'Model').at(-1), 'selected', 1);
        await ui.widgets.filter(widget => widget.label === 'Verify binding and add for review').at(-1).emit('clicked');
        return JSON.parse(ui.calls.filter(call => call.argv[1] === 'verify-bindings').at(-1).input).catalog.profiles.at(-1);
    };
    const first = await promote();
    assert.equal(first.id, 'ollama-qwen-latest');
    assert.equal(first.label, 'Ollama - qwen:latest');
    const count = ui.calls.filter(call => call.argv[1] === 'verify-bindings').length;
    ui.selectApplication(1);
    assert.equal(ui.calls.filter(call => call.argv[1] === 'verify-bindings').length, count);
    await ui.by('Save selections for later').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts, []);
});

test('failed verification keeps draft and supports retry; removing draft never applies', async () => {
    const ui = await launch({fail: 'verify-bindings'});
    ui.selectApplication(ui.applicationIndex ?? 0);
    await ui.by('Verify binding and add for review').emit('clicked');
    assert.equal(ui.by('Verify binding and add for review').sensitive, true);
    await ui.by('Remove draft from supervisor').emit('clicked');
    await ui.by('Save selections for later').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts, []);
});

test('late binding cannot promote removed draft', async () => {
    const ui = await launch({deferAction: 'verify-bindings'});
    ui.selectApplication(ui.applicationIndex ?? 0);
    const binding = ui.by('Verify binding and add for review').emit('clicked');
    await ui.by('Remove draft from supervisor').emit('clicked');
    ui.finish(); await binding;
    assert.equal(ui.widgets.filter(widget => widget.title === 'Display name').length, 0);
    await ui.by('Save selections for later').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts, []);
});

test('choosing a model folder stores a directory reference instead of an endpoint', async () => {
    const ui = await launch({filePath: '/models/llama-directory'});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    ui.by('Choose model folder...').emit('clicked');
    await ui.by('Save selections for later').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.reference, '/models/llama-directory');
    assert.equal(saved.referenceKind, 'model-directory');
    assert.equal(saved.endpoint, undefined);
});

test('cancelled file picker preserves draft input', async () => {
    const ui = await launch({fileError: {matches: () => true}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    ui.by('Choose model file...').emit('clicked');
    await ui.by('Save selections for later').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].reference, undefined);
});

test('native launch fingerprint is computed before binding verification', async () => {
    const ui = await launch({responses: {fingerprint: {sha256: 'verified-fingerprint'}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    ui.edit(ui.by('Loaded service file path'), 'text', '/home/user/.config/systemd/user/model.service');
    await ui.by('Verify binding and add for review').emit('clicked');
    const fingerprint = ui.calls.find(call => call.argv[1] === 'fingerprint');
    assert.ok(fingerprint);
    assert.equal(JSON.parse(fingerprint.input).binding.launchFile, '/home/user/.config/systemd/user/model.service');
    const binding = JSON.parse(ui.calls.find(call => call.argv[1] === 'verify-bindings').input);
    assert.equal(binding.catalog.profiles[0].nativeModel.launchSHA256, 'verified-fingerprint');
});


test('editing a draft during fingerprinting cannot promote the old binding', async () => {
    const ui = await launch({deferAction: 'fingerprint', responses: {fingerprint: {sha256: 'old'}}});
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    const checking = ui.by('Verify binding and add for review').emit('clicked');
    ui.edit(ui.by('Friendly name'), 'text', 'Changed');
    ui.edit(ui.by('Loaded service file path'), 'text', '/changed.service');
    ui.finish(); await checking;
    assert.equal(ui.calls.filter(call => call.argv[1] === 'verify-bindings').length, 0);
    assert.equal(ui.by('Verify binding and add for review').sensitive, true);
});

test('review refreshes configured native launch fingerprints automatically', async () => {
    const ui = await launch({profiles: [{id: 'text', nativeModel: {runtime: 'ollama', model: 'chosen', launchFile: '/trusted/model.service', launchSHA256: 'old'}}],
        responses: {fingerprint: {sha256: 'new'}}, deferAction: 'none'});
    assert.equal(ui.by('Service file SHA-256'), undefined);
    await ui.by('Continue').emit('clicked');
    const validation = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input);
    assert.equal(validation.catalog.profiles[0].nativeModel.launchSHA256, 'new');
    await ui.by('Finish setup').emit('clicked');
    const applied = JSON.parse(ui.calls.find(call => call.argv[1] === 'apply').input);
    assert.equal(applied.catalog.profiles[0].nativeModel.launchSHA256, 'new');
});

test('Save drafts restores unverified launch fields without fingerprint proof', async () => {
    const ui = await launch();
    ui.applicationIndex = 1; ui.selectApplication(ui.applicationIndex ?? 0);
    for (const [label, value] of [['Existing user service', 'ollama-custom.service'], ['Cgroup path', '/custom'], ['Loopback health URL', 'http://127.0.0.1:11434'], ['Runtime instance ID', 'custom'], ['Loaded service file path', '/trusted/model.service'], ['Exact model ID', 'chosen']])
        ui.edit(ui.by(label), 'text', value);
    await ui.by('Save selections for later').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input);
    assert.equal(saved.drafts[0].binding.unit, 'ollama-custom.service');
    assert.equal(saved.drafts[0].binding.model, 'chosen');
    assert.equal('launchSHA256' in saved.drafts[0].binding, false);
    const reopened = await launch({responses: {drafts: saved}});
    assert.equal(reopened.by('Existing user service').text, 'ollama-custom.service');
    assert.equal(reopened.by('Exact model ID').text, 'chosen');
    assert.equal(reopened.calls.some(call => call.argv[1] === 'verify-bindings'), false);
});
