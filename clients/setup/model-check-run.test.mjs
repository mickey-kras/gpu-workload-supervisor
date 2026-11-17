import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {launch} from './harness.mjs';

const identity = name => createHash('sha256').update(name).digest('hex');
const revision = 'c'.repeat(40);
const cachePath = `/hf/models--example-org--chat/snapshots/${revision}`;
const workload = 'chat';
const checks = [{name: 'file-allowlist', outcome: 'passed'}, {name: 'upstream-identity', outcome: 'passed'}, {name: 'chat-template', outcome: 'passed'}, {name: 'behavioral-scan', outcome: 'not_run'}];
const stored = {workload, label: 'Chat', kind: 'llm', status: 'not_scanned', staticChecksPassed: true, reviewPending: false, rechecked: false, checkAvailable: true, launchBlocked: false,
    coverage: 'partial', checker: '1', checkedAt: '2026-11-01T00:00:00Z', checks, identity: identity(`path:${cachePath}`)};
const findings = {...stored, status: 'findings', staticChecksPassed: false, launchBlocked: true, checkedAt: '2026-11-02T00:00:00Z',
    checks: [checks[0], {name: 'upstream-identity', outcome: 'failed', reason: 'upstream-identity-mismatch'}, {name: 'chat-template', outcome: 'not_run'}, checks[3]]};
const accepted = {id: workload, label: 'Chat', adapter: 'systemd', bootPolicy: 'stop-to-idle',
    unit: 'gws-owned-vllm-chat.service', cgroup: '/user/app.slice/gws-owned-vllm-chat.service', healthURL: 'http://127.0.0.1:8000/health',
    nativeModel: {runtime: 'vllm', instance: 'local', model: 'chat', endpoint: 'http://127.0.0.1:8000',
        launchFile: '/home/user/.config/systemd/user/gws-owned-vllm-chat.service', launchSHA256: 'digest', owned: {port: 8000, modelPath: cachePath}}};
const review = {version: 1, workload, pending: true, templateFile: 'chat_template.jinja', chatTemplateSHA256: 'a'.repeat(64), reviewSHA256: 'b'.repeat(64),
    items: [{line: 1, text: 'Answer in English.'}, {line: 3, text: ' Be brief.\n'}]};

async function openSettings(entry, options = {}) {
    const ui = await launch({deferAction: 'unused', profiles: [accepted], ...options, responses: {'model-status': {version: 1, models: entry ? [{...stored, ...entry}] : []}, ...options.responses}});
    await ui.by('Edit application').emit('clicked');
    ui.by('Settings for vLLM').emit('clicked');
    return ui;
}
const shown = (ui, label) => ui.visible(ui.by(label)) && (ui.by(label).label === label || ui.by(label).title === label);
const repo = ui => ui.by('Repository (owner/name)');
const rev = ui => ui.by('Revision (40-character commit ID)');
const settle = () => new Promise(resolve => setImmediate(resolve));

test('run checks prefills the source of a cache folder and warns before the run', async () => {
    const ui = await openSettings({});
    assert.equal(repo(ui).text, 'example-org/chat');
    assert.equal(rev(ui).text, revision);
    assert.ok(shown(ui, 'A result with an error, findings, an unsupported model or a pending template review blocks the launch of this workload until a new check allows it.'));
    assert.ok(shown(ui, 'Run checks'));
    assert.equal(ui.by('Run checks').sensitive, true);
    const fromRecord = await openSettings({source: {repo: 'other-org/model', revision: 'd'.repeat(40)}});
    assert.equal(repo(fromRecord).text, 'other-org/model', 'the stored source wins over the folder name');
    const plain = await openSettings({}, {profiles: [{...accepted, nativeModel: {...accepted.nativeModel, owned: {port: 8000, modelPath: '/models/chat'}}}],
        responses: {'model-status': {version: 1, models: [{...stored, identity: identity('path:/models/chat')}]}}});
    assert.equal(repo(plain).text, '');
    assert.equal(plain.by('Run checks').sensitive, false, 'no run without a source');
});

