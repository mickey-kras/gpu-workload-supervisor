// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
import {applicationHeader, roundedCard} from './presentation.mjs';
import {createShell, createSettings, createApplicationCards, createFooter} from './setup-layout.mjs';
import {ReviewedConfiguration, lowestCatalogVersion} from './review.mjs';
import {createProfileEditor} from './profile-ui.mjs';
import {addDraftEditor} from './discovery-ui.mjs';
import {applications, candidateDiagnostics} from './onboarding.mjs';

// The caller supplies transport; fixture callers never invoke the production subprocess.
export function createSetupWindow(options) { const ui = new SetupWindow(options); ui.start(); return ui; }

class SetupWindow {
    constructor({app, command}) {
        this.app = app; this.command = command;
        this.step = 0;
        this.ready = false;
        this.closed = false;
        this.discovered = [];
        this.discoveryErrors = [];
        this.draftRevision = '';
        this.drafts = [];
        this.draftGeneration = 0;
        this.draftEditors = [];
        this.request = null;
        this.pending = false;
        this.units = [];
        this.valid = false;
        this.profiles = [];
        this.profileApps = new Map();
        this.profileRows = new Map();
        this.reviewed = new ReviewedConfiguration();
        this.editedLabels = new WeakMap();
        this.applicationCards = new Map();
        this.suspendedApplications = new Map();
        for (const name of ['field', 'invalidate', 'preparedStillCurrent', 'preparedProfile',
            'continueSetup', 'saveDraftSelections', 'postponeSetup', 'goBack', 'activateConfiguration', 'closeRequested'])
            this[name] = this[name].bind(this);
        createShell(this); createSettings(this); createApplicationCards(this); createFooter(this);
        this.review.connect('clicked', () => {
            const editor = this.applicationSettings?.editor;
            if (!editor || (['main', 'advanced'].includes(this.applicationSettings.page) && (editor.needsModelDecision() || this.drafts.some(draft => draft.id !== editor.id)))) return this.continueSetup();
            return editor.settingsAction();
        });
        this.apply.connect('clicked', this.activateConfiguration);
        this.window.connect('close-request', this.closeRequested);
    }

    start() {
        this.window.present(); this.initialized = this.initialize();
        return this.initialized;
    }

    renderSummary(items) {
        for (const card of this.summaryCards) this.summary.remove(card);
        this.summaryCards.length = 0;
        for (const profile of items) {
            const appID = this.runtimeOf(profile);
            const card = roundedCard(Gtk); card.add_css_class('setup-summary-card');
            if (appID) card.append(applicationHeader(Gtk, appID, profile.label || profile.id));
            else card.append(new Gtk.Label({label: profile.label || profile.id, wrap: true, xalign: 0}));
            this.summary.append(card); this.summaryCards.push(card);
        }
        if (!items.length) { const label = new Gtk.Label({label: 'No workloads selected. Supervisor configuration will be updated.', wrap: true, xalign: 0}); this.summary.append(label); this.summaryCards.push(label); }
    }

    addProfile(profile, resetReview = true) {
        if (resetReview) this.invalidate();
        const current = {...profile};
        if (this.editedLabels.has(profile)) this.editedLabels.set(current, this.editedLabels.get(profile));
        this.profiles.push(current);
        const removalOnly = ['ollama', 'llama.cpp'].includes(current.nativeModel?.runtime);
        const parent = this.profileContainer(current);
        const group = createProfileEditor({current, Adw, Gtk, GLib, units: this.units, field: this.field, invalidate: this.invalidate,
            applicationRuntime: this.profileApps.get(current.id), removalOnly,
            onLabelEdit: label => this.editedLabels.set(current, label),
            onRemove: group => {
                this.profiles.splice(this.profiles.indexOf(current), 1);
                parent.remove(group);
                this.profileRows.delete(current);
                this.invalidate();
            },
            onEdit: draft => { const editor = this.appendDraft(draft, current, true); if (this.applicationSettings && editor) this.openApplicationSettings(editor, this.applicationSettings.origin); }});
        parent.append(group); this.profileRows.set(current, group); return current;
    }

