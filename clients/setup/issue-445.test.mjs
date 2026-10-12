import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {candidateMessage} from './onboarding.mjs';

const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}};
const installation = {app: 'comfyui', label: 'ComfyUI', recognized: true, instanceStatus: 'not-running', configurationStatus: 'ready',
    unit: 'comfyui.service', endpoint: 'http://127.0.0.1:8188', models: [],
    binding: {unit: 'comfyui.service', cgroup: '/comfyui', healthURL: 'http://127.0.0.1:8188/system_stats'}};
const profile = {id: 'images', label: 'ComfyUI', adapter: 'systemd', unit: installation.unit, cgroup: '/comfyui',
    healthURL: installation.binding.healthURL, bootPolicy: 'stop-to-idle'};
function options(candidates, responses = {}) {
    return {deferAction: 'unused', responses: {discover: {request, units: [], applications: candidates}, prepare: {profile}, ...responses}};
}
const heading = ui => ui.widgets.find(widget => widget.cssClasses?.includes('title-1'));

for (const status of ['invalid', 'missing', 'unsupported', 'inspection-failed', 'ambiguous', 'discovery-error']) {
    test(`included ${status} installation stays inspectable and cannot Use installation even with contradictory readiness`, async () => {
        const ui = await launch(options([{...installation, instanceStatus: status}]));
        ui.edit(ui.by('Use ComfyUI'), 'active', true);
        assert.equal(ui.by('Use ComfyUI').active, true, 'the checkbox records include-in-setup intent');
        await ui.click('Settings for ComfyUI');
        assert.equal(ui.by('Use installation').sensitive, false);
        assert.equal(ui.visible(ui.by('Needs setup')), true);
        await ui.click('View details');
        assert.equal(heading(ui).label, 'Configuration details');
        assert.equal(ui.visible(ui.by('Copy details')), true);
        await ui.click('Cancel');
        assert.equal(ui.by('Use ComfyUI').active, true);
        assert.equal(ui.by('Settings for ComfyUI').focused, true);
        assert.equal(ui.by('Finish setup').sensitive, false);
        assert.equal(ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])), false);
    });
}

test('a recognized installation with an unreachable address can be configured and revalidated without startup', async () => {
    const unreachable = {...installation, instanceStatus: 'unreachable'};
    const ui = await launch(options([unreachable]));
    assert.ok(ui.widgets.some(widget => widget.label === 'Installed · Address unreachable · Ready to configure'));
    assert.match(candidateMessage(unreachable), /Installation recognized/);
    await ui.click('Settings for ComfyUI');
    assert.equal(ui.by('Use installation').sensitive, true);
    await ui.click('Use installation');
    assert.equal(ui.by('Finish setup').sensitive, true);
    assert.ok(ui.calls.some(call => call.argv[1] === 'verify-bindings'));
    assert.equal(ui.calls.some(call => ['apply', 'temporary-discover'].includes(call.argv[1])), false);
});

