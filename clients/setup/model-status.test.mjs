import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {launch} from './harness.mjs';

const identity = name => createHash('sha256').update(name).digest('hex');

const workload = 'chat';
const checks = [{name: 'file-allowlist', outcome: 'passed'}, {name: 'upstream-identity', outcome: 'passed'}, {name: 'chat-template', outcome: 'passed'}, {name: 'behavioral-scan', outcome: 'not_run'}];
const stored = {workload, label: 'Chat', kind: 'llm', status: 'not_scanned', staticChecksPassed: true, reviewPending: false, rechecked: false,
    coverage: 'partial', checker: '1', checkedAt: '2026-11-01T00:00:00Z', checks, identity: identity('path:/models/chat')};
// accepted is the accepted managed vLLM workload that the status entries
// describe.
const accepted = {id: workload, label: 'Chat', adapter: 'systemd', bootPolicy: 'stop-to-idle',
    unit: 'gws-owned-vllm-chat.service', cgroup: '/user/app.slice/gws-owned-vllm-chat.service', healthURL: 'http://127.0.0.1:8000/health',
    nativeModel: {runtime: 'vllm', instance: 'local', model: 'chat', endpoint: 'http://127.0.0.1:8000',
        launchFile: '/home/user/.config/systemd/user/gws-owned-vllm-chat.service', launchSHA256: 'digest', owned: {port: 8000, modelPath: '/models/chat'}}};

// openSettings opens the settings of the accepted workload with a
// model-status report that holds entry.
async function openSettings(entry, options = {}) {
    const ui = await launch({deferAction: 'unused', profiles: [accepted], ...options, responses: {'model-status': {version: 1, models: entry ? [{...stored, ...entry}] : []}, ...options.responses}});
    await ui.by('Edit application').emit('clicked');
    ui.by('Settings for vLLM').emit('clicked');
    return ui;
}
const shown = (ui, label) => ui.visible(ui.by(label)) && ui.by(label).label === label;
const settle = () => new Promise(resolve => setImmediate(resolve));
// statusRow gives the icon and the second line of the card that shows title.
function statusRow(ui, title) {
    const label = ui.by(title);
    const text = ui.widgets.find(widget => widget.children.includes(label));
    const row = ui.widgets.find(widget => widget.children.includes(text));
    return {icon: row.children[0].icon_name, detail: text.children[1]};
}

test('each stored status shows an icon and a text, never a safe model', async () => {
    for (const [entry, title, detail, icon] of [
        [{status: 'passed', staticChecksPassed: false}, 'Scanned, no findings', null, 'emblem-ok-symbolic'],
        [{status: 'findings', staticChecksPassed: false}, 'Findings detected', null, 'dialog-warning-symbolic'],
        [{status: 'error', staticChecksPassed: false}, 'Scan failed', null, 'dialog-error-symbolic'],
        [{status: 'unsupported', staticChecksPassed: false}, 'Scan not available for this model', null, 'dialog-information-symbolic'],
        [{}, 'Not scanned', 'File checks passed. Behavioral scan not available yet.', 'action-unavailable-symbolic'],
        [{staticChecksPassed: false, reason: 'checker-changed'}, 'Not scanned', 'The stored result is from another checker version.', 'action-unavailable-symbolic'],
        [{staticChecksPassed: false, reason: 'template-changed'}, 'Not scanned', 'The chat template of the launch is not the checked template.', 'action-unavailable-symbolic'],
        [{status: 'findings', staticChecksPassed: false, reviewPending: true}, 'Findings detected', 'A chat template review is pending.', 'dialog-warning-symbolic'],
        [{status: 'safe', staticChecksPassed: false, reason: 'other-directory'}, 'Not scanned', 'The stored result is for another model folder.', 'action-unavailable-symbolic'],
        [{kind: 'image', staticChecksPassed: false}, 'No backdoor scan available for image models', null, 'dialog-information-symbolic'],
        [{kind: 'unidentified', staticChecksPassed: false, reason: 'files-unidentified'}, 'Model files not identified', 'Setup cannot identify the model files of this workload.', 'dialog-warning-symbolic'],
        [null, 'Not scanned', 'No check result is stored for this model.', 'action-unavailable-symbolic'],
    ]) {
        const ui = await openSettings(entry);
        assert.ok(shown(ui, 'Model scan'), title);
        assert.ok(shown(ui, title), title);
        const row = statusRow(ui, title);
        assert.equal(row.icon, icon, title);
        if (detail) assert.equal(row.detail.label, detail, title);
        else assert.equal(row.detail.visible, false, `${title} has no second line`);
        assert.ok(!ui.widgets.some(widget => /\bsafe\b/i.test(widget.label ?? '')), 'no status reads as safe');
    }
});

