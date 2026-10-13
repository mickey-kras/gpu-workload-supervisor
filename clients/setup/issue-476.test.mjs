import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {diagnosticReport, correctionPreview} from './onboarding.mjs';
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
