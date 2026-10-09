// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
import {installSetupStyle, applicationHeader, roundedCard, linkButton} from './presentation.mjs';
import {applications} from './onboarding.mjs';
import {addErrorReporter} from './discovery-ui.mjs';

export function createShell(ui) {
    ui.window = new Adw.ApplicationWindow({application: ui.app, title: 'GPU Workload Setup',
        default_width: 620, default_height: 670});
    const removeStyle = installSetupStyle(ui.window);
    ui.appearance = Adw.StyleManager.get_default();
    ui.updateAppearance = () => {
        for (const [name, enabled] of [['setup-dark', ui.appearance.dark], ['setup-high-contrast', ui.appearance.high_contrast]]) {
            if (enabled) ui.window.add_css_class(name); else ui.window.remove_css_class(name);
        }
    };
    const appearanceSignals = [ui.appearance.connect('notify::dark', ui.updateAppearance),
        ui.appearance.connect('notify::high-contrast', ui.updateAppearance)];
    let active = true;
    ui.releasePresentation = () => {
        if (!active) return;
        for (const id of appearanceSignals) ui.appearance.disconnect(id);
        removeStyle();
        active = false;
    };
    ui.window.connect('destroy', ui.releasePresentation);
    ui.updateAppearance();
    ui.toolbar = new Adw.ToolbarView();
    ui.headerBar = new Adw.HeaderBar();
    ui.toolbar.add_top_bar(ui.headerBar);
    ui.box = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 14,
        margin_start: 28, margin_end: 28, margin_top: 20, margin_bottom: 12});
    ui.scroll = new Gtk.ScrolledWindow({vexpand: true, hscrollbar_policy: Gtk.PolicyType.NEVER});
    ui.scroll.set_child(ui.box);
    ui.toolbar.set_content(ui.scroll);
    ui.window.set_content(ui.toolbar);
    ui.heading = new Gtk.Label({label: 'Choose your applications', xalign: 0, wrap: true, focusable: true, selectable: true,
        accessible_role: Gtk.AccessibleRole.HEADING});
    ui.heading.add_css_class('title-1');
    ui.heading.add_css_class('setup-heading');
    ui.box.append(ui.heading);
    ui.introduction = new Gtk.Label({label: 'Use applications already installed on this computer.', wrap: true, xalign: 0});
    ui.introduction.add_css_class('setup-introduction');
    ui.box.append(ui.introduction);
    ui.status = new Gtk.Label({label: 'Checking your desktop and available services…', wrap: true, xalign: 0, selectable: true});
    ui.status.add_css_class('setup-notice');
    ui.status.visible = false;
    ui.status.connect('notify::label', () => { ui.status.visible = true; });
    ui.reportError = addErrorReporter({Adw, Gtk, parent: ui.box, status: ui.status});
    ui.applicationPage = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 12});
    ui.box.append(ui.applicationPage);
    ui.modelPage = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18, visible: false});
    ui.box.append(ui.modelPage);
    ui.finishPage = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18, visible: false});
    ui.box.append(ui.finishPage);
    ui.summary = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 10});
    ui.finishPage.append(ui.summary);
    ui.explanations = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 10, margin_top: 12});
    ui.explanations.append(new Gtk.Separator({orientation: Gtk.Orientation.HORIZONTAL, margin_bottom: 8}));
    for (const [icon, text] of [['media-playback-start-symbolic', 'Only one workload runs at a time.'], ['media-playback-stop-symbolic', 'ComfyUI closes when switching away.'], ['security-high-symbolic', 'Your applications and models stay unchanged.']]) {
        const row = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 16});
        row.append(new Gtk.Image({icon_name: icon, pixel_size: 28}));
        const label = new Gtk.Label({label: text, wrap: true, xalign: 0}); label.add_css_class('setup-explanation'); row.append(label); ui.explanations.append(row);
    }
    ui.finishPage.append(ui.explanations);
    ui.summaryCards = [];
    ui.box.append(ui.status);
}

export function createSettings(ui) {
    ui.rows = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18});
    ui.settings = new Adw.PreferencesGroup();
    ui.applicationPage.append(ui.settings);
    ui.advanced = new Adw.ExpanderRow({title: 'Advanced settings', subtitle: 'State database and NVIDIA GPU'});
    ui.settings.add(ui.advanced);
    ui.advanced.add_row(ui.rows);
    ui.setupSettings = new Gtk.Button({icon_name: 'emblem-system-symbolic', tooltip_text: 'Setup settings'});
    ui.setupSettings.update_property([Gtk.AccessibleProperty.LABEL], ['Setup settings']);
    ui.setupSettings.connect('clicked', () => { ui.settings.visible = true; ui.advanced.expanded = true; });
    ui.headerBar.pack_end(ui.setupSettings);
    ui.changes = new Gtk.Label({label: 'Finish setup to check the current changes.', wrap: true, xalign: 0, selectable: true});
    ui.advanced.add_row(ui.changes);
    ui.review = new Gtk.Button({label: 'Continue', sensitive: false});
    ui.review.add_css_class('suggested-action');
    ui.apply = new Gtk.Button({label: 'Finish setup', sensitive: false, visible: false});
    ui.apply.add_css_class('suggested-action');
    ui.add = new Gtk.Button({label: 'Add existing service (Advanced)', sensitive: false});
    ui.add.connect('clicked', () => ui.addProfile({adapter: 'systemd', bootPolicy: 'stop-to-idle'}));
    ui.advanced.add_row(ui.add);
    ui.draftRows = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 18});
    ui.applicationPage.append(ui.draftRows);
    ui.applicationGroup = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 12});
    ui.applicationPage.append(ui.applicationGroup);
    ui.applicationPage.remove(ui.settings);
    ui.applicationPage.append(ui.settings);
    ui.applicationPage.remove(ui.draftRows);
    ui.applicationPage.append(ui.draftRows);
    ui.saveDrafts = new Gtk.Button({label: 'Save selections for later', sensitive: false});
    ui.advanced.add_row(ui.saveDrafts);
}

