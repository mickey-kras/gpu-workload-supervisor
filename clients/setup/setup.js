// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
import {ReviewedConfiguration} from './review.mjs';

// The setup application is short-lived. Runtime controls use gpu-operator.
function command(argv, input = null) {
    return new Promise((resolve, reject) => {
        const proc = Gio.Subprocess.new(argv, Gio.SubprocessFlags.STDIN_PIPE |
            Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE);
        proc.communicate_utf8_async(input, null, (child, result) => {
            try {
                const [, stdout, stderr] = child.communicate_utf8_finish(result);
                if (!child.get_successful()) throw new Error(stderr.trim() || 'Command failed');
                resolve(stdout);
            } catch (error) { reject(error); }
        });
    });
}
const app = new Adw.Application({application_id: 'local.GPUWorkload.Setup'});
app.connect('activate', () => {
    const window = new Adw.ApplicationWindow({application: app, title: 'GPU Workload Setup',
        default_width: 720, default_height: 760});
    const box = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 12,
        margin_start: 24, margin_end: 24, margin_top: 18, margin_bottom: 18});
    const scroll = new Gtk.ScrolledWindow({vexpand: true}); scroll.set_child(box);
    window.set_content(scroll);
    const heading = new Gtk.Label({label: 'Configure existing GPU workloads', xalign: 0});
    heading.add_css_class('title-1'); box.append(heading);
    box.append(new Gtk.Label({label: 'GNOME Shell 50 is required. Applications and models must already be installed. Setup never starts or stops your workloads.', wrap: true, xalign: 0}));
    const status = new Gtk.Label({wrap: true, xalign: 0, selectable: true}); box.append(status);
    const rows = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 12}); box.append(rows);
    let request = null; let valid = false; const profiles = [];
    const reviewed = new ReviewedConfiguration();
    const review = new Gtk.Button({label: 'Validate and preview'});
    const apply = new Gtk.Button({label: 'Apply reviewed configuration', sensitive: false});
    apply.add_css_class('suggested-action');
    const confirm = new Gtk.CheckButton({label: 'I have quiesced existing work and reviewed these changes.'});
    const invalidate = () => { reviewed.invalidate(); valid = false; apply.sensitive = false; };
    const field = (parent, title, value, changed) => {
        const row = new Adw.EntryRow({title, text: String(value ?? '')});
        row.connect('changed', () => { changed(row.text); invalidate(); }); parent.append(row); return row;
    };
    function addProfile(profile) {
        invalidate();
        const group = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL});
        const current = {...profile, adapter: profile.adapter ?? 'systemd', bootPolicy: profile.bootPolicy ?? 'stop-to-idle'};
        profiles.push(current);
        field(group, 'Workload ID (lowercase, stable)', current.id, text => current.id = text);
        field(group, 'Display name', current.label, text => current.label = text);
        field(group, 'Existing systemd user service', current.unit, text => current.unit = text);
        field(group, 'Cgroup path beneath /sys/fs/cgroup', current.cgroup, text => current.cgroup = text);
        field(group, 'Loopback health URL', current.healthURL, text => current.healthURL = text);
        field(group, 'Measured VRAM requirement (MiB)', current.requiredMiB ?? 0, text => current.requiredMiB = Number(text));
        const retain = new Gtk.CheckButton({label: 'Retain this workload at login if already running', active: current.bootPolicy === 'retain'});
        retain.connect('toggled', () => { current.bootPolicy = retain.active ? 'retain' : 'stop-to-idle'; invalidate(); }); group.append(retain);
        const remove = new Gtk.Button({label: 'Remove from configuration'});
        remove.connect('clicked', () => { profiles.splice(profiles.indexOf(current), 1); rows.remove(group); invalidate(); }); group.append(remove);
        rows.append(group);
    }
    const add = new Gtk.Button({label: 'Add workload', sensitive: false});
    add.connect('clicked', () => addProfile({})); box.append(add);
    box.append(review); box.append(confirm); box.append(apply);
    confirm.connect('toggled', () => apply.sensitive = valid && confirm.active);
    const serialize = () => {
        if (!Number.isSafeInteger(request.profile.gpuIndex) || request.profile.gpuIndex < 0 ||
            profiles.some(profile => !Number.isSafeInteger(profile.requiredMiB ?? 0) || (profile.requiredMiB ?? 0) < 0))
            throw new Error('GPU index and VRAM requirements must be nonnegative whole numbers.');
        return JSON.stringify({...request, catalog: {version: 1, profiles}, confirmQuiesced: confirm.active});
    };
    review.connect('clicked', async () => {
        if (!request) return;
        review.sensitive = false;
        try {
            const candidate = reviewed.begin(serialize());
            const preview = JSON.parse(await command(['/usr/bin/gpu-setup', 'validate'], candidate.request));
            if (!reviewed.accept(candidate)) {
                status.label = 'Configuration changed during validation. Review the updated configuration.';
                return;
            }
            status.label = `Review changes:\n${preview.changes.join('\n')}\n\nWorkloads: ${profiles.map(p => p.label).join(', ')}`;
            valid = true; apply.sensitive = confirm.active;
        } catch (error) { status.label = error.message; invalidate(); }
        finally { review.sensitive = true; }
    });
    apply.connect('clicked', async () => {
        if (!valid || !confirm.active) return;
        const activationRequest = reviewed.confirmed();
        apply.sensitive = false; review.sensitive = false; add.sensitive = false; rows.sensitive = false;
        status.label = 'Applying configuration. Keep this window open; interrupted activation can be resumed.';
        try {
            await command(['/usr/bin/gpu-setup', 'apply'], activationRequest);
            status.label = 'Configuration activated. Reconciliation is enabled for future logins. Log out and back in to discover the extension, then enable “GPU Workload Supervisor” in Extensions. No workload was started.';
        } catch (error) { status.label = `Setup needs attention: ${error.message}\nRerun setup to resume the recorded activation. State and backups are preserved.`; }
        finally { review.sensitive = true; }
    });
    window.present();
    (async () => {
        try {
            const version = await command(['/usr/bin/gnome-shell', '--version']);
            if (!/\b50(?:\.|\s|$)/.test(version) || !GLib.getenv('XDG_CURRENT_DESKTOP')?.includes('GNOME'))
                throw new Error('This package requires a GNOME Shell 50 desktop session.');
            const discovery = JSON.parse(await command(['/usr/bin/gpu-setup', 'discover']));
            request = discovery.request;
            status.label = `Available services (not automatically adopted):\n${discovery.units.join(', ') || 'None found'}`;
            if (discovery.pending) status.label = 'Interrupted setup found. Review and resume its original configuration.';
            field(rows, 'State database path', request.profile.statePath, text => request.profile.statePath = text);
            field(rows, 'NVIDIA GPU index', request.profile.gpuIndex, text => request.profile.gpuIndex = Number(text));
            for (const profile of request.catalog.profiles ?? []) addProfile(profile);
            add.sensitive = !discovery.pending;
            if (discovery.pending) rows.sensitive = false;
        } catch (error) { status.label = error.message; review.sensitive = false; }
    })();
});
app.run([]);
