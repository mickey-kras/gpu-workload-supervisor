import test from 'node:test';
import assert from 'node:assert/strict';
import {launch} from './harness.mjs';
import {loadGjsModule} from '../gnome/tests/gjs-modules.js';

const stub = {default: {}};
const natives = Object.fromEntries(['gi://Gtk?version=4.0', 'gi://Gio', 'gi://Pango'].map(name => [name, stub]));
const servingUI = await loadGjsModule('../../setup/serving-ui.mjs', natives);

const owned = {id: 'vllm-qwen', label: 'Qwen', adapter: 'systemd', bootPolicy: 'stop-to-idle',
    unit: 'gws-owned-vllm-qwen.service', cgroup: '/user/app.slice/gws-owned-vllm-qwen.service', healthURL: 'http://127.0.0.1:8000/health',
    nativeModel: {runtime: 'vllm', instance: 'local', model: 'qwen', endpoint: 'http://127.0.0.1:8000',
        launchFile: '/home/user/.config/systemd/user/gws-owned-vllm-qwen.service', launchSHA256: 'digest', owned: {port: 8000, modelPath: '/models/qwen'}}};
const rendered = ({draft}) => ({profile: {...owned, nativeModel: {...owned.nativeModel, owned: draft.binding.owned}}});

function start(profiles = [owned], responses = {}) {
    return launch({deferAction: 'unused', responses: {
        discover: {request: {profile: {statePath: '/state.db', gpuIndex: 0}, catalog: {version: 2, profiles}}, units: []},
        'render-owned': rendered, ...responses}});
}

async function openLaunchDetails(ui) {
    await ui.click('Settings for vLLM'); await ui.click('Advanced settings'); await ui.click('Launch details');
}

async function review(ui) {
    await ui.click('Done'); await ui.click('Back'); await ui.click('Back'); await ui.click('Continue');
    const call = ui.calls.findLast(item => item.argv[1] === 'validate');
    return call ? JSON.parse(call.input) : null;
}

const visibleText = ui => ui.widgets.filter(widget => ui.visible(widget) && widget.label).map(widget => widget.label).join('\n');

const unitServing = await loadGjsModule('../../setup/serving-ui.mjs', Object.fromEntries([
    ['gi://Gtk?version=4.0', {default: {}}], ['gi://Gio', {default: {}}], ['gi://Pango', {default: {WrapMode: {WORD_CHAR: 2}}}],
]));

function formFixture(initial) {
    class Widget {
        constructor(properties = {}) { Object.assign(this, properties); this.cssClasses = []; }
        connect() { return 1; }
        add_css_class(name) { this.cssClasses.push(name); }
        add(child) { (this.children ??= []).push(child); }
        append(child) { this.add(child); }
        set_child(child) { this.child = child; }
        update_property() {}
    }
    const Adw = {PreferencesGroup: Widget, EntryRow: Widget, ExpanderRow: Widget};
    const Gtk = {Label: Widget, CheckButton: Widget, AccessibleProperty: {LABEL: 'label'}};
    const form = new unitServing.ServingOptionsForm({Adw, Gtk, page: {append() {}}, initial, changed: () => {}});
    const byTitle = title => (form.header.children ?? []).concat(form.fields.children ?? []).find(widget => widget.title === title);
    return {form, byTitle};
}

test('sync adopts the seeded defaults of a selected managed candidate', () => {
    const {form, byTitle} = formFixture(undefined);
    assert.equal(form.value(true), undefined);
    form.sync({version: 1});
    assert.equal(form.enable.active, true);
    assert.equal(byTitle('GPU memory utilization').text, '0.90');
    assert.equal(JSON.stringify(form.value(true)), JSON.stringify({version: 1}));
    // Clearing the pin removes the seeded options from the candidate value.
    form.enable.active = false;
    assert.equal(form.value(true), null);
    // A candidate without serving resets the form to its stored-draft state.
    form.sync(undefined);
    assert.equal(form.enable.active, false);
    assert.equal(form.value(true), undefined);
});

test('capture and restore keep the stored options across a mid-session sync', () => {
    const {form, byTitle} = formFixture({version: 1, gpuMemoryPercent: 85, maxNumSeqs: 16});
    const snapshot = form.capture();
    // Selecting another managed installation adopts its seeded defaults.
    form.sync({version: 1});
    assert.equal(JSON.stringify(form.value(true)), JSON.stringify({version: 1}));
    // Cancel restores the stored draft state, not only the visible rows.
    form.restore(snapshot);
    assert.equal(byTitle('GPU memory utilization').text, '0.85');
    assert.equal(byTitle('Maximum concurrent sequences').text, '16');
    assert.equal(JSON.stringify(form.value(true)), JSON.stringify({version: 1, gpuMemoryPercent: 85, maxNumSeqs: 16}));
});

