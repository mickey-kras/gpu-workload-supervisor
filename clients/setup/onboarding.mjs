export const applications = Object.freeze([
    {id: 'comfyui', label: 'ComfyUI'},
    {id: 'vllm', label: 'vLLM'},
]);

// Discovery is evidence for an editable draft, never permission to start work.
export class ApplicationDraft {
    generation = 0;
    candidate = null;
    constructor(app) {
        if (!applications.some(choice => choice.id === app))
            throw new Error('Choose a supported application.');
        this.input = {app};
    }
    get needsModel() { return this.input.app !== 'comfyui'; }
    edit(values) {
        this.input = {...this.input, ...values};
        this.cancel();
    }
    reference(reference, referenceKind) {
        delete this.input.endpoint;
        this.edit({reference, referenceKind});
    }
    endpoint(endpoint) {
        delete this.input.reference;
        delete this.input.referenceKind;
        this.edit({endpoint});
    }
    cancel() { this.generation++; this.candidate = null; }
    begin() {
        const {app, endpoint, reference, referenceKind} = this.input;
        const request = {app};
        if (reference) Object.assign(request, {reference, referenceKind});
        else if (endpoint) request.endpoint = endpoint;
        return {generation: ++this.generation, request};
    }
    accept(probe, candidate) {
        if (probe.generation !== this.generation) return false;
        this.candidate = candidate;
        return true;
    }
    snapshot() { return {...this.input}; }
}

// Slugs mirror the backend's ValidWorkloadID rule; null means keep the draft UUID.
// Trimming is index-based (plain scans and slices) so derivation stays linear and
// carries no backtracking regexes (SonarQube javascript:S5852).
export function profileIDFromModel(app, model) {
    if (!model) return null;
    let slug = `${app}-${model}`.toLowerCase().replaceAll(/[^a-z0-9_]+/g, '-');
    // Strip the leading run outside [a-z].
    let start = 0;
    while (start < slug.length && (slug[start] < 'a' || slug[start] > 'z')) start++;
    // Strip trailing dashes before and after the 64-character cut.
    let end = slug.length;
    while (end > start && slug[end - 1] === '-') end--;
    slug = slug.slice(start, end).slice(0, 64);
    end = slug.length;
    while (end > 0 && slug[end - 1] === '-') end--;
    slug = slug.slice(0, end);
    return /^[a-z][a-z0-9_-]{0,63}$/.test(slug) && slug !== 'idle' && slug !== 'unknown' ? slug : null;
}

export function candidateDiagnostics(candidate) {
    return [...new Set((Array.isArray(candidate?.diagnostics) ? candidate.diagnostics : [])
        .filter(value => typeof value === 'string' && value.trim()))];
}

export function candidateMessage(candidate) {
    const diagnostics = candidateDiagnostics(candidate);
    if (diagnostics.length > 1) return 'Several configuration checks need attention. Review the reported problems, then check again.';
    if (candidate?.configurationStatus === 'gpu-unresolved') return 'GPU identity needs setup. Review this service’s GPU visibility settings before choosing a full GPU UUID, reloading the service configuration, and checking again.';
    if (candidate?.installed && !candidate?.recognized) return 'Installation found, but qualification is blocked. Review the blockers and supported correction path.';
    if (candidate?.configurationStatus === 'inspection-failed') return 'Installation inspection failed. Check its service configuration or choose an installed executable.';
    if (candidate?.configurationStatus === 'model-missing') return 'Configured model is missing. Choose an existing model or repair the service configuration.';
    const states = {
        'not-running': stoppedCandidateMessage(candidate),
        ambiguous: 'More than one installation matches. Choose the installation you want to control.',
        'discovery-error': 'Discovery failed. Retry to check this installation.',
        'inspection-failed': 'Installation inspection failed. Check its service configuration or choose an installed executable.',
        installed: 'Installed executable found. Choose an existing model to preview a Supervisor-managed launch.',
        unreachable: candidate?.recognized ? 'Installation recognized. Its address is unreachable; configuration can be checked without starting it.' : 'Address unreachable. An address alone does not verify an installed application. Choose a detected installation or its executable to preview a managed launch.',
        missing: candidate?.referenceKind?.startsWith('model-') ? 'Selected model is missing. Choose another model.' : 'Application location is missing. Choose its installed location.',
        unsupported: 'This setup is not supported. Save the selection for later or choose another instance.',
        invalid: 'Configuration could not be read. Check the selected address or location.',
        candidate: 'Application found. Choose its installation to check start and stop controls.',
        available: 'Application responded. Its start and stop controls still need a check.',
    };
    if (candidateReady(candidate) && candidate.instanceStatus !== 'not-running') return candidate.instanceStatus === 'unreachable' ? states.unreachable : 'Installation recognized. Ready to configure.';
    return states[candidate?.instanceStatus] ?? 'Choose an application instance or provide its location.';
}

