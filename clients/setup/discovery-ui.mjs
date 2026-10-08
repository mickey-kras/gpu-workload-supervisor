import {TemporaryDiscovery} from './temporary-discovery-ui.mjs';
import {watchInstanceSelection, addRefreshButton, addFilePickers} from './discovery-inputs.mjs';
import {addOwnedEditor, addBindingEditor} from './launch-ui.mjs';
import {ApplicationDraft, applications, candidateMessage, profileIDFromModel} from './onboarding.mjs';

// Backend error detail stays available but collapsed behind the actionable summary.
export function addErrorReporter({Adw, Gtk, parent, status}) {
    const details = new Adw.ExpanderRow({title: 'Technical details', visible: false});
    const text = new Gtk.Label({wrap: true, xalign: 0, selectable: true});
    details.add_row(text);
    if (parent.add) parent.add(details); else parent.append(details);
    return (summary, error) => {
        status.label = summary;
        text.label = error.message;
        details.expanded = false;
        details.visible = true;
    };
}

export function addDraftEditor(options) { return new DraftEditor(options).view(); }

class DraftEditor {
    constructor({Adw, Gtk, window, parent, initial, detected, discoveryErrors, command, changed, removed, bind, taken, modelParent, temporaryStatus, openSettings}) {
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
        this.openSettings = openSettings;
        this.discoveryErrors ??= [];
        this.syncing = false; this.bindingFields = {}; this.ownedFields = {};
        this.installationEvidence = null;
        this.buildBasics(); this.connectAddress(); this.buildModels();
        this.connectModelPicker();
        this.temporary = new TemporaryDiscovery({Gtk, draft: this.draft, app: initial.app, modelGroup: this.modelGroup, status: this.status, temporaryStatus, command, changed, reportError: this.reportError, show: candidate => this.show(candidate), getEvidence: () => this.installationEvidence});
        this.connectInputs(); this.addActions(); this.initialDiscovery();
    }
    buildBasics() {
        this.draft = new ApplicationDraft(this.initial.app);
        this.draft.edit(this.initial);
        this.group = new this.Adw.PreferencesGroup({title: applications.find(app => app.id === this.initial.app).label,
            description: 'Choose your existing installation, then Finish. Applications and files are preserved.'});
        this.name = new this.Adw.EntryRow({title: 'Friendly name', text: this.initial.label});
        this.group.add(this.name);
        this.name.connect('changed', () => { this.draft.edit({label: this.name.text}); this.changed(this.draft.snapshot()); });
        this.status = new this.Gtk.Label({label: 'Choose a detected instance or check an address. Discovery does not start applications or load models.', wrap: true, xalign: 0, selectable: true});
        this.group.add(this.status);
        this.reportError = addErrorReporter({Adw: this.Adw, Gtk: this.Gtk, parent: this.group, status: this.status});
        this.probeGuidance = null;
        this.instances = this.detected.filter(candidate => candidate.app === this.initial.app);
        this.instance = new this.Adw.ComboRow({title: 'Detected instance', use_markup: false,
            model: this.Gtk.StringList.new(['Choose an instance...', ...this.instances.map(candidate => `${candidate.label}${candidate.location ? ' · ' + candidate.location : ''}`)]), selected: 0});
        this.group.add(this.instance);
        this.details = new this.Adw.ExpanderRow({title: 'Advanced', subtitle: 'Inspect or override technical configuration'});
        if (!this.modelParent) {
            const gear = new this.Gtk.Button({label: `Settings for ${applications.find(app => app.id === this.initial.app).label}`, icon_name: 'emblem-system-symbolic', tooltip_text: 'Advanced application settings'});
            gear.update_property([this.Gtk.AccessibleProperty.LABEL], [`Advanced settings for ${applications.find(app => app.id === this.initial.app).label}`]);
            gear.connect('clicked', () => { this.details.visible = !this.details.visible; this.details.expanded = this.details.visible; });
            this.group.add(gear);
        }
        this.details.visible = !this.modelParent; this.group.add(this.details);
        this.endpoint = new this.Adw.EntryRow({title: 'Application address', text: this.initial.endpoint ?? ''}); this.details.add_row(this.endpoint);
        this.reference = new this.Gtk.Label({label: this.initial.reference ?? 'No file or folder selected', wrap: true, xalign: 0, selectable: true}); this.details.add_row(this.reference);
    }
    connectAddress() {
        this.endpoint.connect('changed', () => {
            if (this.syncing) return;
            this.draft.endpoint(this.endpoint.text); this.reference.label = 'No file or folder selected';
            this.clearBinding(); this.clearModels();
            this.clearOwnedReference();
            this.changed(this.draft.snapshot());
        });
        const addressPicker = new this.Gtk.Button({label: 'Choose application address…'}); this.details.add_row(addressPicker);
        addressPicker.connect('clicked', () => { this.details.expanded = true; this.endpoint.grab_focus(); });
    }
    buildModels() {
        this.modelGroup = this.modelParent ? new this.Adw.PreferencesGroup({title: applications.find(app => app.id === this.initial.app).label}) : this.group;
        if (this.modelParent) {
            this.modelParent.append(this.modelGroup);
            const modelSettings = new this.Gtk.Button({label: `Application settings for ${applications.find(app => app.id === this.initial.app).label}`, icon_name: 'emblem-system-symbolic', tooltip_text: 'Advanced application settings'});
            modelSettings.update_property([this.Gtk.AccessibleProperty.LABEL], [`Settings for ${applications.find(app => app.id === this.initial.app).label}`]);
            modelSettings.connect('clicked', () => { this.openSettings?.(); this.group.visible = true; this.details.visible = true; this.details.expanded = true; });
            this.modelGroup.add(modelSettings);
        }
        this.model = null;
        if (this.draft.needsModel) {
            this.model = new this.Adw.ComboRow({title: 'Model', use_markup: false, model: this.Gtk.StringList.new([this.initial.model || 'Check an instance to list models']), selected: 0});
            this.modelGroup.add(this.model);
        } else {
            this.group.add(new this.Gtk.Label({label: 'ComfyUI workflows select models. No model selection is needed here.', wrap: true, xalign: 0}));
            if (this.modelParent) this.modelGroup.add(new this.Gtk.Label({label: 'Models are selected in your ComfyUI workflows.', wrap: true, xalign: 0}));
        }
        this.models = [];
        this.modelChecks = new Map();
        this.selectionBox = new this.Gtk.Box({orientation: this.Gtk.Orientation.VERTICAL, spacing: 8});
        if (this.modelParent && this.draft.needsModel) this.modelGroup.add(this.selectionBox);
        this.modelName = null;
        if (this.initial.app === 'ollama') {
            this.modelName = new this.Adw.EntryRow({title: 'Existing model name', text: this.initial.model ?? '', visible: false});
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
        const input = this.draft.snapshot();
        if (candidate.recognized) this.installationEvidence = candidate;
        else if (this.installationEvidence && this.installationEvidence.unit === input.binding?.unit && this.installationEvidence.endpoint === input.endpoint) {
            candidate = {...candidate, recognized: this.installationEvidence.recognized,
                configurationStatus: this.installationEvidence.configurationStatus, unit: this.installationEvidence.unit};
        }
        this.status.label = [candidateMessage(candidate), candidate.nextStep].filter(Boolean).join('\n');
        this.temporary.showAvailability(candidate);
        if (this.model) this.showModels(candidate);
        this.changed(this.draft.snapshot());
    }
    connectInputs() {
        watchInstanceSelection({instance: this.instance, instances: this.instances, draft: this.draft, endpoint: this.endpoint, reference: this.reference, clearBinding: () => this.clearBinding(), clearModels: () => this.clearModels(), show: (candidate) => this.show(candidate),
            getBindingFields: () => this.bindingFields, setSync: value => this.syncing = value,
            setProbeGuidance: guidance => this.probeGuidance = guidance, clearOwnedReference: () => this.clearOwnedReference(), selectOwnedReference: (path, kind) => this.selectOwnedReference(path, kind)});
        addRefreshButton({Gtk: this.Gtk, group: this.group, draft: this.draft, status: this.status, command: this.command, show: (candidate) => this.show(candidate), reportError: this.reportError, guidance: () => this.probeGuidance});
        addFilePickers({Gtk: this.Gtk, window: this.window, group: {add: child => this.details.add_row(child)}, draft: this.draft, reference: this.reference, endpoint: this.endpoint, status: this.status, changed: this.changed, clearBinding: () => this.clearBinding(), clearModels: () => this.clearModels(), clearOwnedReference: () => this.clearOwnedReference(), setSync: value => this.syncing = value, selectedReference: (path, kind) => this.selectOwnedReference(path, kind), modelGroup: this.modelParent ? this.modelGroup : null});
        if (this.draft.needsModel) this.ownedFields = addOwnedEditor({Adw: this.Adw, Gtk: this.Gtk, group: this.details, draftGroup: this.group, draft: this.draft, initial: this.initial, command: this.command, bind: this.bind, changed: this.changed, taken: this.taken, status: this.status, reportError: this.reportError, parent: this.parent, removed: this.removed, modelChanged: (id) => this.syncModelChoices(id)});
        this.bindingFields = addBindingEditor({Adw: this.Adw, Gtk: this.Gtk, group: this.details, draftGroup: this.group, draft: this.draft, initial: this.initial, status: this.status, parent: this.parent, removed: this.removed, bind: this.bind, command: this.command, changed: this.changed, taken: this.taken, reportError: this.reportError, isSyncing: () => this.syncing, modelChanged: (id) => this.syncModelChoices(id)});
    }
    addActions() {
        this.finish = new this.Gtk.Button({label: 'Check application'}); this.details.add_row(this.finish);
        // Advanced checks reuse the safeguarded preparation path; the wizard owns activation.
        this.finish.connect('clicked', async () => {
            const input = this.draft.snapshot();
            const appLabel = applications.find(app => app.id === input.app).label;
            const proposed = profileIDFromModel(input.app, input.model);
            if (input.id.startsWith('draft-') && proposed && !this.taken().includes(proposed)) input.id = proposed;
            if (input.model && input.label === appLabel) input.label = `${appLabel} - ${input.model}`;
            const generation = this.draft.generation;
            this.finish.sensitive = false;
            try {
                const action = input.binding?.owned ? 'render-owned' : 'prepare';
                const result = JSON.parse(await this.command(['/usr/bin/gpu-setup', action], JSON.stringify({draft: input})));
                if (generation !== this.draft.generation) return;
                await this.bind(result.profile, () => generation === this.draft.generation, true);
                if (generation !== this.draft.generation) return;
                this.draft.cancel(); this.parent.remove(this.group); this.removed(true);
            } catch (error) {
                if (generation === this.draft.generation) this.reportError('Setup could not finish. Check the installation or Advanced settings, then retry.', error);
            } finally { this.finish.sensitive = true; }
        });
        const remove = new this.Gtk.Button({label: 'Remove this application'}); this.group.add(remove);
        remove.connect('clicked', async () => { this.draft.cancel(); try { await this.temporary.cleanup(); this.parent.remove(this.group); if (this.modelParent && this.draft.needsModel) { this.modelParent.remove(this.modelGroup); } this.removed(); } catch (error) { this.reportError('Temporary cleanup must finish before removing this selection.', error); } });
        this.parent.append(this.group);
    }
    initialDiscovery() {
        const recognized = this.instances.filter(candidate => candidate.recognized);
        if (!this.initial.binding?.unit && !this.initial.endpoint && !this.initial.reference && recognized.length === 1) {
            this.instance.selected = this.instances.indexOf(recognized[0]) + 1;
            this.instance.visible = false;
        } else if (this.initial.binding?.unit) {
            const selected = this.instances.find(candidate => candidate.unit === this.initial.binding.unit);
            if (selected) this.show(selected);
        } else if (this.discoveryErrors.length && !recognized.length) {
            this.reportError('Installation discovery failed. Choose its location, or reopen setup to retry.', new Error(this.discoveryErrors.join('\n')));
        } else if (!recognized.length && this.instances.length) {
            this.show(this.instances.find(candidate => candidate.unit) ?? this.instances[0]);
        } else if (!this.instances.length) {
            this.status.label = `${applications.find(app => app.id === this.initial.app).label} wasn’t detected. Install it first, or choose its location. Supported detection uses recognized user services; custom launch wrappers need Advanced settings.`;
        }
    }
    view() {
        return {
            app: this.initial.app, id: this.initial.id, originalModel: this.initial.model ?? this.initial.binding?.owned?.modelPath, group: this.group, modelGroup: this.modelGroup, finish: this.finish,
            showSettings: () => { this.group.visible = !this.group.visible; this.details.visible = this.group.visible; this.details.expanded = this.group.visible; },
            cancel: () => { this.draft.cancel(); return this.temporary.cancel(); },
            temporaryActive: () => this.temporary.active(),
            needsModelDecision: () => this.draft.needsModel && (this.models.length > 1 || this.draft.snapshot().models?.length > 1 || this.draft.snapshot().models?.length === 0 || !(this.draft.snapshot().model || this.draft.snapshot().binding?.owned?.modelPath)),
            prepare: () => this.prepare(),
        };
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
