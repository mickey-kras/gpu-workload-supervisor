import {applications, profileIDFromModel} from './onboarding.mjs';

export function addOwnedEditor({Adw, Gtk, group, draftGroup, draft, initial, command, bind, changed, taken, status, reportError, parent, removed, modelChanged}) {
    const launch = new Adw.PreferencesGroup({title: 'Supervisor-managed launch',
        description: initial.app === 'ollama' ? 'Models with the same instance name and port share one Ollama service.' : 'Create a service for this model using the installed application. Applications and model files are preserved.'});
    const launchDisclosure = new Adw.ExpanderRow({title: 'Supervisor-managed launch', subtitle: 'Preview a new service for an installed executable'});
    group.add_row(launchDisclosure); launchDisclosure.add_row(launch);
    const fields = {};
    if (initial.app === 'ollama') {
        const model = new Adw.EntryRow({title: 'Model name', text: initial.model ?? ''});
        launch.add(model);
        fields.model = model;
        model.connect('changed', () => { draft.edit({model: model.text, models: undefined}); modelChanged(model.text); changed(draft.snapshot()); });
    }
    const expectedModelReference = initial.app === 'llama.cpp' ? 'model-file' : 'model-directory';
    const defaults = {executable: initial.referenceKind === 'application' ? initial.reference ?? '' : '', instance: initial.binding?.instance ?? 'local', port: {ollama: 11434, 'llama.cpp': 8080, vllm: 8000}[initial.app],
        modelPath: initial.referenceKind === expectedModelReference ? initial.reference ?? '' : '', ...initial.binding?.owned};
    const entries = [['executable', 'Installed executable'], ['instance', 'Instance name'], ['port', 'Launch port'], ...(initial.app === 'ollama' ? [] : [['modelPath', initial.app === 'llama.cpp' ? 'Model file' : 'Model directory']])];
    const advanced = new Adw.PreferencesGroup({title: 'Launch options'});
    const options = {
        ollama: [],
        'llama.cpp': [['ctxSize', 'Context size'], ['gpuLayers', 'GPU layers'], ['alias', 'Served model name']],
        vllm: [['maxModelLen', 'Maximum model length'], ['alias', 'Served model name']],
    }[initial.app];
    function save() {
        const owned = {};
        for (const [key, field] of Object.entries(fields).filter(([, field]) => typeof field !== 'function')) {
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
    fields.selectLaunch = save;
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

export function addBindingEditor({Adw, Gtk, group, draftGroup, draft, initial, status, parent, removed, bind, command, changed, taken, reportError, isSyncing, modelChanged}) {
    const binding = new Adw.PreferencesGroup({title: 'Existing service', description: 'Adopt an existing isolated service and its loaded configuration. This does not create a new launch. Saved fields remain unverified.'});
    const adoption = new Adw.ExpanderRow({title: 'Existing service', subtitle: 'Adopt a service you already configured'});
    group.add_row(adoption); adoption.add_row(binding);
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
            let modelChanges = {};
            if (changedSource) modelChanges = {model: undefined, models: undefined};
            else if (changedModel) modelChanges = {model: values.model, models: undefined};
            draft.edit({binding: values, ...modelChanges});
            if (changedSource) {
                if (fields.model) { fields.model.text = ''; }
                modelChanged(undefined);
            }
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
