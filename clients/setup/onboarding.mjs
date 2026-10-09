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

export function candidateMessage(candidate) {
    if (candidate?.configurationStatus === 'inspection-failed') return 'Installation inspection failed. Check its service configuration or choose an installed executable.';
    if (candidate?.configurationStatus === 'model-missing') return 'Configured model is missing. Choose an existing model or repair the service configuration.';
    const states = {
        'not-running': stoppedCandidateMessage(candidate),
        ambiguous: 'More than one installation matches. Choose the installation you want to control.',
        'discovery-error': 'Discovery failed. Retry to check this installation.',
        'inspection-failed': 'Installation inspection failed. Check its service configuration or choose an installed executable.',
        installed: 'Installed executable found. Choose an existing model to preview a Supervisor-managed launch.',
        unreachable: 'Address unreachable. An address alone does not verify an installed application. Check its address or choose its executable.',
        missing: candidate?.referenceKind?.startsWith('model-') ? 'Selected model is missing. Choose another model.' : 'Application location is missing. Choose its installed location.',
        unsupported: 'This setup is not supported. Save the selection for later or choose another instance.',
        invalid: 'Configuration could not be read. Check the selected address or location.',
        candidate: 'Application found. Choose its installation to check start and stop controls.',
        available: 'Application responded. Its start and stop controls still need a check.',
    };
    if (candidate?.configurationStatus === 'ready' && candidate.instanceStatus !== 'not-running') return 'Installation recognized. Ready to configure.';
    return states[candidate?.instanceStatus] ?? 'Choose an application instance or provide its location.';
}

function stoppedCandidateMessage(candidate) {
    if (candidate?.configurationStatus === 'ready' || candidate?.configurationStatus === 'model-required') return 'Installed and stopped. Ready to configure.';
    if (candidate?.unit) return 'Installed and stopped. More configuration evidence is needed; choose its location or inspect Advanced settings.';
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
