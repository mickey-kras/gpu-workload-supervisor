export function createProfileEditor({current, Adw, Gtk, GLib, units, field, invalidate, applicationRuntime, onRemove, onEdit, onLabelEdit}) {
    const group = new Adw.PreferencesGroup({title: GLib.markup_escape_text(current.label || 'New workload', -1),
        description: current.nativeModel ? `${current.nativeModel.runtime} · ${current.nativeModel.model}. Applications and model files are preserved.` : 'Start and stop this installation from GPU Control. Applications and files are preserved.'});
    field(group, 'Display name', current.label, text => {
        current.label = text; group.title = GLib.markup_escape_text(text || 'New workload', -1);
        onLabelEdit?.(text);
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
    remove.connect('clicked', () => { onRemove(group); });
    const runtime = current.nativeModel?.runtime ?? current.launchBinding?.runtime ?? applicationRuntime ?? (current.adapter === 'comfyui' ? 'comfyui' : undefined);
    if (runtime) {
        const editLaunch = new Gtk.Button({label: 'Edit application'});
        editLaunch.connect('clicked', () => onEdit({id: current.id, label: current.label, app: runtime,
            model: current.nativeModel?.model, endpoint: current.nativeModel?.endpoint ?? current.launchBinding?.endpoint, binding: current.nativeModel?.owned ? {instance: current.nativeModel.instance, owned: {...current.nativeModel.owned}} : {unit: current.unit, cgroup: current.cgroup, healthURL: current.healthURL, instance: current.nativeModel?.instance, model: current.nativeModel?.model, launchFile: current.nativeModel?.launchFile ?? current.launchBinding?.launchFile}}));
        group.add(editLaunch);
    }
    group.add(remove); group.visible = false; return group;
}
