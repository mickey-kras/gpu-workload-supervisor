import test from 'node:test';
import assert from 'node:assert/strict';
import {ReviewedConfiguration} from './review.mjs';

test('editing while validation runs retires its reply', () => {
    const review = new ReviewedConfiguration();
    const original = review.begin(JSON.stringify({catalog: {profiles: ['original']}}));
    review.invalidate();
    assert.equal(review.accept(original), false);
    assert.throws(() => review.confirmed(), /Review/);
});

test('activation uses the validated immutable snapshot and explicit confirmation', () => {
    const review = new ReviewedConfiguration();
    let draft = {catalog: {profiles: ['reviewed']}, confirmQuiesced: false};
    const token = review.begin(JSON.stringify(draft));
    draft.catalog.profiles[0] = 'later edit';
    assert.equal(review.accept(token), true);
    assert.deepEqual(JSON.parse(review.confirmed()), {
        catalog: {profiles: ['reviewed']}, confirmQuiesced: true,
    });
    review.invalidate();
    assert.throws(() => review.confirmed(), /Review/);
});
