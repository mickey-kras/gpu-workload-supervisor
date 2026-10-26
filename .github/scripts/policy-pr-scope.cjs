const { readPolicyConstant } = require('./policy-admission.cjs');
const { createHash } = require('node:crypto');
const { isDeepStrictEqual } = require('node:util');
const { normalizeActionSteps } = require('./policy-action-pins.cjs');

const FULL_IF = "${{ github.event_name != 'pull_request' || inputs.validation-mode != 'docs-only' }}";
const GO_STEPS = ['Tests, race detector, and coverage', 'Upstream connection reuse benchmark',
  'Package coverage floors', 'Real systemd lifecycle qualification', 'Go vet', 'Go cognitive complexity',
  'Stage go quality coverage', 'Upload go quality coverage'];
const CANDIDATE_STEPS = ['Prepare CI-tested candidate artifacts', 'Upload CI-tested candidate artifacts'];
const SCOPE_INPUT = { 'validation-mode': { type: 'string', required: false, default: 'full' } };
const CLASSIFIER_DIGEST = 'b97a71c8a36ea6ae0ed1498a7a94d29d075effc2e84b112f5eba7417ee97318b';
const CLASSIFIER_SCRIPT = [
  "const { readFileSync } = require('node:fs');",
  "const { createHash } = require('node:crypto');",
  "const { resolve } = require('node:path');",
  "const { compileFunction } = require('node:vm');",
  "const filename = resolve('.github/scripts/pr-validation-scope.cjs');",
  'const source = readFileSync(filename);',
  "if (createHash('sha256').update(source).digest('hex') !==",
  `    '${CLASSIFIER_DIGEST}') {`,
  "  throw new Error('PR validation classifier bytes changed in the workspace');", '}',
  'const classifier = { exports: {} };',
  "const noImports = () => { throw new Error('Unexpected classifier dependency'); };",
  "compileFunction(source.toString('utf8'), ['require', 'module', 'exports'], { filename })(",
  '  noImports, classifier, classifier.exports,', ');',
  'await classifier.exports.run({ github, context, core });', '',
].join('\n');
const CLASSIFIER_JOB = {
  name: 'classify changed paths', 'runs-on': 'ubuntu-24.04', 'timeout-minutes': 5,
  permissions: { contents: 'read', 'pull-requests': 'read' },
  outputs: { 'validation-mode': '${{ steps.scope.outputs.validation-mode }}' },
  steps: [
    { uses: 'step-security/harden-runner@351661ca32ac09a36dc5ee2d536e3128f2a3c8ed', with: { 'egress-policy': 'audit' } },
    { uses: 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1',
      with: { ref: '${{ github.event.pull_request.base.sha }}', 'persist-credentials': false } },
    { name: 'Classify changed paths', id: 'scope',
      uses: 'actions/github-script@3a2844b7e9c422d3c10d287c895573f7108da1b3',
      with: { script: CLASSIFIER_SCRIPT } },
  ],
};
const SCOPED_QUALITY = { needs: 'classify', if: '${{ !cancelled() }}', uses: './.github/workflows/ci.yml',
  with: { 'validation-mode': "${{ needs.classify.result == 'success' && needs.classify.outputs.validation-mode || 'full' }}" } };
const SCOPED_AGGREGATE = `python3 - <<'PYTHON'
import json
import os
import re

required = {'native', 'go', 'frontend', 'scans', 'package', 'docs-ste'}
results = json.loads(os.environ['CI_NEEDS'])
docs = os.environ['CI_EVENT'] == 'pull_request' and os.environ['CI_MODE'] == 'docs-only'
expected = {job: ('skipped' if docs and job == 'native' else 'success') for job in required}
if set(results) != required or any(results[job].get('result') != expected[job] for job in required):
    raise SystemExit('Required CI work did not succeed: ' + json.dumps(results, sort_keys=True))
outputs = results['package'].get('outputs', {})
if any(not re.fullmatch(r'[1-9][0-9]*', outputs.get(key, '')) for key in ('payload-id', 'qualified-attempt')):
    raise SystemExit('Missing qualified package output; re-run all jobs instead of selecting another artifact')
PYTHON`;
const AGGREGATE_ENV = { CI_NEEDS: '${{ toJSON(needs) }}',
  CI_EVENT: '${{ github.event_name }}', CI_MODE: '${{ inputs.validation-mode }}' };

