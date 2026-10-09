// Render production GTK widgets; only the read-only backend command is injected.
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
import {createSetupWindow} from '../../clients/setup/setup-window.mjs';

const [output, theme = 'light', width = '620', height = '670'] = ARGV;
const criticalLogs = [];
// Preserve native messages and add the synchronous JS origin of GTK criticals.
// This catches GI calls made during widget construction, before a frame exists.
GLib.log_set_writer_func((level, fields) => {
    const decoded = Object.fromEntries(Object.entries(fields).map(([key, value]) => [key,
        typeof value === 'string' ? value : new TextDecoder().decode(value)]));
    const critical = Boolean(level & GLib.LogLevelFlags.LEVEL_CRITICAL);
    const severity = critical ? 'CRITICAL' : (level & GLib.LogLevelFlags.LEVEL_WARNING) ? 'WARNING' : 'LOG';
    printerr(`${decoded.GLIB_DOMAIN || 'GLib'}-${severity}: ${decoded.MESSAGE || JSON.stringify(decoded)}`);
    if (critical) {
        const entry = {fields: decoded, stack: new Error('Native critical origin').stack};
        criticalLogs.push(entry);
        printerr(entry.stack);
        GLib.file_set_contents(`${output}/native-criticals.json`, JSON.stringify(criticalLogs, null, 2));
    }
    return GLib.LogWriterOutput.HANDLED;
});
function assert(condition, message) { if (!condition) throw new Error(message); }
const delay = ms => new Promise(resolve => GLib.timeout_add(GLib.PRIORITY_DEFAULT, ms, () => { resolve(); return GLib.SOURCE_REMOVE; }));
function widgets(root) {
    const found = [root];
    for (let child = root.get_first_child(); child; child = child.get_next_sibling()) found.push(...widgets(child));
    return found;
}
function byLabel(root, label) { return widgets(root).find(widget => widget instanceof Gtk.CheckButton && widget.label === label); }
function run(argv) {
    const process = Gio.Subprocess.new(argv, Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE);
    const [, stdout, stderr] = process.communicate_utf8(null, null);
    assert(process.get_successful(), `${argv[0]}: ${stderr}`);
    return stdout.trim();
}
async function runAsync(argv) {
    const process = Gio.Subprocess.new(argv, Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE);
    return new Promise((resolve, reject) => process.communicate_utf8_async(null, null, (child, result) => {
        try {
            const [, stdout, stderr] = child.communicate_utf8_finish(result);
            assert(child.get_successful(), `${argv[0]}: ${stderr}`); resolve(stdout);
        } catch (error) { reject(error); }
    }));
}
const calls = [];
let mode = 'stopped';
const candidate = {app: 'ollama', label: 'Ollama', unit: 'example-model.service',
    endpoint: 'http://127.0.0.1:11434', cgroup: '/example/model', location: '/example/model.service',
    recognized: true, configurationStatus: 'ready', instanceStatus: 'not-running', inventoryStatus: 'available',
    models: [{id: 'example-small', label: 'Example small'}, {id: 'example-large', label: 'Example large'}],
    binding: {unit: 'example-model.service', cgroup: '/example/model', healthURL: 'http://127.0.0.1:11434/api/tags', launchFile: '/example/model.service'}};
const comfy = {...candidate, app: 'comfyui', label: 'ComfyUI', unit: 'example-image.service', models: [],
    binding: {...candidate.binding, unit: 'example-image.service', model: ''}};
