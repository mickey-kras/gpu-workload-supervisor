export function watchInstanceSelection({instance, instances, draft, endpoint, reference, clearBinding, clearModels, show, getBindingFields, setSync, setProbeGuidance, clearOwnedReference, selectOwnedReference}) {
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

export function addRefreshButton({Gtk, group, draft, status, command, show, reportError, guidance}) {
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

export function addFilePickers({Gtk, window, group, draft, reference, endpoint, status, changed, clearBinding, clearModels, clearOwnedReference, setSync, selectedReference, modelGroup}) {
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
    let chooserGroup = group;
    if (modelGroup) {
        const chooser = new Gtk.Button({label: 'Choose another model…', halign: Gtk.Align.START, margin_top: 12});
        chooser.add_css_class('flat'); chooser.add_css_class('setup-link');
        const choices = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 8, visible: false});
        chooser.update_state([Gtk.AccessibleState.EXPANDED], [Gtk.AccessibleTristate.FALSE]);
        chooser.connect('clicked', () => {
            choices.visible = !choices.visible;
            chooser.update_state([Gtk.AccessibleState.EXPANDED], [choices.visible ? Gtk.AccessibleTristate.TRUE : Gtk.AccessibleTristate.FALSE]);
        });
        modelGroup.add(chooser); modelGroup.add(choices);
        chooserGroup = {add: child => choices.append(child)};
    }
    for (const [label, method, kind] of [['Choose model file...', 'open', 'model-file'], ['Choose model folder...', 'select_folder', 'model-directory']]) {
        const choose = new Gtk.Button({label}); chooserGroup.add(choose);
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
