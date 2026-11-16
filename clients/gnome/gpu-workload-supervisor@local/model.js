import { compareVersions, CLOSE_ACTION, GPU_CODES } from './contract.js';
const FRESH_MS = 30000;
const REFUSALS = new Set([...GPU_CODES, 'not_permitted']);
export class Model {
    generation = 1;
    sequence = 0;
    status = null;
    pending = null;
    error = 'unavailable';
    dispatch = 0;
    delay = 5000;
    // The last start that the GPU check refused: its target, and the
    // processes on the GPU or the failure of the check.
    blocked = null;
    retire() {
        this.generation++;
        this.pending = null;
        this.error = 'unavailable';
        this.blocked = null;
    }
    view(now) {
        const fresh =
            this.status && !this.error && now - this.dispatch < FRESH_MS;
        return {
            status: this.status,
            checked: this.status?.owner === 'user',
            pending: !!this.pending,
            error: this.error,
            fresh: !!fresh,
            age: this.status ? Math.max(0, now - this.dispatch) : null,
            blocked: this.blocked,
            mutable:
                !!fresh &&
                !this.pending &&
                this.status.phase === 'stable' &&
                this.status.health === 'healthy',
        };
    }
    intent(action, target, now) {
        const v = this.view(now);
        if (!v.mutable) return null;
        const s = this.status;
        const allowed = this._allowsAction(action, target);
        if (!allowed) return null;
        return {
            action,
            target,
            expected: { ...s.expected },
            confirmation: action !== 'user-switch',
        };
    }
    _allowsAction(action, target) {
        const s = this.status;
        if (action === 'take-control')
            return s.owner === 'supervisor' && s.capabilities.takeControl;
        if (action === 'return-control')
            return s.owner === 'user' && s.capabilities.returnControl;
        return (
            action === 'user-switch' &&
            s.owner === 'user' &&
            s.capabilities.userSwitch &&
            s.workloads.some((w) => w.id === target) &&
            s.activeWorkload !== target
        );
    }
    validDecision(d, now) {
        return (
            !!d &&
            !!this.intent(d.action, d.target, now) &&
            JSON.stringify(d.expected) === JSON.stringify(this.status.expected)
        );
    }
    // Close is offered only for a listed process of the desktop user.
    closable(pid) {
        return !!this.blocked?.processes.some(
            (p) => p.pid === pid && p.closable,
        );
    }
    closeIntent(pid) {
        if (this.pending || !this.closable(pid)) return null;
        return { action: CLOSE_ACTION, pid, confirmation: true };
    }
    retryIntent(now) {
        if (!this.blocked) return null;
        return this.intent('user-switch', this.blocked.target, now);
    }
    begin(action, now, decision = null) {
        if (this.pending) return null;
        if (action === CLOSE_ACTION) return this._beginClose(now, decision);
        if (
            action !== 'status' &&
            (!decision ||
                decision.action !== action ||
                !this.validDecision(decision, now))
        )
            return null;
        const request = this._request(action);
        if (action !== 'status') {
            request.expected = { ...this.status.expected };
            if (action === 'user-switch') request.target = decision.target;
        }
        return this._dispatch(request, now);
    }
    _beginClose(now, d) {
        if (d?.action !== CLOSE_ACTION || !this.closable(d.pid)) return null;
        const request = this._request(CLOSE_ACTION);
        request.target = this.blocked.target;
        request.pid = d.pid;
        return this._dispatch(request, now);
    }
    _request(action) {
        return {
            protocolVersion: 1,
            requestId: `g${this.generation}s${++this.sequence}`,
            action,
        };
    }
    _dispatch(request, now) {
        const call = {
            generation: this.generation,
            sequence: this.sequence,
            dispatch: now,
            request,
        };
        this.pending = call;
        return call;
    }
    current(c) {
        return c.generation === this.generation && this.pending === c;
    }
    accept(c, r, now) {
        if (!this.current(c)) return false;
        this._trackGPU(c.request, r);
        if (r.code !== 'ok') {
            this.fail(c, r.code);
            // A refused start changed nothing: refresh the status soon, so
            // Retry is offered again without the error backoff.
            if (REFUSALS.has(r.code)) this.delay = 1000;
            return false;
        }
        const s = r.status;
        if (
            this.status?.expected.incarnation === s.expected.incarnation &&
            compareVersions(s.expected.version, this.status.expected.version) <
                0
        ) {
            this.fail(c, 'stale_state');
            return false;
        }
        this.pending = null;
        this.status = s;
        this.dispatch = c.dispatch;
        this.error = now - c.dispatch >= FRESH_MS ? 'stale_state' : null;
        this.delay = 5000;
        return true;
    }
    _trackGPU(request, r) {
        if (GPU_CODES.includes(r.code)) {
            this.blocked = {
                target: request.target,
                processes: r.gpuCheck.processes ?? [],
                failure: r.gpuCheck.failure ?? null,
            };
        } else if (r.code === 'ok' && request.action === CLOSE_ACTION) {
            // The GPU is free now; the start still needs Retry.
            this.blocked = { ...this.blocked, processes: [], failure: null };
        } else if (r.code === 'ok' && request.action === 'user-switch') {
            this.blocked = null;
        }
    }
    fail(c, code = 'unavailable') {
        if (!this.current(c)) return;
        this.pending = null;
        this.error = code;
        this.delay = Math.min(60000, this.delay * 2);
    }
}
