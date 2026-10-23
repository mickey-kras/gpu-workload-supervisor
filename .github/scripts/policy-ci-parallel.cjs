const { createHash } = require('node:crypto');
const { sameActionSteps } = require('./policy-action-pins.cjs');

const REQUIRED_JOBS = ['native', 'go', 'frontend', 'scans', 'package', 'docs-ste'];
const AGGREGATE_RUN = `python3 - <<'PYTHON'
import json
import os
import re

required = {'native', 'go', 'frontend', 'scans', 'package', 'docs-ste'}
results = json.loads(os.environ['CI_NEEDS'])
if set(results) != required or any(results[job].get('result') != 'success' for job in required):
    raise SystemExit('Required CI work did not succeed: ' + json.dumps(results, sort_keys=True))
outputs = results['package'].get('outputs', {})
if any(not re.fullmatch(r'[1-9][0-9]*', outputs.get(key, '')) for key in ('payload-id', 'qualified-attempt')):
    raise SystemExit('Missing qualified package output; re-run all jobs instead of selecting another artifact')
PYTHON`;
const SHARD_RUN = 'python3 tests/desktop/setup_native.py --output native-setup-evidence/shard-${{ matrix.shard }} --shard-index ${{ matrix.shard }} --shard-count 6';
const STEP_JOBS = {
  native: ['Install native setup fixture dependencies', 'Native GTK setup, accessibility and screenshot matrix', 'Upload native setup evidence'],
  go: ['Go formatting', 'Go module lock is current', 'Tests, race detector, and coverage',
    'Upstream connection reuse benchmark', 'Package coverage floors', 'Real systemd lifecycle qualification',
    'Go vet', 'Go cognitive complexity', 'Test Go suppression guard', 'Go vulnerability audit'],
  frontend: ['GNOME contract and lifecycle fixtures', 'Audit Aislop toolchain', 'Test policy automation',
    'JavaScript maintainability', 'Sonar privacy tests', 'CI shard regressions'],
  scans: ['Gitleaks', 'Semgrep', 'Test setup shared-literal rule', 'Trivy filesystem report',
    'Upload Trivy SARIF', 'Trivy high and critical gate'],
  package: ['Install native setup fixture dependencies', 'Install verified GoReleaser',
    'Validate GoReleaser configuration', 'Build snapshot artifacts', 'Verify snapshot archives',
    'Verify snapshot desktop packages', 'Native desktop transport integration',
    'Packaged setup lifecycle integration', 'Debian payload lifecycle integration', 'Record qualified package attempt', 'Upload qualified package payload'],
  checks: ['Require every CI job to succeed', 'Download qualified package payload',
    'Prepare CI-tested candidate artifacts', 'Upload CI-tested candidate artifacts'],
};

// Exact qualification scripts from the serial workflow, independent of job location.
const QUALIFICATION_HASHES = {
  "Upstream connection reuse benchmark": "6808b159e8f5d4dc67d8c185465991576af0828fbb700afec5fb692be7c1a537",
  "Real systemd lifecycle qualification": "76047c17f4116b5fad96e44d34f0a6658088ee9ad94fd929a0a212de33235f0b",
  "GNOME contract and lifecycle fixtures": "35f213c47512e87a1cd96a924553af5e58bee6469b62577603fc4e690ac828e4",
  "Verify snapshot archives": "dc85b7993f5a54f6bc9968ba01571380a5e23b352cbb300ec742cf0a2bc2641b",
  "Verify snapshot desktop packages": "98636ccdb17ca619a41234031db347e078cf7fd5d3017e32929e088227016442",
  "Native desktop transport integration": "bc6031eb50400552603ca8c98ea6313c2d70d18655ac58bebe0031481d901dce",
  "Packaged setup lifecycle integration": "e6a071374ecd14821bc28a74d39c0f3f207b97882bb8cf7df756ca654c60ec15",
  "Debian payload lifecycle integration": "941a2a875027f5bed821ff131f415e9f3050afff0259a9cf07a830cc13a9152c",
  "Sonar privacy tests": "d851ddb7cd478712e7b8054c9213719b9364bebc695b8d1963be697e15f91114",
  "Prepare CI-tested candidate artifacts": "500f1da88814514a3cf399812f7641620b422e7aa687e306ecf6a0b7bc7878ab",
  "Install native setup fixture dependencies": "1d7c4af49c5dcc5380e03aa54f4fcd0540bba2b0f356a211477048bc3c78d6d4"
};

