import test from 'node:test';
import assert from 'node:assert/strict';
import { ResponseBuffer } from '../gpu-workload-supervisor@local/framing.js';
test('bounded byte framing rejects overflow, trailing data and invalid UTF8', () => {
    const b = new ResponseBuffer(8);
    b.append(new TextEncoder().encode('{}\n'));
    assert.equal(b.finish(), '{}\n');
    assert.throws(() => b.append(new Uint8Array(6)));
    const bad = new ResponseBuffer();
    bad.append(new Uint8Array([255]));
    assert.throws(() => bad.finish());
});
