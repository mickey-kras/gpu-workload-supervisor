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
            assert(ui.window.get_width() === Number(width), 'window width exceeds supported size');
            assert(ui.window.get_height() === Number(height), 'window height exceeds supported size');
            return ui;
        }
        async function capture(ui, name) {
            await delay(180);
            const primary = ui.apply.visible ? ui.apply : ui.review;
            const [located, bounds] = primary.compute_bounds(ui.window);
            assert(located && bounds.get_x() >= 0 && bounds.get_y() >= 0 &&
                bounds.get_x() + bounds.get_width() <= ui.window.get_width() &&
                bounds.get_y() + bounds.get_height() <= ui.window.get_height(), 'primary action must fit the visible window');
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
        await runAsync(['/usr/bin/python3', 'tests/desktop/setup_accessibility.py', `${output}/accessibility.json`]);
        ui.applicationCards.get('comfyui').select.active = true;
        ui.applicationCards.get('ollama').select.active = true;
        await ui.continueSetup(); await delay(100);
        assert(ui.heading.label === 'Choose models', 'Continue reaches models screen');
        const model = byLabel(ui.window, 'Example small');
        assert(model && model.get_mapped(), 'native model choice is visible'); model.active = true;
        byLabel(ui.window, 'Example large').active = true;
        await capture(ui, 'models');
        await ui.continueSetup(); await delay(100);
        assert(ui.heading.label === 'Ready to finish', 'Continue reaches review screen');
        assert(ui.apply.sensitive, 'review unlocks final confirmation');
        assert(calls.filter(action => action === 'prepare').length === 3, 'review prepares three selected workloads');
        await capture(ui, 'review');
        ui.back.emit('clicked'); await delay(100);
        assert(ui.heading.label === 'Choose models', 'Back returns to model decision');
        ui.window.destroy();
        for (const state of ['missing', 'error']) {
            ui = await open(state);
            assert(!widgets(ui.window).some(widget => widget instanceof Adw.ExpanderRow && widget.visible && widget.expanded), `${state}: technical detail remains collapsed`);
            await capture(ui, `applications-${state}`); ui.window.destroy();
        }
        const report = {theme, width: Number(width), height: Number(height), scale: Number(GLib.getenv('GDK_SCALE') || 1),
            dark: style.dark, highContrast: style.high_contrast, calls,
            gtk: `${Gtk.get_major_version()}.${Gtk.get_minor_version()}.${Gtk.get_micro_version()}`,
            adwaita: `${Adw.get_major_version()}.${Adw.get_minor_version()}.${Adw.get_micro_version()}`};
        GLib.file_set_contents(`${output}/report.json`, JSON.stringify(report, null, 2));
        if (theme === 'highcontrast') assert(style.high_contrast, 'native high contrast preference must be active');
        print(`PASS ${theme} ${width}x${height} scale ${report.scale}`);
    })().catch(error => { failed = true; printerr(error.stack); }).finally(() => app.quit());
});
app.run([]);
if (failed) imports.system.exit(1);
