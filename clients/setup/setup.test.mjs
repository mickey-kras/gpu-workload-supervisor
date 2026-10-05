import test from 'node:test';
import assert from 'node:assert/strict';
import {loadGjsModule} from '../gnome/tests/gjs-modules.js';

// Substitute only GI widgets and subprocesses; run the setup's real event handlers.
async function launch({units = [], profiles = [], pending = false, fail = null, version = 'GNOME Shell 50.1', responses = {}, filePath = '/models/selected.gguf', fileError = null, deferAction = 'validate'} = {}) {
    const widgets = []; const calls = []; const deferred = [];
    class Widget {
        signals = new Map(); children = []; sensitive = true;
        constructor(properties = {}) {
            for (const [property, signal] of [['text', 'changed'], ['active', 'toggled'], ['selected', 'notify::selected']]) {
                Object.defineProperty(this, property, {
                    get: () => this[`_${property}`],
                    set: value => {
                        if (this[`_${property}`] === value) return;
                        this[`_${property}`] = value; this.emit(signal);
                    },
                });
            }
            Object.assign(this, properties); widgets.push(this);
        }
        connect(signal, fn) { this.signals.set(signal, fn); }
        emit(signal) { return this.signals.get(signal)?.(this); }
        append(child) { this.children.push(child); }
        add(child) { this.append(child); }
        add_row(child) { this.append(child); }
        add_top_bar(child) { this.append(child); }
        add_bottom_bar(child) { this.append(child); }
        set_child(child) { this.append(child); }
        set_content(child) { this.append(child); }
        add_css_class() {}
        remove(child) { this.children.splice(this.children.indexOf(child), 1); }
        present() {}
        close() { this.closed = true; }
        grab_focus() { this.focused = true; }
        run() { this.emit('activate'); }
    }
    const request = {profile: {statePath: '/state.db', gpuIndex: 2, extra: 'profile'},
        catalog: {version: 1, profiles, extra: {keep: true}}, expectedRevision: 7,
        confirmQuiesced: false, extra: 'request'};
    class StringObject {
        static $gtype = 'GtkStringObject';
        constructor(string) { this.string = string; }
    }
    const native = {
        'gi://Adw?version=1': {default: Object.fromEntries(['Application', 'ApplicationWindow', 'HeaderBar', 'ToolbarView', 'PreferencesGroup', 'EntryRow', 'ComboRow', 'ExpanderRow'].map(name => [name, class extends Widget {}]))},
        'gi://Gtk?version=4.0': {default: {
            ...Object.fromEntries(['Box', 'Label', 'ScrolledWindow', 'Button', 'CheckButton'].map(name => [name, class extends Widget {}])),
            FileDialog: class { open(window, cancel, callback) { callback(this, {}); } select_folder(window, cancel, callback) { callback(this, {}); } open_finish() { if (fileError) throw fileError; return {get_path: () => filePath}; } select_folder_finish() { return this.open_finish(); } }, DialogError: {DISMISSED: 1},
            Orientation: {VERTICAL: 1, HORIZONTAL: 0}, PolicyType: {NEVER: 2},
            StringObject,
            StringList: {new: strings => ({get_string: index => strings[index],
                get_item: index => new StringObject(strings[index]), get_n_items: () => strings.length})},
            PropertyExpression: {new: (type, expression, property) => ({
                evaluate: item => type === StringObject.$gtype && expression === null && item instanceof StringObject && property in item ? [true, item[property]] : [false, null],
            })},
        }},
        'gi://GLib': {default: {getenv: () => 'GNOME', uuid_string_random: () => 'unique-id',
            markup_escape_text: text => text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'),
        }},
        'gi://Gio': {default: {SubprocessFlags: {STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_PIPE: 4},
            Subprocess: {new(argv) { return {
                communicate_utf8_async(input, cancel, callback) {
                    calls.push({argv, input});
                    if (argv[1] === deferAction) deferred.push(() => callback(this, {}));
                    else callback(this, {});
                },
                communicate_utf8_finish() { return [true, argv[1] === '--version' ? version : JSON.stringify(responses[argv[1]] ?? (argv[1] === 'discover' ? {request, units, pending} : {changes: ['Reviewed change']})), 'Backend unavailable']; },
                get_successful: () => argv[1] !== fail,
            }; }} }},
    };
    await loadGjsModule('../../setup/setup.js', native);
    await new Promise(resolve => setImmediate(resolve));
    const by = label => widgets.find(widget => widget.label === label || widget.title === label);
    return {widgets, calls, request, by, finish: () => deferred.shift()(),
        edit(widget, property, value) { widget[property] = value; }};
}

