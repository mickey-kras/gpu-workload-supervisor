import Gio from 'gi://Gio';
import GLib from 'gi://GLib';
import { ResponseBuffer } from './framing.js';
import { parseResponse } from './contract.js';
export async function readBounded(stream, cancellable) {
    const result = new ResponseBuffer();
    for (;;) {
        const bytes = await new Promise((resolve, reject) =>
            stream.read_bytes_async(
                4096,
                GLib.PRIORITY_DEFAULT,
                cancellable,
                (s, r) => {
                    try {
                        resolve(s.read_bytes_finish(r).toArray());
                    } catch (e) {
                        reject(e);
                    }
                },
            ),
        );
        if (!bytes.length) return result.finish();
        result.append(bytes);
    }
}
export class Transport {
    cancel = null;
    detach() {
        this.cancel?.cancel();
        this.cancel = null;
    }
    async call(request) {
        if (this.cancel) throw new Error('Call pending');
        const bytes = new TextEncoder().encode(`${JSON.stringify(request)}\n`);
        if (bytes.length > 16384) throw new Error('Request too large');
        const cancel = new Gio.Cancellable();
        // Never send a signal or force_exit: the operator owns admitted effects.
        const process = Gio.Subprocess.new(
            ['/usr/bin/gpu-operator'],
            Gio.SubprocessFlags.STDIN_PIPE |
                Gio.SubprocessFlags.STDOUT_PIPE |
                Gio.SubprocessFlags.STDERR_SILENCE,
        );
        this.cancel = cancel;
        const input = process.get_stdin_pipe(),
            output = process.get_stdout_pipe();
        // Status: 60s operation + 10s fault latch + 2s each for input/output.
        // Mutation: 1800s operation + 120s cleanup + 10s finalization +
        // 2s each for input/output. Round above these 74s / 1934s bounds.
        // Expiration cancels client I/O only; admitted effects remain backend-owned.
        const budget = request.action === 'status' ? 75000 : 1980000;
        let timer = GLib.timeout_add(GLib.PRIORITY_DEFAULT, budget, () => {
            timer = 0;
            cancel.cancel();
            return GLib.SOURCE_REMOVE;
        });
        try {
            await new Promise((resolve, reject) =>
                input.write_all_async(
                    bytes,
                    GLib.PRIORITY_DEFAULT,
                    cancel,
                    (s, r) => {
                        try {
                            s.write_all_finish(r);
                            resolve();
                        } catch (e) {
                            reject(e);
                        }
                    },
                ),
            );
            await new Promise((resolve, reject) =>
                input.close_async(GLib.PRIORITY_DEFAULT, cancel, (s, r) => {
                    try {
                        s.close_finish(r);
                        resolve();
                    } catch (e) {
                        reject(e);
                    }
                }),
            );
            const text = await readBounded(output, cancel);
            await new Promise((resolve, reject) =>
                process.wait_async(cancel, (p, r) => {
                    try {
                        p.wait_finish(r);
                        resolve();
                    } catch (e) {
                        reject(e);
                    }
                }),
            );
            return parseResponse(text, request.requestId);
        } finally {
            if (timer) GLib.source_remove(timer);
            // Closing our descriptors does not cancel or kill the backend.
            input.close_async(GLib.PRIORITY_DEFAULT, null, () => {});
            output.close_async(GLib.PRIORITY_DEFAULT, null, () => {});
            if (this.cancel === cancel) this.cancel = null;
        }
    }
}
