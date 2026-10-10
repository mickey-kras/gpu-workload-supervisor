import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {candidateChoice} from './onboarding.mjs';

const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
const ready = {app: 'comfyui', label: 'ComfyUI', unit: 'comfyui.service', endpoint: 'http://127.0.0.1:8188', location: '/services/comfyui.service', recognized: true,
    instanceStatus: 'not-running', configurationStatus: 'ready', binding: {unit: 'comfyui.service', cgroup: '/comfyui', healthURL: 'http://127.0.0.1:8188/system_stats'}};
const profile = {id: 'images', label: 'ComfyUI', adapter: 'systemd', unit: ready.unit, cgroup: '/comfyui', healthURL: ready.binding.healthURL,
    bootPolicy: 'stop-to-idle', launchBinding: {runtime: 'comfyui', launchFile: '/services/comfyui.service'}};
function options(candidate = ready, responses = {}) {
    return {deferAction: 'unused', responses: {discover: {request, units: [], applications: [candidate]}, prepare: {profile}, ...responses}};
}
const title = ui => ui.widgets.find(widget => widget.cssClasses?.includes('title') && ui.visible(widget));

test('ready settings have one review action and keep activation explicit', async () => {
    const ui = await launch(options()); await ui.click('Settings for ComfyUI');
    assert.equal(title(ui).label, 'ComfyUI');
    assert.equal(ui.visible(ui.by('Display name')), false);
    assert.equal(ui.visible(ui.by('Remove this application')), false);
    assert.equal(ui.visible(ui.by('View details')), false);
    assert.equal(ui.widgets.filter(widget => widget.cssClasses?.includes('suggested-action') && ui.visible(widget)).length, 1);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'GPU operation has not been tested.'));
    await ui.click('Use installation');
    assert.equal(ui.by('Finish setup').sensitive, true);
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
    await ui.click('Finish setup');
    assert.equal(JSON.parse(ui.calls.at(-1).input).confirmQuiesced, true);
});

test('details preserve diagnostics, copy them, and recheck repaired service metadata', async () => {
    const broken = {...ready, recognized: false, configurationStatus: 'inspection-failed', nextStep: 'Environment variable HOME: expansion, escapes or whitespace are unsupported; use one literal whole assignment.'};
    let discoveries = 0;
    const ui = await launch(options(broken, {discover: () => ({request, units: [], applications: [discoveries++ ? ready : broken]}), probe: {app: 'comfyui', instanceStatus: 'not-running', inventoryStatus: 'not-checked'}}));
    await ui.click('Settings for ComfyUI');
    assert.equal(ui.by('Use installation').sensitive, false);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'HOME uses an unsupported format.'));
    await ui.click('View details');
    assert.equal(title(ui).label, 'Configuration details');
    const diagnostic = ui.widgets.find(widget => widget.label?.includes('Environment variable HOME:'));
    assert.equal(diagnostic.selectable, true); assert.equal(diagnostic.wrap_mode, 2);
    await ui.click('Copy details');
    assert.ok(ui.widgets.find(widget => widget.title === 'GPU Workload Setup').clipboard.includes(broken.nextStep));
    assert.equal(ui.widgets.filter(widget => widget.label === 'Check again' && ui.visible(widget)).length, 1);
    await ui.click('Check again'); await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, true);
    assert.equal(ui.calls.filter(call => call.argv[1] === 'discover').length, 2);
    assert.equal(ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])), false);
});

test('selection exposes full identities and Cancel restores installation, draft fields and focus', async () => {
    const second = {...ready, unit: `${'long'.repeat(90)}.service`, location: `/${'path'.repeat(120)}`, binding: {...ready.binding, unit: `${'long'.repeat(90)}.service`}};
    const ui = await launch(options(ready, {discover: {request, units: [], applications: [ready, second]}}));
    await ui.click('Settings for ComfyUI'); await ui.click('Change installation…');
    const identity = candidateChoice(second); const row = ui.by(identity);
    assert.equal(row.accessibleProperties.label, identity); assert.equal(row.children[0].wrap_mode, 2);
    await ui.click(identity); await ui.click('Advanced settings');
    ui.edit(ui.by('Installation display name'), 'text', 'Draft name');
    await ui.click('Cancel');
    assert.equal(ui.by('Settings for ComfyUI').focused, true);
    await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Launch details');
    await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.at(-1).input).drafts[0];
    assert.equal(saved.label, 'ComfyUI'); assert.equal(saved.binding, undefined);
});

