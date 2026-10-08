import test from 'node:test';
import assert from 'node:assert/strict';
import {ApplicationDraft, applications, candidateMessage, profileIDFromModel} from './onboarding.mjs';

test('four app choices never become executable profiles through discovery', () => {
    assert.deepEqual(applications.map(app => app.id), ['comfyui', 'ollama', 'llama.cpp', 'vllm']);
    for (const app of applications) {
        const draft = new ApplicationDraft(app.id);
        const probe = draft.begin();
        draft.accept(probe, {app: app.id, instanceStatus: 'available', inventoryStatus: 'available', models: [{id: 'model-a'}], lifecycleControl: 'unverified'});
        assert.equal(draft.needsModel, app.id !== 'comfyui');
        assert.equal(draft.snapshot().app, app.id);
        assert.equal('adapter' in draft.snapshot(), false);
    }
});

test('edited and cancelled probes cannot replace current discovery', () => {
    const draft = new ApplicationDraft('ollama');
    const old = draft.begin();
    draft.edit({endpoint: 'http://127.0.0.1:1234'});
    assert.equal(draft.accept(old, {models: [{id: 'old'}]}), false);
    const current = draft.begin();
    assert.equal(current.request.endpoint, 'http://127.0.0.1:1234');
    draft.cancel();
    assert.equal(draft.accept(current, {models: []}), false);
});

test('file fallback replaces endpoint and preserves honest verification', () => {
    const draft = new ApplicationDraft('llama.cpp');
    draft.edit({endpoint: 'http://127.0.0.1:8080'});
    draft.reference('/models/one.gguf', 'model-file');
    assert.deepEqual(draft.begin().request, {app: 'llama.cpp', reference: '/models/one.gguf', referenceKind: 'model-file'});
});

test('profile IDs slugify app and model within backend rules and refuse unusable input', () => {
    assert.equal(profileIDFromModel('ollama', 'qwen:latest'), 'ollama-qwen-latest');
    assert.equal(profileIDFromModel('llama.cpp', '/models/a.gguf'), 'llama-cpp-models-a-gguf');
    assert.equal(profileIDFromModel('ollama', ''), null);
    assert.equal(profileIDFromModel('ollama', undefined), null);
    const long = profileIDFromModel('vllm', `Org/${'Model-Name_'.repeat(20)}`);
    assert.ok(long.length <= 64 && /^[a-z][a-z0-9_-]*$/.test(long) && !long.endsWith('-'));
});

test('profile IDs strip leading non-letters and trailing dashes and normalize case', () => {
    assert.equal(profileIDFromModel('9lives', 'one'), 'lives-one');
    assert.equal(profileIDFromModel('--x', 'model'), 'x-model');
    assert.equal(profileIDFromModel('vllm', 'model---'), 'vllm-model');
    assert.equal(profileIDFromModel('VLLM', 'Model'), 'vllm-model');
    assert.equal(profileIDFromModel('vllm', 'Qwen/QWEN'), 'vllm-qwen-qwen');
});

test('profile IDs re-trim trailing dashes after the 64-character cut', () => {
    assert.equal(profileIDFromModel('a'.repeat(63), 'x'), 'a'.repeat(63));
    assert.equal(profileIDFromModel('a'.repeat(64), '-'), 'a'.repeat(64));
});

test('profile IDs refuse empty-after-strip and reserved slugs', () => {
    assert.equal(profileIDFromModel('---', '...'), null);
    assert.equal(profileIDFromModel('idle', '!'), null);
    assert.equal(profileIDFromModel('unknown', '!'), null);
    assert.equal(profileIDFromModel('', 'idle'), null);
});

test('profile ID derivation stays linear on pathological dash runs', () => {
    assert.equal(profileIDFromModel('ollama', `a${'-'.repeat(50000)}b`), 'ollama-a-b');
});

test('stopped and unreachable instances never report no models', () => {
    assert.match(candidateMessage({instanceStatus: 'not-running', unit: 'installed.service'}), /Installed and stopped/);
    assert.match(candidateMessage({instanceStatus: 'unreachable'}), /Unable to reach/);
    assert.match(candidateMessage({instanceStatus: 'available', inventoryStatus: 'available'}), /need a check/i);
});
