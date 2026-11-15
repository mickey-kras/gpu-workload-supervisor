import {createHash} from 'node:crypto';
import {createWidgetClass} from './harness-widgets.mjs';
import {loadGjsModule} from '../gnome/tests/gjs-modules.js';

// The native icon constructor returns a property bag in this widget fixture.
function FileIcon(properties) { return {...properties}; }
function fileAtURI() { return {get_parent: () => ({get_child: () => ({query_exists: () => true})})}; }

// Substitute only GI widgets and subprocesses; run the setup's real event handlers.
export async function launch({units = [], profiles = [], pending = false, fail = null, version = 'GNOME Shell 50.1', responses = {}, filePath = '/models/selected', fileError = null, deferAction = 'validate', deferOccurrence = null, stderr = {}} = {}) {
    const widgets = []; const calls = []; const deferred = []; let uuidSequence = 0;
    const Widget = createWidgetClass(widgets);
    const request = {profile: {statePath: '/state.db', gpuIndex: 2, extra: 'profile'},
        catalog: {version: 1, profiles, extra: {keep: true}}, expectedRevision: 7,
        confirmQuiesced: false, extra: 'request'};
    class StringObject {
        static $gtype = 'GtkStringObject';
        constructor(string) { this.string = string; }
    }
    const appearance = {dark: true, high_contrast: false, handlers: new Map(),
        connect(signal, fn) { const id = this.handlers.size + 1; this.handlers.set(id, {signal, fn}); return id; },
        disconnect(id) { this.handlers.delete(id); },
    };
    const styleProviders = new Set();
    const native = {
        'gi://Adw?version=1': {default: {StyleManager: {get_default: () => appearance}, ...Object.fromEntries(['Application', 'ApplicationWindow', 'HeaderBar', 'ToolbarView', 'PreferencesGroup', 'EntryRow', 'ComboRow', 'ExpanderRow'].map(name => [name, class extends Widget { constructor(properties) { super(properties); this.widgetType = name; } }]))}},
        'gi://Gtk?version=4.0': {default: {
            ...Object.fromEntries(['Box', 'Label', 'ScrolledWindow', 'Button', 'CheckButton', 'Image', 'Picture', 'Separator', 'Expander'].map(name => [name, class extends Widget { constructor(properties) { super(properties); this.widgetType = name; } }])),
            IconTheme: {get_for_display: () => ({lookup_by_gicon: (icon, size, scale) => ({icon, size, scale})})},
            TextDirection: {NONE: 0}, IconLookupFlags: {FORCE_REGULAR: 1},
            CssProvider: class { load_from_data(data) { this.data = data; } }, StyleContext: {add_provider_for_display(display, provider) { styleProviders.add(provider); },
                remove_provider_for_display(display, provider) { styleProviders.delete(provider); }}, STYLE_PROVIDER_PRIORITY_APPLICATION: 600,
            ContentFit: {CONTAIN: 1},
            Align: {CENTER: 3, START: 1}, AccessibleRole: {PRESENTATION: 1, HEADING: 2},
            FileDialog: class { open(window, cancel, callback) { callback(this, {}); } select_folder(window, cancel, callback) { callback(this, {}); }
                open_finish() {
                    if (fileError) throw fileError;
                    return {get_path: () => typeof filePath === 'function' ? filePath() : filePath};
                }
                select_folder_finish() { return this.open_finish(); } }, DialogError: {DISMISSED: 1},
            AccessibleProperty: {LABEL: 'label'},
            AccessibleState: {EXPANDED: 'expanded'},
            AccessibleTristate: {FALSE: 0, TRUE: 1},
            Orientation: {VERTICAL: 1, HORIZONTAL: 0}, PolicyType: {NEVER: 2},
            StringObject,
            StringList: {new: strings => ({get_string: index => strings[index],
                get_item: index => new StringObject(strings[index]), get_n_items: () => strings.length})},
            PropertyExpression: {new: (type, expression, property) => ({
                evaluate: item => type === StringObject.$gtype && expression === null && item instanceof StringObject && property in item ? [true, item[property]] : [false, null],
            })},
        }},
        'gi://Pango': {default: {WrapMode: {WORD_CHAR: 2}}},
        'gi://Gdk?version=4.0': {default: {ContentProvider: {new_for_value: value => ({value})}}},
        'gi://GObject': {default: {TYPE_STRING: 'string', Value: class { init(type) { this.type = type; } set_string(text) { this.text = text; } }}},
        'gi://GLib': {default: {ChecksumType: {SHA256: 2}, compute_checksum_for_string: (type, text) => createHash('sha256').update(text).digest('hex'), PRIORITY_DEFAULT_IDLE: 200, SOURCE_REMOVE: false, idle_add(priority, callback) { callback(); return 1; }, getenv: () => 'GNOME', uuid_string_random: () => uuidSequence++ ? `unique-id-${uuidSequence}` : 'unique-id',
            markup_escape_text: text => text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;'),
        }},
        'gi://Gio': {default: {FileIcon, File: {new_for_uri: fileAtURI}, SubprocessFlags: {STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_PIPE: 4},
            Cancellable, Subprocess: subprocesses({calls, deferred, deferAction, deferOccurrence, version, responses, fallback: {request, units, pending}, fail, stderr})}},
    };
    // The application log of the module, as console.warn writes it in GJS.
    const logs = [];
    await loadGjsModule('../../setup/setup.js', native, {console: {warn: message => logs.push(message)}});
    await new Promise(resolve => setImmediate(resolve));
    const by = label => {
        const matches = widgets.filter(widget => widget.label === label || widget.title === label || widget.accessibleProperties?.label === label);
        return matches.find(visible) ?? matches[0];
    };
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
    return {widgets, calls, logs, request, appearance, styleProviders, by, visible, click, selectApplication(index) {
        const label = ['ComfyUI', 'vLLM'][index];
        by(`Use ${label}`).active = true;
        by(`Settings for ${label}`).emit('clicked');
    }, finish: () => deferred.shift()(),
        edit(widget, property, value) { widget[property] = value; }};
}

