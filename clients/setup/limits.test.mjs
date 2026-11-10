import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {loadGjsModule} from '../gnome/tests/gjs-modules.js';

const stub = {default: {}};
const natives = Object.fromEntries(['gi://Gtk?version=4.0', 'gi://Gio', 'gi://Pango', 'gi://GObject', 'gi://Gdk?version=4.0'].map(name => [name, stub]));
const limitsUI = await loadGjsModule('../../setup/limits-ui.mjs', natives);
const host = await loadGjsModule('../../setup/host-protection.mjs', natives);

const owned = {id: 'vllm-qwen', label: 'Qwen', adapter: 'systemd', bootPolicy: 'stop-to-idle',
    unit: 'gws-owned-vllm-qwen.service', cgroup: '/user/app.slice/gws-owned-vllm-qwen.service', healthURL: 'http://127.0.0.1:8000/health',
    nativeModel: {runtime: 'vllm', instance: 'local', model: 'qwen', endpoint: 'http://127.0.0.1:8000',
        launchFile: '/home/user/.config/systemd/user/gws-owned-vllm-qwen.service', launchSHA256: 'digest', owned: {port: 8000, modelPath: '/models/qwen'}}};
const adopted = {id: 'vllm-adopted', label: 'Adopted', adapter: 'systemd', bootPolicy: 'stop-to-idle', unit: 'vllm.service', cgroup: '/user/vllm.service',
    healthURL: 'http://127.0.0.1:8000/health', nativeModel: {runtime: 'vllm', instance: 'vllm.service', model: 'qwen', endpoint: 'http://127.0.0.1:8000', launchFile: '/units/vllm.service', launchSHA256: 'digest'}};
const rendered = ({draft}) => ({profile: {...owned, nativeModel: {...owned.nativeModel, owned: draft.binding.owned}}});
const absent = {status: 'absent', uid: 1000, dropIns: [{path: '/etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf', content: '[Slice]\nMemoryMax=20G\n', install: true},
    {path: '/etc/systemd/system/user@1000.service.d/50-gws-delegate-io.conf', install: false}],
commands: ['sudo install -m 0644 cap.conf /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf', 'sudo systemctl daemon-reload'],
problems: ['50-gws-host-cap.conf is missing'], reserve: {memoryMiB: 2048, tasks: 512, cpuPercent: 100, ioPercent: 10},
usage: {memoryInUseMiB: 4096, pageCacheMiB: 2048, ceilingMiB: 58368}};

function start(profiles = [owned], discovery = {}, responses = {}) {
    return launch({deferAction: 'unused', responses: {
        discover: {request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 2, profiles}}, units: [], ...discovery},
        'render-owned': rendered, ...responses}});
}

async function openResources(ui) {
    await ui.click('Settings for vLLM'); await ui.click('Advanced settings'); await ui.click('Resource checks');
}

async function review(ui) {
    await ui.click('Done'); await ui.click('Back'); await ui.click('Back'); await ui.click('Continue');
    const call = ui.calls.findLast(item => item.argv[1] === 'validate');
    return call ? JSON.parse(call.input) : null;
}

// summary finds the status summary of a refusal, not the inline form error
// of the Resource checks page.
const summary = (ui, text) => ui.widgets.some(widget => widget.cssClasses?.includes('setup-notice') && widget.name !== 'limits-error' && widget.label === text);
const visibleText = ui => ui.widgets.filter(widget => ui.visible(widget) && widget.label).map(widget => widget.label).join('\n');

