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
    const retain = ui.widgets.find(widget => widget.accessibleProperties?.label === 'Keep this workload running at login if already active' && ui.visible(widget));
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

for (const value of ['12000', '']) {
    test(`saved resource overrides survive reopening with ${value || 'an explicitly cleared'} VRAM requirement`, async () => {
        const ui = await launch(options()); await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Resource checks');
        ui.edit(ui.by('Measured VRAM requirement (MiB; optional)'), 'text', '8000');
        ui.edit(ui.by('Measured VRAM requirement (MiB; optional)'), 'text', value);
        ui.edit(ui.by('Keep this workload running at login if already active'), 'active', true);
        await ui.click('Done'); await ui.click('Launch details'); await ui.click('Save selections for later');
        const drafts = JSON.parse(ui.calls.at(-1).input).drafts;
        assert.equal(drafts[0].requiredMiB, Number(value)); assert.equal(drafts[0].bootPolicy, 'retain');
        const reopened = await launch(options(ready, {drafts: {drafts}}));
        await reopened.click('Settings for ComfyUI'); await reopened.click('Advanced settings'); await reopened.click('Resource checks');
        assert.equal(reopened.by('Measured VRAM requirement (MiB; optional)').text, value);
        assert.equal(reopened.by('Keep this workload running at login if already active').active, true);
        await reopened.click('Done'); await reopened.click('Review changes');
        const checked = JSON.parse(reopened.calls.find(call => call.argv[1] === 'validate').input).catalog.profiles[0];
        assert.equal(checked.requiredMiB, value === '' ? undefined : Number(value)); assert.equal(checked.bootPolicy, 'retain');
    });
}

test('manual service changes invalidate readiness and show the entered identity', async () => {
    const ui = await launch(options()); await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Launch details');
    ui.edit(ui.by('Existing user service'), 'text', 'other.service');
    await ui.click('Back'); await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, false);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('other.service')));
    assert.equal(ui.visible(ui.by('Ready for setup')), false);
});

test('a saved endpoint override remains editable without inheriting readiness from its service', async () => {
    const saved = {id: 'saved-images', app: 'comfyui', label: 'ComfyUI', endpoint: 'http://127.0.0.1:9999', binding: ready.binding};
    const ui = await launch(options(ready, {drafts: {drafts: [saved]}}));
    await ui.click('Settings for ComfyUI');
    assert.equal(ui.by('Use installation').sensitive, false);
    assert.equal(ui.visible(ui.by('Ready for setup')), false);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'An override does not match this installation.'));
    await ui.click('Change installation…');
    assert.equal(ui.by('Application address').text, saved.endpoint);
    assert.equal(ui.calls.some(call => ['probe', 'prepare', 'apply'].includes(call.argv[1])), false);
});

test('Cancel cannot resurrect old ready evidence after preserving and reopening an edited endpoint', async () => {
    const ui = await launch(options()); await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings');
    ui.edit(ui.by('Health endpoint'), 'text', 'http://127.0.0.1:8188/unsupported');
    await ui.click('Back'); await ui.click('Back'); await ui.click('Settings for ComfyUI');
    assert.equal(ui.by('Use installation').sensitive, false); await ui.click('Cancel'); await ui.click('Settings for ComfyUI');
    assert.equal(ui.by('Use installation').sensitive, false);
    assert.equal(ui.calls.some(call => ['probe', 'prepare', 'apply'].includes(call.argv[1])), false);
    await ui.click('Advanced settings');
    assert.equal(ui.by('Health endpoint').text, 'http://127.0.0.1:8188/unsupported');
});

test('reinspection reconciles unchanged defaults with fresh same-unit endpoint metadata', async () => {
    const updated = {...ready, endpoint: 'http://127.0.0.1:9000', binding: {...ready.binding, healthURL: 'http://127.0.0.1:9000/system_stats'}};
    let discoveries = 0;
    const ui = await launch(options(ready, {discover: () => ({request, units: [], applications: [discoveries++ ? updated : ready]}), probe: {app: 'comfyui', instanceStatus: 'available', inventoryStatus: 'available', models: []}}));
    await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings');
    ui.edit(ui.by('Health endpoint'), 'text', ready.binding.healthURL);
    await ui.click('Back');
    // Rechecking an address is read-only, including when the running service changed its port.
    ui.by('Check again').emit('clicked'); await new Promise(resolve => setImmediate(resolve));
    await ui.click('Advanced settings');
    assert.equal(ui.by('Health endpoint').text, updated.binding.healthURL);
    await ui.click('Back'); await ui.click('Use installation');
    const prepared = JSON.parse(ui.calls.find(call => call.argv[1] === 'prepare').input).draft;
    assert.equal(prepared.endpoint, updated.endpoint); assert.equal(prepared.binding.healthURL, updated.binding.healthURL);
});