// Like Gio.Cancellable, connect takes the callback alone: GJS shadows the
// signal connect of GObject with g_cancellable_connect.
class Cancellable {
    handlers = []; cancelled = false;
    connect(fn) { this.handlers.push(fn); }
    is_cancelled() { return this.cancelled; }
    cancel() { this.cancelled = true; for (const fn of this.handlers) fn(); }
}

// Deferred calls complete with finish(); a call stopped before then never does.
function subprocesses({calls, deferred, deferAction, deferOccurrence, version, responses, fallback, fail, stderr}) {
    return {new(argv) { let requestInput; let call; let waiting = null; let killed = false; return {
        communicate_utf8_async(input, cancel, callback) {
            requestInput = input; call = {argv, input, cancellable: cancel}; calls.push(call);
            if (argv[1] === deferAction && (deferOccurrence === null || calls.filter(item => item.argv[1] === deferAction).length === deferOccurrence)) {
                waiting = () => callback(this, {});
                deferred.push(() => waiting?.());
            } else callback(this, {});
        },
        force_exit() { killed = true; call.killed = true; const finish = waiting; waiting = null; finish?.(); },
        communicate_utf8_finish() {
            if (killed) throw new Error('Operation was cancelled');
            if (argv[1] === '--version') return [true, version, 'Backend unavailable'];
            const response = argv[1] === 'discover' ? fallback : {changes: ['Reviewed change']};
            return [true, JSON.stringify(typeof responses[argv[1]] === 'function' ? responses[argv[1]](JSON.parse(requestInput ?? 'null'), calls) : responses[argv[1]] ?? response), stderr[argv[1]] ?? 'Backend unavailable'];
        },
        get_successful: () => argv[1] !== fail,
    }; }};
}
