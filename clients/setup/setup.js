// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Adw from 'gi://Adw?version=1';
// aislop-ignore-next-line ai-slop/hallucinated-import -- GJS runtime supplies this native module, not npm.
import Gio from 'gi://Gio';
import {createSetupWindow} from './setup-window.mjs';

// The setup application is short-lived. Runtime controls use gpu-operator.
function command(argv, input = null, operation = null) {
    return new Promise((resolve, reject) => {
        const proc = Gio.Subprocess.new(argv, Gio.SubprocessFlags.STDIN_PIPE |
            Gio.SubprocessFlags.STDOUT_PIPE | Gio.SubprocessFlags.STDERR_PIPE);
        if (operation) operation.cancel = () => proc.send_signal(2); // SIGINT: helper restores its recorded stopped state.
        proc.communicate_utf8_async(input, null, (child, result) => {
            if (operation) operation.cancel = null;
            try {
                const [, stdout, stderr] = child.communicate_utf8_finish(result);
                if (!child.get_successful()) throw new Error(stderr.trim() || 'Command failed');
                resolve(stdout);
            } catch (error) { reject(error); }
        });
    });
}
const app = new Adw.Application({application_id: 'local.GPUWorkload.Setup'});
app.connect('activate', () => createSetupWindow({app, command}));
app.run([]);