test('a relative configuration reference remains a unit reinspection target', async () => {
    const broken = {app: 'comfyui', label: 'ComfyUI', unit: ready.unit, reference: ready.unit, referenceKind: 'configuration',
        recognized: false, configurationStatus: 'inspection-failed', instanceStatus: 'candidate'};
    const ui = await launch(options(broken)); await ui.click('Settings for ComfyUI'); await ui.click('View details'); await ui.click('Check again');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'discover').length, 2);
    assert.equal(ui.calls.some(call => call.argv[1] === 'probe'), false);
    assert.equal(ui.by('Finish setup').sensitive, false);
});

test('repairing a relative service reference prepares the authoritative endpoint without a filesystem reference', async () => {
    const broken = {app: 'comfyui', label: 'ComfyUI', unit: ready.unit, reference: ready.unit, referenceKind: 'configuration',
        recognized: false, configurationStatus: 'inspection-failed', instanceStatus: 'candidate'};
    let discoveries = 0;
    const ui = await launch(options(broken, {discover: () => ({request, units: [], applications: [discoveries++ ? ready : broken]}), prepare: ({draft}) => {
        assert.equal(draft.reference, undefined); assert.equal(draft.referenceKind, undefined);
        assert.equal(draft.endpoint, ready.endpoint); assert.equal(draft.binding.unit, ready.unit);
        return {profile};
    }}));
    await ui.click('Settings for ComfyUI'); await ui.click('View details'); await ui.click('Check again'); await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, true); await ui.click('Use installation');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'prepare').length, 1);
    assert.equal(ui.by('Finish setup').sensitive, true);
    assert.equal(ui.calls.some(call => call.argv[1] === 'probe'), false);
});

test('reconciling service metadata preserves an explicitly selected configuration file', async () => {
    const saved = {id: 'saved-images', app: 'comfyui', label: 'ComfyUI',
        reference: ready.location, referenceKind: 'configuration', binding: ready.binding};
    const broken = {...ready, endpoint: undefined, recognized: false, configurationStatus: 'inspection-failed'};
    let discoveries = 0;
    const ui = await launch(options(broken, {drafts: {drafts: [saved]}, discover: () => ({request, units: [], applications: [discoveries++ ? ready : broken]}),
        prepare: ({draft}) => {
            assert.equal(draft.endpoint, undefined);
            assert.equal(draft.reference, ready.location); assert.equal(draft.referenceKind, 'configuration');
            return {profile};
        }}));
    await ui.click('Settings for ComfyUI'); await ui.click('View details'); await ui.click('Check again'); await ui.click('Back'); await ui.click('Use installation');
    const prepared = JSON.parse(ui.calls.find(call => call.argv[1] === 'prepare').input).draft;
    assert.equal(prepared.reference, ready.location); assert.equal(prepared.referenceKind, 'configuration');
    assert.equal(prepared.endpoint, undefined); assert.equal(ui.by('Finish setup').sensitive, true);
});

for (const value of ['abc', 'Infinity', '-1', '1.5', '9007199254740992']) {
    test(`saving selections rejects invalid VRAM requirement ${value} without losing the field`, async () => {
        const ui = await launch(options()); await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Resource checks');
        ui.edit(ui.by('Measured VRAM requirement (MiB; optional)'), 'text', value);
        await ui.click('Done'); await ui.click('Launch details'); await ui.click('Save selections for later');
        assert.equal(ui.calls.some(call => call.argv[1] === 'save-drafts'), false);
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('Selections were not saved. Check the VRAM requirements')));
        await ui.click('Done'); await ui.click('Resource checks');
        assert.equal(ui.by('Measured VRAM requirement (MiB; optional)').text, value);
        ui.edit(ui.by('Measured VRAM requirement (MiB; optional)'), 'text', '12000');
        await ui.click('Done'); await ui.click('Launch details'); await ui.click('Save selections for later');
        assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].requiredMiB, 12000);
    });
}

