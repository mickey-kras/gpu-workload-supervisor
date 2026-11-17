import test from 'node:test';
import assert from 'node:assert/strict';
import {ReviewedConfiguration, draftsVersion, lowestCatalogVersion} from './review.mjs';

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
    for (const limits of [{}, null, {version: 0}, {version: 0, memoryMaxMiB: 0}])
        assert.equal(lowestCatalogVersion([{nativeModel: {owned: {port: 8000, limits}}}]), 2, JSON.stringify(limits));
});

test('serving options need catalog version 6', () => {
    const serving = {nativeModel: {owned: {port: 8000, serving: {version: 1}}}};
    const limited = {nativeModel: {owned: {port: 8000, limits: {version: 1, memoryMaxMiB: 4096}}}};
    assert.equal(lowestCatalogVersion([serving]), 6);
    assert.equal(lowestCatalogVersion([limited, serving]), 6);
    for (const options of [{}, null, {version: 0}])
        assert.equal(lowestCatalogVersion([{nativeModel: {owned: {port: 8000, serving: options}}}]), 2, JSON.stringify(options));
});

test('artifact protection needs catalog version 7', () => {
    const artifacts = {nativeModel: {owned: {port: 8000, artifacts: {version: 1}}}};
    const serving = {nativeModel: {owned: {port: 8000, serving: {version: 1}}}};
    assert.equal(lowestCatalogVersion([artifacts]), 7);
    assert.equal(lowestCatalogVersion([serving, artifacts]), 7);
    for (const options of [{}, null, {version: 0}])
        assert.equal(lowestCatalogVersion([{nativeModel: {owned: {port: 8000, artifacts: options}}}]), 2, JSON.stringify(options));
});

test('drafts version follows artifact protection, serving options and limits', () => {
    const owned = owned => ({binding: {owned}});
    assert.equal(draftsVersion([]), 1);
    assert.equal(draftsVersion([owned({port: 8000})]), 1);
    assert.equal(draftsVersion([owned({limits: {version: 1}})]), 2);
    assert.equal(draftsVersion([owned({limits: {version: 1}}), owned({serving: {version: 1}})]), 3);
    assert.equal(draftsVersion([owned({serving: {version: 1}}), owned({artifacts: {version: 1}})]), 4);
    assert.equal(draftsVersion([owned({artifacts: {version: 1}})]), 4);
});
