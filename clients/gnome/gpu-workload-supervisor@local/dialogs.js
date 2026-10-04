// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import Clutter from 'gi://Clutter';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import St from 'gi://St';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import * as ModalDialog from 'resource:///org/gnome/shell/ui/modalDialog.js';
export function ownershipDialog(action, label, confirm, onClose) {
    const dialog = new ModalDialog.ModalDialog();
    const taking = action === 'take-control';
    dialog.contentLayout.add_child(
        new St.Label({
            text: taking ? 'Take GPU control?' : 'Return GPU control?',
            style_class: 'dialog-title',
        }),
    );
    const description = new St.Label({
        text: taking
            ? `Keep ${label} running and take manual control of the GPU.`
            : 'Stop current GPU work and return control to the supervisor in Idle.',
        style_class: 'dialog-description',
    });
    description.clutter_text.line_wrap = true;
    dialog.contentLayout.add_child(description);
    dialog.setButtons([
        {
            label: 'Cancel',
            key: Clutter.KEY_Escape,
            action: () => dialog.close(),
        },
        {
            label: taking ? 'Take Control' : 'Stop and Return',
            isDefault: true,
            action: () => {
                dialog.close();
                confirm();
            },
        },
    ]);
    dialog.connect('closed', () => {
        onClose();
        dialog.destroy();
    });
    if (!dialog.open()) {
        dialog.destroy();
        return null;
    }
    return dialog;
}
