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
export function addDraftEditor({Adw, Gtk, window, parent, initial, detected, discoveryErrors = [], command, changed, removed, bind, taken}) {
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
    group.add(details);
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
    let model = null;
    if (draft.needsModel) {
        model = new Adw.ComboRow({title: 'Model', use_markup: false, model: Gtk.StringList.new([initial.model || 'Check an instance to list models']), selected: 0});
        group.add(model);
    } else group.add(new Gtk.Label({label: 'ComfyUI workflows select models. No model selection is needed here.', wrap: true, xalign: 0}));
    let models = [];
    let modelName = null;
    if (initial.app === 'ollama') {
        modelName = new Adw.EntryRow({title: 'Existing model name', text: initial.model ?? '', visible: false});
        group.add(modelName);
        modelName.connect('changed', () => {
            if (syncing) return;
            const binding = draft.snapshot().binding;
            draft.edit({model: modelName.text, binding: binding && !binding.owned ? {...binding, model: modelName.text} : binding});
            changed(draft.snapshot());
        });
    }
    const addressPicker = new Gtk.Button({label: 'Choose application address…'}); group.add(addressPicker);
    addressPicker.connect('clicked', () => { details.expanded = true; endpoint.grab_focus(); });
    let bindingFields = {};
    let ownedFields = {};
    model?.connect('notify::selected', () => {
        if (syncing) return;
        const selected = models[model.selected - 1];
        if (!selected) return;
        const input = draft.snapshot();
        draft.edit({model: selected.id, binding: input.binding && !input.binding.owned ? {...input.binding, model: selected.id} : input.binding});
        syncing = true;
        if (bindingFields.model) bindingFields.model.text = selected.id;
        syncing = false;
        if (ownedFields.model) ownedFields.model.text = selected.id;
        if (ownedFields.modelPath) ownedFields.modelPath.text = selected.id.startsWith('/') ? selected.id : '';
        changed(draft.snapshot());
    });
    function clearModels() {
        models = [];
        syncing = true;
        if (model) { model.model = Gtk.StringList.new(['Check an instance to list models']); model.selected = 0; }
        syncing = false;
        draft.edit({model: undefined});
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
        draft.edit({binding: binding?.owned ? {instance: binding.instance, owned: {...binding.owned}} : undefined});
    }
    let installationEvidence = null;
    function show(candidate) {
        const input = draft.snapshot();
        if (candidate.recognized) installationEvidence = candidate;
        else if (installationEvidence && installationEvidence.unit === input.binding?.unit && installationEvidence.endpoint === input.endpoint) {
            candidate = {...candidate, recognized: installationEvidence.recognized,
                configurationStatus: installationEvidence.configurationStatus, unit: installationEvidence.unit};
        }
        status.label = [candidateMessage(candidate), candidate.nextStep].filter(Boolean).join('\n');
        if (model) {
            const chosen = draft.snapshot().model;
            models = candidate.models ?? [];
            const selection = models.findIndex(item => item.id === chosen) + 1;
            let prompt = candidate.inventoryStatus === 'unsupported' ? 'Models could not be listed. Choose a model location.' : 'Check this installation to list models';
            if (chosen && !selection) prompt = `Saved model unavailable: ${chosen}`;
            else if (models.length) prompt = 'Choose a model...';
            else if (candidate.inventoryStatus === 'available') prompt = 'No models reported by this application';
            syncing = true;
            model.model = Gtk.StringList.new([prompt, ...models.map(item => item.label || item.id)]);
            model.selected = selection || (candidate.recognized && models.length === 1 && !chosen ? 1 : 0);
            syncing = false;
            if (candidate.recognized && !chosen && models.length === 1) draft.edit({model: models[0].id});
            model.visible = !(candidate.recognized && candidate.configurationStatus === 'ready' && draft.snapshot().model);
            if (modelName) {
                modelName.visible = candidate.recognized && candidate.configurationStatus === 'model-required' && !models.length;
                syncing = true; modelName.text = draft.snapshot().model ?? ''; syncing = false;
                if (modelName.visible) model.visible = false;
            }
        }
        changed(draft.snapshot());
    }
    watchInstanceSelection({instance, instances, draft, endpoint, reference, clearBinding, show,
        getBindingFields: () => bindingFields, setSync: value => syncing = value,
        setProbeGuidance: guidance => probeGuidance = guidance, clearOwnedReference, selectOwnedReference});
    addRefreshButton({Gtk, group, draft, status, command, show, reportError, guidance: () => probeGuidance});
    addFilePickers({Gtk, window, group, draft, reference, endpoint, status, changed, clearBinding, clearModels, setSync: value => syncing = value, selectedReference: selectOwnedReference});
    if (draft.needsModel) ownedFields = addOwnedEditor({Adw, Gtk, group: details, draftGroup: group, draft, initial, command, bind, changed, taken, status, reportError, parent, removed});
    bindingFields = addBindingEditor({Adw, Gtk, group: details, draftGroup: group, draft, initial, status, parent, removed, bind, command, changed, taken, reportError, isSyncing: () => syncing});
    const finish = new Gtk.Button({label: 'Finish'}); group.add(finish);
    finish.add_css_class('suggested-action');
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
    remove.connect('clicked', () => { draft.cancel(); parent.remove(group); removed(); });
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
    return {cancel: () => draft.cancel()};
}

