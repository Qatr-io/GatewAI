'use strict';

const RC_TAG_PATTERN = /^rc-([0-9a-f]{7})$/;

function extractRcShas(versions) {
  const shas = new Set();
  for (const version of versions) {
    const tags = version?.metadata?.container?.tags ?? [];
    for (const tag of tags) {
      const match = tag.match(RC_TAG_PATTERN);
      if (match) shas.add(match[1]);
    }
  }
  return [...shas];
}

function decideForCommit(pulls) {
  const developPulls = pulls.filter((pr) => pr.base.ref === 'develop');
  if (developPulls.length === 0) return 'keep';
  const hasOpen = developPulls.some((pr) => pr.state === 'open');
  return hasOpen ? 'keep' : 'delete';
}

module.exports = { RC_TAG_PATTERN, extractRcShas, decideForCommit };
