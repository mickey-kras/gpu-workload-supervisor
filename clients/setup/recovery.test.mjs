import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {recoveryGuidance} from './onboarding.mjs';

const request = {profile: {statePath: '/state.db'}, catalog: {version: 1, profiles: []}};
const prefix = 'native launch requires a supported direct local command: ';

test('recovery distinguishes account data from protected runtimes without proposing mutations', () => {
    const data = recoveryGuidance({diagnostics: [prefix + 'Environment: /home/u/AI/hub: missing or broken link',
        prefix + 'ComfyUI option --output-directory /home/u/out: world-writable; remove other-write or choose another path', prefix + 'application script: /home/u/main.py: owned by uid 1001, not by you or root']});
    assert.equal(data.length, 2); // One path repair plus reinspection, despite multiple blockers.
    assert.match(data[0], /Fix the named path/);
    assert.match(data.at(-1), /Check again/);
    const scriptLink = recoveryGuidance({diagnostics: ['/home/u/main.py: missing or broken link']});
    assert.match(scriptLink[0], /Fix the named path/);
    const workingDirectory = recoveryGuidance({diagnostics: [prefix + 'WorkingDirectory: /home/u/AI: world-writable; remove other-write or choose another path']})[0];
    assert.match(workingDirectory, /Fix the named path/);
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
        assert.equal(guidance.some(step => /Fix the named path/.test(step)), false, message);
        assert.match(guidance.at(-1), /Check again/);
    }
    const tooMany = recoveryGuidance({diagnostics: [cases.at(-1)[0]]}).join('\n');
    assert.match(tooMany, /at most 32 files/);
    for (const message of ['Environment variable HF_HOME: loaded metadata is ambiguous; reload and retry',
        'Environment variable PATH: loaded setting differs from inspected source; reload and refresh the binding',
        'Environment variable LD_LIBRARY_PATH: loaded setting has no inspected source; reload and refresh the binding',
        'native launch binding changed: loaded WorkingDirectory differs from OS account home expansion; reload and refresh the binding', 'Environment variable HF_HOME: unfamiliar reason']) {
        assert.deepEqual(recoveryGuidance({diagnostics: [message]}), [], message);
    }
});

test('recovery mentions systemd reload only for an identified external service', () => {
    const candidate = {diagnostics: [prefix + 'executable: /opt/x/run: owned by uid 1001, not by you or root']};
    const executable = recoveryGuidance(candidate).join('\n');
    assert.match(executable, /Check again/);
    assert.equal(executable.includes('reload'), false);
    const service = recoveryGuidance({...candidate, unit: 'external.service'}).join('\n');
    assert.match(service, /systemctl --user daemon-reload/);
    assert.match(service, /Check again/);
});

test('unreadable regular-file launch sources retain path repair guidance', () => {
    const message = 'existing launch is unsupported: /home/u/images.service: not a readable regular file';
    const steps = recoveryGuidance({unit: 'images.service', diagnostics: [message]});
    assert.match(steps[0], /^Fix the named path:/);
    assert.match(steps.at(-2), /systemctl --user daemon-reload/);
    assert.match(steps.at(-1), /Check again/);
});

test('nonmember-group writes require a protected path without forbidding own-group writes', () => {
    const message = 'existing launch is unsupported: Environment: /home/u/cache: group-writable by gid 1002, a group you are not in';
    const steps = recoveryGuidance({unit: 'images.service', diagnostics: [message]});
    assert.match(steps[0], /choose a path protected from write access by groups you do not belong to/);
    assert.match(steps[0], /Group write is allowed for your own groups/);
    assert.match(steps.at(-2), /systemctl --user daemon-reload/);
    assert.match(steps.at(-1), /Check again/);
});

test('account and other non-runtime ownership failures retain diagnostics without runtime replacement advice', async () => {
    const account = prefix + '%h requires one valid effective-UID record in trusted root-owned local /etc/passwd; missing or NSS-only accounts are unsupported';
    for (const diagnostic of [account, 'unfamiliar root-owned configuration failure',
        prefix + 'ExecStartPre requires a trusted direct executable',
        prefix + 'selected application must be a supported direct executable',
        'GPU inventory executable did not pass trust checks.', 'unfamiliar interpreter setting']) {
        assert.deepEqual(recoveryGuidance({diagnostics: [diagnostic]}), [], diagnostic);
    }
    // Match production trust wrappers, including nested automatic-launch wrappers,
    // rather than generic references to executable paths or root ownership.
    for (const diagnostic of [
        'executable: /opt/x/run: owned by uid 1001, not by you or root',
        'interpreter: /opt/x/python3: world-writable; remove other-write or choose another path',
        'application script: /opt/x/main.py: missing or broken link',
    ]) assert.match(recoveryGuidance({diagnostics: [prefix + diagnostic]})[0], /^Fix the named path:/, diagnostic);
    const candidate = {app: 'comfyui', label: 'ComfyUI', unit: 'external.service', recognized: false, instanceStatus: 'not-running',
        configurationStatus: 'inspection-failed', diagnostics: [account]};
    const ui = await launch({deferAction: 'unused', responses: {discover: {request, units: [], applications: [candidate]}}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details'); await ui.click('Copy details');
    assert.ok(ui.widgets.find(widget => widget.title === 'GPU Workload Setup').clipboard.includes(account));
    assert.equal(ui.widgets.some(widget => ui.visible(widget) && widget.label?.startsWith('Fix the named path')), false);
    await ui.click('Back'); assert.equal(ui.by('Use installation').sensitive, false);
});

test('reinspection clears obsolete data recovery only after fresh configuration evidence', async () => {
    const blocked = {app: 'comfyui', label: 'ComfyUI', unit: 'images.service', recognized: false, instanceStatus: 'not-running',
        configurationStatus: 'inspection-failed', diagnostics: [prefix + 'Environment: /home/u/AI/cache: missing or broken link']};
    const repaired = {...blocked, diagnostics: [], recognized: true, configurationStatus: 'ready', binding: {unit: blocked.unit, cgroup: '/images', healthURL: 'http://127.0.0.1:8188/system_stats'}};
    let checked = false;
    const ui = await launch({deferAction: 'unused', responses: {discover: () => ({request, units: [], applications: [checked ? repaired : blocked]})}});
    await ui.click('Settings for ComfyUI'); await ui.click('View details');
    assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('Fix the named path')));
    await ui.click('Check again'); await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, false);
    checked = true;
    await ui.click('View details'); await ui.click('Check again');
    assert.equal(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('Fix the named path')), false);
    await ui.click('Back');
    assert.equal(ui.by('Use installation').sensitive, true);
    assert.equal(ui.calls.some(call => ['apply', 'prepare'].includes(call.argv[1])), false);
});

