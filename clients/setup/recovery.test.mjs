import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {recoveryGuidance} from './onboarding.mjs';

const request = {profile: {statePath: '/state.db'}, catalog: {version: 1, profiles: []}};
const prefix = 'native launch requires a supported direct local command: ';

test('recovery distinguishes account data from protected runtimes without proposing mutations', () => {
    const data = recoveryGuidance({diagnostics: [prefix + 'Environment variable HF_HOME: requires an existing trusted path without links.',
        prefix + 'ComfyUI option --output-directory requires a trusted directory.', prefix + 'application script trust: launch source component 3 is writable.']});
    assert.equal(data.length, 2); // One path repair plus reinspection, despite multiple blockers.
    assert.match(data[0], /owned by this account or root/);
    assert.match(data[0], /ancestors/);
    assert.match(data[0], /Preserve models and application data/);
    assert.equal(data.join('\n').includes('install a trusted package'), false);
    const runtime = recoveryGuidance({diagnostics: [prefix + 'executable: interpreter trust: executable target is group or world writable.']});
    assert.match(runtime[0], /administrator.*protected root-owned/);
    assert.match(runtime[0], /never changes ownership or permissions/);
    assert.match(recoveryGuidance({nextStep: prefix + 'Environment variable PATH: requires trusted directories.'})[0], /every alias target and ancestor/);
    assert.match(recoveryGuidance({diagnostics: [prefix + 'Environment variable LD_LIBRARY_PATH: transitive loader bindings are not qualified.']})[0], /Simply deleting LD_LIBRARY_PATH does not establish/);
    assert.match(recoveryGuidance({diagnostics: [prefix + 'WorkingDirectory requires an existing trusted account-home path.']})[0], /inside the account home/);
    assert.deepEqual(recoveryGuidance({diagnostics: ['CUDA_VISIBLE_DEVICES needs a full GPU UUID.', 'unsupported directive or value Restart', 'ComfyUI option --auto-launch is not qualified.']}), []);
    assert.deepEqual(recoveryGuidance(), []);
});

test('reinspection clears obsolete data recovery only after fresh configuration evidence', async () => {
    const blocked = {app: 'comfyui', label: 'ComfyUI', unit: 'images.service', recognized: false, instanceStatus: 'not-running',
        configurationStatus: 'inspection-failed', diagnostics: [prefix + 'Environment variable XDG_CACHE_HOME: requires an existing absolute clean trusted path without links.']};
    const repaired = {...blocked, diagnostics: [], recognized: true, configurationStatus: 'ready', binding: {unit: blocked.unit, cgroup: '/images', healthURL: 'http://127.0.0.1:8188/system_stats'}};
    let checked = false;
    const ui = await launch({deferAction: 'unused', responses: {discover: () => ({request, units: [], applications: [checked ? repaired : blocked]})}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('owned by this account or root')));
    await ui.click('Check again'); await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, false);
    checked = true;
    await ui.click('View details'); await ui.click('Check again');
    assert.equal(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('owned by this account or root')), false);
    await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, true);
    assert.equal(ui.calls.some(call => ['apply', 'prepare', 'temporary-discover'].includes(call.argv[1])), false);
});

for (const [app, label, port] of [['ollama', 'Ollama', 11434], ['vllm', 'vLLM', 8000]]) {
    test(`${app}: custom executable selection is retained while blocked and rechecked before review`, async () => {
        const path = `/custom/runtime/${app}`;
        const endpoint = {app, label, instanceStatus: 'unreachable', endpoint: `http://127.0.0.1:${port}`, recognized: false};
        const selected = {app, label, reference: path, referenceKind: 'application', installed: true, recognized: false, instanceStatus: 'inspection-failed',
            configurationStatus: 'inspection-failed', diagnostics: [prefix + 'executable trust: executable target is not root-owned.'], lifecycleControl: 'unverified'};
        const qualified = {...selected, recognized: true, diagnostics: [], instanceStatus: 'installed', configurationStatus: 'model-required', sourceKind: 'owned',
            binding: {instance: 'chosen', owned: {executable: path, port}}};
        let repaired = false;
        const ui = await launch({deferAction: 'unused', filePath: path, responses: {discover: {request, units: [], applications: [endpoint]}, probe: input => {
            assert.deepEqual(input, {app, reference: path, referenceKind: 'application'});
            return repaired ? qualified : selected;
        }}});
        await ui.click(`Settings for ${label}`); await ui.click('Change installation…'); await ui.click('Choose installed executable…');
        await ui.click('Back'); await ui.click('View details'); await ui.click('Check again'); await ui.click('Back');
        assert.equal(ui.by('Use installation').sensitive, false);
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'Installed'));
        await ui.click('View details');
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('Runtime executable:')));
        repaired = true;
        await ui.click('Check again'); await ui.click('Back');
        assert.equal(ui.by('Use installation').sensitive, true);
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'Installed'));
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'GPU operation has not been tested.'));
        assert.equal(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'Reachable'), false);
        assert.equal(ui.calls.some(call => ['apply', 'render-owned', 'prepare', 'temporary-discover'].includes(call.argv[1])), false);
        await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
        const saved = JSON.parse(ui.calls.at(-1).input).drafts.find(draft => draft.app === app);
        assert.equal(saved.reference, path);
        assert.equal(saved.binding.owned.executable, path);
    });
}
