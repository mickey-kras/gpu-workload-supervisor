// Native Gio smoke test; does not qualify GNOME Shell rendering or survival.
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GNOME 50 GJS runtime supplies this native module, not npm.
import GLib from 'gi://GLib';
import { readBounded } from '../gpu-workload-supervisor@local/transport.js';
try {
    const input = Gio.MemoryInputStream.new_from_bytes(
        new GLib.Bytes(new TextEncoder().encode('{"ok":true}\n')),
    );
    if ((await readBounded(input, null)) !== '{"ok":true}\n')
        throw new Error('Incorrect stream bytes');
    const huge = Gio.MemoryInputStream.new_from_bytes(
        new GLib.Bytes(new Uint8Array(65537)),
    );
    let rejected = false;
    try {
        await readBounded(huge, null);
    } catch {
        rejected = true;
    }
    if (!rejected) throw new Error('Unbounded stream accepted');
    print('Native Gio bounded read checks passed');
} catch (e) {
    printerr(e);
    imports.system.exit(1);
}
