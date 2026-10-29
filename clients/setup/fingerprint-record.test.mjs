import test from 'node:test';
import assert from 'node:assert/strict';
import {recordFingerprint} from './onboarding.mjs';

test('fingerprint records the hardened vLLM settings read from the launch', () => {
    const nativeModel = {runtime: 'vllm', launchSHA256: 'old', chatTemplate: '/stale.jinja', apiKeyFile: '/stale.env'};
    recordFingerprint(nativeModel, {sha256: 'fresh', chatTemplate: '/models/a/chat_template.jinja', chatTemplateSHA256: 'digest', apiKeyFile: ''});
    assert.deepEqual(nativeModel, {runtime: 'vllm', launchSHA256: 'fresh', chatTemplate: '/models/a/chat_template.jinja', chatTemplateSHA256: 'digest'});
});
