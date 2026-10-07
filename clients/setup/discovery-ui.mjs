import {ApplicationDraft, applications, candidateMessage} from './onboarding.mjs';

// Native widgets are passed by the setup window; this module never starts apps.
export function addDraftEditor({Adw, Gtk, window, parent, initial, detected, command, changed, removed, bind}) {
    const draft = new ApplicationDraft(initial.app);
    draft.edit(initial);
    const group = new Adw.PreferencesGroup({title: applications.find(app => app.id === initial.app).label,
        description: 'Draft - not selectable in GPU Control. Only supervisor configuration is saved.'});
    const name = new Adw.EntryRow({title: 'Friendly name', text: initial.label});
    group.add(name);
    name.connect('changed', () => { draft.edit({label: name.text}); changed(draft.snapshot()); });
    const status = new Gtk.Label({label: 'Choose a detected instance or check an address. Discovery does not start applications or load models.', wrap: true, xalign: 0, selectable: true});
    group.add(status);
    const instances = detected.filter(candidate => candidate.app === initial.app);
    const instance = new Adw.ComboRow({title: 'Detected instance', use_markup: false,
        model: Gtk.StringList.new(['Choose an instance...', ...instances.map(candidate => `${candidate.label} (${candidate.instanceStatus})`)]), selected: 0});
    group.add(instance);
    const details = new Adw.ExpanderRow({title: 'Advanced', subtitle: 'Application address or selected path'});
    group.add(details);
    const endpoint = new Adw.EntryRow({title: 'Application address', text: initial.endpoint ?? ''}); details.add_row(endpoint);
    const reference = new Gtk.Label({label: initial.reference ?? 'No file or folder selected', wrap: true, xalign: 0, selectable: true}); details.add_row(reference);
    let syncing = false;
    endpoint.connect('changed', () => {
        if (syncing) return;
        draft.endpoint(endpoint.text); reference.label = 'No file or folder selected';
        clearBinding(); clearModels();
        changed(draft.snapshot());
    });
    let model = null;
    if (draft.needsModel) {
        model = new Adw.ComboRow({title: 'Model', use_markup: false, model: Gtk.StringList.new([initial.model || 'Check an instance to list models']), selected: 0});
        group.add(model);
    } else group.add(new Gtk.Label({label: 'ComfyUI workflows select models. No model selection is needed here.', wrap: true, xalign: 0}));
    let models = [];
    let bindingFields = {};
    model?.connect('notify::selected', () => {
        if (syncing) return;
        const selected = models[model.selected - 1];
        if (!selected) return;
        draft.edit({model: selected.id});
        if (bindingFields.model) bindingFields.model.text = selected.id;
        changed(draft.snapshot());
    });
    function clearModels() {
        models = [];
        syncing = true;
        if (model) { model.model = Gtk.StringList.new(['Check an instance to list models']); model.selected = 0; }
        syncing = false;
        draft.edit({model: undefined});
    }
    function clearBinding() {
        for (const field of Object.values(bindingFields)) field.text = '';
    }
    function show(candidate) {
        status.label = [candidateMessage(candidate), candidate.nextStep].filter(Boolean).join('\n');
        if (model) {
            const chosen = draft.snapshot().model;
            models = candidate.models ?? [];
            const selection = models.findIndex(item => item.id === chosen) + 1;
            const prompt = chosen && !selection ? `Saved model unavailable: ${chosen}` :
                models.length ? 'Choose a model...' :
                    candidate.inventoryStatus === 'available' ? 'No models reported by this application' :
                        `Model inventory: ${candidate.inventoryStatus ?? 'not checked'}`;
            syncing = true;
            model.model = Gtk.StringList.new([prompt, ...models.map(item => item.label || item.id)]);
            model.selected = selection;
            syncing = false;
        }
        changed(draft.snapshot());
    }
    watchInstanceSelection({instance, instances, draft, endpoint, reference, clearBinding, show,
        getBindingFields: () => bindingFields, setSync: value => syncing = value});
    addRefreshButton({Gtk, group, draft, status, command, show});
    addFilePickers({Gtk, window, group, draft, reference, endpoint, status, changed, clearBinding, clearModels, setSync: value => syncing = value});
    bindingFields = addBindingEditor({Adw, Gtk, group, draft, initial, status, parent, removed, bind, command, changed});
    const remove = new Gtk.Button({label: 'Remove draft from supervisor'}); group.add(remove);
    remove.connect('clicked', () => { draft.cancel(); parent.remove(group); removed(); });
    parent.append(group);
    return {cancel: () => draft.cancel()};
}

