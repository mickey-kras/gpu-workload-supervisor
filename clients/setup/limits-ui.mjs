import {wrappedCheckButton} from './presentation.mjs';

// Unit resource limits of the Resource checks page, the shared number
// formats and the plain text for limit errors. The backend checks every
// value again; these checks only give the operator an early, plain answer.

// PlainLimitError carries a text for the operator as it is.
export class PlainLimitError extends Error {}

export const unitLimitDefaults = Object.freeze({tasksMax: 4096, cpuQuotaPercent: 400});
const mebibyte = 1024 * 1024;

// GiB with at most one decimal, stored as MiB rounded down.
export function parseGiB(text) {
    const match = /^(\d{1,8})(?:\.(\d))?$/.exec(text.trim());
    if (!match) return null;
    return Number(match[1]) * 1024 + Math.floor(Number(match[2] ?? 0) * 1024 / 10);
}

export function formatGiB(mib) { return String(Math.floor(mib * 10 / 1024) / 10); }

// Cores with at most two decimals, stored as a percent of one core.
export function parseCores(text) {
    const match = /^(\d{1,4})(?:\.(\d{1,2}))?$/.exec(text.trim());
    if (!match) return null;
    return Number(match[1]) * 100 + Number((match[2] ?? '').padEnd(2, '0'));
}

export function formatCores(percent) { return String(percent / 100); }

export function parseCount(text) {
    const match = /^\d{1,10}$/.exec(text.trim());
    return match ? Number(match[0]) : null;
}

const parseMiBPerSecond = text => {
    const value = parseCount(text);
    return value === null ? null : value * mebibyte;
};

const count = {parse: parseCount, format: String};
const memory = {parse: parseGiB, format: formatGiB, range: '0.3 to 16384 GiB', min: 256, max: 16 * 1024 * 1024};
const bandwidth = {parse: parseMiBPerSecond, format: value => String(value / mebibyte), range: '1 to 1048576 MiB/s', min: mebibyte, max: 2 ** 40};
const operations = {...count, range: '100 to 10000000', min: 100, max: 10000000};
const weight = {...count, range: '1 to 10000', min: 1, max: 10000};

// The fields of the form. advanced fields are in the closed section.
export const limitFields = [
    {key: 'memoryHighMiB', title: 'Memory high (GiB)', ...memory, required: true},
    {key: 'memoryMaxMiB', title: 'Memory maximum (GiB)', ...memory, required: true},
    {key: 'tasksMax', title: 'Tasks', ...count, range: '64 to 4194304', min: 64, max: 4194304, required: true},
    {key: 'cpuQuotaPercent', title: 'CPU (cores)', parse: parseCores, format: formatCores, range: '0.01 to 1024 cores', min: 1, max: 102400, required: true},
    {key: 'cpuWeight', title: 'CPU weight', ...weight, advanced: true},
    {key: 'ioDevice', title: 'I/O device (for example /dev/nvme0n1)', text: /^\/dev\/[a-z][a-z0-9]{0,31}$/, advanced: true},
    {key: 'ioDeviceNumber', title: 'I/O device number (MAJ:MIN)', text: /^\d{1,7}:\d{1,7}$/, advanced: true},
    {key: 'ioReadBandwidthMax', title: 'Read bandwidth (MiB/s)', ...bandwidth, advanced: true, io: true},
    {key: 'ioWriteBandwidthMax', title: 'Write bandwidth (MiB/s)', ...bandwidth, advanced: true, io: true},
    {key: 'ioReadIOPSMax', title: 'Read operations per second', ...operations, advanced: true, io: true},
    {key: 'ioWriteIOPSMax', title: 'Write operations per second', ...operations, advanced: true, io: true},
    {key: 'ioWeight', title: 'I/O weight', ...weight, advanced: true, io: true},
];

