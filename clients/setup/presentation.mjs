// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gtk from 'gi://Gtk?version=4.0';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';

const icons = {'comfyui': 'comfyui.svg', 'llama.cpp': 'llama-cpp.svg', 'ollama': 'ollama.svg', 'vllm': 'vllm.png'};

// Surface and border colors follow Adwaita, including its high-contrast palette.
// Ubuntu's orange is local to the setup journey, not a system theme override.
export function installSetupStyle(window) {
    const provider = new Gtk.CssProvider();
    provider.load_from_data(`
        .setup-window button.suggested-action:not(:disabled), .setup-window checkbutton check:checked:not(:disabled) { background: #c64600; color: white; }
        .setup-window.setup-high-contrast button.suggested-action:not(:disabled), .setup-window.setup-high-contrast checkbutton check:checked:not(:disabled) { background: @accent_bg_color; color: @accent_fg_color; }
        .setup-window.setup-high-contrast .setup-link { color: @accent_color; }
        .setup-window.setup-dark:not(.setup-high-contrast) .setup-link { color: #ff854b; }
        .setup-heading { font-size: 28px; font-weight: 800; }
        .setup-introduction { font-size: 16px; }
        .setup-card { background: @card_bg_color; color: @card_fg_color; border: 1px solid alpha(@window_fg_color, .12); border-radius: 12px; padding: 12px 16px; }
        .setup-high-contrast .setup-card { border-color: @window_fg_color; }
        .setup-icon-tile { background: #17191b; border-radius: 10px; padding: 6px; min-width: 44px; min-height: 44px; }
        .setup-app-name { font-size: 18px; font-weight: 700; }
        .setup-detection { font-size: 14px; }
        .setup-model-choice { background: alpha(@window_fg_color, .04); border: 1px solid alpha(@window_fg_color, .08); border-radius: 9px; padding: 12px 16px; font-size: 16px; }
        .setup-high-contrast .setup-model-choice { border-color: @window_fg_color; }
        .setup-link { color: #c64600; text-decoration: underline; padding: 4px 0; }
        .setup-footer button { padding: 12px 24px; min-width: 72px; }
        .setup-notice { font-size: 14px; }
        .setup-explanation { font-size: 15px; }
    `, -1);
    Gtk.StyleContext.add_provider_for_display(window.get_display(), provider, Gtk.STYLE_PROVIDER_PRIORITY_APPLICATION);
    window.add_css_class('setup-window');
}

export function roundedCard(Gtk, orientation = Gtk.Orientation.VERTICAL, spacing = 10) {
    const card = new Gtk.Box({orientation, spacing});
    card.add_css_class('setup-card');
    return card;
}

export function applicationIcon(Gtk, appID) {
    const tile = new Gtk.Box({valign: Gtk.Align.CENTER});
    tile.add_css_class('setup-icon-tile');
    const path = Gio.File.new_for_uri(import.meta.url).get_parent().get_child(`icons/${icons[appID]}`).get_path();
    const image = new Gtk.Image({file: path, pixel_size: 44, accessible_role: Gtk.AccessibleRole.PRESENTATION});
    tile.append(image);
    return tile;
}

export function applicationHeader(Gtk, appID, name, gear, subtitle = null) {
    const header = new Gtk.Box({orientation: Gtk.Orientation.HORIZONTAL, spacing: 16});
    header.append(applicationIcon(Gtk, appID));
    const text = new Gtk.Box({orientation: Gtk.Orientation.VERTICAL, spacing: 4, hexpand: true, valign: Gtk.Align.CENTER});
    const label = new Gtk.Label({label: name, xalign: 0, wrap: true}); label.add_css_class('setup-app-name'); text.append(label);
    if (subtitle) text.append(subtitle);
    header.append(text);
    if (gear) { gear.valign = Gtk.Align.CENTER; gear.add_css_class('flat'); header.append(gear); }
    return header;
}

export function linkButton(Gtk, label) {
    const button = new Gtk.Button({label, halign: Gtk.Align.START});
    button.add_css_class('flat'); button.add_css_class('setup-link');
    return button;
}
