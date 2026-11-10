// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
import {HostProtection} from './host-protection.mjs';

// createHostProtection adds the Host protection page to the setup window
// and its entry under Setup settings. The page edits values of the setup
// request; any edit asks for a new review.
export function createHostProtection(ui) {
    ui.hostProtection = new HostProtection({Adw, Gtk, window: ui.window, changed: () => ui.invalidate()});
    ui.hostProtection.onUsage(usage => { for (const editor of ui.draftEditors) editor.setHostUsage(usage); });
    ui.box.append(ui.hostProtection.page);
    ui.hostPage = null;
    const open = new Gtk.Button({label: 'Host protection'});
    open.update_property([Gtk.AccessibleProperty.LABEL], ['Host protection']);
    open.connect('clicked', () => ui.openHostProtection(open));
    ui.advanced.add_row(open);
    ui.openHostProtection = origin => {
        ui.hostPage = {origin, step: ui.step};
        for (const widget of [ui.applicationPage, ui.modelPage, ui.finishPage, ui.heading, ui.introduction, ui.setupSettings, ui.back, ui.later, ui.apply, ui.status]) widget.visible = false;
        ui.hostProtection.page.visible = true;
        ui.heading.label = 'Host protection';
        ui.settingsBack.visible = true; ui.settingsTitle.label = 'Host protection'; ui.settingsTitle.visible = true;
        ui.headerBar.set_title_widget(ui.settingsTitle);
        ui.review.label = 'Done'; ui.review.visible = true; ui.review.sensitive = true;
        ui.settingsBack.grab_focus();
    };
    ui.closeHostProtection = () => {
        const saved = ui.hostPage;
        ui.hostPage = null;
        ui.hostProtection.page.visible = false; ui.heading.visible = true; ui.introduction.visible = true;
        ui.settingsBack.visible = false; ui.settingsTitle.visible = false; ui.headerBar.set_title_widget(null);
        ui.setStep(saved.step);
        saved.origin.grab_focus();
    };
}
