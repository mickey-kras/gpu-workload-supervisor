import test from 'node:test';
import assert from 'node:assert/strict';
import { parseResponse, validateSettings } from '../gpu-workload-supervisor@local/contract.js';

const fixture = () => ({
    protocolVersion: 1,
    requestId: 'r1',
    code: 'ok',
    status: {
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
            idlePolicyConfigurable: false,
        },
        idlePolicy: { timeoutMinutes: 0 },
    },
});

const okWithSettings = (mutate = null) => {
    const r = fixture();
    r.settings = {
        policy: { timeoutMinutes: 60 },
        settingsRevision: 's-rev-1',
    };
    if (mutate) mutate(r);
    return JSON.stringify(r);
};

test('get-settings response carries a strict settings object', () => {
    const r = parseResponse(okWithSettings(), 'r1');
    assert.equal(r.settings.policy.timeoutMinutes, 60);
    assert.equal(r.settings.settingsRevision, 's-rev-1');
});

test('malformed settings objects fail closed', () => {
    for (const mutate of [
        (r) => (r.settings.extra = true),
        (r) => delete r.settings.policy,
        (r) => (r.settings.settingsRevision = ''),
        (r) => (r.settings.settingsRevision = 'bad revision'),
        (r) => (r.settings.policy.timeoutMinutes = 4),
        (r) => (r.settings.policy.timeoutMinutes = 1441),
        (r) => (r.settings.policy.timeoutMinutes = 5.5),
        (r) => (r.settings.policy.extra = true),
        (r) => (r.settings = null),
        (r) => (r.settings = 'off'),
    ]) {
        assert.throws(() => parseResponse(okWithSettings(mutate), 'r1'));
    }
});

test('validateSettings enforces the exact typed shape', () => {
    validateSettings({ policy: { timeoutMinutes: 0 }, settingsRevision: 'r' });
    validateSettings({ policy: { timeoutMinutes: 1440 }, settingsRevision: 'r' });
    for (const bad of [
        null,
        { policy: { timeoutMinutes: 0 } },
        { settingsRevision: 'r' },
        { policy: { timeoutMinutes: 1 }, settingsRevision: 'r' },
        { policy: { timeoutMinutes: -5 }, settingsRevision: 'r' },
        { policy: { timeoutMinutes: 0 }, settingsRevision: 7 },
    ]) {
        assert.throws(() => validateSettings(bad));
    }
});

test('status without the idle policy surface fails closed', () => {
    for (const mutate of [
        (r) => delete r.status.idlePolicy,
        (r) => (r.status.idlePolicy = { timeoutMinutes: 3 }),
        (r) => (r.status.idlePolicy = { timeoutMinutes: 0, extra: true }),
        (r) => delete r.status.capabilities.idlePolicyConfigurable,
        (r) => (r.status.capabilities.idlePolicyConfigurable = 'no'),
    ]) {
        const r = fixture();
        mutate(r);
        assert.throws(() => parseResponse(JSON.stringify(r), 'r1'));
    }
});

test('enabled committed policy inside bounds validates', () => {
    const r = fixture();
    r.status.idlePolicy = { timeoutMinutes: 1440 };
    r.status.capabilities.idlePolicyConfigurable = true;
    assert.equal(parseResponse(JSON.stringify(r), 'r1').status.idlePolicy.timeoutMinutes, 1440);
});
