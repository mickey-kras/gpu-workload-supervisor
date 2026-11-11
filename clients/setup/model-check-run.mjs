// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Pango from 'gi://Pango';
import {blockingReason, reasonText} from './model-reasons.mjs';

// The source rules of gpu-setup model-check: a Hugging Face repository as
// owner/name and a pinned commit of 40 lowercase hex characters.
const repoPattern = /^[A-Za-z0-9][A-Za-z0-9._-]{0,95}\/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$/;
const revisionPattern = /^[0-9a-f]{40}$/;
export const validRepo = repo => repoPattern.test(repo) && !repo.includes('..');
export const validRevision = revision => revisionPattern.test(revision);

// A Hugging Face cache folder of a pinned snapshot:
// .../models--<owner>--<name>/snapshots/<revision>.
const cachePattern = /(?:^|\/)models--([^/]+?)--([^/]+)\/snapshots\/([0-9a-f]{40})\/?$/;

// cacheSource gives the source of a model folder in the Hugging Face cache,
// or null for another folder.
export function cacheSource(path) {
    const match = cachePattern.exec(path ?? '');
    if (!match) return null;
    const source = {repo: `${match[1]}/${match[2]}`, revision: match[3]};
    return validRepo(source.repo) ? source : null;
}

export const launchWarning = 'A result with an error, findings, an unsupported model or a pending template review blocks the launch of this workload until a new check allows it.';

// runNote gives the note of a finished run. A run whose hub listing failed
// is not stored: the last stored result stays.
function runNote(entry) {
    if (entry.notStored) {
        const reason = entry.checks?.find(check => check.name === 'upstream-identity')?.reason;
        return `This run failed. ${reasonText(reason)} The last stored result is kept.`;
    }
    const parts = entry.launchBlocked ? ['Checks finished. This result blocks the launch of this workload.', blockingReason(entry)] : ['Checks finished. The result is stored.'];
    if (entry.reviewPending) parts.push('Review the chat template below.');
    return parts.filter(Boolean).join(' ');
}

function hint(text) {
    const label = new Gtk.Label({label: text, wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, visible: false});
    label.add_css_class('dim-label');
    return label;
}