function watchInstanceSelection({instance, instances, draft, endpoint, reference, clearBinding, show, getBindingFields, setSync}) {
    instance.connect('notify::selected', () => {
        const selected = instances[instance.selected - 1];
        if (!selected) return;
        clearBinding();
        draft.edit({endpoint: undefined, reference: undefined, referenceKind: undefined, model: ''});
        setSync(true); endpoint.text = ''; setSync(false);
        reference.label = 'No file or folder selected';
        if (selected.endpoint) { draft.endpoint(selected.endpoint); setSync(true); endpoint.text = selected.endpoint; setSync(false); }
        else if (selected.reference) { draft.reference(selected.reference, selected.referenceKind); reference.label = selected.reference; }
        else draft.cancel();
        for (const key of ['unit', 'cgroup']) if (selected[key]) getBindingFields()[key].text = selected[key];
        show(selected);
    });
}

function addRefreshButton({Gtk, group, draft, status, command, show}) {
    const refresh = new Gtk.Button({label: 'Refresh discovery'}); group.add(refresh);
    refresh.connect('clicked', async () => {
        const probe = draft.begin(); refresh.sensitive = false;
        status.label = 'Checking application without starting it...';
        try {
            const candidate = JSON.parse(await command(['/usr/bin/gpu-setup', 'probe'], JSON.stringify(probe.request)));
            if (draft.accept(probe, candidate)) show(candidate);
        } catch (error) {
            if (probe.generation === draft.generation) status.label = `Discovery failed. Check the address or location, then retry.\n${error.message}`;
        } finally { refresh.sensitive = true; }
    });
}

function addFilePickers({Gtk, window, group, draft, reference, endpoint, status, changed, clearBinding, clearModels, setSync}) {
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

function addBindingEditor({Adw, Gtk, group, draft, initial, status, parent, removed, bind, command, changed}) {
    const binding = new Adw.ExpanderRow({title: 'Advanced launch binding', subtitle: 'Use an existing isolated service. Saved binding fields remain unverified.'});
    group.add(binding);
    const fields = {};
    for (const [key, title] of [['unit', 'Existing user service'], ['cgroup', 'Cgroup path'], ['healthURL', 'Loopback health URL'], ...(draft.needsModel ? [['instance', 'Runtime instance ID'], ['model', 'Exact model ID'], ['launchFile', 'Loaded service file path'], ['launchSHA256', 'Service file SHA-256']] : [])]) {
        const row = new Adw.EntryRow({title, text: initial.binding?.[key] ?? (key === 'model' ? initial.model ?? '' : ''), editable: key !== 'launchSHA256'});
        binding.add_row(row); fields[key] = row;
        if (key !== 'launchSHA256') row.connect('changed', () => {
            const values = Object.fromEntries(Object.entries(fields).filter(([name]) => name !== 'launchSHA256').map(([name, field]) => [name, field.text]));
            draft.edit({binding: values}); changed(draft.snapshot());
        });
    }
    const promote = new Gtk.Button({label: 'Verify binding and add for review'}); binding.add_row(promote);
    promote.connect('clicked', async () => {
        const input = draft.snapshot();
        const profile = {id: input.id, label: input.label, adapter: 'systemd', bootPolicy: 'stop-to-idle',
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
            draft.cancel(); parent.remove(group); removed();
        } catch (error) { status.label = `Binding is not verified. Check Advanced launch binding, then retry.\n${error.message}`; }
        finally { promote.sensitive = true; }
    });
    return fields;
}