test('a selection that changes the model does not show the accepted result', async () => {
    const ui = await openSettings({});
    assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'));
    ui.edit(ui.by('Model directory'), 'text', '/models/other');
    await ui.click('Advanced settings'); await ui.click('Back');
    assert.ok(shown(ui, 'The model changed in this selection. It has not been checked.'));
    assert.equal(ui.visible(ui.by('Check files again')), false, 'a recheck would read the files of the accepted model');
    assert.equal(ui.by('File checks passed. Behavioral scan not available yet.'), undefined);
});

test('scan details show the revisions, coverage, date and the defensive note on request', async () => {
    const ui = await openSettings({});
    const toggle = ui.by('Scan details');
    const details = ui.widgets.find(widget => widget.label?.startsWith('Checker revision'));
    assert.equal(ui.visible(details), false);
    assert.equal(toggle.accessibleStates.expanded, 0);
    await ui.click('Scan details');
    assert.equal(ui.visible(details), true);
    assert.equal(toggle.accessibleStates.expanded, 1);
    assert.equal(details.label, ['Checker revision: 1', 'Scanner revision: not available', 'Coverage: partial', 'Checked: 2026-11-01 00:00 UTC',
        'File allowlist: passed', 'Upstream identity: passed', 'Chat template: passed', 'Behavioral scan: not run',
        'These checks are a defensive layer, not complete backdoor detection.'].join('\n'));
    await ui.click('Scan details');
    assert.equal(ui.visible(details), false);
    const missing = await openSettings({checker: '', coverage: '', checkedAt: undefined, checks: [{name: 'custom', outcome: 'odd'}, {name: 'chat-template', outcome: 'error'}, {name: 'upstream-identity', outcome: 'failed'}, {name: 'file-allowlist', outcome: 'unsupported'}]});
    assert.match(missing.widgets.find(widget => widget.label?.startsWith('Checker revision')).label, /^Checker revision: not available\nScanner revision: not available\nCoverage: none\nChecked: never\ncustom: odd\nChat template: failed to run\nUpstream identity: failed\nFile allowlist: not available for this model\n/);
    assert.equal(missing.visible(missing.by('Check files again')), false, 'no stored digests to check against');
    const stale = await openSettings({staticChecksPassed: false, reason: 'checker-changed'});
    assert.match(stale.widgets.find(widget => widget.label?.startsWith('Checker revision')).label, /Checked: 2026-11-01 00:00 UTC\nLast stored result\. It does not apply now:\nFile allowlist: passed/);
    assert.equal(stale.visible(stale.by('Check files again')), false, 'a recheck cannot change another checker version');
    for (const reason of ['template-changed', 'other-directory']) {
        const ui2 = await openSettings({staticChecksPassed: false, reason});
        assert.equal(ui2.visible(ui2.by('Check files again')), false, `a recheck cannot change ${reason}`);
    }
});

