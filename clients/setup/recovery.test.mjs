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
    const scriptLink = recoveryGuidance({diagnostics: ['application script component 3 cannot be opened without following links; inspect this component for missing access or symbolic links']});
    assert.match(scriptLink[0], /Application and data paths:/);
    assert.match(scriptLink[0], /without links/);
    const runtime = recoveryGuidance({diagnostics: [prefix + 'executable: interpreter trust: executable target is group or world writable.']});
    assert.match(runtime[0], /administrator.*protected root-owned/);
    assert.match(runtime[0], /never changes ownership or permissions/);
    assert.match(recoveryGuidance({nextStep: prefix + 'Environment variable PATH: requires absolute clean root-owned executable directories; aliases must be root-owned and resolve into trusted directories with no group/world writes'})[0], /every alias target and ancestor/);
    assert.match(recoveryGuidance({diagnostics: [prefix + 'Environment variable LD_LIBRARY_PATH: dynamic library search and transitive loader bindings are not qualified; use a reviewed root-owned runtime installation with its normal trusted loader configuration, then reload and refresh the binding']})[0], /Simply deleting LD_LIBRARY_PATH does not establish/);
    const workingDirectory = recoveryGuidance({diagnostics: [prefix + 'WorkingDirectory requires an existing absolute clean trusted directory without links; inspect ownership and permissions of this setting and its ancestors']})[0];
    assert.match(workingDirectory, /existing absolute trusted directory/);
    assert.match(workingDirectory, /ancestors/);
    assert.equal(workingDirectory.includes('inside the account home'), false);
    assert.deepEqual(recoveryGuidance({diagnostics: ['CUDA_VISIBLE_DEVICES needs a full GPU UUID.', 'unsupported directive or value Restart', 'ComfyUI option --auto-launch is not qualified.']}), []);
    assert.deepEqual(recoveryGuidance(), []);
});

test('syntax and cardinality blockers do not prescribe filesystem trust repairs', () => {
    const cases = [
        ['Environment variable HOME: expansion, escapes or whitespace are unsupported; use one literal whole assignment', 'Environment syntax:'],
        ['Environment variable PATH: duplicate assignment', 'Environment syntax:'],
        ['Environment variable LD_LIBRARY_PATH: unterminated whole-assignment quote', 'Environment syntax:'],
        ['Environment variable HF_HOME: quoted fragments are unsupported', 'Environment syntax:'],
        ['Environment: expected NAME=value', 'Environment syntax:'],
        ['ComfyUI option --output-directory is repeated; use one unambiguous assignment', 'Launch argument syntax:'],
        ['ComfyUI option --output-directory requires an existing trusted absolute directory', 'Launch argument syntax:'],
        ['ComfyUI option --port requires an explicit value', 'Launch argument syntax:'],
        ['ComfyUI option --extra-model-paths-config requires at least one trusted configuration file', 'Launch argument syntax:'],
        ['ComfyUI option --extra-model-paths-config configuration file count exceeds supported limit; paths were not inspected', 'Configuration-file count:'],
    ];
    for (const [message, heading] of cases) {
        const guidance = recoveryGuidance({diagnostics: [prefix + message]});
        assert.ok(guidance[0].startsWith(heading), message);
        assert.equal(guidance.some(step => /Application and data paths:|Runtime executable:|Runtime libraries:|^PATH:/.test(step)), false, message);
        assert.match(guidance.at(-1), /Check again/);
    }
    const tooMany = recoveryGuidance({diagnostics: [cases.at(-1)[0]]}).join('\n');
    assert.match(tooMany, /at most 32 files/);
    assert.match(tooMany, /Preserve every required model mapping/);
    for (const message of ['Environment variable HF_HOME: loaded metadata is ambiguous; reload and retry',
        'Environment variable PATH: loaded setting differs from inspected source; reload and refresh the binding',
        'Environment variable LD_LIBRARY_PATH: loaded setting has no inspected source; reload and refresh the binding',
        'native launch binding changed: loaded WorkingDirectory differs from OS account home expansion; reload and refresh the binding', 'Environment variable HF_HOME: unfamiliar reason']) {
        assert.deepEqual(recoveryGuidance({diagnostics: [message]}), [], message);
    }
});

test('recovery mentions systemd reload only for an identified external service', () => {
    const candidate = {diagnostics: [prefix + 'executable trust: executable target is not root-owned.']};
    const executable = recoveryGuidance(candidate).join('\n');
    assert.match(executable, /Check again/);
    assert.equal(executable.includes('reload'), false);
    const service = recoveryGuidance({...candidate, unit: 'external.service'}).join('\n');
    assert.match(service, /reload the user systemd manager/);
    assert.match(service, /Check again/);
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
