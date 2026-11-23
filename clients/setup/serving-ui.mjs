import {PlainLimitError, parseCount} from './limits-ui.mjs';
import {wrappedCheckButton} from './presentation.mjs';

// Serving options of the Launch details page: the bounded vLLM KV cache and
// batching options of one supervisor-owned launch, the shared number formats
// and the plain text for serving errors. The backend checks every value
// again; these checks only give the operator an early, plain answer. The
// fields and bounds mirror internal/control/serving_options.go.

// servingDefaults are the values a new form shows before the operator edits
// them. An unedited field keeps its stored value, so a draft without it
// keeps its exact bytes.
export const servingDefaults = Object.freeze({gpuMemoryPercent: 90});

// parseUtilization reads the exact two-digit fraction that vLLM reads, such
// as 0.90, and returns the whole percentage it stores. The rendered value is
// exact and a vLLM update cannot change it.
export function parseUtilization(text) {
    const match = /^0\.(\d{2})$/.exec(text.trim());
    return match ? Number(match[1]) : null;
}

export function formatUtilization(percent) { return `0.${String(percent).padStart(2, '0')}`; }

const utilization = {parse: parseUtilization, format: formatUtilization, range: '0.50 to 0.95', min: 50, max: 95};
const count = {parse: parseCount, format: String};

// The fields of the form. An empty optional field keeps the vLLM default:
// maxNumSeqs and maxNumBatchedTokens render only when set.
export const servingFields = [
    {key: 'gpuMemoryPercent', title: 'GPU memory utilization', ...utilization, hint: 'Renders as --gpu-memory-utilization 0.90 when unset.'},
    {key: 'maxNumSeqs', title: 'Maximum concurrent sequences', ...count, range: '1 to 64', min: 1, max: 64},
    {key: 'maxNumBatchedTokens', title: 'Chunked prefill budget (tokens)', ...count, range: '256 to 65536', min: 256, max: 65536},
];

// readField returns the value of one field. An unedited field keeps
// its stored value, so a value that the form cannot show exactly is never
// changed by a save.
function readField(field, row, stored) {
    const text = row.text.trim();
    if (!row.edited) return {value: stored[field.key]};
    if (text === '') return {value: undefined};
    const value = field.parse(text);
    if (value === null) return {error: `${field.title}: enter a number.`};
    if (value < field.min || value > field.max) return {error: `${field.title} must be ${field.range}.`};
    return {value};
}

// checkServing returns the first plain-language problem with serving
// options, or null. It mirrors control.ServingOptions.Validate; the backend checks
// again before it renders or applies a launch.
export function checkServing(serving) {
    if (serving.gpuMemoryPercent !== undefined && (serving.gpuMemoryPercent < 50 || serving.gpuMemoryPercent > 95))
        return 'GPU memory utilization must be 0.50 to 0.95.';
    if (serving.maxNumSeqs !== undefined && (serving.maxNumSeqs < 1 || serving.maxNumSeqs > 64))
        return 'Maximum concurrent sequences must be 1 to 64.';
    if (serving.maxNumBatchedTokens !== undefined && (serving.maxNumBatchedTokens < 256 || serving.maxNumBatchedTokens > 65536))
        return 'Chunked prefill budget must be 256 to 65536 tokens.';
    return null;
}

