import test from 'node:test';
import assert from 'node:assert/strict';
import {addDraftEditor} from './discovery-ui.mjs';

class Widget {
    constructor(properties = {}) { Object.assign(this, properties); this.handlers = {}; this.children = []; }
    connect(signal, handler) { this.handlers[signal] = handler; }
    emit(signal) { return this.handlers[signal]?.(); }
    add(child) { this.children.push(child); }
    add_row(child) { this.add(child); }
    append(child) { this.add(child); }
    remove(child) { this.children.splice(this.children.indexOf(child), 1); }
}
const Adw = {PreferencesGroup: Widget, EntryRow: Widget, ComboRow: Widget, ExpanderRow: Widget};
const Gtk = {Label: Widget, Button: Widget, StringList: {new: entries => entries}};

for (const candidate of [
    {app: 'ollama', instanceStatus: 'not-running', inventoryStatus: 'not-checked', models: []},
    {app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'other'}]},
]) {
    test(`refresh retains saved model for ${candidate.instanceStatus} inventory`, async () => {
        const parent = new Widget();
        const changes = [];
        addDraftEditor({Adw, Gtk, parent, initial: {id: 'draft-one', app: 'ollama', label: 'Qwen', model: 'qwen:latest'},
            detected: [], command: async () => JSON.stringify(candidate), changed: value => changes.push(value),
            removed: () => {}, bind: async () => {}});
        const group = parent.children[0];
        const refresh = group.children.find(child => child.label === 'Refresh discovery');
        await refresh.emit('clicked');
        assert.equal(changes.length, 0);
        const model = group.children.find(child => child.title === 'Model');
        assert.equal(model.selected, 0);
        assert.match(model.model[0], /qwen:latest/);
        if (candidate.models.length) {
            model.selected = 1;
            model.emit('notify::selected');
            assert.equal(changes.at(-1).model, 'other');
        }
    });
}

test('refresh keeps the selected model when inventory order changes', async () => {
    const parent = new Widget();
    const changes = [];
    addDraftEditor({Adw, Gtk, parent, initial: {id: 'draft-one', app: 'ollama', label: 'Qwen', model: 'qwen:latest'},
        detected: [], command: async () => JSON.stringify({app: 'ollama', instanceStatus: 'available', inventoryStatus: 'available',
            models: [{id: 'other'}, {id: 'qwen:latest'}]}), changed: value => changes.push(value),
        removed: () => {}, bind: async () => {}});
    const group = parent.children[0];
    await group.children.find(child => child.label === 'Refresh discovery').emit('clicked');
    const model = group.children.find(child => child.title === 'Model');
    assert.equal(model.selected, 2);
    assert.equal(model.model[model.selected], 'qwen:latest');
    assert.equal(changes.length, 0);
});
