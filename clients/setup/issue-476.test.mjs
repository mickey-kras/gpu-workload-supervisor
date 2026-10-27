import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {diagnosticReport, correctionPreview, candidateMessage} from './onboarding.mjs';
const uuid = 'GPU-01234567-89ab-cdef-0123-456789abcdef';
const blocked = {id: 'blocked', app: 'comfyui', label: 'ComfyUI', installed: true, recognized: false, unit: 'private-secret.service', location: '/private-path-secret', instanceStatus: 'not-running', configurationStatus: 'gpu-unresolved', diagnostics: ['CUDA_VISIBLE_DEVICES needs a full GPU UUID.', 'Restart policy is unsupported.', 'Executable /private-exec-secret is not root-owned.']};
const request = {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}};
test('shareable report structurally removes identities, raw values and unknown states', () => {
    const raw = diagnosticReport([blocked, {...blocked, app: 'vllm', instanceStatus: '/secret-status', diagnostics: ['unrecognized private-error-secret'], models: [{id: '/secret-model'}], endpoint: 'http://secret', evidence: ['/secret-evidence'], binding: {secret: uuid}}], ['private-discovery-secret']);
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
test('a single GPU is proposed internally and applying requires a reviewed fingerprint', async () => {
    let applied = false;
    const ready = {...blocked, recognized: true, diagnostics: [], configurationStatus: 'ready', binding: {unit: blocked.unit, cgroup: '/service', healthURL: 'http://127.0.0.1:8188/system_stats'}};
    const ui = await launch({deferAction: 'unused', responses: {
        discover: () => ({request, units: [], gpuInventoryStatus: 'available', gpuUUIDs: [uuid], applications: [applied ? ready : blocked]}),
        'correct-gpu': input => { applied = Boolean(input.confirm); return {fingerprint: 'reviewed', content: 'GPU correction preview', applied}; },
    }});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    assert.equal(ui.widgets.some(widget => ui.visible(widget) && widget.title?.includes('GPU UUID')), false);
    await ui.click('Configure GPU');
    assert.equal(applied, false);
    assert.equal(ui.by('Use installation').visible, false);
    await ui.click('Apply GPU configuration');
    const calls = ui.calls.filter(call => call.argv[1] === 'correct-gpu');
    assert.equal(JSON.parse(calls[1].input).fingerprint, 'reviewed');
    assert.equal(JSON.parse(calls[1].input).confirm, true);
    await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, true);
    assert.equal(ui.calls.some(call => ['apply'].includes(call.argv[1])), false);
});

test('Check again disables automatic correction when trusted GPU inventory disappears', async () => {
    let count = 0;
    const ui = await launch({deferAction: 'unused', responses: {discover: () => ({request, units: [],
        gpuUUIDs: count++ ? [] : [uuid], gpuInventoryStatus: count === 1 ? 'available' : 'unavailable', applications: [blocked]})}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    assert.equal(ui.by('Configure GPU').visible, true);
    await ui.click('Check again');
    assert.equal(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'Configure GPU'), false);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('GPU inventory query was unavailable.')));
});