// readField gives the stored value of one field. An unedited field keeps
// its stored value, so a value that the form cannot show exactly is never
// changed by a save.
function readField(field, row, stored) {
    const text = row.text.trim();
    if (!row.edited) return {value: stored[field.key]};
    if (text === '') return {value: undefined};
    if (field.text) return field.text.test(text) ? {value: text} : {error: `${field.title}: the value has the wrong format.`};
    const value = field.parse(text);
    if (value === null) return {error: `${field.title}: enter a number.`};
    if (value < field.min || value > field.max) return {error: `${field.title} must be ${field.range}.`};
    return {value};
}

// checkLimits gives the first plain problem of a limits object, or null.
export function checkLimits(limits) {
    for (const field of limitFields.filter(item => item.required)) {
        if (!limits[field.key]) return `Enter a value for ${field.title}.`;
    }
    if (limits.memoryHighMiB >= limits.memoryMaxMiB) return 'Memory high must be less than the memory maximum.';
    const io = limitFields.some(field => field.io && limits[field.key]);
    const device = Boolean(limits.ioDevice || limits.ioDeviceNumber);
    if (io ? !(limits.ioDevice && limits.ioDeviceNumber) : device) return 'I/O limits need the I/O device and its number, and the device needs an I/O limit.';
    return null;
}

// UnitLimitsForm edits the limits of one owned unit. value() gives
// undefined for no change, null to remove the limits, a limits object, or
// an error text. Controls without a field, such as the swap limit, keep
// their stored value.
export class UnitLimitsForm {
    constructor({Adw, Gtk, page, initial, changed}) {
        this.stored = initial ? {...initial} : {};
        this.initial = initial ?? null;
        this.changed = changed;
        this.syncing = false;
        this.header = new Adw.PreferencesGroup({title: 'Resource limits', description: 'Only a launch that Supervisor manages can have limits. To limit an existing service, convert it to a managed launch, finish setup, then add the limits in a second setup run.'});
        this.enable = wrappedCheckButton(Gtk, 'Limit the resources of this workload', {active: Boolean(initial)});
        this.usage = new Gtk.Label({label: '', wrap: true, xalign: 0, visible: false});
        this.usage.add_css_class('dim-label');
        this.error = new Gtk.Label({label: '', wrap: true, xalign: 0, visible: false, selectable: true});
        this.error.add_css_class('setup-notice');
        this.advanced = new Adw.ExpanderRow({title: 'I/O and weight', expanded: false});
        this.rows = new Map();
        for (const field of limitFields) this.addRow(Adw, field);
        this.header.add(this.enable); this.header.add(this.usage);
        this.fields = new Adw.PreferencesGroup();
        for (const widget of [...limitFields.filter(field => !field.advanced).map(field => this.rows.get(field.key)), this.advanced]) this.fields.add(widget);
        for (const widget of [this.header, this.fields, this.error]) page.append(widget);
        this.enable.connect('toggled', () => { if (!this.syncing) this.toggled(); });
        this.toggled(false);
    }
    addRow(Adw, field) {
        const value = this.stored[field.key];
        const row = new Adw.EntryRow({title: field.title, text: value === undefined ? '' : field.format?.(value) ?? String(value)});
        row.edited = false;
        row.connect('changed', () => {
            if (this.syncing) return;
            row.edited = true; this.changed?.();
        });
        this.rows.set(field.key, row);
        if (field.advanced) this.advanced.add_row(row);
    }
    toggled(notify = true) {
        const active = this.enable.active;
        for (const row of this.rows.values()) row.sensitive = active;
        this.advanced.sensitive = active;
        if (active) this.fillDefaults();
        if (notify) this.changed?.();
    }
    fillDefaults() {
        this.syncing = true;
        for (const [key, value] of Object.entries(unitLimitDefaults)) {
            const row = this.rows.get(key);
            const field = limitFields.find(item => item.key === key);
            if (row.text.trim() === '') { row.text = field.format(value); row.edited = true; }
        }
        this.syncing = false;
    }
    // capture and restore keep the form of a cancelled settings session.
    capture() {
        return {active: this.enable.active, rows: [...this.rows.values()].map(row => [row, row.text, row.edited])};
    }
    restore(snapshot) {
        this.syncing = true;
        this.enable.active = snapshot.active;
        for (const [row, text, edited] of snapshot.rows) { row.text = text; row.edited = edited; }
        this.syncing = false;
        this.toggled(false);
    }
    // setUsage shows the memory use of the slice beside the fields.
    setUsage(usage) {
        this.usage.visible = Boolean(usage);
        if (usage) this.usage.label = `Host memory ceiling: ${formatGiB(usage.ceilingMiB)} GiB. In use now: ${formatGiB(usage.memoryInUseMiB)} GiB. ${formatGiB(usage.pageCacheMiB)} GiB of page cache that the kernel can free does not count.`;
    }
    value() {
        this.error.visible = false;
        if (!this.enable.active) return this.initial ? null : undefined;
        const limits = {...this.stored, version: 1};
        for (const field of limitFields) {
            const result = readField(field, this.rows.get(field.key), this.stored);
            if (result.error) return this.fail(result.error);
            if (result.value === undefined) delete limits[field.key]; else limits[field.key] = result.value;
        }
        const problem = checkLimits(limits);
        return problem ? this.fail(problem) : limits;
    }
    fail(text) {
        this.error.label = text; this.error.visible = true;
        return {error: text};
    }
}