test('limits of an owned unit reach validate and apply with catalog v5 and the defaults', async () => {
    const ui = await start();
    await openResources(ui);
    assert.equal(ui.by('Tasks').sensitive, false);
    ui.by('Limit the resources of this workload').active = true;
    assert.equal(ui.by('Tasks').text, '4096'); assert.equal(ui.by('CPU (cores)').text, '4');
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8');
    ui.edit(ui.by('Memory maximum (GiB)'), 'text', '10.5');
    const request = await review(ui);
    assert.equal(request.catalog.version, 5);
    assert.deepEqual(request.catalog.profiles[0].nativeModel.owned.limits, {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 10752, tasksMax: 4096, cpuQuotaPercent: 400});
    assert.equal(request.hostCap, undefined); assert.equal(request.profile.limitReserve, undefined);
    await ui.click('Finish setup');
    const applied = JSON.parse(ui.calls.at(-1).input);
    assert.equal(ui.calls.at(-1).argv[1], 'apply');
    assert.equal(applied.catalog.profiles[0].nativeModel.owned.limits.memoryMaxMiB, 10752);
});

test('stored limits stay exact when untouched, and turning them off removes them', async () => {
    const limits = {version: 1, memoryHighMiB: 1000, memoryMaxMiB: 3000, memorySwapMaxMiB: 512, tasksMax: 777, cpuQuotaPercent: 150,
        ioDevice: '/dev/nvme0n1', ioDeviceNumber: '259:0', ioReadBandwidthMax: 1048576 * 5, ioWriteIOPSMax: 200};
    const limited = {...owned, nativeModel: {...owned.nativeModel, owned: {...owned.nativeModel.owned, limits}}};
    let ui = await start([limited]);
    await openResources(ui);
    assert.equal(ui.by('Memory high (GiB)').text, '0.9'); assert.equal(ui.by('CPU (cores)').text, '1.5'); assert.equal(ui.by('Read bandwidth (MiB/s)').text, '5');
    const rendered = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned')?.input ?? 'null');
    assert.equal(rendered, null, 'nothing renders before review');
    ui.edit(ui.by('Write bandwidth (MiB/s)'), 'text', '7');
    let request = await review(ui);
    assert.deepEqual(request.catalog.profiles[0].nativeModel.owned.limits, {...limits, ioWriteBandwidthMax: 7 * 1048576});
    const draft = JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft;
    assert.deepEqual(draft.binding.owned.limits, {...limits, ioWriteBandwidthMax: 7 * 1048576}, 'render-owned renders the final limits');
    ui = await start([limited]);
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = false;
    request = await review(ui);
    assert.equal(request.catalog.profiles[0].nativeModel.owned.limits, undefined);
    assert.equal(request.catalog.version, 2);
});