test('large discovery stays compact and adopts no service until explicit selection', async () => {
    const ui = await launch({units: Array.from({length: 10000}, (_, i) => `worker-${i}.service`)});
    assert.ok(ui.widgets.every(widget => (widget.label ?? '').length < 1000));
    assert.deepEqual(ui.calls.map(call => call.argv[1]), ['--version', 'discover', 'drafts']);
    ui.by('Add existing service (Advanced)').emit('clicked');
    const service = ui.by('Existing user service');
    assert.ok(service, 'service selection is available in each workload card');
    assert.equal(service.enable_search, true);
    assert.ok(service.expression, 'search must extract the model item string');
    const matches = Array.from({length: service.model.get_n_items()}, (_, index) => {
        const [ok, value] = service.expression.evaluate(service.model.get_item(index));
        return ok && value.includes('worker-9999') ? value : null;
    }).filter(Boolean);
    assert.deepEqual(matches, ['worker-9999.service']);
    assert.equal(service.selected, 0);
    ui.edit(service, 'selected', 10000, 'notify::selected');
    const validating = ui.by('Review configuration').emit('clicked');
    const sent = JSON.parse(ui.calls.at(-1).input);
    assert.equal(sent.catalog.profiles[0].unit, 'worker-9999.service');
    assert.equal(sent.catalog.profiles[0].cgroup, undefined);
    assert.equal(sent.catalog.profiles[0].healthURL, undefined);
    assert.equal(sent.catalog.profiles[0].requiredMiB, undefined);
    ui.finish(); await validating;
});

test('editing invalidates review and requires fresh explicit confirmation', async () => {
    const ui = await launch({profiles: [{id: 'stable', label: 'Existing', unit: 'old.service', cgroup: '/old', healthURL: 'http://localhost:1', custom: 'keep'}]});
    const reviewing = ui.by('Review configuration').emit('clicked');
    ui.finish(); await reviewing;
    const confirm = ui.widgets.find(widget => widget.children.some(child => child.label?.startsWith('I have paused')));
    ui.edit(confirm, 'active', true, 'toggled');
    assert.equal(ui.by('Apply configuration').sensitive, true);
    ui.edit(ui.by('Display name'), 'text', 'Renamed', 'changed');
    assert.equal(ui.by('Apply configuration').sensitive, false);
    assert.equal(confirm.active, false);
    const second = ui.by('Review configuration').emit('clicked');
    ui.finish(); await second;
    await ui.by('Apply configuration').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
    ui.edit(confirm, 'active', true, 'toggled');
    await ui.by('Apply configuration').emit('clicked');
    const applied = JSON.parse(ui.calls.at(-1).input);
    assert.equal(applied.catalog.profiles[0].id, 'stable');
    assert.equal(applied.catalog.profiles[0].custom, 'keep');
    assert.equal(applied.catalog.profiles[0].label, 'Renamed');
    assert.deepEqual(applied.catalog.extra, {keep: true});
    assert.equal(applied.profile.extra, 'profile');
    assert.equal(applied.extra, 'request');
    assert.equal(applied.confirmQuiesced, true);
});

test('pending activation reviews and resumes its exact original request', async () => {
    const ui = await launch({pending: true, profiles: [{id: 'stable', label: 'Existing', unit: 'missing.service', opaque: {keep: true}}]});
    assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
    const reviewing = ui.by('Review configuration').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input), ui.request);
    ui.finish(); await reviewing;
    const confirm = ui.widgets.find(widget => widget.children.some(child => child.label?.startsWith('I have paused')));
    ui.edit(confirm, 'active', true, 'toggled');
    await ui.by('Apply configuration').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input), {...ui.request, confirmQuiesced: true});
    assert.equal(ui.calls.filter(call => call.argv[1] === 'verify-bindings').length, 0);
});

