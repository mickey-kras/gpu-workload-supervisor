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

// Native widgets are passed by the setup window; this module never starts apps.
export function addDraftEditor({Adw, Gtk, window, parent, initial, detected, discoveryErrors = [], command, changed, removed, bind, taken, modelParent = null, temporaryStatus = null, openSettings = null}) {
    const draft = new ApplicationDraft(initial.app);
    draft.edit(initial);
    const group = new Adw.PreferencesGroup({title: applications.find(app => app.id === initial.app).label,
        description: 'Choose your existing installation, then Finish. Applications and files are preserved.'});
    const name = new Adw.EntryRow({title: 'Friendly name', text: initial.label});
    group.add(name);
    name.connect('changed', () => { draft.edit({label: name.text}); changed(draft.snapshot()); });
    const status = new Gtk.Label({label: 'Choose a detected instance or check an address. Discovery does not start applications or load models.', wrap: true, xalign: 0, selectable: true});
    group.add(status);
    const reportError = addErrorReporter({Adw, Gtk, parent: group, status});
    let probeGuidance = null;
    const instances = detected.filter(candidate => candidate.app === initial.app);
    const instance = new Adw.ComboRow({title: 'Detected instance', use_markup: false,
        model: Gtk.StringList.new(['Choose an instance...', ...instances.map(candidate => `${candidate.label}${candidate.location ? ` · ${candidate.location}` : ''}`)]), selected: 0});
    group.add(instance);
    const details = new Adw.ExpanderRow({title: 'Advanced', subtitle: 'Inspect or override technical configuration'});
    if (!modelParent) {
        const gear = new Gtk.Button({label: `Settings for ${applications.find(app => app.id === initial.app).label}`, icon_name: 'emblem-system-symbolic', tooltip_text: 'Advanced application settings'});
        gear.update_property([Gtk.AccessibleProperty.LABEL], [`Advanced settings for ${applications.find(app => app.id === initial.app).label}`]);
        gear.connect('clicked', () => { details.visible = !details.visible; details.expanded = details.visible; });
        group.add(gear);
    }
    details.visible = !modelParent; group.add(details);
    const endpoint = new Adw.EntryRow({title: 'Application address', text: initial.endpoint ?? ''}); details.add_row(endpoint);
    const reference = new Gtk.Label({label: initial.reference ?? 'No file or folder selected', wrap: true, xalign: 0, selectable: true}); details.add_row(reference);
    let syncing = false;
    endpoint.connect('changed', () => {
        if (syncing) return;
        draft.endpoint(endpoint.text); reference.label = 'No file or folder selected';
        clearBinding(); clearModels();
        clearOwnedReference();
        changed(draft.snapshot());
    });
    const modelGroup = modelParent ? new Adw.PreferencesGroup({title: applications.find(app => app.id === initial.app).label}) : group;
    if (modelParent) {
        modelParent.append(modelGroup);
        const modelSettings = new Gtk.Button({label: `Application settings for ${applications.find(app => app.id === initial.app).label}`, icon_name: 'emblem-system-symbolic', tooltip_text: 'Advanced application settings'});
        modelSettings.update_property([Gtk.AccessibleProperty.LABEL], [`Settings for ${applications.find(app => app.id === initial.app).label}`]);
        modelSettings.connect('clicked', () => { openSettings?.(); group.visible = true; details.visible = true; details.expanded = true; });
        modelGroup.add(modelSettings);
    }
    let model = null;
    if (draft.needsModel) {
        model = new Adw.ComboRow({title: 'Model', use_markup: false, model: Gtk.StringList.new([initial.model || 'Check an instance to list models']), selected: 0});
        modelGroup.add(model);
    } else {
        group.add(new Gtk.Label({label: 'ComfyUI workflows select models. No model selection is needed here.', wrap: true, xalign: 0}));
        if (modelParent) modelGroup.add(new Gtk.Label({label: 'Models are selected in your ComfyUI workflows.', wrap: true, xalign: 0}));
    }
    let models = [];
    const modelChecks = new Map();
    const selectionBox = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 8});
    if (modelParent && draft.needsModel) modelGroup.add(selectionBox);
    let modelName = null;
    if (initial.app === 'ollama') {
        modelName = new Adw.EntryRow({title: 'Existing model name', text: initial.model ?? '', visible: false});
        modelGroup.add(modelName);
        modelName.connect('changed', () => {
            if (syncing) return;
            const binding = draft.snapshot().binding;
            draft.edit({model: modelName.text, models: undefined, binding: binding && !binding.owned ? {...binding, model: modelName.text} : binding});
            syncModelChoices(modelName.text); changed(draft.snapshot());
        });
    }
    function syncModelChoices(id) {
        syncing = true;
        for (const [modelID, check] of modelChecks) check.active = modelID === id;
        if (modelName) modelName.text = id ?? '';
        syncing = false;
    }
    const addressPicker = new Gtk.Button({label: 'Choose application address…'}); details.add_row(addressPicker);
    addressPicker.connect('clicked', () => { details.expanded = true; endpoint.grab_focus(); });
    let bindingFields = {};
    let ownedFields = {};
    model?.connect('notify::selected', () => {
        if (syncing) return;
        const selected = models[model.selected - 1];
        if (!selected) return;
        const input = draft.snapshot();
        draft.edit({model: selected.id, models: undefined, binding: input.binding && !input.binding.owned ? {...input.binding, model: selected.id} : input.binding});
        syncing = true;
        if (bindingFields.model) bindingFields.model.text = selected.id;
        syncing = false;
        if (ownedFields.model) ownedFields.model.text = selected.id;
        if (ownedFields.modelPath) ownedFields.modelPath.text = selected.id.startsWith('/') ? selected.id : '';
        syncModelChoices(selected.id); changed(draft.snapshot());
    });
    function clearModels() {
        models = [];
        for (const check of modelChecks.values()) selectionBox.remove(check);
        modelChecks.clear();
        syncing = true;
        if (model) { model.model = Gtk.StringList.new(['Check an instance to list models']); model.selected = 0; }
        syncing = false;
        draft.edit({model: undefined, models: undefined});
        if (ownedFields.model) ownedFields.model.text = '';
    }
    function clearOwnedReference() {
        if (ownedFields.model) ownedFields.model.text = '';
        if (ownedFields.modelPath) ownedFields.modelPath.text = '';
    }
    function selectOwnedReference(path, kind) {
        if (!ownedFields.modelPath) return;
        const expected = initial.app === 'llama.cpp' ? 'model-file' : 'model-directory';
        ownedFields.modelPath.text = kind === expected ? path : '';
    }
    function clearBinding() {
        syncing = true;
        for (const field of Object.values(bindingFields)) field.text = '';
        syncing = false;
        const binding = draft.snapshot().binding;
        const owned = binding?.owned ? {...binding.owned} : null;
        if (owned) delete owned.modelPath;
        draft.edit({binding: owned ? {instance: binding.instance, owned} : undefined});
    }
    let installationEvidence = null;
    const temporaryOperation = {};
    let temporaryPromise = null; let temporarySession = null;
    const temporaryConsent = new Gtk.CheckButton({label: 'I allow this brief start and will keep other application controls paused.', visible: false});
    const temporaryStart = new Gtk.Button({label: 'Start Ollama briefly to list models', visible: false, sensitive: false});
    const temporaryCancel = new Gtk.Button({label: 'Cancel model detection', visible: false});
    const temporaryExplanation = new Gtk.Label({label: 'Listing models requires a brief start only if you cannot provide an existing model name. This may use GPU memory. Do not start applications or use their external controls during this check. Setup restores the previous stopped state and reports any cleanup failure.', wrap: true, xalign: 0, visible: false});
    if (initial.app === 'ollama') {
        modelGroup.add(temporaryExplanation); modelGroup.add(temporaryConsent); modelGroup.add(temporaryStart); modelGroup.add(temporaryCancel);
    }
    temporaryConsent.connect('toggled', () => { temporaryStart.sensitive = temporaryConsent.active && !temporaryPromise; });
    async function refreshTemporaryStatus() {
        Object.assign(temporaryStatus, {session: temporarySession, available: false, expected: undefined});
        try {
            const refreshed = JSON.parse(await command(['/usr/bin/gpu-setup', 'temporary-status']));
            Object.assign(temporaryStatus, refreshed, {session: refreshed.session ?? temporarySession});
        } catch (error) { reportError('The stopped state was restored, but model-check status could not be refreshed. Reopen setup before another temporary check.', error); }
    }
    async function cleanupTemporary() {
        if (temporaryPromise) { temporaryOperation.cancel?.(); await temporaryPromise; }
        if (!temporarySession || temporarySession.status === 'completed') return;
        if (!temporarySession.id || !temporarySession.token) throw new Error('The current temporary check could not be identified. Reopen setup to read its durable recovery record.');
        try {
            const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'temporary-cleanup'], JSON.stringify({id: temporarySession.id, token: temporarySession.token, externalControlPaused: true})));
            temporarySession = result.session;
            if (result.error || temporarySession?.status !== 'completed') throw new Error(result.error || 'Temporary application cleanup needs attention.');
            await refreshTemporaryStatus();
        } catch (error) { reportError('Ollama cleanup needs attention. Keep external controls paused and retry cleanup before leaving setup.', error); throw error; }
    }
    temporaryCancel.connect('clicked', async () => { draft.cancel(); try { await cleanupTemporary(); status.label = 'Model detection cancelled. Previous stopped state restored.'; } catch (error) { reportError('Cancellation needs cleanup. Keep external controls paused and retry.', error); } });
    temporaryStart.connect('clicked', async () => {
        if (temporarySession && temporarySession.status !== 'completed' && !temporaryPromise) { try { await cleanupTemporary(); temporaryStart.label = 'Start Ollama briefly to list models'; temporaryStart.sensitive = false; } catch (error) { reportError('Temporary cleanup needs attention. Reopen setup if its current record cannot be read.', error); } return; }
        if (!temporaryConsent.active || !temporaryStatus?.available || temporaryPromise) return;
        temporarySession = null;
        const input = draft.snapshot(); const generation = draft.generation;
        temporaryStart.sensitive = false; temporaryCancel.visible = true; changed(input);
        temporaryPromise = (async () => {
            try {
                const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'temporary-discover'], JSON.stringify({unit: input.binding?.unit, expected: temporaryStatus.expected, consent: true, externalControlPaused: true}), temporaryOperation));
                temporarySession = result.session;
                if (result.error || temporarySession?.status !== 'completed') throw new Error(result.error || 'The temporary application check did not finish cleanup.');
                await refreshTemporaryStatus();
                if (generation === draft.generation) show({...installationEvidence, models: result.models, inventoryStatus: 'available'});
            } catch (error) {
                try {
                    const recovery = JSON.parse(await command(['/usr/bin/gpu-setup', 'temporary-status']));
                    Object.assign(temporaryStatus, recovery); temporarySession = recovery.session ?? temporarySession;
                } catch (statusError) {
                    // Uncertain helper state must remain blocked until status can be checked.
                    temporarySession = {status: 'cleanup_required'};
                    Object.assign(temporaryStatus, {session: temporarySession, available: false, expected: undefined});
                    reportError('Temporary check status could not be read. Reopen setup to recover its recorded stopped state.', statusError);
                }
                reportError(temporarySession && !temporarySession.id ? 'Model check status is unknown. Setup remains blocked. Reopen setup to recover its recorded stopped state; leaving setup preserves this block.' : 'Model detection needs attention. Check the previous stopped state before continuing. Retry cleanup if required, or provide an existing model name.', error);
            }
        })();
        await temporaryPromise; temporaryPromise = null; temporaryCancel.visible = false;
        temporaryStart.label = temporarySession && temporarySession.status !== 'completed' ? 'Retry Ollama cleanup' : 'Start Ollama briefly to list models';
        temporaryConsent.active = false;
        if (temporarySession && temporarySession.status !== 'completed') {
            temporaryStart.sensitive = Boolean(temporarySession.id && temporarySession.token);
            // A retry reuses this same signal handler and the durable session token.
        }
    });
    function show(candidate) {
        const input = draft.snapshot();
        if (candidate.recognized) installationEvidence = candidate;
        else if (installationEvidence && installationEvidence.unit === input.binding?.unit && installationEvidence.endpoint === input.endpoint) {
            candidate = {...candidate, recognized: installationEvidence.recognized,
                configurationStatus: installationEvidence.configurationStatus, unit: installationEvidence.unit};
        }
        status.label = [candidateMessage(candidate), candidate.nextStep].filter(Boolean).join('\n');
        const canStart = initial.app === 'ollama' && candidate.recognized && candidate.instanceStatus === 'not-running' && candidate.configurationStatus === 'model-required' && !(candidate.models?.length) && temporaryStatus?.available === true;
        temporaryExplanation.visible = canStart; temporaryConsent.visible = canStart; temporaryStart.visible = canStart;
        if (model) {
            const chosen = draft.snapshot().model;
            models = candidate.models ?? [];
            // Existing llama.cpp/vLLM services pin their launch model. Alternate
            // files require a separately validated launch, never a unit rewrite.
            if (candidate.recognized && ['llama.cpp', 'vllm'].includes(initial.app) && candidate.binding?.model)
                models = models.filter(item => item.id === candidate.binding.model);
            const selection = models.findIndex(item => item.id === chosen) + 1;
            let prompt = candidate.inventoryStatus === 'unsupported' ? 'Models could not be listed. Choose a model location.' : 'Check this installation to list models';
            if (chosen && !selection) prompt = `Saved model unavailable: ${chosen}`;
            else if (models.length) prompt = 'Choose a model...';
            else if (candidate.inventoryStatus === 'available') prompt = 'No models reported by this application';
            syncing = true;
            model.model = Gtk.StringList.new([prompt, ...models.map(item => item.label || item.id)]);
            model.selected = selection || (candidate.recognized && models.length === 1 && !chosen ? 1 : 0);
            syncing = false;
            if (candidate.recognized && !chosen && !draft.snapshot().models && models.length === 1) draft.edit({model: models[0].id});
            model.visible = !(candidate.recognized && candidate.configurationStatus === 'ready' && draft.snapshot().model);
            if (modelParent) {
                const selectedModels = draft.snapshot().models;
                if (selectedModels && (models.length || candidate.inventoryStatus === 'available')) {
                    const available = selectedModels.filter(id => models.some(item => item.id === id));
                    if (available.length !== selectedModels.length) draft.edit({models: available});
                }
                const savedModels = draft.snapshot().models ?? (draft.snapshot().model ? [draft.snapshot().model] : []);
                for (const check of modelChecks.values()) selectionBox.remove(check);
                modelChecks.clear();
                for (const item of models) {
                    const check = new Gtk.CheckButton({label: item.label || item.id, active: savedModels.includes(item.id)});
                    check.connect('toggled', () => {
                        if (syncing) return;
                        const selected = [...modelChecks].filter(([, widget]) => widget.active).map(([id]) => id);
                        draft.edit({models: selected, model: selected[0] ?? ''}); changed(draft.snapshot());
                    });
                    modelChecks.set(item.id, check); selectionBox.append(check);
                }
                model.visible = false;
            }
            if (modelName) {
                modelName.visible = candidate.recognized && candidate.configurationStatus === 'model-required' && !models.length;
                syncing = true; modelName.text = draft.snapshot().model ?? ''; syncing = false;
                if (modelName.visible) model.visible = false;
            }
        }
        changed(draft.snapshot());
    }
    watchInstanceSelection({instance, instances, draft, endpoint, reference, clearBinding, clearModels, show,
        getBindingFields: () => bindingFields, setSync: value => syncing = value,
        setProbeGuidance: guidance => probeGuidance = guidance, clearOwnedReference, selectOwnedReference});
    addRefreshButton({Gtk, group, draft, status, command, show, reportError, guidance: () => probeGuidance});
    addFilePickers({Gtk, window, group: {add: child => details.add_row(child)}, draft, reference, endpoint, status, changed, clearBinding, clearModels, clearOwnedReference, setSync: value => syncing = value, selectedReference: selectOwnedReference, modelGroup: modelParent ? modelGroup : null});
    if (draft.needsModel) ownedFields = addOwnedEditor({Adw, Gtk, group: details, draftGroup: group, draft, initial, command, bind, changed, taken, status, reportError, parent, removed, modelChanged: syncModelChoices});
    bindingFields = addBindingEditor({Adw, Gtk, group: details, draftGroup: group, draft, initial, status, parent, removed, bind, command, changed, taken, reportError, isSyncing: () => syncing, modelChanged: syncModelChoices});
    const finish = new Gtk.Button({label: 'Check application'}); details.add_row(finish);
    // Advanced checks reuse the safeguarded preparation path; the wizard owns activation.
    finish.connect('clicked', async () => {
        const input = draft.snapshot();
        const appLabel = applications.find(app => app.id === input.app).label;
        const proposed = profileIDFromModel(input.app, input.model);
        if (input.id.startsWith('draft-') && proposed && !taken().includes(proposed)) input.id = proposed;
        if (input.model && input.label === appLabel) input.label = `${appLabel} - ${input.model}`;
        const generation = draft.generation;
        finish.sensitive = false;
        try {
            const action = input.binding?.owned ? 'render-owned' : 'prepare';
            const result = JSON.parse(await command(['/usr/bin/gpu-setup', action], JSON.stringify({draft: input})));
            if (generation !== draft.generation) return;
            await bind(result.profile, () => generation === draft.generation, true);
            if (generation !== draft.generation) return;
            draft.cancel(); parent.remove(group); removed(true);
        } catch (error) {
            if (generation === draft.generation) reportError('Setup could not finish. Check the installation or Advanced settings, then retry.', error);
        } finally { finish.sensitive = true; }
    });
    const remove = new Gtk.Button({label: 'Remove draft from supervisor'}); group.add(remove);
    remove.connect('clicked', async () => { draft.cancel(); try { await cleanupTemporary(); parent.remove(group); if (modelParent && draft.needsModel) modelParent.remove(modelGroup); removed(); } catch (error) { reportError('Temporary cleanup must finish before removing this selection.', error); } });
    parent.append(group);
    const recognized = instances.filter(candidate => candidate.recognized);
    if (!initial.binding?.unit && !initial.endpoint && !initial.reference && recognized.length === 1) {
        instance.selected = instances.indexOf(recognized[0]) + 1;
        instance.visible = false;
    } else if (initial.binding?.unit) {
        const selected = instances.find(candidate => candidate.unit === initial.binding.unit);
        if (selected) show(selected);
    } else if (discoveryErrors.length && !recognized.length) {
        reportError('Installation discovery failed. Choose its location, or reopen setup to retry.', new Error(discoveryErrors.join('\n')));
    } else if (!recognized.length && instances.length) {
        show(instances.find(candidate => candidate.unit) ?? instances[0]);
    } else if (!instances.length) {
        status.label = `${applications.find(app => app.id === initial.app).label} wasn’t detected. Install it first, or choose its location. Supported detection uses recognized user services; custom launch wrappers need Advanced settings.`;
    }
    return {
        app: initial.app, id: initial.id, originalModel: initial.model ?? initial.binding?.owned?.modelPath, group, modelGroup, finish,
        showSettings: () => { group.visible = !group.visible; details.visible = group.visible; details.expanded = group.visible; },
        cancel: () => {
            draft.cancel();
            if (!temporaryPromise && temporarySession && !temporarySession.id) {
                reportError('Temporary check status is unknown. Leaving setup preserves its durable recovery block; reopen setup to check it.', new Error('No current session identity is available for safe cleanup.'));
                return Promise.resolve();
            }
            return cleanupTemporary();
        },
        temporaryActive: () => Boolean(temporaryPromise || (temporarySession?.id && temporarySession.status !== 'completed')),
        needsModelDecision: () => draft.needsModel && (models.length > 1 || draft.snapshot().models?.length > 1 || draft.snapshot().models?.length === 0 || !(draft.snapshot().model || draft.snapshot().binding?.owned?.modelPath)),
        prepare: async () => {
            if (temporaryPromise || (temporarySession && temporarySession.status !== 'completed')) throw new Error('Finish temporary application cleanup before continuing.');
            const input = draft.snapshot();
            const generation = draft.generation;
            const selected = draft.needsModel ? (input.models ?? (input.model || input.binding?.owned?.modelPath ? [input.model || input.binding.owned.modelPath] : [])) : [''];
            if (!selected.length) throw new Error('Choose at least one existing model.');
            const prepared = [];
            for (const selectedModel of selected) {
                const value = {...input, model: selectedModel, binding: input.binding && !input.binding.owned ? {...input.binding, model: selectedModel} : input.binding};
                delete value.models;
                const proposed = profileIDFromModel(input.app, selectedModel);
                if (proposed && (input.id.startsWith('draft-') || selectedModel !== (initial.model ?? initial.binding?.owned?.modelPath))) value.id = proposed;
                const appLabel = applications.find(app => app.id === input.app).label;
                if (selectedModel && (input.label === appLabel || selectedModel !== (initial.model ?? initial.binding?.owned?.modelPath))) value.label = `${appLabel} - ${models.find(item => item.id === selectedModel)?.label || selectedModel}`;
                const result = JSON.parse(await command(['/usr/bin/gpu-setup', input.binding?.owned ? 'render-owned' : 'prepare'], JSON.stringify({draft: value})));
                if (generation !== draft.generation) throw new Error('Application changed while checking it. Continue again.');
                prepared.push(result.profile);
            }
            return {profiles: prepared, current: () => generation === draft.generation};
        },
    };
}