const SETUP_STEPS = {
  "harden": {
    "uses": "step-security/harden-runner@351661ca32ac09a36dc5ee2d536e3128f2a3c8ed",
    "with": {
      "egress-policy": "audit"
    }
  },
  "checkout": {
    "uses": "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1",
    "with": {
      "fetch-depth": 0,
      "persist-credentials": false
    }
  },
  "go": {
    "id": "go",
    "uses": "actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
    "with": {
      "go-version-file": "go.mod",
      "cache": true
    }
  },
  "node": {
    "uses": "actions/setup-node@820762786026740c76f36085b0efc47a31fe5020",
    "with": {
      "node-version": 22,
      "cache": "npm",
      "cache-dependency-path": "clients/gnome/package-lock.json"
    }
  }
};
const ACTION_STEPS = {
  "Upload native setup evidence": {
    "name": "Upload native setup evidence",
    "if": "${{ always() }}",
    "uses": "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
    "with": {
      "name": "native-setup-${{ matrix.shard }}-${{ github.run_id }}-${{ github.run_attempt }}",
      "path": "native-setup-evidence/",
      "if-no-files-found": "error",
      "retention-days": 14
    }
  },
  "Go vulnerability audit": {
    "name": "Go vulnerability audit",
    "uses": "golang/govulncheck-action@032d45514ae346b1db93c04b0c90b841c370344f",
    "with": {
      "go-version-input": "${{ steps.go.outputs.go-version }}",
      "check-latest": false,
      "go-package": "./...",
      "repo-checkout": false,
      "cache": false
    }
  },
  "Gitleaks": {
    "name": "Gitleaks",
    "uses": "gitleaks/gitleaks-action@e0c47f4f8be36e29cdc102c57e68cb5cbf0e8d1e",
    "env": {
      "GITHUB_TOKEN": "${{ github.token }}",
      "GITLEAKS_ENABLE_UPLOAD_ARTIFACT": false
    }
  },
  "Trivy filesystem report": {
    "name": "Trivy filesystem report",
    "id": "trivy_report",
    "uses": "aquasecurity/trivy-action@ed142fd0673e97e23eac54620cfb913e5ce36c25",
    "with": {
      "scan-type": "fs",
      "scan-ref": ".",
      "format": "sarif",
      "output": "trivy-results.sarif",
      "severity": "HIGH,CRITICAL",
      "ignore-unfixed": true,
      "exit-code": "0"
    }
  },
  "Upload Trivy SARIF": {
    "name": "Upload Trivy SARIF",
    "if": "${{ !cancelled() && steps.trivy_report.outcome == 'success' && (github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) }}",
    "uses": "github/codeql-action/upload-sarif@2892aa5e19bbd11bc0cff5427e3b750a04d9e3c2",
    "with": {
      "sarif_file": "trivy-results.sarif",
      "category": ".github/workflows/ci.yml:fs"
    }
  },
  "Trivy high and critical gate": {
    "name": "Trivy high and critical gate",
    "uses": "aquasecurity/trivy-action@ed142fd0673e97e23eac54620cfb913e5ce36c25",
    "with": {
      "scan-type": "fs",
      "scan-ref": ".",
      "format": "table",
      "severity": "HIGH,CRITICAL",
      "ignore-unfixed": true,
      "exit-code": "1"
    }
  },
  "Install verified GoReleaser": {
    "name": "Install verified GoReleaser",
    "uses": "./.github/actions/setup-goreleaser"
  },
  "Upload qualified package payload": {
    "name": "Upload qualified package payload",
    "id": "payload",
    "uses": "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
    "with": {
      "name": "package-payload-${{ github.run_id }}-${{ github.run_attempt }}",
      "path": "dist/*.tar.gz\ndist/*.deb\ndist/checksums.txt\n",
      "if-no-files-found": "error",
      "compression-level": 0,
      "retention-days": 1
    }
  },
  "Download qualified package payload": {
    "name": "Download qualified package payload",
    "uses": "actions/download-artifact@37930b1c2abaa49bbe596cd826c3c89aef350131",
    "with": {
      "artifact-ids": "${{ needs.package.outputs.payload-id }}",
      "path": "dist/"
    }
  },
  "Upload CI-tested candidate artifacts": {
    "name": "Upload CI-tested candidate artifacts",
    "uses": "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
    "with": {
      "name": "candidate-${{ steps.candidate.outputs.commit }}-${{ github.run_id }}-${{ github.run_attempt }}",
      "path": "candidate-artifacts/",
      "if-no-files-found": "error",
      "compression-level": 0,
      "retention-days": 14
    }
  }
};

