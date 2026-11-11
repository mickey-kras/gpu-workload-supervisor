// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Pango from 'gi://Pango';
import {roundedCard} from './presentation.mjs';
import {CheckRunner, blockedNote} from './model-check-run.mjs';
import {TemplateReview} from './model-review.mjs';
import {blockingReason, reasonText} from './model-reasons.mjs';

// The status is read-only: it shows the stored check results of gpu-setup
// model-status and never shows a model as safe. Each state has an icon and
// a text, so the state does not depend on color.
const states = {
    passed: ['emblem-ok-symbolic', 'Scanned, no findings'],
    findings: ['dialog-warning-symbolic', 'Findings detected'],
    error: ['dialog-error-symbolic', 'Scan failed'],
    unsupported: ['dialog-information-symbolic', 'Scan not available for this model'],
    not_scanned: ['action-unavailable-symbolic', 'Not scanned'],
    image: ['dialog-information-symbolic', 'No backdoor scan available for image models'],
    unidentified: ['dialog-warning-symbolic', 'Model files not identified'],
    loading: ['content-loading-symbolic', 'Reading scan status…'],
    unavailable: ['dialog-warning-symbolic', 'Scan status not available'],
};
const reasons = {
    'no-result': 'No check result is stored for this model.',
    'other-directory': 'The stored result is for another model folder.',
    'checker-changed': 'The stored result is from another checker version.',
    'result-unusable': 'The stored result cannot be read.',
    'template-changed': 'The chat template of the launch is not the checked template.',
    'files-changed': 'The model files changed after the last check.',
    'files-unavailable': 'Setup cannot read the model files.',
    'files-unidentified': 'Setup cannot identify the model files of this workload.',
    'model-changed': 'The model changed in this selection. It has not been checked.',
};
// A recheck can change only these reasons, so only they keep the button.
const recheckable = new Set(['', 'files-changed', 'files-unavailable']);
const notes = {
    '': 'The model files match the last check.',
    'files-changed': 'The model files changed after the last check. The stored result no longer applies.',
    'files-unavailable': 'Setup cannot read the model files. The stored result does not apply.',
};
const actionNames = {'model-check': 'check', 'model-approve': 'approval'};
const checkNames = {'file-allowlist': 'File allowlist', 'upstream-identity': 'Upstream identity', 'chat-template': 'Chat template', 'behavioral-scan': 'Behavioral scan'};
const outcomes = {passed: 'passed', failed: 'failed', error: 'failed to run', not_run: 'not run', unsupported: 'not available for this model'};
export const defensiveNote = 'These checks are a defensive layer, not complete backdoor detection.';

// modelStatusText gives the icon, the title and the second line of an
// entry. A workload without an entry is not scanned, unless its application
// runs image models. An unknown status reads as not scanned.
export function modelStatusText(entry, app, state = 'loaded') {
    if (state !== 'loaded') return {icon: states[state][0], title: states[state][1], detail: state === 'unavailable' ? 'Setup could not read the stored check results.' : ''};
    const key = statusKey(entry, app);
    const [icon, title] = states[key];
    let detail = '';
    if (key !== 'image') detail = entry ? reasons[entry.reason] ?? '' : reasons['no-result'];
    if (key === 'not_scanned' && entry?.staticChecksPassed) detail = 'File checks passed. Behavioral scan not available yet.';
    if (['findings', 'error', 'unsupported'].includes(key) && !entry?.reason) detail = blockingReason(entry);
    if (entry?.reviewPending) detail = [detail, 'A chat template review is pending.'].filter(Boolean).join(' ');
    return {icon, title, detail};
}

const stored = new Set(['passed', 'findings', 'error', 'unsupported', 'not_scanned']);
function statusKey(entry, app) {
    if (entry?.kind === 'image' || (!entry && app === 'comfyui')) return 'image';
    if (entry?.kind === 'unidentified') return 'unidentified';
    return stored.has(entry?.status) ? entry.status : 'not_scanned';
}

// modelStatusDetails lists the checker revision, the coverage, the check
// date and each check of an entry, then the defensive note. The checks of
// a result that does not apply are labelled as the last stored result.
export function modelStatusDetails(entry, failedAction = '') {
    const lines = [`Checker revision: ${entry?.checker || 'not available'}`, 'Scanner revision: not available', `Coverage: ${entry?.coverage || 'none'}`,
        `Checked: ${checkDate(entry?.checkedAt)}`];
    if (entry?.reason && entry.checks?.length) lines.push('Last stored result. It does not apply now:');
    for (const check of entry?.checks ?? []) {
        const line = `${checkNames[check.name] ?? check.name}: ${outcomes[check.outcome] ?? check.outcome}`;
        lines.push(check.reason ? `${line}. ${reasonText(check.reason)}` : line);
    }
    if (failedAction) lines.push(`Error: the ${actionNames[failedAction] ?? 'status'} action of gpu-setup failed.`);
    return [...lines, defensiveNote].join('\n');
}