for (const [app, label, port] of [['vllm', 'vLLM', 8000]]) {
    test(`${app}: custom executable selection is retained while blocked and rechecked before review`, async () => {
        const path = `/custom/runtime/${app}`;
        const endpoint = {app, label, instanceStatus: 'unreachable', endpoint: `http://127.0.0.1:${port}`, recognized: false};
        const selected = {app, label, reference: path, referenceKind: 'application', installed: true, recognized: false, instanceStatus: 'inspection-failed',
            configurationStatus: 'inspection-failed', diagnostics: [prefix + 'executable: /custom/runtime/' + app + ': owned by uid 1001, not by you or root'], lifecycleControl: 'unverified'};
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
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label?.includes('Fix the named path')));
        repaired = true;
        await ui.click('Check again'); await ui.click('Back');
        assert.equal(ui.by('Use installation').sensitive, true);
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'Installed'));
        assert.ok(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'GPU operation has not been tested.'));
        assert.equal(ui.widgets.some(widget => ui.visible(widget) && widget.label === 'Reachable'), false);
        assert.equal(ui.calls.some(call => ['apply', 'render-owned', 'prepare'].includes(call.argv[1])), false);
        await ui.click('Advanced settings'); await ui.click('Launch details'); await ui.click('Save selections for later');
        const saved = JSON.parse(ui.calls.at(-1).input).drafts.find(draft => draft.app === app);
        assert.equal(saved.reference, path);
        assert.equal(saved.binding.owned.executable, path);
    });
}

test('production legacy and current backend path failures offer review and reinspection', () => {
    const messages = [
        'Environment variable HOME: requires an existing absolute clean trusted path without links; inspect ownership and permissions of this setting and its ancestors',
        'Environment variable PATH: requires absolute clean root-owned executable directories; aliases must be root-owned and resolve into trusted directories with no group/world writes',
        'Environment variable LD_LIBRARY_PATH: dynamic library search and transitive loader bindings are not qualified; use a reviewed root-owned runtime installation with its normal trusted loader configuration, then reload and refresh the binding',
        'WorkingDirectory requires an existing absolute clean trusted directory without links; inspect ownership and permissions of this setting and its ancestors',
        'ComfyUI option --output-directory requires an existing absolute clean trusted directory without links; inspect this setting and its ancestors',
        'ComfyUI option --extra-model-paths-config requires existing absolute clean trusted regular configuration files without links; inspect this setting and its ancestors',
        'executable trust: executable target is not root-owned; install this executable through the system package manager or a trusted root-owned location',
        'application script component 3 cannot be opened without following links; inspect this component for missing access or symbolic links',
        'application script trust: launch source component 3: group or world write permission permits replacement; inspect this component',
        'Environment: /home/u/cache: path must be absolute and clean',
        'Environment variable PATH: empty search path components can select the current directory',
        'Environment variable LD_PRELOAD: nonempty code-loading overrides are unsupported',
        'ComfyUI option --port requires its supported full option name and value',
    ];
    for (const message of messages) {
        for (const evidence of [{diagnostics: [prefix + message]}, {nextStep: prefix + message}]) {
            const steps = recoveryGuidance({...evidence, unit: 'external.service'});
            assert.ok(steps.length > 2, message);
            assert.match(steps.at(-2), /systemctl --user daemon-reload/);
            assert.match(steps.at(-1), /Check again/);
        }
    }
    const syntax = recoveryGuidance({nextStep: 'Environment variable HOME: quoted fragments are unsupported'}).join('\n');
    assert.match(syntax, /Environment="HOME=\/home\/alice"/);
    assert.equal(syntax.includes('whole value'), false);
    assert.deepEqual(recoveryGuidance({diagnostics: ['unfamiliar reason'], nextStep: messages[0]}), []);
});