test('invalid resource input survives preserved edits and Cancel without becoming a savable null', async () => {
    const ui = await launch(options()); await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Resource checks');
    ui.edit(ui.by('Measured VRAM requirement (MiB; optional)'), 'text', 'abc');
    await ui.click('Done'); await ui.click('Launch details'); await ui.click('Save selections for later');
    await ui.click('Back'); await ui.click('Back'); await ui.click('Back');
    await ui.click('Settings for ComfyUI'); await ui.click('Cancel'); await ui.click('Settings for ComfyUI');
    await ui.click('Advanced settings'); await ui.click('Resource checks');
    assert.equal(ui.by('Measured VRAM requirement (MiB; optional)').text, 'abc');
    await ui.click('Done'); await ui.click('Launch details'); await ui.click('Save selections for later');
    assert.equal(ui.calls.some(call => call.argv[1] === 'save-drafts'), false);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('Selections were not saved. Check the VRAM requirements')));
});

test('a saved Ollama model stays reviewable when service discovery leaves the model selection open', async () => {
    const candidate = {app: 'ollama', label: 'Ollama', unit: 'ollama.service', endpoint: 'http://127.0.0.1:11434', recognized: true,
        configurationStatus: 'model-required', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'existing:latest'}],
        binding: {unit: 'ollama.service', cgroup: '/ollama', healthURL: 'http://127.0.0.1:11434/api/tags', model: ''}};
    const saved = {id: 'existing-model', app: 'ollama', label: 'Ollama', model: 'existing:latest', endpoint: candidate.endpoint,
        binding: {...candidate.binding, model: 'existing:latest'}};
    const ui = await launch(options(candidate, {drafts: {drafts: [saved]}, prepare: {profile: {...profile, id: saved.id, unit: candidate.unit,
        healthURL: candidate.binding.healthURL, nativeModel: {runtime: 'ollama', model: saved.model}}}}));
    await ui.click('Settings for Ollama'); assert.equal(ui.by('Use installation').sensitive, true);
    await ui.click('Use installation');
    const prepared = JSON.parse(ui.calls.find(call => call.argv[1] === 'prepare').input).draft;
    assert.equal(prepared.model, saved.model); assert.equal(prepared.binding.model, saved.model);
    assert.equal(ui.by('Finish setup').sensitive, true);
});

test('health edits in Launch details stay synchronized with the General field', async () => {
    const ui = await launch(options()); await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Launch details');
    ui.edit(ui.by('Loopback health URL'), 'text', 'http://127.0.0.1:8188/custom-health');
    await ui.click('Done'); assert.equal(ui.by('Health endpoint').text, 'http://127.0.0.1:8188/custom-health');
    ui.edit(ui.by('Health endpoint'), 'text', 'http://127.0.0.1:8188/changed');
    await ui.click('Launch details'); assert.equal(ui.by('Loopback health URL').text, 'http://127.0.0.1:8188/changed');
});

test('owned launch edits invalidate readiness while Cancel restores the checked original', async () => {
    const binding = {instance: 'local', owned: {executable: '/installed/ollama', port: 11434}};
    const candidate = {app: 'ollama', label: 'Ollama', recognized: true, sourceKind: 'owned', configurationStatus: 'model-required', instanceStatus: 'installed',
        reference: '/installed/ollama', referenceKind: 'application', binding, models: []};
    const ui = await launch(options(candidate)); await ui.click('Settings for Ollama'); await ui.click('Advanced settings'); await ui.click('Launch details');
    ui.edit(ui.by('Launch port'), 'text', '12345'); await ui.click('Done'); await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, false);
    await ui.click('Cancel'); await ui.click('Settings for Ollama');
    assert.equal(ui.by('Use installation').sensitive, true);
    await ui.click('Advanced settings'); await ui.click('Launch details');
    assert.equal(ui.by('Launch port').text, '11434');
});

test('checking an empty installation selection keeps visible guidance and blocks review', async () => {
    const ui = await launch(options(ready, {discover: {request, units: [], applications: []}}));
    await ui.click('Settings for ComfyUI'); await ui.click('View details'); await ui.click('Check again');
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('Choose a detected instance')));
    assert.equal(ui.calls.some(call => ['probe', 'prepare'].includes(call.argv[1])), false);
    await ui.click('Back'); assert.equal(ui.by('Use installation').sensitive, false);
});