function watchInstanceSelection({instance, instances, draft, endpoint, reference, clearBinding, clearModels, show, getBindingFields, setSync, setProbeGuidance, clearOwnedReference, selectOwnedReference}) {
    instance.connect('notify::selected', () => {
        const selected = instances[instance.selected - 1];
        if (!selected) return;
        clearBinding(); clearModels();
        draft.edit({endpoint: undefined, reference: undefined, referenceKind: undefined, model: '', models: undefined});
        clearOwnedReference();
        setSync(true); endpoint.text = ''; setSync(false);
        reference.label = 'No file or folder selected';
        if (selected.endpoint) { draft.endpoint(selected.endpoint); setSync(true); endpoint.text = selected.endpoint; setSync(false); }
        else if (selected.reference) { draft.reference(selected.reference, selected.referenceKind); reference.label = selected.reference; selectOwnedReference(selected.reference, selected.referenceKind); }
        else draft.cancel();
        setProbeGuidance(selected.endpoint || selected.reference ? null : selected.nextStep);
        setSync(true);
        for (const key of ['unit', 'cgroup']) if (selected[key]) getBindingFields()[key].text = selected[key];
        setSync(false);
        if (!draft.snapshot().binding?.owned) {
            const binding = selected.binding ?? {unit: selected.unit ?? '', cgroup: selected.cgroup ?? ''};
            draft.edit({binding: {...binding}, model: binding.model ?? ''});
            setSync(true);
            for (const [key, field] of Object.entries(getBindingFields())) field.text = binding[key] ?? '';
            setSync(false);
        }
        show(selected);
    });
}

