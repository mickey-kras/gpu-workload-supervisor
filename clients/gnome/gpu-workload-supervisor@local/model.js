import { compareVersions } from './contract.js';
const FRESH_MS = 30000;
export class Model {
    constructor() {
        this.generation = 1;
        this.sequence = 0;
        this.status = null;
        this.pending = null;
        this.error = 'unavailable';
        this.dispatch = 0;
        this.delay = 5000;
    }
    retire() {
        this.generation++;
        this.pending = null;
        this.error = 'unavailable';
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
        const allowed =
            action === 'take-control'
                ? s.owner === 'supervisor' && s.capabilities.takeControl
                : action === 'return-control'
                  ? s.owner === 'user' && s.capabilities.returnControl
                  : action === 'user-switch' &&
                    s.owner === 'user' &&
                    s.capabilities.userSwitch &&
                    s.workloads.some((w) => w.id === target) &&
                    s.activeWorkload !== target;
        if (!allowed) return null;
        return {
            action,
            target,
            expected: { ...s.expected },
            confirmation: action !== 'user-switch',
        };
    }
    validDecision(d, now) {
        return (
            !!d &&
            !!this.intent(d.action, d.target, now) &&
            JSON.stringify(d.expected) === JSON.stringify(this.status.expected)
        );
    }
    begin(action, now, decision = null) {
        if (this.pending) return null;
        if (
            action !== 'status' &&
            (!decision ||
                decision.action !== action ||
                !this.validDecision(decision, now))
        )
            return null;
        const request = {
            protocolVersion: 1,
            requestId: `g${this.generation}s${++this.sequence}`,
            action,
        };
        if (action !== 'status') {
            request.expected = { ...this.status.expected };
            if (action === 'user-switch') request.target = decision.target;
        }
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
        if (r.code !== 'ok') {
            this.fail(c, r.code);
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
    fail(c, code = 'unavailable') {
        if (!this.current(c)) return;
        this.pending = null;
        this.error = code;
        this.delay = Math.min(60000, this.delay * 2);
    }
}
