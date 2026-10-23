const { createHash } = require('node:crypto');
const { canonical, normalizeWorkflowActionPins } = require('./policy-action-pins.cjs');

const CI = '.github/workflows/ci.yml';
const AISLOP = '.github/workflows/aislop.yml';
const MAIN = '.github/workflows/main.yml';

const REUSE_HASHES = {
  ".github/workflows/ci.yml": "2197ef20c3c2c0897dffb80a74df788feddac6ecc744315e2873e9def2413e1d",
  ".github/workflows/aislop.yml": "9b90546484fb17549a120cf47eb7ffd8b625aef2437568f393cb6e7ef51edc26",
  ".github/workflows/main.yml": "aeab59c43ceddc085498cd0ffcd5990fd5543b1f8a0b4c1fa995d07f18ddba6d"
};
const HELPER_HASHES = {
  ".github/scripts/quality-coverage.py": "96c047e6c876af769edbb141e2230861b665d368d27579244a2f8f356d141c65",
  ".github/scripts/check-scan-reports.py": "140cdb3f9330dab071fd687e88c053bc83513e1d3d5fc045250297eb60595cc6"
};

function protectedControls(path, workflow) {
  const controls = { permissions: workflow?.permissions, env: workflow?.env, defaults: workflow?.defaults };
  if (path === CI) return { ...controls, call: workflow?.on?.workflow_call, jobs: workflow?.jobs };
  if (path === AISLOP) return { ...controls, jobs: workflow?.jobs };
  return { ...controls, sonar: workflow?.jobs?.sonar };
}

function fingerprint(path, workflow) {
  return createHash('sha256').update(JSON.stringify(canonical(protectedControls(path, normalizeWorkflowActionPins(workflow))))).digest('hex');
}

function normalizeCiReuse(files, workflows, failures, admitted = new Set()) {
  const ci = workflows[CI];
  const steps = workflows[AISLOP]?.jobs?.status?.steps || [];
  const enabled = ci?.on?.workflow_call?.outputs ||
    steps.some(step => step.name === 'Aislop Go quality gate' && step.run?.includes('--sarif')) ||
    workflows[MAIN]?.jobs?.sonar?.steps?.some(step => step.name === 'Download Go quality coverage');
  if (!enabled) return workflows;
  // Accept only the reviewed deduplication profile before adapting it to the
  // existing graph and command checks. Never execute candidate helper code.
  for (const [path, hash] of Object.entries(REUSE_HASHES)) {
    if (fingerprint(path, workflows[path]) !== hash) failures.push(`${path} changed coverage/scanner reuse controls`);
  }
  for (const [path, hash] of Object.entries(HELPER_HASHES)) {
    if (!admitted.has(path) && createHash('sha256').update(files[path] || '').digest('hex') !== hash) {
      failures.push(`${path} changed fail-closed coverage/scanner validation`);
    }
  }
  const normalized = structuredClone(workflows);
  const jobs = normalized[CI]?.jobs || {};
  if (!jobs.go || !jobs.frontend || !jobs.scans || !jobs.package) return normalized;
  normalized[CI].on.workflow_call = null;
  for (const id of ['go', 'frontend']) {
    delete jobs[id].outputs;
    jobs[id].steps = jobs[id].steps.filter(step => !step.name?.includes('quality coverage'));
  }
  const scans = jobs.scans.steps;
  const report = scans.find(step => step.name === 'Trivy filesystem report');
  if (report) delete report.env;
  if (report) Object.assign(report.with, { format: 'sarif', output: 'trivy-results.sarif', severity: 'HIGH,CRITICAL' });
  jobs.scans.steps = scans.filter(step => !['Trivy conversion regressions', 'Convert Trivy SARIF'].includes(step.name));
  const upload = scans.find(step => step.name === 'Upload Trivy SARIF');
  if (upload) upload.if = "${{ !cancelled() && steps.trivy_report.outcome == 'success' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}";
  const gate = scans.find(step => step.name === 'Trivy high and critical gate');
  if (gate) {
    delete gate.run;
    gate.uses = 'aquasecurity/trivy-action@ed142fd0673e97e23eac54620cfb913e5ce36c25';
    gate.with = { 'scan-type': 'fs', 'scan-ref': '.', format: 'table', severity: 'HIGH,CRITICAL',
      'ignore-unfixed': true, 'exit-code': '1' };
  }
  const integration = jobs.package.steps.find(step => step.name === 'Native desktop transport integration');
  if (integration) integration.run = 'sudo apt-get update\nsudo apt-get install --no-install-recommends -y gjs python3\nsudo --preserve-env=GITHUB_ACTIONS,RUNNER_ENVIRONMENT timeout 180s bash scripts/native-desktop-integration.sh --isolated-test-host\n';
  const aislop = normalized[AISLOP]?.jobs?.status?.steps || [];
  const goScan = aislop.find(step => step.name === 'Aislop Go quality gate');
  if (goScan) {
    delete goScan.id;
    delete goScan.if;
    goScan.run = '.github/aislop/node_modules/.bin/aislop ci --human internal';
  }
  const sonar = normalized[MAIN]?.jobs?.sonar;
  if (sonar) {
    sonar.steps = sonar.steps.filter(step => !step.name?.includes('quality coverage'));
    const analysis = sonar.steps.findIndex(step => step.name === 'SonarQube analysis');
    sonar.steps.splice(analysis, 0, { name: 'Generate Go coverage', run: 'go test -coverprofile=coverage.out ./...' });
  }
  return normalized;
}

module.exports = { normalizeCiReuse, fingerprint };