test('the source fields refuse a repository or revision that is not valid', async () => {
    const ui = await openSettings({});
    for (const [row, value, hint] of [[repo, 'chat', 'Enter the Hugging Face repository as owner/name.'], [repo, 'org/..', 'Enter the Hugging Face repository as owner/name.'],
        [rev, 'main', 'Enter the full commit ID: 40 lowercase hex characters. A branch or tag name is not a pin.'], [rev, 'C'.repeat(40), 'Enter the full commit ID: 40 lowercase hex characters. A branch or tag name is not a pin.']]) {
        const before = row(ui).text;
        ui.edit(row(ui), 'text', value);
        assert.ok(shown(ui, hint), value);
        assert.ok(row(ui).cssClasses.includes('error'), value);
        assert.equal(ui.by('Run checks').sensitive, false, value);
        ui.edit(row(ui), 'text', before);
        assert.equal(ui.visible(ui.by(hint)), false);
        assert.ok(!row(ui).cssClasses.includes('error'));
    }
    ui.edit(repo(ui), 'text', '');
    assert.equal(ui.visible(ui.by('Enter the Hugging Face repository as owner/name.')), false, 'an empty field is not an error');
    assert.equal(ui.by('Run checks').sensitive, false);
});

test('run checks stores the result and shows it', async () => {
    const ui = await openSettings({}, {deferAction: 'model-check', responses: {'model-check': findings}});
    ui.edit(repo(ui), 'text', ' example-org/chat ');
    const running = ui.click('Run checks');
    const call = ui.calls.at(-1);
    assert.deepEqual([...call.argv], ['/usr/bin/gpu-setup', 'model-check']);
    assert.deepEqual(JSON.parse(call.input), {version: 1, workload, source: {repo: 'example-org/chat', revision}});
    assert.ok(call.cancellable, 'the run can be canceled');
    assert.ok(shown(ui, 'Running checks… This can take a long time for large models.'));
    assert.equal(ui.by('Cancel check').focused, true, 'focus moves to the control that stops the run');
    assert.equal(ui.visible(ui.by('Run checks')), false);
    assert.equal(ui.visible(ui.by('Check files again')), false);
    ui.finish(); await running;
    assert.ok(shown(ui, 'Findings detected'));
    assert.ok(shown(ui, 'Checks finished. This result blocks the launch of this workload. A model file differs from the repository at this revision.'));
    assert.equal(ui.by('Run checks').focused, true, 'focus returns to Run checks');
    assert.match(ui.widgets.find(widget => widget.label?.startsWith('Checker revision')).label, /Upstream identity: failed/);
    const passed = await openSettings({}, {responses: {'model-check': {...stored, checkedAt: '2026-11-03T00:00:00Z'}}});
    await passed.click('Run checks');
    assert.ok(shown(passed, 'Checks finished. The result is stored.'));
    const pending = await openSettings({}, {responses: {'model-check': {...findings, reviewPending: true}}});
    await pending.click('Run checks');
    assert.ok(shown(pending, 'Checks finished. This result blocks the launch of this workload. A model file differs from the repository at this revision. Review the chat template below.'));
    assert.ok(shown(pending, 'Review chat template'));
});

test('a canceled run keeps the last result', async () => {
    const ui = await openSettings({}, {deferAction: 'model-check', responses: {'model-check': findings}});
    const running = ui.click('Run checks');
    await ui.click('Cancel check'); await running;
    assert.equal(ui.calls.findLast(call => call.argv[1] === 'model-check').killed, true, 'the gpu-setup process was stopped');
    assert.equal(ui.calls.at(-1).argv[1], 'model-status', 'the card reads what is stored');
    assert.ok(shown(ui, 'Check canceled. The last result is kept.'));
    assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'));
    assert.equal(ui.by('Findings detected'), undefined);
    assert.equal(ui.by('Run checks').focused, true);
    assert.deepEqual(ui.logs, [], 'a canceled run is not a failure');
    ui.finish();
    assert.equal(ui.by('Findings detected'), undefined);
});

