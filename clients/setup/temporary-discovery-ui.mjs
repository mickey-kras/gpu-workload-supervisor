// Own the consented temporary check, its helper cancellation and durable recovery.
export class TemporaryDiscovery {
    constructor({Gtk, draft, app, modelGroup, status, temporaryStatus, command, changed, reportError, show, getEvidence}) {
        this.Gtk = Gtk;
        this.draft = draft;
        this.app = app;
        this.modelGroup = modelGroup;
        this.status = status;
        this.temporaryStatus = temporaryStatus;
        this.command = command;
        this.changed = changed;
        this.reportError = reportError;
        this.show = show;
        this.getEvidence = getEvidence;
        this.temporaryPromise = null; this.temporarySession = null;
        this.temporaryOperation = {};
        this.temporaryConsent = new this.Gtk.CheckButton({label: 'I allow this brief start and will keep other application controls paused.', visible: false});
        this.temporaryStart = new this.Gtk.Button({label: 'Start Ollama briefly to list models', visible: false, sensitive: false});
        this.temporaryCancel = new this.Gtk.Button({label: 'Cancel model detection', visible: false});
        this.temporaryExplanation = new this.Gtk.Label({label: 'Listing models requires a brief start only if you cannot provide an existing model name. This may use GPU memory. Do not start applications or use their external controls during this check. Setup restores the previous stopped state and reports any cleanup failure.', wrap: true, xalign: 0, visible: false});
        if (this.app === 'ollama') {
            this.modelGroup.add(this.temporaryExplanation); this.modelGroup.add(this.temporaryConsent); this.modelGroup.add(this.temporaryStart); this.modelGroup.add(this.temporaryCancel);
        }
        this.temporaryConsent.connect('toggled', () => { this.temporaryStart.sensitive = this.temporaryConsent.active && !this.temporaryPromise; });
        this.temporaryCancel.connect('clicked', () => this.cancelDetection());
        this.temporaryStart.connect('clicked', () => this.start());
    }
    async refreshTemporaryStatus() {
        Object.assign(this.temporaryStatus, {session: this.temporarySession, available: false, expected: undefined});
        try {
            const refreshed = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'temporary-status']));
            Object.assign(this.temporaryStatus, refreshed, {session: refreshed.session ?? this.temporarySession});
        } catch (error) { this.reportError('The stopped state was restored, but model-check status could not be refreshed. Reopen setup before another temporary check.', error); }
    }
    async cleanupTemporary() {
        if (this.temporaryPromise) { this.temporaryOperation.cancel?.(); await this.temporaryPromise; }
        if (!this.temporarySession || this.temporarySession.status === 'completed') return;
        if (!this.temporarySession.id || !this.temporarySession.token) throw new Error('The current temporary check could not be identified. Reopen setup to read its durable recovery record.');
        try {
            const result = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'temporary-cleanup'], JSON.stringify({id: this.temporarySession.id, token: this.temporarySession.token, externalControlPaused: true})));
            this.temporarySession = result.session;
            if (result.error || this.temporarySession?.status !== 'completed') throw new Error(result.error || 'Temporary application cleanup needs attention.');
            await this.refreshTemporaryStatus();
        } catch (error) { this.reportError('Ollama cleanup needs attention. Keep external controls paused and retry cleanup before leaving setup.', error); throw error; }
    }
    async cancelDetection() {
        this.draft.cancel(); try { await this.cleanupTemporary(); this.status.label = 'Model detection cancelled. Previous stopped state restored.'; } catch (error) { this.reportError('Cancellation needs cleanup. Keep external controls paused and retry.', error); }
    }
    async start() {
        if (this.temporarySession && this.temporarySession.status !== 'completed' && !this.temporaryPromise) { try { await this.cleanupTemporary(); this.temporaryStart.label = 'Start Ollama briefly to list models'; this.temporaryStart.sensitive = false; } catch (error) { this.reportError('Temporary cleanup needs attention. Reopen setup if its current record cannot be read.', error); } return; }
        if (!this.temporaryConsent.active || !this.temporaryStatus?.available || this.temporaryPromise) return;
        this.temporarySession = null;
        const input = this.draft.snapshot(); const generation = this.draft.generation;
        this.temporaryStart.sensitive = false; this.temporaryCancel.visible = true; this.changed(input);
        this.temporaryPromise = (async () => {
            try {
                const result = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'temporary-discover'], JSON.stringify({unit: input.binding?.unit, expected: this.temporaryStatus.expected, consent: true, externalControlPaused: true}), this.temporaryOperation));
                this.temporarySession = result.session;
                if (result.error || this.temporarySession?.status !== 'completed') throw new Error(result.error || 'The temporary application check did not finish cleanup.');
                await this.refreshTemporaryStatus();
                if (generation === this.draft.generation) this.show({...this.getEvidence(), models: result.models, inventoryStatus: 'available'});
            } catch (error) {
                try {
                    const recovery = JSON.parse(await this.command(['/usr/bin/gpu-setup', 'temporary-status']));
                    Object.assign(this.temporaryStatus, recovery); this.temporarySession = recovery.session ?? this.temporarySession;
                } catch (statusError) {
                    // Uncertain helper state must remain blocked until status can be checked.
                    this.temporarySession = {status: 'cleanup_required'};
                    Object.assign(this.temporaryStatus, {session: this.temporarySession, available: false, expected: undefined});
                    this.reportError('Temporary check status could not be read. Reopen setup to recover its recorded stopped state.', statusError);
                }
                this.reportError(this.temporarySession && !this.temporarySession.id ? 'Model check status is unknown. Setup remains blocked. Reopen setup to recover its recorded stopped state; leaving setup preserves this block.' : 'Model detection needs attention. Check the previous stopped state before continuing. Retry cleanup if required, or provide an existing model name.', error);
            }
        })();
        await this.temporaryPromise; this.temporaryPromise = null; this.temporaryCancel.visible = false;
        this.temporaryStart.label = this.temporarySession && this.temporarySession.status !== 'completed' ? 'Retry Ollama cleanup' : 'Start Ollama briefly to list models';
        this.temporaryConsent.active = false;
        if (this.temporarySession && this.temporarySession.status !== 'completed') {
            this.temporaryStart.sensitive = Boolean(this.temporarySession.id && this.temporarySession.token);
            // A retry reuses this same signal handler and the durable session token.
        }
    }
    showAvailability(candidate) {
        const canStart = this.app === 'ollama' && candidate.recognized && candidate.instanceStatus === 'not-running' && candidate.configurationStatus === 'model-required' && !(candidate.models?.length) && this.temporaryStatus?.available === true;
        this.temporaryExplanation.visible = canStart; this.temporaryConsent.visible = canStart; this.temporaryStart.visible = canStart;
    }
    cancel() {
        if (!this.temporaryPromise && this.temporarySession && !this.temporarySession.id) {
            this.reportError('Temporary check status is unknown. Leaving setup preserves its durable recovery block; reopen setup to check it.', new Error('No current session identity is available for safe cleanup.'));
            return Promise.resolve();
        }
        return this.cleanupTemporary();
    }
    cleanup() {
        return this.cleanupTemporary();
    }
    active() {
        return Boolean(this.temporaryPromise || (this.temporarySession?.id && this.temporarySession.status !== 'completed'));
    }
    blocked() {
        return Boolean(this.temporaryPromise || (this.temporarySession && this.temporarySession.status !== 'completed'));
    }
}
