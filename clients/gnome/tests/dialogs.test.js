import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// Exercise the actual dialog construction with only the unavailable Shell APIs
// replaced. This checks lifecycle wiring, not native focus or keyboard behavior.
function loadDialog(opens) {
    let instance;
    class Dialog {
        constructor() {
            instance = this;
            this.contentLayout = { add_child() {} };
            this.destroyed = false;
        }
        setButtons(buttons) {
            this.buttons = buttons;
        }
        connect(signal, callback) {
            this.onClosed = callback;
        }
        open() {
            return opens;
        }
        destroy() {
            this.destroyed = true;
        }
        close() {
            this.onClosed();
        }
    }
    class Label {
        constructor(properties) {
            Object.assign(this, properties);
            this.clutter_text = {};
        }
    }
    const source = readFileSync(
        new URL('../gpu-workload-supervisor@local/dialogs.js', import.meta.url),
        'utf8',
    )
        .replace(/^import .*;\n/gm, '')
        .replace('export function ownershipDialog', 'function ownershipDialog');
    const context = vm.createContext({
        Clutter: { KEY_Escape: 65307 },
        St: { Label },
        ModalDialog: { ModalDialog: Dialog },
    });
    vm.runInContext(source, context);
    return { create: context.ownershipDialog, instance: () => instance };
}

test('failed modal open destroys dialog and does not retain ownership prompt', () => {
    const dialog = loadDialog(false);
    let confirmed = false;
    const result = dialog.create(
        'take-control',
        'Rendering',
        () => {
            confirmed = true;
        },
        () => {},
    );
    assert.equal(result, null);
    assert.equal(dialog.instance().destroyed, true);
    assert.equal(confirmed, false);
});

test('ownership confirmation uses Shell isDefault and closes before committing intent', () => {
    const dialog = loadDialog(true);
    const events = [];
    const result = dialog.create(
        'return-control',
        'Rendering',
        () => events.push('confirm'),
        () => events.push('close'),
    );
    assert.equal(result, dialog.instance());
    const button = result.buttons[1];
    assert.equal(button.isDefault, true);
    assert.equal(Object.hasOwn(button, 'default'), false);
    button.action();
    assert.deepEqual(events, ['close', 'confirm']);
    assert.equal(result.destroyed, true);
});