test('a run whose hub listing failed shows the failure and keeps the stored result', async () => {
    for (const [reason, text] of [['upstream-unreachable', 'Setup could not reach Hugging Face.'], ['upstream-not-found', 'Hugging Face did not find this repository or revision.'],
        ['upstream-denied', 'Hugging Face refused access. Check your Hugging Face login.'], ['upstream-moved', 'Hugging Face moved this repository. Enter its current name.'],
        ['upstream-history-too-long', 'The branch history is too long to find this revision. Enter a commit that a branch or a tag points at.'], ['upstream-invalid', 'Hugging Face gave a file list that setup cannot read.'], ['upstream-new', 'Reason code: upstream-new']]) {
        const failure = {...stored, status: 'error', staticChecksPassed: false, notStored: true, launchBlocked: true,
            checks: [checks[0], {name: 'upstream-identity', outcome: 'error', reason}, {name: 'chat-template', outcome: 'not_run'}, checks[3]]};
        const ui = await openSettings({}, {responses: {'model-check': failure}});
        await ui.click('Run checks');
        assert.ok(shown(ui, `This run failed. ${text} The last stored result is kept.`), reason);
        assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'), 'the stored result stays');
        assert.equal(ui.by('Scan failed'), undefined);
        assert.equal(repo(ui).text, 'example-org/chat', 'the source stays for another run');
    }
});

test('a failed run writes the error to the log and keeps the last result', async () => {
    const ui = await openSettings({}, {fail: 'model-check'});
    await ui.click('Run checks');
    assert.ok(shown(ui, 'The checks did not store a result. The last result is kept.'));
    assert.deepEqual(ui.logs, ['gpu-setup model-check failed: Backend unavailable']);
    assert.match(ui.widgets.find(widget => widget.label?.startsWith('Checker revision')).label, /\nError: the check action of gpu-setup failed\.\n/);
    assert.ok(!ui.widgets.some(widget => widget.label?.includes('Backend unavailable')));
    const other = await openSettings({}, {responses: {'model-check': {...stored, workload: 'other'}}});
    await other.click('Run checks');
    assert.deepEqual(other.logs, ['gpu-setup model-check failed: The result is not for this workload.']);
});

test('run checks is not offered for a workload that setup cannot check', async () => {
    const adopted = await openSettings({checkAvailable: false});
    assert.equal(adopted.visible(adopted.by('Run checks')), false);
    assert.equal(adopted.visible(repo(adopted)), false);
    assert.ok(shown(adopted, 'Setup can check only a model folder of a managed launch. This workload names only a model ID.'));
    const changed = await openSettings({});
    changed.edit(changed.by('Model directory'), 'text', '/models/other');
    await changed.click('Advanced settings'); await changed.click('Back');
    assert.equal(changed.visible(changed.by('Run checks')), false, 'a run would check the accepted model, not the selection');
    assert.equal(changed.visible(changed.by('Setup can check only a model folder of a managed launch. This workload names only a model ID.')), false);
    const unread = await openSettings({}, {fail: 'model-status'});
    assert.equal(unread.visible(unread.by('Run checks')), false);
});

test('the operator reads the review list and approves it', async () => {
    const approved = {...stored, checkedAt: '2026-11-04T00:00:00Z'};
    const ui = await openSettings({...findings, reviewPending: true}, {deferAction: 'model-approve', responses: {'model-review': review, 'model-approve': approved}});
    const toggle = ui.by('Review chat template');
    assert.ok(shown(ui, 'Review chat template'));
    assert.equal(toggle.accessibleStates.expanded, 0);
    await ui.click('Review chat template'); await settle();
    assert.equal(toggle.accessibleStates.expanded, 1);
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input), {version: 1, workload});
    assert.ok(shown(ui, 'Line 1: "Answer in English."'));
    assert.ok(shown(ui, 'Line 3: " Be brief.\\n"'), 'hidden text is visible');
    assert.ok(shown(ui, `Template file: chat_template.jinja\nTemplate SHA-256: ${'a'.repeat(64)}\nItems: 2`));
    const approving = ui.click('Approve template');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input), {version: 1, workload, chatTemplateSHA256: 'a'.repeat(64), reviewSHA256: 'b'.repeat(64)});
    assert.equal(ui.by('Cancel approval').focused, true);
    ui.finish(); await approving;
    assert.ok(shown(ui, 'Template approved for this template SHA-256.'));
    assert.ok(shown(ui, 'File checks passed. Behavioral scan not available yet.'));
    assert.equal(ui.visible(ui.by('Review chat template')), false, 'no review is pending');
    assert.equal(ui.by('Scan details').focused, true, 'focus moves to a visible control');
    assert.equal(toggle.accessibleStates.expanded, 0);
});

