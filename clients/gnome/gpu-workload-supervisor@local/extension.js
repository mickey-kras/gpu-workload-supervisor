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
import { ownershipDialog } from './dialogs.js';
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
};
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
        this._toggle.menu.addMenuItem(new PopupMenu.PopupSeparatorMenuItem());
        this._details = new PopupMenu.PopupSubMenuMenuItem('Details');
        this._toggle.menu.addMenuItem(this._details);
        this._refresh = this._toggle.menu.addAction('Refresh', () =>
            this._call('status'),
        );
        this._setup = this._toggle.menu.addAction('Setup…', () => {
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
        this._renderDetails(v, s);
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
