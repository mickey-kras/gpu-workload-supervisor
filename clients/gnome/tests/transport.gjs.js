// Native Gio/process integration. Does not qualify Shell rendering or GPU effects.
import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import { readBounded, Transport } from '../gpu-workload-supervisor@local/transport.js';

function assert(condition, message) {
    if (!condition) throw new Error(message);
}
async function rejects(operation, check, label) {
    try {
        await operation();
    } catch (error) {
        assert(check(error), `${label}: unexpected error ${error}`);
        return;
    }
    throw new Error(`${label}: unexpectedly succeeded`);
}
const message = (expected) => (error) => error.message === expected;
const cancelled = (error) => error.matches(Gio.io_error_quark(), Gio.IOErrorEnum.CANCELLED);
const pause = (ms) => new Promise((resolve) => GLib.timeout_add(GLib.PRIORITY_DEFAULT, ms, () => {
    resolve();
    return GLib.SOURCE_REMOVE;
}));
function stream(bytes) {
    return Gio.MemoryInputStream.new_from_bytes(new GLib.Bytes(bytes));
}
const encode = (value) => new TextEncoder().encode(value);
const fixtureDir = GLib.getenv('GPU_NATIVE_FIXTURE_DIR');
function exists(name) {
    return GLib.file_test(`${fixtureDir}/${name}`, GLib.FileTest.EXISTS);
}
async function waitFile(name, budget = 5000) {
    const end = GLib.get_monotonic_time() + budget * 1000;
    while (!exists(name)) {
        assert(GLib.get_monotonic_time() < end, `Timed out waiting for ${name}`);
        await pause(10);
    }
}
function request(id, extra = {}) {
    return { protocolVersion: 1, requestId: id, action: 'status', ...extra };
}
async function memoryChecks() {
    assert(await readBounded(stream(encode('{"ok":true}\n')), null) === '{"ok":true}\n', 'Stream bytes changed');
    const boundary = 'x'.repeat(65536);
    assert(await readBounded(stream(encode(boundary)), null) === boundary, 'Exact byte limit rejected');
    await rejects(() => readBounded(stream(new Uint8Array(65537)), null), message('Response too large'), 'Oversized stream');
    await rejects(() => readBounded(stream(new Uint8Array([0xff])), null), (error) => error instanceof TypeError, 'Invalid UTF-8');
    const cancel = new Gio.Cancellable();
    cancel.cancel();
    await rejects(() => readBounded(stream(encode('bytes')), cancel), cancelled, 'Cancelled read');
    print('PASS native stream boundaries, UTF-8 and cancellation');
}
async function protocolChecks() {
    const transport = new Transport();
    for (const id of ['normal', 'exact-response']) {
        const response = await transport.call(request(id));
        assert(response.code === 'busy' && response.requestId === id, `Incorrect response for ${id}`);
    }
    const boundary = request('exact-request', { padding: '' });
    boundary.padding = 'x'.repeat(16384 - encode(`${JSON.stringify(boundary)}\n`).length);
    await transport.call(boundary);
    const [ok, raw] = GLib.file_get_contents(`${fixtureDir}/exact-request.request`);
    assert(ok && raw.length === 16384 && raw[raw.length - 1] === 10, 'Request byte boundary or framing changed');
    const overflow = { ...boundary, requestId: 'large-request', padding: boundary.padding + 'x' };
    await rejects(() => transport.call(overflow), message('Request too large'), 'Oversized request');
    assert(!exists('large-request.request'), 'Oversized request reached subprocess');
    await rejects(() => transport.call(request('oversized-response')), message('Response too large'), 'Oversized process output');
    await rejects(() => transport.call(request('wrong-id')), message('Invalid operator response'), 'Mismatched request ID');
    for (const id of ['malformed', 'empty']) {
        await rejects(() => transport.call(request(id)), (error) => error instanceof SyntaxError, id);
    }
    assert((await transport.call(request('after-errors'))).code === 'busy', 'Transport did not recover after error');
    print('PASS real subprocess framing, typed response, bounds and failure recovery');
}
async function detachCheck(id) {
    const transport = new Transport();
    // Attach rejection handler immediately: cancellation must never go unhandled.
    const outcome = rejects(() => transport.call(request(id)), cancelled, id);
    await waitFile(`${id}.admitted`);
    await rejects(() => transport.call(request('duplicate')), message('Call pending'), 'Concurrent call');
    assert(!exists('duplicate.request'), 'Duplicate request reached subprocess');
    transport.detach();
    await outcome;
    assert(!exists(`${id}.completed`), 'Fixture completed before cancellation');
    GLib.file_set_contents(`${fixtureDir}/${id}.release`, 'continue');
    await waitFile(`${id}.completed`);
    assert((await transport.call(request(`after-${id}`))).code === 'busy', 'Transport unusable after detach');
}
async function deadlineCheck() {
    const transport = new Transport();
    const start = GLib.get_monotonic_time();
    await rejects(() => transport.call(request('deadline')), cancelled, 'Status deadline');
    const elapsed = (GLib.get_monotonic_time() - start) / 1000000;
    assert(elapsed >= 74 && elapsed < 90, `Unexpected status deadline: ${elapsed}s`);
    assert(!exists('deadline.completed'), 'Fixture completed before deadline');
    GLib.file_set_contents(`${fixtureDir}/deadline.release`, 'continue');
    await waitFile('deadline.completed', 10000);
    assert((await transport.call(request('after-deadline'))).code === 'busy', 'Transport unusable after deadline');
    print('PASS real status deadline cancels client I/O while backend continues');
}
try {
    await memoryChecks();
    if (fixtureDir) {
        await protocolChecks();
        await detachCheck('detached');
        await detachCheck('waiting-exit');
        print('PASS detach before/after peer stdout closure, pending guard and backend survival');
        await deadlineCheck();
    } else {
        print('Subprocess checks require scripts/native-desktop-integration.sh');
    }
} catch (error) {
    printerr(error.stack ?? error);
    imports.system.exit(1);
}
