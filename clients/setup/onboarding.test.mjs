import test from 'node:test';
import assert from 'node:assert/strict';
import {ApplicationDraft, applications, candidateMessage} from './onboarding.mjs';

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

test('stopped and unreachable instances never report no models', () => {
    assert.match(candidateMessage({instanceStatus: 'not-running'}), /Not running/);
    assert.match(candidateMessage({instanceStatus: 'unreachable'}), /Unable to reach/);
    assert.match(candidateMessage({instanceStatus: 'available', inventoryStatus: 'available'}), /not verified/i);
});
