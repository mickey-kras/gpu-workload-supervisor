import {loadGjsModule} from '../gnome/tests/gjs-modules.js';

// Substitute only GI widgets and subprocesses; run the setup's real event handlers.
export async function launch({units = [], profiles = [], pending = false, fail = null, version = 'GNOME Shell 50.1', responses = {}, filePath = '/models/selected.gguf', fileError = null, deferAction = 'validate'} = {}) {
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
        add_css_class(name) { this.cssClasses ??= []; this.cssClasses.push(name); }
        remove(child) { this.children.splice(this.children.indexOf(child), 1); }
        present() { this.presented = true; }
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
            FileDialog: class { open(window, cancel, callback) { callback(this, {}); } select_folder(window, cancel, callback) { callback(this, {}); }
                open_finish() {
                    if (fileError) throw fileError;
                    return {get_path: () => filePath};
                }
                select_folder_finish() { return this.open_finish(); } }, DialogError: {DISMISSED: 1},
            Orientation: {VERTICAL: 1, HORIZONTAL: 0}, PolicyType: {NEVER: 2},
            StringObject,
            StringList: {new: strings => ({get_string: index => strings[index],
                get_item: index => new StringObject(strings[index]), get_n_items: () => strings.length})},
            PropertyExpression: {new: (type, expression, property) => ({
                evaluate: item => type === StringObject.$gtype && expression === null && item instanceof StringObject && property in item ? [true, item[property]] : [false, null],
            })},
        }},
        'gi://GLib': {default: {getenv: () => 'GNOME', uuid_string_random: () => 'unique-id',
            markup_escape_text: text => text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;'),
        }},
        'gi://Gio': {default: {SubprocessFlags: {STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_PIPE: 4},
            Subprocess: {new(argv) { return {
                communicate_utf8_async(input, cancel, callback) {
                    calls.push({argv, input});
                    if (argv[1] === deferAction) deferred.push(() => callback(this, {}));
                    else callback(this, {});
                },
                communicate_utf8_finish() {
                    if (argv[1] === '--version') return [true, version, 'Backend unavailable'];
                    const fallback = argv[1] === 'discover' ? {request, units, pending} : {changes: ['Reviewed change']};
                    return [true, JSON.stringify(responses[argv[1]] ?? fallback), 'Backend unavailable'];
                },
                get_successful: () => argv[1] !== fail,
            }; }} }},
    };
    await loadGjsModule('../../setup/setup.js', native);
    await new Promise(resolve => setImmediate(resolve));
    const by = label => widgets.find(widget => widget.label === label || widget.title === label);
    return {widgets, calls, request, by, finish: () => deferred.shift()(),
        edit(widget, property, value) { widget[property] = value; }};
}
