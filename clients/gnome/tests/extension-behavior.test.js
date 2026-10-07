import test from 'node:test';
import assert from 'node:assert/strict';
import { loadGjsModule } from './gjs-modules.js';

class Item {
    constructor(text) { this.label = { text }; }
    setSensitive(value) { this.sensitive = value; }
    setOrnament(value) { this.ornament = value; }
    destroy() { this.destroyed = true; }
}
class Menu {
    items = [];
    addMenuItem(item) { this.items.push(item); }
    addAction(text, action) {
        const item = new Item(text);
        item.action = action;
        this.items.push(item);
        return item;
    }
    removeAll() { this.items = []; }
    setHeader() {}
    close() { this.closed = true; }
}
class Toggle extends Item {
    menu = new Menu();
    connect(signal, callback) { this.click = callback; }
}
class Indicator extends Item {
    quickSettingsItems = [];
}
class SubMenu extends Item {
    menu = new Menu();
}
class Dialog extends Item {
    contentLayout = { add_child() {} };
    setButtons(buttons) { this.buttons = buttons; }
    connect(signal, callback) { this.closed = callback; }
    open() { return true; }
    close() { this.closed(); }
}
function Label(properties) { return { ...properties, clutter_text: {} }; }
function status(owner = 'supervisor', activeWorkload = 'render') {
    return {
        owner, activeWorkload, desiredWorkload: activeWorkload,
        phase: 'stable', health: 'healthy', admission: 'closed',
        observedAt: '2026-10-04T00:00:00Z',
        expected: { incarnation: 'a', version: '1', owner, configurationRevision: 'c1' },
        workloads: [{ id: 'idle', label: 'Idle' }, { id: 'render', label: 'Rendering' }],
        capabilities: { takeControl: owner === 'supervisor', returnControl: owner === 'user', userSwitch: owner === 'user', idlePolicyConfigurable: false },
        idlePolicy: { timeoutMinutes: 0 },
    };
}
const settle = () => new Promise(resolve => setImmediate(resolve));
async function shell() {
    let time = 0;
    let next = 1;
    const timers = new Map();
    const launches = [];
    const session = {
        allowSettings: true,
        connect(signal, callback) { this.update = callback; return 7; },
        disconnect(id) { this.disconnected = id; },
    };
    const GLib = {
        PRIORITY_DEFAULT: 0, SOURCE_REMOVE: false, SOURCE_CONTINUE: true,
        get_monotonic_time: () => time * 1000,
        timeout_add(priority, delay, callback) {
            const id = next++;
            timers.set(id, { delay, callback });
            return id;
        },
        timeout_add_seconds(priority, seconds, callback) {
            return this.timeout_add(priority, seconds * 1000, callback);
        },
        source_remove(id) { timers.delete(id); },
    };
    const Gio = {
        Cancellable: class { cancel() {} },
        SubprocessFlags: { STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_SILENCE: 4 },
        Subprocess: { new() { throw new Error('Operator unavailable'); } },
        DesktopAppInfo: { new: () => ({ launch: (args, context) => launches.push(context) }) },
    };
    const native = {
        'gi://Gio': { default: Gio },
        'gi://GLib': { default: GLib },
        'gi://GObject': { default: { registerClass: cls => cls } },
        'gi://Clutter': { default: { KEY_Escape: 65307 } },
        'gi://St': { default: { Label } },
        'resource:///org/gnome/shell/extensions/extension.js': { Extension: class {} },
        'resource:///org/gnome/shell/ui/main.js': {
            sessionMode: session,
            panel: { statusArea: { quickSettings: { addExternalIndicator() {} } } },
        },
        'resource:///org/gnome/shell/ui/popupMenu.js': {
            PopupMenuSection: Menu, PopupSeparatorMenuItem: Item,
            PopupSubMenuMenuItem: SubMenu, PopupMenuItem: Item,
            Ornament: { DOT: 'dot', NONE: 'none' },
        },
        'resource:///org/gnome/shell/ui/quickSettings.js': { QuickMenuToggle: Toggle, SystemIndicator: Indicator },
        'resource:///org/gnome/shell/ui/modalDialog.js': { ModalDialog: Dialog },
    };
    const { default: Extension } = await loadGjsModule('extension.js', native, {
        global: { create_app_launch_context: () => 'launch-context' },
    });
    const extension = new Extension();
    extension.enable();
    await settle();
    return { extension, timers, session, launches, advance: ms => { time += ms; } };
}

