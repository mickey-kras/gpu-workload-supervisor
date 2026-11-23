const { readFileSync } = require('node:fs');
const YAML = require('yaml');

function reuseFixture() {
  let files = {};
  for (const name of ['ci', 'aislop', 'main', 'pr-validation']) {
    const path = `.github/workflows/${name}.yml`;
    files[path] = YAML.parse(readFileSync(path, 'utf8'));
  }
  if (files['.github/workflows/ci.yml'].on.workflow_call?.inputs) {
    const { normalizeScopedValidation } = require('./policy-pr-scope.cjs');
    const { AGGREGATE_RUN } = require('./policy-ci-parallel.cjs');
    files = normalizeScopedValidation({ '.github/scripts/pr-validation-scope.cjs':
      readFileSync('.github/scripts/pr-validation-scope.cjs', 'utf8') }, files, [], AGGREGATE_RUN + '\n');
  }
  const ci = files['.github/workflows/ci.yml'];
  if (ci.on.workflow_call?.outputs) return files;
  ci.on.workflow_call = { outputs: {
    'go-coverage-id': { description: 'Successful Go quality coverage artifact', value: '${{ jobs.go.outputs.coverage-id }}' },
    'javascript-coverage-id': { description: 'Successful JavaScript quality coverage artifact', value: '${{ jobs.frontend.outputs.coverage-id }}' },
  } };
  for (const [id, kind] of [['go', 'go'], ['frontend', 'javascript']]) {
    ci.jobs[id].outputs = { 'coverage-id': '${{ steps.coverage.outputs.artifact-id }}' };
    ci.jobs[id].steps.push({ name: `Stage ${kind} quality coverage`,
      run: `python3 .github/scripts/quality-coverage.py stage ${kind} ${kind}-quality-coverage` }, {
      name: `Upload ${kind} quality coverage`, id: 'coverage',
      uses: 'actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a', with: {
        name: `${kind}-quality-coverage-` + '${{ github.run_id }}-${{ github.run_attempt }}', path: `${kind}-quality-coverage/`,
        'if-no-files-found': 'error', 'retention-days': 1,
      },
    });
  }
  const scans = ci.jobs.scans.steps;
  const report = scans.find(step => step.name === 'Trivy filesystem report');
  report.env = { TRIVY_SEVERITY: 'UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL' };
  Object.assign(report.with, { format: 'json', output: 'trivy-results.json', severity: 'UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL' });
  const upload = scans.find(step => step.name === 'Upload Trivy SARIF');
  scans.splice(scans.indexOf(upload), 0, { name: 'Trivy conversion regressions',
    run: 'python3 -m unittest discover -s tests -p test_trivy_convert.py' }, {
    name: 'Convert Trivy SARIF', id: 'trivy_sarif',
    run: 'python3 .github/scripts/check-scan-reports.py trivy trivy-results.json\ntrivy convert --format sarif --severity UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL --output trivy-results.sarif trivy-results.json\n',
  });
  upload.if = "${{ !cancelled() && steps.trivy_sarif.outcome == 'success' && steps.trivy_report.outcome == 'success' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}";
  const gate = scans.find(step => step.name === 'Trivy high and critical gate');
  delete gate.uses;
  delete gate.with;
  gate.run = 'trivy convert --format table --severity HIGH,CRITICAL --exit-code 1 trivy-results.json';
  ci.jobs.package.steps.find(step => step.name === 'Native desktop transport integration').run =
    'sudo --preserve-env=GITHUB_ACTIONS,RUNNER_ENVIRONMENT timeout 180s bash scripts/native-desktop-integration.sh --isolated-test-host';
  const aislop = files['.github/workflows/aislop.yml'].jobs.status;
  aislop.steps.find(step => step.run?.startsWith('npm ci')).run = 'npm ci --prefix .github/aislop --ignore-scripts --no-audit --no-fund';
  const goScan = aislop.steps.find(step => step.name === 'Aislop Go quality gate');
  goScan.id = 'go_scan';
  goScan.if = 'always()';
  goScan.run = '.github/aislop/node_modules/.bin/aislop ci --sarif internal > aislop.sarif';
  aislop.steps = aislop.steps.filter(step => step.name !== 'Aislop Go SARIF');
  aislop.steps.find(step => step.name === 'Aislop findings gate').run = 'python3 .github/scripts/check-scan-reports.py aislop aislop.sarif';
  const aislopUpload = aislop.steps.find(step => step.name === 'Upload Aislop SARIF');
  aislopUpload.if = aislopUpload.if.replace('!cancelled() &&', "!cancelled() && steps.go_scan.outcome != 'skipped' &&");
  const sonar = files['.github/workflows/main.yml'].jobs.sonar;
  sonar.steps = sonar.steps.filter(step => !step.uses?.startsWith('actions/setup-go@') &&
    !step.uses?.startsWith('actions/setup-node@') && !['Generate Go coverage', 'Generate JavaScript coverage'].includes(step.name));
  const inserted = [
    { name: 'Require current quality coverage outputs', env: {
      GO_COVERAGE_ID: '${{ needs.quality.outputs.go-coverage-id }}',
      JAVASCRIPT_COVERAGE_ID: '${{ needs.quality.outputs.javascript-coverage-id }}',
    }, run: `python3 - <<'PYTHON'
import os
import re

if any(not re.fullmatch(r'[1-9][0-9]*', os.environ[key]) for key in ('GO_COVERAGE_ID', 'JAVASCRIPT_COVERAGE_ID')):
    raise SystemExit('Missing successful quality coverage outputs; re-run all jobs')
PYTHON
` },
    { name: 'Download Go quality coverage', uses: 'actions/download-artifact@37930b1c2abaa49bbe596cd826c3c89aef350131',
      with: { 'artifact-ids': '${{ needs.quality.outputs.go-coverage-id }}', path: 'go-quality-coverage/' } },
    { name: 'Download JavaScript quality coverage', uses: 'actions/download-artifact@37930b1c2abaa49bbe596cd826c3c89aef350131',
      with: { 'artifact-ids': '${{ needs.quality.outputs.javascript-coverage-id }}', path: 'javascript-quality-coverage/' } },
    { name: 'Validate and restore quality coverage',
      run: 'python3 .github/scripts/quality-coverage.py restore go go-quality-coverage\npython3 .github/scripts/quality-coverage.py restore javascript javascript-quality-coverage\n' },
  ];
  const first = sonar.steps.findIndex(step => step.name === 'Mask configured SonarQube endpoint');
  sonar.steps.splice(first, 0, ...inserted);
  return files;
}

module.exports = { reuseFixture };
