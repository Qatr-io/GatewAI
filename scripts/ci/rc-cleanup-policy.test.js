'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { extractRcShas, decideForCommit } = require('./rc-cleanup-policy.js');

test('extractRcShas', async (t) => {
  await t.test('matches rc-<sha7> tags and dedups', () => {
    const versions = [
      { metadata: { container: { tags: ['rc-abc1234'] } } },
      { metadata: { container: { tags: ['rc-abc1234', 'dev'] } } },
      { metadata: { container: { tags: ['v1.2.3'] } } },
      { metadata: { container: { tags: [] } } },
      {},
    ];
    assert.deepStrictEqual(extractRcShas(versions), ['abc1234']);
  });

  await t.test('ignores tags that do not match the exact 7-hex-char shape', () => {
    const versions = [
      { metadata: { container: { tags: ['rc-abc12345', 'rc-abc123', 'rc-ABC1234'] } } },
    ];
    assert.deepStrictEqual(extractRcShas(versions), []);
  });
});

test('decideForCommit', async (t) => {
  await t.test('keeps when an open develop PR is associated', () => {
    const pulls = [{ base: { ref: 'develop' }, state: 'open' }];
    assert.strictEqual(decideForCommit(pulls), 'keep');
  });

  await t.test('deletes when all associated develop PRs are closed', () => {
    const pulls = [
      { base: { ref: 'develop' }, state: 'closed' },
      { base: { ref: 'develop' }, state: 'closed' },
    ];
    assert.strictEqual(decideForCommit(pulls), 'delete');
  });

  await t.test('keeps when no associated PR targets develop', () => {
    const pulls = [{ base: { ref: 'main' }, state: 'closed' }];
    assert.strictEqual(decideForCommit(pulls), 'keep');
  });

  await t.test('keeps when there is no associated PR at all', () => {
    assert.strictEqual(decideForCommit([]), 'keep');
  });

  await t.test('keeps when one develop PR is open and another is closed', () => {
    const pulls = [
      { base: { ref: 'develop' }, state: 'closed' },
      { base: { ref: 'develop' }, state: 'open' },
    ];
    assert.strictEqual(decideForCommit(pulls), 'keep');
  });
});
