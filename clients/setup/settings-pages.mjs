// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import GObject from 'gi://GObject';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gdk from 'gi://Gdk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Pango from 'gi://Pango';
import {applicationHeader, linkButton, roundedCard} from './presentation.mjs';
import {applications, candidateChoice, candidateIdentity, candidateMessage} from './onboarding.mjs';

// Settings share the wizard's scroller and fixed footer. Each optional section
// has its own page, so long service identities never compete with entry fields.
export class InstallationSettings {
    constructor({Adw, Gtk, app, window, navigate}) {
        this.Gtk = Gtk; this.navigate = navigate;
        this.busy = false;
        this.appLabel = applications.find(item => item.id === app).label;
        this.root = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18});
        this.pages = new Map(); this.history = []; this.page = 'main'; this.ready = false;
        for (const id of ['main', 'selection', 'advanced', 'resources', 'launch', 'configuration']) {
            const page = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18, visible: id === 'main'});
            this.pages.set(id, page); this.root.append(page);
        }
        const main = this.pages.get('main');
        const heading = new Gtk.Label({label: 'Installation', xalign: 0}); heading.add_css_class('heading'); main.append(heading);
        this.identity = new Gtk.Label({label: 'No installation selected', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, selectable: true, hexpand: true});
        this.identity.add_css_class('dim-label');
        this.state = new Gtk.Label({label: 'Unverified', xalign: 0}); this.state.add_css_class('dim-label');
        const card = roundedCard(Gtk, Gtk.Orientation.HORIZONTAL, 12);
        const header = applicationHeader(Gtk, app, this.appLabel, null, this.identity); header.hexpand = true; card.append(header);
        this.state.valign = Gtk.Align.CENTER; this.state.add_css_class('setup-state'); card.append(this.state); main.append(card);
        this.change = linkButton(Gtk, 'Change installation…'); main.append(this.change);
        this.change.connect('clicked', () => this.push('selection', this.change));
        this.status = new Gtk.Label({label: 'Choose an installation to check its configuration.', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, selectable: true});
        this.status.add_css_class('setup-notice');
        this.notice = roundedCard(Gtk); this.notice.add_css_class('setup-status');
        const noticeRow = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 16});
        this.noticeIcon = new Gtk.Image({icon_name: 'dialog-warning-symbolic', pixel_size: 32, valign: Gtk.Align.START}); noticeRow.append(this.noticeIcon);
        const noticeText = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 6, hexpand: true});
        this.noticeTitle = new Gtk.Label({label: 'Choose an installation', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0}); this.noticeTitle.add_css_class('title-2'); noticeText.append(this.noticeTitle); noticeText.append(this.status); noticeRow.append(noticeText); this.notice.append(noticeRow);
        this.viewDetails = this.navigation('View details', 'configuration'); this.notice.append(this.viewDetails); this.viewDetails.visible = false;
        main.append(this.notice);
        main.append(this.navigation('Advanced settings', 'advanced'));
        const guidance = new Gtk.Label({label: app === 'comfyui' ? 'Models are chosen in ComfyUI.' : 'Existing models are chosen in the next step.', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0}); guidance.add_css_class('dim-label'); main.append(guidance);
        this.selection = new Adw.PreferencesGroup(); this.pages.get('selection').append(this.selection);
        this.general = new Adw.PreferencesGroup({title: 'General'}); this.pages.get('advanced').append(this.general);
        const optional = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 12});
        optional.append(this.navigation('Resource checks', 'resources')); optional.append(this.navigation('Launch details', 'launch'));
        this.pages.get('advanced').append(optional);
        this.ownership = new Gtk.Label({label: 'External service files are preserved.', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0}); this.ownership.add_css_class('dim-label'); this.pages.get('advanced').append(this.ownership);
        this.resources = new Adw.PreferencesGroup({description: 'Capacity has not been measured. An empty requirement does not prove that a model fits.'}); this.pages.get('resources').append(this.resources);
        this.launch = new Adw.PreferencesGroup(); this.pages.get('launch').append(this.launch);
        this.diagnosticIdentity = new Gtk.Label({wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, selectable: true}); this.pages.get('configuration').append(this.diagnosticIdentity);
        this.diagnosticSummary = new Gtk.Label({label: 'Configuration needs attention', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0}); this.diagnosticSummary.add_css_class('title-2'); this.pages.get('configuration').append(this.diagnosticSummary);
        this.diagnostic = new Gtk.Label({wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, selectable: true, hexpand: true}); this.diagnostic.add_css_class('monospace');
        const detailCard = roundedCard(Gtk); detailCard.append(this.diagnostic); this.pages.get('configuration').append(detailCard);
        const copy = new Gtk.Button({label: 'Copy details', halign: Gtk.Align.END}); this.pages.get('configuration').append(copy);
        copy.connect('clicked', () => {
            const value = new GObject.Value(); value.init(GObject.TYPE_STRING);
            value.set_string([this.diagnosticIdentity.label, this.diagnosticSummary.label, this.diagnostic.label].filter(Boolean).join('\n\n'));
            window.get_display().get_clipboard().set_content(Gdk.ContentProvider.new_for_value(value));
        });
        this.pages.get('configuration').append(new Gtk.Label({label: 'Your service has not been changed.', wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0}));
    }
    navigation(label, target) {
        const button = new this.Gtk.Button({hexpand: true});
        const row = new this.Gtk.Box({orientation: this.Gtk.Orientation.HORIZONTAL, spacing: 12});
        row.append(new this.Gtk.Label({label, wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, hexpand: true}));
        row.append(new this.Gtk.Image({icon_name: 'go-next-symbolic'})); button.set_child(row);
        button.update_property([this.Gtk.AccessibleProperty.LABEL], [label]);
        button.connect('clicked', () => this.push(target, button)); return button;
    }
    push(page, origin) { this.history.push({page: this.page, origin}); this.show(page); }
    back() {
        const saved = this.history.pop();
        if (!saved) return false;
        this.show(saved.page); saved.origin?.grab_focus(); return true;
    }
    show(page = 'main') {
        this.page = page;
        for (const [id, widget] of this.pages) widget.visible = id === page;
        this.notify();
    }
    notify() {
        const titles = {main: this.appLabel, selection: 'Change installation', advanced: 'Advanced settings', resources: 'Resource checks', launch: 'Launch details', configuration: 'Configuration details'};
        this.navigate?.({title: titles[this.page], page: this.page, ready: this.ready && !this.busy, busy: this.busy});
    }
    candidate(candidate) {
        this.identity.label = candidateIdentity(candidate);
        this.state.label = candidate.instanceStatus === 'not-running' && candidate.unit ? 'Stopped' : 'Unverified';
        if (candidate.instanceStatus === 'available') this.state.label = 'Reachable';
        this.ready = Boolean(candidate.recognized && ['ready', 'model-required'].includes(candidate.configurationStatus));
        this.noticeTitle.label = this.ready ? 'Ready for setup' : 'Configuration needs attention';
        this.noticeIcon.icon_name = this.ready ? 'emblem-ok-symbolic' : 'dialog-warning-symbolic';
        this.noticeIcon.remove_css_class(this.ready ? 'warning' : 'success'); this.noticeIcon.add_css_class(this.ready ? 'success' : 'warning');
        if (this.ready) this.notice.remove_css_class('setup-warning'); else this.notice.add_css_class('setup-warning');
        this.status.label = candidateMessage(candidate);
        if (/HOME.*(unsupported|expansion|whitespace)/i.test(candidate.nextStep ?? '')) this.status.label = 'HOME uses an unsupported format.';
        if (this.ready) this.status.label = 'GPU operation has not been tested.';
        this.diagnosticIdentity.label = candidateChoice(candidate);
        this.diagnosticSummary.label = this.ready ? 'Configuration details' : 'Configuration needs attention';
        let evidence = candidate.evidence ?? [];
        if (!Array.isArray(evidence)) evidence = [String(evidence)];
        this.diagnostic.label = [candidateMessage(candidate), ...evidence, candidate.nextStep].filter(Boolean).join('\n');
        this.viewDetails.visible = !this.ready;
        this.ownership.label = candidate.binding?.owned ? 'Supervisor manages this launch. Applications and model files are preserved.' : 'External service files are preserved.';
        this.notify();
    }
    problem(summary, error) {
        this.ready = false; this.noticeTitle.label = 'Configuration needs attention'; this.noticeIcon.icon_name = 'dialog-warning-symbolic';
        this.noticeIcon.remove_css_class('success'); this.noticeIcon.add_css_class('warning');
        this.notice.add_css_class('setup-warning'); this.status.label = summary; this.viewDetails.visible = true;
        this.diagnosticSummary.label = 'Configuration needs attention'; this.diagnostic.label = error.message;
        this.notify();
    }
    checking(busy) { this.busy = busy; if (busy) this.diagnosticSummary.label = 'Checking configuration…'; this.notify(); }
    invalidate() { this.problem('Configuration changed. Check again before using this installation.', new Error('The installation settings have changed and require validation.')); }
}

export function installationChoiceLabel(Gtk, identity) {
    return new Gtk.Label({label: identity, wrap: true, wrap_mode: Pango.WrapMode.WORD_CHAR, xalign: 0, hexpand: true});
}