for (const [name, edit, text] of [
    ['missing memory', ui => ui.edit(ui.by('Memory high (GiB)'), 'text', '8'), 'Enter a value for Memory maximum (GiB).'],
    ['memory order', ui => { ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '8'); }, 'Memory high must be less than the memory maximum.'],
    ['two decimals', ui => ui.edit(ui.by('Memory high (GiB)'), 'text', '8.25'), 'Memory high (GiB): enter a number.'],
    ['range', ui => ui.edit(ui.by('Tasks'), 'text', '10'), 'Tasks must be 64 to 4194304.'],
    ['bandwidth', ui => ui.edit(ui.by('Write bandwidth (MiB/s)'), 'text', 'fast'), 'Write bandwidth (MiB/s): enter a number.'],
    ['device format', ui => ui.edit(ui.by('I/O device (for example /dev/nvme0n1)'), 'text', 'sda'), 'I/O device (for example /dev/nvme0n1): the value has the wrong format.'],
    ['I/O without device', ui => { ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9'); ui.edit(ui.by('Read operations per second'), 'text', '500'); },
        'I/O limits need the I/O device and its number, and the device needs an I/O limit.'],
    ['cleared default', ui => { ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9'); ui.edit(ui.by('CPU (cores)'), 'text', ''); }, 'Enter a value for CPU (cores).'],
]) {
    test(`a plain limit error stops the review: ${name}`, async () => {
        const ui = await start();
        await openResources(ui);
        ui.by('Limit the resources of this workload').active = true;
        edit(ui);
        assert.equal(await review(ui), null);
        assert.ok(!ui.calls.some(call => ['render-owned', 'prepare'].includes(call.argv[1])), 'refused before any render');
        assert.ok(visibleText(ui).includes(text), visibleText(ui));
    });
}

test('an adopted unit gets the convert-first refusal and no review', async () => {
    const ui = await start([adopted], {}, {prepare: {profile: adopted}, 'verify-bindings': {}});
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    assert.equal(await review(ui), null);
    assert.ok(!ui.calls.some(call => ['render-owned', 'prepare'].includes(call.argv[1])), 'refused before any render');
    assert.ok(visibleText(ui).includes('Limits need a launch that Supervisor manages. Clear Limit the resources of this workload to adopt this service, or use a managed launch.'));
});

test('Cancel restores the limits form of the session', async () => {
    const ui = await start();
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8');
    await ui.click('Cancel');
    await openResources(ui);
    assert.equal(ui.by('Limit the resources of this workload').active, false);
    assert.equal(ui.by('Memory high (GiB)').text, '');
});

test('the host protection page shows the status, usage, root step and escape routes', async () => {
    const routes = [{route: 'cron', item: 'a crontab exists'}, {route: 'polkit', item: 'cannot check /etc/polkit-1/rules.d', unchecked: true},
        {route: 'incomplete', item: 'cannot check a source', unchecked: true}, {route: 'more', item: '3 more entries', count: 3}];
    const ui = await start([owned], {hostCap: absent, escapeRoutes: routes});
    await ui.click('Setup settings');
    await ui.click('Host protection');
    const text = visibleText(ui);
    for (const expected of ['This account has no host protection. An administrator must run one step as root.', '50-gws-host-cap.conf is missing',
        'Memory ceiling: 57 GiB. In use now: 4 GiB, without 2 GiB of page cache that the kernel can free.',
        'Programs from this account can run outside host protection through cron: a crontab exists.',
        'The scan cannot check /etc/polkit-1/rules.d. A route can be open', 'The scan cannot check a source.', 'Not shown: 3 more entries.']) assert.ok(text.includes(expected), expected);
    await ui.click('Copy root step');
    const copied = ui.widgets.find(widget => widget.title === 'GPU Workload Setup').clipboard;
    assert.ok(copied.startsWith('# /etc/systemd/system/user-1000.slice.d/50-gws-host-cap.conf\n[Slice]\nMemoryMax=20G'));
    assert.ok(copied.endsWith('sudo systemctl daemon-reload')); assert.ok(!copied.includes('delegate-io'));
    await ui.click('Done');
    assert.equal(ui.visible(ui.by('Continue')), true);
    await ui.click('Settings for vLLM'); await ui.click('Advanced settings'); await ui.click('Resource checks');
    assert.ok(visibleText(ui).includes('Host memory ceiling: 57 GiB. In use now: 4 GiB.'));
});

test('host values and a custom reserve go into the request only when they apply', async () => {
    const limits = {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 10240, tasksMax: 4096, cpuQuotaPercent: 400};
    const limited = {...owned, nativeModel: {...owned.nativeModel, owned: {...owned.nativeModel.owned, limits}}};
    const stored = {memoryMiB: 3000, tasks: 600, cpuPercent: 150, ioPercent: 10};
    const ui = await start([limited], {request: {profile: {statePath: '/state.db', gpuIndex: 0, limitReserve: stored}, catalog: {version: 5, profiles: [limited]}},
        hostCap: {...absent, values: {memoryHighMiB: 20480, memoryMaxMiB: 24576}}}, {validate: {changes: ['Reviewed change'], hostCap: {status: 'present', reserve: stored}}});
    await ui.click('Setup settings'); await ui.click('Host protection');
    assert.equal(ui.by('Host memory high (GiB)').text, '20'); assert.equal(ui.by('Memory for the supervisor (GiB)').text, '2.9');
    await ui.click('Back');
    await ui.click('Continue');
    let request = JSON.parse(ui.calls.findLast(call => call.argv[1] === 'validate').input);
    assert.deepEqual(request.hostCap, {memoryHighMiB: 20480, memoryMaxMiB: 24576});
    assert.deepEqual(request.profile.limitReserve, stored, 'an untouched stored reserve stays exact');
    await ui.click('Back');
    await ui.click('Setup settings'); await ui.click('Host protection');
    assert.ok(visibleText(ui).includes('Host protection is active for this account.'), 'the preview step replaces the discovery step');
    for (const [title, value] of [['Memory for the supervisor (GiB)', '2'], ['Tasks for the supervisor', '512'], ['CPU for the supervisor (cores)', '1']]) ui.edit(ui.by(title), 'text', value);
    ui.edit(ui.by('Host memory high (GiB)'), 'text', '22');
    await ui.click('Done'); await ui.click('Continue');
    request = JSON.parse(ui.calls.findLast(call => call.argv[1] === 'validate').input);
    assert.equal(request.profile.limitReserve, undefined, 'the default reserve stores nothing');
    assert.deepEqual(request.hostCap, {memoryHighMiB: 22528, memoryMaxMiB: 24576});
});

test('host values need a limited unit, and host value errors stop the review', async () => {
    let ui = await start([owned]);
    await ui.click('Setup settings'); await ui.click('Host protection');
    ui.edit(ui.by('Host memory high (GiB)'), 'text', '20'); ui.edit(ui.by('Host memory maximum (GiB)'), 'text', '24');
    await ui.click('Done'); await ui.click('Continue');
    assert.equal(JSON.parse(ui.calls.findLast(call => call.argv[1] === 'validate').input).hostCap, undefined);
    for (const [values, text] of [[['20', ''], 'Enter both host memory values in GiB, with at most one decimal.'], [['24', '20'], 'Host memory high must be less than host memory maximum.']]) {
        ui = await start([owned]);
        await ui.click('Setup settings'); await ui.click('Host protection');
        ui.edit(ui.by('Host memory high (GiB)'), 'text', values[0]); ui.edit(ui.by('Host memory maximum (GiB)'), 'text', values[1]);
        await ui.click('Done'); await ui.click('Continue');
        assert.ok(!ui.calls.some(call => call.argv[1] === 'validate'));
        assert.ok(visibleText(ui).includes(text), text);
    }
    ui = await start([owned]);
    await ui.click('Setup settings'); await ui.click('Host protection');
    ui.edit(ui.by('Tasks for the supervisor'), 'text', 'many'); ui.edit(ui.by('I/O for the supervisor (percent of each cap)'), 'text', '');
    await ui.click('Done'); await ui.click('Continue');
    assert.ok(visibleText(ui).includes('Tasks for the supervisor: enter a number.'));
});

test('the page states when nothing was checked or no limits are set', async () => {
    let ui = await start([owned]);
    await ui.click('Setup settings'); await ui.click('Host protection');
    assert.ok(visibleText(ui).includes('No workload has limits, so this account needs no host protection.'));
    assert.ok(visibleText(ui).includes('Escape routes were not checked.'));
    assert.equal(ui.visible(ui.by('Copy root step')), false);
    ui = await start([owned], {escapeRoutes: [], hostCap: {status: 'unknown', problems: ['cannot read'], reserve: absent.reserve}});
    await ui.click('Setup settings'); await ui.click('Host protection');
    assert.ok(visibleText(ui).includes('The scan found no escape routes.'));
    assert.ok(visibleText(ui).includes('Host protection cannot be checked.'));
    await ui.click('Back');
    assert.equal(ui.visible(ui.by('Continue')), true);
});

test('limit errors of the backend map to the plain texts of the design', () => {
    const cases = [
        ['setup: resource limits need a supervisor-owned unit: vllm (unit x) is adopted', 'Limits need a launch that Supervisor manages.'],
        ['a required cgroup controller is not enabled for this account: io is absent from /x/cgroup.controllers', 'This computer does not let the account limit io.'],
        ['the system cannot apply a resource limit: /user.slice/memory.high is missing (memory controller is off)', 'The system cannot apply memory.high. memory controller is off on this computer.'],
        ['the host protection of this account is missing or different: x', 'See Host protection in Setup settings.'],
        ['hostCap needs a unit with resource limits; remove hostCap', 'Host protection values need a workload with limits.'],
        ['a resource limit is unset or leaves too little room for the supervisor: use 4096 MiB or less', 'This limit leaves too little room for the supervisor.'],
        ['slice memory does not fit this limit: 10 MiB is in use', 'Slice memory does not fit this limit.'],
        ['259:0 (/dev/nvme0n1) does not support I/O weight', 'This I/O device does not support I/O weight.'],
        ['the I/O device of the resource limits is not usable: x', 'The I/O device of the limits is not usable.'],
    ];
    for (const [message, text] of cases) assert.ok(limitsUI.limitErrorText(message).startsWith(text) || limitsUI.limitErrorText(message).includes(text), message);
    assert.equal(limitsUI.limitErrorText('something else'), null);
    assert.equal(limitsUI.limitSummary(new limitsUI.PlainLimitError('plain')), 'plain');
    assert.equal(limitsUI.limitSummary(new Error('other')), null);
});

test('number formats, the limit check and the request helpers', () => {
    assert.equal(limitsUI.parseGiB('1.5'), 1536); assert.equal(limitsUI.parseGiB('0.3'), 307); assert.equal(limitsUI.parseGiB('x'), null);
    assert.equal(limitsUI.formatGiB(1000), '0.9'); assert.equal(limitsUI.parseCores('0.5'), 50); assert.equal(limitsUI.parseCores('2.25'), 225);
    assert.equal(limitsUI.parseCores('1.234'), null); assert.equal(limitsUI.formatCores(250), '2.5'); assert.equal(limitsUI.parseCount('12a'), null);
    assert.equal(limitsUI.checkLimits({memoryHighMiB: 1, memoryMaxMiB: 2, tasksMax: 64, cpuQuotaPercent: 1, ioDevice: '/dev/sda'}),
        'I/O limits need the I/O device and its number, and the device needs an I/O limit.');
    assert.equal(limitsUI.checkLimits({memoryHighMiB: 1, memoryMaxMiB: 2, tasksMax: 64, cpuQuotaPercent: 1}), null);
    const limitedOwned = {...owned, nativeModel: {...owned.nativeModel, owned: {...owned.nativeModel.owned, limits: {version: 1}}}};
    assert.equal(limitsUI.refuseAdoptedLimits(owned, true), owned);
    assert.equal(limitsUI.refuseAdoptedLimits(limitedOwned, false), limitedOwned);
    assert.throws(() => limitsUI.refuseAdoptedLimits(limitedOwned, true), /Convert this service first/);
    const request = {profile: {statePath: '/s', limitReserve: {memoryMiB: 1}}, hostCap: {memoryHighMiB: 1}};
    const plain = value => JSON.parse(JSON.stringify(value));
    assert.deepEqual(plain(host.hostRequest(request, {hostCap: {memoryHighMiB: 2}}, false)), {profile: {statePath: '/s'}});
    assert.deepEqual(plain(host.hostRequest(request, {hostCap: {memoryHighMiB: 2}, limitReserve: {memoryMiB: 3}}, true)), {profile: {statePath: '/s', limitReserve: {memoryMiB: 3}}, hostCap: {memoryHighMiB: 2}});
    assert.throws(() => host.hostRequest(request, {error: 'bad'}, true), /bad/);
    assert.equal(host.rootStepText(null), '');
    assert.equal(host.rootStepText({dropIns: [], commands: ['c']}), 'c');
    assert.equal(host.escapeRouteText({route: 'more', item: '2 more', unchecked: true}), 'Not shown: 2 more.');
});

test('limits stay in the owned draft through launch edits, saved selections and render-owned', async () => {
    const ui = await start();
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    await ui.click('Done'); await ui.click('Launch details');
    ui.edit(ui.by('Launch port'), 'text', '8001');
    await ui.click('Save selections for later');
    const request = JSON.parse(ui.calls.findLast(call => call.argv[1] === 'save-drafts').input);
    assert.equal(request.version, 2, 'drafts with limits use draft version 2');
    const saved = request.drafts[0];
    const want = {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 9216, tasksMax: 4096, cpuQuotaPercent: 400};
    assert.deepEqual(saved.binding.owned.limits, want);
    assert.equal(saved.binding.owned.port, 8001);
    await ui.click('Done'); await ui.click('Back'); await ui.click('Back'); await ui.click('Continue');
    assert.deepEqual(JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft.binding.owned.limits, want);
});

test('a saved draft with limits opens with them', async () => {
    const draft = {id: 'draft-one', app: 'vllm', label: 'Qwen', binding: {instance: 'local', owned: {port: 8000, modelPath: '/models/qwen', limits: {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 9216, tasksMax: 512, cpuQuotaPercent: 100}}}};
    const ui = await start([], {}, {drafts: {drafts: [draft]}});
    await openResources(ui);
    assert.equal(ui.by('Limit the resources of this workload').active, true);
    assert.equal(ui.by('Tasks').text, '512');
});

test('limits turned on and off again leave no limits in render, validate or saved drafts', async () => {
    let ui = await start();
    await openResources(ui);
    const toggle = ui.by('Limit the resources of this workload');
    toggle.active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    toggle.active = false;
    const request = await review(ui);
    assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft.binding.owned.limits, undefined);
    assert.equal(request.catalog.profiles[0].nativeModel.owned.limits, undefined);
    assert.equal(request.catalog.version, 2);
    const limits = {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 9216, tasksMax: 512, cpuQuotaPercent: 100};
    const draft = {id: 'draft-one', app: 'vllm', label: 'Qwen', binding: {instance: 'local', owned: {port: 8000, modelPath: '/models/qwen', limits}}};
    ui = await start([], {}, {drafts: {drafts: [draft]}});
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = false;
    await ui.click('Done'); await ui.click('Launch details'); await ui.click('Save selections for later');
    const saved = JSON.parse(ui.calls.findLast(call => call.argv[1] === 'save-drafts').input);
    assert.equal(saved.version, 1);
    assert.equal(Object.hasOwn(saved.drafts[0].binding.owned, 'limits'), false);
});

test('an invalid limits form refuses the managed launch preview and the save', async () => {
    const draft = {id: 'draft-one', app: 'vllm', label: 'Qwen', binding: {instance: 'local', owned: {port: 8000, modelPath: '/models/qwen'}}};
    let ui = await start([], {}, {drafts: {drafts: [draft]}});
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    ui.edit(ui.by('Memory maximum (GiB)'), 'text', '7');
    await ui.click('Done'); await ui.click('Launch details'); await ui.click('Save selections for later');
    assert.ok(!ui.calls.some(call => call.argv[1] === 'save-drafts'), 'an invalid form is not saved');
    assert.ok(visibleText(ui).includes('Memory high must be less than the memory maximum.'));
    ui = await start([], {}, {drafts: {drafts: [draft]}});
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8');
    await ui.click('Done'); await ui.click('Launch details');
    await ui.by('Preview managed launch and add for review').emit('clicked');
    assert.ok(!ui.calls.some(call => ['render-owned', 'verify-bindings'].includes(call.argv[1])), 'the preview refuses an invalid form');
    assert.ok(summary(ui, 'Enter a value for Memory maximum (GiB).'), 'the plain form text is reported');
    ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    await ui.by('Preview managed launch and add for review').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft.binding.owned.limits,
        {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 9216, tasksMax: 4096, cpuQuotaPercent: 400});
});

