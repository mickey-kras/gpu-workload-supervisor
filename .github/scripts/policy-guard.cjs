const REQUIRED = {
  '.github/workflows/pr-validation.yml': [
    'pull_request:', 'uses: ./.github/workflows/ci.yml',
    'uses: ./.github/workflows/aislop.yml',
    'uses: ./.github/workflows/codeql.yml',
    'uses: ./.github/workflows/branch-policy.yml',
    'uses: ./.github/workflows/dependency-review.yml',
  ],
  '.github/workflows/ci.yml': [
    'workflow_call:', 'go test -race', 'go mod tidy', 'go vet ./...',
    'govulncheck', 'gitleaks/gitleaks-action@', 'semgrep scan',
    'aquasecurity/trivy-action@', 'Trivy high and critical gate',
  ],
  '.github/workflows/main.yml': [
    'branches: [main]', 'uses: ./.github/workflows/ci.yml',
    'uses: ./.github/workflows/aislop.yml',
    'uses: ./.github/workflows/codeql.yml',
    'tailscale/github-action@', 'SonarSource/sonarqube-scan-action@',
    'SonarSource/sonarqube-quality-gate-action@',
  ],
  '.github/workflows/policy-guard.yml': [
    'pull_request_target:', 'github.workflow_sha', 'policy-guard.cjs',
  ],
  '.github/scripts/policy-guard.cjs': ['REQUIRED', 'inspect', 'async function run'],
  '.github/workflows/release.yml': [
    'refs/heads/main', 'listWorkflowRuns', 'heads/release/',
    'attest-build-provenance@', 'create-github-app-token@', 'gh release create',
  ],
  '.github/workflows/codeql.yml': ['languages: go', 'github/codeql-action/analyze@'],
  '.github/workflows/aislop.yml': ['npm ci --prefix .github/aislop', 'Aislop Go quality gate'],
  '.github/workflows/dependency-review.yml': ['actions/dependency-review-action@'],
  '.github/workflows/branch-policy.yml': ['WORK_BRANCH_RE:', "dependabot[bot]"],
  '.github/workflows/dependabot-auto-merge.yml': [
    'pull_request_target:', 'github.workflow_sha', 'metadata-action',
    'dependabot-auto-merge.cjs',
  ],
  '.github/workflows/dependabot-auto-merge-refresh.yml': [
    'schedule:', 'uses: ./.github/workflows/dependabot-auto-merge.yml',
  ],
  '.github/dependabot.yml': [
    'package-ecosystem: gomod', 'package-ecosystem: npm',
    'package-ecosystem: github-actions', 'rebase-strategy: auto',
  ],
  'sonar-project.properties': [
    'sonar.projectKey=gpu-workload-supervisor', 'sonar.go.coverage.reportPaths=coverage.out',
  ],
};

function inspect(files) {
  const failures = [];
  for (const [path, terms] of Object.entries(REQUIRED)) {
    const content = files[path];
    if (typeof content !== 'string') {
      failures.push(`Missing required file: ${path}`);
      continue;
    }
    for (const term of terms) {
      if (!content.includes(term)) failures.push(`${path} lost required control: ${term}`);
    }
  }
  for (const [path, content] of Object.entries(files)) {
    if (!path.startsWith('.github/workflows/') || !/\.ya?ml$/.test(path)) continue;
    for (const line of content.split('\n')) {
      const match = line.match(/^\s*(?:-\s*)?uses:\s*([^#\s]+)/);
      if (!match || match[1].startsWith('./') || match[1].startsWith('docker://')) continue;
      if (!/^[\w.-]+(?:\/[\w.-]+)+@[a-f0-9]{40}$/.test(match[1])) {
        failures.push(`${path} has an unpinned action: ${match[1]}`);
      }
    }
  }
  return failures;
}

async function run({ github, context }) {
  const { owner, repo } = context.repo;
  const sha = context.payload.pull_request.head.sha;
  const { data: commit } = await github.rest.git.getCommit({ owner, repo, commit_sha: sha });
  const { data: tree } = await github.rest.git.getTree({
    owner, repo, tree_sha: commit.tree.sha, recursive: 'true',
  });
  if (tree.truncated) throw new Error('Cannot verify a truncated PR tree');
  const paths = tree.tree.filter(entry => entry.type === 'blob' &&
    (Object.hasOwn(REQUIRED, entry.path) ||
      (entry.path.startsWith('.github/workflows/') && /\.ya?ml$/.test(entry.path))))
    .map(entry => entry.path);
  const files = {};
  for (const path of paths) {
    const { data } = await github.rest.repos.getContent({ owner, repo, path, ref: sha });
    if (!('content' in data)) throw new Error(`Unexpected directory: ${path}`);
    files[path] = Buffer.from(data.content, data.encoding || 'base64').toString('utf8');
  }
  const failures = inspect(files);
  if (failures.length) throw new Error(failures.join('\n'));
}

module.exports = { inspect, run };
