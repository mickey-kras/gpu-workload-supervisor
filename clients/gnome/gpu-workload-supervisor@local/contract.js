// Pure, shared by GJS and Node. Versions never pass through Number.
export const ERROR_CODES = [
    'invalid_request',
    'unsupported_version',
    'incompatible_configuration',
    'stale_state',
    'wrong_owner',
    'busy',
    'recovery_required',
    'timeout',
    'unavailable',
    'deferred',
];
const fail = () => {
    throw new Error('Invalid operator response');
};
function keys(o, names) {
    if (
        !o ||
        typeof o !== 'object' ||
        Array.isArray(o) ||
        Object.keys(o).length !== names.length ||
        names.some((name) => !Object.hasOwn(o, name))
    )
        fail();
}
function one(v, values) {
    if (!values.includes(v)) fail();
}
function token(v) {
    if (typeof v !== 'string' || !/^[!-~]{1,128}$/.test(v)) fail();
}
export function compareVersions(a, b) {
    if (a.length !== b.length) return a.length > b.length ? 1 : -1;
    if (a === b) return 0;
    return a > b ? 1 : -1;
}
function validateExpected(s) {
    keys(s.expected, [
        'incarnation',
        'version',
        'owner',
        'configurationRevision',
    ]);
    token(s.expected.incarnation);
    token(s.expected.configurationRevision);
    const v = s.expected.version;
    if (
        typeof v !== 'string' ||
        !/^[1-9]\d{0,19}$/.test(v) ||
        compareVersions(v, '18446744073709551615') > 0 ||
        s.expected.owner !== s.owner
    )
        fail();
}

function validateCatalog(s) {
    if (
        !Array.isArray(s.workloads) ||
        s.workloads.length < 1 ||
        s.workloads.length > 65
    )
        fail();
    const ids = new Set();
    for (const w of s.workloads) {
        keys(w, ['id', 'label']);
        if (
            typeof w.id !== 'string' ||
            !/^[a-z][a-z0-9_-]{0,63}$/.test(w.id) ||
            w.id === 'unknown' ||
            ids.has(w.id)
        )
            fail();
        ids.add(w.id);
        if (
            typeof w.label !== 'string' ||
            !w.label.trim() ||
            [...w.label].length > 80 ||
            /[\p{Cc}\u202a-\u202e\u2066-\u2069]/u.test(w.label)
        )
            fail();
    }
    if (
        !ids.has('idle') ||
        !ids.has(s.desiredWorkload) ||
        (!ids.has(s.activeWorkload) && s.activeWorkload !== 'unknown')
    )
        fail();
}

function validateIdlePolicy(p) {
    keys(p, ['timeoutMinutes']);
    const t = p.timeoutMinutes;
    if (!Number.isInteger(t) || (t !== 0 && (t < 5 || t > 1440))) fail();
}

export function validateSettings(settings) {
    keys(settings, ['policy', 'settingsRevision']);
    validateIdlePolicy(settings.policy);
    token(settings.settingsRevision);
}

function validateStatus(s) {
    keys(s, [
        'owner',
        'desiredWorkload',
        'activeWorkload',
        'phase',
        'health',
        'admission',
        'observedAt',
        'expected',
        'workloads',
        'capabilities',
    ]);
    one(s.owner, ['supervisor', 'user']);
    one(s.phase, [
        'stable',
        'draining',
        'unloading',
        'loading',
        'verifying',
        'reconciling',
    ]);
    one(s.health, ['healthy', 'degraded', 'error']);
    one(s.admission, ['open', 'closed']);
    if (
        typeof s.observedAt !== 'string' ||
        s.observedAt.length > 64 ||
        !Number.isFinite(Date.parse(s.observedAt))
    )
        fail();
    validateExpected(s);
    validateCatalog(s);
    keys(s.capabilities, ['takeControl', 'userSwitch', 'returnControl']);
    if (Object.values(s.capabilities).some((v) => typeof v !== 'boolean'))
        fail();
}

// The typed settings actions answer with a settings object only and never
// mint a status from durable state; every other ok response keeps the exact
// version-1 shape with status and no settings. A successful activate-workload
// additionally carries the freshly rotated lease fence.
export const SETTINGS_ACTIONS = ['get-settings', 'set-idle-policy'];
export const ACTIVATE_ACTION = 'activate-workload';

// The committed fence handed to the activating caller: an opaque incarnation
// token plus a canonical nonzero uint64 epoch string (never a number), the
// same shape the expected version uses.
function validateLeaseFence(f) {
    keys(f, ['incarnation', 'epoch']);
    token(f.incarnation);
    if (
        typeof f.epoch !== 'string' ||
        !/^[1-9]\d{0,19}$/.test(f.epoch) ||
        compareVersions(f.epoch, '18446744073709551615') > 0
    )
        fail();
}

export function parseResponse(text, requestId, action) {
    if (new TextEncoder().encode(text).length > 65536) fail();
    const r = JSON.parse(text);
    validateResponseKeys(r, action);
    if (r.protocolVersion !== 1 || r.requestId !== requestId) fail();
    if (r.code !== 'ok') {
        if (r.code === 'deferred') validateDeferredStatus(r);
        return r;
    }
    if (SETTINGS_ACTIONS.includes(action)) {
        validateSettings(r.settings);
        return r;
    }
    validateStatus(r.status);
    if (action === ACTIVATE_ACTION) {
        validateLeaseFence(r.leaseFence);
        // The handed-out fence must be the fence the returned status commits:
        // a mismatched incarnation would be rejected as stale by every
        // admission, so bind them here and fail closed otherwise.
        if (r.leaseFence.incarnation !== r.status.expected.incarnation) fail();
    }
    return r;
}

function successfulResponseKeys(base, action) {
    if (SETTINGS_ACTIONS.includes(action)) return [...base, 'settings'];
    if (action === ACTIVATE_ACTION) return [...base, 'status', 'leaseFence'];
    return [...base, 'status'];
}

function validateResponseKeys(r, action) {
    const base = ['protocolVersion', 'requestId', 'code'];
    if (r.code === 'ok') {
        keys(r, successfulResponseKeys(base, action));
    } else if (r.code === 'deferred') {
        // Deferred is defined only for activate-workload; on any other action
        // it is a malformed response and fails closed.
        if (action !== ACTIVATE_ACTION) fail();
        // A deferred activation reports the observed current status so the
        // desktop can render why the workload did not start, and optionally
        // the current lease fence so a caller that lost a committed
        // activation response can recover the committed generation.
        keys(r, 'leaseFence' in r ? [...base, 'status', 'leaseFence'] : [...base, 'status']);
    } else {
        keys(r, base);
        one(r.code, ERROR_CODES);
    }
}

function validateDeferredStatus(r) {
    validateStatus(r.status);
    if ('leaseFence' in r) {
        validateLeaseFence(r.leaseFence);
        if (r.leaseFence.incarnation !== r.status.expected.incarnation) fail();
    }
}