test('advanced pages keep common fields separate and resource overrides survive review', async () => {
    const existing = {...profile, requiredMiB: 8000, bootPolicy: 'retain', systemdSlice: 'app.slice'};
    const ui = await launch(options(ready, {discover: {request: {...request, catalog: {version: 1, profiles: [existing]}}, units: [], applications: [ready]}}));
    await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings');
    assert.equal(title(ui).label, 'Advanced settings');
    assert.equal(ui.visible(ui.by('Installation display name')), true);
    assert.equal(ui.visible(ui.by('Health endpoint')), true);
    assert.equal(ui.visible(ui.by('Existing user service')), false);
    await ui.click('Resource checks');
    assert.equal(title(ui).label, 'Resource checks');
    const capacity = ui.widgets.find(widget => widget.title === 'Measured VRAM requirement (MiB; optional)' && ui.visible(widget));
    assert.equal(capacity.text, '8000'); ui.edit(capacity, 'text', '12345');
    const retain = ui.widgets.find(widget => widget.label === 'Keep this workload running at login if already active' && ui.visible(widget));
    ui.edit(retain, 'active', false); await ui.click('Done'); await ui.click('Review changes');
    const reviewed = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles[0];
    assert.equal(reviewed.requiredMiB, 12345); assert.equal(reviewed.bootPolicy, 'stop-to-idle'); assert.equal(reviewed.systemdSlice, 'app.slice');
    assert.equal(ui.calls.some(call => call.argv[1] === 'apply'), false);
});

test('pending and stale checks cannot enable the installation action after an address edit', async () => {
    const broken = {...ready, recognized: false, configurationStatus: 'inspection-failed'};
    const ui = await launch({...options(broken, {probe: ready}), deferAction: 'probe'});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    const checking = ui.by('Check again').emit('clicked'); await new Promise(resolve => setImmediate(resolve));
    assert.equal(ui.by('Check again').sensitive, false);
    assert.ok(ui.widgets.some(widget => widget.label === 'Checking configuration…' && ui.visible(widget)));
    await ui.click('Back'); await ui.click('Change installation…'); ui.edit(ui.by('Application address'), 'text', 'http://127.0.0.1:9000');
    ui.finish(); await checking; await ui.click('Done');
    assert.equal(ui.by('Use installation').sensitive, false);
    assert.equal(ui.calls.some(call => ['prepare', 'apply'].includes(call.argv[1])), false);
});

for (const state of ['ambiguous', 'discovery-error', 'inspection-failed', 'unreachable', 'missing', 'unsupported', 'invalid', 'candidate', 'available', 'installed']) {
    test(`${state} remains configuration evidence without GPU qualification`, async () => {
        const candidate = {...ready, recognized: false, configurationStatus: 'unverified', instanceStatus: state};
        const ui = await launch(options(candidate)); await ui.click('Settings for ComfyUI');
        assert.equal(ui.by('Use installation').sensitive, false);
        assert.equal(ui.visible(ui.by('View details')), true);
        assert.equal(ui.calls.some(call => ['prepare', 'apply', 'temporary-discover'].includes(call.argv[1])), false);
    });
}

test('a real unit-only HOME inspection failure can be checked again without inventing an endpoint', async () => {
    const broken = {app: 'comfyui', label: 'ComfyUI', sourceKind: 'configuration', unit: ready.unit, location: ready.location,
        recognized: false, instanceStatus: 'not-running', configurationStatus: 'inspection-failed', inventoryStatus: 'not-applicable',
        nextStep: 'Environment variable HOME: expansion, escapes or whitespace are unsupported; use one literal whole assignment.'};
    let discoveries = 0;
    const ui = await launch(options(broken, {discover: () => ({request, units: [], applications: [discoveries++ ? ready : broken]})}));
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    await ui.click('Check again'); await ui.click('Back');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'discover').length, 2);
    assert.equal(ui.calls.some(call => ['probe', 'prepare', 'apply'].includes(call.argv[1])), false);
    assert.equal(ui.by('Use installation').sensitive, true);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes(ready.endpoint)));
});
