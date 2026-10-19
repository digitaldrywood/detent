# Branching

[Back to README](../README.md#documentation)

Detent uses two long-lived branches.

- `develop` is the default integration branch. Start changes from
  current `origin/develop` and land reviewed changes into `develop` under
  [AGENTS.md validation](../AGENTS.md#validation), including the configured
  per-landing command and no additional local-gate status prerequisite. No GitHub Actions run
  starts for a pull request and no branch ruleset requires a status check.
- `main` records deliberate production promotions. Stable releases are tagged
  from a green `release/vX.Y` head, and production uses artifacts built from that
  validated tag. Promotion to `main` is an operator-reviewed PR.
- `release/vX.Y` branches stabilize a fixed develop snapshot. They accept only
  fixes already landed on develop, cherry-picked with `git cherry-pick -x`.
  Develop landing never waits for a release branch.

The release workflow owns both staging and production deployment. It serializes
the complete sequence without cancelling an active release when newer work
arrives. Staging consumes the release's signed build artifact; develop pushes
cannot replace it during smoke validation. Stable production installations and
releases use validated version tags. Every release starts by cutting and freezing
a `release/vX.Y` branch from develop. Fixes land on develop first and reach the
frozen branch only through reviewed `git cherry-pick -x` PRs. Only a green full
suite on that release branch permits tagging, staging and then production.
There is no operator develop prerelease or direct develop deployment path.

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

## Release branches

Detent release full-suite failures share one open native stabilization owner per
release series. Every failed job retains bounded run, attempt, pinned SHA, log,
command and replay evidence on that owner. Reference existing repair work without
restarting it or transferring ownership; group fixes by reproduced root cause
and validate one integrated candidate using applicable exact-source receipts.
Historical diagnostics do not prove a current failure. Later failed occurrences
after completion start a new episode; terminal replay changes nothing. Green
evidence reconciles only that series and never closes repairs. Explicit operator
cancellation creates no repair issues; genuine timeouts and incomplete suites
remain release-ineligible. Develop landing remains independent.

### Cut

`.github/workflows/release-cut.yml` cuts a branch weekly by default (Monday at
06:37 UTC). Adjust its cron to change the cadence. Manual dispatch accepts a
`version` (`vX.Y`) and a develop `sha`; both may be omitted. The default version
is the next minor after the highest stable tag or existing release series, or
`v0.1` when neither exists. An existing branch is never overwritten.

Before selecting source and again immediately before branch creation, the cut
owner checks durable Actions history. Automatic cuts (scheduled runs and manual
dispatches without an explicit SHA) succeed as a no-op when another cut, a
release-branch full suite, or the release workflow is queued, requested, pending,
running or waiting for approval. The release workflow covers signed builds and
both deployments. The current cut run and ordinary develop CI are excluded.
The existing `release-cut` concurrency group serializes cut decisions; the
`validated-release` group continues to serialize deployment without cancellation.
Each no-op prints its reason and the existing run URL, source/tag and SHA. An
Actions API read failure fails the cut instead of reporting a no-op.

A successful production deployment, origin smoke and MCP smoke also satisfy an
automatic release opportunity for one release cadence, even when newer develop
commits have arrived. Admission reads the production job of the successful
`release.yml` run's current attempt, rather than treating a branch, tag, cut or
artifact upload as production success. Failed or cancelled attempts do not start
the window; retrying an existing release retains its immutable evidence rules.
After the cadence window expires, the current frozen series still owns release
work until its branch head has a successful stable-tag production deployment
and both smokes. Failed or inactive release CI, tagged deployment failure, and
a branch advanced beyond the last deployed tag all make the automatic cut a
successful no-op referencing that branch and source SHA. Admission reuses the
same production receipts outside the cadence window and checks current remote
branch heads at each decision. A later successfully deployed series supersedes
older leftover branches, so historical abandoned branches cannot hold future
cuts indefinitely. Explicit manual SHA requests retain their bypass.

The rolling UTC window is `(decision time - cadence, decision time]`: completion
at the lower boundary is expired, and completion at the decision time counts.
Cadence comes from the single numeric five-field cron in `release-cut.yml`, as
the elapsed time between its two latest scheduled occurrences at or before the
decision time. The current weekly cron therefore supplies seven days, rather
than an independent hourly setting. The window and cron appear in Actions output;
an invalid or unsupported cadence fails admission.

For a needed newer fix during this window, manually dispatch the cut with an
explicit develop SHA. That request bypasses automatic admission while retaining
source coverage, immutable branches and full release-head validation. To recover
a failed existing series, retry its failed CI or release jobs instead of cutting
the covered source again. Scheduled no-ops do not suppress the hourly develop
suite for a newly untested SHA, and never hold develop landings.

The default source is the newest develop ancestor not already contained in an
existing release series or stable version tag, with successful full-suite CI
and matching `develop-full-ci` evidence from that run. Legacy
`scheduled-full-ci` evidence is accepted only from an authenticated develop run.
A successful
preflight skip is not evidence. If no uncovered green ancestor exists, use the
unreleased develop tip. If develop tip is already covered, the cut succeeds
without creating a branch or dispatching CI. An explicit
source must be an ancestor of develop. For the first cut after introducing this
flow, select a develop SHA containing the release workflows, scripts and
`CI_RELEASE_BRANCH` handoff. The cut refuses an incompatible snapshot before
creating the branch.

Coverage includes a release series still awaiting its first release. An explicit
SHA already contained in a release series or stable version tag is refused;
the cut never substitutes a different commit for an explicit request. Historical
operator prerelease tags do not prevent the first stable release of their source.
Every new cut still runs the full suite on its release branch before stable tagging.

```bash
gh workflow run release-cut.yml --ref develop
gh workflow run release-cut.yml --ref develop -f version=v1.2 -f sha=<develop-sha>
```

The cut owner creates the branch and explicitly dispatches `ci.yml` on it,
because GitHub's workflow token does not trigger push workflows when it creates
a ref. Pushes and merged cherry-pick PRs do not start CI; after merging a
batch of picks, dispatch the full suite on the release branch. Cutting and fixing a release do not freeze develop.

### Stabilize

Land fixes on develop first. From a clean, dedicated linked worktree, run the
script from a checkout containing this flow:

```bash
scripts/release-pick.sh <develop-fix-sha> release/v1.2
```

The script fetches develop and the target, refuses commits outside develop,
creates a pick branch from the release head, cherry-picks with `-x`, pushes, and
opens a PR to the release branch. Review and validate the fix before merging.
A conflict leaves the pick in progress without pushing or opening a PR. Resolve
it, continue the cherry-pick, validate, then push and open the PR against the
release branch. Picks go only from develop to release; do not merge a release
branch back into develop or land new features on it.

### Validate and tag

The hourly scheduled workflow continues to validate pinned develop commits.
Each develop SHA receives a full suite once across scheduled and manual runs;
preflight skips completed non-cancelled runs regardless of conclusion. Set
`force: true` or select a non-`none` `fail_job` to rerun. Green develop evidence
is published as `develop-full-ci`, helps select the next cut and does not
publish a stable release.

Every release dispatch runs the same full suite on the event's exact
SHA, with a separate concurrency group from develop and other release branches.
A release run never reuses develop evidence or skips because that SHA ran
elsewhere. Only its successful full-suite finalizer publishes
`scheduled-full-ci`, verifies that the validated SHA is still the release head,
and creates an annotated provenance tag. The first tag is `vX.Y.0`; subsequent
validated heads increment the patch within the same series. A commit already
tagged in that series receives no new tag or replacement evidence. A retry
authenticates the tag's original status, refuses superseded evidence, and
dispatches the release workflow only if no run exists for the tag. Retry failed
jobs in an existing release run. A moved release head must pass its own full
suite before tagging.

To explicitly validate and tag the current release head:

```bash
gh workflow run ci.yml --ref release/v1.2 -f force=true
gh run list --workflow ci.yml --branch release/v1.2
gh run watch <run-id> --exit-status
git fetch origin release/v1.2:refs/remotes/origin/release/v1.2 --tags
git tag --points-at origin/release/v1.2 --list 'v1.2.*'
gh release view v1.2.0
```

Do not create or overwrite stable tags locally. The existing `release.yml`
builds from the validated tag, preserves Hub and runner version stamping,
publishes signed artifacts, and deploys staging with smoke before production
with smoke. The finalizer explicitly dispatches it because a tag pushed by the
workflow token does not trigger another push workflow. No Detent runner calls
the GitHub API for this flow; only operators and GitHub Actions own it.

## Promotion

Promote the tagged snapshot to `main` through a normal reviewed PR after the
release succeeds. Pin the tag so later release picks cannot change the PR's
source. In a fresh linked worktree, merge the tag into main history and verify
that the resulting tree exactly matches the validated tag:

```bash
git fetch origin main --tags
git worktree add -b promote/v1.2.0 ../detent-promote-v1.2.0 origin/main
cd ../detent-promote-v1.2.0
git merge --no-ff --no-edit v1.2.0
git diff --exit-code v1.2.0 HEAD --
git push -u origin promote/v1.2.0
gh pr create --base main --head promote/v1.2.0 --title 'chore(release): promote v1.2.0' --body 'Promote the validated v1.2.0 snapshot. The promotion tree matches the release tag.'
```

Resolve any equivalent cherry-pick conflicts before continuing and retain the
exact tag tree. If main contains a production hotfix absent from the tag, bring
it into develop and pick it onto the release branch, then validate a new patch
before promotion. Do not deploy an unvalidated merge tree. Merging this PR
records production history; the tag release workflow owns production artifacts
and deployments. Later patches repeat the same flow on the existing series.

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
promotion. Bring the same change into `develop` right after it merges, then
cherry-pick its develop commit onto the active release branch before the next
validated patch or promotion. Do not use a release branch as a source for a
develop pick.