test('native-style inherited endpoint keeps reinspection busy until its read-only probe completes', async () => {
    const native = {...blocked, id: undefined, endpoint: 'http://127.0.0.1:18188', cgroup: '/example/model', inventoryStatus: 'available', models: []};
    const ui = await launch({deferAction: 'probe', responses: {discover: {request, units: [], applications: [native]}, probe: {app: 'comfyui', instanceStatus: 'unreachable', inventoryStatus: 'unknown'}}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    const checking = ui.click('Check again');
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(ui.by('Check again').sensitive, false);
    ui.finish(); await checking;
    assert.equal(ui.by('Check again').sensitive, true);
});

test('one diagnostic export uses freshly rechecked installation evidence and keeps other applications', async () => {
    const repaired = {...blocked, recognized: true, diagnostics: [], configurationStatus: 'ready', binding: {unit: blocked.unit, cgroup: '/service', healthURL: 'http://127.0.0.1:8188/system_stats'}};
    const other = {id: 'other-address', app: 'vllm', label: 'vLLM', endpoint: 'http://127.0.0.1:8000', recognized: false, instanceStatus: 'unreachable', configurationStatus: 'unverified'};
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
    assert.equal(report.installations.find(item => item.application === 'vllm').state, 'unreachable');
    assert.equal(/private|secret|127\.0\.0\.1/.test(window.clipboard), false);
    assert.equal(ui.calls.some(call => ['apply', 'prepare'].includes(call.argv[1])), false);
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
    assert.equal(ui.calls.some(call => ['apply', 'prepare'].includes(call.argv[1])), false);
});


test('automatic correction failure restores manual recovery and discards confirmation', async () => {
    let calls = 0;
    const ui = await launch({deferAction: 'unused', responses: {
        discover: {request, units: [], gpuInventoryStatus: 'available', gpuUUIDs: [uuid], applications: [blocked]},
        'correct-gpu': () => { if (calls++ === 0) return {fingerprint: 'preview', content: 'review me'}; throw new Error('Service is outside the user configuration directory'); },
    }});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    await ui.click('Configure GPU'); await ui.click('Apply GPU configuration');
    assert.equal(ui.by('Copy reviewed override').visible, true);
    assert.equal(ui.by('Copy reviewed override').sensitive, true);
    assert.equal(ui.by('Configure GPU').visible, true);
    await ui.click('Configure GPU');
    const last = ui.calls.filter(call => call.argv[1] === 'correct-gpu').at(-1);
    assert.equal(JSON.parse(last.input).confirm, false);
});

for (const message of ['GPU correction saved; reload failed', 'GPU correction saved; configuration still needs attention']) {
    test(`correction failure preserves saved-state guidance: ${message}`, async () => {
        let calls = 0;
        const ui = await launch({deferAction: 'unused', responses: {
            discover: {request, units: [], gpuInventoryStatus: 'available', gpuUUIDs: [uuid], applications: [blocked]},
            'correct-gpu': () => { if (calls++ === 0) return {fingerprint: 'preview', content: 'review me'}; throw new Error(message); },
        }});
        await ui.click('Settings for ComfyUI'); await ui.click('View details');
        await ui.click('Configure GPU'); await ui.click('Apply GPU configuration');
        const guidance = ui.widgets.find(widget => ui.visible(widget) && widget.label?.includes(message));
        assert.ok(guidance);
        assert.equal(guidance.label.includes('Not applied'), false);
        assert.match(guidance.label, /saved, run systemctl --user daemon-reload and Check again before attempting another correction/);
        await ui.click('Configure GPU');
        assert.equal(JSON.parse(ui.calls.filter(call => call.argv[1] === 'correct-gpu').at(-1).input).confirm, false);
    });
}

test('discovery from another editor refreshes GPU inventory and discards its reviewed proposal', async () => {
    const other = {...blocked, app: 'vllm', label: 'vLLM', unit: 'vllm.service'};
    let count = 0;
    const ui = await launch({deferAction: 'unused', responses: {
        discover: () => ({request, units: [], applications: [blocked, other], gpuUUIDs: count ? [] : [uuid], gpuInventoryStatus: count++ ? 'unavailable' : 'available'}),
        'correct-gpu': {fingerprint: 'old-preview', content: 'old preview'},
    }});
    await ui.click('Settings for vLLM'); await ui.click('View details'); await ui.click('Configure GPU');
    await ui.click('Back'); await ui.click('Back');
    await ui.click('Settings for ComfyUI'); await ui.click('View details'); await ui.click('Check again');
    await ui.click('Back'); await ui.click('Back');
    await ui.click('Settings for vLLM'); await ui.click('View details');
    assert.equal(ui.widgets.some(widget => ui.visible(widget) && ['Configure GPU', 'Apply GPU configuration'].includes(widget.label)), false);
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('GPU inventory query was unavailable.')));
    assert.equal(ui.calls.filter(call => call.argv[1] === 'correct-gpu').length, 1);
});