// ServingOptionsForm edits the serving options of one owned launch. value()
// gives undefined for no change, null to remove the options, an options
// object, or an error text. Without the options, the unit keeps its earlier
// bytes and fingerprint.
export class ServingOptionsForm {
    constructor({Adw, Gtk, page, initial, changed}) {
        this.stored = initial ? {...initial} : {};
        this.initial = initial ?? null;
        this.changed = changed;
        this.syncing = false;
        this.header = new Adw.PreferencesGroup({title: 'KV cache and batching',
            description: 'Pin the vLLM KV cache and batching defaults so a vLLM update cannot change them silently. Without it, the unit keeps its earlier bytes and fingerprint.'});
        this.enable = wrappedCheckButton(Gtk, 'Pin the KV cache and batching defaults', {active: Boolean(initial)});
        this.error = new Gtk.Label({label: '', name: 'serving-error', wrap: true, xalign: 0, visible: false, selectable: true});
        this.error.add_css_class('setup-notice');
        this.rows = new Map();
        for (const field of servingFields) this.addRow(Adw, field);
        // The KV cache dtype stays auto: fp8 needs a qualification run for
        // the model and the GPU (#453), and no qualification evidence
        // exists yet. The row stays visible, so the operator sees the pin.
        this.dtype = new Adw.EntryRow({title: 'KV cache dtype', text: this.stored.kvCacheDType || 'auto'});
        this.dtype.editable = false;
        this.dtypeNote = new Gtk.Label({label: 'fp8 needs a qualification run for this model and GPU (#453). Until that run exists, the launch pins auto.', wrap: true, xalign: 0});
        this.dtypeNote.add_css_class('dim-label');
        this.prefixCaching = wrappedCheckButton(Gtk, 'Disable prefix caching (diagnosis only)', {active: Boolean(initial?.disablePrefixCaching)});
        this.prefixCaching.connect('toggled', () => { if (!this.syncing) this.changed?.(); });
        this.header.add(this.enable);
        this.fields = new Adw.PreferencesGroup({description: 'With prefix caching on, the launch renders --enable-prefix-caching --prefix-caching-hash-algo sha256. Disabling it for diagnosis renders --no-enable-prefix-caching instead; the two never appear together.'});
        for (const field of servingFields) this.fields.add(this.rows.get(field.key));
        this.fields.add(this.dtype);
        this.fields.add(this.dtypeNote);
        this.fields.add(this.prefixCaching);
        for (const widget of [this.header, this.fields, this.error]) page.append(widget);
        this.enable.connect('toggled', () => { if (!this.syncing) this.toggled(); });
        this.toggled(false);
        this.state = formState(this.value(true));
    }
    addRow(Adw, field) {
        const value = this.stored[field.key];
        // The utilization row shows the default before an edit; an unedited
        // row stores nothing, so a draft without the field keeps its bytes.
        let text = '';
        if (value !== undefined) text = field.format(value);
        else if (field.key === 'gpuMemoryPercent') text = field.format(servingDefaults.gpuMemoryPercent);
        const row = new Adw.EntryRow({title: field.title, text});
        row.edited = false;
        row.connect('changed', () => {
            if (this.syncing) return;
            row.edited = true; this.changed?.();
        });
        this.rows.set(field.key, row);
    }
    toggled(notify = true) {
        const active = this.enable.active;
        for (const row of this.rows.values()) row.sensitive = active;
        this.dtype.sensitive = active;
        this.prefixCaching.sensitive = active;
        if (notify) this.changed?.();
    }
    // capture and restore keep the form of a cancelled settings session.
    // The snapshot covers the backing serving state too, so a restore after
    // a mid-session sync cannot resurrect the values of the other
    // installation.
    capture() {
        return {active: this.enable.active, rows: [...this.rows.values()].map(row => [row, row.text, row.edited]), prefixCaching: this.prefixCaching.active,
            stored: {...this.stored}, initial: this.initial ? {...this.initial} : null};
    }
    restore(snapshot) {
        this.syncing = true;
        this.stored = {...snapshot.stored};
        this.initial = snapshot.initial ? {...snapshot.initial} : null;
        this.enable.active = snapshot.active;
        for (const [row, text, edited] of snapshot.rows) { row.text = text; row.edited = edited; }
        this.dtype.text = this.stored.kvCacheDType || 'auto';
        this.prefixCaching.active = snapshot.prefixCaching;
        this.syncing = false;
        this.toggled(false);
    }
    // sync adopts the serving options of a selected candidate. The probe
    // seeds the pinned defaults on a new managed launch, so the form shows
    // them instead of the state of the stored draft.
    sync(value) {
        this.stored = value ? {...value} : {};
        this.initial = value ?? null;
        this.syncing = true;
        this.enable.active = Boolean(value);
        this.dtype.text = this.stored.kvCacheDType || 'auto';
        for (const field of servingFields) {
            const row = this.rows.get(field.key);
            const stored = this.stored[field.key];
            let text = '';
            if (stored !== undefined) text = field.format(stored);
            else if (field.key === 'gpuMemoryPercent') text = field.format(servingDefaults.gpuMemoryPercent);
            row.text = text;
            row.edited = false;
        }
        this.prefixCaching.active = Boolean(value?.disablePrefixCaching);
        this.syncing = false;
        this.toggled(false);
        this.state = formState(this.value(true));
    }
    // value returns the form value. With quiet, it leaves the error text
    // visibility alone.
    value(quiet = false) {
        if (!quiet) this.error.visible = false;
        if (!this.enable.active) return this.initial ? null : undefined;
        const serving = {...this.stored, version: 1};
        const invalid = this.readFields(serving);
        if (invalid) return quiet ? invalid : this.fail(invalid.error);
        if (this.prefixCaching.active) serving.disablePrefixCaching = true; else delete serving.disablePrefixCaching;
        const problem = checkServing(serving);
        if (!problem) return serving;
        return quiet ? {error: problem} : this.fail(problem);
    }
    // readFields puts the edited field values in the serving object. It
    // returns the first field error, or null.
    readFields(serving) {
        for (const field of servingFields) {
            const result = readField(field, this.rows.get(field.key), this.stored);
            if (result.error) return result;
            if (result.value === undefined) delete serving[field.key]; else serving[field.key] = result.value;
        }
        return null;
    }
    fail(text) {
        this.error.label = text; this.error.visible = true;
        return {error: text};
    }
}