    profileContainer(profile) {
        return ['ollama', 'llama.cpp'].includes(profile.nativeModel?.runtime) ? this.draftRows : this.rows;
    }

    setStep(next) {
        if (next !== 0 && this.applicationSettings) this.closeApplicationSettings(false);
        this.step = next;
        this.box.spacing = this.step === 2 ? 10 : 14;
        this.heading.label = ['Choose your applications', 'Choose models', 'Ready to finish'][this.step];
        this.introduction.label = ['Use applications already installed on this computer.', 'The selected model appears as a workload in GPU Control.', 'Supervisor will switch between these workloads.'][this.step];
        this.applicationPage.visible = this.step === 0; this.modelPage.visible = this.step === 1; this.finishPage.visible = this.step === 2;
        this.draftRows.visible = this.step === 0;
        this.setupSettings.visible = this.step === 0; this.back.visible = this.step > 0; this.later.visible = this.step === 0; this.status.visible = this.step === 2 || this.pending; this.apply.visible = this.step === 2; this.review.visible = this.step !== 2;
        this.review.label = 'Continue'; this.review.sensitive = Boolean(this.request);
        (this.step > 0 ? this.back : this.findApplication).grab_focus();
        this.scroll.get_vadjustment().value = 0;
    }

    openApplicationSettings(editor, origin) {
        this.invalidate();
        if (!this.applicationSettings) this.applicationSettings = {step: this.step, origin, scroll: this.scroll.get_vadjustment().value};
        this.setStep(0);
        this.applicationSettings.editor = editor;
        this.heading.label = applications.find(app => app.id === editor.app).label;
        this.heading.visible = false; this.settingsBack.visible = true; this.back.visible = false;
        this.settingsTitle.visible = true; this.headerBar.set_title_widget(this.settingsTitle);
        this.introduction.visible = false;
        this.applicationGroup.visible = false; this.findApplication.visible = false; this.settings.visible = false;
        this.setupSettings.visible = false; this.back.visible = false;
        this.saveDrafts.visible = false; this.later.visible = true; this.later.label = 'Cancel';
        for (const draftEditor of this.draftEditors) draftEditor.group.visible = draftEditor === editor;
        editor.showSettings(editor.replacing);
        this.settingsBack.grab_focus();
    }

    closeApplicationSettings(restoreFocus = true) {
        const saved = this.applicationSettings;
        if (!saved) return;
        this.applicationSettings = null;
        this.saveDrafts.visible = false; this.introduction.visible = true; this.heading.visible = true; this.settingsBack.visible = false; this.settingsTitle.visible = false; this.headerBar.set_title_widget(null); this.later.label = 'Set up later';
        for (const editor of this.draftEditors) editor.group.visible = false;
        this.applicationGroup.visible = true; this.findApplication.visible = true;
        this.setStep(saved.step);
        GLib.idle_add(GLib.PRIORITY_DEFAULT_IDLE, () => {
            if (this.closed) return GLib.SOURCE_REMOVE;
            if (restoreFocus) saved.origin.grab_focus();
            this.scroll.get_vadjustment().value = saved.scroll;
            return GLib.SOURCE_REMOVE;
        });
    }

    navigateApplicationSettings(editor, {title, page, ready, busy}) {
        if (!editor || !this.applicationSettings || this.applicationSettings.editor !== editor) return;
        const previous = this.applicationSettings.page;
        this.applicationSettings.scrolls ??= new Map();
        if (previous !== page) this.applicationSettings.scrolls.set(previous, this.scroll.get_vadjustment().value);
        this.applicationSettings.page = page;
        this.heading.label = title; this.settingsTitle.label = title;
        this.review.label = {main: 'Use installation', configuration: 'Check again', advanced: 'Review changes'}[page] ?? 'Done';
        this.review.sensitive = !busy && (page === 'main' ? ready : true);
        this.status.visible = false;
        if (previous !== page) {
            this.scroll.get_vadjustment().value = this.applicationSettings.scrolls.get(page) ?? 0;
            this.settingsBack.grab_focus();
        }
    }

