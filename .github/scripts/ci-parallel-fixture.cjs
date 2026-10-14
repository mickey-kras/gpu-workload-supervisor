const { readFileSync } = require('node:fs');
const YAML = require('yaml');
const { REQUIRED_JOBS, STEP_JOBS, AGGREGATE_RUN } = require('./policy-ci-parallel.cjs');

function parallelFixture() {
  const workflow = YAML.parse(readFileSync('.github/workflows/ci.yml', 'utf8'));
  // Legacy graph mutation tests use the independent 508 contract after reuse.
  if (workflow.on.workflow_call?.outputs) {
    workflow.on.workflow_call = null;
    for (const id of ['go', 'frontend']) {
      delete workflow.jobs[id].outputs;
      workflow.jobs[id].steps = workflow.jobs[id].steps.filter(step => !step.name?.includes('quality coverage') && step.name !== 'CI reuse regressions');
    }
    const scans = workflow.jobs.scans.steps;
    const report = scans.find(step => step.name === 'Trivy filesystem report');
    delete report.env;
    Object.assign(report.with, { format: 'sarif', output: 'trivy-results.sarif', severity: 'HIGH,CRITICAL' });
    workflow.jobs.scans.steps = scans.filter(step => !['Trivy conversion regressions', 'Convert Trivy SARIF'].includes(step.name));
    scans.find(step => step.name === 'Upload Trivy SARIF').if = "${{ !cancelled() && steps.trivy_report.outcome == 'success' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}";
    const gate = scans.find(step => step.name === 'Trivy high and critical gate');
    delete gate.run;
    gate.uses = 'aquasecurity/trivy-action@ed142fd0673e97e23eac54620cfb913e5ce36c25';
    gate.with = { 'scan-type': 'fs', 'scan-ref': '.', format: 'table', severity: 'HIGH,CRITICAL', 'ignore-unfixed': true, 'exit-code': '1' };
    workflow.jobs.package.steps.find(step => step.name === 'Native desktop transport integration').run =
      'sudo apt-get update\nsudo apt-get install --no-install-recommends -y gjs python3\nsudo --preserve-env=GITHUB_ACTIONS,RUNNER_ENVIRONMENT timeout 180s bash scripts/native-desktop-integration.sh --isolated-test-host\n';
  }
  if (workflow.jobs.native) return workflow;
  const steps = workflow.jobs.checks.steps;
  const common = steps.filter(step => !step.name && !step.id && !step.uses?.startsWith('actions/setup-node@'));
  const find = name => structuredClone(steps.find(step => step.name === name));
  const gate = { name: 'Require every CI job to succeed', env: { CI_NEEDS: '${{ toJSON(needs) }}' }, run: AGGREGATE_RUN + '\n' };
  const payloadName = 'package-payload-${{ github.run_id }}-${{ github.run_attempt }}';
  const added = {
    'Require every CI job to succeed': gate,
    'Download qualified package payload': { name: 'Download qualified package payload',
      uses: 'actions/download-artifact@37930b1c2abaa49bbe596cd826c3c89aef350131', with: { 'artifact-ids': '${{ needs.package.outputs.payload-id }}', path: 'dist/' } },
    'Upload qualified package payload': { name: 'Upload qualified package payload', id: 'payload',
      uses: 'actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a', with: {
        name: payloadName, path: 'dist/*.tar.gz\ndist/*.deb\ndist/checksums.txt\n',
        'if-no-files-found': 'error', 'compression-level': 0, 'retention-days': 1,
      } },
    'Record qualified package attempt': { name: 'Record qualified package attempt', id: 'qualification',
      run: 'echo "attempt=$GITHUB_RUN_ATTEMPT" >> "$GITHUB_OUTPUT"' },
    'CI shard regressions': { name: 'CI shard regressions',
      run: 'python3 -m unittest discover -s tests/desktop -p test_setup_native_shards.py' },
  };
  workflow.jobs = Object.fromEntries(Object.entries(STEP_JOBS).map(([id, names]) => [id, {
    name: id, 'runs-on': 'ubuntu-latest', steps: [...structuredClone(common), ...names.map(name => added[name] || find(name))],
  }]));
  for (const id of ['go', 'package']) {
    workflow.jobs[id].steps.splice(common.length, 0, structuredClone(steps.find(step => step.uses?.startsWith('actions/setup-go@'))));
  }
  for (const id of ['go', 'frontend', 'package']) {
    workflow.jobs[id].steps.splice(common.length + (id === 'frontend' ? 0 : 1), 0, structuredClone(steps.find(step => step.uses?.startsWith('actions/setup-node@'))));
  }
  workflow.jobs.package.outputs = { 'payload-id': '${{ steps.payload.outputs.artifact-id }}',
    'qualified-attempt': '${{ steps.qualification.outputs.attempt }}' };
  workflow.jobs.native.strategy = { 'fail-fast': false, matrix: { shard: [0, 1, 2, 3, 4, 5] } };
  workflow.jobs.native.steps.find(step => step.name === 'Native GTK setup, accessibility and screenshot matrix').run = 'python3 tests/desktop/setup_native.py --output native-setup-evidence/shard-${{ matrix.shard }} --shard-index ${{ matrix.shard }} --shard-count 6';
  workflow.jobs.native.steps.find(step => step.name === 'Upload native setup evidence').with.name =
    'native-setup-${{ matrix.shard }}-${{ github.run_id }}-${{ github.run_attempt }}';
  const manifest = workflow.jobs.checks.steps.find(step => step.name === 'Prepare CI-tested candidate artifacts');
  manifest.env.PACKAGE_QUALIFICATION_ATTEMPT = '${{ needs.package.outputs.qualified-attempt }}';
  manifest.run = manifest.run.replace('"runAttempt": os.environ["GITHUB_RUN_ATTEMPT"],',
    '"runAttempt": os.environ["GITHUB_RUN_ATTEMPT"],\n    "packageQualificationAttempt": os.environ["PACKAGE_QUALIFICATION_ATTEMPT"],');
  workflow.jobs.checks.if = '${{ always() }}';
  workflow.jobs.checks.needs = [...REQUIRED_JOBS];
  return workflow;
}

module.exports = { parallelFixture };
