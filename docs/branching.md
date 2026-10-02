# Branching

[Back to README](../README.md#documentation)

Detent uses two long-lived branches.

- `develop` is the default integration and staging branch. Start changes from
  current `origin/develop` and land reviewed changes into `develop` under
  [AGENTS.md validation](../AGENTS.md#validation), with no local validation gate
  or local-gate status prerequisite. No GitHub Actions run
  starts for a pull request and no branch ruleset requires a status check.
- `main` remains available for deliberate production promotion. Scheduled full
  validation tags a pinned `develop` SHA and publishes release artifacts from
  that validated tag. It does not merge to `main` or deploy production.

`develop` always deploys to staging on push, even before scheduled validation.
Production installations and releases use validated version tags only.

Genuine source/test failures blocking deployment or a validated release require
at least High priority in this repository's native reporting context, preserving
Urgent, Backlog and admission holds. Historical pinned failures do not establish
that the current head fails or staging is down. See the
[failure reporting policy](../AGENTS.md#deployment-and-release-failure-reporting).

Every push to `develop` redeploys the operator staging Hub through
`.github/workflows/deploy-staging.yml`. It runs on a GitHub-hosted runner, never
for pull requests, and uses the `staging` environment, whose deployment policy
admits only `develop`. The workflow streams the built binary to a restricted SSH
key whose only command is the staging deploy script; that script rolls back when
https health does not report the pushed commit. Production is not deployed
automatically.

## Promotion

The scheduled GitHub Actions workflow validates one pinned `develop` SHA every
hour when it has new commits since the last validated tag. A green run posts
release evidence and cuts an annotated patch version tag on that SHA. Production
promotion is deliberate and separate from the scheduled validation. Do not
merge `develop` to `main` automatically.

## Hotfixes

A hotfix may target `main` directly when production cannot wait for the next
promotion. Bring the same change into `develop` right after it merges, with a
pull request from `main` into `develop` or an equivalent change, before the
next promotion.
