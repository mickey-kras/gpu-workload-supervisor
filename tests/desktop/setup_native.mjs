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
    (async () => {
        const style = Adw.StyleManager.get_default();
        style.set_color_scheme(theme === 'dark' ? Adw.ColorScheme.FORCE_DARK : Adw.ColorScheme.FORCE_LIGHT);
        async function open(nextMode) {
            mode = nextMode;
            const ui = createSetupWindow({app, command});
            ui.window.set_default_size(Number(width), Number(height));
            await ui.initialized; await delay(250);
            const actualWidth = ui.window.get_width(), actualHeight = ui.window.get_height();
            const startup = {expected: {width: Number(width), height: Number(height)},
                actual: {width: actualWidth, height: actualHeight},
                minimumHorizontal: ui.window.measure(Gtk.Orientation.HORIZONTAL, -1),
                minimumVertical: ui.window.measure(Gtk.Orientation.VERTICAL, actualWidth)};
            GLib.file_set_contents(`${output}/startup-${nextMode}-geometry.json`, JSON.stringify(startup, null, 2));
            const startupXid = run(['xdotool', 'search', '--onlyvisible', '--name', ui.window.title]).split('\n').at(-1);
            run(['import', '-window', startupXid, `${output}/startup-${nextMode}.png`]);
            assert(actualWidth === Number(width), `window width exceeds supported size: expected ${width}, actual ${actualWidth}; geometry=${JSON.stringify(startup)}`);
            assert(actualHeight === Number(height), `window height exceeds supported size: expected ${height}, actual ${actualHeight}; geometry=${JSON.stringify(startup)}`);
            return ui;
        }
        const geometry = [];
        async function inspect(screen, name, focus) {
            await runAsync(['/usr/bin/python3', 'tests/desktop/setup_accessibility.py', `${output}/accessibility-${name}.json`, screen, focus]);
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
            assert(ownsFocus(ui, ui.heading), 'screen transition focuses its heading');
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
        ui.window.destroy();
        for (const state of ['missing', 'error']) {
            ui = await open(state);
            assert(!widgets(ui.window).some(widget => widget instanceof Adw.ExpanderRow && widget.visible && widget.expanded), `${state}: technical detail remains collapsed`);
            await capture(ui, `applications-${state}`); ui.window.destroy();
        }
        const report = {theme, width: Number(width), height: Number(height), scale: Number(GLib.getenv('GDK_SCALE') || 1),
            dark: style.dark, highContrast: style.high_contrast, calls, geometry,
            gtk: `${Gtk.get_major_version()}.${Gtk.get_minor_version()}.${Gtk.get_micro_version()}`,
            adwaita: `${Adw.get_major_version()}.${Adw.get_minor_version()}.${Adw.get_micro_version()}`};
        GLib.file_set_contents(`${output}/report.json`, JSON.stringify(report, null, 2));
        if (theme === 'highcontrast') assert(style.high_contrast, 'native high contrast preference must be active');
        print(`PASS ${theme} ${width}x${height} scale ${report.scale}`);
    })().catch(error => { failed = true; printerr(`${error.message}\n${error.stack}`); }).finally(() => app.quit());
});
app.run([]);
if (failed) imports.system.exit(1);