test('limits on the converted launch of an adopted unit are refused after the render', async () => {
    const limits = {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 9216, tasksMax: 512, cpuQuotaPercent: 100};
    const draft = {id: adopted.id, app: 'vllm', label: 'Adopted', binding: {instance: 'local', owned: {port: 8000, modelPath: '/models/qwen', limits}}};
    const ui = await start([adopted], {}, {drafts: {drafts: [draft]}});
    await ui.click('Continue');
    assert.ok(ui.calls.some(call => call.argv[1] === 'render-owned'), 'the refusal comes after the render');
    assert.ok(!ui.calls.some(call => call.argv[1] === 'validate'));
    assert.ok(visibleText(ui).includes('Limits need a launch that Supervisor manages. Convert this service first and finish setup.'));
});

test('Use installation refuses limits on a service that Supervisor does not manage', async () => {
    const candidate = {app: 'comfyui', label: 'ComfyUI', unit: 'comfyui.service', endpoint: 'http://127.0.0.1:8188', location: '/services/comfyui.service', recognized: true,
        instanceStatus: 'not-running', configurationStatus: 'ready', binding: {unit: 'comfyui.service', cgroup: '/comfyui', healthURL: 'http://127.0.0.1:8188/system_stats'}};
    const ui = await start([], {applications: [candidate]});
    await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Resource checks');
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    await ui.click('Done'); await ui.click('Back');
    await ui.click('Use installation');
    assert.ok(!ui.calls.some(call => ['prepare', 'render-owned', 'verify-bindings'].includes(call.argv[1])));
    assert.ok(visibleText(ui).includes('Limits need a launch that Supervisor manages. Clear Limit the resources of this workload to adopt this service, or use a managed launch.'));
});

