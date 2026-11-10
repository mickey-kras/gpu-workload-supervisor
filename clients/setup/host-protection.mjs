import {copySetupText} from './settings-pages.mjs';
import {PlainLimitError, formatCores, formatGiB, parseCores, parseCount, parseGiB} from './limits-ui.mjs';

// The installation-wide Host protection page of Setup settings: the host
// cap status and its root step, the escape route warnings, the host cap
// memory values and the reserve for the supervisor. The texts follow the
// desktop tables of RESOURCE-LIMITS.md.

export const reserveDefaults = Object.freeze({memoryMiB: 2048, tasks: 512, cpuPercent: 100, ioPercent: 10});

const statusTexts = {
    present: 'Host protection is active for this account.',
    absent: 'This account has no host protection. An administrator must run one step as root.',
    different: 'Host protection settings do not match. An administrator must update them.',
    unknown: 'Host protection cannot be checked. The problems below give the cause.',
    notNeeded: 'No workload has limits, so this account needs no host protection.',
};

const reserveFields = [
    {key: 'memoryMiB', title: 'Memory for the supervisor (GiB)', parse: parseGiB, format: formatGiB},
    {key: 'tasks', title: 'Tasks for the supervisor', parse: parseCount, format: String},
    {key: 'cpuPercent', title: 'CPU for the supervisor (cores)', parse: parseCores, format: formatCores},
    {key: 'ioPercent', title: 'I/O for the supervisor (percent of each cap)', parse: parseCount, format: String},
];

// escapeRouteText picks the row of the desktop table for one entry: by
// its route first, then by its unchecked flag.
export function escapeRouteText(entry) {
    if (entry.route === 'more') return `Not shown: ${entry.item}.`;
    if (entry.route === 'incomplete' || entry.unchecked) return `The scan ${entry.item.replace(/^the scan /, '')}. A route can be open in a place that the scan cannot read.`;
    return `Programs from this account can run outside host protection through ${entry.route}: ${entry.item}. Limits protect against runaway workloads, not against such programs.`;
}

// rootStepText gives the drop-ins to install and the commands, as one text
// for the administrator.
export function rootStepText(step) {
    const files = (step?.dropIns ?? []).filter(dropIn => dropIn.install && dropIn.content).map(dropIn => `# ${dropIn.path}\n${dropIn.content.trimEnd()}`);
    const commands = step?.commands ?? [];
    if (!files.length && !commands.length) return '';
    return [...files, commands.join('\n')].filter(Boolean).join('\n\n');
}

