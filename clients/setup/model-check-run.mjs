// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Pango from 'gi://Pango';
import {blockingReason} from './model-reasons.mjs';

export const launchWarning = 'A result with an error, findings or an unsupported model blocks the launch of this workload until a new check allows it.';

// kept describes what remains when a run stores nothing: the last stored
// result, or no result.
const kept = hadResult => (hadResult ? 'The last result is kept.' : 'Setup stored nothing.');

function runNote(entry) {
    const parts = entry.launchBlocked ? ['Checks finished. This result blocks the launch of this workload.', blockingReason(entry)] : ['Checks finished. The result is stored.'];
    return parts.filter(Boolean).join(' ');
}

function hint(text) {
    const label = new Gtk.Label({label: text, wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, visible: false});
    label.add_css_class('dim-label');
    return label;
}

// CheckRunner adds the launch warning and the Run checks button to the
// Model scan card of view. The button runs gpu-setup model-check for the
// accepted workload of the card.
export class CheckRunner {
    constructor(view) {
        this.view = view;
        this.root = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 6, visible: false});
        const warning = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 8});
        warning.append(new Gtk.Image({icon_name: 'dialog-warning-symbolic', valign: Gtk.Align.START, accessible_role: Gtk.AccessibleRole.PRESENTATION}));
        warning.append(new Gtk.Label({label: launchWarning, wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, hexpand: true}));
        this.root.append(warning);
        this.unavailable = hint('Setup can check only a model folder of a managed launch. This workload names only a model ID.');
        this.button = new Gtk.Button({label: 'Run checks', visible: false});
        this.button.connect('clicked', () => this.run());
    }

    // update shows the runner for an accepted LLM workload whose model
    // folder setup can check, while no other check runs.
    update(entry, state) {
        const llm = state === 'loaded' && entry?.kind === 'llm' && entry.reason !== 'model-changed';
        const available = llm && entry.checkAvailable === true;
        this.root.visible = available; this.unavailable.visible = llm && !available;
        this.button.visible = available && !this.view.cancellable;
        this.button.sensitive = !this.view.cancellable && !this.view.reading;
    }

    async run() {
        const {ui, workload} = this.view;
        const hadResult = Boolean(ui.modelStatus.entries.get(workload)?.checkedAt);
        await this.view.operate({start: 'Running checks… This can take a long time for large models.', failed: `The checks did not store a result. ${kept(hadResult)}`,
            canceled: `Check canceled. ${kept(hadResult)}`, action: 'model-check', focus: () => (this.button.visible ? this.button : null)}, async cancellable => {
            const entry = JSON.parse(await ui.command(['/usr/bin/gpu-setup', 'model-check'], JSON.stringify({version: 1, workload}), cancellable));
            if (entry?.workload !== workload) throw new Error('The result is not for this workload.');
            ui.modelStatus.entries.set(workload, entry);
            return runNote(entry);
        });
    }
}

// blockedTexts give the text of the summary note for the cause of a block.
const blockedTexts = {
    result: 'This workload cannot start: its stored check result blocks the launch. See the cause on its Model scan card.',
    stale: 'This workload cannot start with its stored check result. Run the checks again on its Model scan card.',
    template: 'This workload cannot start: its launch pins another chat template than the checked template. Run setup again for this workload, then run the checks.',
    changed: 'This workload cannot start with its stored check result. After you finish setup, run the checks again on its Model scan card.',
};

function blockedKey(entry) {
    if (entry.selection) return 'changed';
    if (entry.reason === 'template-changed') return 'template';
    return entry.reason ? 'stale' : 'result';
}

// blockedNote builds the note on a review summary line that explains why a
// workload cannot start and links to its Model scan card. update(entry)
// shows the text for a blocked entry and hides the note for null.
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
