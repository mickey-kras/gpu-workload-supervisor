// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
import {ReviewedConfiguration} from './review.mjs';
import {applications} from './onboarding.mjs';
import {addDraftEditor, addErrorReporter} from './discovery-ui.mjs';

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
    const window = new Adw.ApplicationWindow({application: app, title: 'Manage workloads',
        default_width: 720, default_height: 760});
    const toolbar = new Adw.ToolbarView();
    toolbar.add_top_bar(new Adw.HeaderBar());
    const box = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18,
        margin_start: 24, margin_end: 24, margin_top: 18, margin_bottom: 18});
    const scroll = new Gtk.ScrolledWindow({vexpand: true, hscrollbar_policy: Gtk.PolicyType.NEVER});
    scroll.set_child(box); toolbar.set_content(scroll); window.set_content(toolbar);
    const heading = new Gtk.Label({label: 'Manage workloads', xalign: 0});
    heading.add_css_class('title-1'); box.append(heading);
    box.append(new Gtk.Label({label: 'Add ComfyUI, Ollama, llama.cpp or vLLM. Applications and models must already be installed. Discovery never starts applications or loads models.', wrap: true, xalign: 0}));
    const status = new Gtk.Label({label: 'Checking your desktop and available services…', wrap: true, xalign: 0, selectable: true}); box.append(status);
    const reportError = addErrorReporter({Adw, Gtk, parent: box, status});
    const rows = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18}); box.append(rows);
    const settings = new Adw.PreferencesGroup(); box.append(settings);
    const advanced = new Adw.ExpanderRow({title: 'Advanced settings', subtitle: 'State database and NVIDIA GPU'});
    settings.add(advanced);
    const changes = new Gtk.Label({label: 'Finish setup to check the current changes.', wrap: true, xalign: 0, selectable: true});
    advanced.add_row(changes);
    let discovered = []; let discoveryErrors = []; let draftRevision = ''; let drafts = []; let draftGeneration = 0;
    const draftEditors = [];
    let request = null; let pending = false; let units = []; let valid = false; const profiles = []; const profileApps = new Map(); const profileRows = new Map();
    const reviewed = new ReviewedConfiguration();
    const review = new Gtk.Button({label: 'Finish setup', sensitive: false});
    const apply = new Gtk.Button({label: 'Apply configuration', sensitive: false});
    apply.add_css_class('suggested-action');
    const confirm = new Gtk.CheckButton({sensitive: false});
    confirm.set_child(new Gtk.Label({label: 'I have paused new work and finished active jobs. I have reviewed the changes above.', wrap: true, xalign: 0}));
    const invalidate = () => {
        reviewed.invalidate(); valid = false; apply.sensitive = false;
        confirm.active = false; confirm.sensitive = false;
    };
    const field = (parent, title, value, changed) => {
        const row = new Adw.EntryRow({title, text: String(value ?? '')});
        row.connect('changed', () => { changed(row.text); invalidate(); });
        if (parent instanceof Adw.ExpanderRow) parent.add_row(row);
        else parent.add(row);
        return row;
    };
    function addProfile(profile, resetReview = true) {
        if (resetReview) invalidate();
        const current = {...profile};
        profiles.push(current);
        const group = new Adw.PreferencesGroup({title: GLib.markup_escape_text(current.label || 'New workload', -1),
            description: current.nativeModel ? `${current.nativeModel.runtime} · ${current.nativeModel.model}. Applications and model files are preserved.` : 'Start and stop this installation from GPU Control. Applications and files are preserved.'});
        field(group, 'Display name', current.label, text => {
            current.label = text; group.title = GLib.markup_escape_text(text || 'New workload', -1);
        });
        const details = new Adw.ExpanderRow({title: 'Advanced',
            subtitle: 'Stable ID, manual service name, VRAM and login behavior', expanded: false});
        group.add(details);
        const bindingParent = details;
        const choices = ['', ...new Set([...(current.unit ? [current.unit] : []), ...units]), null];
        let syncingService = false;
        const service = new Adw.ComboRow({title: 'Existing user service', enable_search: true, use_markup: false,
            expression: Gtk.PropertyExpression.new(Gtk.StringObject.$gtype, null, 'string'),
            model: Gtk.StringList.new(['Choose a service…', ...choices.slice(1, -1), 'Enter another service…']),
            selected: current.unit ? choices.indexOf(current.unit) : 0});
        if (current.nativeModel?.owned) service.sensitive = false;
        if (bindingParent === details) details.add_row(service); else group.add(service);
        field(bindingParent, 'Cgroup path beneath /sys/fs/cgroup (required)', current.cgroup, text => current.cgroup = text).editable = !current.nativeModel?.owned;
        field(bindingParent, 'Loopback health URL (required)', current.healthURL, text => current.healthURL = text).editable = !current.nativeModel?.owned;

        const manualService = field(details, 'Service name (manual entry)', current.unit, text => {
            current.unit = text;
            syncingService = true;
            const index = choices.indexOf(text);
            service.selected = index < 0 ? choices.length - 1 : index;
            syncingService = false;
        });
        manualService.editable = !current.nativeModel?.owned;
        service.connect('notify::selected', () => {
            if (syncingService) return;
            const unit = choices[service.selected];
            if (unit === null) {
                details.expanded = true; manualService.grab_focus();
                return;
            }
            current.unit = unit ?? '';
            manualService.text = current.unit;
            invalidate();
        });
        if (current.nativeModel) {
            current.nativeModel = {...current.nativeModel};
            for (const [key, title] of [['instance', 'Runtime instance ID'], ['model', 'Exact model ID'], ['endpoint', 'Runtime base URL'], ['launchFile', 'Loaded service file path']])
                field(details, title, current.nativeModel[key], text => current.nativeModel[key] = text).editable = !current.nativeModel.owned;
        }
        if (current.launchBinding) {
            current.launchBinding = {...current.launchBinding};
            for (const [key, title] of [['endpoint', 'Application address'], ['launchFile', 'Loaded service file path'], ['launchSHA256', 'Service file SHA-256']])
                field(details, title, current.launchBinding[key], text => current.launchBinding[key] = text).editable = key !== 'launchSHA256';
        }
        field(details, 'Workload ID (lowercase, stable; required)', current.id, text => current.id = text).editable = !current.nativeModel?.owned;
        field(details, 'Measured VRAM requirement (MiB; optional)', current.requiredMiB, text => {
            if (text.trim() === '') delete current.requiredMiB;
            else current.requiredMiB = Number(text);
        });
        const retain = new Gtk.CheckButton({label: 'Keep this workload running at login if already active', active: current.bootPolicy === 'retain'});
        retain.connect('toggled', () => { current.bootPolicy = retain.active ? 'retain' : 'stop-to-idle'; invalidate(); });
        details.add_row(retain);
        const remove = new Gtk.Button({label: 'Remove from supervisor'});
        remove.connect('clicked', () => { profiles.splice(profiles.indexOf(current), 1); rows.remove(group); invalidate(); });
        const runtime = current.nativeModel?.runtime ?? current.launchBinding?.runtime ?? profileApps.get(current.id);
        if (runtime) {
            const editLaunch = new Gtk.Button({label: 'Edit application'});
            editLaunch.connect('clicked', () => appendDraft({id: current.id, label: current.label, app: runtime,
                model: current.nativeModel?.model, endpoint: current.nativeModel?.endpoint ?? current.launchBinding?.endpoint, binding: current.nativeModel?.owned ? {instance: current.nativeModel.instance, owned: {...current.nativeModel.owned}} : {unit: current.unit, cgroup: current.cgroup, healthURL: current.healthURL, instance: current.nativeModel?.instance, model: current.nativeModel?.model, launchFile: current.nativeModel?.launchFile ?? current.launchBinding?.launchFile}}, current));
            group.add(editLaunch);
        }
        group.add(remove); rows.append(group); profileRows.set(current, group);
    }
    const add = new Gtk.Button({label: 'Add existing service (Advanced)', sensitive: false});
    add.connect('clicked', () => addProfile({adapter: 'systemd', bootPolicy: 'stop-to-idle'}));
    advanced.add_row(add);
    const draftRows = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18}); box.append(draftRows);
    const application = new Adw.ComboRow({title: 'Application', use_markup: false,
        model: Gtk.StringList.new(applications.map(item => item.label)), selected: 0});
    const applicationGroup = new Adw.PreferencesGroup(); applicationGroup.add(application); box.append(applicationGroup);
    const addApplication = new Gtk.Button({label: 'Add workload', sensitive: false}); box.append(addApplication);
    const saveDrafts = new Gtk.Button({label: 'Save drafts', sensitive: false}); box.append(saveDrafts);
    const catalogFor = items => ({...request.catalog, version: items.some(profile => profile.nativeModel?.owned) ? 2 : request.catalog.version, profiles: items});
    function appendDraft(initial, replacing = null) {
        if (drafts.some(draft => draft.id === initial.id)) { status.label = 'This workload already has an open draft. Finish or remove that draft first.'; return; }
        drafts.push(initial);
        draftEditors.push(addDraftEditor({Adw, Gtk, Gio, window, parent: draftRows, initial, detected: discovered, discoveryErrors, command,
            bind: async (profile, current, finish = false) => {
                replacing ??= profiles.find(existing => existing.id === initial.id) ?? null;
                if (replacing && !profiles.includes(replacing)) throw new Error('The original workload was removed. Reopen Manage workloads before editing it.');
                const preserveSettings = () => replacing ? {...profile, id: replacing.id, requiredMiB: replacing.requiredMiB, bootPolicy: replacing.bootPolicy} : profile;
                const candidate = JSON.stringify({...request, catalog: catalogFor([...profiles.filter(existing => existing !== replacing), preserveSettings()]), confirmQuiesced: false});
                await command(['/usr/bin/gpu-setup', 'verify-bindings'], candidate);
                if (replacing && !profiles.includes(replacing)) throw new Error('The original workload changed during preview. Reopen Manage workloads.');
                if (!current()) return;
                const prepared = preserveSettings();
                const commit = () => {
                    if (replacing) {
                        profiles.splice(profiles.indexOf(replacing), 1);
                        rows.remove(profileRows.get(replacing)); profileRows.delete(replacing);
                    }
                    profileApps.set(prepared.id, initial.app);
                    addProfile(prepared, !finish);
                };
                if (finish) {
                    const items = [...profiles.filter(existing => existing !== replacing), prepared];
                    if (!await reviewConfiguration({items, current, commit}))
                        throw new Error('The configuration needs another check before confirmation. Your draft is kept.');
                } else {
                    commit();
                    status.label = 'Application checked. Finish setup to confirm the changes.';
                }
            },
            changed: value => { invalidate(); drafts = drafts.map(item => item.id === initial.id ? value : item); draftGeneration++; saveDrafts.sensitive = !pending; },
            removed: (finished = false) => { if (!finished) invalidate(); drafts = drafts.filter(item => item.id !== initial.id); draftGeneration++; saveDrafts.sensitive = !pending; },
            taken: () => profiles.map(profile => profile.id)}));
    }
    addApplication.connect('clicked', () => {
        const selected = applications[application.selected];
        appendDraft({id: `draft-${GLib.uuid_string_random()}`, label: selected.label, app: selected.id});
        draftGeneration++; saveDrafts.sensitive = true;
    });
    saveDrafts.connect('clicked', async () => {
        const generation = draftGeneration;
        let saved = false;
        saveDrafts.sensitive = false;
        try {
            const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'save-drafts'], JSON.stringify({version: 1, expectedRevision: draftRevision, drafts})));
            draftRevision = result.revision; saved = true;
            status.label = 'Drafts saved. Saved applications still need verification. Drafts are not selectable in GPU Control until safe lifecycle control is configured and verified.';
        } catch (error) { reportError('Drafts were not saved. Reopen Manage workloads to refresh before retrying.', error); }
        finally { saveDrafts.sensitive = !pending && (!saved || generation !== draftGeneration); }
    });
    const later = new Gtk.Button({label: 'Set up later'}); box.append(later);
    later.connect('clicked', () => { draftEditors.forEach(editor => editor.cancel()); window.close(); });
    const footer = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 8,
        margin_start: 24, margin_end: 24, margin_top: 12, margin_bottom: 12});
    const actions = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 12, homogeneous: true});
    footer.append(confirm); actions.append(review); actions.append(apply); footer.append(actions);
    footer.add_css_class('toolbar');
    toolbar.add_bottom_bar(footer);
    confirm.connect('toggled', () => apply.sensitive = valid && confirm.active);
    const serialize = (items = profiles) => {
        // A pending activation must resume the recorded request without defaults or edits.
        if (pending) return JSON.stringify(request);
        if (!Number.isSafeInteger(request.profile.gpuIndex) || request.profile.gpuIndex < 0 ||
            items.some(profile => !Number.isSafeInteger(profile.requiredMiB ?? 0) || (profile.requiredMiB ?? 0) < 0))
            throw new Error('GPU index and VRAM requirements must be nonnegative whole numbers.');
        return JSON.stringify({...request, catalog: catalogFor(items), confirmQuiesced: false});
    };
    async function reviewConfiguration(options = {}) {
        const {items = profiles, current = () => true, commit = () => {}} = options;
        if (!request) return;
        invalidate(); review.sensitive = false;
        try {
            const candidate = reviewed.begin(serialize(items));
            if (!pending) {
                const snapshot = JSON.parse(candidate.request);
                for (const profile of snapshot.catalog.profiles) {
                    if (!profile.nativeModel || profile.nativeModel.owned) continue;
                    const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'fingerprint'], JSON.stringify({binding: profile.nativeModel})));
                    profile.nativeModel.launchSHA256 = result.sha256;
                }
                candidate.request = JSON.stringify(snapshot);
            }
            const preview = JSON.parse(await command(['/usr/bin/gpu-setup', 'validate'], candidate.request));
            const hasComfy = items.some(profile => profileApps.get(profile.id) === 'comfyui' || profile.launchBinding?.runtime === 'comfyui' || profile.id.startsWith('comfyui'));
            if (!pending) await command(['/usr/bin/gpu-setup', 'verify-bindings'], candidate.request);
            if (!current() || !reviewed.accept(candidate)) {
                status.label = 'Configuration changed during review. Review the updated configuration.';
                return;
            }
            commit();
            changes.label = preview.changes.join('\n');
            const removedProfiles = (request.catalog.profiles ?? []).filter(original => !items.some(profile => profile.id === original.id));
            const removal = removedProfiles.length ? ` Remove ${removedProfiles.map(profile => profile.label || profile.id).join(', ')} from Supervisor? Their applications and files are preserved.` : '';
            status.label = `Allow GPU Workload Supervisor to start and stop ${items.map(profile => profile.label || profile.id).join(', ') || 'the configured applications'}?${hasComfy ? ' ComfyUI closes when switching to another workload.' : ''}${removal}\nConfirm below after active jobs finish. No application was started during setup.`;
            valid = true; confirm.sensitive = true; return true;
        } catch (error) {
            reportError('Review failed. Correct the fields above or reopen Manage workloads to refresh, then review again.', error);
            invalidate(); return false;
        }
        finally { review.sensitive = true; }
    }
    review.connect('clicked', reviewConfiguration);
    apply.connect('clicked', async () => {
        if (!valid || !confirm.active) return;
        const activationRequest = reviewed.confirmed();
        apply.sensitive = false; review.sensitive = false; add.sensitive = false;
        rows.sensitive = false; settings.sensitive = false; confirm.sensitive = false;
        draftRows.sensitive = false; addApplication.sensitive = false; saveDrafts.sensitive = false; later.sensitive = false;
        status.label = 'Applying configuration. Keep this window open; interrupted activation can be resumed.';
        try {
            await command(['/usr/bin/gpu-setup', 'apply'], activationRequest);
            status.label = 'Configuration activated. Reconciliation is enabled for future logins. Log out and back in to discover the extension, then enable “GPU Workload Supervisor” in Extensions. GPU Control appears in the top-right Quick Settings menu. No workload was started.';
        } catch (error) {
            reportError('Setup needs attention. Switch to Idle and finish active jobs before changing configured workloads. Reopen Manage workloads to refresh or resume an interrupted activation. State and backups are preserved.', error);
            review.sensitive = true; later.sensitive = true;
        }
        finally { invalidate(); }
    });
    window.connect('close-request', () => { draftEditors.forEach(editor => editor.cancel()); invalidate(); return false; });
    window.present();
    (async () => {
        try {
            const version = await command(['/usr/bin/gnome-shell', '--version']);
            if (!/\b50(?:\.|\s|$)/.test(version) || !GLib.getenv('XDG_CURRENT_DESKTOP')?.includes('GNOME'))
                throw new Error('This package requires a GNOME Shell 50 desktop session.');
            const discovery = JSON.parse(await command(['/usr/bin/gpu-setup', 'discover']));
            request = discovery.request; units = discovery.units; pending = Boolean(discovery.pending); discovered = discovery.applications ?? []; discoveryErrors = discovery.errors ?? [];
            if (!pending) {
                const saved = JSON.parse(await command(['/usr/bin/gpu-setup', 'drafts']));
                draftRevision = saved.revision ?? '';
                for (const draft of saved.drafts ?? []) appendDraft(draft);
            }
            addApplication.sensitive = !pending; applicationGroup.sensitive = !pending;
            status.label = pending ? 'Interrupted setup found. Review and resume its original configuration.' :
                'Choose an application, then Finish. Existing applications can be edited below.';
            field(advanced, 'State database path', request.profile.statePath, text => request.profile.statePath = text);
            field(advanced, 'NVIDIA GPU index', request.profile.gpuIndex, text => request.profile.gpuIndex = Number(text));
            for (const profile of request.catalog.profiles ?? []) addProfile(profile);
            add.sensitive = !pending; rows.sensitive = !pending; settings.sensitive = !pending;
            review.sensitive = true;
            if (discoveryErrors.length) reportError('Some installations could not be checked. Choose an application location or reopen setup to retry discovery.', new Error(discoveryErrors.join('\n')));
        } catch (error) { status.label = error.message; review.sensitive = false; }
    })();
});
app.run([]);