    selectApplication(choice) {
        const suspended = this.suspendedApplications.get(choice.id);
        if (suspended) {
            for (const {profile, row} of suspended.profiles) {
                this.profiles.push(profile); this.rows.append(row); this.profileRows.set(profile, row);
            }
            this.drafts.push(...suspended.drafts);
            for (const editor of suspended.editors) editor.modelGroup.visible = true;
            this.suspendedApplications.delete(choice.id); this.draftGeneration++; this.saveDrafts.sensitive = !this.pending;
            if (suspended.profiles.length || suspended.drafts.length) return;
        }
        if (this.drafts.some(draft => draft.app === choice.id) || this.profiles.some(profile => this.runtimeOf(profile) === choice.id)) return;
        this.appendDraft({id: `draft-${GLib.uuid_string_random()}`, label: choice.label, app: choice.id});
        this.draftGeneration++; this.saveDrafts.sensitive = true;
    }

    appendDraft(initial, replacing = null, reveal = false) {
        if (this.drafts.some(draft => draft.id === initial.id)) { this.status.label = 'This workload already has an open selection. Finish or remove that selection first.'; return this.draftEditors.find(editor => editor.id === initial.id); }
        this.drafts.push(initial);
        let editor;
        editor = addDraftEditor({Adw, Gtk, Gio, window: this.window, parent: this.draftRows, initial, detected: this.discovered, discovered: discovery => this.updateDiscovery(discovery), gpuUUIDs: this.gpuUUIDs, gpuInventoryStatus: this.gpuInventoryStatus, discoveryErrors: this.discoveryErrors, command: this.command, modelParent: this.modelPage, reportProblem: this.reportError, navigateSettings: state => this.navigateApplicationSettings(editor, state), saveSelections: () => this.saveDraftSelections(), openSettings: origin => this.openApplicationSettings(editor, origin),
            bind: async (profile, current, finish = false) => {
                replacing ??= this.profiles.find(existing => existing.id === initial.id) ?? null;
                if (replacing && !this.profiles.includes(replacing)) throw new Error('The original workload was removed. Reopen Manage workloads before editing it.');
                const preserveSettings = () => replacing ? {...replacing, ...profile, id: replacing.id, requiredMiB: replacing.requiredMiB, bootPolicy: replacing.bootPolicy, ...editor.settingsOverrides()} : {...profile, ...editor.settingsOverrides()};
                if (this.profiles.some(existing => existing !== replacing && (existing.id === profile.id || (profile.unit && existing.unit === profile.unit && existing.nativeModel?.model === profile.nativeModel?.model)))) throw new Error('This installation and model is already configured.');
                const candidate = JSON.stringify({...this.request, catalog: this.catalogFor([...this.profiles.filter(existing => existing !== replacing), preserveSettings()]), confirmQuiesced: false});
                await this.command(['/usr/bin/gpu-setup', 'verify-bindings'], candidate);
                if (replacing && !this.profiles.includes(replacing)) throw new Error('The original workload changed during preview. Reopen Manage workloads.');
                if (!current()) return;
                const prepared = preserveSettings();
                const commit = () => {
                    if (replacing) {
                        this.profiles.splice(this.profiles.indexOf(replacing), 1);
                        this.profileContainer(replacing).remove(this.profileRows.get(replacing)); this.profileRows.delete(replacing);
                    }
                    this.profileApps.set(prepared.id, initial.app);
                    this.addProfile(prepared, !finish);
                };
                if (finish) {
                    if (this.drafts.some(draft => draft.id !== initial.id)) throw new Error('Other included applications still need a check. Return to setup and Continue to check every selected installation.');
                    const items = [...this.profiles.filter(existing => existing !== replacing), prepared];
                    if (!await this.reviewConfiguration({items, current, commit}))
                        throw new Error('The configuration needs another check before confirmation. Your draft is kept.');
                    this.ready = true; this.renderSummary(items); this.setStep(2); this.apply.sensitive = true;
                } else {
                    commit();
                    this.status.label = 'Application checked. Finish setup to confirm the changes.';
                }
            },
            changed: value => {
                if (this.drafts.find(item => item.id === initial.id)?.label !== value.label) this.editedLabels.delete(editor?.replacing ?? replacing);
                this.invalidate(); this.drafts = this.drafts.map(item => item.id === initial.id ? value : item); this.draftGeneration++; this.saveDrafts.sensitive = !this.pending;
            },
            removed: (finished = false) => { if (!finished) { this.invalidate(); } editor.group.visible = false; editor.modelGroup.visible = false; this.drafts = this.drafts.filter(item => item.id !== initial.id); this.draftEditors = this.draftEditors.filter(item => item !== editor); this.draftGeneration++; this.saveDrafts.sensitive = !this.pending; },
            taken: () => this.profiles.map(profile => profile.id)});
        editor.group.visible = reveal; editor.replacing = replacing; this.draftEditors.push(editor);
        return editor;
    }