export class HostProtection {
    constructor({Adw, Gtk, window, changed}) {
        this.Gtk = Gtk; this.window = window; this.changed = changed;
        this.usage = null; this.listeners = []; this.syncing = false;
        this.storedReserve = {...reserveDefaults}; this.storedCap = [];
        this.page = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18, visible: false});
        this.statusTitle = new Gtk.Label({label: 'Host protection', wrap: true, xalign: 0});
        this.statusTitle.add_css_class('title-2');
        this.statusText = this.label(statusTexts.notNeeded);
        this.usageText = this.label('', false);
        this.problems = this.label('', false);
        this.step = this.label('', false); this.step.add_css_class('monospace');
        this.copy = new Gtk.Button({label: 'Copy root step', visible: false, halign: Gtk.Align.END});
        this.copy.connect('clicked', () => copySetupText(this.window, this.step.label));
        this.copyNote = this.label('Copying applies nothing. An administrator runs the step as root.', false);
        for (const widget of [this.statusTitle, this.statusText, this.usageText, this.problems, this.step, this.copy, this.copyNote]) this.page.append(widget);
        this.capGroup = new Adw.PreferencesGroup({title: 'Host protection values', description: 'The root step caps all programs of this account with these values. Both are needed when a workload has limits.'});
        this.capRows = ['Host memory high (GiB)', 'Host memory maximum (GiB)'].map(title => this.entry(Adw, this.capGroup, title, ''));
        this.reserveGroup = new Adw.PreferencesGroup({title: 'Room for the supervisor', description: 'Workload limits must leave this room under the host protection values.'});
        this.reserveRows = reserveFields.map(field => this.entry(Adw, this.reserveGroup, field.title, field.format(reserveDefaults[field.key])));
        this.routesGroup = new Adw.PreferencesGroup({title: 'Escape route warnings', description: 'These warnings do not block limits.'});
        this.routes = this.label('Escape routes were not checked.');
        this.routesGroup.add(this.routes);
        this.error = this.label('', false); this.error.add_css_class('setup-notice');
        for (const widget of [this.capGroup, this.reserveGroup, this.routesGroup, this.error]) this.page.append(widget);
    }
    label(text, visible = true) {
        const label = new this.Gtk.Label({label: text, wrap: true, xalign: 0, selectable: true, visible});
        return label;
    }
    entry(Adw, group, title, text) {
        const row = new Adw.EntryRow({title, text});
        row.edited = false;
        row.connect('changed', () => {
            if (this.syncing) return;
            row.edited = true; this.changed?.();
        });
        group.add(row); return row;
    }
    // setDiscovery shows the step and the escape routes of a discovery. The
    // first discovery also gives the stored reserve of the profile.
    setDiscovery(discovery, initial = false) {
        if (initial) this.setReserve(discovery.request?.profile?.limitReserve);
        if (initial || discovery.hostCap) this.setStep(discovery.hostCap);
        if (initial || Object.hasOwn(discovery, 'escapeRoutes')) this.setRoutes(discovery.escapeRoutes);
    }
    onUsage(listener) { this.listeners.push(listener); listener(this.usage); }
    // setStep shows a host cap step of discovery or preview.
    setStep(step) {
        this.statusText.label = statusTexts[step?.status] ?? statusTexts.notNeeded;
        this.problems.label = (step?.problems ?? []).join('\n');
        this.problems.visible = Boolean(this.problems.label);
        this.step.label = rootStepText(step);
        const action = ['absent', 'different'].includes(step?.status) && Boolean(this.step.label);
        this.step.visible = action; this.copy.visible = action; this.copyNote.visible = action;
        this.usage = step?.usage ?? null;
        this.usageText.visible = Boolean(this.usage);
        if (this.usage) this.usageText.label = `Memory ceiling: ${formatGiB(this.usage.ceilingMiB)} GiB. In use now: ${formatGiB(this.usage.memoryInUseMiB)} GiB, without ${formatGiB(this.usage.pageCacheMiB)} GiB of page cache that the kernel can free.`;
        if (step?.values) this.showValues(step.values);
        for (const listener of this.listeners) listener(this.usage);
    }
    // showValues fills empty host memory fields from the values of a step.
    // An unedited field keeps the exact MiB value.
    showValues(values) {
        this.syncing = true;
        for (const [index, key] of ['memoryHighMiB', 'memoryMaxMiB'].entries()) {
            const row = this.capRows[index];
            if (row.text.trim() === '' && values[key]) { row.text = formatGiB(values[key]); row.edited = false; this.storedCap[index] = values[key]; }
        }
        this.syncing = false;
    }
    setRoutes(routes) {
        if (!Array.isArray(routes)) { this.routes.label = 'Escape routes were not checked.'; return; }
        this.routes.label = routes.length ? routes.map(escapeRouteText).join('\n\n') : 'The scan found no escape routes.';
    }
    setReserve(reserve) {
        this.storedReserve = {...(reserve ?? reserveDefaults)};
        this.syncing = true;
        reserveFields.forEach((field, index) => { this.reserveRows[index].text = field.format(this.storedReserve[field.key]); this.reserveRows[index].edited = false; });
        this.syncing = false;
    }
    // values gives the hostCap and limitReserve of the request. A reserve
    // equal to the defaults gives no limitReserve, so the profile keeps the
    // bytes of a profile without a stored reserve.
    values() {
        this.error.visible = false;
        const [high, max] = this.capRows.map(row => row.text.trim());
        let hostCap;
        if (high || max) {
            const values = this.capRows.map((row, index) => row.edited || this.storedCap[index] === undefined ? parseGiB(row.text) : this.storedCap[index]);
            if (values.includes(null)) return this.fail('Enter both host memory values in GiB, with at most one decimal.');
            if (values[0] >= values[1]) return this.fail('Host memory high must be less than host memory maximum.');
            hostCap = {memoryHighMiB: values[0], memoryMaxMiB: values[1]};
        }
        const reserve = {};
        for (const [index, field] of reserveFields.entries()) {
            const row = this.reserveRows[index];
            const text = row.text.trim();
            if (row.edited) reserve[field.key] = text === '' ? reserveDefaults[field.key] : field.parse(text);
            else reserve[field.key] = this.storedReserve[field.key];
            if (reserve[field.key] === null) return this.fail(`${field.title}: enter a number.`);
        }
        const custom = reserveFields.some(field => reserve[field.key] !== reserveDefaults[field.key]);
        return {hostCap, limitReserve: custom ? reserve : undefined};
    }
    fail(text) {
        this.error.label = text; this.error.visible = true;
        return {error: text};
    }
}

// hostRequest gives the setup request with the values of the page. Only a
// catalog with limits gets hostCap: the backend refuses it otherwise.
export function hostRequest(request, values, limited) {
    if (values.error) throw new PlainLimitError(values.error);
    const next = {...request, profile: {...request.profile}};
    delete next.hostCap;
    if (values.limitReserve) next.profile.limitReserve = values.limitReserve; else delete next.profile.limitReserve;
    if (limited && values.hostCap) next.hostCap = values.hostCap;
    return next;
}
