import test from 'node:test';
import assert from 'node:assert/strict';
import { loadGjsModule } from './gjs-modules.js';

test('native dialog boundary preserves cancellation, confirmation and open failure', async () => {
    let opens = true;
    const events = [];
    class Dialog {
        contentLayout = { add_child() {} };
        setButtons(buttons) { this.buttons = buttons; }
        connect(signal, callback) { this.closed = callback; }
        open() { return opens; }
        close() { this.closed(); }
        destroy() { events.push('destroy'); }
    }
    class Label { clutter_text = {}; }
    const { ownershipDialog } = await loadGjsModule('dialogs.js', {
        'gi://Clutter': { default: { KEY_Escape: 65307 } },
        'gi://St': { default: { Label } },
        'resource:///org/gnome/shell/ui/modalDialog.js': { ModalDialog: Dialog },
    });
    const create = action => ownershipDialog(action, 'Rendering',
        () => events.push('confirm'), () => events.push('closed'));
    create('take-control').buttons[0].action();
    assert.deepEqual(events.splice(0), ['closed', 'destroy']);
    create('return-control').buttons[1].action();
    assert.deepEqual(events.splice(0), ['closed', 'destroy', 'confirm']);
    opens = false;
    assert.equal(create('take-control'), null);
    assert.deepEqual(events, ['destroy']);
});

for (const failure of [null, 'write', 'close', 'read', 'wait', 'invalid']) {
    test(`transport settles and cleans descriptors: ${failure ?? 'success'}`, async () => {
        const timers = new Map();
        const closes = [];
        let cancellations = 0;
        let reads = 0;
        const finish = stage => { if (failure === stage) throw new Error(stage); };
        const input = {
            write_all_async(bytes, priority, cancel, callback) {
                assert.equal(new TextDecoder().decode(bytes), '{"requestId":"r1","action":"status"}\n');
                callback(this, {});
            },
            write_all_finish() { finish('write'); },
            close_async(priority, cancel, callback) { closes.push('input'); callback(this, {}); },
            close_finish() { finish('close'); },
        };
        const output = {
            read_bytes_async(size, priority, cancel, callback) { callback(this, {}); },
            read_bytes_finish() {
                finish('read');
                return { toArray: () => reads++ ? new Uint8Array() : new TextEncoder().encode(
                    failure === 'invalid' ? 'not json' : '{"protocolVersion":1,"requestId":"r1","code":"unavailable"}\n',
                ) };
            },
            close_async() { closes.push('output'); },
        };
        const process = {
            get_stdin_pipe: () => input, get_stdout_pipe: () => output,
            wait_async(cancel, callback) { callback(this, {}); },
            wait_finish() { finish('wait'); },
        };
        const { Transport } = await loadGjsModule('transport.js', {
            'gi://Gio': { default: {
                Cancellable: class { cancel() { cancellations++; } },
                SubprocessFlags: { STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_SILENCE: 4 },
                Subprocess: { new: () => process },
            } },
            'gi://GLib': { default: {
                PRIORITY_DEFAULT: 0, SOURCE_REMOVE: false,
                timeout_add(priority, delay, callback) { timers.set(1, callback); return 1; },
                source_remove(id) { timers.delete(id); },
            } },
        });
        const transport = new Transport();
        const result = transport.call({ requestId: 'r1', action: 'status' });
        await assert.rejects(transport.call({ action: 'status' }), /Call pending/);
        if (failure) await assert.rejects(result);
        else assert.equal((await result).code, 'unavailable');
        assert.equal(transport.cancel, null);
        assert.equal(timers.size, 0);
        assert.ok(closes.includes('input') && closes.includes('output'));
        transport.detach();
        assert.equal(cancellations, 0);
        await assert.rejects(transport.call({ payload: 'x'.repeat(16384) }), /Request too large/);
    });
}

test('transport deadline and detach cancel client I/O without replaying the backend', async () => {
    for (const action of ['status', 'get-settings', 'set-idle-policy', 'take-control']) {
        let timeout;
        let budget;
        let cancellations = 0;
        let processes = 0;
        const { Transport } = await loadGjsModule('transport.js', {
            'gi://Gio': { default: {
                Cancellable: class { cancel() { cancellations++; } },
                SubprocessFlags: { STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_SILENCE: 4 },
                Subprocess: { new() {
                    processes++;
                    return {
                        get_stdin_pipe: () => ({ write_all_async() {} }),
                        get_stdout_pipe: () => ({}),
                    };
                } },
            } },
            'gi://GLib': { default: {
                PRIORITY_DEFAULT: 0, SOURCE_REMOVE: false,
                timeout_add(priority, delay, callback) { budget = delay; timeout = callback; return 1; },
                source_remove() {},
            } },
        });
        const transport = new Transport();
        void transport.call({ action });
        assert.equal(
            budget,
            ['status', 'get-settings', 'set-idle-policy'].includes(action) ? 75000 : 1980000,
        );
        assert.equal(cancellations, 0);
        assert.equal(timeout(), false);
        assert.equal(cancellations, 1);
        transport.detach();
        assert.equal(cancellations, 2);
        assert.equal(transport.cancel, null);
        assert.equal(processes, 1);
    }
});
