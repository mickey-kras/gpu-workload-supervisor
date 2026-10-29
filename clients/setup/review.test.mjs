import test from 'node:test';
import assert from 'node:assert/strict';
import {ReviewedConfiguration, lowestCatalogVersion} from './review.mjs';

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

test('catalog version is the lowest that the profiles need', () => {
    const owned = {nativeModel: {owned: {port: 8000}}};
    const limited = {nativeModel: {owned: {port: 8000, limits: {version: 1, memoryMaxMiB: 4096}}}};
    const trainer = {trainer: {version: 1}};
    assert.equal(lowestCatalogVersion([]), 1);
    assert.equal(lowestCatalogVersion([{nativeModel: {}}]), 1);
    assert.equal(lowestCatalogVersion([owned]), 2);
    assert.equal(lowestCatalogVersion([owned, trainer]), 4);
    assert.equal(lowestCatalogVersion([trainer, limited]), 5);
});