test('a failed approval reads the status and the review list again', async () => {
    let reads = 0;
    const ui = await openSettings({...findings, reviewPending: true}, {fail: 'model-approve', responses: {'model-review': () => ({...review, items: [{line: 1, text: `Read ${++reads}`}]})}});
    await ui.click('Review chat template'); await settle();
    assert.ok(shown(ui, 'Line 1: "Read 1"'));
    const statusReads = ui.calls.filter(call => call.argv[1] === 'model-status').length;
    await ui.click('Approve template'); await settle();
    assert.ok(shown(ui, 'The approval was not stored.'));
    assert.equal(ui.calls.filter(call => call.argv[1] === 'model-status').length, statusReads + 1, 'the status is read again');
    assert.ok(shown(ui, 'Line 1: "Read 2"'), 'the review list is read again');
    const listed = label => ui.widgets.some(widget => widget.label === label && ui.widgets.some(parent => parent.children.includes(widget)));
    assert.equal(listed('Line 1: "Read 1"'), false, 'the old list goes');
    assert.deepEqual(ui.logs, ['gpu-setup model-approve failed: Backend unavailable']);
    assert.match(ui.widgets.find(widget => widget.label?.startsWith('Checker revision')).label, /\nError: the approval action of gpu-setup failed\.\n/);
    assert.equal(ui.by('Review chat template').focused, true);
});

test('a review list that cannot be read offers no approval', async () => {
    for (const [options, text, logs] of [
        [{fail: 'model-review'}, 'Setup could not read the review list.', ['gpu-setup model-review failed: Backend unavailable']],
        [{responses: {'model-review': {...review, workload: 'other'}}}, 'Setup could not read the review list.', ['gpu-setup model-review failed: unsupported review list']],
        [{responses: {'model-review': {version: 1, workload, pending: false, items: []}}}, 'No review is pending for the stored result.', []],
    ]) {
        const ui = await openSettings({...findings, reviewPending: true}, options);
        await ui.click('Review chat template'); await settle();
        assert.ok(shown(ui, text), text);
        assert.equal(ui.visible(ui.by('Approve template')), false);
        assert.deepEqual(ui.logs, logs);
    }
});

test('closing the review stops its read and a newer read wins', async () => {
    const ui = await openSettings({...findings, reviewPending: true}, {deferAction: 'model-review', responses: {'model-review': review}});
    await ui.click('Review chat template');
    const first = ui.calls.at(-1);
    assert.ok(shown(ui, 'Reading the review list…'));
    await ui.click('Review chat template'); await settle();
    assert.equal(first.killed, true, 'closing stops the read');
    assert.deepEqual(ui.logs, []);
    const reading = ui.click('Review chat template');
    ui.finish(); ui.finish(); await reading; await settle();
    assert.ok(shown(ui, 'Line 1: "Answer in English."'));
    const closing = await openSettings({...findings, reviewPending: true}, {deferAction: 'model-review', responses: {'model-review': review}});
    await closing.click('Review chat template');
    const read = closing.calls.at(-1);
    closing.widgets.find(widget => widget.widgetType === 'ApplicationWindow').close(); await settle();
    assert.equal(read.killed, true, 'closing setup stops the read');
});

test('closing setup stops a running check', async () => {
    const ui = await openSettings({}, {deferAction: 'model-check'});
    const running = ui.click('Run checks');
    const call = ui.calls.at(-1);
    ui.widgets.find(widget => widget.widgetType === 'ApplicationWindow').close();
    await running;
    assert.equal(call.killed, true);
    assert.notEqual(ui.by('Run checks').focused, true, 'a closed setup takes no focus');
});