test('late validation cannot enable applying edited settings', async () => {
    const ui = await launch();
    const reviewing = ui.by('Review configuration').emit('clicked');
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '3', 'changed');
    ui.finish(); await reviewing;
    const confirm = ui.widgets.find(widget => widget.children.some(child => child.label?.startsWith('I have paused')));
    ui.edit(confirm, 'active', true, 'toggled');
    await ui.by('Apply configuration').emit('clicked');
    assert.equal(ui.by('Apply configuration').sensitive, false);
    assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
});


test('required workload fields stay visible and removing a card removes only that profile', async () => {
    const ui = await launch({profiles: [{id: 'keep', unit: 'keep.service', label: 'Keep'}, {unit: 'edit.service', label: 'Edit'}]});
    const group = ui.by('Edit');
    assert.ok(group.children.find(widget => widget.title === 'Workload details').children.includes(ui.widgets.filter(widget => widget.title === 'Cgroup path beneath /sys/fs/cgroup (required)')[1]));
    assert.ok(group.children.find(widget => widget.title === 'Workload details').children.includes(ui.widgets.filter(widget => widget.title === 'Loopback health URL (required)')[1]));
    assert.equal(group.children.find(widget => widget.title === 'Workload details').expanded, true);
    group.children.find(widget => widget.label === 'Remove from supervisor').emit('clicked');
    const reviewing = ui.by('Review configuration').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).catalog.profiles, [{id: 'keep', unit: 'keep.service', label: 'Keep'}]);
    ui.finish(); await reviewing;
});

for (const failure of ['discover', 'validate', 'apply']) {
    test(`${failure} failure cannot enable an unreviewed or repeated apply`, async () => {
        const ui = await launch({fail: failure});
        if (failure === 'discover') {
            assert.equal(ui.by('Review configuration').sensitive, false);
            assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
        } else {
            const reviewing = ui.by('Review configuration').emit('clicked');
            ui.finish(); await reviewing;
            const confirm = ui.widgets.find(widget => widget.children.some(child => child.label?.startsWith('I have paused')));
            if (failure === 'apply') {
                ui.edit(confirm, 'active', true, 'toggled');
                await ui.by('Apply configuration').emit('clicked');
                assert.equal(confirm.sensitive, false);
                assert.equal(ui.by('Review configuration').sensitive, false);
                assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
            }
            await ui.by('Apply configuration').emit('clicked');
            assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, failure === 'apply' ? 1 : 0);
        }
        assert.equal(ui.by('Apply configuration').sensitive, false);
        assert.ok(ui.widgets.some(widget => widget.label?.includes('Backend unavailable')));
    });
}

test('invalid numerical input fails before backend validation and can be corrected', async () => {
    const ui = await launch();
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '-1', 'changed');
    await ui.by('Review configuration').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'validate').length, 0);
    assert.equal(ui.by('Apply configuration').sensitive, false);
    assert.equal(ui.by('Review configuration').sensitive, true);
    ui.edit(ui.by('NVIDIA GPU index'), 'text', '0', 'changed');
    ui.by('Add existing service (Advanced)').emit('clicked');
    const vram = ui.by('Measured VRAM requirement (MiB; optional)');
    ui.edit(vram, 'text', '123.5', 'changed');
    await ui.by('Review configuration').emit('clicked');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'validate').length, 0);
    ui.edit(vram, 'text', '', 'changed');
    const reviewing = ui.by('Review configuration').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].requiredMiB, undefined);
    ui.finish(); await reviewing;
});

test('unsupported desktop never discovers or applies workloads', async () => {
    const ui = await launch({version: 'GNOME Shell 49.0'});
    assert.deepEqual(ui.calls.map(call => call.argv[1]), ['--version']);
    assert.equal(ui.by('Review configuration').sensitive, false);
    assert.equal(ui.by('Add existing service (Advanced)').sensitive, false);
});

