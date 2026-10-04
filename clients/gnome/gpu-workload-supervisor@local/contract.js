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
];
const fail = () => {
    throw new Error('Invalid operator response');
};
function keys(o, names) {
    if (
        !o ||
        typeof o !== 'object' ||
        Array.isArray(o) ||
        Object.keys(o).sort().join() !== [...names].sort().join()
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
    return a.length === b.length
        ? a === b
            ? 0
            : a > b
              ? 1
              : -1
        : a.length > b.length
          ? 1
          : -1;
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
        !/^[1-9][0-9]{0,19}$/.test(v) ||
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
            /[\x00-\x1f\x7f-\x9f\u202a-\u202e\u2066-\u2069]/.test(w.label)
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

export function parseResponse(text, requestId) {
    if (new TextEncoder().encode(text).length > 65536) fail();
    const r = JSON.parse(text);
    if (r.code === 'ok')
        keys(r, ['protocolVersion', 'requestId', 'code', 'status']);
    else {
        keys(r, ['protocolVersion', 'requestId', 'code']);
        one(r.code, ERROR_CODES);
    }
    if (r.protocolVersion !== 1 || r.requestId !== requestId) fail();
    if (r.code !== 'ok') return r;
    validateStatus(r.status);
    return r;
}