function addRefreshButton({Gtk, group, draft, status, command, show, reportError, guidance}) {
    const refresh = new Gtk.Button({label: 'Refresh discovery'}); group.add(refresh);
    refresh.connect('clicked', async () => {
        const target = draft.snapshot();
        if (!target.endpoint && !target.reference) {
            status.label = guidance() ?? 'Choose a detected instance, enter the application address, or select a model file or folder before refreshing.';
            return;
        }
        const probe = draft.begin(); refresh.sensitive = false;
        status.label = 'Checking application without starting it...';
        try {
            const candidate = JSON.parse(await command(['/usr/bin/gpu-setup', 'probe'], JSON.stringify(probe.request)));
            if (draft.accept(probe, candidate)) show(candidate);
        } catch (error) {
            if (probe.generation === draft.generation) reportError('Discovery failed. Check the address or location, then retry.', error);
        } finally { refresh.sensitive = true; }
    });
}

function addFilePickers({Gtk, window, group, draft, reference, endpoint, status, changed, clearBinding, clearModels, clearOwnedReference, setSync, selectedReference, modelGroup}) {
    const location = new Gtk.Button({label: 'Choose application location…'}); group.add(location);
    location.connect('clicked', () => {
        const dialog = new Gtk.FileDialog({title: 'Choose existing application folder'});
        dialog.select_folder(window, null, (source, result) => {
            try {
                const path = source.select_folder_finish(result)?.get_path();
                if (!path) return;
                clearBinding(); clearModels();
                draft.reference(path, 'application-directory'); reference.label = path;
                clearOwnedReference();
                setSync(true); endpoint.text = ''; setSync(false);
                status.label = 'Location selected. Finish will check whether a supported service owns this installation.';
                changed(draft.snapshot());
            } catch (error) {
                if (!error.matches?.(Gtk.DialogError, Gtk.DialogError.DISMISSED)) status.label = `File selection failed: ${error.message}`;
            }
        });
    });
    if (!draft.needsModel) return;
    for (const [label, method, kind] of [['Choose model file...', 'open', 'model-file'], ['Choose model folder...', 'select_folder', 'model-directory']]) {
        const choose = new Gtk.Button({label}); (modelGroup ?? group).add(choose);
        choose.connect('clicked', () => {
            const dialog = new Gtk.FileDialog({title: label});
            dialog[method](window, null, (source, result) => {
                try {
                    const file = source[`${method}_finish`](result);
                    const path = file?.get_path();
                    if (!path) return;
                    clearBinding(); clearModels();
                    draft.reference(path, kind); reference.label = path;
                    selectedReference(path, kind);
                    setSync(true); endpoint.text = ''; setSync(false);
                    status.label = 'Location selected. Compatibility and safe lifecycle control are not verified.';
                    changed(draft.snapshot());
                } catch (error) {
                    if (!error.matches?.(Gtk.DialogError, Gtk.DialogError.DISMISSED)) status.label = `File selection failed: ${error.message}`;
                }
            });
        });
    }
}

