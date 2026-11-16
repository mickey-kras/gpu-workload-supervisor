import test from 'node:test';
import assert from 'node:assert/strict';
import {
    CLOSE_ACTION,
    parseResponse,
} from '../gpu-workload-supervisor@local/contract.js';
import { Model } from '../gpu-workload-supervisor@local/model.js';

const fixture = (owner) => ({
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
        expected: { incarnation: 'a', version: '1', owner, configurationRevision: 'c1' },
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

const userProcess = () => ({
    pid: 4242,
    name: 'python3',
    user: 'alice',
    usedMiB: 2048,
    closable: true,
});
const otherProcess = () => ({
    pid: 77,
    name: 'blender',
    user: 'bob',
    usedMiB: null,
    closable: false,
});
const inUse = (requestId = 'r1', processes = [userProcess(), otherProcess()]) => ({
    protocolVersion: 1,
    requestId,
    code: 'gpu_in_use',
    gpuCheck: { processes },
});
const checkFailed = (requestId = 'r1') => ({
    protocolVersion: 1,
    requestId,
    code: 'gpu_check_failed',
    gpuCheck: { failure: 'the nvidia-smi process query timed out' },
});
const parse = (r, action) => parseResponse(JSON.stringify(r), r.requestId, action);

test('gpu codes carry the processes or the failure on start and close actions', () => {
    for (const action of ['user-switch', 'activate-workload', CLOSE_ACTION]) {
        assert.equal(parse(inUse(), action).gpuCheck.processes.length, 2);
        assert.match(parse(checkFailed(), action).gpuCheck.failure, /timed out/);
    }
    for (const action of ['status', 'take-control', 'return-control', 'get-settings']) {
        assert.throws(() => parse(inUse(), action));
        assert.throws(() => parse(checkFailed(), action));
    }
    const notPermitted = { protocolVersion: 1, requestId: 'r1', code: 'not_permitted' };
    assert.equal(parse(notPermitted, CLOSE_ACTION).code, 'not_permitted');
    assert.throws(() => parse(notPermitted, 'user-switch'));
    assert.throws(() => parse({ ...notPermitted, gpuCheck: {} }, CLOSE_ACTION));
});

test('malformed gpu details fail closed', () => {
    const mutations = [
        (r) => delete r.gpuCheck,
        (r) => (r.gpuCheck = null),
        (r) => (r.gpuCheck.extra = 1),
        (r) => (r.gpuCheck.processes = []),
        (r) => (r.gpuCheck.processes = 'x'),
        (r) => (r.gpuCheck.processes = Array.from({ length: 33 }, userProcess)),
        (r) => (r.gpuCheck.processes[0].pid = 0),
        (r) => (r.gpuCheck.processes[0].pid = 4194305),
        (r) => (r.gpuCheck.processes[0].pid = '1'),
        (r) => (r.gpuCheck.processes[0].name = 'a\nb'),
        (r) => (r.gpuCheck.processes[0].name = 'x'.repeat(257)),
        (r) => (r.gpuCheck.processes[0].user = 7),
        (r) => (r.gpuCheck.processes[0].usedMiB = -1),
        (r) => (r.gpuCheck.processes[0].usedMiB = 1.5),
        (r) => (r.gpuCheck.processes[0].closable = 'yes'),
        (r) => delete r.gpuCheck.processes[0].usedMiB,
        (r) => (r.status = {}),
    ];
    for (const mutate of mutations) {
        const r = inUse();
        mutate(r);
        assert.throws(() => parse(r, 'user-switch'));
    }
    for (const failure of ['', 'bad‮text', 7]) {
        const r = checkFailed();
        r.gpuCheck.failure = failure;
        assert.throws(() => parse(r, 'user-switch'));
    }
    const both = checkFailed();
    both.gpuCheck.processes = [userProcess()];
    assert.throws(() => parse(both, 'user-switch'));
    const unknown = inUse();
    unknown.gpuCheck.processes[0].name = '';
    unknown.gpuCheck.processes[0].user = '';
    assert.equal(parse(unknown, 'user-switch').code, 'gpu_in_use');
});

function userModel() {
    const m = new Model();
    const c = m.begin('status', 0);
    m.accept(c, fixture('user'), 1);
    return m;
}

function blockedModel(response = inUse) {
    const m = userModel();
    const c = m.begin('user-switch', 2, m.intent('user-switch', 'idle', 2));
    assert.equal(m.accept(c, response(c.request.requestId), 3), false);
    return m;
}

test('a refused start keeps its target and processes for Close and Retry', () => {
    const m = blockedModel();
    assert.equal(m.error, 'gpu_in_use');
    assert.equal(m.delay, 1000, 'a refusal refreshes soon, without backoff');
    assert.deepEqual(m.view(3).blocked.processes.map((p) => p.pid), [4242, 77]);
    assert.equal(m.blocked.target, 'idle');
    assert.equal(m.blocked.failure, null);
    assert.equal(m.closable(4242), true);
    assert.equal(m.closable(77), false);
    assert.equal(m.closeIntent(77), null);
    assert.equal(m.closeIntent(5), null);
    assert.deepEqual(m.closeIntent(4242), { action: CLOSE_ACTION, pid: 4242, confirmation: true });
    // An error leaves the status stale: Retry waits for a fresh status.
    assert.equal(m.retryIntent(4), null);
    const s = m.begin('status', 4);
    m.accept(s, fixture('user'), 5);
    assert.equal(m.retryIntent(5).target, 'idle');
    assert.ok(m.blocked, 'a status keeps the refusal on screen');
});

test('a failed check is shown as a failure, never as a free GPU', () => {
    const m = blockedModel(checkFailed);
    assert.equal(m.error, 'gpu_check_failed');
    assert.equal(m.blocked.failure, 'the nvidia-smi process query timed out');
    assert.deepEqual(m.blocked.processes, []);
    assert.equal(m.closeIntent(4242), null);
});

test('close sends only the listed pid of the blocked target, without a precondition', () => {
    const m = blockedModel();
    assert.equal(m.begin(CLOSE_ACTION, 4), null);
    assert.equal(m.begin(CLOSE_ACTION, 4, { action: CLOSE_ACTION, pid: 77 }), null);
    assert.equal(m.begin(CLOSE_ACTION, 4, { action: 'user-switch', pid: 4242 }), null);
    const c = m.begin(CLOSE_ACTION, 4, m.closeIntent(4242));
    assert.deepEqual(c.request, {
        protocolVersion: 1,
        requestId: c.request.requestId,
        action: CLOSE_ACTION,
        target: 'idle',
        pid: 4242,
    });
    assert.equal(m.closeIntent(4242), null, 'no Close while a request is pending');
    // The process ended or exited after SIGTERM: the GPU is free, Retry stays.
    assert.equal(m.accept(c, { ...fixture('user'), requestId: c.request.requestId }, 5), true);
    assert.deepEqual(m.blocked, { target: 'idle', processes: [], failure: null });
    assert.equal(m.retryIntent(5).target, 'idle');
    const retry = m.begin('user-switch', 6, m.retryIntent(6));
    const done = fixture('user');
    done.status.activeWorkload = 'idle';
    m.accept(retry, { ...done, requestId: retry.request.requestId }, 7);
    assert.equal(m.blocked, null);
});

test('close that leaves the process running or is refused keeps the refusal', () => {
    const m = blockedModel();
    let c = m.begin(CLOSE_ACTION, 4, m.closeIntent(4242));
    m.accept(c, inUse(c.request.requestId, [userProcess()]), 5);
    assert.equal(m.error, 'gpu_in_use');
    assert.deepEqual(m.blocked.processes.map((p) => p.pid), [4242]);
    c = m.begin(CLOSE_ACTION, 6, m.closeIntent(4242));
    m.accept(c, { protocolVersion: 1, requestId: c.request.requestId, code: 'not_permitted' }, 7);
    assert.equal(m.error, 'not_permitted');
    assert.deepEqual(m.blocked.processes.map((p) => p.pid), [4242]);
    m.retire();
    assert.equal(m.blocked, null);
    assert.equal(m.retryIntent(8), null);
});
