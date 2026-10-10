const assert = require('node:assert/strict');
const {mkdtempSync, readFileSync, writeFileSync, mkdirSync, rmSync} = require('node:fs');
const {tmpdir} = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const test = require('node:test');

const root = path.resolve(__dirname, '../..');
const files = [
  'clients/gnome/gpu-workload-supervisor@local/dialogs.js',
  'clients/gnome/gpu-workload-supervisor@local/extension.js',
  'clients/gnome/gpu-workload-supervisor@local/transport.js',
  'clients/gnome/tests/transport.gjs.js',
  'clients/setup/setup.js',
  'clients/setup/setup-window.mjs',
  'clients/setup/setup-layout.mjs',
  'clients/setup/settings-pages.mjs',
  'clients/setup/presentation.mjs',
  'tests/desktop/setup_native.mjs',
];
const nativeModules = new Set([
  'gi://Adw?version=1', 'gi://Gtk?version=4.0', 'gi://Gio', 'gi://GLib',
  'gi://GObject', 'gi://Gdk?version=4.0', 'gi://Pango', 'gi://Clutter', 'gi://St',
  'resource:///org/gnome/shell/extensions/extension.js',
  'resource:///org/gnome/shell/ui/main.js',
  'resource:///org/gnome/shell/ui/popupMenu.js',
  'resource:///org/gnome/shell/ui/quickSettings.js',
  'resource:///org/gnome/shell/ui/modalDialog.js',
]);
const directive = /^\/\/ aislop-ignore-next-line ai-slop\/hallucinated-import -- .*GJS runtime supplies this native module, not npm\.$/;

function nativeImports() {
  const imports = [];
  for (const file of files) {
    const lines = readFileSync(path.join(root, file), 'utf8').split('\n');
    lines.forEach((line, index) => {
      if (!line.includes('aislop-ignore')) return;
      assert.match(line, directive, `${file}:${index + 1}: suppression must be scoped and justified`);
      const imported = /^import .+ from '([^']+)';$/.exec(lines[index + 1]);
      assert.ok(imported && nativeModules.has(imported[1]), `${file}:${index + 2}: only known native modules`);
      imports.push(`${line}\n${lines[index + 1]}`);
    });
  }
  assert.equal(imports.length, 32);
  return imports;
}

function scan(directory) {
  const result = spawnSync(process.execPath, [path.join(__dirname, 'node_modules/aislop/dist/cli.js'),
    'scan', directory, '--json'], {encoding: 'utf8', timeout: 60000});
  assert.ifError(result.error);
  assert.equal(result.signal, null);
  const report = JSON.parse(result.stdout);
  assert.equal(report.cliVersion, '0.18.0');
  return report.diagnostics.filter(finding => finding.rule === 'ai-slop/hallucinated-import');
}

test('native import annotations preserve unknown npm import detection', () => {
  const imports = nativeImports();
  const directory = mkdtempSync(path.join(tmpdir(), 'gjs-aislop-'));
  try {
    mkdirSync(path.join(directory, '.aislop'));
    writeFileSync(path.join(directory, 'package.json'), '{"type":"module"}');
    writeFileSync(path.join(directory, '.aislop/config.yml'),
      'engines:\n  format: false\n  lint: false\n  code-quality: false\n  architecture: false\n  security: false\ntelemetry:\n  enabled: false\nci:\n  failBelow: 100\n');
    imports.forEach((source, index) => writeFileSync(path.join(directory, `native-${index}.js`), source));
    const unknown = "import missing from 'gws-undeclared-regression-package';\nexport default missing;\n";
    writeFileSync(path.join(directory, 'native-0.js'), `${imports[0]}\n${unknown}`);
    let findings = scan(directory);
    assert.equal(findings.length, 1);
    assert.match(findings[0].message, /gws-undeclared-regression-package/);
    // The identical native import without its annotation must still exercise the pinned rule.
    writeFileSync(path.join(directory, 'unannotated.js'), imports[0].split('\n')[1]);
    findings = scan(directory);
    assert.equal(findings.length, 2);
    assert.ok(findings.some(finding => finding.message.includes('gi:')));
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
});
