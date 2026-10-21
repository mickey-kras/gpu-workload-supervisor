import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {diagnosticReport, correctionPreview, candidateMessage} from './onboarding.mjs';
const uuid = 'GPU-01234567-89ab-cdef-0123-456789abcdef';
const blocked = {id: 'blocked', app: 'comfyui', label: 'ComfyUI', installed: true, recognized: false, unit: 'private-secret.service', location: '/private-path-secret', instanceStatus: 'not-running', configurationStatus: 'gpu-unresolved', diagnostics: ['CUDA_VISIBLE_DEVICES needs a full GPU UUID.', 'Restart policy is unsupported.', 'Executable /private-exec-secret is not root-owned.']};
const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}};
test('shareable report structurally removes identities, raw values and unknown states', () => {
    const raw = diagnosticReport([blocked, {...blocked, app: 'ollama', instanceStatus: '/secret-status', diagnostics: ['unrecognized private-error-secret'], models: [{id: '/secret-model'}], endpoint: 'http://secret', evidence: ['/secret-evidence'], binding: {secret: uuid}}], ['private-discovery-secret']);
    assert.equal(/private|secret|GPU-/.test(raw), false);
    const report = JSON.parse(raw);
    assert.equal(report.installations.length, 2);
    assert.equal(report.discoveryErrorCount, 1);
    assert.deepEqual(report.installations[0].blockers, ['gpu-identity', 'restart-policy', 'execution-trust']);
    assert.equal(report.installations[1].state, 'unknown');
});
test('service metadata alone never claims an observed installation', () => {
    const report = JSON.parse(diagnosticReport([{...blocked, installed: false, configurationStatus: 'model-missing'}]));
    assert.equal(report.installations[0].installed, false);
    assert.equal(report.installations[0].qualified, false);
    const message = candidateMessage({unit: 'only-metadata.service', instanceStatus: 'not-running', installed: false, recognized: false});
    assert.match(message, /Service found and stopped/);
    assert.equal(message.includes('Installed'), false);
});
test('reviewed override requires explicit full UUID and never invents a numeric mapping', () => {
    assert.equal(correctionPreview(blocked), '');
    assert.equal(correctionPreview(blocked, '0'), '');
    assert.equal(correctionPreview(blocked, 'GPU-0123'), '');
    assert.equal(correctionPreview(blocked, uuid), `[Service]\nEnvironment="CUDA_VISIBLE_DEVICES=${uuid}"\nRestart=no\n`);
});
test('native handlers detect blocked installation, select UUID, copy preview and recheck before ready', async () => {
    let count = 0;
    const ready = {...blocked, recognized: true, diagnostics: [], configurationStatus: 'ready', binding: {unit: blocked.unit, cgroup: '/service', healthURL: 'http://127.0.0.1:8188/system_stats'}};
    const ui = await launch({deferAction: 'unused', responses: {discover: () => ({request, units: [], gpuUUIDs: [uuid], applications: [count++ ? ready : blocked]})}});
    await ui.click('Copy diagnostic report');
    const window = ui.widgets.find(widget => widget.title === 'GPU Workload Setup');
    assert.equal(window.clipboard.includes('private'), false);
    await ui.click('Settings for ComfyUI');
    assert.equal(ui.by('Use installation').sensitive, false);
    await ui.click('View details');
    const instructions = ui.widgets.find(widget => ui.visible(widget) && widget.label?.includes('systemctl --user edit SERVICE'));
    assert.ok(instructions);
    assert.match(instructions.label, /Save the drop-in, run systemctl --user daemon-reload, then Check again/);
    assert.match(instructions.label, /Restart=no disables automatic crash recovery/);
    assert.match(instructions.label, /Copying applies nothing/);
    assert.equal(ui.by('Copy reviewed override').sensitive, false);
    ui.edit(ui.by('Physical GPU UUID (explicit selection required)'), 'selected', 1);
    assert.equal(ui.by('Copy reviewed override').sensitive, true);
    await ui.click('Copy reviewed override');
    assert.equal(window.clipboard, correctionPreview(blocked, uuid));
    await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, false);
    await ui.click('View details');
    assert.equal(ui.calls.some(call => ['apply', 'temporary-discover'].includes(call.argv[1])), false);
    await ui.click('Check again'); await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, true);
});

test('Check again refreshes available UUIDs and drops a disappeared explicit selection', async () => {
    let count = 0;
    const inventories = [[], [uuid], []];
    const ui = await launch({deferAction: 'unused', responses: {discover: () => {
        const gpuUUIDs = inventories[Math.min(count++, 2)];
        return {request, units: [], gpuUUIDs, gpuInventoryStatus: gpuUUIDs.length ? 'available' : 'unavailable', applications: [blocked]};
    }}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    const choice = ui.by('Physical GPU UUID (explicit selection required)');
    assert.equal(choice.model.get_n_items(), 1);
    assert.equal(ui.by('Copy reviewed override').sensitive, false);
    await ui.click('Check again');
    assert.equal(choice.model.get_n_items(), 2);
    assert.equal(choice.selected, 0);
    assert.equal(ui.by('Copy reviewed override').sensitive, false);
    ui.edit(choice, 'selected', 1);
    assert.equal(ui.by('Copy reviewed override').sensitive, true);
    await ui.click('Check again');
    assert.equal(choice.model.get_n_items(), 1);
    assert.equal(choice.selected, 0);
    assert.equal(ui.by('Copy reviewed override').sensitive, false);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('GPU inventory query was unavailable.')));
    await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, false);
    assert.equal(ui.calls.some(call => ['apply', 'temporary-discover'].includes(call.argv[1])), false);
});

