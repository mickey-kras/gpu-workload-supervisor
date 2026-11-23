// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import GObject from 'gi://GObject';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import { Extension } from 'resource:///org/gnome/shell/extensions/extension.js';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import * as PopupMenu from 'resource:///org/gnome/shell/ui/popupMenu.js';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import * as QuickSettings from 'resource:///org/gnome/shell/ui/quickSettings.js';
import { Model } from './model.js';
import { Transport } from './transport.js';
import { closeProcessDialog, ownershipDialog } from './dialogs.js';
import { CLOSE_ACTION } from './contract.js';
const now = () => GLib.get_monotonic_time() / 1000;
const messages = {
    unavailable: 'Unavailable — outcome may be uncertain',
    busy: 'GPU control is busy',
    stale_state: 'Status is stale — refresh required',
    wrong_owner: 'Ownership changed — refresh required',
    recovery_required: 'Recovery required — use operator tools',
    incompatible_configuration: 'Setup or activation required',
    unsupported_version: 'Incompatible operator version',
    invalid_request: 'Operator contract mismatch',
    timeout: 'Timed out — outcome uncertain',
    gpu_in_use: 'GPU in use — see menu',
    gpu_check_failed: 'GPU check failed — see menu',
    not_permitted: 'Not your process — not closed',
};
// The idle policy is global. Switching it on picks 60 minutes; the submenu
// offers this fixed list only.
const IDLE_TIMEOUTS = [15, 30, 60, 120, 240];
const IDLE_DEFAULT = 60;
const processName = (p) => p.name || 'unknown program';
function processRow(p) {
    const memory = p.usedMiB === null ? 'unknown memory' : `${p.usedMiB} MiB`;
    return `${processName(p)} · PID ${p.pid} · ${p.user || 'unknown user'} · ${memory}`;
}
const Toggle = GObject.registerClass(
    class Toggle extends QuickSettings.QuickMenuToggle {
        constructor(controller) {
            super({
                title: 'GPU Control',
                subtitle: 'Loading…',
                iconName: 'video-display-symbolic',
                toggleMode: false,
            });
            this.connect('clicked', () => controller.ownership());
            this.menu.setHeader('video-display-symbolic', 'GPU Control');
        }
    },
);
export default class GPUControl extends Extension {
    enable() {
        this._catalog = null;
        this._detailRows = null;
        this._workloadItems = [];
        this._model = new Model();
        this._transport = new Transport();
        this._alive = true;
        this._timer = 0;
        this._dialog = null;
        this._indicator = new QuickSettings.SystemIndicator();
        this._toggle = new Toggle(this);
        this._indicator.quickSettingsItems.push(this._toggle);
        this._workloads = new PopupMenu.PopupMenuSection();
        this._toggle.menu.addMenuItem(this._workloads);
        this._gpu = new PopupMenu.PopupMenuSection();
        this._gpuKey = null;
        this._gpuActions = [];
        this._toggle.menu.addMenuItem(this._gpu);
        this._toggle.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());
        this._idle = new PopupMenu.PopupSwitchMenuItem(
            'Auto-idle after inactivity',
            false,
        );
        this._idle.connect('toggled', (item, on) => {
            // Shell 50 also emits toggled when _renderIdle sets the saved
            // state. Only a change from the saved policy is a user request.
            const saved = (this._model.settings?.policy.timeoutMinutes ?? 0) !== 0;
            if (on !== saved) this.setIdle(on ? IDLE_DEFAULT : 0);
        });
        this._toggle.menu.addMenuItem(this._idle);
        this._idleTimeout = new PopupMenu.PopupSubMenuMenuItem('Idle timeout');
        this._idleItems = IDLE_TIMEOUTS.map((minutes) => ({
            minutes,
            item: this._idleTimeout.menu.addAction(`${minutes} minutes`, () =>
                this.setIdle(minutes),
            ),
        }));
        this._toggle.menu.addMenuItem(this._idleTimeout);
        this._toggle.menu.connect('open-state-changed', (menu, open) => {
            if (open && this._alive) this._reload();
        });
        this._details = new PopupMenu.PopupSubMenuMenuItem('Details');
        this._toggle.menu.addMenuItem(this._details);
        this._refresh = this._toggle.menu.addAction('Refresh', () =>
            this._reload(),
        );
        this._setup = this._toggle.menu.addAction('Manage workloads…', () => {
            Gio.DesktopAppInfo.new(
                'gpu-workload-supervisor-setup.desktop',
            )?.launch([], globalThis.global.create_app_launch_context(0, -1));
        });
        this._setup.visible = Main.sessionMode.allowSettings;
        this._sessionSignal = Main.sessionMode.connect('updated', () => {
            this._setup.visible = Main.sessionMode.allowSettings;
        });
        Main.panel.statusArea.quickSettings.addExternalIndicator(
            this._indicator,
        );
        this._ageTimer = GLib.timeout_add_seconds(
            GLib.PRIORITY_DEFAULT,
            1,
            () => {
                if (!this._alive) return GLib.SOURCE_REMOVE;
                this._render();
                return GLib.SOURCE_CONTINUE;
            },
        );
        this._call('status');
    }
    _label(id) {
        return (
            this._model.status?.workloads.find((w) => w.id === id)?.label ??
            'Unknown'
        );
    }
    ownership() {
        const m = this._model;
        const d = m.intent(
            m.status?.owner === 'user' ? 'return-control' : 'take-control',
            null,
            now(),
        );
        if (!d || this._dialog) return;
        this._toggle.menu.close();
        this._dialog = ownershipDialog(
            d.action,
            this._label(m.status.activeWorkload),
            () => {
                if (this._alive && m.validDecision(d, now()))
                    this._call(d.action, d);
                else if (this._alive) this._call('status');
            },
            () => {
                this._dialog = null;
            },
        );
    }
    closeProcess(pid) {
        const m = this._model;
        const d = m.closeIntent(pid);
        if (!d || this._dialog) return;
        const process = m.blocked.processes.find((p) => p.pid === pid);
        this._toggle.menu.close();
        this._dialog = closeProcessDialog(
            processName(process),
            pid,
            () => {
                if (this._alive && m.closable(pid)) this._call(CLOSE_ACTION, d);
            },
            () => {
                this._dialog = null;
            },
        );
    }
    setIdle(timeoutMinutes) {
        const d = this._model.idleIntent(timeoutMinutes, now());
        if (d) this._call(d.action, d);
        // A refused click puts the switch back to the committed policy.
        else this._render();
    }
    // Read status, then the idle policy settings.
    _reload() {
        this._model.settingsStale = true;
        this._call('status');
    }
    retry() {
        const d = this._model.retryIntent(now());
        if (d) this._call(d.action, d);
    }
    async _call(action, decision = null) {
        if (!this._alive) return;
        const m = this._model,
            transport = this._transport,
            c = m.begin(action, now(), decision);
        if (!c) return;
        if (this._timer) {
            GLib.source_remove(this._timer);
            this._timer = 0;
        }
        this._render();
        try {
            const response = await transport.call(c.request);
            m.accept(c, response, now());
        } catch {
            m.fail(c, 'unavailable');
        }
        if (!this._alive || this._model !== m) return;
        this._render();
        if (action === 'status' && !m.error && m.settingsStale)
            return this._call('get-settings');
        this._timer = GLib.timeout_add(GLib.PRIORITY_DEFAULT, m.delay, () => {
            this._timer = 0;
            this._call('status');
            return GLib.SOURCE_REMOVE;
        });
    }
    _render() {
        const m = this._model,
            v = m.view(now()),
            s = v.status;
        this._toggle.checked = v.checked;
        this._toggle.accessible_description = v.mutable
            ? 'GPU ownership control'
            : 'GPU mutations unavailable; open menu for Details';
        this._toggle.subtitle = this._subtitle(v);
        this._refresh.setSensitive(!v.pending);
        this._renderWorkloads(m, s);
        this._renderGPU(m, v.blocked);
        this._renderIdle(m);
        this._renderDetails(v, s);
    }
    _renderIdle(m) {
        const minutes = m.settings?.policy.timeoutMinutes ?? 0;
        this._idle.setToggleState(minutes !== 0);
        this._idle.setSensitive(
            !!m.idleIntent(minutes ? 0 : IDLE_DEFAULT, now()),
        );
        this._idleTimeout.visible = minutes !== 0;
        this._idleTimeout.label.text = `Idle timeout: ${minutes} minutes`;
        for (const { minutes: value, item } of this._idleItems) {
            item.setOrnament(
                value === minutes
                    ? PopupMenu.Ornament.DOT
                    : PopupMenu.Ornament.NONE,
            );
            item.setSensitive(!!m.idleIntent(value, now()));
        }
    }
    _gpuHeader(blocked) {
        const label = this._label(blocked.target);
        if (blocked.failure)
            return `GPU check failed: ${blocked.failure} — ${label} not started`;
        if (blocked.processes.length)
            return `GPU in use — ${label} not started`;
        return `GPU is free — retry ${label}`;
    }
    _renderGPU(m, blocked) {
        const key = JSON.stringify(blocked);
        if (key !== this._gpuKey) {
            this._gpuKey = key;
            this._gpu.removeAll();
            this._gpuActions = [];
            if (blocked) this._buildGPU(blocked);
        }
        for (const { pid, item } of this._gpuActions)
            item.setSensitive(
                pid === null ? !!m.retryIntent(now()) : !!m.closeIntent(pid),
            );
    }
    _buildGPU(blocked) {
        const rows = [this._gpuHeader(blocked), ...blocked.processes.map(processRow)];
        for (const text of rows)
            this._gpu.addMenuItem(
                new PopupMenu.PopupMenuItem(text, { reactive: false }),
            );
        for (const p of blocked.processes.filter((q) => q.closable)) {
            const item = this._gpu.addAction(`Close ${processName(p)}…`, () =>
                this.closeProcess(p.pid),
            );
            this._gpuActions.push({ pid: p.pid, item });
        }
        const retry = this._gpu.addAction('Retry', () => this.retry());
        this._gpuActions.push({ pid: null, item: retry });
    }
    _subtitle(v) {
        if (v.pending) return 'Request pending…';
        if (v.error) return messages[v.error] ?? 'Unavailable';
        if (!v.fresh) return 'Status is stale';
        const owner = v.checked ? 'Manual' : 'Supervisor';
        return `${owner} · ${this._label(v.status.activeWorkload)}`;
    }
    _renderWorkloads(m, s) {
        const catalog = JSON.stringify(s?.workloads ?? []);
        if (catalog !== this._catalog) {
            this._catalog = catalog;
            this._workloads.removeAll();
            this._workloadItems = [];
            for (const w of s?.workloads ?? []) {
                const item = this._workloads.addAction(w.label, () => {
                    const d = m.intent('user-switch', w.id, now());
                    if (d) this._call(d.action, d);
                });
                this._workloadItems.push({ id: w.id, item });
            }
        }
        for (const { id, item } of this._workloadItems ?? []) {
            item.setOrnament(
                s.activeWorkload === id
                    ? PopupMenu.Ornament.DOT
                    : PopupMenu.Ornament.NONE,
            );
            item.setSensitive(!!m.intent('user-switch', id, now()));
        }
    }
    _renderDetails(v, s) {
        const rows = s
            ? [
                  `Owner: ${s.owner === 'user' ? 'User' : 'Supervisor'}`,
                  `Requested: ${this._label(s.desiredWorkload)}`,
                  `Active: ${this._label(s.activeWorkload)}`,
                  `Phase: ${s.phase}`,
                  `Health: ${s.health}`,
                  `Admission: ${s.admission}`,
                  `Freshness: ${v.fresh ? 'fresh' : 'stale'} (${Math.floor(v.age / 1000)}s)`,
                  `Observed: ${s.observedAt}`,
              ]
            : ['No accepted observation'];
        if (this._detailRows?.length !== rows.length) {
            this._details.menu.removeAll();
            this._detailRows = rows.map((text) => {
                const item = new PopupMenu.PopupMenuItem(text, {
                    reactive: false,
                });
                this._details.menu.addMenuItem(item);
                return item;
            });
        }
        rows.forEach((text, index) => {
            this._detailRows[index].label.text = text;
        });
    }
    disable() {
        this._alive = false;
        this._model?.retire();
        this._transport?.detach();
        for (const key of ['_timer', '_ageTimer']) {
            if (this[key]) GLib.source_remove(this[key]);
            this[key] = 0;
        }
        if (this._sessionSignal)
            Main.sessionMode.disconnect(this._sessionSignal);
        this._sessionSignal = 0;
        this._dialog?.destroy();
        this._dialog = null;
        this._toggle?.destroy();
        this._indicator?.destroy();
        this._toggle = null;
        this._indicator = null;
        this._transport = null;
        this._model = null;
    }
}