export function candidateReady(candidate) {
    return Boolean(candidate?.recognized && candidate.binding && ['ready', 'model-required'].includes(candidate.configurationStatus) &&
        !['invalid', 'missing', 'unsupported', 'inspection-failed', 'discovery-error', 'ambiguous'].includes(candidate.instanceStatus));
}

function stoppedCandidateMessage(candidate) {
    if (candidate?.configurationStatus === 'ready' || candidate?.configurationStatus === 'model-required') return 'Installed and stopped. Ready to configure.';
    if (candidate?.unit) return 'Service found and stopped. Configuration remains unqualified; inspect its blockers or choose the installed executable.';
    return 'Default address is not responding. Installation is unverified; choose its installed location.';
}

// A service and an endpoint are separate candidates unless discovery established
// their binding. Keep their complete identities available even when GTK elides a row.
export function candidateIdentity(candidate) {
    let kind = 'Location';
    if (candidate.unit) kind = 'Service';
    else if (candidate.sourceKind === 'owned' || candidate.referenceKind === 'application') kind = 'Executable';
    else if (candidate.endpoint) kind = 'Address';
    else if (candidate.referenceKind === 'configuration') kind = 'Configuration';
    const identities = [candidate.unit, candidate.location, candidate.reference, candidate.endpoint].filter(Boolean);
    return `${kind}: ${[...new Set(identities)].join(' · ') || candidate.id || candidate.label}`;
}

export function candidateChoice(candidate) {
    const status = {'inspection-failed': 'inspection failed', 'model-missing': 'model missing', 'model-required': 'model needed'}[candidate.configurationStatus] ?? (candidate.instanceStatus === 'not-running' && candidate.unit ? 'stopped' : candidate.instanceStatus);
    const statusSuffix = status ? ` · ${status}` : '';
    return `${candidate.label} · ${candidateIdentity(candidate)}${statusSuffix}`;
}

// Shareable diagnostics contain only allowlisted states/codes, never local identities.
export function diagnosticReport(candidates, errors = []) {
    const states = new Set(['candidate', 'available', 'installed', 'not-running', 'unreachable', 'missing', 'unsupported', 'invalid', 'inspection-failed', 'discovery-error', 'ambiguous']);
    const configuration = new Set(['ready', 'model-required', 'model-missing', 'gpu-unresolved', 'inspection-failed', 'unsupported']);
    const report = (Array.isArray(candidates) ? candidates : []).slice(0, 256).filter(item => applications.some(app => app.id === item?.app)).map(item => ({
        application: item.app, installed: Boolean(item.installed || item.recognized), qualified: candidateReady(item),
        state: states.has(item.instanceStatus) ? item.instanceStatus : 'unknown',
        configuration: configuration.has(item.configurationStatus) ? item.configurationStatus : 'unknown',
        blockers: candidateDiagnostics(item).slice(0, 32).map(blockerCode),
    }));
    return JSON.stringify({version: 1, installations: report, discoveryErrorCount: Math.min(Array.isArray(errors) ? errors.length : 0, 256), truncated: (candidates?.length ?? 0) > 256}, null, 2);
}
function blockerCode(message) {
    if (/CUDA_VISIBLE_DEVICES|GPU identity|GPU UUID/.test(message)) return 'gpu-identity';
    if (/\bRestart\b/.test(message)) return 'restart-policy';
    if (/WorkingDirectory|working directory/.test(message)) return 'working-directory';
    if (/Environment|environment variable/.test(message)) return 'environment';
    if (/executable|ownership|owner|writable|trusted|symlink|symbolic link/i.test(message)) return 'execution-trust';
    if (/model/.test(message)) return 'model';
    if (/loaded|reload|drop-in/.test(message)) return 'loaded-configuration';
    return 'launch-configuration';
}