function addOwnedEditor({Adw, Gtk, group, draftGroup, draft, initial, command, bind, changed, taken, status, reportError, parent, removed, modelChanged}) {
    const launch = new Adw.PreferencesGroup({title: 'Supervisor-managed launch',
        description: initial.app === 'ollama' ? 'Models with the same instance name and port share one Ollama service.' : 'Create a service for this model using the installed application. Applications and model files are preserved.'});
    group.add_row(launch);
    const fields = {};
    if (initial.app === 'ollama') {
        const model = new Adw.EntryRow({title: 'Model name', text: initial.model ?? ''});
        launch.add(model);
        fields.model = model;
        model.connect('changed', () => { draft.edit({model: model.text, models: undefined}); modelChanged(model.text); changed(draft.snapshot()); });
    }
    const expectedModelReference = initial.app === 'llama.cpp' ? 'model-file' : 'model-directory';
    const defaults = {instance: initial.binding?.instance ?? 'local', port: {ollama: 11434, 'llama.cpp': 8080, vllm: 8000}[initial.app],
        modelPath: initial.referenceKind === expectedModelReference ? initial.reference ?? '' : '', ...initial.binding?.owned};
    const entries = [['instance', 'Instance name'], ['port', 'Launch port'], ...(initial.app === 'ollama' ? [] : [['modelPath', initial.app === 'llama.cpp' ? 'Model file' : 'Model directory']])];
    const advanced = new Adw.PreferencesGroup({title: 'Launch options'});
    const options = {
        ollama: [],
        'llama.cpp': [['ctxSize', 'Context size'], ['gpuLayers', 'GPU layers'], ['alias', 'Served model name']],
        vllm: [['maxModelLen', 'Maximum model length'], ['alias', 'Served model name']],
    }[initial.app];
    function save() {
        const owned = {};
        for (const [key, field] of Object.entries(fields)) {
            if (key === 'instance' || key === 'model' || field.text === '') continue;
            owned[key] = ['port', 'ctxSize', 'gpuLayers', 'maxModelLen'].includes(key) ? Number(field.text) : field.text;
        }
        const previous = draft.snapshot();
        if (initial.app !== 'ollama' && !owned.modelPath && previous.referenceKind === expectedModelReference && previous.reference) owned.modelPath = previous.reference;
        const changedModelPath = previous.binding?.owned?.modelPath !== owned.modelPath;
        draft.edit({binding: {instance: fields.instance.text, owned}, ...(changedModelPath ? {model: undefined, models: undefined} : {})});
        if (changedModelPath) modelChanged(owned.modelPath);
        changed(draft.snapshot());
    }
    for (const [key, title] of [...entries, ...options]) {
        const row = new Adw.EntryRow({title, text: String(defaults[key] ?? '')});
        fields[key] = row;
        if (options.some(([option]) => option === key)) advanced.add(row); else launch.add(row);
        row.connect('changed', save);
    }
    if (options.length) launch.add(advanced);
    const preview = new Gtk.Button({label: 'Preview managed launch and add for review'}); launch.add(preview);
    preview.connect('clicked', async () => {
        preview.sensitive = false;
        try {
            save();
            const input = draft.snapshot();
            for (const [key, value] of Object.entries(input.binding.owned))
                if (['port', 'ctxSize', 'gpuLayers', 'maxModelLen'].includes(key) && (!Number.isSafeInteger(value) || value < 0))
                    throw new Error('Launch numbers must be nonnegative whole numbers.');
            const proposed = profileIDFromModel(input.app, input.model || input.binding.owned.modelPath);
            if (input.id.startsWith('draft-') && proposed && !taken().includes(proposed)) input.id = proposed;
            const appLabel = applications.find(app => app.id === input.app).label;
            if (input.label === appLabel && input.model) input.label = `${appLabel} - ${input.model}`;
            const generation = draft.generation;
            const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'render-owned'], JSON.stringify({draft: input})));
            if (generation !== draft.generation) { status.label = 'Draft changed during preview. Preview the updated launch.'; return; }
            await bind(result.profile, () => generation === draft.generation);
            if (generation !== draft.generation) { status.label = 'Draft changed during preview. Preview the updated launch.'; return; }
            draft.cancel(); parent.remove(draftGroup); removed();
        } catch (error) { reportError('Launch preview failed. Check the model, instance and launch options, then retry.', error); }
        finally { preview.sensitive = true; }
    });
    return fields;
}