test('sync adopts stored option values and the diagnosis flag', () => {
    const {form, byTitle} = formFixture(undefined);
    form.sync({version: 1, gpuMemoryPercent: 85, maxNumSeqs: 16, disablePrefixCaching: true});
    assert.equal(byTitle('GPU memory utilization').text, '0.85');
    assert.equal(byTitle('Maximum concurrent sequences').text, '16');
    assert.equal(form.prefixCaching.active, true);
    assert.equal(JSON.stringify(form.value(true)), JSON.stringify({version: 1, gpuMemoryPercent: 85, maxNumSeqs: 16, disablePrefixCaching: true}));
});

test('utilization parses and formats the exact two-digit fraction', () => {
    assert.equal(servingUI.parseUtilization('0.90'), 90);
    assert.equal(servingUI.parseUtilization(' 0.50 '), 50);
    assert.equal(servingUI.parseUtilization('0.95'), 95);
    assert.equal(servingUI.parseUtilization('0.9'), null);
    assert.equal(servingUI.parseUtilization('90'), null);
    assert.equal(servingUI.parseUtilization('0.900'), null);
    assert.equal(servingUI.formatUtilization(90), '0.90');
    assert.equal(servingUI.formatUtilization(5), '0.05');
});

test('checkServing mirrors the backend bounds', () => {
    assert.equal(servingUI.checkServing({version: 1}), null);
    assert.equal(servingUI.checkServing({version: 1, gpuMemoryPercent: 50}), null);
    assert.equal(servingUI.checkServing({version: 1, gpuMemoryPercent: 95}), null);
    assert.equal(servingUI.checkServing({version: 1, maxNumSeqs: 1}), null);
    assert.equal(servingUI.checkServing({version: 1, maxNumSeqs: 64}), null);
    assert.equal(servingUI.checkServing({version: 1, maxNumBatchedTokens: 256}), null);
    assert.equal(servingUI.checkServing({version: 1, maxNumBatchedTokens: 65536}), null);
    assert.match(servingUI.checkServing({version: 1, gpuMemoryPercent: 49}), /0\.50 to 0\.95/);
    assert.match(servingUI.checkServing({version: 1, gpuMemoryPercent: 96}), /0\.50 to 0\.95/);
    assert.match(servingUI.checkServing({version: 1, maxNumSeqs: 0}), /1 to 64/);
    assert.match(servingUI.checkServing({version: 1, maxNumSeqs: 65}), /1 to 64/);
    assert.match(servingUI.checkServing({version: 1, maxNumBatchedTokens: 255}), /256 to 65536/);
    assert.match(servingUI.checkServing({version: 1, maxNumBatchedTokens: 65537}), /256 to 65536/);
});

test('servingSummary describes the pin, or is null without one', () => {
    assert.equal(servingUI.servingSummary({}), null);
    assert.equal(servingUI.servingSummary({nativeModel: {owned: {}}}), null);
    assert.equal(servingUI.servingSummary({nativeModel: {owned: {serving: {}}}}), null);
    assert.equal(servingUI.servingSummary({nativeModel: {owned: {serving: {version: 1}}}}),
        'KV cache auto · GPU memory 0.90 · chunked prefill · prefix caching (sha256)');
    assert.equal(servingUI.servingSummary({nativeModel: {owned: {serving: {version: 1, gpuMemoryPercent: 85, maxNumSeqs: 16, maxNumBatchedTokens: 4096, disablePrefixCaching: true}}}}),
        'KV cache auto · GPU memory 0.85 · chunked prefill · prefix caching off (diagnosis) · up to 16 sequences · 4096 batched tokens');
});

test('withFormServing pins the value into the draft input and cancels a pending render', () => {
    const draft = {input: {binding: {owned: {port: 8000}}}, generation: 0, cancel() { this.generation++; }};
    const form = {state: 'null', value: () => ({version: 1, gpuMemoryPercent: 85})};
    const value = {binding: {owned: {port: 8000}}};
    const next = servingUI.withFormServing(form, draft, value);
    assert.deepEqual(next.binding.owned.serving, {version: 1, gpuMemoryPercent: 85});
    assert.deepEqual(draft.input.binding.owned.serving, {version: 1, gpuMemoryPercent: 85});
    assert.equal(form.state, JSON.stringify({version: 1, gpuMemoryPercent: 85}));
    // An invalid value starts a new draft generation but does not change the input.
    const before = JSON.stringify(draft.input);
    const invalid = {value: () => ({error: 'bad'}), state: 'null'};
    const unchanged = servingUI.withFormServing(invalid, draft, value);
    assert.equal(unchanged, value);
    assert.equal(JSON.stringify(draft.input), before);
    assert.equal(invalid.state, 'invalid');
    assert.equal(draft.generation, 2);
    // No form leaves the value alone.
    assert.equal(servingUI.withFormServing(null, draft, value), value);
    // Without an owned launch the value passes through unchanged.
    const adopt = {value: () => ({version: 1}), state: '"x"'};
    const adoptedValue = {binding: {unit: 'vllm.service'}};
    assert.equal(servingUI.withFormServing(adopt, draft, adoptedValue), adoptedValue);
});