test('check files again reads the files once and shows the result', async () => {
    for (const [reason, title, note] of [
        ['files-changed', 'The model files changed after the last check.', 'The model files changed after the last check. The stored result no longer applies.'],
        ['files-unavailable', 'Setup cannot read the model files.', 'Setup cannot read the model files. The stored result does not apply.'],
        ['', 'File checks passed. Behavioral scan not available yet.', 'The model files match the last check.'],
    ]) {
        const ui = await openSettings({}, {deferAction: 'model-status', deferOccurrence: 2, responses: {'model-status': input => ({version: 1, models: [{...stored,
            ...(input.recheck ? {rechecked: true, reason, staticChecksPassed: !reason} : {})}]})}});
        assert.ok(!ui.widgets.some(widget => widget.label === '' && widget.cssClasses?.includes('dim-label') && ui.visible(widget)), 'no empty note');
        const checking = ui.click('Check files again');
        const call = ui.calls.at(-1);
        assert.deepEqual(JSON.parse(call.input), {version: 1, recheck: [workload]});
        assert.ok(call.cancellable, 'the check can be canceled');
        assert.ok(shown(ui, 'Checking model files…'));
        assert.ok(shown(ui, 'Cancel check'));
        assert.equal(ui.by('Cancel check').focused, true, 'focus moves to the control that stops the check');
        assert.equal(ui.visible(ui.by('Check files again')), false);
        ui.finish(); await checking;
        assert.ok(shown(ui, 'Not scanned'));
        assert.ok(shown(ui, title), title);
        assert.ok(shown(ui, note), note);
        assert.equal(ui.visible(ui.by('Cancel check')), false);
        assert.equal(ui.by('Check files again').focused, true);
    }
});

test('a check that read no files shows its reason, not a match', async () => {
    const unread = await openSettings({}, {responses: {'model-status': input => ({version: 1, models: [{...stored, ...(input.recheck ? {rechecked: false} : {})}]})}});
    await unread.click('Check files again');
    assert.equal(unread.by('The model files match the last check.'), undefined, 'no match without a comparison');
    const ui = await openSettings({}, {responses: {'model-status': input => ({version: 1, models: [{...stored,
        ...(input.recheck ? {rechecked: false, reason: 'other-directory', staticChecksPassed: false} : {})}]})}});
    await ui.click('Check files again');
    assert.equal(ui.by('The model files match the last check.'), undefined);
    assert.equal(ui.widgets.filter(widget => widget.label === 'The stored result is for another model folder.' && ui.visible(widget)).length, 2);
    assert.equal(ui.visible(ui.by('Check files again')), false);
    assert.equal(ui.by('Scan details').focused, true, 'focus moves to a visible control');
});

test('a canceled or failed check keeps the last result', { timeout: 5000 }, async () => {
    const ui = await openSettings({}, {deferAction: 'model-status', deferOccurrence: 2, responses: {'model-status': input => ({version: 1, models: [input.recheck ? {...stored, rechecked: true, reason: 'files-changed', staticChecksPassed: false} : stored]})}});
    const checking = ui.click('Check files again');
    await ui.click('Cancel check'); await checking;
    assert.ok(shown(ui, 'Check canceled. The last result is kept.'));
    assert.equal(ui.by('Check files again').focused, true);
    assert.equal(ui.calls.at(-1).cancellable.is_cancelled(), true);
    assert.equal(ui.calls.at(-1).killed, true, 'the subprocess was stopped');
    assert.equal(ui.by('The model files changed after the last check.'), undefined, 'the canceled result is not shown');
    assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'), 'the last result is kept');
    ui.finish();
    assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'));
    assert.ok(!ui.by('Checker revision: 1')?.label?.includes('Error'));
    const failed = await openSettings({}, {responses: {'model-status': input => ({version: 1, models: input.recheck ? [] : [stored]})}});
    await failed.click('Check files again');
    assert.ok(shown(failed, 'The files could not be checked again. The last result is kept.'));
    assert.equal(failed.by('Check files again').focused, true);
    assert.ok(shown(failed, 'File checks passed. Behavioral scan not available yet.'));
    assert.match(failed.widgets.find(widget => widget.label?.startsWith('Checker revision')).label, /\nError: the status action of gpu-setup failed\.\n/);
});

test('the status shows while it loads and when it cannot be read', async () => {
    const loading = await openSettings({}, {deferAction: 'model-status'});
    assert.ok(shown(loading, 'Reading scan status…'));
    loading.finish(); await settle();
    assert.ok(shown(loading, 'Not scanned'));
    const ui = await openSettings({}, {fail: 'model-status'});
    assert.ok(shown(ui, 'Scan status not available'));
    assert.ok(shown(ui, 'Setup could not read the stored check results.'));
    assert.equal(ui.visible(ui.by('Check files again')), false);
    const details = ui.widgets.find(widget => widget.label?.startsWith('Checker revision')).label;
    assert.match(details, /\nError: the status action of gpu-setup failed\.\n/);
    assert.ok(!details.includes('Backend unavailable'), 'the details hold no raw error output');
    const other = await openSettings(null, {responses: {'model-status': {version: 2, models: [stored]}}});
    assert.ok(shown(other, 'Scan status not available'), 'a report of another version is not read');
    for (const instance of [ui, other]) assert.ok(instance.calls.some(call => call.argv[1] === 'discover'), 'setup continues');
});

