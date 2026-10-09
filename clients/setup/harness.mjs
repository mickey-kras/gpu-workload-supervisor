import {createWidgetClass} from './harness-widgets.mjs';
import {loadGjsModule} from '../gnome/tests/gjs-modules.js';

// Substitute only GI widgets and subprocesses; run the setup's real event handlers.
export async function launch({units = [], profiles = [], pending = false, fail = null, version = 'GNOME Shell 50.1', responses = {}, filePath = '/models/selected.gguf', fileError = null, deferAction = 'validate', deferOccurrence = null} = {}) {
    const widgets = []; const calls = []; const deferred = []; const signals = []; let uuidSequence = 0;
    const Widget = createWidgetClass(widgets);
    const request = {profile: {statePath: '/state.db', gpuIndex: 2, extra: 'profile'},
        catalog: {version: 1, profiles, extra: {keep: true}}, expectedRevision: 7,
        confirmQuiesced: false, extra: 'request'};
    class StringObject {
        static $gtype = 'GtkStringObject';
        constructor(string) { this.string = string; }
    }
    const native = {
        'gi://Adw?version=1': {default: {StyleManager: {get_default: () => ({dark: true, high_contrast: false, connect() {}})}, ...Object.fromEntries(['Application', 'ApplicationWindow', 'HeaderBar', 'ToolbarView', 'PreferencesGroup', 'EntryRow', 'ComboRow', 'ExpanderRow'].map(name => [name, class extends Widget { constructor(properties) { super(properties); this.widgetType = name; } }]))}},
        'gi://Gtk?version=4.0': {default: {
            ...Object.fromEntries(['Box', 'Label', 'ScrolledWindow', 'Button', 'CheckButton', 'Image', 'Separator', 'Expander'].map(name => [name, class extends Widget { constructor(properties) { super(properties); this.widgetType = name; } }])),
            IconTheme: {get_for_display: () => ({lookup_by_gicon: (icon, size, scale) => ({icon, size, scale})})},
            TextDirection: {NONE: 0}, IconLookupFlags: {FORCE_REGULAR: 1},
            CssProvider: class { load_from_data() {} }, StyleContext: {add_provider_for_display() {}}, STYLE_PROVIDER_PRIORITY_APPLICATION: 600,
            Align: {CENTER: 3, START: 1}, AccessibleRole: {PRESENTATION: 1},
            FileDialog: class { open(window, cancel, callback) { callback(this, {}); } select_folder(window, cancel, callback) { callback(this, {}); }
                open_finish() {
                    if (fileError) throw fileError;
                    return {get_path: () => filePath};
                }
                select_folder_finish() { return this.open_finish(); } }, DialogError: {DISMISSED: 1},
            AccessibleProperty: {LABEL: 'label'},
            Orientation: {VERTICAL: 1, HORIZONTAL: 0}, PolicyType: {NEVER: 2},
            StringObject,
            StringList: {new: strings => ({get_string: index => strings[index],
                get_item: index => new StringObject(strings[index]), get_n_items: () => strings.length})},
            PropertyExpression: {new: (type, expression, property) => ({
                evaluate: item => type === StringObject.$gtype && expression === null && item instanceof StringObject && property in item ? [true, item[property]] : [false, null],
            })},
        }},
        'gi://GLib': {default: {getenv: () => 'GNOME', uuid_string_random: () => uuidSequence++ ? `unique-id-${uuidSequence}` : 'unique-id',
            markup_escape_text: text => text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;'),
        }},
        'gi://Gio': {default: {FileIcon: class { constructor(properties) { Object.assign(this, properties); } }, File: {new_for_uri: () => ({get_parent: () => ({get_child: name => ({get_path: () => '/setup/' + name, query_exists: () => true})})})}, SubprocessFlags: {STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_PIPE: 4},
            Subprocess: {new(argv) { let requestInput; return {
                send_signal(signal) { signals.push({argv, signal}); },
                communicate_utf8_async(input, cancel, callback) {
                    requestInput = input; calls.push({argv, input});
                    if (argv[1] === deferAction && (deferOccurrence === null || calls.filter(call => call.argv[1] === deferAction).length === deferOccurrence)) deferred.push(() => callback(this, {}));
                    else callback(this, {});
                },
                communicate_utf8_finish() {
                    if (argv[1] === '--version') return [true, version, 'Backend unavailable'];
                    const fallback = argv[1] === 'discover' ? {request, units, pending} : {changes: ['Reviewed change']};
                    return [true, JSON.stringify(typeof responses[argv[1]] === 'function' ? responses[argv[1]](JSON.parse(requestInput ?? 'null'), calls) : responses[argv[1]] ?? fallback), 'Backend unavailable'];
                },
                get_successful: () => argv[1] !== fail,
            }; }} }},
    };
    await loadGjsModule('../../setup/setup.js', native);
    await new Promise(resolve => setImmediate(resolve));
    const by = label => widgets.find(widget => widget.label === label || widget.title === label || widget.accessibleProperties?.label === label);
    const visible = widget => {
        if (!widget || widget.visible === false) return false;
        const parent = widgets.find(candidate => candidate.children.includes(widget));
        if (!parent) return true;
        if (['ExpanderRow', 'Expander'].includes(parent.widgetType) && !parent.expanded) return false;
        return visible(parent);
    };
    const click = async label => {
        const widget = by(label);
        if (!visible(widget) || !widget.sensitive) throw new Error(`Control is not actionable: ${label}`);
        await widget.emit('clicked');
    };
    return {widgets, calls, signals, request, by, visible, click, selectApplication(index) {
        const label = ['ComfyUI', 'Ollama', 'llama.cpp', 'vLLM'][index];
        by(`Use ${label}`).active = true;
        by(`Configure ${label}`).emit('clicked');
    }, finish: () => deferred.shift()(),
        edit(widget, property, value) { widget[property] = value; }};
}
