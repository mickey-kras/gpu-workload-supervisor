import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { ERROR_CODES } from '../gpu-workload-supervisor@local/contract.js';

const root = join(fileURLToPath(import.meta.url), '..', '..', '..', '..');
const go = readFileSync(join(root, 'internal/operator/protocol.go'), 'utf8');
const client = readFileSync(
    join(root, 'clients/gnome/gpu-workload-supervisor@local/contract.js'),
    'utf8'
);

const codeBlock = go.match(/type Code string\s*const \(([\s\S]*?)\)/);
assert.ok(codeBlock, 'go Code const block not found');
const goCodes = [...codeBlock[1].matchAll(/Code = "([^"]+)"/g)]
    .map((m) => m[1])
    .filter((code) => code !== 'ok');
assert.ok(goCodes.length > 0, 'no go Code values extracted');

const structBlock = go.match(/type Expected struct \{([\s\S]*?)\}/);
assert.ok(structBlock, 'go Expected struct not found');
const goFields = [...structBlock[1].matchAll(/json:"([^",]+)[^"]*"/g)].map(
    (m) => m[1]
);
assert.ok(goFields.length > 0, 'no go Expected json tags extracted');

const keysBlock = client.match(/keys\(s\.expected, \[([\s\S]*?)\]\)/);
assert.ok(keysBlock, 'client validateExpected field list not found');
const clientFields = [...keysBlock[1].matchAll(/'([^']+)'/g)].map((m) => m[1]);

test('client error codes match the go operator codes', () => {
    assert.deepEqual([...ERROR_CODES].sort(), [...goCodes].sort());
});

test('client expected fields match the go Expected json tags', () => {
    assert.deepEqual([...clientFields].sort(), [...goFields].sort());
});

const goStructTags = (name) => {
    const block = go.match(new RegExp(`type ${name} struct \\{([\\s\\S]*?)\\}`));
    assert.ok(block, `go ${name} struct not found`);
    return [...block[1].matchAll(/json:"([^",]+)[^"]*"/g)]
        .map((m) => m[1])
        .sort();
};
const clientKeyList = (call) => {
    const block = client.match(new RegExp(`${call},\\s*\\[([\\s\\S]*?)\\]\\)`));
    assert.ok(block, `client ${call} field list not found`);
    return [...block[1].matchAll(/'([^']+)'/g)].map((m) => m[1]).sort();
};

test('client capabilities keys match the go Capabilities json tags', () => {
    assert.deepEqual(
        clientKeyList('keys\\(s\\.capabilities'),
        goStructTags('Capabilities'),
    );
});

test('client settings fields match the go SettingsResponse json tags', () => {
    assert.deepEqual(
        clientKeyList('keys\\(settings'),
        goStructTags('SettingsResponse'),
    );
    assert.deepEqual(
        clientKeyList('keys\\(p'),
        goStructTags('IdlePolicyStatus'),
    );
});