test('a limits edit while a managed launch preview is pending discards the stale render', async () => {
    const draft = {id: 'draft-one', app: 'vllm', label: 'Qwen', binding: {instance: 'local', owned: {port: 8000, modelPath: '/models/qwen'}}};
    const ui = await launch({deferAction: 'render-owned', responses: {
        discover: {request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 2, profiles: []}}, units: []},
        drafts: {drafts: [draft]}, 'render-owned': rendered}});
    await openResources(ui);
    const toggle = ui.by('Limit the resources of this workload');
    toggle.active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    const preview = ui.by('Preview managed launch and add for review').emit('clicked');
    toggle.active = false;
    ui.finish(); await preview;
    assert.ok(!ui.calls.some(call => call.argv[1] === 'verify-bindings'), 'the stale render is discarded');
    assert.ok(ui.widgets.some(widget => widget.label?.includes('Draft changed during preview')));
});

test('Verify binding refuses enabled limits on a service that Supervisor does not manage', async () => {
    const ui = await launch({deferAction: 'unused'});
    ui.selectApplication(0);
    await ui.click('Advanced settings'); await ui.click('Resource checks');
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    await ui.by('Verify binding and add for review').emit('clicked');
    assert.ok(!ui.calls.some(call => ['fingerprint', 'verify-bindings'].includes(call.argv[1])), 'enabled limits stop the binding path');
    assert.ok(summary(ui, 'Limits need a launch that Supervisor manages. Clear Limit the resources of this workload to adopt this service, or use a managed launch.'));
    ui.by('Limit the resources of this workload').active = false;
    await ui.by('Verify binding and add for review').emit('clicked');
    assert.ok(ui.calls.some(call => call.argv[1] === 'verify-bindings'), 'without limits the binding path runs');
});

