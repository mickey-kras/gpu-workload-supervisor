const test = require('node:test');
const assert = require('node:assert/strict');
const { inspectAuditReport, runAudit } = require('./audit-aislop.cjs');

const BEFORE_EXPIRY = Date.parse('2026-11-18T00:00:00Z');

function advisory(id, { name = 'picomatch', range = '<=4.0.7', severity = 'high' } = {}) {
  return { name, severity, range, url: `https://github.com/advisories/${id}` };
}

function picomatchGraph(rootAdvisory = advisory('GHSA-fc5q-39xc-vjpf')) {
  return {
    auditReportVersion: 2,
    vulnerabilities: {
      picomatch: { name: 'picomatch', severity: 'high', via: [rootAdvisory] },
      fdir: { name: 'fdir', severity: 'high', via: ['picomatch'] },
      tinyglobby: { name: 'tinyglobby', severity: 'high', via: ['fdir', 'picomatch'] },
      knip: { name: 'knip', severity: 'high', via: ['fdir', 'picomatch', 'tinyglobby'] },
      aislop: { name: 'aislop', severity: 'high', via: ['knip', 'picomatch'] },
    },
  };
}

test('clean v2 reports and low-only findings pass', () => {
  inspectAuditReport(JSON.stringify({ auditReportVersion: 2, vulnerabilities: {} }));
  const lowOnly = advisory('GHSA-0000-0000-0000', { name: 'other', severity: 'low' });
  inspectAuditReport(JSON.stringify({
    auditReportVersion: 2,
    vulnerabilities: { other: { name: 'other', severity: 'low', via: [lowOnly] } },
  }));
});

test('unfixed picomatch GHSA-fc5q-39xc-vjpf and its transitive chain pass for the exact advisory range', () => {
  assert.equal(inspectAuditReport(JSON.stringify(picomatchGraph()), BEFORE_EXPIRY), true);
  const exitOne = () => {
    const error = new Error('vulnerabilities found');
    error.status = 1;
    error.signal = null;
    error.stdout = JSON.stringify(picomatchGraph());
    throw error;
  };
  assert.equal(runAudit('.github/aislop', exitOne, BEFORE_EXPIRY), true);
});

test('the exception expires at the configured deadline', () => {
  const report = JSON.stringify(picomatchGraph());
  assert.throws(() => inspectAuditReport(report, Date.parse('2026-12-02T08:00:00Z')), /Unexcused/);
});

test('malformed reports and cyclic vulnerability graphs fail closed', () => {
  const invalidReports = [
    '',
    'not json',
    JSON.stringify({ auditReportVersion: 1, vulnerabilities: {} }),
    JSON.stringify({ auditReportVersion: 2, error: { code: 'ENOAUDIT' }, vulnerabilities: {} }),
    JSON.stringify({ auditReportVersion: 2, vulnerabilities: [] }),
    JSON.stringify({ auditReportVersion: 2, vulnerabilities: { pkg: { severity: 'high', via: [] } } }),
    JSON.stringify({
      auditReportVersion: 2,
      vulnerabilities: { a: { severity: 'high', via: ['b'] }, b: { severity: 'high', via: ['a'] } },
    }),
  ];
  for (const raw of invalidReports) {
    assert.throws(() => inspectAuditReport(raw));
  }
});

test('changed picomatch ranges and any other moderate or higher advisories fail closed', () => {
  const tarAdvisory = advisory('GHSA-1111-2222-3333', { name: 'tar', severity: 'moderate' });
  const unexcusedReports = [
    picomatchGraph(advisory('GHSA-fc5q-39xc-vjpf', { range: '<4.0.7' })),
    picomatchGraph(advisory('GHSA-fc5q-39xc-vjpf', { range: '*' })),
    picomatchGraph(advisory('GHSA-aaaa-bbbb-cccc')),
    picomatchGraph(advisory('GHSA-fc5q-39xc-vjpf', { severity: 'critical' })),
    {
      auditReportVersion: 2,
      vulnerabilities: {
        ...picomatchGraph().vulnerabilities,
        tar: { name: 'tar', severity: 'moderate', via: [tarAdvisory] },
      },
    },
  ];
  for (const report of unexcusedReports) {
    assert.throws(() => inspectAuditReport(JSON.stringify(report), BEFORE_EXPIRY));
  }
});

test('execution errors, signals and unsupported prefixes fail closed', () => {
  assert.throws(() => runAudit('clients/gnome', () => '{}'), /Unsupported audit prefix/);
  assert.throws(() => runAudit('.github/aislop', () => {
    const error = new Error('spawn ENOENT');
    error.code = 'ENOENT';
    throw error;
  }), /execution failed/);
  assert.throws(() => runAudit('.github/aislop', () => {
    const error = new Error('killed');
    error.status = 1;
    error.signal = 'SIGTERM';
    error.stdout = JSON.stringify({ auditReportVersion: 2, vulnerabilities: {} });
    throw error;
  }), /execution failed/);
});