test('extension renders committed ownership, catalog actions, freshness and setup visibility', async () => {
    const { extension: e, timers, session, launches, advance } = await shell();
    assert.equal(e._toggle.checked, false);
    assert.match(e._toggle.subtitle, /Unavailable/);
    assert.equal(e._detailRows[0].label.text, 'No accepted observation');
    e.ownership();
    assert.equal(e._dialog, null);
    e._setup.action();
    assert.deepEqual(launches, ['launch-context']);
    session.allowSettings = false;
    session.update();
    assert.equal(e._setup.visible, false);
    const requests = [];
    e._transport.call = async request => {
        requests.push(request);
        return { code: 'ok', status: status('user', request.target ?? 'render') };
    };
    await e._call('status');
    assert.equal(e._toggle.subtitle, 'Manual · Rendering');
    assert.equal(e._toggle.checked, true);
    assert.equal(e._workloadItems[1].item.ornament, 'dot');
    assert.equal(e._workloadItems[1].item.sensitive, false);
    e._workloadItems[1].item.action();
    assert.equal(requests.length, 1);
    e._workloadItems[0].item.action();
    assert.equal(e._toggle.subtitle, 'Request pending…');
    await settle();
    assert.equal(requests.at(-1).action, 'user-switch');
    assert.equal(e._toggle.subtitle, 'Manual · Idle');
    const rows = e._detailRows;
    e._render();
    assert.equal(e._detailRows, rows);
    advance(30000);
    assert.equal(timers.get(e._ageTimer).callback(), true);
    assert.equal(e._toggle.subtitle, 'Status is stale');
    assert.equal(e._workloadItems[1].item.sensitive, false);
    e._model.error = 'unrecognized';
    e._render();
    assert.equal(e._toggle.subtitle, 'Unavailable');
    e._refresh.action();
    await settle();
    const poll = timers.get(e._timer);
    assert.equal(poll.callback(), false);
    await settle();
    const toggle = e._toggle;
    const indicator = e._indicator;
    e.disable();
    assert.equal(toggle.destroyed, true);
    assert.equal(indicator.destroyed, true);
    assert.equal(session.disconnected, 7);
    assert.equal(e._model, null);
    e.disable();
});

test('ownership confirmation commits only fresh decisions and refreshes stale prompts', async () => {
    const { extension: e, advance } = await shell();
    const actions = [];
    e._transport.call = async request => {
        actions.push(request.action);
        const owner = request.action === 'take-control' ? 'user' : 'supervisor';
        return { code: 'ok', status: status(owner) };
    };
    await e._call('status');
    assert.equal(e._toggle.subtitle, 'Supervisor · Rendering');
    e._toggle.click();
    const taking = e._dialog;
    e.ownership();
    assert.equal(e._dialog, taking);
    assert.equal(e._toggle.checked, false);
    taking.buttons[1].action();
    await settle();
    assert.equal(actions.at(-1), 'take-control');
    assert.equal(e._toggle.checked, true);
    e.ownership();
    const returning = e._dialog;
    assert.equal(returning.buttons[1].label, 'Stop and Return');
    advance(30000);
    returning.buttons[1].action();
    await settle();
    assert.equal(actions.at(-1), 'status');
    e.ownership();
    const disabledPrompt = e._dialog;
    e.disable();
    disabledPrompt.buttons[1].action();
    await settle();
    assert.equal(disabledPrompt.destroyed, true);
    assert.equal(actions.at(-1), 'status');
});

test('pending requests cannot replay and late completion after disable cannot schedule polling', async () => {
    const { extension: e, timers } = await shell();
    let complete;
    let calls = 0;
    e._transport.call = () => {
        calls++;
        return new Promise(resolve => { complete = resolve; });
    };
    const pending = e._call('status');
    await e._call('status');
    assert.equal(calls, 1);
    const model = e._model;
    const age = timers.get(e._ageTimer);
    e.disable();
    assert.equal(age.callback(), false);
    complete({ code: 'ok', status: status('user') });
    await pending;
    assert.equal(model.status, null);
    assert.equal(timers.size, 0);
    await e._call('status');
    assert.equal(calls, 1);
});