// CheckRunner adds the source fields, the launch warning and the Run checks
// button to the Model scan card of view. The button runs gpu-setup
// model-check for the accepted workload of the card.
export class CheckRunner {
    constructor(view) {
        this.view = view;
        this.root = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 6, visible: false});
        this.fields = new Adw.PreferencesGroup({title: 'Model source'});
        this.repo = new Adw.EntryRow({title: 'Repository (owner/name)', text: ''});
        this.revision = new Adw.EntryRow({title: 'Revision (40-character commit ID)', text: ''});
        this.repoHint = hint('Enter the Hugging Face repository as owner/name.');
        this.revisionHint = hint('Enter the full commit ID: 40 lowercase hex characters. A branch or tag name is not a pin.');
        for (const row of [this.repo, this.revision]) {
            this.fields.add(row);
            row.connect('changed', () => {
                if (!this.filling) this.edited = true;
                this.validate();
            });
        }
        this.root.append(this.fields); this.root.append(this.repoHint); this.root.append(this.revisionHint);
        const warning = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 8});
        warning.append(new Gtk.Image({icon_name: 'dialog-warning-symbolic', valign: Gtk.Align.START, accessible_role: Gtk.AccessibleRole.PRESENTATION}));
        warning.append(new Gtk.Label({label: launchWarning, wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, hexpand: true}));
        this.root.append(warning);
        this.unavailable = hint('Setup can check only a model folder of a managed launch. This workload names only a model ID.');
        this.button = new Gtk.Button({label: 'Run checks', visible: false});
        this.button.connect('clicked', () => this.run());
    }

    source() {
        return {repo: this.repo.text.trim(), revision: this.revision.text.trim()};
    }

    // validate marks each field that is not empty and not valid, and
    // allows a run only with a valid source.
    validate() {
        const {repo, revision} = this.source();
        for (const [row, label, valid, value] of [[this.repo, this.repoHint, validRepo(repo), repo], [this.revision, this.revisionHint, validRevision(revision), revision]]) {
            const wrong = value !== '' && !valid;
            label.visible = wrong;
            if (wrong) row.add_css_class('error'); else row.remove_css_class('error');
        }
        this.button.sensitive = validRepo(repo) && validRevision(revision) && !this.view.cancellable && !this.view.reading;
    }

    // fill sets the fields to the source of a stored result that applies,
    // or to the source in the name of a cache folder, until the operator
    // edits them.
    fill(entry, path) {
        if (this.edited) return;
        const source = (entry?.reason ? null : entry?.source) ?? cacheSource(path) ?? {repo: '', revision: ''};
        this.filling = true;
        this.repo.text = source.repo; this.revision.text = source.revision;
        this.filling = false;
    }

    // update shows the runner for an accepted LLM workload whose model
    // folder setup can check, while no other check runs.
    update(entry, state, path) {
        const llm = state === 'loaded' && entry?.kind === 'llm' && entry.reason !== 'model-changed';
        const available = llm && entry.checkAvailable === true;
        this.root.visible = available; this.unavailable.visible = llm && !available;
        this.button.visible = available && !this.view.cancellable;
        if (available) this.fill(entry, path);
        this.validate();
    }

    async run() {
        const {ui, workload} = this.view;
        const source = this.source();
        const outcome = await this.view.operate({start: 'Running checks… This can take a long time for large models.', failed: 'The checks did not store a result. The last result is kept.',
            action: 'model-check', focus: () => (this.button.visible ? this.button : null)}, async cancellable => {
            const entry = JSON.parse(await ui.command(['/usr/bin/gpu-setup', 'model-check'], JSON.stringify({version: 1, workload, source}), cancellable));
            if (entry?.workload !== workload) throw new Error('The result is not for this workload.');
            if (!entry.notStored) { ui.modelStatus.entries.set(workload, entry); this.edited = false; }
            return runNote(entry);
        });
        // A new run can store another review list: read the open list again.
        if (outcome === 'done' && !ui.closed && this.view.review.body.visible) await this.view.review.load();
    }
}

// blockedTexts give the text of the summary note for the cause of a block.
const blockedTexts = {
    review: 'This workload cannot start until you approve its chat template. Review it on its Model scan card.',
    result: 'This workload cannot start: its stored check result blocks the launch. See the cause on its Model scan card.',
    stale: 'This workload cannot start with its stored check result. Run the checks again on its Model scan card.',
    template: 'This workload cannot start: its launch pins another chat template than the checked template. Run setup again for this workload, then run the checks.',
    changed: 'This workload cannot start with its stored check result. After you finish setup, run the checks again on its Model scan card.',
};

function blockedKey(entry) {
    if (entry.reviewPending) return 'review';
    if (entry.selection) return 'changed';
    if (entry.reason === 'template-changed') return 'template';
    return entry.reason ? 'stale' : 'result';
}

// blockedNote gives the note of a review summary line: it tells why a
// workload cannot start and links to its Model scan card. update(entry)
// shows the text for a blocked entry, or hides the note for null.
export function blockedNote(ui, workload, app, label) {
    const root = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 2, visible: false});
    const row = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 8});
    row.append(new Gtk.Image({icon_name: 'dialog-warning-symbolic', valign: Gtk.Align.START, accessible_role: Gtk.AccessibleRole.PRESENTATION}));
    const text = new Gtk.Label({label: '', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, hexpand: true});
    row.append(text);
    root.append(row);
    const link = new Gtk.Button({label: `Open Model scan of ${label}`, halign: Gtk.Align.START}); link.add_css_class('flat'); link.add_css_class('setup-link');
    link.connect('clicked', () => ui.openApplicationCard(app, link, workload));
    root.append(link);
    return {root, update(entry) {
        root.visible = entry !== null;
        if (entry) text.label = blockedTexts[blockedKey(entry)];
    }};
}
