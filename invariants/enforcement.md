# Enforcement audit and activation status

Audit date: September 9, 2026. Starting commit:
`e69f1b6e` (the initial issue worktree and origin/main matched).

## Current audit — September 11, 2026

Recovery rebased onto `ed60bbc97d1c91bfbed650b4feea99b987886688`.
Classic branch protection now returns HTTP 404. Active repository ruleset
22812552 (`main merge queue`) targets the default branch, has no bypass actors,
and reports `current_user_can_bypass=never`. It requires four Actions checks:
Lint, Verify (ubuntu-latest), Test Coverage, and Browser Visual. Strict freshness
is false; a SQUASH/ALLGREEN merge queue is enabled. Reviews remain zero with
conversation resolution required. Neither invariant check is required yet.
The worker remains `corylanou` with admin permission, including the ability to
change these controls. No server configuration changed during this recovery.
The behavioral and trusted-base policy workflows now handle merge_group events.
#2416 and #2418 are merged; question restart/concurrency, reply eligibility, and
independent rework tests are now registered. The release coordinator on main
also checks complete exact-commit evidence; the older notes below describe the
initial implementation and must not be treated as the current configuration.

## Historical server-side configuration

`gh api repos/digitaldrywood/detent/branches/main/protection` returned strict
freshness and ten required GitHub Actions checks (app ID 15368): Lint, Verify
(ubuntu-latest), Test Coverage, Browser Visual, Portability Verify (macos-latest),
Portability Verify (windows-latest), Windows Core, Installer Smoke (ubuntu-latest),
Installer Smoke (windows-latest), and GoReleaser Snapshot.

Initially administrators were exempt (`enforce_admins=false`). Required approving reviews
are zero; code owner review is disabled; conversation resolution is required.
Force push and deletion are disabled. The repository ruleset list is empty.
At the initial audit there was no merge-queue rule. See the current audit above.

The worker authenticates as `corylanou` (user ID 585100), with repository admin
permission. An agent can produce the same review identity as Cory. Enabling
admin enforcement alone does not remove the credential's ability to edit the
protection itself. This run enabled admin enforcement with the branch-protection
endpoint and read back `enforce_admins=true`, strict freshness, the unchanged ten
checks, and zero required reviews. Worker access to edit protections remains a
residual administrative bypass; identity separation is still required.

The organization reports the Team plan. Organization ruleset inspection failed
with HTTP 404 and the explicit missing `admin:org` scope diagnostic. This is not
evidence that no organization configuration exists. Repository workflow tokens
default to read permissions and cannot approve PR reviews.

## What this branch enforces locally

- `make check-invariants` requires actual passing test events for the registered
  suite, with no missing tests or skipped subtests.
- `make check` includes that gate; every ordinary CI invocation includes
  `Invariant Gate` without changed-path filters.
- Release evidence requires every manifest check to complete successfully for
  the full exact SHA and expected GitHub Actions source. API errors fail closed;
  all check-run pages are read. Duplicate matching results must all succeed.
- The release workflow invokes that verification before publishing. Release
  binaries embed the full commit rather than a short prefix.
- `invariant-policy.yml` runs on pull_request_target, checks out the trusted base,
  fetches proposed objects without executing them, and checks all changed paths
  against base policy. It has no write token, persisted checkout credential, or
  Go cache. No PR-controlled label, review, or file grants an exemption.

Protected-file changes currently fail closed, including legitimate amendments.
That is a preparation mechanism, not a complete operator exception workflow.
Existing code owner configuration does not exist; the entire `.github/` directory
is protected, including any future CODEOWNERS file.

## Remaining activation boundaries

This branch must not be described as universal merge/deploy enforcement or marked
complete while these boundaries remain:

1. Separate the worker identity from Cory's approving identity and remove worker
   administrative bypass. This is the specific question on #2417; no synthetic
   approval issue or dependency is required. Existing authorization to implement
   these ten rules remains valid.
2. Anchor policy evaluation server-side to trusted workflow provenance or a
   dedicated check-producing App inaccessible to proposed code. A required job
   name with app ID 15368 is insufficient: PR-authored Actions share that app.
   Do not claim that adding a check name establishes this trust boundary.
3. Implement authenticated, commit-scoped operator exceptions using the separated
   identity. Do not accept the current worker's `corylanou` reviews as evidence.
   Preserve ordinary automatic merging; do not add blanket review requirements.
4. Activate trusted policy after its initial approved merge, preserve the ten
   current mandatory checks, and validate an adversarial PR against server-side
   enforcement. Initial base commits have no checker; absence must fail, not be
   treated as an approved bootstrap/no-op. The bootstrap is not complete here.
5. #2416 integration is complete: its behavioral tests are registered. General
   natural-language approval scope still requires review.

GitHub documents [required workflow rules](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets)
and [required-check source and skip behavior](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks).
These distinguish server-enforced workflow provenance from a shared check name.

## Deployment paths and remaining gaps

Independently tracked follow-ups (state must be checked in GitHub): [#2419](https://github.com/digitaldrywood/detent/issues/2419)
owns automatic update provenance and running-commit verification;
[#2420](https://github.com/digitaldrywood/detent/issues/2420) owns durable release
retry/reporting and complete evidence before tag creation. Neither is a question
placeholder or a dependency added to this issue.

- `.github/workflows/release.yml`: publishes tagged main through GoReleaser,
  including package-manager repositories. This branch adds exact-commit evidence.
- `.goreleaser.yaml`: embeds the full commit, builds archives and packages, and
  signs checksum metadata. Artifact integrity is covered by existing minisign
  tamper and replacement rollback tests. Signed provenance binding checks to the
  commit is not yet consumed by every installer/updater.
- `internal/release/release.go`: coordinates tag creation. This branch disallows
  skipped/neutral success. Current main also requires complete exact-commit check
  evidence, covered by registered `TestMandatoryEvidence`. The publishing workflow
  adds its manifest gate; these guards do not establish trusted producer isolation.
- `internal/update/update.go`, `preflight.go`, and `startup_recovery.go`: verify
  checksums/signatures and version, perform startup preflight, and recover or roll
  back. They do not yet require tested full-commit evidence or verify that the
  restarted listener serves that commit. `invariantcheck -mode running -evidence
  STATE_JSON -version VERSION -head FULL_SHA` provides a tested comparison but
  is not integrated into those automatic paths.
- `go install`, local Makefile builds, package managers, direct binary replacement,
  and privileged host actions remain paths outside a universal gate. Existing
  administrative credentials can also change GitHub controls and publish assets.

No live process was restarted, replaced, stopped, or signalled for this issue.
The previously authorized release/local update is still pending its own gates.
