const YAML = require('yaml');
const { canonical } = require('./policy-action-pins.cjs');

const LOCKFILES = ['.github/scripts/package-lock.json', '.github/aislop/package-lock.json'];
const FORBIDDEN_INPUTS = ['.npmrc', '.github/.npmrc', '.github/scripts/.npmrc', '.github/aislop/.npmrc',
  '.github/scripts/npm-shrinkwrap.json', '.github/aislop/npm-shrinkwrap.json'];
const parsed = new Map();
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const equal = (a, b) => JSON.stringify(canonical(a)) === JSON.stringify(canonical(b));

function parse(source) {
  if (typeof source !== 'string' || Buffer.byteLength(source) > 4 * 1024 * 1024) {
    throw new Error('missing or oversized JSON');
  }
  if (parsed.has(source)) return parsed.get(source);
  const value = JSON.parse(source);
  const document = YAML.parseDocument(source, { uniqueKeys: true });
  if (document.errors.length) throw new Error('duplicate JSON keys');
  if (parsed.size === 8) parsed.delete(parsed.keys().next().value);
  parsed.set(source, value);
  return value;
}

function registryTarball(value) {
  if (typeof value !== 'string' || !/^https:\/\/registry\.npmjs\.org\/(?:@[a-z0-9_.-]+\/)?[a-z0-9_.-]+\/-\/[a-zA-Z0-9_.+-]+\.tgz$/.test(value)) return false;
  const url = new URL(value);
  return url.origin === 'https://registry.npmjs.org' && !url.username && !url.password && !url.search && !url.hash;
}

function integrity(value) {
  if (typeof value !== 'string' || !/^sha512-[A-Za-z0-9+/]{86}==$/.test(value)) return false;
  const encoded = value.slice(7);
  const decoded = Buffer.from(encoded, 'base64');
  return decoded.length === 64 && decoded.toString('base64') === encoded;
}

function inspectLock(path, source, previousSource, manifestSource, previousManifestSource, admitted) {
  const lock = parse(source);
  const previous = parse(previousSource);
  const manifest = parse(manifestSource);
  const previousManifest = parse(previousManifestSource);
  if (!object(lock) || lock.lockfileVersion !== 3 || !object(lock.packages) ||
      !object(lock.packages['']) || Object.keys(lock.packages).length > 50000 ||
      !object(previous?.packages) || !object(manifest) ||
      (!admitted && !equal(manifest, previousManifest))) {
    throw new Error('requires lockfile v3, a base lockfile, and an unchanged CI manifest');
  }
  const root = lock.packages[''];
  for (const key of ['name', 'version', 'dependencies', 'devDependencies', 'optionalDependencies', 'peerDependencies', 'engines']) {
    if (!equal(root[key], manifest[key])) throw new Error(`root ${key} differs from package.json`);
  }
  if (root.hasInstallScript || root.link || root.inBundle || root.resolved || root.integrity) {
    throw new Error('unexpected lockfile root metadata');
  }
  for (const [location, entry] of Object.entries(lock.packages)) {
    if (location === '') continue;
    if (!/^(?:node_modules\/(?:@[a-z0-9_.-]+\/)?[a-z0-9_.-]+)(?:\/node_modules\/(?:@[a-z0-9_.-]+\/)?[a-z0-9_.-]+)*$/.test(location) ||
        !object(entry) || entry.link || entry.inBundle || !registryTarball(entry.resolved) || !integrity(entry.integrity) ||
        typeof entry.version !== 'string' || !/^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/.test(entry.version)) {
      throw new Error(`invalid registry package at ${location}`);
    }
    if (Object.hasOwn(entry, 'hasInstallScript') && typeof entry.hasInstallScript !== 'boolean') {
      throw new Error(`invalid install-script flag at ${location}`);
    }
    if (entry.hasInstallScript && previous.packages[location]?.hasInstallScript !== true) {
      throw new Error(`new install script at ${location}`);
    }
    const name = location.split('node_modules/').at(-1);
    const tarball = `https://registry.npmjs.org/${name}/-/${name.split('/').at(-1)}-${entry.version}.tgz`;
    if (entry.resolved !== tarball || entry.hasShrinkwrap) {
      throw new Error(`package identity or nested shrinkwrap changed at ${location}`);
    }
  }
}

function inspectLockfiles(files, baseline, failures, admitted = new Set()) {
  for (const path of FORBIDDEN_INPUTS) {
    if (Object.hasOwn(files, path)) failures.push(`${path}: CI npm input redirection is forbidden`);
  }
  for (const path of LOCKFILES) {
    const manifest = path.replace('package-lock.json', 'package.json');
    try {
      inspectLock(path, files[path], baseline[path], files[manifest], baseline[manifest], admitted.has(manifest));
    } catch (error) {
      failures.push(`${path}: ${error.message}`);
    }
  }
}

module.exports = { FORBIDDEN_INPUTS, LOCKFILES, inspectLockfiles };
