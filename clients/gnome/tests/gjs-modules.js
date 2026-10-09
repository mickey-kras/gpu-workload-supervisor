import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

// Execute the unchanged production modules with their real URLs for coverage.
// Only native GNOME imports are substituted; this does not test native IPC/UI.
export async function loadGjsModule(name, native, globals = {}) {
    const context = vm.createContext({ TextEncoder, TextDecoder, ...globals });
    const cache = new Map();
    async function load(url) {
        if (cache.has(url)) return cache.get(url);
        let module;
        if (Object.hasOwn(native, url)) {
            const exports = native[url];
            module = new vm.SyntheticModule(Object.keys(exports), function () {
                for (const [key, value] of Object.entries(exports))
                    this.setExport(key, value);
            }, { context, identifier: url });
        } else {
            module = new vm.SourceTextModule(await readFile(new URL(url), 'utf8'), {
                context, identifier: url, initializeImportMeta: meta => { meta.url = url; },
            });
        }
        cache.set(url, module);
        await module.link((specifier, parent) => load(
            specifier.startsWith('.') ? new URL(specifier, parent.identifier).href : specifier,
        ));
        return module;
    }
    const module = await load(new URL(`../gpu-workload-supervisor@local/${name}`, import.meta.url).href);
    await module.evaluate();
    return module.namespace;
}
