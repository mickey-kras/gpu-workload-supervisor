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


test('review: refreshed inventory preserves selected model', async () => {
 const ui=await launch({responses:{probe:{app:'ollama',instanceStatus:'available',inventoryStatus:'available',models:[{id:'one'},{id:'two'}]}}});
 ui.edit(ui.by('Application'),'selected',1);ui.by('Add workload').emit('clicked');
 await ui.by('Refresh discovery').emit('clicked');
 ui.edit(ui.by('Model'),'selected',1);
 await ui.by('Save drafts').emit('clicked');
 assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model,'two');
 await ui.by('Refresh discovery').emit('clicked');
 await ui.by('Save drafts').emit('clicked');
 assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].model,'two');
});
test('review: explicit empty inventory is distinguished from unsupported inventory', async () => {
 const labels=[];
 for(const inventoryStatus of ['available','unsupported']) {
  const ui=await launch({responses:{probe:{app:'ollama',instanceStatus:'available',inventoryStatus,models:[]}}});
  ui.edit(ui.by('Application'),'selected',1);ui.by('Add workload').emit('clicked');
  await ui.by('Refresh discovery').emit('clicked');
  labels.push(ui.widgets.filter(w=>w.label||w.title).map(w=>w.label||w.title).join('\n')+'\n'+ui.by('Model').model.get_string(0));
 }
 assert.notEqual(labels[0],labels[1]);
});
test('review: selecting stopped unit clears prior endpoint evidence', async () => {
 const profiles=[];
 const request={profile:{statePath:'/state.db',gpuIndex:2},catalog:{version:1,profiles},expectedRevision:7};
 const ui=await launch({responses:{discover:{request,units:[],applications:[
 {app:'ollama',label:'Endpoint A',endpoint:'http://127.0.0.1:11434',instanceStatus:'available',models:[{id:'one'}]},
 {app:'ollama',label:'Stopped unit B',unit:'ollama-b.service',cgroup:'/b',instanceStatus:'not-running',models:[{id:'two'}]},
 ]},fingerprint:{sha256:'fingerprint'}}});
 ui.edit(ui.by('Application'),'selected',1);ui.by('Add workload').emit('clicked');
 ui.edit(ui.by('Detected instance'),'selected',1);
 ui.edit(ui.by('Detected instance'),'selected',2);
 await ui.by('Save drafts').emit('clicked');
 assert.equal(JSON.parse(ui.calls.at(-1).input).drafts[0].endpoint,undefined);
});

test('review: late discovery cannot overwrite edited endpoint', async () => {
 const ui=await launch({deferAction:'probe',responses:{probe:{app:'ollama',instanceStatus:'available',models:[{id:'stale'}]}}});
 ui.edit(ui.by('Application'),'selected',1);ui.by('Add workload').emit('clicked');
 const checking=ui.by('Refresh discovery').emit('clicked');
 ui.edit(ui.by('Application address'),'text','http://127.0.0.1:2222');
 ui.finish();await checking;
 await ui.by('Save drafts').emit('clicked');
 const saved=JSON.parse(ui.calls.at(-1).input).drafts[0];
 assert.equal(saved.endpoint,'http://127.0.0.1:2222');assert.equal(saved.model,undefined);
});
test('review: late binding cannot promote removed draft', async () => {
 const ui=await launch({deferAction:'verify-bindings'});
 ui.by('Add workload').emit('clicked');
 const binding=ui.by('Verify binding and add for review').emit('clicked');
 ui.by('Remove draft from supervisor').emit('clicked');
 ui.finish();await binding;
 assert.equal(ui.widgets.filter(w=>w.title==='Display name').length,0);
 await ui.by('Save drafts').emit('clicked');assert.deepEqual(JSON.parse(ui.calls.at(-1).input).drafts,[]);
});
test('review: late fingerprint cannot verify edited draft', async () => {
 const ui=await launch({deferAction:'fingerprint',responses:{fingerprint:{sha256:'old-file'}}});
 ui.edit(ui.by('Application'),'selected',1);ui.by('Add workload').emit('clicked');
 const binding=ui.by('Verify binding and add for review').emit('clicked');
 ui.edit(ui.by('Loaded service file path'),'text','/changed.service');
 ui.finish();await binding;
 assert.equal(ui.calls.filter(c=>c.argv[1]==='verify-bindings').length,0);
});
