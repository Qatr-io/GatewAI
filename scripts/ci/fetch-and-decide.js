'use strict';

const { extractRcShas, decideForCommit } = require('./rc-cleanup-policy.js');

const PACKAGES = ['gatewai/gateway', 'gatewai/relay'];

module.exports = async ({ github, context, core }) => {
  const owner = context.repo.owner;

  const shas = new Set();
  for (const packageName of PACKAGES) {
    const versions = await github.paginate(
      github.rest.packages.getAllPackageVersionsForPackageOwnedByOrg,
      { package_type: 'container', org: owner, package_name: packageName, per_page: 100 }
    );
    for (const sha of extractRcShas(versions)) shas.add(sha);
  }

  core.info(`Found ${shas.size} distinct rc-<sha> tag(s): ${[...shas].join(', ') || '(none)'}`);

  const toDelete = [];
  for (const sha of shas) {
    let pulls;
    try {
      const response = await github.rest.repos.listPullRequestsAssociatedWithCommit({
        owner: context.repo.owner,
        repo: context.repo.repo,
        commit_sha: sha,
      });
      pulls = response.data;
    } catch (err) {
      core.warning(`Could not look up PRs for commit ${sha}: ${err.message}. Keeping rc-${sha}.`);
      continue;
    }

    const decision = decideForCommit(pulls);
    core.info(`rc-${sha}: ${decision}`);
    if (decision === 'delete') toDelete.push(`rc-${sha}`);
  }

  core.setOutput('delete_rc_tags', toDelete.join(','));
  core.info(`Tags to delete: ${toDelete.join(', ') || '(none)'}`);
};