export function correctionPreview(candidate, uuid = '') {
    const diagnostics = candidateDiagnostics(candidate).join('\n');
    const gpu = candidate?.configurationStatus === 'gpu-unresolved' || /CUDA_VISIBLE_DEVICES|GPU identity/.test(diagnostics);
    const restart = /\bRestart\b/.test(diagnostics);
    const validUUID = /^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(uuid);
    if (!candidate?.unit || (!gpu && !restart) || (gpu && !validUUID)) return '';
    return ['[Service]', ...(gpu ? [`Environment="CUDA_VISIBLE_DEVICES=${uuid}"`] : []), ...(restart ? ['Restart=no'] : [])].join('\n') + '\n';
}

// Guidance is one line per problem class. Diagnostics already name the path.
export function recoveryGuidance(candidate) {
    const reported = candidateDiagnostics(candidate);
    const diagnostics = reported.length ? reported : [candidate?.nextStep ?? ''];
    const steps = new Set();
    for (const message of diagnostics) {
        const syntax = recoverySyntax(message);
        if (syntax) {
            steps.add(syntax);
        } else if (/group-writable by gid \d+, a group you are not in/.test(message)) {
            steps.add('Fix the named path: choose a path protected from write access by groups you do not belong to. Group write is allowed for your own groups; setup never changes permissions.');
        } else if (/world-writable|owned by uid|missing or broken link|parent directory unavailable|not a directory|not a (?:readable )?regular file|not an executable|path must be absolute and clean|symlink resolution exceeds/.test(message)) {
            steps.add('Fix the named path: exists, owned by you or root, not world-writable. Setup never changes permissions.');
        } else if (legacyPathFailure(message)) {
            steps.add('Review the reported path and its ancestors: use an existing trusted installation or data path. Check ownership, permissions and link targets; setup changes no files.');
        }
    }
    if (steps.size) {
        if (candidate?.unit) steps.add('After editing the unit: systemctl --user daemon-reload.');
        steps.add('Check again re-inspects. Ready does not prove GPU operation.');
    }
    return [...steps];
}

function recoverySyntax(message) {
    if (/Environment(?: variable [A-Za-z_]\w*)?: (?:expected NAME=value|expansion, escapes or whitespace|unterminated whole-assignment quote|quoted fragments|duplicate assignment)/.test(message)) {
        return 'Environment syntax: one literal NAME=value per assignment; quote the whole assignment, for example Environment="HOME=/home/alice".';
    }
    if (/Environment variable (?:PATH|LD_LIBRARY_PATH|PYTHONPATH): empty search path components/.test(message)) {
        return 'Search paths: use existing trusted absolute directories with no empty entries; an empty entry can select the current directory.';
    }
    if (/Environment variable (?:LD_PRELOAD|LD_AUDIT|PYTHONHOME|PYTHONUSERBASE|PYTHONSTARTUP): nonempty code-loading overrides/.test(message)) {
        return 'Code-loading environment: this nonempty override is unsupported. Review the runtime configuration before choosing a supported direct launch.';
    }
    if (/Environment variable LD_LIBRARY_PATH: dynamic library search and transitive loader bindings are not qualified/.test(message)) {
        return 'Library search: review the runtime and its configured directories. Do not remove a required loader setting merely to pass setup.';
    }
    if (/ComfyUI option --extra-model-paths-config configuration file count exceeds supported limit/.test(message)) {
        return 'Configuration-file count: at most 32 files per --extra-model-paths-config.';
    }
    if (/ComfyUI option --[a-z-]+ (?:is repeated;|requires an explicit value|requires an existing trusted absolute directory|requires at least one trusted configuration file|requires its supported full option name and value)/.test(message)) {
        return 'Launch argument syntax: fix the reported option (missing value, repeated flag, missing path).';
    }
    return '';
}

function legacyPathFailure(message) {
    return /Environment variable [A-Za-z_]\w*: requires (?:an existing absolute clean trusted path without links|absolute clean root-owned executable directories)/.test(message) ||
        /(?:WorkingDirectory|ComfyUI option --[a-z-]+) requires (?:an existing (?:absolute clean trusted directory|trusted account-home path)|existing absolute clean trusted regular configuration files) without links/.test(message) ||
        /application script trust:.*launch source component \d+: (?:ownership is outside root or the desktop account|group or world write permission permits replacement|source is not a readable regular file)/.test(message) ||
        /(?:executable|interpreter|application script)(?: trust| target| ancestor| path| alias| component|:).*?(?:not root-owned|group or world writable|group\/world writable|cannot be opened without following links|target unavailable|alias ancestry|alias resolution|alias target unavailable)/.test(message);
}
