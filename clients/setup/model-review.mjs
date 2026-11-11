// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Pango from 'gi://Pango';

const wrapped = (label, extra = {}) => new Gtk.Label({label, wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, ...extra});

// reviewLine shows one literal of the template with its line. JSON quotes
// make spaces, line ends and other hidden text visible.
export const reviewLine = item => `Line ${item.line}: ${JSON.stringify(item.text)}`;

// TemplateReview adds the chat template review to the Model scan card of
// view. It reads the review list with gpu-setup model-review, and stores the
// approval of the operator with gpu-setup model-approve. The approval names
// the two SHA-256 values of the list that the operator read.
export class TemplateReview {
    constructor(view) {
        this.view = view;
        this.root = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 6, visible: false});
        this.toggle = new Gtk.Button({label: 'Review chat template', halign: Gtk.Align.START}); this.toggle.add_css_class('flat'); this.toggle.add_css_class('setup-link');
        this.toggle.update_state([Gtk.AccessibleState.EXPANDED], [Gtk.AccessibleTristate.FALSE]);
        this.toggle.connect('clicked', () => this.expand(!this.body.visible));
        this.body = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 6, visible: false});
        this.body.append(wrapped('The chat template adds this text to each conversation. Approve it only when you read each item and accept it. The approval applies to this template SHA-256 only.'));
        this.status = wrapped('', {selectable: true}); this.body.append(this.status);
        this.list = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 4}); this.body.append(this.list);
        this.approve = new Gtk.Button({label: 'Approve template', halign: Gtk.Align.START, visible: false}); this.approve.connect('clicked', () => this.store());
        this.body.append(this.approve);
        this.root.append(this.toggle); this.root.append(this.body);
    }

    // update shows the review only for a result that applies and asks for
    // a review.
    update(entry) {
        const pending = entry?.kind === 'llm' && entry.reviewPending === true && !entry.reason;
        this.root.visible = pending;
        if (!pending && this.body.visible) this.expand(false);
        this.approve.sensitive = Boolean(this.review?.pending) && !this.view.cancellable;
    }

    expand(open) {
        this.body.visible = open;
        this.toggle.update_state([Gtk.AccessibleState.EXPANDED], [open ? Gtk.AccessibleTristate.TRUE : Gtk.AccessibleTristate.FALSE]);
        if (open) this.load();
        else { this.reading?.cancel(); this.show(null, ''); }
    }

    // show lists the items of review, or the text of a state.
    show(review, text) {
        this.review = review;
        this.clear?.();
        const rows = review?.items?.map(item => wrapped(reviewLine(item), {selectable: true})) ?? [];
        for (const row of rows) this.list.append(row);
        this.clear = () => {
            for (const row of rows) this.list.remove(row);
            this.clear = null;
        };
        this.status.label = review?.pending ? `Template file: ${review.templateFile}\nTemplate SHA-256: ${review.chatTemplateSHA256}\nItems: ${review.items.length}` : text;
        this.approve.visible = Boolean(review?.pending);
        this.approve.sensitive = Boolean(review?.pending) && !this.view.cancellable;
    }

    // load reads the review list. A read that fails or that a newer read
    // replaced shows nothing to approve.
    async load() {
        const {ui, workload} = this.view;
        this.reading?.cancel();
        const reading = new Gio.Cancellable(); this.reading = reading;
        this.show(null, 'Reading the review list…');
        try {
            const review = JSON.parse(await ui.command(['/usr/bin/gpu-setup', 'model-review'], JSON.stringify({version: 1, workload}), reading));
            if (review?.version !== 1 || review.workload !== workload || !Array.isArray(review.items)) throw new Error('unsupported review list');
            if (this.reading === reading) this.show(review, 'No review is pending for the stored result.');
        } catch (error) {
            if (reading.is_cancelled()) return;
            console.warn(`gpu-setup model-review failed: ${error.message}`);
            if (this.reading === reading) this.show(null, 'Setup could not read the review list.');
        } finally {
            if (this.reading === reading) this.reading = null;
        }
    }

    // store approves the list that the operator read. When the stored
    // result changed, setup reads the status and the list again.
    async store() {
        const {ui, workload} = this.view;
        const {chatTemplateSHA256, reviewSHA256} = this.review;
        const stored = await this.view.operate({start: 'Storing the approval…', failed: 'The approval was not stored. The stored result changed or could not be read. Read the review list again.',
            action: 'model-approve', focus: () => (this.root.visible ? this.toggle : null)}, async cancellable => {
            const entry = JSON.parse(await ui.command(['/usr/bin/gpu-setup', 'model-approve'], JSON.stringify({version: 1, workload, chatTemplateSHA256, reviewSHA256}), cancellable));
            if (entry?.workload !== workload) throw new Error('The result is not for this workload.');
            ui.modelStatus.entries.set(workload, entry);
            return 'Template approved for this template SHA-256.';
        });
        if (!stored && !ui.closed) await this.view.reload();
    }
}