function inspectParallelCi(workflow, failures) {
  const jobs = workflow?.jobs || {};
  if (Object.hasOwn(workflow, 'env') || Object.hasOwn(workflow, 'defaults')) failures.push('Parallel CI changed workflow execution environment');
  const canonical = value => Array.isArray(value) ? value.map(canonical) :
    value && typeof value === 'object' ? Object.fromEntries(Object.entries(value).sort().map(([key, item]) => [key, canonical(item)])) : value;
  const same = (actual, expected) => JSON.stringify(canonical(actual)) === JSON.stringify(canonical(expected));
  const requireValue = (ok, label) => { if (!ok) failures.push(`Parallel CI lost ${label}`); };
  requireValue(same(Object.keys(jobs).sort(), [...REQUIRED_JOBS, 'checks'].sort()), 'required job set');
  for (const id of REQUIRED_JOBS) {
    const job = jobs[id];
    requireValue(job && !Object.hasOwn(job, 'if') && !Object.hasOwn(job, 'needs') &&
      !Object.hasOwn(job, 'continue-on-error'), `unconditional job ${id}`);
  }
  requireValue(same(jobs['docs-ste'], { uses: './.github/workflows/docs-ste.yml' }), 'required STE workflow');
  const aggregate = jobs.checks;
  requireValue(aggregate?.name === 'checks' && aggregate.if === '${{ always() }}' &&
    same(aggregate.needs, REQUIRED_JOBS), 'fail-closed aggregate dependencies');
  requireValue(same(jobs.native?.strategy, { 'fail-fast': false, matrix: { shard: [0, 1, 2, 3, 4, 5] } }), 'all six GTK shards');
  for (const [id, names] of Object.entries(STEP_JOBS)) {
    const steps = jobs[id]?.steps || [];
    const setup = ['harden', 'checkout'];
    if (id === 'go' || id === 'package') setup.push('go', 'node');
    if (id === 'frontend') setup.push('node');
    const sequence = [...setup.map(name => SETUP_STEPS[name]), ...names];
    requireValue(steps.length === sequence.length, `${id} complete step sequence`);
    for (const [index, expected] of sequence.entries()) {
      const step = steps[index];
      if (typeof expected !== 'string') {
        requireValue(sameActionSteps([step], [expected]), `${id} setup action ${index}`);
      } else if (ACTION_STEPS[expected]) {
        requireValue(sameActionSteps([step], [ACTION_STEPS[expected]]), `${id} action ${expected}`);
      } else {
        const metadata = step && Object.fromEntries(Object.entries(step).filter(([key]) => key !== 'run'));
        const options = { name: expected };
        if (expected === 'Verify snapshot archives') options.shell = 'bash';
        if (expected === 'Require every CI job to succeed') options.env = { CI_NEEDS: '${{ toJSON(needs) }}' };
        if (expected === 'Record qualified package attempt') options.id = 'qualification';
        if (expected === 'Prepare CI-tested candidate artifacts') {
          options.id = 'candidate';
          options.shell = 'bash';
          options.env = {
            PR_HEAD_SHA: '${{ github.event.pull_request.head.sha }}',
            PR_BASE_SHA: '${{ github.event.pull_request.base.sha }}',
            PACKAGE_QUALIFICATION_ATTEMPT: '${{ needs.package.outputs.qualified-attempt }}',
          };
        }
        // YAML key order does not affect semantics; compare the exact key/value set.
        const sorted = value => value && Object.fromEntries(Object.entries(value).sort());
        requireValue(same(sorted(metadata), sorted(options)) && typeof step?.run === 'string', `${id} run control ${expected}`);
      }
    }
    const allowed = ['name', 'runs-on', 'timeout-minutes', 'steps'];
    if (id === 'native') allowed.push('strategy');
    if (id === 'package') allowed.push('outputs');
    if (id === 'checks') allowed.push('if', 'needs');
    requireValue(jobs[id] && Object.keys(jobs[id]).every(key => allowed.includes(key)) &&
      jobs[id].name === id && jobs[id]['runs-on'] === 'ubuntu-latest', `${id} runner and execution settings`);
    for (const step of steps.filter(step => step.name)) {
      const expectedHash = QUALIFICATION_HASHES[step.name];
      if (expectedHash) {
        requireValue(createHash('sha256').update(step.run?.trim() || '').digest('hex') === expectedHash,
          `qualification script ${step.name}`);
      }
    }
  }
  const find = (id, name) => jobs[id]?.steps?.find(step => step.name === name);
  const gate = find('checks', 'Require every CI job to succeed');
  requireValue(gate?.run?.trim() === AGGREGATE_RUN && same(gate.env, { CI_NEEDS: '${{ toJSON(needs) }}' }), 'strict success-only result gate');
  requireValue(find('native', 'Native GTK setup, accessibility and screenshot matrix')?.run === SHARD_RUN, 'GTK shard command');
  requireValue(find('frontend', 'CI shard regressions')?.run === 'python3 -m unittest discover -s tests/desktop -p test_setup_native_shards.py', 'shard regression gate');
  requireValue(same(jobs.package?.outputs, { 'payload-id': '${{ steps.payload.outputs.artifact-id }}',
    'qualified-attempt': '${{ steps.qualification.outputs.attempt }}' }), 'package artifact output');
  const qualification = find('package', 'Record qualified package attempt');
  requireValue(qualification?.id === 'qualification' && qualification.run === 'echo "attempt=$GITHUB_RUN_ATTEMPT" >> "$GITHUB_OUTPUT"', 'original package qualification attempt');

}

function normalizeParallelCi(workflows, failures) {
  const path = '.github/workflows/ci.yml';
  const workflow = workflows[path];
  if (!workflow?.jobs) {
    failures.push('Parallel CI lost required job set');
    return workflows;
  }
  inspectParallelCi(workflow, failures);
  // Existing command checks still apply to every relocated gate, after graph checks.
  const steps = Object.values(workflow.jobs).flatMap(job => job.steps || []);
  return { ...workflows, [path]: { ...workflow, jobs: { checks: { steps } } } };
}

module.exports = { normalizeParallelCi, REQUIRED_JOBS, AGGREGATE_RUN, SHARD_RUN, STEP_JOBS };
