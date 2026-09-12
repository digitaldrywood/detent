# Release

[Back to README](../README.md#documentation)

The release coordinator cuts releases from `main` after every configured
mandatory check succeeds for the exact candidate commit. It creates an
annotated semver tag containing that check evidence. Plain manually-created
tags do not carry this provenance and the release workflow rejects them.

Tags matching `v*` trigger the release workflow, which validates the annotated
tag against the tagged full commit, runs GoReleaser, and
publishes the GitHub Release archives, checksums, Homebrew formula, and Windows
package-manager manifests. Scoop publishing targets
`digitaldrywood/scoop-bucket`; Winget publishing pushes to the
`digitaldrywood/winget-pkgs` fork and opens a pull request against
`microsoft/winget-pkgs`. GoReleaser generates the manifests during snapshots and
skips publishing when `SCOOP_BUCKET_GITHUB_TOKEN` or `WINGET_GITHUB_TOKEN` is
not configured.

CI runs `GoReleaser Snapshot` on pushes to `main` and manual workflow dispatch
to validate packaging after merge. It is not a PR-required check and does not
run on tag pushes or the nightly CI schedule. See [Merge Train](merge-train.md)
for the required-check split and main failure tracking.
Required branch checks must not pass as path- or event-dependent no-ops on pull
requests when the same check name runs real validation on `main`.

## Automatic coordination

The coordinator enforces `gate.required_status_checks` together with any
additional `release.required_check_names`. Configure the complete list of mandatory
check-run and status-context names for release candidates. Names are exact and
case-sensitive. An empty combined manifest pauses automatic tagging. Every mandatory check must be present
for the exact candidate SHA, completed, and successful. Missing or truncated
responses, stale evidence, and cancelled, neutral, or skipped checks keep tagging
closed. The coordinator continues to reject failing optional checks as well.

When `release.rerun_flaky_once` is enabled, a durable intent comment on the
originating issue reserves the existing single rerun before the request is sent.
A restart or lost acknowledgment never grants another automatic rerun. If the
process stops between reservation and dispatch, the reservation remains consumed;
the issue report records the uncertainty for operator investigation. Current
candidate checks are inspected again on every evaluation.

The coordinator's annotation is materialized as
`detent_release_provenance.json`. GoReleaser includes that manifest and every
platform archive in the same signed checksum file, and embeds the full commit
in each binary. Before the signing key is available, the release workflow
re-reads active default-branch ruleset requirements plus authenticated check-run
and status evidence from GitHub. The annotation must include every repository
requirement, and every declared ruleset or release-only check must identify
successful evidence for the tagged commit; policy drift, fabricated names, stale
runs, or incomplete API results fail the release.
The updater fails closed if the signature, provenance checksum,
repository, tag, full commit, or successful mandatory-check evidence is absent
or inconsistent. Before replacement it executes the staged binary and requires
its version and full commit to match the signed provenance. After restart,
startup recovery requires both identities from the running instance before it
marks the update healthy or removes rollback material.

## Host-admin update boundaries

Detent enforces signed provenance for its release-managed self-update path,
including an explicit release swap requested for a `go install` binary. It does
not replace binaries managed by Homebrew, Scoop, Winget, deb/rpm packages, or a
normal `go install`; automatic update reports the appropriate external command
instead. Package-manager upgrades, direct `go install`, manual binary copies,
and service-manager deployment are host-administrator actions outside product
enforcement. Administrators are responsible for validating the package source
and confirming the restarted binary's full commit for those paths.

Release progress and blockers use fingerprinted comments on the first sorted
originating issue reference, without creating new coordination issues. Comment
reconciliation reads all pages directly, avoiding search-index lag. Origin
references are preserved in annotated tag metadata for reporting after restart.
Tag publication reconciles the exact tag target before mutation and after an
uncertain response; a tag pointing elsewhere is a failure. These operations rely
on Detent's single service owner per project; concurrent evaluations within that
owner are serialized.
