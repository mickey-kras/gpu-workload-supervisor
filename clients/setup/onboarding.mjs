export const applications = Object.freeze([
    {id: 'comfyui', label: 'ComfyUI'},
    {id: 'ollama', label: 'Ollama'},
    {id: 'llama.cpp', label: 'llama.cpp'},
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

// Guidance describes supported repairs, never authorizes them or changes the
// qualification verdict. Data paths and executable paths have different owners.
export function recoveryGuidance(candidate) {
    const diagnostics = candidateDiagnostics(candidate);
    const messages = diagnostics.length ? diagnostics : [candidate?.nextStep ?? ''];
    const steps = new Set();
    for (const message of messages) {
        const syntax = recoverySyntax(message);
        if (syntax) {
            steps.add(syntax);
        } else if (/Environment variable PATH: requires absolute clean root-owned executable directories/.test(message)) {
            steps.add('PATH: review each directory, including every alias target and ancestor. Use absolute protected root-owned executable directories; do not add user-writable directories. Preserve settings the application requires.');
        } else if (/Environment variable LD_LIBRARY_PATH: dynamic library search and transitive loader bindings are not qualified/.test(message)) {
            steps.add('Runtime libraries: use a reviewed root-owned runtime installation with its normal trusted loader configuration. Simply deleting LD_LIBRARY_PATH does not establish that its libraries are safe or that the application still works.');
        } else if (/WorkingDirectory requires an existing (?:absolute clean trusted directory without links|trusted account-home path without links)/.test(message)) {
            steps.add('Working directory: choose an existing absolute trusted directory. Inspect the directory and its ancestors for links, ownership and unintended group/world writes; preserve the application and its data.');
        } else if (dataPathFailure(message)) {
            steps.add('Application and data paths: inspect each reported setting and its ancestors. Use existing absolute paths without links, owned by this account or root and protected from other users’ writes. Keep directory settings as directories and configuration/token settings as regular files. Preserve models and application data; repair only the reported unsafe component or select another qualified path.');
        } else if (/(?:^|: )(?:executable|interpreter) trust:|(?:^|: )executable symlink changes application identity;/.test(message)) {
            steps.add('Runtime executable: ask an administrator to install a trusted package in a protected root-owned location, including its interpreter, libraries and executable ancestors. Choose that executable or review the external service command. Preserve the original application and models; setup never changes ownership or permissions.');
        }
    }
    if (steps.size) {
        if (candidate?.unit) steps.add('After reviewing changes to this external service, reload the user systemd manager.');
        steps.add('Choose Check again to inspect fresh evidence; configuration readiness does not prove GPU operation.');
    }
    return [...steps];
}

function recoverySyntax(message) {
    if (/Environment(?: variable [A-Za-z_]\w*)?: (?:expected NAME=value|expansion, escapes or whitespace|unterminated whole-assignment quote|quoted fragments|duplicate assignment)/.test(message)) {
        return 'Environment syntax: review the reported assignment. Use one literal NAME=value assignment per setting, with balanced quotes around the whole assignment when needed. Expansion, escapes, whitespace inside values and quoted fragments are unsupported; filesystem permission changes do not repair this syntax.';
    }
    if (/ComfyUI option --extra-model-paths-config configuration file count exceeds supported limit/.test(message)) {
        return 'Configuration-file count: review the launch arguments; each --extra-model-paths-config accepts at most 32 files. Preserve every required model mapping in a supported configuration before changing the command. The reported paths have not been inspected.';
    }
    if (/ComfyUI option --[a-z-]+ (?:is repeated;|requires an explicit value|requires an existing trusted absolute directory|requires at least one trusted configuration file)/.test(message)) {
        return 'Launch argument syntax: review the reported option. Supply its missing directory, file or value, or resolve repeated options into one unambiguous assignment. Preserve the intended application settings, then check again; changing file ownership does not repair argument syntax.';
    }
    return '';
}

function dataPathFailure(message) {
    const setting = /Environment variable (?:HOME|HF_HOME|HUGGINGFACE_HUB_CACHE|TRANSFORMERS_CACHE|XDG_CACHE_HOME|TMPDIR|HF_TOKEN_PATH):|ComfyUI option --(?:output-directory|temp-directory|user-directory|extra-model-paths-config)/.test(message);
    return (setting && /requires (?:an )?existing .*without links/.test(message)) || /application script (?:trust|component)|launch source component/.test(message);
}
