// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
import {installSetupStyle, applicationIcon, applicationHeader, roundedCard, linkButton} from './presentation.mjs';
import {ReviewedConfiguration} from './review.mjs';
import {createProfileEditor} from './profile-ui.mjs';
import {applications} from './onboarding.mjs';
import {addDraftEditor, addErrorReporter} from './discovery-ui.mjs';

// The caller supplies transport; fixture callers never invoke the production subprocess.
export function createSetupWindow({app, command}) {
    const window = new Adw.ApplicationWindow({application: app, title: 'GPU Workload Setup',
        default_width: 620, default_height: 670});
    installSetupStyle(window);
    const appearance = Adw.StyleManager.get_default();
    const updateAppearance = () => {
        for (const [name, enabled] of [['setup-dark', appearance.dark], ['setup-high-contrast', appearance.high_contrast]]) {
            if (enabled) window.add_css_class(name); else window.remove_css_class(name);
        }
    };
    appearance.connect('notify::dark', updateAppearance); appearance.connect('notify::high-contrast', updateAppearance); updateAppearance();
    const toolbar = new Adw.ToolbarView();
    toolbar.add_top_bar(new Adw.HeaderBar());
    const box = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 14,
        margin_start: 28, margin_end: 28, margin_top: 20, margin_bottom: 12});
    const scroll = new Gtk.ScrolledWindow({vexpand: true, hscrollbar_policy: Gtk.PolicyType.NEVER});
    scroll.set_child(box); toolbar.set_content(scroll); window.set_content(toolbar);
    const heading = new Gtk.Label({label: 'Choose your applications', xalign: 0, wrap: true, focusable: true});
    heading.add_css_class('title-1'); heading.add_css_class('setup-heading'); box.append(heading);
    const introduction = new Gtk.Label({label: 'Use applications already installed on this computer.', wrap: true, xalign: 0}); introduction.add_css_class('setup-introduction'); box.append(introduction);
    const status = new Gtk.Label({label: 'Checking your desktop and available services…', wrap: true, xalign: 0, selectable: true}); status.add_css_class('setup-notice'); status.visible = false;
    status.connect('notify::label', () => { status.visible = true; });
    const reportError = addErrorReporter({Adw, Gtk, parent: box, status});
    const applicationPage = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 12}); box.append(applicationPage);
    const modelPage = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18, visible: false}); box.append(modelPage);
    const finishPage = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18, visible: false}); box.append(finishPage);
    const summary = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 10}); finishPage.append(summary);
    const explanations = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 10, margin_top: 12});
    explanations.append(new Gtk.Separator({orientation: Gtk.Orientation.HORIZONTAL, margin_bottom: 8}));
    for (const [icon, text] of [['media-playback-start-symbolic', 'Only one workload runs at a time.'], ['media-playback-stop-symbolic', 'ComfyUI closes when switching away.'], ['security-high-symbolic', 'Your applications and models stay unchanged.']]) {
        const row = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 16});
        row.append(new Gtk.Image({icon_name: icon, pixel_size: 28}));
        const label = new Gtk.Label({label: text, wrap: true, xalign: 0}); label.add_css_class('setup-explanation'); row.append(label); explanations.append(row);
    }
    finishPage.append(explanations);
    const summaryCards = [];
    function renderSummary(items) {
        for (const card of summaryCards) summary.remove(card);
        summaryCards.length = 0;
        for (const profile of items) {
            const appID = runtimeOf(profile);
            const card = roundedCard(Gtk);
            if (appID) card.append(applicationHeader(Gtk, appID, profile.label || profile.id));
            else card.append(new Gtk.Label({label: profile.label || profile.id, wrap: true, xalign: 0}));
            summary.append(card); summaryCards.push(card);
        }
        if (!items.length) { const label = new Gtk.Label({label: 'No workloads selected. Supervisor configuration will be updated.', wrap: true, xalign: 0}); summary.append(label); summaryCards.push(label); }
    }
    box.append(status);
    let step = 0; let ready = false; let closed = false;
    const rows = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18});
    const settings = new Adw.PreferencesGroup(); applicationPage.append(settings);
    const advanced = new Adw.ExpanderRow({title: 'Advanced settings', subtitle: 'State database and NVIDIA GPU'});
    settings.add(advanced); advanced.add_row(rows);
    const changes = new Gtk.Label({label: 'Finish setup to check the current changes.', wrap: true, xalign: 0, selectable: true});
    advanced.add_row(changes);
    let temporaryStatus = null; let discovered = []; let discoveryErrors = []; let draftRevision = ''; let drafts = []; let draftGeneration = 0;
    const draftEditors = [];
    let request = null; let pending = false; let units = []; let valid = false; const profiles = []; const profileApps = new Map(); const profileRows = new Map();
    const reviewed = new ReviewedConfiguration();
    const editedLabels = new WeakMap();
    const review = new Gtk.Button({label: 'Continue', sensitive: false});
    review.add_css_class('suggested-action');
    const apply = new Gtk.Button({label: 'Finish setup', sensitive: false, visible: false});
    apply.add_css_class('suggested-action');
    const invalidate = () => {
        ready = false;
        reviewed.invalidate(); valid = false; apply.sensitive = false;
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
        if (editedLabels.has(profile)) editedLabels.set(current, editedLabels.get(profile));
        profiles.push(current);
        const group = createProfileEditor({current, Adw, Gtk, GLib, units, field, invalidate,
            applicationRuntime: profileApps.get(current.id),
            onLabelEdit: label => editedLabels.set(current, label),
            onRemove: group => { profiles.splice(profiles.indexOf(current), 1); rows.remove(group); invalidate(); },
            onEdit: draft => appendDraft(draft, current, true)});
        rows.append(group); profileRows.set(current, group); return current;
    }
    const add = new Gtk.Button({label: 'Add existing service (Advanced)', sensitive: false});
    add.connect('clicked', () => addProfile({adapter: 'systemd', bootPolicy: 'stop-to-idle'}));
    advanced.add_row(add);
    const draftRows = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18}); applicationPage.append(draftRows);
    const applicationCards = new Map();
    const suspendedApplications = new Map();
    const applicationGroup = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 12});
    applicationPage.append(applicationGroup); applicationPage.remove(settings); applicationPage.append(settings); applicationPage.remove(draftRows); applicationPage.append(draftRows);
    const saveDrafts = new Gtk.Button({label: 'Save selections for later', sensitive: false}); advanced.add_row(saveDrafts);
    function setStep(next) {
        step = next;
        heading.label = ['Choose your applications', 'Choose models', 'Ready to finish'][step];
        introduction.label = ['Use applications already installed on this computer.', 'Each selected model appears as a separate workload in GPU Control.', 'Supervisor will switch between these workloads.'][step];
        applicationPage.visible = step === 0; modelPage.visible = step === 1; finishPage.visible = step === 2;
        draftRows.visible = step === 0;
        back.visible = step > 0; later.visible = step === 0; status.visible = step === 2 || pending; apply.visible = step === 2; review.visible = step !== 2;
        review.label = 'Continue';
        heading.grab_focus();
    }
    function selectApplication(choice) {
        const suspended = suspendedApplications.get(choice.id);
        if (suspended) {
            for (const {profile, row} of suspended.profiles) {
                profiles.push(profile); rows.append(row); profileRows.set(profile, row);
            }
            drafts.push(...suspended.drafts);
            for (const editor of suspended.editors) editor.modelGroup.visible = true;
            suspendedApplications.delete(choice.id); draftGeneration++; saveDrafts.sensitive = !pending;
            if (suspended.profiles.length || suspended.drafts.length) return;
        }
        if (drafts.some(draft => draft.app === choice.id) || profiles.some(profile => runtimeOf(profile) === choice.id)) return;
        appendDraft({id: `draft-${GLib.uuid_string_random()}`, label: choice.label, app: choice.id});
        draftGeneration++; saveDrafts.sensitive = true;
    }
    const runtimeOf = profile => profile.nativeModel?.runtime ?? profile.launchBinding?.runtime ?? profileApps.get(profile.id) ?? (profile.adapter === 'comfyui' ? 'comfyui' : undefined);
    for (const choice of ['comfyui', 'llama.cpp', 'ollama', 'vllm'].map(id => applications.find(choice => choice.id === id))) {
        const card = roundedCard(Gtk, Gtk.Orientation.HORIZONTAL, 16);
        const detection = new Gtk.Label({label: 'Checking installation…', wrap: true, xalign: 0}); detection.add_css_class('dim-label'); detection.add_css_class('setup-detection');
        const select = new Gtk.CheckButton({active: false, sensitive: false, valign: Gtk.Align.CENTER});
        select.update_property([Gtk.AccessibleProperty.LABEL], [`Use ${choice.label}`]); card.append(select);
        const gear = new Gtk.Button({label: `Configure ${choice.label}`, icon_name: 'emblem-system-symbolic', tooltip_text: `Settings for ${choice.label}`, sensitive: false});
        const header = applicationHeader(Gtk, choice.id, choice.label, gear, detection); header.hexpand = true; card.append(header);
        gear.update_property([Gtk.AccessibleProperty.LABEL], [`Settings for ${choice.label}`]);
        gear.connect('clicked', () => {
            if (!select.active) select.active = true;
            const editor = draftEditors.find(editor => editor.app === choice.id && drafts.some(draft => draft.id === editor.id));
            if (editor) editor.showSettings();
            else {
                for (const existing of profiles.filter(profile => runtimeOf(profile) === choice.id)) profileRows.get(existing).visible = !profileRows.get(existing).visible;
                settings.visible = true; advanced.expanded = true;
            }
        });
        select.connect('toggled', () => {
            if (select.active) selectApplication(choice);
            else {
                const appDrafts = drafts.filter(draft => draft.app === choice.id);
                suspendedApplications.set(choice.id, {
                    profiles: profiles.filter(profile => runtimeOf(profile) === choice.id).map(profile => ({profile, row: profileRows.get(profile)})),
                    drafts: appDrafts,
                    editors: draftEditors.filter(editor => appDrafts.some(draft => draft.id === editor.id)),
                });
                for (const editor of draftEditors.filter(editor => editor.app === choice.id)) { editor.cancel().catch(error => reportError('Temporary cleanup needs attention. Keep external controls paused and retry.', error)); editor.group.visible = false; editor.modelGroup.visible = false; }
                drafts = drafts.filter(draft => draft.app !== choice.id);
                for (const profile of [...profiles].filter(profile => runtimeOf(profile) === choice.id)) {
                    profiles.splice(profiles.indexOf(profile), 1); rows.remove(profileRows.get(profile)); profileRows.delete(profile);
                }
                draftGeneration++; saveDrafts.sensitive = !pending;
            }
            invalidate();
        });
        applicationGroup.append(card); applicationCards.set(choice.id, {select, gear, detection});
    }
    const findApplication = linkButton(Gtk, 'Cannot find your application?');
    findApplication.connect('clicked', () => { status.label = 'Open an application’s settings (gear) to choose its existing location. Applications must already be installed.'; status.visible = true; });
    applicationPage.remove(settings); applicationPage.remove(draftRows);
    applicationPage.append(findApplication); applicationPage.append(settings); applicationPage.append(draftRows);
    settings.visible = false;
    const catalogFor = items => ({...request.catalog, version: items.some(profile => profile.nativeModel?.owned) ? 2 : request.catalog.version, profiles: items, disabled: items.length === 0});
    function appendDraft(initial, replacing = null, reveal = false) {
        if (drafts.some(draft => draft.id === initial.id)) { status.label = 'This workload already has an open selection. Finish or remove that selection first.'; return; }
        drafts.push(initial);
        let editor;
        editor = addDraftEditor({Adw, Gtk, Gio, window, parent: draftRows, initial, detected: discovered, discoveryErrors, command, modelParent: modelPage, temporaryStatus, reportProblem: reportError, openSettings: () => { invalidate(); setStep(0); },
            bind: async (profile, current, finish = false) => {
                replacing ??= profiles.find(existing => existing.id === initial.id) ?? null;
                if (replacing && !profiles.includes(replacing)) throw new Error('The original workload was removed. Reopen Manage workloads before editing it.');
                const preserveSettings = () => replacing ? {...replacing, ...profile, id: replacing.id, requiredMiB: replacing.requiredMiB, bootPolicy: replacing.bootPolicy} : profile;
                if (profiles.some(existing => existing !== replacing && (existing.id === profile.id || (profile.unit && existing.unit === profile.unit && existing.nativeModel?.model === profile.nativeModel?.model)))) throw new Error('This installation and model is already configured.');
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
                    ready = true; renderSummary(items); setStep(2); apply.sensitive = true;
                } else {
                    commit();
                    status.label = 'Application checked. Finish setup to confirm the changes.';
                }
            },
            changed: value => {
                if (drafts.find(item => item.id === initial.id)?.label !== value.label) editedLabels.delete(editor?.replacing ?? replacing);
                invalidate(); drafts = drafts.map(item => item.id === initial.id ? value : item); draftGeneration++; saveDrafts.sensitive = !pending;
            },
            removed: (finished = false) => { if (!finished) { invalidate(); } drafts = drafts.filter(item => item.id !== initial.id); draftGeneration++; saveDrafts.sensitive = !pending; },
            taken: () => profiles.map(profile => profile.id)});
        editor.group.visible = reveal; editor.replacing = replacing; draftEditors.push(editor);
    }
    saveDrafts.connect('clicked', async () => {
        const generation = draftGeneration;
        let saved = false;
        saveDrafts.sensitive = false;
        try {
            const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'save-drafts'], JSON.stringify({version: 1, expectedRevision: draftRevision, drafts})));
            draftRevision = result.revision; saved = true;
            status.label = 'Selections saved. Saved applications still need a check before they can run. They are not selectable in GPU Control until safe start and stop control is set up and checked.';
        } catch (error) { reportError('Selections were not saved. Reopen Manage workloads to refresh before retrying.', error); }
        finally { saveDrafts.sensitive = !pending && (!saved || generation !== draftGeneration); }
    });
    const later = new Gtk.Button({label: 'Set up later'});
    async function cancelEditors() {
        // Only this window's consented checks can be cancelled automatically.
        // Recovered sessions require a fresh paused-controls acknowledgement.
        await Promise.all(draftEditors.map(editor => editor.cancel()));
    }
    later.connect('clicked', async () => {
        try { await cancelEditors(); closed = true; window.close(); }
        catch (error) { reportError('Temporary application cleanup must finish before closing setup. Keep external application controls paused and retry.', error); }
    });
    const footer = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 8,
        margin_start: 24, margin_end: 24, margin_top: 12, margin_bottom: 12});
    footer.add_css_class('setup-footer');
    const actions = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 8});
    const back = new Gtk.Button({label: 'Back', visible: false});
    back.connect('clicked', async () => {
        invalidate();
        try { await cancelEditors(); setStep(step === 2 && draftEditors.some(editor => drafts.some(draft => draft.id === editor.id) && editor.needsModelDecision()) ? 1 : 0); }
        catch (error) { reportError('Temporary application cleanup must finish before going back. Retry cleanup.', error); }
    });
    actions.append(back); actions.append(later); actions.append(new Gtk.Box({hexpand: true})); actions.append(review); actions.append(apply); footer.append(actions);
    footer.add_css_class('toolbar');
    toolbar.add_bottom_bar(footer);
    const serialize = (items = profiles) => {
        // A pending activation must resume the recorded request without defaults or edits.
        if (pending) return JSON.stringify(request);
        if (!Number.isSafeInteger(request.profile.gpuIndex) || request.profile.gpuIndex < 0 ||
            items.some(profile => !Number.isSafeInteger(profile.requiredMiB ?? 0) || (profile.requiredMiB ?? 0) < 0))
            throw new Error('GPU index and VRAM requirements must be nonnegative whole numbers.');
        return JSON.stringify({...request, catalog: catalogFor(items), confirmQuiesced: false});
    };
    async function reviewConfiguration({items, current, commit}) {
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
            if (!pending) await command(['/usr/bin/gpu-setup', 'verify-bindings'], candidate.request);
            if (!current() || !reviewed.accept(candidate)) {
                status.label = 'Configuration changed during review. Review the updated configuration.';
                return;
            }
            commit();
            changes.label = preview.changes.join('\n');
            const removedProfiles = (request.catalog.profiles ?? []).filter(original => !items.some(profile => profile.id === original.id));
            const removal = removedProfiles.length ? ` Remove ${removedProfiles.map(profile => profile.label || profile.id).join(', ')} from Supervisor? Their applications and files are preserved.` : '';
            status.label = `Finish setup allows Supervisor to start and stop these workloads. Finish active jobs before continuing.${removal}`;
            valid = true; return true;
        } catch (error) {
            reportError('Review failed. Correct the fields above or reopen Manage workloads to refresh, then review again.', error);
            invalidate(); return false;
        }
        finally { review.sensitive = true; }
    }
    function preparedStillCurrent({editor, result}) {
        return result.current() && (!editor.replacing || profiles.includes(editor.replacing)) && (editor.staged ?? []).every(profile => profiles.includes(profile));
    }
    function preservePreparedProfile(editor, result, profile, index) {
        const sameModel = editor.replacing?.nativeModel?.model === profile.nativeModel?.model;
        const replacingModelKept = result.profiles.some(item => item.nativeModel?.model === editor.replacing?.nativeModel?.model);
        const previous = editor.staged?.find(existing => existing.nativeModel?.model === profile.nativeModel?.model) ?? (sameModel || (index === 0 && !replacingModelKept) ? editor.replacing : null);
        if (previous) {
            const updated = {...previous, ...profile, label: editedLabels.get(previous) ?? (profile.nativeModel?.model === editor.originalModel ? profile.label : previous.label), id: previous.id, requiredMiB: previous.requiredMiB, bootPolicy: previous.bootPolicy};
            if (editedLabels.has(previous)) editedLabels.set(updated, editedLabels.get(previous));
            return updated;
        }
        return profile;
    }
    function preparedProfiles({editor, result}) {
        return result.profiles.map((profile, index) => preservePreparedProfile(editor, result, profile, index));
    }
    async function continueSetup() {
        if (!request || closed) return;
        if (draftEditors.some(editor => editor.temporaryActive()) || (temporaryStatus?.session && temporaryStatus.session.status !== 'completed')) { status.label = 'Restore the stopped application before continuing.'; status.visible = true; return; }
        const editors = draftEditors.filter(editor => drafts.some(draft => draft.id === editor.id));
        if (step === 0 && editors.some(editor => editor.needsModelDecision())) { setStep(1); return; }
        review.sensitive = false;
        const generation = draftGeneration;
        try {
            const prepared = [];
            for (const editor of editors) prepared.push({editor, result: await editor.prepare()});
            const current = () => !closed && generation === draftGeneration && prepared.every(preparedStillCurrent);
            if (!current()) { status.label = 'The selected application changed while checking it. Reopen setup to refresh.'; status.visible = true; return; }
            const replaced = new Set(prepared.flatMap(({editor}) => editor.staged ?? (editor.replacing ? [editor.replacing] : [])));
            const additions = prepared.flatMap(preparedProfiles);
            const items = [...profiles.filter(profile => !replaced.has(profile)), ...additions];
            const keys = new Set(); const ids = new Set();
            for (const profile of items) {
                const key = `${profile.unit ?? ''}|${profile.nativeModel?.model ?? ''}`;
                if (ids.has(profile.id) || (profile.unit && keys.has(key))) throw new Error('This installation and model is already configured. Select it once.');
                ids.add(profile.id); keys.add(key);
            }
            const commit = () => {
                for (const profile of replaced) { profiles.splice(profiles.indexOf(profile), 1); rows.remove(profileRows.get(profile)); profileRows.delete(profile); }
                let index = 0;
                for (const {editor, result} of prepared) {
                    editor.staged = result.profiles.map(() => { const profile = additions[index++]; profileApps.set(profile.id, editor.app); return addProfile(profile, false); });
                    editor.replacing = editor.staged[0];
                }
            };
            if (await reviewConfiguration({items, current, commit})) {
                ready = true; renderSummary(items);
                setStep(2); apply.sensitive = true;
            }
        } catch (error) { reportError('Setup needs attention. Check the selected applications and models, then continue again.', error); invalidate(); }
        finally { review.sensitive = true; }
    }
    review.connect('clicked', continueSetup);
    apply.connect('clicked', async () => {
        if (!valid || !ready) return;
        const activationRequest = reviewed.confirmed();
        apply.sensitive = false; review.sensitive = false; add.sensitive = false;
        rows.sensitive = false; settings.sensitive = false;
        draftRows.sensitive = false; applicationGroup.sensitive = false; back.sensitive = false; saveDrafts.sensitive = false; later.sensitive = false;
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
    window.connect('close-request', () => {
        invalidate();
        if (draftEditors.some(editor => editor.temporaryActive())) {
            later.emit('clicked'); return true;
        }
        closed = true; draftEditors.forEach(editor => editor.cancel()); return false;
    });
    window.present();
    function addRecoveryCleanup() {
        if (!temporaryStatus.session || temporaryStatus.session.status === 'completed') return;
        const cleanupConsent = new Gtk.CheckButton({label: 'I have paused external application controls and finished any resumed work.'});
        const retryCleanup = new Gtk.Button({label: 'Restore stopped application', sensitive: false});
        cleanupConsent.connect('toggled', () => { retryCleanup.sensitive = cleanupConsent.active; });
        applicationPage.append(new Gtk.Label({label: 'An earlier temporary model check still blocks setup. Restoring its stopped state will stop this application. Finish resumed work and pause external application controls before restoring it. Leaving setup keeps the recovery record for next time.', wrap: true, xalign: 0}));
        applicationPage.append(cleanupConsent); applicationPage.append(retryCleanup);
        retryCleanup.connect('clicked', async () => {
            if (!cleanupConsent.active || temporaryStatus.session.status === 'completed') return;
            retryCleanup.sensitive = false;
            try {
                const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'temporary-cleanup'], JSON.stringify({id: temporaryStatus.session.id, token: temporaryStatus.session.token, externalControlPaused: true})));
                if (result.error || result.session?.status !== 'completed') throw new Error(result.error || 'Cleanup remains incomplete.');
                temporaryStatus.session = result.session; temporaryStatus.available = false; temporaryStatus.expected = undefined;
                cleanupConsent.sensitive = false;
                const refreshed = JSON.parse(await command(['/usr/bin/gpu-setup', 'temporary-status']));
                Object.assign(temporaryStatus, refreshed, {session: refreshed.session ?? result.session});
                status.label = 'Previous stopped state restored. Reopen setup to refresh detection.';
            } catch (error) { reportError(temporaryStatus.session.status === 'completed' ? 'The stopped state was restored, but model-check status could not be refreshed. Reopen setup before another temporary check.' : 'Temporary cleanup needs attention. Pause external controls and acknowledge again before retrying.', error); }
            finally { cleanupConsent.active = false; retryCleanup.sensitive = false; }
        });
    }
    function addConfiguredOllama() {
        // Existing Ollama models share one recognized installation. Ordinary
        // editing uses that application's model group, without adding it again.
        const existingOllama = profiles.filter(profile => runtimeOf(profile) === 'ollama');
        const recognizedOllama = discovered.filter(candidate => candidate.app === 'ollama' && candidate.recognized);
        if (!pending && existingOllama.length && recognizedOllama.length === 1 && !drafts.some(draft => draft.app === 'ollama')) {
            const current = existingOllama.find(profile => profile.unit === recognizedOllama[0].unit);
            if (current && recognizedOllama[0].models?.length > 1) {
                const related = existingOllama.filter(profile => profile.unit === current.unit);
                appendDraft({id: current.id, app: 'ollama', label: current.label, model: current.nativeModel.model, models: related.map(profile => profile.nativeModel.model), endpoint: current.nativeModel.endpoint,
                    binding: current.nativeModel.owned ? {instance: current.nativeModel.instance, owned: {...current.nativeModel.owned}} : {unit: current.unit, cgroup: current.cgroup, healthURL: current.healthURL, instance: current.nativeModel.instance, model: current.nativeModel.model, launchFile: current.nativeModel.launchFile}}, current);
                draftEditors.at(-1).staged = related;
            }
        }
    }
    function updateApplicationCards() {
        for (const [appID, card] of applicationCards) {
            const candidates = discovered.filter(candidate => candidate.app === appID);
            const known = candidates.filter(candidate => candidate.recognized);
            card.detection.label = detectionMessage(known, candidates);
            card.select.sensitive = !pending; card.gear.sensitive = !pending;
            card.select.active = drafts.some(draft => draft.app === appID) || profiles.some(profile => runtimeOf(profile) === appID);
        }
    }
    function detectionMessage(known, candidates) {
        const states = {'unreachable': 'Unreachable · Open settings', 'discovery-error': 'Detection failed · Open settings', 'missing': 'Location missing · Open settings', 'invalid': 'Configuration unreadable · Open settings', 'unsupported': 'Unsupported · Open settings'};
        if (discoveryErrors.length && !known.length) return 'Detection failed · Open settings';
        if (known.length > 1) return 'Choose an installation in settings.';
        if (!known.length) return states[candidates[0]?.instanceStatus] ?? 'Not detected';
        const state = states[known[0].instanceStatus];
        if (state) return state;
        return known[0].instanceStatus === 'not-running' ? 'Installed and stopped. Ready to configure.' : 'Detected';
    }
    const initialized = (async () => {
        try {
            const version = await command(['/usr/bin/gnome-shell', '--version']);
            if (!/\b50(?:\.|\s|$)/.test(version) || !GLib.getenv('XDG_CURRENT_DESKTOP')?.includes('GNOME'))
                throw new Error('This package requires a GNOME Shell 50 desktop session.');
            const discovery = JSON.parse(await command(['/usr/bin/gpu-setup', 'discover']));
            request = discovery.request; units = discovery.units; pending = Boolean(discovery.pending); discovered = discovery.applications ?? []; discoveryErrors = discovery.errors ?? [];
            if (!pending) {
                temporaryStatus = JSON.parse(await command(['/usr/bin/gpu-setup', 'temporary-status']));
                const saved = JSON.parse(await command(['/usr/bin/gpu-setup', 'drafts']));
                draftRevision = saved.revision ?? '';
                for (const draft of saved.drafts ?? []) appendDraft(draft);
                addRecoveryCleanup();
            }
            applicationGroup.sensitive = !pending;
            status.label = pending ? 'Interrupted setup found. Review and resume its original configuration.' :
                'Select applications below. Discovery does not start applications or load models.';
            status.visible = pending;
            field(advanced, 'State database path', request.profile.statePath, text => request.profile.statePath = text);
            field(advanced, 'NVIDIA GPU index', request.profile.gpuIndex, text => request.profile.gpuIndex = Number(text));
            for (const profile of request.catalog.profiles ?? []) addProfile(profile);
            for (const editor of draftEditors) {
                editor.replacing ??= profiles.find(profile => profile.id === editor.id) ?? null;
                if (editor.replacing) editor.staged = profiles.filter(profile => runtimeOf(profile) === editor.app && profile.unit === editor.replacing.unit);
            }
            addConfiguredOllama();
            add.sensitive = !pending; rows.sensitive = !pending; settings.sensitive = !pending;
            updateApplicationCards();
            review.sensitive = true;
            if (discoveryErrors.length) reportError('Some installations could not be checked. Choose an application location or reopen setup to retry discovery.', new Error(discoveryErrors.join('\n')));
        } catch (error) { status.label = error.message; status.visible = true; review.sensitive = false; }
    })();
    return {window, applicationCards, heading, continueSetup, back, initialized, review, apply, status};
}