function normalizeScopedValidation(files, workflows, failures, aggregateScript, admitted = new Set()) {
  const classifierPath = '.github/scripts/pr-validation-scope.cjs';
  const digest = createHash('sha256').update(files[classifierPath] || '').digest('hex');
  const reviewed = admitted.has(classifierPath);
  if (!reviewed && digest !== CLASSIFIER_DIGEST) {
    failures.push('PR validation classifier was changed');
  }
  const pr = workflows['.github/workflows/pr-validation.yml'];
  const ci = workflows['.github/workflows/ci.yml'];
  const classifier = pr?.jobs?.classify;
  const input = ci?.on?.workflow_call?.inputs;
  if (!classifier && !input) return workflows;
  if (!pr?.jobs || !ci?.jobs?.native || !ci.jobs.go || !ci.jobs.checks ||
      typeof ci?.on?.workflow_call !== 'object' || !ci.on.workflow_call) {
    failures.push('Path-aware validation has an incomplete workflow graph');
    return workflows;
  }
  const requireValue = (ok, label) => { if (!ok) failures.push(`Path-aware validation lost ${label}`); };
  const expectedClassifier = structuredClone(CLASSIFIER_JOB);
  if (reviewed) {
    const policyDigest = readPolicyConstant(files['.github/scripts/policy-pr-scope.cjs'], 'CLASSIFIER_DIGEST');
    requireValue(policyDigest === digest, 'classifier policy digest synchronized with helper');
    expectedClassifier.steps[2].with.script = CLASSIFIER_SCRIPT.replace(CLASSIFIER_DIGEST, digest);
  }
  requireValue(isDeepStrictEqual(classifier && { ...classifier, steps: normalizeActionSteps(classifier.steps) },
    { ...expectedClassifier, steps: normalizeActionSteps(expectedClassifier.steps) }), 'trusted read-only classifier sequence');
  requireValue(!Object.hasOwn(pr, 'env') && !Object.hasOwn(pr, 'defaults'), 'trusted classifier environment');
  requireValue(isDeepStrictEqual(pr.jobs.quality, SCOPED_QUALITY), 'full-fallback quality input');
  requireValue(isDeepStrictEqual(input, SCOPE_INPUT), 'default-full reusable input');
  const normalized = structuredClone(workflows);
  const normalizedCi = normalized['.github/workflows/ci.yml'];
  const normalizedPr = normalized['.github/workflows/pr-validation.yml'];
  delete normalizedPr.jobs.classify;
  normalizedPr.jobs.quality = { uses: './.github/workflows/ci.yml' };
  delete normalizedCi.on.workflow_call.inputs;
  requireValue(normalizedCi.jobs.native?.if === FULL_IF, 'PR-only native condition');
  delete normalizedCi.jobs.native.if;
  for (const [job, names] of [['go', GO_STEPS], ['checks', CANDIDATE_STEPS]]) {
    for (const step of normalizedCi.jobs[job]?.steps || []) {
      if (!names.includes(step.name)) continue;
      requireValue(step.if === FULL_IF, `PR-only condition for ${step.name}`);
      delete step.if;
    }
  }
  const gate = normalizedCi.jobs.checks?.steps?.find(step => step.name === 'Require every CI job to succeed');
  requireValue(gate?.run?.trim() === SCOPED_AGGREGATE && isDeepStrictEqual(gate.env, AGGREGATE_ENV),
    'explicit docs-only native skip result gate');
  if (gate) { gate.run = aggregateScript; gate.env = { CI_NEEDS: '${{ toJSON(needs) }}' }; }
  return normalized;
}

module.exports = { normalizeScopedValidation, FULL_IF, GO_STEPS, CANDIDATE_STEPS,
  SCOPE_INPUT, CLASSIFIER_JOB, SCOPED_QUALITY, SCOPED_AGGREGATE, AGGREGATE_ENV, CLASSIFIER_SCRIPT };