test('the review summary explains a blocked workload and links to its Model scan card', async () => {
    const request = {profile: {statePath: '/state.db', gpuIndex: 2}, catalog: {version: 1, profiles: []}, expectedRevision: 7};
    const endpoint = 'http://127.0.0.1:9000';
    const prepared = {id: 'vllm-existing', label: 'vllm', adapter: 'systemd', unit: 'vllm.service', cgroup: '/trusted/service', healthURL: `${endpoint}/health`, bootPolicy: 'stop-to-idle',
        nativeModel: {runtime: 'vllm', instance: 'local', model: 'existing', endpoint, launchFile: '/installed/service.conf'}};
    const installation = {app: 'vllm', label: 'vllm', unit: 'vllm.service', endpoint, cgroup: '/trusted/service', location: '/installed/service.conf', recognized: true,
        configurationStatus: 'ready', instanceStatus: 'not-running', models: [{id: 'existing'}],
        binding: {unit: 'vllm.service', cgroup: '/trusted/service', healthURL: `${endpoint}/health`, model: 'existing', launchFile: '/installed/service.conf'}};
    const entry = {...findings, workload: 'vllm-existing', identity: identity('model:existing'), checkAvailable: false};
    for (const [models, blocked] of [[[entry], true], [[{...entry, launchBlocked: false}], false], [[], false]]) {
        const ui = await launch({deferAction: 'unused', responses: {discover: {request: {...request, catalog: {version: 1, profiles: [prepared]}}, units: [], applications: [installation]},
            prepare: {profile: prepared}, fingerprint: {sha256: 'fresh'}, 'model-status': {version: 1, models}}});
        await ui.by('Edit application').emit('clicked');
        ui.by('Settings for vLLM').emit('clicked');
        await ui.by('Use installation').emit('clicked');
        const note = 'This workload cannot start: its stored check result blocks the launch. See the cause on its Model scan card.';
        assert.equal(Boolean(shown(ui, note)), blocked, JSON.stringify(models));
        if (!blocked) continue;
        const link = ui.by('Open Model scan of vllm');
        await ui.click('Open Model scan of vllm');
        assert.ok(ui.visible(ui.widgets.filter(widget => widget.label === 'Model scan').at(-1)), 'the Model scan card shows');
        assert.ok(shown(ui, 'Findings detected'));
        assert.ok(shown(ui, 'Setup can check only a model folder of a managed launch. This workload names only a model ID.'));
        await ui.click('Back');
        assert.equal(link.focused, true, 'Back returns focus to the link');
    }
});