const ownedDraft = {id: 'draft-one', app: 'vllm', label: 'Qwen', binding: {instance: 'local', owned: {port: 8000, modelPath: '/models/qwen'}}};
function deferred(action, deferOccurrence = null) {
    return launch({deferAction: action, deferOccurrence, responses: {
        discover: {request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 2, profiles: []}}, units: []},
        drafts: {drafts: [ownedDraft]}, 'render-owned': rendered}});
}

test('a pending render survives a limits event that changes nothing', async () => {
    const ui = await deferred('render-owned');
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    const preview = ui.by('Preview managed launch and add for review').emit('clicked');
    ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9.0');
    ui.finish(); await preview;
    assert.ok(ui.calls.some(call => call.argv[1] === 'verify-bindings'), 'an unchanged value keeps the render');
});

test('Use installation discards a render that a limits edit made stale, refuses an invalid form and renders valid limits', async () => {
    // Only the first render waits, so a wrong second render ends at once.
    let ui = await deferred('render-owned', 1);
    await openResources(ui);
    const toggle = ui.by('Limit the resources of this workload');
    toggle.active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    const finish = ui.by('Use installation').emit('clicked');
    ui.edit(ui.by('Memory maximum (GiB)'), 'text', '7');
    ui.finish(); await finish;
    assert.ok(!ui.calls.some(call => call.argv[1] === 'verify-bindings'), 'the stale render is discarded');
    await ui.by('Use installation').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'render-owned').length, 1, 'an invalid form makes no render');
    ui = await deferred('unused');
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    await ui.by('Use installation').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.find(call => call.argv[1] === 'render-owned').input).draft.binding.owned.limits,
        {version: 1, memoryHighMiB: 8192, memoryMaxMiB: 9216, tasksMax: 4096, cpuQuotaPercent: 400});
});