test('manual service names preserve template instances and picker updates the same request field', async () => {
    const ui = await launch({units: ['known.service']});
    ui.by('Add existing service (Advanced)').emit('clicked');
    const manual = ui.by('Service name (manual entry)');
    assert.ok(manual, 'undiscovered services remain configurable');
    const service = ui.by('Existing user service');
    ui.edit(service, 'selected', 2, 'notify::selected');
    assert.equal(ui.by('Workload details').expanded, true);
    assert.equal(manual.focused, true);
    ui.edit(manual, 'text', 'worker@model.service', 'changed');
    const reviewing = ui.by('Review configuration').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].unit, 'worker@model.service');
    ui.finish(); await reviewing;
    ui.edit(service, 'selected', 1, 'notify::selected');
    assert.equal(manual.text, 'known.service');
    assert.equal(ui.by('Apply configuration').sensitive, false);
    const second = ui.by('Review configuration').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].unit, 'known.service');
    ui.finish(); await second;
});


test('workload titles escape markup while reviewed labels retain their exact text', async () => {
    const ui = await launch({profiles: [{id: 'literal', label: '<b>GPU & work</b>', unit: 'literal.service'}]});
    const group = ui.widgets.find(widget => widget.description?.startsWith('Select a service'));
    assert.equal(group.title, '&lt;b&gt;GPU &amp; work&lt;/b&gt;');
    ui.edit(ui.by('Display name'), 'text', 'Render < & >');
    assert.equal(group.title, 'Render &lt; &amp; &gt;');
    const reviewing = ui.by('Review configuration').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).catalog.profiles[0].label, 'Render < & >');
    ui.finish(); await reviewing;
});


test('app-first draft is separate from catalog and deferral never applies', async () => {
    const ui = await launch();
    ui.by('Add workload').emit('clicked');
    assert.ok(ui.by('Friendly name'));
    assert.ok(ui.widgets.some(widget => widget.label?.startsWith('ComfyUI workflows')));
    assert.equal(ui.by('Model'), undefined);
    await ui.by('Save drafts').emit('clicked');
    const saved = ui.calls.find(call => call.argv[1] === 'save-drafts');
    assert.equal(JSON.parse(saved.input).drafts[0].app, 'comfyui');
    assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
    ui.by('Set up later').emit('clicked');
    assert.ok(ui.widgets.some(widget => widget.closed));
});

for (const [index, app] of ['comfyui', 'ollama', 'llama.cpp', 'vllm'].entries()) {
    test(`${app} discovery remains a draft until explicit binding verification`, async () => {
        const ui = await launch({responses: {probe: {app, instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'one', label: 'One'}, {id: 'two', label: 'Two'}]}}});
        ui.edit(ui.by('Application'), 'selected', index);
        ui.by('Add workload').emit('clicked');
        await ui.by('Refresh discovery').emit('clicked');
        assert.equal(JSON.parse(ui.calls.find(call => call.argv[1] === 'probe').input).app, app);
        if (app !== 'comfyui') {
            ui.edit(ui.by('Model'), 'selected', 1);
            ui.by('Choose model file...').emit('clicked');
            await ui.by('Refresh discovery').emit('clicked');
            const probe = JSON.parse(ui.calls.at(-1).input);
            assert.equal(probe.reference, '/models/selected.gguf');
            assert.equal(probe.referenceKind, 'model-file');
        }
        assert.equal(ui.calls.filter(call => ['apply', 'verify-bindings'].includes(call.argv[1])).length, 0);
        await ui.by('Verify binding and add for review').emit('clicked');
        assert.equal(ui.calls.at(-1).argv[1], 'verify-bindings');
        const bound = JSON.parse(ui.calls.at(-1).input).catalog.profiles[0];
        assert.equal(bound.nativeModel?.runtime, app === 'comfyui' ? undefined : app);
        assert.equal(ui.calls.filter(call => call.argv[1] === 'apply').length, 0);
    });
}

test('failed verification keeps draft and supports retry; removing draft never applies', async () => {
    const ui = await launch({fail: 'verify-bindings'});
    ui.by('Add workload').emit('clicked');
    await ui.by('Verify binding and add for review').emit('clicked');
    assert.equal(ui.by('Verify binding and add for review').sensitive, true);
    ui.by('Remove draft from supervisor').emit('clicked');
    await ui.by('Save drafts').emit('clicked');
    assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts, []);
});

