// Render production GTK widgets with injected transport; consented temporary
// operations are simulated and recorded without invoking any real service.
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
let installationReply = null;
let deferDiscovery = false;
let temporaryReply = null;
let cleanupFails = false;
const commandRecords = [];
const longUnit = 'comfyui-production-rendering-installation-with-long-service-identity.service';
const longLocation = `/home/example/.config/systemd/user/${longUnit}`;
const longEndpoint = 'http://127.0.0.1:18188';
const serviceIdentity = `Service: ${longUnit} · ${longLocation}`;
const longIdentity = `${serviceIdentity} · ${longEndpoint}`;
const diagnosticEvidence = 'ExecStart: /home/example/Applications/ComfyUI-production-rendering-environment/bin/python /home/example/Applications/ComfyUI-production-rendering-environment/main.py --listen 127.0.0.1 --port 18188';

const candidate = {app: 'ollama', label: 'Ollama', unit: 'example-model.service',
    endpoint: 'http://127.0.0.1:11434', cgroup: '/example/model', location: '/example/model.service',
    recognized: true, configurationStatus: 'ready', instanceStatus: 'not-running', inventoryStatus: 'available',
    models: [{id: 'example-small', label: 'Example small'}, {id: 'example-large', label: 'Example large'}],
    binding: {unit: 'example-model.service', cgroup: '/example/model', healthURL: 'http://127.0.0.1:11434/api/tags', launchFile: '/example/model.service'}};
const comfy = {...candidate, app: 'comfyui', label: 'ComfyUI', unit: 'example-image.service', models: [],
    binding: {...candidate.binding, unit: 'example-image.service', model: ''}};