test('native-style inherited endpoint keeps reinspection busy until its read-only probe completes', async () => {
    const native = {...blocked, id: undefined, endpoint: 'http://127.0.0.1:18188', cgroup: '/example/model', inventoryStatus: 'available', models: [], evidence: ['Example command evidence'], nextStep: 'Inspect the existing service. Its files have not been changed.'};
    let discoveries = 0;
    const ui = await launch({deferAction: 'probe', responses: {discover: () => ({request, units: [], gpuUUIDs: discoveries++ ? [uuid] : [], applications: [native]}), probe: {app: 'comfyui', instanceStatus: 'unreachable', inventoryStatus: 'unknown'}}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    const checking = ui.click('Check again');
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(ui.calls.filter(call => call.argv[1] === 'discover').length, 2);
    assert.equal(ui.calls.filter(call => call.argv[1] === 'probe').length, 1);
    assert.equal(ui.by('Physical GPU UUID (explicit selection required)').model.get_n_items(), 2);
    assert.equal(ui.by('Check again').sensitive, false);
    ui.finish(); await checking;
    assert.equal(ui.by('Check again').sensitive, true);
    assert.equal(ui.by('Copy reviewed override').sensitive, false);
    assert.equal(ui.calls.some(call => ['apply', 'temporary-discover'].includes(call.argv[1])), false);
});

test('one diagnostic export uses freshly rechecked installation evidence and keeps other applications', async () => {
    const repaired = {...blocked, recognized: true, diagnostics: [], configurationStatus: 'ready', binding: {unit: blocked.unit, cgroup: '/service', healthURL: 'http://127.0.0.1:8188/system_stats'}};
    const other = {id: 'other-address', app: 'ollama', label: 'Ollama', endpoint: 'http://127.0.0.1:11434', recognized: false, instanceStatus: 'unreachable', configurationStatus: 'unverified'};
    let discoveries = 0;
    const ui = await launch({deferAction: 'unused', responses: {discover: () => ({request, units: [], applications: [discoveries++ ? repaired : blocked, other]})}});
    const window = ui.widgets.find(widget => widget.title === 'GPU Workload Setup');
    await ui.click('Copy diagnostic report');
    assert.equal(JSON.parse(window.clipboard).installations.find(item => item.application === 'comfyui').qualified, false);
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    await ui.click('Check again'); await ui.click('Back'); await ui.click('Back');
    await ui.click('Copy diagnostic report');
    const report = JSON.parse(window.clipboard);
    assert.equal(report.installations.length, 2);
    assert.equal(report.installations.find(item => item.application === 'comfyui').qualified, true);
    assert.equal(report.installations.find(item => item.application === 'comfyui').configuration, 'ready');
    assert.equal(report.installations.find(item => item.application === 'ollama').state, 'unreachable');
    assert.equal(/private|secret|127\.0\.0\.1/.test(window.clipboard), false);
    assert.equal(ui.calls.some(call => ['apply', 'prepare', 'temporary-discover'].includes(call.argv[1])), false);
});

test('RestartSec blockers never propose or classify a Restart policy correction', async () => {
    const interval = {...blocked, configurationStatus: 'inspection-failed', diagnostics: ['existing launch is unsupported: incompatible directive RestartSec']};
    assert.equal(correctionPreview(interval), '');
    assert.deepEqual(JSON.parse(diagnosticReport([interval])).installations[0].blockers, ['launch-configuration']);
    const mixed = {...interval, configurationStatus: 'gpu-unresolved', diagnostics: [...interval.diagnostics, 'CUDA_VISIBLE_DEVICES requires a full GPU UUID.']};
    assert.equal(correctionPreview(mixed, uuid), `[Service]\nEnvironment="CUDA_VISIBLE_DEVICES=${uuid}"\n`);
    const policy = {...interval, diagnostics: ['existing launch is unsupported: unsupported directive or value Restart']};
    assert.equal(correctionPreview(policy), '[Service]\nRestart=no\n');
    assert.deepEqual(JSON.parse(diagnosticReport([policy])).installations[0].blockers, ['restart-policy']);
    const ui = await launch({deferAction: 'unused', responses: {discover: {request, units: [], gpuUUIDs: [uuid], applications: [interval]}}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    assert.equal(ui.by('Copy reviewed override').sensitive, false);
    assert.equal(ui.visible(ui.by('Copy reviewed override')), false);
    assert.equal(ui.calls.some(call => ['apply', 'prepare', 'temporary-discover'].includes(call.argv[1])), false);
});
