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
Stable production installations and releases use validated version tags.
An operator may also deploy a pinned develop commit to Cloud through the release
workflow's operator prerelease path below.

Genuine source/test failures blocking deployment or a validated release require
at least High priority in this repository's native reporting context, preserving
Urgent, Backlog and admission holds. Historical pinned failures do not establish
that the current head fails or staging is down. See the
[failure reporting policy](../AGENTS.md#deployment-and-release-failure-reporting).

A newly validated version tag dispatches `.github/workflows/release.yml`, which
publishes signed release artifacts and passes the same linux/amd64 Hub binary to
`.github/workflows/deploy-staging.yml` and then `deploy-production.yml`. Each job
runs the hosted-origin smoke and checks the deployed version and full commit. A
failed deploy or smoke stops the chain and creates or updates a High native
issue through the existing Cloud reporting owner. Logs and reporting receipts
remain Actions artifacts even if Cloud publication is unavailable.

Both GitHub deployment environments must admit validated `v*` tags. Staging uses
`STAGING_SSH_KEY` and `STAGING_SSH_KNOWN_HOSTS`; production uses
`PRODUCTION_SSH_KEY` and `PRODUCTION_SSH_KNOWN_HOSTS`. Each environment's restricted
key connects as `apprunner` at its Cloud origin and permits only `deploy COMMIT`,
streaming the binary on stdin. Provision the production forced-command owner
with the same commit verification, backup and atomic replacement contract as
staging before enabling release deployment. Host keys are pinned using the
respective legacy Hub hostname alias. Do not give either key an unrestricted
shell. `DETENT_API_KEY` must authorize the selected native reporter in both
environments; Cloud failure reporting never falls back to GitHub issue writes.

## Operator-landed Cloud deploys

Dispatch `.github/workflows/release.yml` on `develop` with `operator_landed`
enabled. The workflow pins the dispatch commit, verifies its develop ancestry,
and runs `make check-land`. It binds the completed source-validation Actions
check to the existing release provenance owner, including authenticated develop
ruleset requirements. This release path uses that check instead of the scheduled
full-suite requirement; it does not publish a local-gate status or change the
scheduled stable-release policy.

The workflow derives `v<next-patch>-op.<sha12>` from the latest published stable
release and the pinned commit, creates an annotated provenance tag, verifies its
evidence, and publishes it through the existing GoReleaser signing workflow.
The prerelease stays out of the stable latest-release installer path and package
manager feeds. Its signed runner archives and the deployed Hub binary come from
the same build. Staging deploy and smoke still precede production deploy and
smoke. Deployment environments must permit operator dispatches from `develop`
in addition to release tags.

The Hub reports the semver prerelease version. Officially installed enrolled
runners follow it through their existing verified update and coordinated drain:
new claims pause, in-flight work finishes, then the service restarts. A later
stable release of the same patch sorts above the prerelease. Operator builds are
Cloud deployments only; they do not become standalone stable releases.

Retry failed release or deployment jobs without rerunning a successful source
validation job. Provenance tags are immutable, and the existing evidence verifier
rejects an annotation superseded by a newer source check. Do not overwrite an
existing operator tag or publish signed artifacts from a workstation.

## Promotion

The scheduled GitHub Actions workflow validates one pinned `develop` SHA every
hour, even if that commit already carries a release-provenance tag. A green run
posts release evidence. It creates a new annotated patch tag and dispatches
release only when that SHA has no validated release tag. No new tag means no
release deployment that hour. The release workflow serializes the staging and
production sequence. It does not merge `develop` to `main`.

Enrolled runners receive a durable update request for their Hub's release version
on their normal heartbeat. The existing updater verifies the exact release's
minisign checksum signature, archive, release provenance and candidate binary
identity, waits for active work to finish, and replaces the installed binary.
The official launchd and systemd services restart the runner after its drained
exit. Manually started POSIX runners re-exec the installed binary. The runner
reports the update receipt and actual running build on heartbeat; the fleet
shows the outcome. Explicit operator requests and urgent updates retain their
existing owners. Standalone update settings remain independent. Automatic Hub
following needs an enrolled runner with this update protocol, a writable
installer-managed binary and its persistent update state; unsupported or
unwritable installations report a refusal rather than claiming an update.

## Hotfixes

A hotfix may target `main` directly when production cannot wait for the next
promotion. Bring the same change into `develop` right after it merges, with a
pull request from `main` into `develop` or an equivalent change, before the
next promotion.