test('a launch that pins another template reads as blocked in the summary', async () => {
    const pending = {...accepted, nativeModel: {...accepted.nativeModel, chatTemplateSHA256: 'new-pin'}};
    const ui = await launch({pending: true, profiles: [pending], responses: {'model-status': {version: 1, models: [{...stored, templateSHA256: 'old-pin'}]}}});
    const reviewing = (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
    ui.finish(); await reviewing;
    assert.ok(shown(ui, 'The chat template of the launch is not the checked template.'));
    assert.ok(shown(ui, 'This workload cannot start with its stored check result. After you finish setup, run the checks again on its Model scan card.'));
});

test('an approval for another workload or a failed status read keeps the card', async () => {
    const other = await openSettings({...findings, reviewPending: true}, {responses: {'model-review': review, 'model-approve': {...stored, workload: 'other'}}});
    await other.click('Review chat template'); await settle();
    await other.click('Approve template'); await settle();
    assert.deepEqual(other.logs, ['gpu-setup model-approve failed: The result is not for this workload.']);
    assert.ok(shown(other, 'Findings detected'));
    let reads = 0;
    const ui = await openSettings({...findings, reviewPending: true}, {fail: 'model-approve', responses: {'model-review': review,
        'model-status': () => { if (++reads > 1) throw new Error('status read failed'); return {version: 1, models: [{...stored, ...findings, reviewPending: true}]}; }}});
    await ui.click('Review chat template'); await settle();
    await ui.click('Approve template'); await settle();
    assert.equal(ui.logs.length, 2);
    assert.match(ui.logs[1], /^gpu-setup model-status failed: /);
    assert.ok(shown(ui, 'Findings detected'), 'the card keeps the status it shows');
    assert.ok(shown(ui, 'The approval was not stored.'));
});

test('closing setup stops the status read after a failed approval', async () => {
    const ui = await openSettings({...findings, reviewPending: true}, {fail: 'model-approve', deferAction: 'model-status', deferOccurrence: 2, responses: {'model-review': review}});
    await ui.click('Review chat template'); await settle();
    const approving = ui.click('Approve template'); await settle();
    const read = ui.calls.at(-1);
    assert.equal(read.argv[1], 'model-status');
    ui.widgets.find(widget => widget.widgetType === 'ApplicationWindow').close();
    await approving; await settle();
    assert.equal(read.killed, true);
    assert.equal(ui.calls.filter(call => call.argv[1] === 'model-review').length, 1, 'a closed setup reads nothing more');
});

test('a folder name that is not a repository gives no source', async () => {
    const profile = {...accepted, nativeModel: {...accepted.nativeModel, owned: {port: 8000, modelPath: `/hf/models--.bad--chat/snapshots/${revision}`}}};
    const ui = await openSettings({}, {profiles: [profile], responses: {'model-status': {version: 1, models: [{...stored, identity: identity(`path:/hf/models--.bad--chat/snapshots/${revision}`)}]}}});
    assert.equal(repo(ui).text, '');
});

test('a run that stops after its write shows the stored result', async () => {
    for (const fail of [false, true]) {
        let reads = 0;
        const ui = await openSettings({}, {deferAction: fail ? 'unused' : 'model-check', fail: fail ? 'model-check' : null,
            responses: {'model-status': () => ({version: 1, models: [++reads > 1 ? findings : stored]})}});
        const running = ui.click('Run checks');
        if (!fail) await ui.click('Cancel check');
        await running; await settle();
        assert.ok(shown(ui, 'The action stopped after setup stored a new result. The card shows the stored result.'), `fail=${fail}`);
        assert.ok(shown(ui, 'Findings detected'), 'the card shows the stored result');
    }
});

test('the stored source fills the fields only when the result applies', async () => {
    const ui = await openSettings({reason: 'other-directory', staticChecksPassed: false, source: {repo: 'old-org/old', revision: 'd'.repeat(40)}});
    assert.equal(repo(ui).text, 'example-org/chat', 'the cache folder wins over a source of another folder');
    assert.equal(rev(ui).text, revision);
});

test('a blocking result shows its cause on the card and in the scan details', async () => {
    const token = {...stored, status: 'error', staticChecksPassed: false, launchBlocked: true,
        checks: [checks[0], {name: 'upstream-identity', outcome: 'error', reason: 'hub-token-unusable'}, {name: 'chat-template', outcome: 'not_run'}, checks[3]]};
    const ui = await openSettings(token);
    assert.ok(shown(ui, 'Setup cannot use your Hugging Face login token. Check HF_TOKEN or the token file.'));
    assert.match(ui.widgets.find(widget => widget.label?.startsWith('Checker revision')).label,
        /\nUpstream identity: failed to run\. Setup cannot use your Hugging Face login token\. Check HF_TOKEN or the token file\.\n/);
    const links = await openSettings({...token, checks: [{name: 'file-allowlist', outcome: 'failed', reason: 'link-or-nonregular-file'}, ...checks.slice(1)], status: 'findings'});
    assert.ok(shown(links, 'The model folder has a link or a special file. A Hugging Face cache folder holds links: download a copy with hf download --local-dir and select that folder.'));
    const other = await openSettings({...token, checks: [{name: 'file-allowlist', outcome: 'failed', reason: 'new-reason'}, ...checks.slice(1)], status: 'findings'});
    assert.ok(shown(other, 'Reason code: new-reason'));
});

test('an open review list is read again after a new run', async () => {
    let reads = 0;
    const ui = await openSettings({...findings, reviewPending: true}, {responses: {'model-review': () => ({...review, items: [{line: 1, text: `Read ${++reads}`}]}),
        'model-check': {...findings, reviewPending: true, checkedAt: '2026-11-05T00:00:00Z'}}});
    await ui.click('Review chat template'); await settle();
    await ui.click('Run checks'); await settle();
    assert.ok(shown(ui, 'Line 1: "Read 2"'));
});

test('the summary note names the action that ends the block', async () => {
    const pinned = {...accepted, nativeModel: {...accepted.nativeModel, chatTemplateSHA256: 'pin'}};
    for (const [entry, profile, text] of [
        [{...findings, reviewPending: true, templateSHA256: 'pin'}, pinned, 'This workload cannot start until you approve its chat template. Review it on its Model scan card.'],
        [{...findings, reason: 'other-directory', templateSHA256: 'pin'}, pinned, 'This workload cannot start with its stored check result. Run the checks again on its Model scan card.'],
        [{...findings, reviewPending: true, templateSHA256: 'pin'}, {...pinned, nativeModel: {...pinned.nativeModel, chatTemplateSHA256: 'new-pin'}},
            'This workload cannot start with its stored check result. After you finish setup, run the checks again on its Model scan card.'],
        [{...findings, status: 'not_scanned', reason: 'template-changed', templateSHA256: 'pin'}, pinned,
            'This workload cannot start: its launch pins another chat template than the checked template. Run setup again for this workload, then run the checks.'],
        [{...stored, templateSHA256: 'pin'}, {...pinned, nativeModel: {...pinned.nativeModel, owned: {port: 8000, modelPath: '/models/new'}}},
            'This workload cannot start with its stored check result. After you finish setup, run the checks again on its Model scan card.'],
    ]) {
        const ui = await launch({pending: true, profiles: [profile], responses: {'model-status': {version: 1, models: [entry]}}});
        const reviewing = (ui.by('Continue') ?? ui.by('Use installation')).emit('clicked');
        ui.finish(); await reviewing;
        assert.ok(shown(ui, text), text);
    }
    const unchecked = await launch({pending: true, profiles: [{...pinned, nativeModel: {...pinned.nativeModel, owned: {port: 8000, modelPath: '/models/new'}}}],
        responses: {'model-status': {version: 1, models: [{...stored, checkedAt: undefined, checks: [], templateSHA256: 'pin'}]}}});
    const reviewing = (unchecked.by('Continue') ?? unchecked.by('Use installation')).emit('clicked');
    unchecked.finish(); await reviewing;
    assert.ok(!unchecked.widgets.some(widget => widget.label?.startsWith('This workload cannot start') && unchecked.visible(widget)), 'no stored result, no block');
});

test('a canceled approval says so and reads what is stored', async () => {
    const ui = await openSettings({...findings, reviewPending: true}, {deferAction: 'model-approve', responses: {'model-review': review}});
    await ui.click('Review chat template'); await settle();
    const approving = ui.click('Approve template');
    await ui.click('Cancel approval'); await approving; await settle();
    assert.ok(shown(ui, 'Approval canceled. The last result is kept.'));
    assert.equal(ui.calls.at(-1).argv[1], 'model-review', 'the open list is read again');
    assert.ok(ui.calls.some(call => call.argv[1] === 'model-status' && call !== ui.calls[0]), 'the status is read again');
});

test('a failed action names its cause', async () => {
    for (const [action, message, text] of [
        ['model-check', 'setup was busy with another change until the time limit, so nothing was stored; try again', 'The checks did not store a result. The last result is kept. Setup was busy with another change. Try again.'],
        ['model-check', 'deployment is in maintenance; resume setup', 'The checks did not store a result. The last result is kept. Setup is in maintenance. Finish setup, then try again.'],
        ['model-check', 'model check stopped at its time limit', 'The checks did not store a result. The last result is kept. The action stopped at its time limit.'],
        ['model-check', 'the stored result does not apply to the accepted workload; run the checks again', 'The checks did not store a result. The last result is kept. The workload changed during the action. Run the checks again.'],
        ['model-check', 'model checks need an activated setup; finish setup first', 'The checks did not store a result. The last result is kept. Finish setup first.'],
        ['model-check', 'installed release is not activated; run setup', 'The checks did not store a result. The last result is kept. The installed release is not activated. Finish setup, then try again.'],
        ['model-approve', 'setup was busy with another change until the time limit, so nothing was stored; try again', 'The approval was not stored. Setup was busy with another change. Try again.'],
        ['model-approve', 'the approval does not match the chat template review of this result', 'The approval was not stored. The review list changed. Read it again.'],
        ['model-approve', 'the stored model check result changed during the approval; read the result again before you approve', 'The approval was not stored. The stored result changed. Read the review list again.'],
    ]) {
        const ui = await openSettings({...findings, reviewPending: true}, {fail: action, stderr: {[action]: message}, responses: {'model-review': review}});
        if (action === 'model-approve') {
            await ui.click('Review chat template'); await settle();
            await ui.click('Approve template');
        } else await ui.click('Run checks');
        await settle();
        assert.ok(shown(ui, text), text);
    }
});

test('Run checks waits while the card reads the stored status', async () => {
    const ui = await openSettings({}, {fail: 'model-check', deferAction: 'model-status', deferOccurrence: 2});
    const running = ui.click('Run checks'); await settle();
    assert.equal(ui.visible(ui.by('Run checks')), false, 'no second run during the read');
    ui.finish(); await running; await settle();
    assert.equal(ui.by('Run checks').sensitive, true);
    assert.equal(ui.by('Run checks').focused, true, 'focus returns after the read');
});

test('the reason texts cover each reason code of the checks', async () => {
    const {readFileSync, readdirSync} = await import('node:fs');
    const dir = new URL('../../internal/modelcheck/', import.meta.url);
    const go = readdirSync(dir).filter(name => name.endsWith('.go') && !name.endsWith('_test.go'))
        .map(name => readFileSync(new URL(name, dir), 'utf8')).join('\n');
    const codes = [...go.matchAll(/\bReason\w+\s*=\s*"([a-z0-9-]+)"/g)].map(match => match[1]);
    const display = new Set(['no-result', 'other-directory', 'checker-changed', 'result-unusable', 'template-changed', 'files-unavailable']);
    const {reasonText} = await import('./model-reasons.mjs');
    assert.ok(codes.length > 30);
    for (const code of codes.filter(code => !display.has(code))) assert.ok(!reasonText(code).startsWith('Reason code:'), code);
});

test('a run without a stored result says that nothing was stored', async () => {
    const empty = {...stored, checkedAt: undefined, checks: [], staticChecksPassed: false, reason: 'no-result'};
    const hub = {...stored, status: 'error', staticChecksPassed: false, notStored: true,
        checks: [checks[0], {name: 'upstream-identity', outcome: 'error', reason: 'upstream-unreachable'}, {name: 'chat-template', outcome: 'not_run'}, checks[3]]};
    const ui = await openSettings(empty, {responses: {'model-check': hub}});
    await ui.click('Run checks');
    assert.ok(shown(ui, 'This run failed. Setup could not reach Hugging Face. Setup stored nothing.'));
    assert.equal(ui.widgets.some(widget => widget.label?.includes('last stored result is kept') && ui.visible(widget)), false);
    const failed = await openSettings(empty, {fail: 'model-check'});
    await failed.click('Run checks'); await settle();
    assert.ok(shown(failed, 'The checks did not store a result. Setup stored nothing.'));
    const canceled = await openSettings(empty, {deferAction: 'model-check'});
    const running = canceled.click('Run checks');
    await canceled.click('Cancel check'); await running; await settle();
    assert.ok(shown(canceled, 'Check canceled. Setup stored nothing.'));
});

test('a status read after a failed run changes only its own card', async () => {
    const {mergeReport} = await import('./model-report.mjs');
    const rechecked = {...stored, workload: 'other', rechecked: true, reason: 'files-changed'};
    const status = {state: 'loaded', entries: new Map([[workload, stored], ['other', rechecked]])};
    const merged = mergeReport(status, [{...findings}, {...stored, workload: 'other'}], workload);
    assert.equal(merged.entries.get('other'), rechecked, 'the recheck result of card A stays');
    assert.equal(merged.entries.get(workload).status, 'findings', 'card B takes its new entry');
    assert.equal(mergeReport(merged, [], workload).entries.has(workload), false, 'an entry that goes away goes');
    const loaded = mergeReport({state: 'unavailable', entries: new Map()}, [stored, rechecked], workload);
    assert.equal(loaded.entries.size, 2, 'a status that was not loaded takes the whole report');
    let reads = 0;
    const ui = await openSettings({}, {fail: 'model-check', responses: {'model-status': () => ({version: 1, models: [++reads > 1 ? findings : stored]})}});
    await ui.click('Run checks'); await settle();
    assert.equal(reads, 2, 'the card reads the status again');
    assert.ok(shown(ui, 'Findings detected'));
});

test('the Cancel button goes while the card reads the stored status', async () => {
    const ui = await openSettings({}, {fail: 'model-check', deferAction: 'model-status', deferOccurrence: 2});
    const running = ui.click('Run checks'); await settle();
    assert.equal(ui.visible(ui.by('Cancel check')), false, 'Cancel does nothing during the read, so it is hidden');
    ui.finish(); await running; await settle();
    assert.equal(ui.by('Run checks').focused, true);
});
