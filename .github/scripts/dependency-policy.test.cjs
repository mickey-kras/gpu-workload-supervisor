const test = require('node:test');
const assert = require('node:assert/strict');
const { inspect } = require('./policy-guard.cjs');

const path = '.github/dependency-review-config.yml';
const reviewed = "# Vulnerabilities block updates; license obligations depend on how code is used.\n# See SECURITY.md for the manual license review required for shipped dependencies.\nfail-on-severity: high\nvulnerability-check: true\nlicense-check: false\nwarn-only: false\nfail-on-scopes:\n  - runtime\n  - development\n  - unknown\n";
const failures = content => inspect({ [path]: content }).filter(error => error.startsWith('Dependency review policy') || error.startsWith('Dependency review config'));
const legacy = "# Dependency review policy for pull requests (actions/dependency-review-action).\n# Only dependencies a pull request adds or updates are reviewed, so existing\n# dependencies are grandfathered. A reviewed exception for one of them belongs\n# in allow-dependencies-licenses below as an exact package URL (purl).\nfail-on-severity: high\ndeny-licenses:\n  - AGPL-1.0-only\n  - AGPL-1.0-or-later\n  - AGPL-3.0-only\n  - AGPL-3.0-or-later\n  - GPL-1.0-only\n  - GPL-1.0-or-later\n  - GPL-2.0-only\n  - GPL-2.0-or-later\n  - GPL-3.0-only\n  - GPL-3.0-or-later\n  - LGPL-2.1-only\n  - LGPL-2.1-or-later\n  - LGPL-3.0-only\n  - LGPL-3.0-or-later\n  - SSPL-1.0\n  - LicenseRef-clearlydefined-OTHER\n# Grandfathered license exceptions as exact purls (e.g. pkg:npm/example@1.2.3).\nallow-dependencies-licenses: []\n";

test('retired blanket policy is rejected', () => {
  assert.notDeepEqual(failures(legacy), []);
  assert.notDeepEqual(failures(legacy + '\n'), []);
});

test('dependency review accepts usage-based license review with blocking vulnerability checks', () => {
  assert.deepEqual(failures(reviewed), []);
});

test('dependency review rejects vulnerability bypasses and narrowed scopes', () => {
  for (const content of [
    reviewed.replace('high', 'critical'),
    reviewed.replace('vulnerability-check: true', 'vulnerability-check: false'),
    reviewed.replace('warn-only: false', 'warn-only: true'),
    reviewed.replace('  - development\n', ''),
    reviewed + 'allow-ghsas: [GHSA-xxxx-xxxx-xxxx]\n',
    reviewed + 'vulnerability-check: false\n',
  ]) assert.notDeepEqual(failures(content), []);
});