// withFormServing puts a valid value of the serving form in an owned draft
// value and in the draft input, so render-owned renders and fingerprints
// the final unit, and saved drafts keep the options. A form that is off
// removes the options. An invalid value does not change the draft input,
// but it starts a new draft generation. checkFormServing refuses it before
// a render or a save.
export function withFormServing(form, draft, value) {
    if (!form) return value;
    const serving = form.value(true);
    // A change of the form value, also to an invalid one, is a new draft
    // generation, so a pending render of the earlier value is discarded.
    const state = formState(serving);
    if (state !== form.state) { form.state = state; draft.cancel(); }
    if (!value.binding?.owned) return value;
    if (serving?.error) return value;
    const owned = {...value.binding.owned};
    if (serving) owned.serving = serving; else delete owned.serving;
    const next = {...value, binding: {...value.binding, owned}};
    draft.input = {...draft.input, binding: next.binding};
    return next;
}

// formState is the comparable value of the form: its serving options, none,
// or an invalid value.
function formState(serving) {
    return serving?.error ? 'invalid' : JSON.stringify(serving ?? null);
}

const adoptWithServingText = 'KV cache and batching options need a launch that Supervisor manages. Clear Pin the KV cache and batching defaults to adopt this service, or use a managed launch.';

// checkFormServing refuses an invalid form, and serving options on a draft
// without an owned launch.
export function checkFormServing(form, input) {
    const serving = form.value();
    if (serving?.error) throw new PlainLimitError(serving.error);
    if (serving && !input.binding?.owned) throw new PlainLimitError(adoptWithServingText);
}

// servingSummary returns a one-line plain summary of a profile's serving
// options, or null when it has none. The finish page shows it under the
// workload card.
export function servingSummary(profile) {
    const serving = profile.nativeModel?.owned?.serving;
    if (!serving || !Object.values(serving).some(Boolean)) return null;
    const parts = [`KV cache ${serving.kvCacheDType || 'auto'}`, `GPU memory ${formatUtilization(serving.gpuMemoryPercent ?? servingDefaults.gpuMemoryPercent)}`, 'chunked prefill'];
    parts.push(serving.disablePrefixCaching ? 'prefix caching off (diagnosis)' : 'prefix caching (sha256)');
    if (serving.maxNumSeqs) parts.push(`up to ${serving.maxNumSeqs} sequences`);
    if (serving.maxNumBatchedTokens) parts.push(`${serving.maxNumBatchedTokens} batched tokens`);
    return parts.join(' · ');
}

// servingSummaries returns one plain line per profile that pins the vLLM KV
// cache and batching defaults. The finish page lists them under the
// previewed changes.
export function servingSummaries(profiles) {
    const lines = [];
    for (const profile of profiles) {
        const summary = servingSummary(profile);
        if (summary) lines.push(`${profile.label || profile.id}: ${summary}`);
    }
    return lines;
}