test('checkFormServing refuses an invalid value and serving on an adopted launch', () => {
    const invalid = {value: () => ({error: 'GPU memory utilization must be 0.50 to 0.95.'})};
    assert.throws(() => servingUI.checkFormServing(invalid, {binding: {owned: {}}}), /GPU memory utilization must be 0\.50 to 0\.95\./);
    const pinned = {value: () => ({version: 1})};
    assert.throws(() => servingUI.checkFormServing(pinned, {binding: {unit: 'vllm.service'}}),
        /KV cache and batching options need a launch that Supervisor manages/);
    const ownedInput = {binding: {owned: {port: 8000}}};
    assert.equal(servingUI.checkFormServing(pinned, ownedInput), undefined);
    assert.equal(servingUI.checkFormServing({value: () => undefined}, ownedInput), undefined);
});

test('pinning the defaults on a managed draft reaches validate at catalog v6', async () => {
    const ui = await start();
    await openLaunchDetails(ui);
    assert.equal(ui.by('Pin the KV cache and batching defaults').active, false);
    assert.equal(ui.by('GPU memory utilization').sensitive, false);
    ui.by('Pin the KV cache and batching defaults').active = true;
    assert.equal(ui.by('GPU memory utilization').text, '0.90');
    assert.equal(ui.by('KV cache dtype').text, 'auto');
    assert.equal(ui.by('KV cache dtype').editable, false);
    const request = await review(ui);
    assert.equal(request.catalog.version, 6);
    assert.deepEqual(request.catalog.profiles[0].nativeModel.owned.serving, {version: 1});
    await ui.click('Finish setup');
    assert.equal(ui.calls.at(-1).argv[1], 'apply');
    const applied = JSON.parse(ui.calls.at(-1).input);
    assert.deepEqual(applied.catalog.profiles[0].nativeModel.owned.serving, {version: 1});
});

test('edited options reach validate with the exact bounded values', async () => {
    const ui = await start();
    await openLaunchDetails(ui);
    ui.by('Pin the KV cache and batching defaults').active = true;
    ui.edit(ui.by('GPU memory utilization'), 'text', '0.85');
    ui.edit(ui.by('Maximum concurrent sequences'), 'text', '16');
    ui.edit(ui.by('Chunked prefill budget (tokens)'), 'text', '4096');
    ui.by('Disable prefix caching (diagnosis only)').active = true;
    const request = await review(ui);
    assert.deepEqual(request.catalog.profiles[0].nativeModel.owned.serving,
        {version: 1, gpuMemoryPercent: 85, maxNumSeqs: 16, maxNumBatchedTokens: 4096, disablePrefixCaching: true});
});

test('an unedited optional field keeps the vLLM default and renders nothing', async () => {
    const ui = await start();
    await openLaunchDetails(ui);
    ui.by('Pin the KV cache and batching defaults').active = true;
    ui.edit(ui.by('GPU memory utilization'), 'text', '0.85');
    const request = await review(ui);
    assert.deepEqual(request.catalog.profiles[0].nativeModel.owned.serving, {version: 1, gpuMemoryPercent: 85});
});

test('a value out of bounds is refused before any render', async () => {
    const ui = await start();
    await openLaunchDetails(ui);
    ui.by('Pin the KV cache and batching defaults').active = true;
    ui.edit(ui.by('GPU memory utilization'), 'text', '0.99');
    assert.equal(await review(ui), null);
    assert.ok(!ui.calls.some(call => ['render-owned', 'prepare'].includes(call.argv[1])), 'refused before any render');
    assert.ok(visibleText(ui).includes('GPU memory utilization must be 0.50 to 0.95.'));
});

test('a non-numeric value is refused before any render', async () => {
    const ui = await start();
    await openLaunchDetails(ui);
    ui.by('Pin the KV cache and batching defaults').active = true;
    ui.edit(ui.by('Maximum concurrent sequences'), 'text', 'many');
    assert.equal(await review(ui), null);
    assert.ok(visibleText(ui).includes('enter a number'));
});

test('Cancel restores the serving form of the session', async () => {
    const ui = await start();
    await openLaunchDetails(ui);
    ui.by('Pin the KV cache and batching defaults').active = true;
    ui.edit(ui.by('GPU memory utilization'), 'text', '0.85');
    await ui.click('Cancel');
    await openLaunchDetails(ui);
    assert.equal(ui.by('Pin the KV cache and batching defaults').active, false);
    assert.equal(ui.by('GPU memory utilization').text, '0.90');
});

test('turning the pin off after editing restores the legacy byte form', async () => {
    const ui = await start();
    await openLaunchDetails(ui);
    ui.by('Pin the KV cache and batching defaults').active = true;
    ui.edit(ui.by('GPU memory utilization'), 'text', '0.85');
    ui.by('Pin the KV cache and batching defaults').active = false;
    const request = await review(ui);
    assert.equal(request.catalog.version, 2);
    assert.equal(request.catalog.profiles[0].nativeModel.owned.serving, undefined);
});