function addBindingEditor({Adw, Gtk, group, draftGroup, draft, initial, status, parent, removed, bind, command, changed, taken, reportError, isSyncing, modelChanged}) {
    const binding = new Adw.PreferencesGroup({title: 'Existing service', description: 'Use an existing isolated service. Saved fields remain unverified.'});
    group.add_row(binding);
    const fields = {};
    for (const [key, title] of [['unit', 'Existing user service'], ['cgroup', 'Cgroup path'], ['healthURL', 'Loopback health URL'], ...(draft.needsModel ? [['instance', 'Runtime instance ID'], ['model', 'Exact model ID'], ['launchFile', 'Loaded service file path'], ['launchSHA256', 'Service file SHA-256']] : [])]) {
        const row = new Adw.EntryRow({title, text: initial.binding?.[key] ?? (key === 'model' ? initial.model ?? '' : ''), editable: key !== 'launchSHA256'});
        binding.add(row); fields[key] = row;
        if (key !== 'launchSHA256') row.connect('changed', () => {
            if (isSyncing()) return;
            const values = Object.fromEntries(Object.entries(fields).filter(([name]) => name !== 'launchSHA256').map(([name, field]) => [name, field.text]));
            const previous = draft.snapshot().binding;
            const changedSource = ['unit', 'instance', 'launchFile'].some(key => values[key] !== previous?.[key]);
            const changedModel = values.model !== previous?.model;
            draft.edit({binding: values, ...(changedSource ? {model: undefined, models: undefined} : changedModel ? {model: values.model, models: undefined} : {})});
            if (changedSource) { if (fields.model) fields.model.text = ''; modelChanged(undefined); }
            else if (changedModel) modelChanged(values.model);
            changed(draft.snapshot());
        });
    }
    const promote = new Gtk.Button({label: 'Verify binding and add for review'}); binding.add(promote);
    promote.connect('clicked', async () => {
        const input = draft.snapshot();
        const appLabel = applications.find(app => app.id === input.app).label;
        const label = input.model && input.label === appLabel ? `${appLabel} - ${input.model}` : input.label;
        const proposed = profileIDFromModel(input.app, input.model);
        const id = proposed && !taken().includes(proposed) ? proposed : input.id;
        const profile = {id, label, adapter: 'systemd', bootPolicy: 'stop-to-idle',
            unit: fields.unit.text, cgroup: fields.cgroup.text, healthURL: fields.healthURL.text};
        if (draft.needsModel) profile.nativeModel = {runtime: input.app, instance: fields.instance.text, model: fields.model.text,
            endpoint: input.endpoint ?? '', launchFile: fields.launchFile.text, launchSHA256: fields.launchSHA256.text};
        const generation = draft.generation;
        promote.sensitive = false;
        try {
            if (profile.nativeModel) {
                const result = JSON.parse(await command(['/usr/bin/gpu-setup', 'fingerprint'], JSON.stringify({binding: profile.nativeModel})));
                profile.nativeModel.launchSHA256 = result.sha256;
                if (generation !== draft.generation) { status.label = 'Draft changed during verification. Verify the updated binding.'; return; }
                fields.launchSHA256.text = result.sha256;
            }
            await bind(profile, () => generation === draft.generation);
            if (generation !== draft.generation) { status.label = 'Draft changed during verification. Verify the updated binding.'; return; }
            draft.cancel(); parent.remove(draftGroup); removed();
        } catch (error) { reportError('Binding is not verified. Check Advanced launch binding, then retry.', error); }
        finally { promote.sensitive = true; }
    });
    return fields;
}