function checkDate(at) {
    return at ? at.slice(0, 16).replace('T', ' ') + ' UTC' : 'never';
}

// modelIdentity gives the SHA-256 that gpu-setup gives as the identity of
// the model files of a launch: of the model directory of a managed launch,
// else of the model ID. Setup compares it, and never shows it.
function modelIdentity(model, path) {
    return GLib.compute_checksum_for_string(GLib.ChecksumType.SHA256, path ? `path:${path}` : `model:${model ?? ''}`, -1);
}
// A launch without a pin has the empty pin, as in gpu-setup.
const profileLaunch = profile => ({identity: modelIdentity(profile?.nativeModel?.model, profile?.nativeModel?.owned?.modelPath), path: profile?.nativeModel?.owned?.modelPath, pin: profile?.nativeModel?.chatTemplateSHA256 ?? ''});
// As in StoredStatus, the template rule needs the complete list of checks.
// ReadRecord accepts only complete lists, so this guard only mirrors it.
const templatePassed = entry => entry.checks?.length === 4 && entry.checks.find(check => check.name === 'chat-template')?.outcome === 'passed';

// logFailure writes the error of gpu-setup to the application log. The
// card shows fixed text only.
function logFailure(error, action = 'model-status') {
    console.warn(`gpu-setup ${action} failed: ${error.message}`);
}

// readReport gives the entries of a model status report of version 1.
function readReport(output) {
    const report = JSON.parse(output);
    if (report?.version !== 1 || !Array.isArray(report.models)) throw new Error('unsupported model status report');
    return report.models;
}

// loadModelStatus reads the status of the accepted workloads once. A
// failure leaves each status unavailable; it never stops setup.
export async function loadModelStatus(ui) {
    ui.modelStatus = {state: 'loading', entries: new Map()};
    ui.modelStatusLoad = new Gio.Cancellable();
    try {
        const models = readReport(await ui.command(['/usr/bin/gpu-setup', 'model-status'], JSON.stringify({version: 1}), ui.modelStatusLoad));
        ui.modelStatus = {state: 'loaded', entries: new Map(models.map(entry => [entry.workload, entry]))};
    } catch (error) {
        if (!ui.modelStatusLoad?.is_cancelled()) logFailure(error);
        ui.modelStatus = {state: 'unavailable', entries: new Map(), error: true};
    }
    ui.modelStatusLoad = null;
    if (!ui.closed) refreshModelStatus(ui);
}

// cancelModelChecks stops the status load and each running check, for
// example when setup closes, so no gpu-setup process stays.
export function cancelModelChecks(ui) {
    ui.modelStatusLoad?.cancel();
    for (const view of ui.modelStatusViews?.values() ?? []) view.stop();
}

// refreshModelStatus shows the current status in each view, for example
// after the model of a selection changed.
export function refreshModelStatus(ui) {
    for (const view of ui.modelStatusViews?.values() ?? []) view.update();
}

// dropModelStatus removes the status card of an editor that setup removed,
// and stops its check.
export function dropModelStatus(ui, editor) {
    ui.modelStatusViews?.get(editor)?.stop();
    ui.modelStatusViews?.delete(editor);
}

// clearSummaryStatus removes the status lines of the review summary before
// setup builds the summary again.
export function clearSummaryStatus(ui) {
    for (const key of ui.modelStatusViews?.keys() ?? []) if (typeof key === 'string' && key.startsWith('summary:')) ui.modelStatusViews.delete(key);
}

// showModelStatus adds the status card of a workload to the main page of
// the settings of editor, once for each editor, and shows the current
// status. Each new editor of a workload has its own main page.
export function showModelStatus(ui, editor) {
    ui.modelStatusViews ??= new Map();
    if (!ui.modelStatusViews.has(editor)) {
        // A selection has no pin before its preview, so the settings card
        // adds only a comparison of the model path or ID. The review summary
        // also compares the pin.
        const proposed = () => {
            const draft = ui.drafts?.find(item => item.id === editor.id);
            return draft ? {identity: modelIdentity(draft.model ?? draft.binding?.model, draft.binding?.owned?.modelPath), path: draft.binding?.owned?.modelPath} : null;
        };
        const view = new StatusView(ui, editor.id, editor.app, proposed, true);
        editor.mainPage.append(view.root); ui.modelStatusViews.set(editor, view);
    }
    ui.modelStatusViews.get(editor).update();
}