export function createApplicationCards(ui) {
    for (const choice of ['comfyui', 'llama.cpp', 'ollama', 'vllm'].map(id => applications.find(choice => choice.id === id))) {
        const card = roundedCard(Gtk, Gtk.Orientation.HORIZONTAL, 16);
        const detection = new Gtk.Label({label: 'Checking installation…', wrap: true, xalign: 0}); detection.add_css_class('dim-label'); detection.add_css_class('setup-detection');
        const select = new Gtk.CheckButton({active: false, sensitive: false, valign: Gtk.Align.CENTER});
        select.update_property([Gtk.AccessibleProperty.LABEL], [`Use ${choice.label}`]); card.append(select);
        const gear = new Gtk.Button({icon_name: 'emblem-system-symbolic', tooltip_text: `Settings for ${choice.label}`, sensitive: false});
        const header = applicationHeader(Gtk, choice.id, choice.label, gear, detection); header.hexpand = true; card.append(header);
        gear.update_property([Gtk.AccessibleProperty.LABEL], [`Settings for ${choice.label}`]);
        gear.connect('clicked', () => {
            if (!select.active) select.active = true;
            const editor = ui.draftEditors.find(editor => editor.app === choice.id && ui.drafts.some(draft => draft.id === editor.id));
            ui.settings.visible = true; ui.advanced.expanded = true;
            if (editor) editor.showSettings();
            else {
                for (const existing of ui.profiles.filter(profile => ui.runtimeOf(profile) === choice.id)) ui.profileRows.get(existing).visible = !ui.profileRows.get(existing).visible;
            }
        });
        select.connect('toggled', () => {
            if (select.active) ui.selectApplication(choice);
            else {
                const appDrafts = ui.drafts.filter(draft => draft.app === choice.id);
                ui.suspendedApplications.set(choice.id, {
                    profiles: ui.profiles.filter(profile => ui.runtimeOf(profile) === choice.id).map(profile => ({profile, row: ui.profileRows.get(profile)})),
                    drafts: appDrafts,
                    editors: ui.draftEditors.filter(editor => appDrafts.some(draft => draft.id === editor.id)),
                });
                for (const editor of ui.draftEditors.filter(editor => editor.app === choice.id)) { editor.cancel().catch(error => ui.reportError('Temporary cleanup needs attention. Keep external controls paused and retry.', error)); editor.group.visible = false; editor.modelGroup.visible = false; }
                ui.drafts = ui.drafts.filter(draft => draft.app !== choice.id);
                for (const profile of [...ui.profiles].filter(profile => ui.runtimeOf(profile) === choice.id)) {
                    ui.profiles.splice(ui.profiles.indexOf(profile), 1); ui.rows.remove(ui.profileRows.get(profile)); ui.profileRows.delete(profile);
                }
                ui.draftGeneration++; ui.saveDrafts.sensitive = !ui.pending;
            }
            ui.invalidate();
        });
        ui.applicationGroup.append(card); ui.applicationCards.set(choice.id, {select, gear, detection});
    }
    ui.findApplication = linkButton(Gtk, 'Cannot find your application?');
    ui.findApplication.connect('clicked', () => { ui.status.label = 'Open an application’s settings (gear) to choose its existing location. Applications must already be installed.'; ui.status.visible = true; });
    ui.applicationPage.remove(ui.settings);
    ui.applicationPage.remove(ui.draftRows);
    ui.applicationPage.append(ui.findApplication);
    ui.applicationPage.append(ui.settings);
    ui.applicationPage.append(ui.draftRows);
    ui.settings.visible = false;
}

export function createFooter(ui) {
    ui.saveDrafts.connect('clicked', ui.saveDraftSelections);
    ui.later = new Gtk.Button({label: 'Set up later'});
    ui.later.connect('clicked', ui.postponeSetup);
    ui.footer = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 8,
        margin_start: 24, margin_end: 24, margin_top: 12, margin_bottom: 12});
    ui.footer.add_css_class('setup-footer');
    ui.actions = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 8});
    ui.back = new Gtk.Button({label: 'Back', visible: false});
    ui.actions.append(ui.back);
    ui.actions.append(ui.later);
    ui.actions.append(new Gtk.Box({hexpand: true}));
    ui.actions.append(ui.review);
    ui.actions.append(ui.apply);
    ui.footer.append(ui.actions);
    ui.footer.add_css_class('toolbar');
    ui.toolbar.add_bottom_bar(ui.footer);
    ui.back.connect('clicked', ui.goBack);
}