function stateCandidate(state) {
    const result = {...comfy, unit: longUnit, location: longLocation, endpoint: longEndpoint,
        binding: {...comfy.binding, unit: longUnit, launchFile: longLocation, healthURL: `${longEndpoint}/system_stats`},
        evidence: [diagnosticEvidence], nextStep: 'Inspect the existing service. Its files have not been changed.'};
    if (state === 'running') result.instanceStatus = 'available';
    // Failed service parsing cannot supply endpoint or verified launch binding.
    if (['inspection-failed', 'unsupported-home'].includes(state)) Object.assign(result, {endpoint: undefined, binding: undefined, cgroup: undefined});
    if (state === 'inspection-failed') Object.assign(result, {recognized: false, configurationStatus: 'inspection-failed', instanceStatus: 'inspection-failed'});
    if (state === 'unsupported-home') Object.assign(result, {recognized: false, configurationStatus: 'unsupported', instanceStatus: 'unsupported', reference: longUnit, referenceKind: 'configuration', nextStep: 'HOME uses an unsupported expansion or whitespace. Choose a trusted installation path.'});
    if (state === 'unsupported-trust') Object.assign(result, {recognized: false, configurationStatus: 'unsupported', instanceStatus: 'unsupported',
        evidence: ['Untrusted path: /home/example/shared-applications/ComfyUI/main.py is writable by another user.', diagnosticEvidence]});
    if (state === 'missing-location') Object.assign(result, {recognized: false, configurationStatus: 'missing', instanceStatus: 'missing'});
    if (state === 'unreachable') Object.assign(result, {unit: undefined, location: undefined, binding: undefined, recognized: false, configurationStatus: 'unverified', instanceStatus: 'unreachable'});
    return result;
}
async function command(argv, input, operation) {
    const action = argv[1]; calls.push(action);
    commandRecords.push({action, input: input ? JSON.parse(input) : null});
    assert(!['apply', 'render-owned', 'temporary-start'].includes(action), `unexpected mutation: ${action}`);
    if (action === '--version') return 'GNOME Shell 50.1';
    if (action === 'discover') {
        const discovery = {request: {profile: {statePath: '/example/state.db', gpuIndex: 0}, catalog: {version: 1, profiles: []}}, units: [], applications: mode === 'missing' ? [] : mode === 'temporary' ? [{...candidate, configurationStatus: 'model-required', models: []}] : ['ready', 'running', 'missing-location', 'unreachable', 'unsupported-trust', 'unsupported-home', 'inspection-failed'].includes(mode) ? [stateCandidate(mode)] : [comfy, candidate], errors: mode === 'error' ? ['Example discovery unavailable.'] : []};
        if (deferDiscovery) {
            deferDiscovery = false;
            return await new Promise(resolve => { installationReply = result => { installationReply = null; resolve(JSON.stringify({...discovery, applications: [result]})); }; });
        }
        return JSON.stringify(discovery);
    }
    if (action === 'temporary-status') return JSON.stringify(mode === 'temporary' ? {available: true, expected: {fixture: 'native-read-only'}} : {available: false});
    if (action === 'probe') return await new Promise(resolve => { deferDiscovery = false; installationReply = result => { installationReply = null; resolve(JSON.stringify(result)); }; });
    if (action === 'temporary-discover') {
        const request = JSON.parse(input);
        assert(request.consent === true && request.externalControlPaused === true && request.unit === candidate.unit, 'temporary fixture requires explicit consent for the selected service');
        return await new Promise(resolve => {
            temporaryReply = result => { temporaryReply = null; resolve(JSON.stringify(result)); };
            operation.cancel = () => temporaryReply?.({session: {id: 'native-session', token: 'fixture-token', status: 'cleanup_required'}, error: 'Cancelled temporary check'});
        });
    }
    if (action === 'temporary-cleanup') {
        const request = JSON.parse(input);
        assert(request.id === 'native-session' && request.token === 'fixture-token' && request.externalControlPaused, 'cleanup must retain exact consented session identity');
        return JSON.stringify({session: {id: request.id, token: request.token, status: cleanupFails ? 'cleanup_required' : 'completed'}, ...(cleanupFails ? {error: 'Fixture cleanup failed; application stopped state is unconfirmed.'} : {})});
    }
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
        const screenshots = [];
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
        async function waitFor(observe, message) {
            for (let count = 0; count < 100; count++) {
                const observed = observe();
                if (observed) return observed;
                await delay(30);
            }
            throw new Error(message);
        }
        function visibleChooser(ui) {
            const windows = Gtk.Window.get_toplevels();
            for (let index = 0; index < windows.get_n_items(); index++) {
                const window = windows.get_item(index);
                if (window !== ui.window && window.get_transient_for() === ui.window && window.get_mapped()) return window;
            }
            return null;
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
            const activeHeading = ui.applicationSettings && ui.settingsTitle ? ui.settingsTitle : ui.heading;
            const diagnostics = {expectedHeading: heading, actualHeading: ui.heading.label,
                headingFocusable: activeHeading.get_focusable(), headingMapped: activeHeading.get_mapped(),
                headingSelection: activeHeading.get_selectable() ? activeHeading.get_selection_bounds() : [false, 0, 0],
                currentFocus: focus ? {widget: focus.constructor.name, label: focus.label ?? null, name: focus.get_name()} : null};
            GLib.file_set_contents(`${output}/transition-${transition}-focus.json`, JSON.stringify(diagnostics, null, 2));
            await runAsync(['/usr/bin/python3', 'tests/desktop/setup_accessibility.py', `${output}/accessibility-transition-${transition}.json`, 'diagnostic']);
            assert(ownsFocus(ui, activeHeading), `screen transition focuses its visible heading: ${JSON.stringify(diagnostics)}`);
            assert(!diagnostics.headingSelection[0], `transition heading must not select its text: ${JSON.stringify(diagnostics)}`);
        }
        async function capture(ui, name) {
            await delay(180);
            const footer = [ui.review, ui.apply, ui.back, ui.later].filter(widget => widget.get_mapped());
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
            screenshots.push(name);
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
        function named(ui, label) {
            const found = widgets(ui.window).find(widget => widget instanceof Gtk.Button && widget.get_mapped() &&
                (widget.label === label || widgets(widget).some(child => child instanceof Gtk.Label && child.label === label)));
            assert(found, `missing mapped native action: ${label}`); return found;
        }
        async function settingsTree(ui, name, focus, required, absent = [], requiredContains = []) {
            const contract = `${output}/contract-${name}.json`;
            const sensitivity = required.filter(expected => 'sensitive' in expected).map(expected => {
                const widget = named(ui, expected.name);
                const actual = {name: expected.name, expected: expected.sensitive,
                    propertySensitive: widget.get_sensitive(), effectiveSensitive: widget.is_sensitive()};
                return actual;
            });
            GLib.file_set_contents(`${output}/gtk-availability-${name}.json`, JSON.stringify(sensitivity, null, 2));
            for (const actual of sensitivity)
                assert(actual.effectiveSensitive === actual.expected, `wrong production GTK action sensitivity: ${JSON.stringify(actual)}`);
            GLib.file_set_contents(contract, JSON.stringify({required, absent, requiredContains}));
            await runAsync(['/usr/bin/python3', 'tests/desktop/setup_accessibility.py', `${output}/accessibility-${name}.json`, 'settings', focus, contract]);
        }
        function button(name, sensitive = true) { return {name, role: 'push button', sensitive}; }
        function wrapping(ui, label) {
            const native = widgets(ui.window).find(widget => widget instanceof Gtk.Label && widget.get_mapped() && widget.label === label);
            assert(native?.wrap && native.lines === -1 && native.ellipsize === 0, `identity/diagnostic must wrap fully without ellipsis: ${label}`);
            const [located, bounds] = native.compute_bounds(ui.window);
            assert(located && bounds.get_x() >= 0 && bounds.get_x() + bounds.get_width() <= ui.window.get_width(), 'wrapped text stays inside the native window horizontally');
            geometry.push({screen: ui.heading.label, text: label, x: bounds.get_x(), y: bounds.get_y(), width: bounds.get_width(), height: bounds.get_height(), wrapped: true});
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
        const settingsGear = ui.applicationCards.get('ollama').gear;
        await tabTo(ui, settingsGear);
        const settingsScroll = ui.scroll.get_vadjustment().value;
        await activate(ui, settingsGear, 'Ollama');
        assert(ui.review.label === 'Use installation' && ui.later.label === 'Cancel', 'main settings uses fixed confirmation and cancel actions');
        assert(!ui.applicationGroup.get_mapped(), 'settings hides unrelated application cards');
        const selectedBeforeCancel = JSON.stringify(ui.drafts);
        await activate(ui, named(ui, 'Change installation…'), 'Change installation');
        const installedPicker = named(ui, 'Choose installed executable…');
        await tabTo(ui, installedPicker); run(['xdotool', 'key', 'space']);
        const chooserWindow = await waitFor(() => visibleChooser(ui), 'native executable chooser must open before cancellation');
        const chooserXid = run(['xdotool', 'search', '--onlyvisible', '--name', chooserWindow.title]).split('\n').at(-1);
        run(['xdotool', 'windowfocus', chooserXid, 'key', 'Escape']);
        await waitFor(() => !chooserWindow.get_visible(), 'Escape dismisses the actual native chooser');
        run(['xdotool', 'windowfocus', xid]);
        await waitFor(() => run(['xdotool', 'getwindowfocus']) === xid, 'native focus returns after chooser dismissal');
        assert(ownsFocus(ui, installedPicker), 'cancelled chooser retains originating picker focus');
        assert(JSON.stringify(ui.drafts) === selectedBeforeCancel, 'chooser cancellation preserves service and model selections');
        await capture(ui, 'installation-selection');
        await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']); await delay(100);
        assert(ui.heading.label === 'Ollama' && ownsFocus(ui, named(ui, 'Change installation…')), 'Done restores main settings and originating navigation focus');
        await capture(ui, 'application-settings');
        await tabTo(ui, ui.later); run(['xdotool', 'key', 'space']); await delay(150);
        assert(ui.heading.label === 'Choose your applications', 'Cancel restores the originating screen');
        assert(ownsFocus(ui, settingsGear), 'Cancel restores the originating gear focus');
        assert(Math.abs(ui.scroll.get_vadjustment().value - settingsScroll) < 1, 'Cancel restores originating scroll position');
        assert(ui.applicationCards.get('ollama').select.active, 'Cancel retains the selected application');
        assert(JSON.stringify(ui.drafts) === selectedBeforeCancel, 'Cancel retains selected service and model');
        await capture(ui, 'application-settings-return');
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
        const settingsStates = ['ready', 'running', 'missing-location', 'unreachable', 'unsupported-trust', 'unsupported-home', 'inspection-failed'];
        for (const state of settingsStates) {
            ui = await open(state);
            const gear = ui.applicationCards.get('comfyui').gear;
            await activate(ui, gear, 'ComfyUI');
            const ready = state === 'ready' || state === 'running';
            const identity = state === 'unreachable' ? `Address: ${longEndpoint}` : ['inspection-failed', 'unsupported-home'].includes(state) ? serviceIdentity : longIdentity;
            wrapping(ui, identity);
            assert(ui.settingsTitle.get_mapped() && !ui.heading.get_mapped() && ui.settingsBack.get_mapped(), 'settings uses the visible centered header title and native header Back');
            const notice = widgets(ui.window).filter(widget => widget.get_mapped() && widget.has_css_class('setup-status'));
            assert(notice.length === 1, 'main settings uses one grouped configuration notice');
            const noticeIcon = widgets(notice[0]).find(widget => widget instanceof Gtk.Image);
            assert(noticeIcon?.icon_name === (ready ? 'emblem-ok-symbolic' : 'dialog-warning-symbolic'), 'configuration notice uses native success/warning symbol');
            if (!ready) assert(widgets(notice[0]).includes(named(ui, 'View details')), 'View details belongs to the warning card');
            await settingsTree(ui, `settings-${state}`, 'ComfyUI', [{name: identity}, button('Use installation', ready), button('Cancel')],
                ['Display name', 'Health endpoint', 'Measured VRAM requirement (MiB; optional)', 'Copy details'],
                ready ? ['Ready for setup', 'GPU operation has not been tested.'] : state === 'unsupported-home' ? ['HOME uses an unsupported format.'] : []);
            await capture(ui, `settings-${state}`);
            if (state === 'ready') {
                await capture(ui, 'settings-stopped');
                await activate(ui, named(ui, 'Change installation…'), 'Change installation');
                const choice = `ComfyUI · ${longIdentity} · stopped`;
                await settingsTree(ui, 'installation-long-identity', 'Change installation', [button(choice), button('Done'), button('Cancel')]);
                wrapping(ui, choice);
                await capture(ui, 'settings-installation-long-identity');
                await tabTo(ui, named(ui, choice)); run(['xdotool', 'key', 'space']); await delay(80);
                assert(ui.heading.label === 'ComfyUI' && ui.review.sensitive, 'keyboard selection returns to the verified long-identity installation');
            }
            if (!ready) {
                await activate(ui, named(ui, 'View details'), 'Configuration details');
                await settingsTree(ui, `configuration-${state}`, 'Configuration details', [button('Copy details'), button('Check again'), button('Cancel')],
                    ['Display name', 'Measured VRAM requirement (MiB; optional)'], state === 'unsupported-trust' ? ['Untrusted path:', diagnosticEvidence] : [state === 'unreachable' ? 'Address unreachable.' : diagnosticEvidence]);
                const detail = widgets(ui.window).find(widget => widget instanceof Gtk.Label && widget.get_mapped() && widget.has_css_class('monospace'));
                assert(detail, 'configuration diagnostics use actual selectable GTK text');
                wrapping(ui, detail.label);
                await capture(ui, `configuration-${state}`);
                ui.scroll.get_vadjustment().value = ui.scroll.get_vadjustment().upper;
                await capture(ui, `configuration-${state}-bottom`);
                await tabTo(ui, named(ui, 'Copy details')); run(['xdotool', 'key', 'space']);
                const clipboard = ui.window.get_display().get_clipboard();
                const copied = await new Promise((resolve, reject) => clipboard.read_text_async(null, (source, result) => {
                    try { resolve(source.read_text_finish(result)); } catch (error) { reject(error); }
                }));
                assert(copied.includes(state === 'unreachable' ? longEndpoint : longUnit) && copied.includes(state === 'unreachable' ? 'Address unreachable.' : diagnosticEvidence), 'Copy details retains full installation identity and diagnostics');
                if (state === 'unsupported-home') {
                    const probesBeforeRepair = calls.filter(action => action === 'probe').length;
                    deferDiscovery = true;
                    await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']);
                    await waitFor(() => installationReply, 'service reinspection starts for repaired HOME');
                    installationReply(stateCandidate('ready'));
                    await waitFor(() => ui.review.sensitive, 'unit-only repaired configuration inspection completes');
                    assert(calls.filter(action => action === 'probe').length === probesBeforeRepair, 'relative service reference cannot fabricate a path or endpoint probe target');
                    await tabTo(ui, ui.settingsBack); run(['xdotool', 'key', 'space']); await delay(80);
                    assert(ui.heading.label === 'ComfyUI' && ui.review.sensitive, 'fresh service metadata restores ready installation after unsupported HOME is repaired');
                    await capture(ui, 'settings-rechecked-ready');
                }
            }
            await closeAndVerify(ui, state);
        }
        ui = await open('ready');
        await activate(ui, ui.applicationCards.get('comfyui').gear, 'ComfyUI');
        const beforeDraft = JSON.stringify(ui.drafts);
        const settingsEditor = ui.applicationSettings.editor;
        const beforeOverrides = JSON.stringify(settingsEditor.settingsOverrides());
        await activate(ui, named(ui, 'Advanced settings'), 'Advanced settings');
        await settingsTree(ui, 'settings-advanced', 'Advanced settings', [button('Resource checks'), button('Launch details'), button('Review changes'), button('Cancel')], ['Copy details']);
        const displayName = widgets(ui.window).find(widget => widget instanceof Adw.EntryRow && widget.title === 'Display name' && widget.get_mapped());
        assert(displayName, 'display name is editable on Advanced settings');
        await tabTo(ui, displayName); run(['xdotool', 'key', 'ctrl+a']); run(['xdotool', 'type', '--clearmodifiers', 'Draft ComfyUI name']); await delay(80);
        assert(JSON.stringify(ui.drafts) !== beforeDraft, 'native keyboard edit updates draft');
        await capture(ui, 'settings-advanced-edited');
        for (const [navigation, heading, screenshot] of [['Resource checks', 'Resource checks', 'settings-resource-checks'], ['Launch details', 'Launch details', 'settings-launch-details']]) {
            const origin = named(ui, navigation);
            await activate(ui, origin, heading);
            await settingsTree(ui, screenshot, heading, [button('Done'), button('Cancel')], ['Copy details']);
            if (heading === 'Resource checks') {
                const capacity = widgets(ui.window).find(widget => widget instanceof Adw.EntryRow && widget.title === 'Measured VRAM requirement (MiB; optional)' && widget.get_mapped());
                assert(capacity, 'resource requirement is a native optional entry');
                await tabTo(ui, capacity); run(['xdotool', 'key', 'ctrl+a']); run(['xdotool', 'type', '--clearmodifiers', '512']); await delay(60);
                const retain = byLabel(ui.window, 'Keep this workload running at login if already active');
                await tabTo(ui, retain); run(['xdotool', 'key', 'space']); await delay(40);
                assert(settingsEditor.settingsOverrides().requiredMiB === 512 && settingsEditor.settingsOverrides().bootPolicy === 'retain', 'native resource edits stage measured requirement and boot policy');
            }
            await capture(ui, screenshot);
            await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']); await delay(100);
            assert(ui.heading.label === 'Advanced settings' && ownsFocus(ui, origin), `${heading} Done restores originating navigation focus`);
        }
        await tabTo(ui, ui.later); run(['xdotool', 'key', 'space']); await delay(120);
        assert(ui.heading.label === 'Choose your applications' && JSON.stringify(ui.drafts) === beforeDraft && JSON.stringify(settingsEditor.settingsOverrides()) === beforeOverrides, 'Cancel discards native advanced draft and resource edits');
        await capture(ui, 'settings-draft-cancelled');
        await closeAndVerify(ui, 'draft-cancel');

        ui = await open('inspection-failed');
        await activate(ui, ui.applicationCards.get('comfyui').gear, 'ComfyUI');
        await activate(ui, named(ui, 'View details'), 'Configuration details');
        const probesBeforeStale = calls.filter(action => action === 'probe').length;
        deferDiscovery = true;
        await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']);
        await waitFor(() => installationReply, 'Check again rechecks selected installation metadata');
        assert(!ui.review.sensitive, 'checking disables the fixed footer action');
        await tabTo(ui, ui.settingsTitle);
        await settingsTree(ui, 'configuration-checking', 'Configuration details', [button('Check again', false)], [], ['Checking']);
        await capture(ui, 'configuration-checking');
        await tabTo(ui, ui.settingsBack ?? ui.back); run(['xdotool', 'key', 'space']); await delay(100);
        assert(ui.heading.label === 'ComfyUI' && !ui.review.sensitive, 'pending check cannot enable Use installation after Back');
        await activate(ui, named(ui, 'Change installation…'), 'Change installation');
        const address = widgets(ui.window).find(widget => widget instanceof Adw.EntryRow && widget.title === 'Application address' && widget.get_mapped());
        assert(address, 'changed target is a native entry');
        await tabTo(ui, address); run(['xdotool', 'key', 'ctrl+a']); run(['xdotool', 'type', '--clearmodifiers', 'http://127.0.0.1:19999']); await delay(60);
        installationReply(stateCandidate('ready'));
        await waitFor(() => ui.review.sensitive, 'stale unit-only inspection ends without authorizing changed draft');
        assert(calls.filter(action => action === 'probe').length === probesBeforeStale, 'unit-only stale inspection retains its original target type');
        assert(ui.drafts.at(-1).endpoint === 'http://127.0.0.1:19999', 'late successful inspection cannot overwrite edited address');
        await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']); await delay(100);
        assert(ui.heading.label === 'ComfyUI' && !ui.review.sensitive, 'stale check cannot authorize the changed installation');
        await capture(ui, 'settings-stale-check');
        await closeAndVerify(ui, 'stale-check');

        ui = await open('unreachable');
        await activate(ui, ui.applicationCards.get('comfyui').gear, 'ComfyUI');
        await activate(ui, named(ui, 'View details'), 'Configuration details');
        await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']);
        await waitFor(() => installationReply, 'endpoint-only Check again probes its current address');
        assert(!ui.review.is_sensitive(), 'endpoint probe disables configuration action');
        await tabTo(ui, ui.settingsBack); run(['xdotool', 'key', 'space']); await delay(80);
        await activate(ui, named(ui, 'Change installation…'), 'Change installation');
        const changedAddress = widgets(ui.window).find(widget => widget instanceof Adw.EntryRow && widget.title === 'Application address' && widget.get_mapped());
        await tabTo(ui, changedAddress); run(['xdotool', 'key', 'ctrl+a']); run(['xdotool', 'type', '--clearmodifiers', 'http://127.0.0.1:19998']); await delay(60);
        installationReply(stateCandidate('ready'));
        await waitFor(() => ui.review.is_sensitive(), 'stale endpoint probe completes without committing its result');
        assert(ui.drafts.at(-1).endpoint === 'http://127.0.0.1:19998', 'late endpoint probe cannot overwrite edited address');
        await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']); await delay(80);
        assert(ui.heading.label === 'ComfyUI' && !ui.review.is_sensitive(), 'stale endpoint probe cannot authorize the changed installation');
        await capture(ui, 'settings-stale-endpoint-check');
        await closeAndVerify(ui, 'stale-endpoint-check');

        ui = await open('temporary');
        ui.applicationCards.get('ollama').select.active = true;
        await activate(ui, ui.review, 'Choose models');
        const consent = byLabel(ui.window, 'I allow this brief start and will keep other application controls paused.');
        const start = named(ui, 'Start Ollama briefly to list models');
        assert(consent?.get_mapped() && !start.sensitive, 'temporary model detection requires native explicit consent');
        await settingsTree(ui, 'temporary-without-consent', 'Choose models', [button('Start Ollama briefly to list models', false), {name: consent.label, role: 'check box'}]);
        await capture(ui, 'temporary-without-consent');
        await tabTo(ui, consent); run(['xdotool', 'key', 'space']); await delay(60);
        assert(start.sensitive, 'native consent enables temporary detection');
        await tabTo(ui, start); run(['xdotool', 'key', 'space']);
        await waitFor(() => temporaryReply, 'consented temporary check begins');
        await capture(ui, 'temporary-checking');
        cleanupFails = true;
        temporaryReply({session: {id: 'native-session', token: 'fixture-token', status: 'cleanup_required'}, error: 'Fixture previous stopped state not restored.'});
        await waitFor(() => start.label === 'Retry Ollama cleanup', 'failed cleanup offers explicit retry');
        await tabTo(ui, start);
        await settingsTree(ui, 'temporary-cleanup-required', 'Retry Ollama cleanup', [button('Retry Ollama cleanup')], [], ['previous stopped state']);
        await capture(ui, 'temporary-cleanup-required');
        const prepareCount = calls.filter(action => action === 'prepare').length;
        await tabTo(ui, ui.review); run(['xdotool', 'key', 'space']); await delay(80);
        assert(calls.filter(action => action === 'prepare').length === prepareCount && !ui.apply.sensitive, 'unrestored temporary check blocks configuration review');
        ui.window.close(); await delay(120);
        assert(ui.window.get_visible(), 'failed temporary cleanup vetoes closing the real native window');
        await capture(ui, 'temporary-close-blocked');
        cleanupFails = false;
        await tabTo(ui, start); run(['xdotool', 'key', 'space']);
        await waitFor(() => !ui.draftEditors.some(editor => editor.temporaryActive()), 'retry restores stopped state and clears cleanup block');
        assert(!consent.active && !start.sensitive, 'temporary cleanup resets consent');
        await capture(ui, 'temporary-cleanup-complete');
        await closeAndVerify(ui, 'temporary-cleanup');
        ui = await open('temporary');
        ui.applicationCards.get('ollama').select.active = true;
        await activate(ui, ui.review, 'Choose models');
        const cancelConsent = byLabel(ui.window, 'I allow this brief start and will keep other application controls paused.');
        await tabTo(ui, cancelConsent); run(['xdotool', 'key', 'space']); await delay(30);
        await tabTo(ui, named(ui, 'Start Ollama briefly to list models')); run(['xdotool', 'key', 'space']);
        await waitFor(() => temporaryReply, 'cancel-path temporary check begins');
        await tabTo(ui, named(ui, 'Cancel model detection')); run(['xdotool', 'key', 'space']);
        await waitFor(() => temporaryReply === null && !ui.draftEditors.some(editor => editor.temporaryActive()), 'native cancellation waits for stopped-state restoration');
        assert(!cancelConsent.active && !ui.drafts.at(-1).model, 'cancelled detection resets consent and cannot accept a late model inventory');
        await capture(ui, 'temporary-cancelled-restored');
        await closeAndVerify(ui, 'temporary-cancelled');
        const report = {theme, width: Number(width), height: Number(height), scale: Number(GLib.getenv('GDK_SCALE') || 1),
            dark: style.dark, highContrast: style.high_contrast, calls, commandRecords, geometry, screenshots, lifetime, criticalLogs,
            scope: 'Production GTK factory with injected generic backend; screenshots and accessibility only. No GNOME Shell, real service startup, or GPU operation qualified.',
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