// withLimits puts the limits of a form value on an owned profile. An
// adopted source unit or a profile without an owned launch refuses limits:
// setup converts the unit first, and the limits come in a second apply.
export function withLimits(profile, value, adopted) {
    if (value === undefined) return profile;
    if (value?.error) throw new PlainLimitError(value.error);
    const owned = profile.nativeModel?.owned;
    if (value !== null && (!owned || adopted)) throw new PlainLimitError(limitErrorText('resource limits need a supervisor-owned unit'));
    if (!owned) return profile;
    const next = {...owned};
    if (value === null) delete next.limits; else next.limits = value;
    return {...profile, nativeModel: {...profile.nativeModel, owned: next}};
}

// Plain text for the errors of the limit checks, in the order of the
// desktop table of RESOURCE-LIMITS.md. The raw text stays in the details.
const errorTexts = [
    [/resource limits need a supervisor-owned unit/, () => 'Limits need a launch that Supervisor manages. Convert this service first and finish setup. Then add the limits in a second setup run.'],
    [/required cgroup controller is not enabled for this account: (\w+)/, match => `This computer does not let the account limit ${match[1]}. An administrator must change the account settings.`],
    [/system cannot apply a resource limit: (\S+) is missing \((.+?) is off\)/, match => `The system cannot apply ${match[1].split('/').pop()}. ${match[2]} is off on this computer.`],
    [/host protection of this account is missing or different/, () => 'The host protection of this account is missing or different. See Host protection in Setup settings.'],
    [/hostCap needs a unit with resource limits/, () => 'Host protection values need a workload with limits. Clear the host memory fields, or turn on limits for a workload.'],
    [/resource limit is unset or leaves too little room for the supervisor/, () => 'This limit leaves too little room for the supervisor. Use a lower limit or raise the host protection values, or turn the limits off. The details give the allowed values.'],
    [/slice memory does not fit this limit/, () => 'Slice memory does not fit this limit. Stop other programs of this account, or lower the memory maximum. Check /dev/shm and tmpfs use.'],
    [/does not support I\/O weight/, () => 'This I/O device does not support I/O weight. Clear the I/O weight.'],
    [/I\/O device of the resource limits is not usable/, () => 'The I/O device of the limits is not usable. Check the device and its number.'],
];

export function limitErrorText(message) {
    for (const [pattern, text] of errorTexts) {
        const match = pattern.exec(message);
        if (match) return text(match);
    }
    return null;
}

// limitSummary gives the plain summary of a setup error, or null when the
// error is no limit error.
export function limitSummary(error) {
    return error instanceof PlainLimitError ? error.message : limitErrorText(error.message);
}
