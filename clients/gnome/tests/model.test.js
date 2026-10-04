import test from 'node:test';
import assert from 'node:assert/strict';
import { parseResponse } from '../gpu-workload-supervisor@local/contract.js';
import { Model } from '../gpu-workload-supervisor@local/model.js';
export const fixture = (
    owner = 'supervisor',
    version = '9007199254740993',
) => ({
    protocolVersion: 1,
    requestId: 'r1',
    code: 'ok',
    status: {
        owner,
        desiredWorkload: 'render',
        activeWorkload: 'render',
        phase: 'stable',
        health: 'healthy',
        admission: 'closed',
        observedAt: '2026-10-04T00:00:00Z',
        expected: {
            incarnation: 'a',
            version,
            owner,
            configurationRevision: 'c1',
        },
        workloads: [
            { id: 'idle', label: 'Idle' },
            { id: 'render', label: 'Rendering' },
        ],
        capabilities: {
            takeControl: owner === 'supervisor',
            userSwitch: owner === 'user',
            returnControl: owner === 'user',
        },
    },
});
test('strict envelope and lossless token', () => {
    assert.equal(
        parseResponse(JSON.stringify(fixture()), 'r1').status.expected.version,
        '9007199254740993',
    );
    for (const mutate of [
        (r) => (r.extra = true),
        (r) => (r.status.expected.version = 4),
        (r) => (r.status.expected.version = '18446744073709551616'),
        (r) => (r.status.workloads[1].label = 'bad\nlabel'),
        (r) => (r.code = 'busy'),
        (r) => (r.requestId = 'wrong'),
    ]) {
        const r = fixture();
        mutate(r);
        assert.throws(() => parseResponse(JSON.stringify(r), 'r1'));
    }
});
test('committed ownership only; ownership dialogs and direct workload selection', () => {
    const m = new Model();
    const call = m.begin('status', 0);
    m.accept(call, fixture(), 1);
    assert.equal(m.view(1).checked, false);
    assert.equal(m.intent('user-switch', 'idle', 1), null);
    assert.equal(m.intent('take-control', null, 1).confirmation, true);
    const c = m.begin('take-control', 2, m.intent('take-control', null, 2));
    assert.equal(m.view(2).checked, false);
    assert.equal(m.begin('status', 2), null);
    m.accept(c, { ...fixture('user'), requestId: c.request.requestId }, 3);
    assert.equal(m.view(3).checked, true);
    assert.equal(m.intent('user-switch', 'render', 3), null);
    assert.equal(m.intent('user-switch', 'idle', 3).confirmation, false);
    assert.equal(m.intent('return-control', null, 3).confirmation, true);
});
test('dispatch freshness, errors, pending and backoff fail closed', () => {
    const m = new Model();
    let c = m.begin('status', 0);
    m.accept(c, fixture(), 31000);
    assert.equal(m.view(31000).mutable, false);
    c = m.begin('status', 32000);
    m.accept(c, fixture(), 32001);
    assert.equal(m.view(32001).mutable, true);
    c = m.begin('take-control', 32002, m.intent('take-control', null, 32002));
    m.fail(c, 'unavailable');
    assert.equal(m.view(32003).mutable, false);
    assert.equal(m.view(32003).checked, false);
    assert.equal(m.intent('take-control', null, 32003), null);
    assert.ok(m.delay <= 60000);
});
test('retired generations and lower versions cannot replace accepted state', () => {
    const m = new Model();
    let c = m.begin('status', 0);
    m.accept(c, fixture('user'), 1);
    c = m.begin('status', 2);
    m.accept(c, fixture('supervisor', '9007199254740992'), 3);
    assert.equal(m.view(3).checked, true);
    assert.equal(m.view(3).mutable, false);
    c = m.begin('status', 4);
    m.retire();
    assert.equal(m.accept(c, fixture(), 5), false);
});
test('replacement incarnation invalidates captured ownership decisions', () => {
    const m = new Model();
    let c = m.begin('status', 0);
    m.accept(c, fixture(), 1);
    const decision = m.intent('take-control', null, 1);
    c = m.begin('status', 2);
    const next = fixture();
    next.status.expected.incarnation = 'restored';
    m.accept(c, next, 3);
    assert.equal(m.validDecision(decision, 3), false);
});
test('faulted and transient statuses cannot authorize mutations', () => {
    for (const field of ['health', 'phase']) {
        const m = new Model();
        const c = m.begin('status', 0),
            r = fixture();
        r.status[field] = field === 'health' ? 'error' : 'loading';
        m.accept(c, r, 1);
        assert.equal(m.view(1).mutable, false);
    }
});
test('mutation cannot dispatch without a fresh explicit decision', () => {
    const m = new Model();
    assert.equal(m.begin('take-control', 0), null);
    let c = m.begin('status', 0);
    m.accept(c, fixture(), 1);
    assert.equal(m.begin('take-control', 1), null);
    const d = m.intent('take-control', null, 1);
    assert.ok(m.begin('take-control', 2, d));
});
test('all typed errors invalidate decisions and never manufacture idle', () => {
    for (const code of [
        'invalid_request',
        'unsupported_version',
        'incompatible_configuration',
        'stale_state',
        'wrong_owner',
        'busy',
        'recovery_required',
        'timeout',
        'unavailable',
    ]) {
        const m = new Model();
        let c = m.begin('status', 0);
        m.accept(c, fixture('user'), 1);
        c = m.begin('status', 2);
        const r = parseResponse(
            JSON.stringify({
                protocolVersion: 1,
                requestId: c.request.requestId,
                code,
            }),
            c.request.requestId,
        );
        m.accept(c, r, 3);
        assert.equal(m.view(3).mutable, false);
        assert.equal(m.status.activeWorkload, 'render');
        assert.equal(m.view(3).checked, true);
    }
});
test('uint64 maximum retained, catalog replacement and unknown remain distinct', () => {
    const m = new Model();
    let c = m.begin('status', 0);
    const r = fixture('user', '18446744073709551615');
    r.status.workloads.push({ id: 'third', label: 'Third workload' });
    r.status.activeWorkload = 'unknown';
    m.accept(c, parseResponse(JSON.stringify(r), 'r1'), 1);
    assert.equal(m.status.activeWorkload, 'unknown');
    const d = m.intent('user-switch', 'third', 1);
    assert.equal(d.expected.version, '18446744073709551615');
    c = m.begin('status', 2);
    const next = fixture('user', '18446744073709551615');
    next.status.expected.configurationRevision = 'c2';
    m.accept(c, next, 3);
    assert.equal(m.validDecision(d, 3), false);
    assert.equal(m.intent('user-switch', 'third', 3), null);
});
test('duplicate and superseded results cannot update status or freshness', () => {
    const m = new Model();
    const c = m.begin('status', 0);
    m.accept(c, fixture(), 1);
    assert.equal(m.accept(c, fixture('user'), 2), false);
    const next = m.begin('status', 3);
    assert.equal(m.accept(c, fixture('user'), 4), false);
    m.accept(next, fixture('user'), 5);
    assert.equal(m.dispatch, 3);
});
