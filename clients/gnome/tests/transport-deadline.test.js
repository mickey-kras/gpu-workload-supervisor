import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

// Drive the actual transport deadline using a virtual GLib clock. Gio's pending
// write isolates timeout behavior without pretending to exercise native IPC.
function pendingTransport() {
    let now = 0;
    let nextId = 1;
    let cancellations = 0;
    let processes = 0;
    const timers = new Map();
    const Gio = {
        Cancellable: class {
            cancel() {
                cancellations++;
            }
        },
        SubprocessFlags: { STDIN_PIPE: 1, STDOUT_PIPE: 2, STDERR_SILENCE: 4 },
        Subprocess: {
            new() {
                processes++;
                return {
                    get_stdin_pipe: () => ({ write_all_async() {} }),
                    get_stdout_pipe: () => ({}),
                };
            },
        },
    };
    const GLib = {
        PRIORITY_DEFAULT: 0,
        SOURCE_REMOVE: false,
        timeout_add(priority, delay, callback) {
            const id = nextId++;
            timers.set(id, { at: now + delay, callback });
            return id;
        },
        source_remove(id) {
            timers.delete(id);
        },
    };
    const source = readFileSync(
        new URL(
            '../gpu-workload-supervisor@local/transport.js',
            import.meta.url,
        ),
        'utf8',
    )
        .replaceAll(/^import .*;\n/gm, '')
        .replaceAll('export ', '');
    const context = vm.createContext({
        Gio,
        GLib,
        TextEncoder,
        SETTINGS_ACTIONS: ['get-settings', 'set-idle-policy'],
    });
    vm.runInContext(
        `${source}\n globalThis.transport = new Transport();`,
        context,
    );
    return {
        transport: context.transport,
        cancellations: () => cancellations,
        processes: () => processes,
        advance(ms) {
            now += ms;
            for (const [id, timer] of timers) {
                if (timer.at <= now) {
                    timers.delete(id);
                    timer.callback();
                }
            }
        },
    };
}

for (const [action, backendBound, clientBudget] of [
    ['status', 60000 + 10000 + 2000 + 2000, 75000],
    ['get-settings', 60000 + 10000 + 2000 + 2000, 75000],
    ['set-idle-policy', 60000 + 10000 + 2000 + 2000, 75000],
    ['take-control', 1800000 + 120000 + 10000 + 2000 + 2000, 1980000],
    ['user-switch', 1800000 + 120000 + 10000 + 2000 + 2000, 1980000],
    ['return-control', 1800000 + 120000 + 10000 + 2000 + 2000, 1980000],
]) {
    test(`${action} read deadline allows backend finalization then cancels once without replay`, () => {
        const clock = pendingTransport();
        clock.transport.call({
            protocolVersion: 1,
            requestId: 'deadline',
            action,
        });
        clock.advance(backendBound);
        assert.equal(clock.cancellations(), 0);
        clock.advance(clientBudget - backendBound - 1);
        assert.equal(clock.cancellations(), 0);
        clock.advance(1);
        assert.equal(clock.cancellations(), 1);
        clock.advance(clientBudget);
        assert.equal(clock.cancellations(), 1);
        assert.equal(clock.processes(), 1);
    });
}