async function command(argv, input) {
    const action = argv[1]; calls.push(action);
    assert(!['apply', 'render-owned', 'temporary-start'].includes(action), `unexpected mutation: ${action}`);
    if (action === '--version') return 'GNOME Shell 50.1';
    if (action === 'discover') return JSON.stringify({request: {profile: {statePath: '/example/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}}, units: [], applications: mode === 'missing' ? [] : [comfy, candidate], errors: mode === 'error' ? ['Example discovery unavailable.'] : []});
    if (action === 'temporary-status') return JSON.stringify({available: false});
    if (action === 'drafts') return JSON.stringify({revision: 'fixture', drafts: []});
    if (action === 'prepare') {
        const {draft} = JSON.parse(input);
        const installation = draft.app === 'comfyui' ? comfy : candidate;
        return JSON.stringify({profile: {id: draft.id, label: draft.label, adapter: 'systemd', unit: installation.unit,
            cgroup: installation.cgroup, healthURL: installation.binding.healthURL, bootPolicy: 'stop-to-idle',
            ...(draft.app === 'comfyui' ? {launchBinding: {runtime: 'comfyui', launchFile: comfy.binding.launchFile}} :
                {nativeModel: {runtime: 'ollama', model: draft.model, endpoint: candidate.endpoint, launchFile: candidate.binding.launchFile}})}});
    }
    if (action === 'fingerprint') return JSON.stringify({sha256: 'generic-fixture-sha'});
    if (action === 'validate') return JSON.stringify({changes: ['Configure the selected existing model.']});
    if (action === 'verify-bindings') return '{}';
    throw new Error(`unhandled fixture command: ${action}`);
}
const desktop = new Gio.Settings({schema_id: 'org.gnome.desktop.interface'});
if (theme === 'highcontrast') {
    desktop.set_string('gtk-theme', 'HighContrast');
    new Gio.Settings({schema_id: 'org.gnome.desktop.a11y.interface'}).set_boolean('high-contrast', true);
}
const app = new Adw.Application({application_id: 'local.GPUWorkload.NativeFixture'});
let failed = false;
app.connect('activate', () => {
    app.hold(); // Keep the event loop alive while retained closed windows are tested.
    (async () => {
        const style = Adw.StyleManager.get_default();
        style.set_color_scheme(theme === 'dark' ? Adw.ColorScheme.FORCE_DARK : Adw.ColorScheme.FORCE_LIGHT);
        async function open(nextMode) {
            mode = nextMode;
            const ui = createSetupWindow({app, command});
            ui.window.set_default_size(Number(width), Number(height));
            await ui.initialized; await delay(250);
            const actualWidth = ui.window.get_width(), actualHeight = ui.window.get_height();
            const startupXid = run(['xdotool', 'search', '--onlyvisible', '--name', ui.window.title]).split('\n').at(-1);
            const x11 = Object.fromEntries(run(['xdotool', 'getwindowgeometry', '--shell', startupXid]).split('\n').map(line => line.split('=')));
            const scale = Number(GLib.getenv('GDK_SCALE') || 1);
            const surface = ui.window.get_surface();
            // GtkWidget allocation excludes CSD shadows; X11 drawable dimensions
            // and screenshot pixels measure the complete native window at scale.
            const startup = {expectedLogicalWindow: {width: Number(width), height: Number(height)},
                contentAllocation: {width: actualWidth, height: actualHeight},
                headingSelection: ui.heading.get_selection_bounds(),
                surfaceLogical: {width: surface.get_width(), height: surface.get_height()},
                x11Pixels: {width: Number(x11.WIDTH), height: Number(x11.HEIGHT)}, scale,
                minimumHorizontal: ui.window.measure(Gtk.Orientation.HORIZONTAL, -1),
                minimumVertical: ui.window.measure(Gtk.Orientation.VERTICAL, actualWidth)};
            GLib.file_set_contents(`${output}/startup-${nextMode}-geometry.json`, JSON.stringify(startup, null, 2));
            run(['import', '-window', startupXid, `${output}/startup-${nextMode}.png`]);
            assert(!startup.headingSelection[0], `initial heading must not select its text: ${JSON.stringify(startup.headingSelection)}`);
            assert(Number(x11.WIDTH) === Number(width) * scale, `native window width mismatch: geometry=${JSON.stringify(startup)}`);
            assert(Number(x11.HEIGHT) === Number(height) * scale, `native window height mismatch: geometry=${JSON.stringify(startup)}`);
            const appImages = widgets(ui.window).filter(widget => widget.has_css_class('setup-icon-tile')).flatMap(tile => widgets(tile).filter(widget => widget instanceof Gtk.Image || widget instanceof Gtk.Picture));
            assert(appImages.length >= 4, 'all four application icons are rendered');
            startup.icons = appImages.map(image => ({widget: image.constructor.name,
                width: image.get_width(), height: image.get_height(),
                intrinsicWidth: image.paintable?.get_intrinsic_width(), intrinsicHeight: image.paintable?.get_intrinsic_height()}));
            GLib.file_set_contents(`${output}/startup-${nextMode}-geometry.json`, JSON.stringify(startup, null, 2));
            for (const icon of startup.icons) {
                assert(icon.intrinsicWidth > 0 && icon.intrinsicWidth <= 44 && icon.intrinsicHeight > 0 && icon.intrinsicHeight <= 44, `application icon paintable must fit its 44px tile: ${JSON.stringify(icon)}`);
                assert(icon.width === 44 && icon.height === 44, `application icon must display in a 44px square: ${JSON.stringify(icon)}`);
            }
            return ui;
        }
        const geometry = [];
        const retainedClosed = [];
        const lifetime = [];
        async function closeAndVerify(ui, state) {
            retainedClosed.push(ui); // Prevent disposal from masking signal reference cycles.
            ui.window.close(); await delay(80);
            assert(!ui.window.get_visible(), 'permanent close hides the retained native window');
            const darkAfterClose = ui.window.has_css_class('setup-dark');
            const originalScheme = style.color_scheme;
            style.set_color_scheme(darkAfterClose ? Adw.ColorScheme.FORCE_LIGHT : Adw.ColorScheme.FORCE_DARK);
            await delay(80);
            assert(ui.window.has_css_class('setup-dark') === darkAfterClose, 'closed retained window must not receive appearance updates');
            style.set_color_scheme(originalScheme); await delay(30);
            lifetime.push({state, retained: true, darkAfterClose, appearanceUnchanged: true});
        }
        async function inspect(screen, name, focus, disclosure = null) {
            const argv = ['/usr/bin/python3', 'tests/desktop/setup_accessibility.py', `${output}/accessibility-${name}.json`, screen, focus];
            if (disclosure) argv.push(disclosure);
            await runAsync(argv);
        }
        function ownsFocus(ui, target) {
            for (let focus = ui.window.get_focus(); focus; focus = focus.get_parent()) if (focus === target) return true;
            return false;
        }
        async function tabTo(ui, target) {
            for (let count = 0; count < 100; count++) {
                if (ownsFocus(ui, target)) return;
                run(['xdotool', 'key', 'Tab']); await delay(20);
            }
            throw new Error(`keyboard cannot reach ${target.label}`);
        }
        async function activate(ui, target, heading) {
            await tabTo(ui, target);
            run(['xdotool', 'key', 'space']);
            for (let count = 0; count < 100 && ui.heading.label !== heading; count++) await delay(20);
            assert(ui.heading.label === heading, `keyboard activation must reach ${heading}`);
            await delay(80);
            const transition = heading.toLowerCase().replaceAll(' ', '-');
            const transitionXid = run(['xdotool', 'search', '--onlyvisible', '--name', ui.window.title]).split('\n').at(-1);
            run(['import', '-window', transitionXid, `${output}/transition-${transition}.png`]);
            const focus = ui.window.get_focus();
            const diagnostics = {expectedHeading: heading, actualHeading: ui.heading.label,
                headingFocusable: ui.heading.get_focusable(), headingMapped: ui.heading.get_mapped(),
                headingSelection: ui.heading.get_selection_bounds(),
                currentFocus: focus ? {widget: focus.constructor.name, label: focus.label ?? null, name: focus.get_name()} : null};
            GLib.file_set_contents(`${output}/transition-${transition}-focus.json`, JSON.stringify(diagnostics, null, 2));
            await runAsync(['/usr/bin/python3', 'tests/desktop/setup_accessibility.py', `${output}/accessibility-transition-${transition}.json`, 'diagnostic']);
            assert(ownsFocus(ui, ui.heading), `screen transition focuses its heading: ${JSON.stringify(diagnostics)}`);
            assert(!diagnostics.headingSelection[0], `transition heading must not select its text: ${JSON.stringify(diagnostics)}`);
        }
        async function capture(ui, name) {
            await delay(180);
            const footer = [ui.review, ui.apply, ui.back, ...widgets(ui.window).filter(widget => widget instanceof Gtk.Button && widget.label === 'Set up later')].filter(widget => widget.get_mapped());
            const rectangles = footer.map(widget => {
                const [located, bounds] = widget.compute_bounds(ui.window);
                const rect = {label: widget.label, x: bounds.get_x(), y: bounds.get_y(), width: bounds.get_width(), height: bounds.get_height()};
                assert(located && rect.x >= 0 && rect.y >= 0 && rect.x + rect.width <= ui.window.get_width() && rect.y + rect.height <= ui.window.get_height(), 'footer action must fit visible window');
                return rect;
            });
            for (let a = 0; a < rectangles.length; a++) for (let b = a + 1; b < rectangles.length; b++) {
                const left = rectangles[a], right = rectangles[b];
                assert(left.x + left.width <= right.x || right.x + right.width <= left.x || left.y + left.height <= right.y || right.y + right.height <= left.y, 'footer actions must not overlap');
            }
            geometry.push({screen: name, actions: rectangles});
            const xid = run(['xdotool', 'search', '--onlyvisible', '--name', ui.window.title]).split('\n').at(-1);
            run(['import', '-window', xid, `${output}/${name}.png`]);
            if (name === 'review' && Number(width) === 620 && Number(height) === 670) {
                const adjustment = ui.scroll.get_vadjustment();
                const [located, bounds] = ui.heading.compute_bounds(ui.scroll);
                const layout = {contentUpper: adjustment.upper, viewportSize: adjustment.page_size,
                    scrollValue: adjustment.value, heading: {located, x: bounds.get_x(), y: bounds.get_y(), width: bounds.get_width(), height: bounds.get_height()},
                    viewport: {width: ui.scroll.get_width(), height: ui.scroll.get_height()}};
                GLib.file_set_contents(`${output}/review-viewport.json`, JSON.stringify(layout, null, 2));
                assert(adjustment.upper <= adjustment.page_size + 1, `reference review content must fit without scrolling: ${JSON.stringify(layout)}`);
                assert(located && layout.heading.x >= 0 && layout.heading.y >= 0 && layout.heading.x + layout.heading.width <= layout.viewport.width && layout.heading.y + layout.heading.height <= layout.viewport.height,
                    `reference review heading remains visible after keyboard navigation: ${JSON.stringify(layout)}`);
            }
            return xid;
        }
        let ui = await open('stopped');
        assert(ui.applicationCards.size === 4, 'all application cards exist');
        for (const card of ui.applicationCards.values()) {
            assert(card.select instanceof Gtk.CheckButton, 'selection is a native checkbox');
            assert(card.gear.tooltip_text?.length > 0, 'settings button has accessible description');
        }
        const xid = await capture(ui, 'applications-stopped');
        run(['xdotool', 'windowfocus', xid, 'key', 'Tab']); await delay(50);
        assert(ui.window.get_focus() !== null, 'Tab must reach a native focus target');
        const firstFocus = ui.window.get_focus();
        run(['xdotool', 'key', 'Tab']); await delay(50);
        assert(ui.window.get_focus() !== null && ui.window.get_focus() !== firstFocus, 'Tab advances to a different native focus target');
        await capture(ui, 'applications-keyboard-focus');
        ui.applicationCards.get('comfyui').select.active = true;
        ui.applicationCards.get('ollama').select.active = true;
        await capture(ui, 'applications-selected');
        await tabTo(ui, ui.review);
        await inspect('applications', 'applications-continue', 'Continue');
        await activate(ui, ui.review, 'Choose models');
        await inspect('models', 'models-arrival', 'Choose models');
        for (const label of ['Example small', 'Example large']) {
            const model = byLabel(ui.window, label);
            assert(model && model.get_mapped(), 'native model choice is visible');
            await tabTo(ui, model); run(['xdotool', 'key', 'space']); await delay(40);
            assert(model.active, 'keyboard selects an existing model');
        }
        const chooser = widgets(ui.window).find(widget => widget instanceof Gtk.Button && widget.label === 'Choose another model…' && widget.get_mapped());
        assert(chooser, 'native model chooser disclosure exists');
        await tabTo(ui, chooser);
        await inspect('models', 'models-chooser-closed', 'Choose another model…', 'closed');
        run(['xdotool', 'key', 'space']); await delay(80);
        await inspect('models', 'models-chooser-open', 'Choose another model…', 'open');
        await capture(ui, 'models-chooser-open');
        run(['xdotool', 'key', 'space']); await delay(80);
        await inspect('models', 'models-chooser-recollapsed', 'Choose another model…', 'closed');
        await tabTo(ui, ui.review);
        await inspect('models', 'models-continue', 'Continue');
        await capture(ui, 'models');
        await activate(ui, ui.review, 'Ready to finish');
        await inspect('review', 'review-arrival', 'Ready to finish');
        assert(ui.apply.sensitive, 'review unlocks final confirmation');
        assert(calls.filter(action => action === 'prepare').length === 3, 'review prepares three selected workloads');
        await tabTo(ui, ui.apply);
        await inspect('review', 'review-finish', 'Finish setup');
        await capture(ui, 'review');
        await tabTo(ui, ui.back);
        await inspect('review', 'review-back', 'Back');
        await activate(ui, ui.back, 'Choose models');
        await inspect('models', 'models-return', 'Choose models');
        await closeAndVerify(ui, 'stopped');
        for (const state of ['missing', 'error']) {
            ui = await open(state);
            assert(!widgets(ui.window).some(widget => widget instanceof Adw.ExpanderRow && widget.visible && widget.expanded), `${state}: technical detail remains collapsed`);
            await capture(ui, `applications-${state}`); await closeAndVerify(ui, state);
        }
        const report = {theme, width: Number(width), height: Number(height), scale: Number(GLib.getenv('GDK_SCALE') || 1),
            dark: style.dark, highContrast: style.high_contrast, calls, geometry, lifetime, criticalLogs,
            gtk: `${Gtk.get_major_version()}.${Gtk.get_minor_version()}.${Gtk.get_micro_version()}`,
            adwaita: `${Adw.get_major_version()}.${Adw.get_minor_version()}.${Adw.get_micro_version()}`};
        GLib.file_set_contents(`${output}/report.json`, JSON.stringify(report, null, 2));
        if (theme === 'highcontrast') assert(style.high_contrast, 'native high contrast preference must be active');
        assert(criticalLogs.length === 0, `native GTK criticals: ${JSON.stringify(criticalLogs)}`);
        print(`PASS ${theme} ${width}x${height} scale ${report.scale}`);
    })().catch(error => { failed = true; printerr(`${error.message}\n${error.stack}`); }).finally(() => { app.release(); app.quit(); });
});
app.run([]);
if (failed) imports.system.exit(1);
