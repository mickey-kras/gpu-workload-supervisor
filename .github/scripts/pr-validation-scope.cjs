const FULL = 'full';
const DOCS = 'docs-only';

function documentationPath(path) {
  if (typeof path !== 'string' || path.includes('\\') || path.includes('\0') ||
      path.startsWith('/') || path.split('/').some(part => !part || part === '.' || part === '..')) {
    return false;
  }
  return path === 'README.md' || (path.startsWith('docs/') && path.endsWith('.md'));
}

function classifyFiles(files, count) {
  if (!Number.isInteger(count) || count < 1 || count > 3000 ||
      !Array.isArray(files) || files.length !== count ||
      files.some(file => !file || typeof file !== 'object' || Array.isArray(file)) ||
      new Set(files.map(file => file.filename)).size !== count) return FULL;
  for (const file of files) {
    if (!['added', 'modified', 'removed', 'renamed'].includes(file.status) ||
        !documentationPath(file.filename) ||
        !/^[a-f0-9]{40}$/.test(file.sha || '') ||
        !['additions', 'deletions', 'changes'].every(key =>
          Number.isInteger(file[key]) && file[key] >= 0) ||
        file.changes !== file.additions + file.deletions ||
        (file.status === 'removed' && file.additions !== 0) ||
        (file.changes > 0 && (typeof file.patch !== 'string' || !file.patch))) return FULL;
    if (file.status === 'renamed') {
      if (!documentationPath(file.previous_filename)) return FULL;
    } else if (file.previous_filename !== undefined) return FULL;
  }
  return DOCS;
}

function expectedPull(pull, snapshot, repository) {
  return pull.state === 'open' && pull.base?.ref === 'main' &&
    pull.base.repo?.full_name === repository &&
    pull.number === snapshot.number && pull.head?.sha === snapshot.head?.sha &&
    pull.base.sha === snapshot.base?.sha;
}

async function run({ github, context, core }) {
  let mode = FULL;
  let reason = 'non-PR work always receives full validation';
  try {
    const snapshot = context.payload?.pull_request;
    if (context.eventName === 'pull_request' && Number.isInteger(snapshot?.number) &&
        /^[a-f0-9]{40}$/.test(snapshot.head?.sha) && /^[a-f0-9]{40}$/.test(snapshot.base?.sha)) {
      const params = { ...context.repo, pull_number: snapshot.number };
      const repository = `${context.repo.owner}/${context.repo.repo}`;
      const { data: pull } = await github.rest.pulls.get(params);
      if (expectedPull(pull, snapshot, repository)) {
        const files = await github.paginate(github.rest.pulls.listFiles, { ...params, per_page: 100 });
        const { data: current } = await github.rest.pulls.get(params);
        if (expectedPull(current, snapshot, repository) && current.changed_files === pull.changed_files) {
          mode = classifyFiles(files, pull.changed_files);
          reason = mode === DOCS ? 'complete documentation-only diff' : 'paths or diff evidence require full validation';
        } else reason = 'PR identity or diff changed during classification';
      } else reason = 'PR identity differs from the event snapshot';
    } else if (context.eventName === 'pull_request') reason = 'PR event metadata is incomplete';
  } catch {
    reason = 'classification API or metadata was unavailable';
  }
  core.info(`Validation mode: ${mode} (${reason})`);
  core.setOutput('validation-mode', mode);
  return mode;
}

module.exports = { classifyFiles, run };
