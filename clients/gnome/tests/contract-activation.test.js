import test from 'node:test';
import assert from 'node:assert/strict';
import { parseResponse } from '../gpu-workload-supervisor@local/contract.js';

const statusBody = () => ({
    owner: 'supervisor',
    desiredWorkload: 'render',
    activeWorkload: 'render',
    phase: 'stable',
    health: 'healthy',
    admission: 'closed',
    observedAt: '2026-10-04T00:00:00Z',
    expected: {
        incarnation: 'a',
        version: '1',
        owner: 'supervisor',
        configurationRevision: 'c1',
    },
    workloads: [
        { id: 'idle', label: 'Idle' },
        { id: 'render', label: 'Rendering' },
    ],
    capabilities: {
        takeControl: true,
        userSwitch: false,
        returnControl: false,
    },
});

const activateOk = (mutate = null) => {
    const r = {
        protocolVersion: 1,
        requestId: 'r1',
        code: 'ok',
        status: statusBody(),
        leaseFence: { incarnation: 'opaque-incarnation', epoch: '7' },
    };
    // A well-formed response binds the fence to the status incarnation.
    r.status.expected.incarnation = r.leaseFence.incarnation;
    if (mutate) mutate(r);
    return JSON.stringify(r);
};

const deferred = (mutate = null, action = 'activate-workload') => {
    const r = {
        protocolVersion: 1,
        requestId: 'r1',
        code: 'deferred',
        status: statusBody(),
    };
    if (mutate) mutate(r);
    return JSON.stringify(r);
};

test('activate-workload ok carries a strict lease fence', () => {
    const r = parseResponse(activateOk(), 'r1', 'activate-workload');
    assert.equal(r.leaseFence.incarnation, 'opaque-incarnation');
    assert.equal(r.leaseFence.epoch, '7');
    assert.equal(r.status.activeWorkload, 'render');
});

test('activate-workload ok fails closed on a malformed lease fence', () => {
    for (const body of [
        activateOk((r) => delete r.leaseFence),
        activateOk((r) => (r.leaseFence = { incarnation: 'x', epoch: '7', extra: 1 })),
        activateOk((r) => (r.leaseFence = { incarnation: 'x' })),
        activateOk((r) => (r.leaseFence.epoch = 7)), // numbers never pass
        activateOk((r) => (r.leaseFence.epoch = 'not-a-number')),
        activateOk((r) => (r.leaseFence.epoch = '')),
        activateOk((r) => (r.leaseFence.epoch = '0')), // zero epoch
        activateOk((r) => (r.leaseFence.epoch = '01')), // leading zero
        activateOk((r) => (r.leaseFence.epoch = '18446744073709551616')), // max uint64 + 1
        activateOk((r) => (r.leaseFence.epoch = '99999999999999999999')), // 20 digits over range
        activateOk((r) => (r.leaseFence.incarnation = '')),
        activateOk((r) => (r.leaseFence.incarnation = 'has space')),
    ]) {
        assert.throws(() => parseResponse(body, 'r1', 'activate-workload'));
    }
});

test('lease fence epoch accepts canonical uint64 strings', () => {
    for (const epoch of ['1', '7', '18446744073709551615']) {
        const r = parseResponse(activateOk((r) => (r.leaseFence.epoch = epoch)), 'r1', 'activate-workload');
        assert.equal(r.leaseFence.epoch, epoch);
    }
});

test('lease fence is bound to the returned status incarnation', () => {
    const r = parseResponse(activateOk(), 'r1', 'activate-workload');
    assert.equal(r.leaseFence.incarnation, r.status.expected.incarnation);
    // A fence from another generation than the returned status fails closed.
    for (const body of [
        activateOk((r) => (r.leaseFence.incarnation = 'other-incarnation')),
        activateOk((r) => (r.status.expected.incarnation = 'other-incarnation')),
    ]) {
        assert.throws(() => parseResponse(body, 'r1', 'activate-workload'));
    }
});

test('other actions reject a leaseFence key outright', () => {
    for (const action of ['status', 'take-control', 'user-switch', 'return-control', 'get-settings']) {
        assert.throws(() => parseResponse(activateOk(), 'r1', action));
    }
});

test('deferred responses carry the observed status', () => {
    const r = parseResponse(deferred(), 'r1', 'activate-workload');
    assert.equal(r.code, 'deferred');
    assert.equal(r.status.activeWorkload, 'render');
});

test('deferred responses fail closed on any other shape', () => {
    for (const body of [
        deferred((r) => delete r.status),
        deferred((r) => (r.leaseFence = { incarnation: 'x', epoch: '1', extra: 1 })),
        deferred((r) => (r.leaseFence = null)),
        deferred((r) => (r.leaseFence = { incarnation: 'x' })), // missing epoch
        deferred((r) => (r.status = { owner: 'supervisor' })), // invalid status
        deferred((r) => (r.status = null)),
    ]) {
        assert.throws(() => parseResponse(body, 'r1', 'activate-workload'));
    }
});

test('deferred may carry the current fence, bound to the status incarnation', () => {
    // Bound fence: accepted (recoverable committed generation).
    const bound = deferred((r) => {
        r.leaseFence = { incarnation: r.status.expected.incarnation, epoch: '9' };
    });
    const ok = parseResponse(bound, 'r1', 'activate-workload');
    assert.equal(ok.leaseFence.epoch, '9');
    // Unbound fence (another generation than the reported status): rejected.
    const unbound = deferred((r) => {
        r.leaseFence = { incarnation: 'other-incarnation', epoch: '9' };
    });
    assert.throws(() => parseResponse(unbound, 'r1', 'activate-workload'));
    // Fence shape is still strict on deferred responses.
    const badEpoch = deferred((r) => {
        r.leaseFence = { incarnation: r.status.expected.incarnation, epoch: '0' };
    });
    assert.throws(() => parseResponse(badEpoch, 'r1', 'activate-workload'));
});

test('deferred is rejected on every action except activate-workload', () => {
    for (const action of ['status', 'get-settings', 'set-idle-policy', 'take-control', 'user-switch', 'return-control', undefined]) {
        assert.throws(() => parseResponse(deferred(), 'r1', action), `deferred accepted for ${action}`);
    }
});

test('unknown error codes are still rejected', () => {
    for (const code of ['deferredx', 'ok2', '', 'DEFERRED']) {
        const body = JSON.stringify({
            protocolVersion: 1,
            requestId: 'r1',
            code,
        });
        assert.throws(() => parseResponse(body, 'r1', 'status'));
    }
});

test('non-deferred error responses still reject extra keys', () => {
    const body = JSON.stringify({
        protocolVersion: 1,
        requestId: 'r1',
        code: 'busy',
        status: statusBody(),
    });
    assert.throws(() => parseResponse(body, 'r1', 'status'));
});