// modelStatusLine gives the status line of a review summary card. Each
// workload has one summary line to update.
export function modelStatusLine(ui, profile, app) {
    ui.modelStatusViews ??= new Map();
    const view = new StatusView(ui, profile.id, app, () => profileLaunch(profile), false, profile.label || profile.id);
    ui.modelStatusViews.set(`summary:${profile.id}`, view); view.update();
    return view.root;
}

class StatusView {
    constructor(ui, workload, app, proposed, card, label = workload) {
        this.ui = ui; this.workload = workload; this.app = app; this.proposed = proposed;
        this.root = card ? roundedCard(Gtk) : new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 4});
        if (card) {
            const heading = new Gtk.Label({label: 'Model scan', xalign: 0, accessible_role: Gtk.AccessibleRole.HEADING}); heading.add_css_class('heading'); this.root.append(heading);
        }
        const row = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 8});
        this.icon = new Gtk.Image({valign: Gtk.Align.START, accessible_role: Gtk.AccessibleRole.PRESENTATION}); row.append(this.icon);
        const text = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 2, hexpand: true});
        this.title = new Gtk.Label({wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0}); text.append(this.title);
        this.detail = new Gtk.Label({wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0}); this.detail.add_css_class('dim-label'); text.append(this.detail);
        row.append(text); this.root.append(row);
        if (!card) { this.blocked = blockedNote(ui, workload, app, label); this.root.append(this.blocked.root); return; }
        this.toggle = new Gtk.Button({label: 'Scan details', halign: Gtk.Align.START}); this.toggle.add_css_class('flat'); this.toggle.add_css_class('setup-link');
        this.details = new Gtk.Label({wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, selectable: true, visible: false});
        this.toggle.update_state([Gtk.AccessibleState.EXPANDED], [Gtk.AccessibleTristate.FALSE]);
        this.toggle.connect('clicked', () => {
            this.details.visible = !this.details.visible;
            this.toggle.update_state([Gtk.AccessibleState.EXPANDED], [this.details.visible ? Gtk.AccessibleTristate.TRUE : Gtk.AccessibleTristate.FALSE]);
        });
        this.root.append(this.toggle); this.root.append(this.details);
        this.note = new Gtk.Label({wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, visible: false}); this.note.add_css_class('dim-label'); this.root.append(this.note);
        this.runner = new CheckRunner(this); this.root.append(this.runner.root); this.root.append(this.runner.unavailable);
        const actions = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 8, halign: Gtk.Align.START});
        actions.append(this.runner.button);
        this.recheck = new Gtk.Button({label: 'Check files again'}); this.recheck.connect('clicked', () => this.check()); actions.append(this.recheck);
        this.cancel = new Gtk.Button({label: 'Cancel check', visible: false}); this.cancel.connect('clicked', () => this.cancellable?.cancel()); actions.append(this.cancel);
        this.root.append(actions);
        this.review = new TemplateReview(this); this.root.append(this.review.root);
    }

    // stop ends each gpu-setup process of this view.
    stop() { this.cancellable?.cancel(); this.reading?.cancel(); this.review?.reading?.cancel(); }

    // entry gives the entry of this workload, with the rules of
    // StoredStatus. An LLM workload whose model path or model ID differs
    // from the launch that gpu-setup read is not scanned. This also covers an
    // interrupted setup, whose pending launch is not the accepted launch. A
    // result whose template check passed applies only to a launch with the
    // same pin; other results, for example findings, keep their status.
    entry(status) {
        const entry = status.entries.get(this.workload);
        if (entry?.kind !== 'llm') return entry;
        const proposed = this.proposed();
        if (proposed === null) return entry;
        const notScanned = (reason, launchBlocked = false) => ({workload: this.workload, kind: 'llm', status: 'not_scanned', reason, staticChecksPassed: false, launchBlocked});
        // The gate keeps a stored result by workload ID: it refuses that
        // result for a launch of another model folder.
        if (!entry.identity || proposed.identity !== entry.identity) return notScanned('model-changed', Boolean(entry.checkedAt && proposed.path));
        // The launch gate refuses a passed template check for another pin.
        if ('pin' in proposed && !entry.reason && templatePassed(entry) && proposed.pin !== (entry.templateSHA256 ?? '')) return notScanned('template-changed', true);
        return entry;
    }

    update() {
        const status = this.ui.modelStatus ?? {state: 'loading', entries: new Map()};
        const entry = this.entry(status);
        const {icon, title, detail} = modelStatusText(entry, this.app, status.state);
        this.icon.icon_name = icon; this.title.label = title; this.detail.label = detail; this.detail.visible = Boolean(detail);
        if (!this.details) { this.blocked.update(status.state === 'loaded' && entry?.launchBlocked === true ? entry : null); return; }
        // A note describes the entry of the last check. It goes when the
        // card shows another entry, for example after a model change.
        if (this.noted && !this.cancellable && entry !== this.noted.entry) { this.setNote(''); this.noted = null; this.failedAction = null; }
        this.details.label = modelStatusDetails(entry, this.failedAction ?? status.error);
        this.recheck.visible = Boolean(entry?.kind === 'llm' && entry.checkedAt && recheckable.has(entry.reason ?? '')) && !this.cancellable;
        this.runner.update(entry, status.state, this.proposed()?.path); this.review.update(entry);
    }

    setNote(label) { this.note.label = label; this.note.visible = Boolean(label); }

    // check reads the model files of the workload again. A canceled or
    // failed check keeps the last result.
    check() {
        return this.operate({start: 'Checking model files…', failed: 'The files could not be checked again. The last result is kept.', focus: () => (this.recheck.visible ? this.recheck : null)}, async cancellable => {
            const models = readReport(await this.ui.command(['/usr/bin/gpu-setup', 'model-status'], JSON.stringify({version: 1, recheck: [this.workload]}), cancellable));
            const entry = models.find(item => item.workload === this.workload);
            if (!entry) throw new Error('The report has no status for this workload.');
            this.ui.modelStatus.entries.set(this.workload, entry);
            return entry.rechecked ? notes[entry.reason ?? ''] ?? reasons[entry.reason] ?? '' : reasons[entry.reason] ?? '';
        });
    }

    // operate runs one gpu-setup action of this card that the operator can
    // cancel, and shows its note. Focus moves from the Cancel button to
    // focus(), when it is visible. It gives 'done', 'failed' or 'canceled'.
    // A failed or canceled check of files keeps the last result. A failed
    // or canceled action that writes reads the stored status again, see
    // reload: the action can stop after its write.
    async operate({start, failed, action = 'model-status', canceled = 'Check canceled. The last result is kept.', cancelLabel = 'Cancel check', focus}, work) {
        const before = this.entry(this.ui.modelStatus ?? {entries: new Map()});
        this.cancellable = new Gio.Cancellable(); this.cancel.label = cancelLabel; this.cancel.visible = true; this.failedAction = null; this.noted = null; this.setNote(start); this.update();
        this.cancel.grab_focus();
        let outcome = 'done';
        try {
            this.setNote(await work(this.cancellable));
        } catch (error) {
            outcome = this.cancellable.is_cancelled() ? 'canceled' : 'failed';
            if (outcome === 'failed') { this.failedAction = action; logFailure(error, action); }
            this.setNote(outcome === 'canceled' ? canceled : failed);
        } finally {
            // Focus moves only from the Cancel button that this action hides.
            const focused = this.cancel.is_focus();
            this.cancellable = null; this.cancel.visible = false;
            this.noted = {entry: this.entry(this.ui.modelStatus)};
            if (!this.ui.closed) {
                refreshModelStatus(this.ui);
                const target = focus();
                if (focused) (target ?? this.toggle).grab_focus();
            }
        }
        if (outcome !== 'done' && action !== 'model-status' && !this.ui.closed) await this.reload(before);
        return outcome;
    }

    // reload reads the status again after a failed or canceled action that
    // writes. When the stored result differs from the result before the
    // action, the note says so; else it keeps the note of the action. It
    // reads the review list again when it is open. A failed read keeps the
    // status that the card shows.
    async reload(before) {
        let note = this.note.label;
        const failedAction = this.failedAction;
        const reading = new Gio.Cancellable(); this.reading = reading;
        try {
            const models = readReport(await this.ui.command(['/usr/bin/gpu-setup', 'model-status'], JSON.stringify({version: 1}), reading));
            this.ui.modelStatus = {state: 'loaded', entries: new Map(models.map(entry => [entry.workload, entry]))};
            if (changed(before, this.entry(this.ui.modelStatus))) note = 'The action stopped after setup stored a new result. The card shows the stored result.';
        } catch (failure) {
            if (!reading.is_cancelled()) logFailure(failure);
        }
        this.reading = null;
        if (this.ui.closed || reading.is_cancelled()) return;
        this.noted = {entry: this.entry(this.ui.modelStatus)}; this.setNote(note); this.failedAction = failedAction;
        refreshModelStatus(this.ui);
        if (this.review.body.visible) await this.review.load();
    }
}

// changed tells whether two entries describe different stored results.
const resultKey = entry => JSON.stringify([entry?.checkedAt, entry?.status, entry?.reason, entry?.reviewPending, entry?.checks]);
const changed = (before, after) => resultKey(before) !== resultKey(after);