test('Save selections checks the limits of every open draft', async () => {
    const comfy = {id: 'draft-two', app: 'comfyui', label: 'ComfyUI'};
    const ui = await start([], {}, {drafts: {drafts: [ownedDraft, comfy]}});
    await ui.click('Settings for ComfyUI'); await ui.click('Advanced settings'); await ui.click('Resource checks');
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    await ui.click('Done'); await ui.click('Back'); await ui.click('Back');
    await ui.click('Settings for vLLM'); await ui.click('Advanced settings'); await ui.click('Launch details');
    await ui.click('Save selections for later');
    assert.ok(!ui.calls.some(call => call.argv[1] === 'save-drafts'), 'the second draft blocks the save');
    const status = ui.widgets.find(widget => widget.label === 'Checking your desktop and available services…' || widget.cssClasses?.includes('setup-notice'));
    assert.ok(status.label === 'ComfyUI: Limits need a launch that Supervisor manages. Clear Limit the resources of this workload to adopt this service, or use a managed launch.', status.label);
});

test('Verify binding refuses enabled limits on an owned draft and an invalid form', async () => {
    let ui = await deferred('unused');
    await openResources(ui);
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '9');
    await ui.by('Verify binding and add for review').emit('clicked');
    assert.ok(!ui.calls.some(call => ['fingerprint', 'verify-bindings'].includes(call.argv[1])), 'an owned draft does not let the binding drop the limits');
    assert.ok(summary(ui, 'Limits need a launch that Supervisor manages. Clear Limit the resources of this workload to adopt this service, or use a managed launch.'));
    ui = await launch({deferAction: 'unused'});
    ui.selectApplication(0);
    await ui.click('Advanced settings'); await ui.click('Resource checks');
    ui.by('Limit the resources of this workload').active = true;
    ui.edit(ui.by('Memory high (GiB)'), 'text', '8'); ui.edit(ui.by('Memory maximum (GiB)'), 'text', '7');
    await ui.by('Verify binding and add for review').emit('clicked');
    assert.ok(!ui.calls.some(call => ['fingerprint', 'verify-bindings'].includes(call.argv[1])), 'an invalid form stops the binding path');
    assert.ok(summary(ui, 'Memory high must be less than the memory maximum.'));
});