test('Use installation cannot omit another included unresolved application; deselection lets the valid rest finish', async () => {
    const unresolved = {app: 'vllm', label: 'vLLM', instanceStatus: 'unreachable', configurationStatus: 'unverified',
        sourceKind: 'endpoint', endpoint: 'http://127.0.0.1:8000', recognized: false, models: []};
    const ui = await launch(options([installation, unresolved]));
    ui.edit(ui.by('Use ComfyUI'), 'active', true); ui.edit(ui.by('Use vLLM'), 'active', true);
    await ui.click('Settings for ComfyUI'); await ui.click('Use installation');
    assert.equal(heading(ui).label, 'Choose models');
    await ui.click('Continue');
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.calls.some(call => call.argv[1] === 'validate'), false);
    await ui.click('Back'); ui.edit(ui.by('Use vLLM'), 'active', false);
    await ui.click('Continue');
    assert.equal(heading(ui).label, 'Ready to finish');
    assert.deepEqual(JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles.map(item => item.id), ['images']);
    assert.equal(ui.by('Finish setup').sensitive, true);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

for (const failure of ['prepare', 'validate', 'verify-bindings']) {
    test(`${failure} blocking validation retains selected intent and never reaches Ready to finish`, async () => {
        const ui = await launch({...options([installation]), fail: failure});
        ui.edit(ui.by('Use ComfyUI'), 'active', true);
        await ui.click('Continue');
        assert.equal(ui.by('Use ComfyUI').active, true);
        assert.equal(heading(ui).label, 'Choose your applications');
        assert.equal(ui.by('Finish setup').sensitive, false);
        await ui.click('Continue');
        assert.equal(ui.by('Use ComfyUI').active, true, 'retry preserves inclusion');
        await ui.by('Finish setup').emit('clicked');
        assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    });
}

test('a busy installation check cannot be counted as passed after cancelling settings', async () => {
    const ui = await launch({...options([installation]), deferAction: 'discover', deferOccurrence: 2});
    await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings');
    ui.edit(ui.by('Health endpoint'), 'text', 'http://127.0.0.1:8188/changed');
    await ui.click('Back'); await ui.click('View details');
    const checking = ui.by('Check again').emit('clicked');
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(ui.by('Check again').sensitive, false);
    await ui.click('Cancel'); await ui.click('Continue');
    assert.equal(ui.by('Use ComfyUI').active, true);
    assert.equal(ui.by('Finish setup').sensitive, false);
    assert.equal(ui.calls.some(call => call.argv[1] === 'prepare'), false);
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Wait for the installation check')));
    ui.finish(); await checking;
    assert.equal(ui.by('Finish setup').sensitive, false, 'late check only ends busy state');
});

for (const app of ['ollama', 'vllm']) {
    test(`${app}: a discovered executable enters the ordinary managed model flow while the endpoint stays unverified`, async () => {
        const label = app === 'ollama' ? 'Ollama' : 'vLLM';
        const executable = `/environment/bin/${app}`;
        const endpoint = {app, label, sourceKind: 'endpoint', recognized: false, instanceStatus: 'unreachable', configurationStatus: 'unverified', endpoint: 'http://127.0.0.1:8000', models: []};
        const managed = {app, label, sourceKind: 'owned', reference: executable, referenceKind: 'application', recognized: true, instanceStatus: 'installed', configurationStatus: 'model-required', models: [],
            binding: {instance: 'local', owned: {executable, port: 9000}}};
        const ui = await launch(options([endpoint, managed], {'render-owned': {profile: {...profile, id: app, nativeModel: {runtime: app, owned: {executable, port: 9000}}}}}));
        ui.edit(ui.by(`Use ${label}`), 'active', true);
        await ui.click('Continue');
        assert.equal(heading(ui).label, 'Choose models');
        if (app === 'ollama') ui.edit(ui.by('Existing model name'), 'text', 'local-model');
        else {
            await ui.click('Choose another model…'); await ui.click('Choose model folder...');
        }
        await ui.click('Continue');
        const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft;
        assert.equal(rendered.binding.owned.executable, executable);
        assert.equal(ui.calls.some(call => ['prepare', 'apply', 'temporary-discover'].includes(call.argv[1])), false);
        assert.equal(ui.by('Finish setup').sensitive, true);
        assert.doesNotMatch(candidateMessage(endpoint), /Start.*separately/);
    });
}

test('unresolved GPU identity has review guidance and remains a Needs setup installation', async () => {
    const ui = await launch(options([{...installation, recognized: false, binding: undefined, configurationStatus: 'gpu-unresolved'}]));
    await ui.click('Settings for ComfyUI');
    assert.equal(ui.by('Use installation').sensitive, false);
    assert.equal(ui.visible(ui.by('Needs setup')), true);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.startsWith('GPU identity needs setup. Review')));
    assert.equal(ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])), false);
});