    appendUnsupportedDraft(draft) {
        this.drafts.push(draft);
        const group = new Adw.PreferencesGroup({title: draft.label, description: `${draft.app} is no longer supported. Remove this selection, then save.`});
        const remove = new Gtk.Button({label: `Remove saved selection: ${draft.label}`});
        group.add(remove); this.draftRows.append(group);
        remove.connect('clicked', () => {
            this.invalidate(); this.draftRows.remove(group);
            this.drafts = this.drafts.filter(item => item !== draft);
            this.draftGeneration++; this.saveDrafts.sensitive = !this.pending;
        });
    }

    cancelEditors() {
        for (const editor of this.draftEditors) editor.cancel();
    }

    async reviewConfiguration({items, current, commit}) {
        if (!this.request) return;
        this.invalidate(); this.review.sensitive = false;
        try {
            const candidate = this.reviewed.begin(this.serialize(items));
            if (!this.pending) {
                const snapshot = JSON.parse(candidate.request);
                for (const profile of snapshot.catalog.profiles) {
                    if (!profile.nativeModel || profile.nativeModel.owned) continue;
                    const result = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'fingerprint'], JSON.stringify({binding: profile.nativeModel})));
                    profile.nativeModel.launchSHA256 = result.sha256;
                }
                candidate.request = JSON.stringify(snapshot);
            }
            const preview = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'validate'], candidate.request));
            if (!this.pending) await this.command(['/usr/bin/gpu-setup', 'verify-bindings'], candidate.request);
            if (!current() || !this.reviewed.accept(candidate)) {
                this.status.label = 'Configuration changed during review. Review the updated configuration.';
                return;
            }
            commit();
            this.changes.label = preview.changes.join('\n');
            const removedProfiles = (this.request.catalog.profiles ?? []).filter(original => !items.some(profile => profile.id === original.id));
            const removal = removedProfiles.length ? ` Remove ${removedProfiles.map(profile => profile.label || profile.id).join(', ')} from Supervisor? Their applications and files are preserved.` : '';
            this.status.label = `Finish setup allows Supervisor to start and stop these workloads. Finish active jobs before continuing.${removal}`;
            this.valid = true; return true;
        } catch (error) {
            this.reportError('Review failed. Correct the fields above or reopen Manage workloads to refresh, then review again.', error);
            this.invalidate(); return false;
        }
        finally { this.review.sensitive = true; }
    }

    preparedStillCurrent({editor, result}) {
        return result.current() && (!editor.replacing || this.profiles.includes(editor.replacing));
    }

    preparedProfile({editor, result}) {
        const {profile} = result;
        const previous = editor.replacing;
        if (previous) {
            const updated = {...previous, ...profile, label: this.editedLabels.get(previous) ?? (profile.nativeModel?.model === editor.originalModel ? profile.label : previous.label), id: previous.id, requiredMiB: previous.requiredMiB, bootPolicy: previous.bootPolicy, ...editor.settingsOverrides()};
            if (this.editedLabels.has(previous)) this.editedLabels.set(updated, this.editedLabels.get(previous));
            return updated;
        }
        return {...profile, ...editor.settingsOverrides()};
    }

    async continueSetup() {
        if (this.drafts.some(draft => !applications.some(app => app.id === draft.app))) {
            this.status.label = 'Remove unsupported saved selections, then save before continuing.'; this.status.visible = true; return;
        }
        if (!this.request || this.closed) return;
        this.closeApplicationSettings(false);
        const editors = this.draftEditors.filter(editor => this.drafts.some(draft => draft.id === editor.id));
        if (editors.some(editor => editor.checking())) { this.status.label = 'Wait for the installation check to finish before continuing.'; this.status.visible = true; return; }
        if (this.step === 0 && editors.some(editor => editor.needsModelDecision())) { this.setStep(1); return; }
        this.review.sensitive = false;
        const generation = this.draftGeneration;
        try {
            const prepared = [];
            for (const editor of editors) prepared.push({editor, result: await editor.prepare()});
            const current = () => !this.closed && generation === this.draftGeneration && prepared.every(this.preparedStillCurrent);
            if (!current()) { this.status.label = 'The selected application changed while checking it. Reopen setup to refresh.'; this.status.visible = true; return; }
            const replaced = new Set(prepared.flatMap(({editor}) => editor.replacing ? [editor.replacing] : []));
            const additions = prepared.map(this.preparedProfile);
            const items = [...this.profiles.filter(profile => !replaced.has(profile)), ...additions];
            const keys = new Set(); const ids = new Set();
            for (const profile of items) {
                const key = `${profile.unit ?? ''}|${profile.nativeModel?.model ?? ''}`;
                if (ids.has(profile.id) || (profile.unit && keys.has(key))) throw new Error('This installation and model is already configured. Select it once.');
                ids.add(profile.id); keys.add(key);
            }
            const commit = () => {
                for (const profile of replaced) { this.profiles.splice(this.profiles.indexOf(profile), 1); this.profileContainer(profile).remove(this.profileRows.get(profile)); this.profileRows.delete(profile); }
                prepared.forEach(({editor}, index) => {
                    const profile = additions[index];
                    this.profileApps.set(profile.id, editor.app);
                    editor.replacing = this.addProfile(profile, false);
                });
            };
            if (await this.reviewConfiguration({items, current, commit})) {
                this.ready = true; this.renderSummary(items);
                this.setStep(2); this.apply.sensitive = true;
            }
        } catch (error) { this.reportError('Setup needs attention. Check the selected applications and models, then continue again.', error); this.invalidate(); }
        finally { this.review.sensitive = true; }
    }

    updateDiscovery(discovery) {
        this.discovered = discovery.applications ?? [];
        this.gpuUUIDs = discovery.gpuUUIDs ?? [];
        this.gpuInventoryStatus = discovery.gpuInventoryStatus;
        this.discoveryErrors = discovery.errors ?? [];
        for (const editor of this.draftEditors) editor.updateDiscovery(discovery);
        for (const [appID, card] of this.applicationCards) {
            const candidates = this.discovered.filter(candidate => candidate.app === appID);
            card.detection.label = this.detectionMessage(candidates.filter(candidate => candidate.recognized), candidates);
        }
    }

    updateApplicationCards() {
        for (const [appID, card] of this.applicationCards) {
            const candidates = this.discovered.filter(candidate => candidate.app === appID);
            const known = candidates.filter(candidate => candidate.recognized);
            card.detection.label = this.detectionMessage(known, candidates);
            card.select.sensitive = !this.pending; card.gear.sensitive = !this.pending;
            card.select.active = this.drafts.some(draft => draft.app === appID) || this.profiles.some(profile => this.runtimeOf(profile) === appID);
        }
    }

    detectionMessage(known, candidates) {
        const states = {'unreachable': 'Address unreachable · Installation unverified', 'inspection-failed': 'Inspection failed · Open settings', 'discovery-error': 'Detection failed · Open settings', 'missing': 'Location missing · Open settings', 'invalid': 'Configuration unreadable · Open settings', 'unsupported': 'Unsupported · Open settings'};
        if (known.length > 1) return 'Choose an installation in settings.';
        const selected = known[0] ?? candidates.find(candidate => candidate.sourceKind === 'configuration' || candidate.unit) ?? candidates.find(candidate => candidate.installed) ?? candidates[0];
        if (candidateDiagnostics(selected).length > 1) return 'Configuration needs attention · Open settings';
        if (selected?.installed && !selected?.recognized) return 'Installed · Qualification blocked · Open settings';
        const configuration = {'inspection-failed': 'Inspection failed · Open settings', 'model-missing': 'Configured model missing · Open settings', 'gpu-unresolved': 'GPU identity needs setup · Open settings'}[selected?.configurationStatus];
        if (configuration) return configuration;
        if (!known.length) return states[selected?.instanceStatus] ?? (selected?.instanceStatus === 'not-running' && !selected.unit ? 'Default address unavailable · Installation unverified' : 'Not detected');
        if (selected.instanceStatus === 'unreachable') return 'Installed · Address unreachable · Ready to configure';
        const state = states[selected.instanceStatus];
        if (state) return state;
        if (selected.configurationStatus === 'model-required') return 'Installed · Choose an existing model';
        if (selected.instanceStatus === 'not-running') return 'Installed and stopped. Ready to configure.';
        if (selected.instanceStatus === 'installed') return 'Installed · Preview a managed launch';
        return 'Detected';
    }

    invalidate() {
        this.ready = false;
        this.reviewed.invalidate(); this.valid = false; this.apply.sensitive = false;
    }

    field(parent, title, value, changed) {
        const row = new Adw.EntryRow({title, text: String(value ?? '')});
        row.connect('changed', () => { changed(row.text); this.invalidate(); });
        if (parent instanceof Adw.ExpanderRow) parent.add_row(row);
        else parent.add(row);
        return row;
    }

    runtimeOf(profile) { return profile.nativeModel?.runtime ?? profile.launchBinding?.runtime ?? this.profileApps.get(profile.id) ?? (profile.adapter === 'comfyui' ? 'comfyui' : undefined); }

    catalogFor(items) { return ({...this.request.catalog, version: lowestCatalogVersion(items), profiles: items, disabled: items.length === 0}); }

    serialize(items = this.profiles) {
        // A pending activation must resume the recorded request without defaults or edits.
        if (this.pending) return JSON.stringify(this.request);
        if (!Number.isSafeInteger(this.request.profile.gpuIndex) || this.request.profile.gpuIndex < 0 ||
            items.some(profile => !Number.isSafeInteger(profile.requiredMiB ?? 0) || (profile.requiredMiB ?? 0) < 0))
            throw new Error('GPU index and VRAM requirements must be nonnegative whole numbers.');
        return JSON.stringify({...this.request, catalog: this.catalogFor(items), confirmQuiesced: false});
    }

    async saveDraftSelections() {
        const generation = this.draftGeneration;
        let saved = false;
        this.saveDrafts.sensitive = false;
        try {
            if (this.drafts.some(draft => !Number.isSafeInteger(draft.requiredMiB ?? 0) || (draft.requiredMiB ?? 0) < 0))
                throw new Error('VRAM requirements must be nonnegative whole numbers within the supported numeric range.');
            const result = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'save-drafts'], JSON.stringify({version: 1, expectedRevision: this.draftRevision, drafts: this.drafts})));
            this.draftRevision = result.revision; saved = true;
            this.status.label = 'Selections saved. Saved applications still need a check before they can run. They are not selectable in GPU Control until safe start and stop control is set up and checked.';
        } catch (error) { this.reportError('Selections were not saved. Check the VRAM requirements or reopen Manage workloads to refresh before retrying.', error); }
        finally { this.saveDrafts.sensitive = !this.pending && (!saved || generation !== this.draftGeneration); }
    }

    postponeSetup() {
        if (this.applicationSettings?.editor) {
            this.applicationSettings.editor.cancelSettings(); this.closeApplicationSettings();
            return;
        }
        this.cancelEditors(); this.closed = true; this.releasePresentation(); this.window.close();
    }

    goBack() {
        this.invalidate();
        if (this.applicationSettings?.editor?.settingsBack()) return;
        this.cancelEditors();
        if (this.applicationSettings) this.closeApplicationSettings(); else this.setStep(this.step === 2 && this.draftEditors.some(editor => this.drafts.some(draft => draft.id === editor.id) && editor.needsModelDecision()) ? 1 : 0);
    }

    async activateConfiguration() {
        if (!this.valid || !this.ready) return;
        const activationRequest = this.reviewed.confirmed();
        this.apply.sensitive = false; this.review.sensitive = false; this.add.sensitive = false;
        this.rows.sensitive = false; this.settings.sensitive = false;
        this.draftRows.sensitive = false; this.applicationGroup.sensitive = false; this.back.sensitive = false; this.saveDrafts.sensitive = false; this.later.sensitive = false;
        this.status.label = 'Applying configuration. Keep this window open; interrupted activation can be resumed.';
        try {
            await this.command(['/usr/bin/gpu-setup', 'apply'], activationRequest);
            this.status.label = 'Configuration activated. Reconciliation is enabled for future logins. Log out and back in to discover the extension, then enable “GPU Workload Supervisor” in Extensions. GPU Control appears in the top-right Quick Settings menu. No workload was started.';
        } catch (error) {
            this.reportError('Setup needs attention. Switch to Idle and finish active jobs before changing configured workloads. Reopen Manage workloads to refresh or resume an interrupted activation. State and backups are preserved.', error);
            this.review.sensitive = true; this.later.sensitive = true;
        }
        finally { this.invalidate(); }
    }

    closeRequested() {
        this.invalidate();
        this.closed = true; this.cancelEditors(); this.releasePresentation(); return false;
    }

    async initialize() {
        try {
            const version = await this.command(['/usr/bin/gnome-shell', '--version']);
            if (!/\b50(?:\.|\s|$)/.test(version) || !GLib.getenv('XDG_CURRENT_DESKTOP')?.includes('GNOME'))
                throw new Error('This package requires a GNOME Shell 50 desktop session.');
            const discovery = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'discover']));
            this.request = discovery.request; this.units = discovery.units; this.pending = Boolean(discovery.pending); this.discovered = discovery.applications ?? []; this.gpuUUIDs = discovery.gpuUUIDs ?? []; this.gpuInventoryStatus = discovery.gpuInventoryStatus; this.discoveryErrors = discovery.errors ?? [];
            let draftWarning = '';
            if (!this.pending) {
                const saved = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'drafts']));
                this.draftRevision = saved.revision ?? '';
                draftWarning = saved.warning ?? '';
                for (const draft of saved.drafts ?? []) {
                    if (applications.some(app => app.id === draft.app)) this.appendDraft(draft);
                    else this.appendUnsupportedDraft(draft);
                }
            }
            this.applicationGroup.sensitive = !this.pending;
            this.status.label = this.pending ? 'Interrupted setup found. Review and resume its original configuration.' :
                'Select applications below. Discovery does not start applications or load models.';
            this.status.visible = this.pending;
            this.field(this.advanced, 'State database path', this.request.profile.statePath, text => this.request.profile.statePath = text);
            this.field(this.advanced, 'NVIDIA GPU index', this.request.profile.gpuIndex, text => this.request.profile.gpuIndex = Number(text));
            for (const profile of this.request.catalog.profiles ?? []) this.addProfile(profile);
            for (const editor of this.draftEditors) editor.replacing ??= this.profiles.find(profile => profile.id === editor.id) ?? null;
            this.add.sensitive = !this.pending; this.rows.sensitive = !this.pending; this.draftRows.sensitive = !this.pending; this.settings.sensitive = !this.pending;
            this.updateApplicationCards();
            this.review.sensitive = true;
            if (draftWarning) this.reportError('Remove unsupported saved selections, then save. Other selections are kept.', new Error(draftWarning));
            if (this.discoveryErrors.length) this.reportError('Some installations could not be checked. Choose an application location or reopen setup to retry discovery.', new Error(this.discoveryErrors.join('\n')));
        } catch (error) { this.status.label = error.message; this.status.visible = true; this.review.sensitive = false; }
    }
}