function watchInstanceSelection({instance, instances, draft, endpoint, reference, clearBinding, show, getBindingFields, setSync, setProbeGuidance, clearOwnedReference, selectOwnedReference}) {
    instance.connect('notify::selected', () => {
        const selected = instances[instance.selected - 1];
        if (!selected) return;
        clearBinding();
        draft.edit({endpoint: undefined, reference: undefined, referenceKind: undefined, model: ''});
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

function addFilePickers({Gtk, window, group, draft, reference, endpoint, status, changed, clearBinding, clearModels, setSync, selectedReference}) {
    const location = new Gtk.Button({label: 'Choose application location…'}); group.add(location);
    location.connect('clicked', () => {
        const dialog = new Gtk.FileDialog({title: 'Choose existing application folder'});
        dialog.select_folder(window, null, (source, result) => {
            try {
                const path = source.select_folder_finish(result)?.get_path();
                if (!path) return;
                clearBinding();
                draft.reference(path, 'application-directory'); reference.label = path;
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
        const choose = new Gtk.Button({label}); group.add(choose);
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

function addOwnedEditor({Adw, Gtk, group, draftGroup, draft, initial, command, bind, changed, taken, status, reportError, parent, removed}) {
    const launch = new Adw.PreferencesGroup({title: 'Supervisor-managed launch',
        description: initial.app === 'ollama' ? 'Models with the same instance name and port share one Ollama service.' : 'Create a service for this model using the installed application. Applications and model files are preserved.'});
    group.add_row(launch);
    const fields = {};
    if (initial.app === 'ollama') {
        const model = new Adw.EntryRow({title: 'Model name', text: initial.model ?? ''});
        launch.add(model);
        fields.model = model;
        model.connect('changed', () => { draft.edit({model: model.text}); changed(draft.snapshot()); });
    }
    const defaults = {instance: initial.binding?.instance ?? 'local', port: {ollama: 11434, 'llama.cpp': 8080, vllm: 8000}[initial.app],
        modelPath: initial.reference ?? '', ...initial.binding?.owned};
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
        if (initial.app !== 'ollama' && !owned.modelPath && draft.snapshot().reference) owned.modelPath = draft.snapshot().reference;
        draft.edit({binding: {instance: fields.instance.text, owned}});
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

function addBindingEditor({Adw, Gtk, group, draftGroup, draft, initial, status, parent, removed, bind, command, changed, taken, reportError, isSyncing}) {
    const binding = new Adw.PreferencesGroup({title: 'Existing service', description: 'Use an existing isolated service. Saved fields remain unverified.'});
    group.add_row(binding);
    const fields = {};
    for (const [key, title] of [['unit', 'Existing user service'], ['cgroup', 'Cgroup path'], ['healthURL', 'Loopback health URL'], ...(draft.needsModel ? [['instance', 'Runtime instance ID'], ['model', 'Exact model ID'], ['launchFile', 'Loaded service file path'], ['launchSHA256', 'Service file SHA-256']] : [])]) {
        const row = new Adw.EntryRow({title, text: initial.binding?.[key] ?? (key === 'model' ? initial.model ?? '' : ''), editable: key !== 'launchSHA256'});
        binding.add(row); fields[key] = row;
        if (key !== 'launchSHA256') row.connect('changed', () => {
            if (isSyncing()) return;
            const values = Object.fromEntries(Object.entries(fields).filter(([name]) => name !== 'launchSHA256').map(([name, field]) => [name, field.text]));
            draft.edit({binding: values}); changed(draft.snapshot());
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
