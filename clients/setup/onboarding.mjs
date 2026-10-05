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
    get ready() { return false; }
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

export function candidateMessage(candidate) {
    const states = {
        'not-running': 'Not running. Start the application through your existing controls, then refresh.',
        unreachable: 'Unable to reach this application. Check its address and refresh.',
        missing: 'Application or selected path is missing. Choose another location.',
        unsupported: 'This setup is not supported. Keep it as a draft or choose another instance.',
        invalid: 'Configuration could not be read. Check the selected address or location.',
        candidate: 'Application found. Its model inventory and lifecycle are not verified.',
        available: 'Application responded. Safe model lifecycle control is not verified.',
    };
    return states[candidate?.instanceStatus] ?? 'Choose an application instance or provide its location.';
}
