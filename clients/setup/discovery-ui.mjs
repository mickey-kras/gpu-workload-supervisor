import {applicationHeader} from './presentation.mjs';
import {InstallationSettings, installationChoiceLabel} from './settings-pages.mjs';
import {TemporaryDiscovery} from './temporary-discovery-ui.mjs';
import {watchInstanceSelection, addRefreshButton, addFilePickers} from './discovery-inputs.mjs';
import {addOwnedEditor, addBindingEditor} from './launch-ui.mjs';
import {ApplicationDraft, applications, candidateChoice, candidateIdentity, profileIDFromModel} from './onboarding.mjs';

// Backend error detail stays available but collapsed behind the actionable summary.
export function addErrorReporter({Adw, Gtk, parent, status}) {
    const details = new Adw.ExpanderRow({title: 'Technical details', visible: false});
    const text = new Gtk.Label({wrap: true, xalign: 0, selectable: true});
    details.add_row(text);
    if (parent.add) parent.add(details); else parent.append(details);
    return (summary, error) => {
        status.label = summary; status.visible = true;
        text.label = error.message;
        details.expanded = false;
        details.visible = true;
    };
}

export function addDraftEditor(options) { return new DraftEditor(options).view(); }

class DraftEditor {
    constructor({Adw, Gtk, window, parent, initial, detected, discoveryErrors, command, changed, removed, bind, taken, modelParent, temporaryStatus, openSettings, reportProblem, navigateSettings, saveSelections}) {
        this.Adw = Adw;
        this.Gtk = Gtk;
        this.window = window;
        this.parent = parent;
        this.initial = initial;
        this.detected = detected;
        this.discoveryErrors = discoveryErrors;
        this.command = command;
        this.changed = changed;
        this.removed = removed;
        this.bind = bind;
        this.taken = taken;
        this.modelParent = modelParent;
        this.temporaryStatus = temporaryStatus;
        this.openSettings = openSettings; this.reportProblem = reportProblem; this.navigateSettings = navigateSettings; this.saveSelections = saveSelections;
        this.discoveryErrors ??= [];
        this.syncing = false; this.bindingFields = {}; this.ownedFields = {}; this.overrides = {}; this.bindingEdits = new Set();
        if (Object.hasOwn(initial, 'requiredMiB')) this.overrides.requiredMiB = initial.requiredMiB || undefined;
        if (Object.hasOwn(initial, 'bootPolicy')) this.overrides.bootPolicy = initial.bootPolicy || undefined;
        this.installationEvidence = null;
        this.buildBasics(); this.connectAddress(); this.buildModels();
        this.connectModelPicker();
        this.temporary = new TemporaryDiscovery({Gtk, draft: this.draft, app: initial.app, modelGroup: this.modelGroup, status: this.modelStatus ?? this.status, temporaryStatus, command, changed, reportError: this.reportError, show: candidate => this.show(candidate), getEvidence: () => this.installationEvidence});
        this.connectInputs(); this.buildResources(); this.addActions(); this.initialDiscovery();
    }
    buildBasics() {
        this.draft = new ApplicationDraft(this.initial.app);
        this.draft.edit(this.initial);
        this.settings = new InstallationSettings({Adw: this.Adw, Gtk: this.Gtk, app: this.initial.app, window: this.window, navigate: this.navigateSettings});
        this.group = this.settings.root;
        this.name = new this.Adw.EntryRow({title: 'Display name', text: this.initial.label});
        this.name.update_property([this.Gtk.AccessibleProperty.LABEL], ['Installation display name']);
        this.settings.general.add(this.name);
        this.name.connect('changed', () => { if (this.syncing) return; this.draft.edit({label: this.name.text}); this.changed(this.draft.snapshot()); });
        this.status = this.settings.status;
        this.reportError = (summary, error) => {
            this.settings.problem(summary, error);
            if (this.modelStatus) this.modelStatus.label = summary;
            if (!this.group.visible) this.reportProblem?.(summary, error);
        };
        this.probeGuidance = null;
        this.instances = this.detected.filter(candidate => candidate.app === this.initial.app);
        const original = this.instances.find(candidate => candidate.unit === this.initial.binding?.unit);
        for (const [key, value] of Object.entries(this.initial.binding ?? {})) {
            if (value && value !== original?.binding?.[key]) this.bindingEdits.add(key);
        }
        this.endpointEdited = Boolean(this.initial.endpoint && this.initial.endpoint !== original?.endpoint);
        // The index remains the shared selection controller. The visible picker
        // uses wrapped, accessible rows instead of an elided combo popup.
        this.instance = new this.Adw.ComboRow({title: 'Detected instance', visible: false, use_markup: false,
            model: this.Gtk.StringList.new(['Choose an instance...', ...this.instances.map(candidateChoice)]), selected: 0});
        this.settings.selection.add(this.instance);
        for (const [index, candidate] of this.instances.entries()) {
            const button = new this.Gtk.Button({hexpand: true});
            const identity = candidateChoice(candidate);
            button.set_child(installationChoiceLabel(this.Gtk, identity));
            button.update_property([this.Gtk.AccessibleProperty.LABEL], [identity]);
            button.connect('clicked', () => { this.syncing = true; this.instance.selected = 0; this.syncing = false; this.instance.selected = index + 1; this.settings.back(); });
            this.settings.selection.add(button);
        }
        this.details = this.settings.launch;
        this.endpoint = new this.Adw.EntryRow({title: 'Application address', text: this.initial.endpoint ?? ''}); this.settings.selection.add(this.endpoint);
        this.health = new this.Adw.EntryRow({title: 'Health endpoint', text: this.initial.binding?.healthURL ?? ''}); this.settings.general.add(this.health);
        this.health.connect('changed', () => {
            if (this.syncing) return;
            const input = this.draft.snapshot();
            this.bindingEdits.add('healthURL'); this.draft.edit({binding: {...input.binding, healthURL: this.health.text}});
            this.syncing = true; if (this.bindingFields.healthURL) this.bindingFields.healthURL.text = this.health.text; this.syncing = false;
            this.installationEvidence = null; this.settings.invalidate(); this.changed(this.draft.snapshot());
        });
        this.reference = new this.Gtk.Label({label: this.initial.reference ?? 'No file or folder selected', wrap: true, xalign: 0, selectable: true}); this.settings.selection.add(this.reference);
    }
    connectAddress() {
        this.endpoint.connect('changed', () => {
            if (this.syncing) return;
            this.resetSelection(); this.endpointEdited = true; this.draft.endpoint(this.endpoint.text); this.reference.label = 'No file or folder selected';
            this.clearBinding(); this.clearModels();
            this.clearOwnedReference();
            this.installationEvidence = null; this.settings.invalidate(); this.changed(this.draft.snapshot());
        });
        const addressPicker = new this.Gtk.Button({label: 'Choose application address…'}); this.settings.selection.add(addressPicker);
        addressPicker.connect('clicked', () => { this.endpoint.grab_focus(); });
    }
    buildModels() {
        this.modelGroup = this.modelParent ? new this.Adw.PreferencesGroup() : this.settings.selection;
        if (this.modelParent) {
            this.modelParent.append(this.modelGroup); this.modelGroup.add_css_class('setup-card');
            const modelSettings = new this.Gtk.Button({icon_name: 'emblem-system-symbolic', tooltip_text: 'Advanced application settings'});
            modelSettings.update_property([this.Gtk.AccessibleProperty.LABEL], [`Settings for ${applications.find(app => app.id === this.initial.app).label}`]);
            modelSettings.connect('clicked', () => this.openSettings?.(modelSettings));
            const subtitle = this.draft.needsModel ? null : new this.Gtk.Label({label: 'Models are selected in your workflows.', wrap: true, xalign: 0});
            subtitle?.add_css_class('dim-label'); subtitle?.add_css_class('setup-guidance');
            this.modelGroup.add(applicationHeader(this.Gtk, this.initial.app, applications.find(app => app.id === this.initial.app).label, modelSettings, subtitle));
        }
        this.modelStatus = new this.Gtk.Label({wrap: true, xalign: 0, selectable: true});
        if (this.draft.needsModel) this.modelGroup.add(this.modelStatus);
        this.model = null;
        if (this.draft.needsModel) {
            this.model = new this.Adw.ComboRow({title: 'Model', use_markup: false, model: this.Gtk.StringList.new([this.initial.model || 'Check an instance to list models']), selected: 0});
            this.modelGroup.add(this.model);
        }
        this.models = [];
        this.modelChecks = new Map();
        this.selectionBox = new this.Gtk.Box({orientation: this.Gtk.Orientation.VERTICAL, spacing: 8});
        if (this.modelParent && this.draft.needsModel) this.modelGroup.add(this.selectionBox);
        this.modelName = null;
        if (this.initial.app === 'ollama') {
            this.modelName = new this.Adw.EntryRow({title: 'Existing model name', text: this.initial.model ?? '', visible: Boolean(this.initial.binding?.owned?.executable)});
            this.modelGroup.add(this.modelName);
            this.modelName.connect('changed', () => {
                if (this.syncing) return;
                const binding = this.draft.snapshot().binding;
                this.draft.edit({model: this.modelName.text, models: undefined, binding: binding && !binding.owned ? {...binding, model: this.modelName.text} : binding});
                this.syncModelChoices(this.modelName.text); this.changed(this.draft.snapshot());
            });
        }
    }
    syncModelChoices(id) {
        this.syncing = true;
        for (const [modelID, check] of this.modelChecks) check.active = modelID === id;
        if (this.modelName) this.modelName.text = id ?? '';
        this.syncing = false;
    }
    clearModels() {
        this.models = [];
        for (const check of this.modelChecks.values()) this.selectionBox.remove(check);
        this.modelChecks.clear();
        this.syncing = true;
        if (this.model) { this.model.model = this.Gtk.StringList.new(['Check an instance to list models']); this.model.selected = 0; }
        this.syncing = false;
        this.draft.edit({model: undefined, models: undefined});
        if (this.ownedFields.model) this.ownedFields.model.text = '';
    }
    clearOwnedReference() {
        if (this.ownedFields.model) this.ownedFields.model.text = '';
        if (this.ownedFields.modelPath) this.ownedFields.modelPath.text = '';
    }
    selectOwnedReference(path, kind) {
        if (!this.ownedFields.modelPath) return;
        const expected = this.initial.app === 'llama.cpp' ? 'model-file' : 'model-directory';
        this.ownedFields.modelPath.text = kind === expected ? path : '';
    }
    clearBinding() {
        this.syncing = true;
        for (const field of Object.values(this.bindingFields)) field.text = '';
        this.syncing = false;
        const binding = this.draft.snapshot().binding;
        const owned = binding?.owned ? {...binding.owned} : null;
        if (owned) delete owned.modelPath;
        this.draft.edit({binding: owned ? {instance: binding.instance, owned} : undefined});
    }
    connectModelPicker() {
        this.model?.connect('notify::selected', () => {
            if (this.syncing) return;
            const selected = this.models[this.model.selected - 1];
            if (!selected) return;
            const input = this.draft.snapshot();
            this.draft.edit({model: selected.id, models: undefined, binding: input.binding && !input.binding.owned ? {...input.binding, model: selected.id} : input.binding});
            this.syncing = true;
            if (this.bindingFields.model) this.bindingFields.model.text = selected.id;
            this.syncing = false;
            if (this.ownedFields.model) this.ownedFields.model.text = selected.id;
            if (this.ownedFields.modelPath) this.ownedFields.modelPath.text = selected.id.startsWith('/') ? selected.id : '';
            this.syncModelChoices(selected.id); this.changed(this.draft.snapshot());
        });
    }
    showModelChecks(candidate) {
        const selectedModels = this.draft.snapshot().models;
        if (selectedModels && (this.models.length || candidate.inventoryStatus === 'available')) {
            const available = selectedModels.filter(id => this.models.some(item => item.id === id));
            if (available.length !== selectedModels.length) this.draft.edit({models: available});
        }
        const savedModels = this.draft.snapshot().models ?? (this.draft.snapshot().model ? [this.draft.snapshot().model] : []);
        for (const check of this.modelChecks.values()) this.selectionBox.remove(check);
        this.modelChecks.clear();
        for (const item of this.models) {
            const check = new this.Gtk.CheckButton({label: item.label || item.id, active: savedModels.includes(item.id)});
            check.add_css_class('setup-model-choice');
            check.connect('toggled', () => {
                if (this.syncing) return;
                const selected = [...this.modelChecks].filter(([, widget]) => widget.active).map(([id]) => id);
                this.draft.edit({models: selected, model: selected[0] ?? ''}); this.changed(this.draft.snapshot());
            });
            this.modelChecks.set(item.id, check); this.selectionBox.append(check);
        }
        this.model.visible = false;
    }
    showModels(candidate) {
        const chosen = this.draft.snapshot().model;
        this.models = candidate.models ?? [];
        // Existing llama.cpp/vLLM services pin their launch model. Alternate
        // files require a separately validated launch, never a unit rewrite.
        if (candidate.recognized && ['llama.cpp', 'vllm'].includes(this.initial.app) && candidate.binding?.model)
            this.models = this.models.filter(item => item.id === candidate.binding.model);
        const selection = this.models.findIndex(item => item.id === chosen) + 1;
        const prompt = this.modelPrompt(candidate, chosen, selection);
        this.syncing = true;
        this.model.model = this.Gtk.StringList.new([prompt, ...this.models.map(item => item.label || item.id)]);
        this.model.selected = selection || (candidate.recognized && this.models.length === 1 && !chosen ? 1 : 0);
        this.syncing = false;
        if (candidate.recognized && !chosen && !this.draft.snapshot().models && this.models.length === 1) this.draft.edit({model: this.models[0].id});
        this.model.visible = !(candidate.recognized && candidate.configurationStatus === 'ready' && this.draft.snapshot().model);
        if (this.modelParent) this.showModelChecks(candidate);
        if (this.modelName) {
            this.modelName.visible = candidate.recognized && candidate.configurationStatus === 'model-required' && !this.models.length;
            this.syncing = true; this.modelName.text = this.draft.snapshot().model ?? ''; this.syncing = false;
            if (this.modelName.visible) this.model.visible = false;
        }
    }
    modelPrompt(candidate, chosen, selection) {
        if (chosen && !selection) return `Saved model unavailable: ${chosen}`;
        if (this.models.length) return 'Choose a model...';
        if (candidate.inventoryStatus === 'available') return 'No models reported by this application';
        if (candidate.inventoryStatus === 'unsupported') return 'Models could not be listed. Choose a model location.';
        return 'Check this installation to list models';
    }
    show(candidate) {
        this.lastCandidate = candidate;
        this.installationEvidence = candidate.recognized ? candidate : null;
        this.reconcileService(candidate);
        this.settings.candidate(candidate);
        const mismatch = [...this.bindingEdits].some(key => key !== 'model' && candidate.binding && this.draft.snapshot().binding?.[key] !== candidate.binding[key]);
        if (mismatch) this.settings.problem('An override does not match this installation.', new Error('Keep the override or restore the current service settings before reviewing. Your service has not been changed.'));
        this.syncing = true; this.health.text = this.draft.snapshot().binding?.healthURL ?? ''; this.health.editable = !this.draft.snapshot().binding?.owned; this.syncing = false;
        this.temporary.showAvailability(candidate);
        if (this.model) this.showModels(candidate);
        this.changed(this.draft.snapshot());
    }
    reconcileService(candidate) {
        const input = this.draft.snapshot();
        if (!candidate.recognized || !candidate.binding || candidate.unit !== input.binding?.unit || input.binding?.owned) return;
        const binding = {...input.binding};
        for (const key of ['unit', 'cgroup', 'healthURL', 'instance', 'launchFile']) {
            if (!this.bindingEdits.has(key)) binding[key] = candidate.binding[key];
        }
        const endpoint = this.endpointEdited ? input.endpoint : candidate.endpoint;
        const reference = input.referenceKind === 'configuration' && input.reference === candidate.unit ? undefined : input.reference;
        this.draft.edit({binding, endpoint, reference, referenceKind: reference ? input.referenceKind : undefined});
        this.reference.label = reference ?? 'No file or folder selected';
        this.syncing = true;
        this.endpoint.text = endpoint ?? '';
        for (const [key, field] of Object.entries(this.bindingFields)) field.text = binding[key] ?? '';
        this.syncing = false;
    }
    connectInputs() {
        watchInstanceSelection({beforeSelection: () => { this.bindingEdits.clear(); this.endpointEdited = false; }, isSyncing: () => this.syncing, instance: this.instance, instances: this.instances, draft: this.draft, endpoint: this.endpoint, reference: this.reference, clearBinding: () => this.clearBinding(), clearModels: () => this.clearModels(), show: (candidate) => this.show(candidate),
            getBindingFields: () => this.bindingFields, setSync: value => this.syncing = value, selectOwnedBinding: binding => {
                this.syncing = true;
                for (const [key, field] of Object.entries(this.ownedFields)) {
                    if (typeof field === 'function') continue;
                    let value = '';
                    if (key === 'instance') value = binding.instance ?? '';
                    else if (key !== 'model') value = binding.owned[key] ?? '';
                    field.text = String(value);
                }
                this.syncing = false; this.ownedFields.selectLaunch();
            },
            setProbeGuidance: guidance => this.probeGuidance = guidance, clearOwnedReference: () => this.clearOwnedReference(), selectOwnedReference: (path, kind) => this.selectOwnedReference(path, kind)});
        this.refresh = addRefreshButton({Gtk: this.Gtk, group: {add: child => this.settings.pages.get('configuration').append(child)}, draft: this.draft, status: this.status, command: this.command, show: (candidate) => this.show(candidate), reportError: this.reportError, guidance: () => this.probeGuidance, checking: busy => this.settings.checking(busy), inspectService: async input => {
            const discovery = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'discover']));
            return discovery.applications?.find(candidate => candidate.app === input.app && candidate.unit === input.binding.unit) ?? null;
        }});
        addFilePickers({Gtk: this.Gtk, window: this.window, group: this.settings.selection, advancedGroup: this.settings.selection, draft: this.draft, reference: this.reference, endpoint: this.endpoint, status: this.status, changed: this.changed, clearBinding: () => this.clearBinding(), clearModels: () => this.clearModels(), clearOwnedReference: () => this.clearOwnedReference(), setSync: value => this.syncing = value, selectedReference: (path, kind) => this.selectOwnedReference(path, kind), selectedExecutable: path => this.selectExecutable(path), modelGroup: this.modelParent ? this.modelGroup : null, selectionChanged: () => this.resetSelection()});
        if (this.draft.needsModel) this.ownedFields = addOwnedEditor({Adw: this.Adw, Gtk: this.Gtk, group: this.details, draftGroup: this.group, draft: this.draft, initial: this.initial, command: this.command, bind: this.bind, changed: this.changed, taken: this.taken, status: this.status, isSyncing: () => this.syncing, edited: () => { this.installationEvidence = null; this.settings.invalidate(); }, reportError: this.reportError, parent: this.parent, removed: this.removed, modelChanged: (id) => this.syncModelChoices(id)});
        this.bindingFields = addBindingEditor({Adw: this.Adw, Gtk: this.Gtk, group: this.details, draftGroup: this.group, draft: this.draft, initial: this.initial, status: this.status, parent: this.parent, removed: this.removed, bind: this.bind, command: this.command, changed: this.changed, taken: this.taken, reportError: this.reportError, isSyncing: () => this.syncing, edited: key => {
                this.bindingEdits.add(key); this.installationEvidence = null; this.settings.invalidate();
                this.syncing = true; this.health.text = this.draft.snapshot().binding?.healthURL ?? ''; this.syncing = false;
                this.settings.identity.label = candidateIdentity({...this.draft.snapshot(), ...this.draft.snapshot().binding});
            }, modelChanged: (id) => this.syncModelChoices(id)});
    }
    buildResources() {
        this.capacity = new this.Adw.EntryRow({title: 'Measured VRAM requirement (MiB; optional)', text: ''});
        this.settings.resources.add(this.capacity);
        this.capacity.connect('changed', () => {
            if (this.syncing) return;
            this.overrides.requiredMiB = this.capacity.text.trim() === '' ? undefined : Number(this.capacity.text);
            this.draft.edit({requiredMiB: this.overrides.requiredMiB ?? 0}); this.changed(this.draft.snapshot());
        });
        this.retain = new this.Gtk.CheckButton({label: 'Keep this workload running at login if already active'});
        this.settings.resources.add(this.retain);
        this.retain.connect('toggled', () => {
            if (this.syncing) return;
            this.overrides.bootPolicy = this.retain.active ? 'retain' : 'stop-to-idle'; this.draft.edit({bootPolicy: this.overrides.bootPolicy}); this.changed(this.draft.snapshot());
        });
    }
    addActions() {
        const save = new this.Gtk.Button({label: 'Save selections for later'}); this.details.add(save); save.connect('clicked', () => this.saveSelections());
        this.finish = new this.Gtk.Button({label: 'Use installation', visible: false}); this.group.append(this.finish);
        // Advanced checks reuse the safeguarded preparation path; the wizard owns activation.
        this.finish.connect('clicked', async () => {
            const input = this.draft.snapshot();
            const appLabel = applications.find(app => app.id === input.app).label;
            const proposed = profileIDFromModel(input.app, input.model);
            if (input.id.startsWith('draft-') && proposed && !this.taken().includes(proposed)) input.id = proposed;
            if (input.model && input.label === appLabel) input.label = `${appLabel} - ${input.model}`;
            const generation = this.draft.generation;
            this.finish.sensitive = false; this.settings.checking(true);
            try {
                if (this.temporary.blocked()) throw new Error('Finish temporary application cleanup before continuing.');
                const action = input.binding?.owned ? 'render-owned' : 'prepare';
                const result = JSON.parse(await this.command(['/usr/bin/gpu-setup', action], JSON.stringify({draft: input})));
                if (generation !== this.draft.generation) return;
                await this.bind(result.profile, () => generation === this.draft.generation, true);
                if (generation !== this.draft.generation) return;
                this.draft.cancel(); this.parent.remove(this.group); this.removed(true);
            } catch (error) {
                if (generation === this.draft.generation) this.reportError('Setup could not finish. Check the installation or Advanced settings, then retry.', error);
            } finally { this.finish.sensitive = true; this.settings.checking(false); }
        });
        const remove = new this.Gtk.Button({label: 'Remove this application'}); this.details.add(remove);
        remove.connect('clicked', async () => { this.draft.cancel(); try { await this.temporary.cleanup(); this.parent.remove(this.group); if (this.modelParent) { this.modelParent.remove(this.modelGroup); } this.removed(); } catch (error) { this.reportError('Temporary cleanup must finish before removing this selection.', error); } });
        this.parent.append(this.group);
    }
    initialDiscovery() {
        const recognized = this.instances.filter(candidate => candidate.recognized);
        if (!this.initial.binding?.unit && !this.initial.endpoint && !this.initial.reference && recognized.length === 1) {
            this.instance.selected = this.instances.indexOf(recognized[0]) + 1;
            this.instance.visible = false;
        } else if (this.initial.binding?.unit) {
            const selected = this.instances.find(candidate => candidate.unit === this.initial.binding.unit);
            if (selected) { this.syncing = true; this.instance.selected = this.instances.indexOf(selected) + 1; this.syncing = false; this.show(selected); }
            else this.show({app: this.initial.app, label: this.initial.label, unit: this.initial.binding.unit, endpoint: this.initial.endpoint, recognized: false, configurationStatus: 'unverified', instanceStatus: 'candidate'});
        } else if (!recognized.length && this.instances.length) {
            const selected = this.instances.find(candidate => candidate.unit) ?? this.instances[0];
            this.instance.selected = this.instances.indexOf(selected) + 1;
        } else if (!this.instances.length) {
            this.settings.problem('Installation not detected', new Error(`${applications.find(app => app.id === this.initial.app).label} wasn’t detected. Install it first, or choose its location. Supported detection uses recognized user services; custom launch wrappers need Advanced settings.`));
        }
    }
    view() {
        return {
            app: this.initial.app, id: this.initial.id, originalModel: this.initial.model ?? this.initial.binding?.owned?.modelPath, group: this.group, modelGroup: this.modelGroup, finish: this.finish,
            showSettings: profile => {
                this.syncing = true;
                const required = Object.hasOwn(this.overrides, 'requiredMiB') ? this.overrides.requiredMiB : profile?.requiredMiB;
                this.capacity.text = String(required ?? ''); this.retain.active = (this.overrides.bootPolicy ?? profile?.bootPolicy) === 'retain'; this.syncing = false;
                this.health.editable = !this.draft.snapshot().binding?.owned;
                if (!this.health.editable && profile?.healthURL) { this.syncing = true; this.health.text = profile.healthURL; this.syncing = false; }
                this.session = this.capture(); this.group.visible = true; this.settings.history = []; this.settings.show(); this.settings.change.grab_focus();
            },
            settingsAction: () => {
                if (this.settings.page === 'configuration') return this.refresh.emit('clicked');
                if (['selection', 'resources', 'launch'].includes(this.settings.page)) return this.settings.back();
                return this.finish.emit('clicked');
            },
            settingsBack: () => this.settings.back(),
            cancelSettings: async () => { this.draft.cancel(); await this.temporary.cancel(); this.restore(this.session); },
            settingsOverrides: () => this.overrides,
            showProblem: this.reportError,
            cancel: () => { this.draft.cancel(); return this.temporary.cancel(); },
            temporaryActive: () => this.temporary.active(),
            needsModelDecision: () => this.draft.needsModel && (this.models.length > 1 || this.draft.snapshot().models?.length > 1 || this.draft.snapshot().models?.length === 0 || !(this.draft.snapshot().model || this.draft.snapshot().binding?.owned?.modelPath)),
            prepare: () => this.prepare(),
        };
    }
    resetSelection() { this.syncing = true; this.instance.selected = 0; this.syncing = false; this.installationEvidence = null; this.bindingEdits.clear(); this.settings.invalidate(); }
    selectExecutable(path) {
        this.installationEvidence = null;
        this.ownedFields.executable.text = path;
        this.ownedFields.selectLaunch();
        this.reference.label = path;
        this.status.label = 'Executable selected. Choose an existing model, then Continue to validate and preview a Supervisor-managed launch. Nothing starts during preview.';
        if (this.modelName) this.modelName.visible = true;
    }
    capture() {
        const fields = [this.name, this.endpoint, this.health, ...Object.values(this.bindingFields), ...Object.values(this.ownedFields)].filter(field => typeof field !== 'function');
        return {input: JSON.parse(JSON.stringify(this.draft.snapshot())), candidate: this.lastCandidate, evidence: this.installationEvidence, ready: this.settings.ready && !this.settings.busy, bindingEdits: [...this.bindingEdits], endpointEdited: this.endpointEdited, selection: this.instance.selected, fields: fields.map(field => [field, field.text]), capacity: this.capacity.text, retain: this.retain.active, overrides: {...this.overrides}};
    }
    restore(snapshot) {
        if (!snapshot) return;
        this.syncing = true;
        for (const [field, text] of snapshot.fields) field.text = text;
        this.capacity.text = snapshot.capacity; this.retain.active = snapshot.retain;
        this.syncing = false;
        this.syncing = true; this.instance.selected = snapshot.selection; this.syncing = false; this.draft.input = snapshot.input; this.draft.cancel(); this.installationEvidence = snapshot.evidence; this.bindingEdits = new Set(snapshot.bindingEdits); this.endpointEdited = snapshot.endpointEdited; this.overrides = snapshot.overrides;
        this.changed(this.draft.snapshot());
        const selected = snapshot.candidate;
        if (selected) this.show(selected);
        if (!selected || !snapshot.ready) this.settings.invalidate();
    }
    selectedModels(input) {
        if (!this.draft.needsModel) return [''];
        if (input.models !== undefined && input.models !== null) return input.models;
        const model = input.model || input.binding?.owned?.modelPath;
        return model ? [model] : [];
    }
    modelInput(input, selectedModel) {
        const value = {...input, model: selectedModel, binding: input.binding && !input.binding.owned ? {...input.binding, model: selectedModel} : input.binding};
        delete value.models;
        const changedModel = selectedModel !== (this.initial.model ?? this.initial.binding?.owned?.modelPath);
        const proposed = profileIDFromModel(input.app, selectedModel);
        if (proposed && (input.id.startsWith('draft-') || changedModel)) value.id = proposed;
        const appLabel = applications.find(app => app.id === input.app).label;
        if (selectedModel && (input.label === appLabel || changedModel)) value.label = `${appLabel} - ${this.models.find(item => item.id === selectedModel)?.label || selectedModel}`;
        return value;
    }
    async prepare() {
        if (this.temporary.blocked()) throw new Error('Finish temporary application cleanup before continuing.');
        const input = this.draft.snapshot();
        const generation = this.draft.generation;
        const selected = this.selectedModels(input);
        if (!selected.length) throw new Error('Choose at least one existing model.');
        const prepared = [];
        for (const selectedModel of selected) {
            const value = this.modelInput(input, selectedModel);
            const result = JSON.parse(await this.command(['/usr/bin/gpu-setup', input.binding?.owned ? 'render-owned' : 'prepare'], JSON.stringify({draft: value})));
            if (generation !== this.draft.generation) throw new Error('Application changed while checking it. Continue again.');
            prepared.push(result.profile);
        }
        return {profiles: prepared, current: () => generation === this.draft.generation};
    }
}

