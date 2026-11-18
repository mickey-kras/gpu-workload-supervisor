const { execFileSync } = require('node:child_process');
const { statSync } = require('node:fs');
const { resolve } = require('node:path');

const AISLOP_PREFIX = '.github/aislop';
const BLOCKED_SEVERITIES = new Set(['moderate', 'high', 'critical']);
// GHSA-fc5q-39xc-vjpf affects picomatch * with no upstream release yet.
// Requiring range "*" fails closed automatically once a patched release exists.
const UNFIXED_ALLOWLIST = new Map([
  ['GHSA-fc5q-39xc-vjpf', { name: 'picomatch', range: '*' }],
]);

function plainObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function allowedAdvisory(pkg, item) {
  if (!plainObject(item) || typeof item.url !== 'string') return false;
  const match = /\/(GHSA-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4})$/.exec(item.url);
  if (!match) return false;
  const rule = UNFIXED_ALLOWLIST.get(match[1]);
  if (!rule) return false;
  return pkg === rule.name && item.name === rule.name && item.range === rule.range;
}

function excusedPackage(pkg, vulnerabilities, visiting = new Set()) {
  const entry = vulnerabilities[pkg];
  if (!plainObject(entry) || !Array.isArray(entry.via)) return false;
  if (entry.via.length === 0 || visiting.has(pkg)) return false;
  const next = new Set(visiting);
  next.add(pkg);
  for (const item of entry.via) {
    const ok = typeof item === 'string'
      ? excusedPackage(item, vulnerabilities, next)
      : allowedAdvisory(pkg, item);
    if (!ok) return false;
  }
  return true;
}

function parseReport(raw) {
  let report;
  try {
    report = JSON.parse(raw);
  } catch {
    throw new Error('npm audit did not return valid JSON');
  }
  const validShape = plainObject(report) && !report.error && report.auditReportVersion === 2;
  if (!validShape || !plainObject(report.vulnerabilities)) {
    throw new Error('npm audit did not produce a v2 vulnerability report');
  }
  return report.vulnerabilities;
}

function blockedVulnerability(pkg, entry) {
  if (!plainObject(entry) || typeof entry.severity !== 'string' || !Array.isArray(entry.via)) {
    throw new Error(`Malformed vulnerability entry for ${pkg}`);
  }
  if (BLOCKED_SEVERITIES.has(entry.severity)) return true;
  return entry.via.some(item => plainObject(item) && BLOCKED_SEVERITIES.has(item.severity));
}

function inspectAuditReport(raw) {
  const vulnerabilities = parseReport(raw);
  const unexcused = [];
  for (const [pkg, entry] of Object.entries(vulnerabilities)) {
    if (blockedVulnerability(pkg, entry) && !excusedPackage(pkg, vulnerabilities)) {
      unexcused.push(`${pkg} (${entry.severity})`);
    }
  }
  if (unexcused.length > 0) {
    throw new Error(`Unexcused npm audit vulnerabilities: ${unexcused.sort().join(', ')}`);
  }
}

function captureAuditJson(prefix, exec) {
  const args = ['audit', '--prefix', prefix, '--audit-level=moderate', '--json'];
  try {
    return exec('npm', args, { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 });
  } catch (error) {
    const hasExitOutput = typeof error?.status === 'number' && !error.signal && typeof error.stdout === 'string';
    if (!hasExitOutput) {
      const detail = error instanceof Error ? error.message : 'unknown error';
      throw new Error(`npm audit execution failed: ${detail}`, { cause: error });
    }
    return error.stdout;
  }
}

function runAudit(prefix = AISLOP_PREFIX, exec = execFileSync) {
  if (prefix !== AISLOP_PREFIX) throw new Error(`Unsupported audit prefix: ${prefix}`);
  if (!statSync(resolve(prefix, 'package-lock.json')).isFile()) {
    throw new Error(`Missing lockfile in ${prefix}`);
  }
  inspectAuditReport(captureAuditJson(prefix, exec));
}

if (require.main === module) {
  try {
    runAudit();
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}

module.exports = { inspectAuditReport, runAudit };