test('the review summary shows the status of each workload', async () => {
    const request = {profile: {statePath: '/state.db', gpuIndex: 2}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
    const endpoint = 'http://127.0.0.1:9000';
    const prepare = app => {
        const prepared = {id: `${app}-existing`, label: app, adapter: 'systemd', unit: `${app}.service`, cgroup: '/trusted/service', healthURL: `${endpoint}/health`, bootPolicy: 'stop-to-idle'};
        if (app === 'vllm') prepared.nativeModel = {runtime: app, instance: 'local', model: 'existing', endpoint, launchFile: '/installed/service.conf'};
        else prepared.launchBinding = {runtime: app, endpoint, launchFile: '/installed/service.conf', launchSHA256: 'original'};
        return prepared;
    };
    const passed = {...stored, workload: 'vllm-existing', identity: identity('model:existing'), templateSHA256: 'older-pin'};
    // A result with findings never has a passed template check: the template
    // check runs after the file and identity checks passed.
    const findings = {...passed, status: 'findings', staticChecksPassed: false, checks: checks.map(check => check.name === 'chat-template' ? {...check, outcome: 'failed'} : check)};
    for (const [app, label, accept, models, title, detail, pin] of [
        ['comfyui', 'ComfyUI', false, [], 'No backdoor scan available for image models', null],
        ['vllm', 'vLLM', false, [], 'Not scanned', 'No check result is stored for this model.'],
        ['vllm', 'vLLM', true, [{...findings, templateSHA256: undefined}], 'Findings detected', null],
        ['vllm', 'vLLM', 'other', [{...findings, identity: identity('model:older')}], 'Not scanned', 'The model changed in this selection. It has not been checked.'],
        ['vllm', 'vLLM', true, [findings], 'Findings detected', null, 'new-pin'],
        ['vllm', 'vLLM', true, [passed], 'Not scanned', 'The chat template of the launch is not the checked template.', 'new-pin'],
        ['vllm', 'vLLM', true, [passed], 'Not scanned', 'File checks passed. Behavioral scan not available yet.', 'older-pin'],
        ['vllm', 'vLLM', true, [passed], 'Not scanned', 'The chat template of the launch is not the checked template.'],
        ['vllm', 'vLLM', true, [{...passed, reason: 'checker-changed', staticChecksPassed: false}], 'Not scanned', 'The stored result is from another checker version.', 'new-pin'],
        ['vllm', 'vLLM', true, [{...passed, templateSHA256: undefined}], 'Not scanned', 'File checks passed. Behavioral scan not available yet.'],
    ]) {
        const prepared = prepare(app);
        if (pin) prepared.nativeModel.chatTemplateSHA256 = pin;
        const installation = {app, label: app, unit: `${app}.service`, endpoint, cgroup: '/trusted/service', location: '/installed/service.conf', recognized: true,
            configurationStatus: 'ready', instanceStatus: 'not-running', models: app === 'comfyui' ? [] : [{id: 'existing'}],
            binding: {unit: `${app}.service`, cgroup: '/trusted/service', healthURL: `${endpoint}/health`, model: app === 'comfyui' ? '' : 'existing', launchFile: '/installed/service.conf'}};
        const acceptedProfile = {...prepared, nativeModel: {...prepared.nativeModel, model: accept === 'other' ? 'older' : 'existing'}};
        const ui = await launch({deferAction: 'unused', responses: {discover: {request: {...request, catalog: {version: 1, profiles: accept ? [acceptedProfile] : []}}, units: [], applications: [installation]},
            prepare: {profile: prepared}, fingerprint: {sha256: 'fresh', ...(pin ? {chatTemplateSHA256: pin} : {})}, 'model-status': {version: 1, models}}});
        if (accept) await ui.by('Edit application').emit('clicked');
        else ui.edit(ui.by(`Use ${label}`), 'active', true);
        ui.by(`Settings for ${label}`).emit('clicked');
        await ui.by('Use installation').emit('clicked');
        assert.equal(ui.by('Finish setup').sensitive, true, `${app} ${accept}`);
        const card = ui.widgets.find(widget => widget.cssClasses?.includes('setup-summary-card') && ui.visible(widget));
        const texts = [];
        const collect = widget => { if (widget.label && widget.visible !== false) texts.push(widget.label); widget.children.forEach(collect); };
        collect(card);
        assert.ok(texts.includes(title), `${app} ${accept} ${pin}: ${texts}`);
        if (detail) assert.ok(texts.includes(detail), `${app} ${accept} ${pin}: ${texts}`);
        else assert.ok(!texts.some(text => /changed|template/.test(text)), `${app} ${accept} ${pin}: ${texts}`);
        if (!detail?.includes('template')) assert.ok(!texts.some(text => text.includes('chat template')), `${app} ${accept} ${pin}: ${texts}`);
        assert.ok(!texts.includes('Check files again'), 'the summary only shows the status');
    }
});

test('an interrupted setup shows the stored status only for the accepted launch', async () => {
    for (const [modelPath, title] of [['/models/new', 'The model changed in this selection. It has not been checked.'], ['/models/chat', 'File checks passed. Behavioral scan not available yet.']]) {
        const pending = {...accepted, nativeModel: {...accepted.nativeModel, owned: {...accepted.nativeModel.owned, modelPath}}};
        const ui = await launch({pending: true, profiles: [pending], responses: {'model-status': {version: 1, models: [stored]}}});
        const reviewing = (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
        ui.finish(); await reviewing;
        const card = ui.widgets.find(widget => widget.cssClasses?.includes('setup-summary-card') && ui.visible(widget));
        const texts = [];
        const collect = widget => { if (widget.label && widget.visible !== false) texts.push(widget.label); widget.children.forEach(collect); };
        collect(card);
        assert.ok(texts.includes(title), `${modelPath}: ${texts}`);
    }
});

test('closing setup stops a running check', async () => {
    for (const [later, close] of [[false, ui => ui.widgets.find(widget => widget.widgetType === 'ApplicationWindow').close()], [true, ui => ui.click('Set up later')]]) {
        const ui = await openSettings({}, {deferAction: 'model-status', deferOccurrence: 2});
        const checking = ui.click('Check files again');
        const call = ui.calls.at(-1);
        if (later) {
            await ui.click('Cancel');
            ui.by('Cancel check').grab_focus();
        }
        await close(ui); await checking;
        assert.equal(call.killed, true, 'the gpu-setup process was stopped');
        assert.notEqual(ui.by('Check files again').focused, true, 'a closed setup takes no focus');
    }
});

test('closing setup stops the status load', async () => {
    const ui = await openSettings({}, {deferAction: 'model-status'});
    const load = ui.calls.find(call => call.argv[1] === 'model-status');
    ui.widgets.find(widget => widget.widgetType === 'ApplicationWindow').close();
    await settle();
    assert.equal(load.killed, true, 'the gpu-setup process was stopped');
});

test('the settings card compares the model, not the pin of the selection', async () => {
    const pinned = {...accepted, nativeModel: {...accepted.nativeModel, chatTemplateSHA256: 'accepted-pin'}};
    const ui = await openSettings({templateSHA256: 'accepted-pin'}, {profiles: [pinned]});
    assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'));
    assert.equal(ui.by('The chat template of the launch is not the checked template.'), undefined);
});

test('a note of a check goes when the card shows another entry', async () => {
    const ui = await openSettings({}, {responses: {'model-status': input => ({version: 1, models: [{...stored, ...(input.recheck ? {rechecked: true} : {})}]})}});
    await ui.click('Check files again');
    assert.ok(shown(ui, 'The model files match the last check.'));
    ui.edit(ui.by('Model directory'), 'text', '/models/other');
    await ui.click('Advanced settings'); await ui.click('Back');
    assert.ok(shown(ui, 'The model changed in this selection. It has not been checked.'));
    assert.equal(ui.visible(ui.by('The model files match the last check.')), false, 'the note does not contradict the status');
});

test('a failed status action is written to the log, not to the card', async () => {
    const ui = await openSettings({}, {fail: 'model-status'});
    assert.ok(shown(ui, 'Scan status not available'));
    assert.deepEqual(ui.logs, ['gpu-setup model-status failed: Backend unavailable']);
    assert.ok(!ui.widgets.some(widget => widget.label?.includes('Backend unavailable')));
    const failed = await openSettings({}, {responses: {'model-status': input => ({version: 1, models: input.recheck ? [] : [stored]})}});
    await failed.click('Check files again');
    assert.deepEqual(failed.logs, ['gpu-setup model-status failed: The report has no status for this workload.']);
    const canceled = await openSettings({}, {deferAction: 'model-status', deferOccurrence: 2});
    const checking = canceled.click('Check files again');
    await canceled.click('Cancel check'); await checking;
    assert.deepEqual(canceled.logs, [], 'a canceled check is not a failure');
    const closed = await openSettings({}, {deferAction: 'model-status'});
    closed.widgets.find(widget => widget.widgetType === 'ApplicationWindow').close();
    await settle();
    assert.deepEqual(closed.logs, [], 'a canceled load is not a failure');
});

test('a new editor of a workload gets its own status card', async () => {
    const endpoint = 'http://127.0.0.1:9000';
    const prepared = {id: 'vllm-existing', label: 'vllm', adapter: 'systemd', unit: 'vllm.service', cgroup: '/trusted/service', healthURL: `${endpoint}/health`, bootPolicy: 'stop-to-idle',
        nativeModel: {runtime: 'vllm', instance: 'local', model: 'existing', endpoint, launchFile: '/installed/service.conf'}};
    const installation = {app: 'vllm', label: 'vllm', unit: 'vllm.service', endpoint, cgroup: '/trusted/service', location: '/installed/service.conf', recognized: true,
        configurationStatus: 'ready', instanceStatus: 'not-running', models: [{id: 'existing'}],
        binding: {unit: 'vllm.service', cgroup: '/trusted/service', healthURL: `${endpoint}/health`, model: 'existing', launchFile: '/installed/service.conf'}};
    const request = {profile: {statePath: '/state.db', gpuIndex: 2}, catalog: {version: 1, profiles: [prepared]}, expectedRevision: 7};
    const ui = await launch({deferAction: 'model-status', deferOccurrence: 2, responses: {discover: {request, units: [], applications: [installation]}, prepare: {profile: prepared}, fingerprint: {sha256: 'fresh'},
        'model-status': {version: 1, models: [{...stored, workload: 'vllm-existing', identity: identity('model:existing')}]}}});
    const cards = () => ui.widgets.filter(widget => widget.label === 'Model scan');
    await ui.by('Edit application').emit('clicked');
    ui.by('Settings for vLLM').emit('clicked');
    const checking = ui.click('Check files again');
    const recheck = ui.calls.at(-1);
    await ui.by('Use installation').emit('clicked');
    await checking;
    assert.equal(recheck.cancellable.is_cancelled(), true, 'Use installation removes the selection and stops its check');
    assert.equal(recheck.killed, true);
    assert.equal(ui.by('Finish setup').sensitive, true);
    await ui.by('Back').emit('clicked');
    await ui.widgets.filter(widget => widget.label === 'Edit application').at(-1).emit('clicked');
    ui.by('Settings for vLLM').emit('clicked');
    assert.equal(cards().length, 2);
    assert.ok(ui.visible(cards().at(-1)), 'the settings of the new editor show the status');
    assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'));
});

test('the error of a failed check goes with its note', async () => {
    const ui = await openSettings({}, {responses: {'model-status': input => ({version: 1, models: input.recheck ? [] : [stored]})}});
    await ui.click('Check files again');
    const details = () => ui.widgets.find(widget => widget.label?.startsWith('Checker revision')).label;
    assert.match(details(), /\nError: /);
    ui.edit(ui.by('Model directory'), 'text', '/models/other');
    await ui.click('Advanced settings'); await ui.click('Back');
    assert.ok(shown(ui, 'The model changed in this selection. It has not been checked.'));
    assert.doesNotMatch(details(), /\nError: /);
});