test('cancelled file picker preserves draft input', async () => {
    const ui = await launch({fileError: {matches: () => true}});
    ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
    ui.by('Choose model file...').emit('clicked');
    await ui.by('Save drafts').emit('clicked');
    assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].reference, undefined);
});

test('native launch fingerprint is computed before binding verification', async () => {
    const ui = await launch({responses: {fingerprint: {sha256: 'verified-fingerprint'}}});
    ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
    ui.edit(ui.by('Loaded service file path'), 'text', '/home/user/.config/systemd/user/model.service');
    await ui.by('Verify binding and add for review').emit('clicked');
    const fingerprint = ui.calls.find(call => call.argv[1] === 'fingerprint');
    assert.ok(fingerprint);
    assert.equal(JSON.parse(fingerprint.input).binding.launchFile, '/home/user/.config/systemd/user/model.service');
    const binding = JSON.parse(ui.calls.find(call => call.argv[1] === 'verify-bindings').input);
    assert.equal(binding.catalog.profiles[0].nativeModel.launchSHA256, 'verified-fingerprint');
});


test('editing a draft during fingerprinting cannot promote the old binding', async () => {
    const ui = await launch({deferAction: 'fingerprint', responses: {fingerprint: {sha256: 'old'}}});
    ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
    const checking = ui.by('Verify binding and add for review').emit('clicked');
    ui.edit(ui.by('Friendly name'), 'text', 'Changed');
    ui.finish(); await checking;
    assert.equal(ui.calls.filter(call => call.argv[1] === 'verify-bindings').length, 0);
    assert.equal(ui.by('Verify binding and add for review').sensitive, true);
});

test('review refreshes configured native launch fingerprints automatically', async () => {
    const ui = await launch({profiles: [{id: 'text', nativeModel: {runtime: 'ollama', model: 'chosen', launchFile: '/trusted/model.service', launchSHA256: 'old'}}],
        responses: {fingerprint: {sha256: 'new'}}, deferAction: 'none'});
    assert.equal(ui.by('Service file SHA-256'), undefined);
    await ui.by('Review configuration').emit('clicked');
    const validation = JSON.parse(ui.calls.find(call => call.argv[1] === 'validate').input);
    assert.equal(validation.catalog.profiles[0].nativeModel.launchSHA256, 'new');
    ui.edit(ui.widgets.find(widget => widget.active === false && widget.sensitive), 'active', true);
    await ui.by('Apply configuration').emit('clicked');
    const applied = JSON.parse(ui.calls.find(call => call.argv[1] === 'apply').input);
    assert.equal(applied.catalog.profiles[0].nativeModel.launchSHA256, 'new');
});

test('Save drafts restores unverified launch fields without fingerprint proof', async () => {
    const ui = await launch();
    ui.edit(ui.by('Application'), 'selected', 1); ui.by('Add workload').emit('clicked');
    for (const [label, value] of [['Existing user service', 'ollama-custom.service'], ['Cgroup path', '/custom'], ['Loopback health URL', 'http://127.0.0.1:11434'], ['Runtime instance ID', 'custom'], ['Exact model ID', 'chosen'], ['Loaded service file path', '/trusted/model.service']])
        ui.edit(ui.by(label), 'text', value);
    await ui.by('Save drafts').emit('clicked');
    const saved = JSON.parse(ui.calls.at(-1).input);
    assert.equal(saved.drafts[0].binding.unit, 'ollama-custom.service');
    assert.equal(saved.drafts[0].binding.model, 'chosen');
    assert.equal('launchSHA256' in saved.drafts[0].binding, false);
    const reopened = await launch({responses: {drafts: saved}});
    assert.equal(reopened.by('Existing user service').text, 'ollama-custom.service');
    assert.equal(reopened.by('Exact model ID').text, 'chosen');
    assert.equal(reopened.calls.some(call => call.argv[1] === 'verify-bindings'), false);
});
