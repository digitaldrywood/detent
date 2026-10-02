# Release

[Back to README](../README.md#documentation)

GitHub Actions runs the complete suite hourly on a pinned `develop` commit
when there are new commits since the last validated tag. The scheduled workflow
uses the default branch's cron. It does not run on pull requests. A manual
dispatch can force exactly one job to fail to verify issue filing and recovery.

A green run posts a `scheduled-full-ci` commit status, cuts an annotated patch
version tag on the validated commit, and dispatches the release workflow. The
annotation records the exact authenticated status ID. The release workflow
verifies that provenance and publishes the GitHub Release archives, checksums,
Homebrew formula, and Windows package-manager manifests. The tag is the
release candidate; unvalidated `develop` commits are not production releases.
The scheduled run does not merge `develop` into `main` or deploy production.

Every push to `develop` still deploys to staging, whether the scheduled full
suite passes or fails. Staging is an integration environment, not a production
release. Production installations and releases use validated version tags.

A failed scheduled job opens or updates one fingerprinted Todo hotfix issue.
The next green run closes the open scheduled-failure issues. The workflow skips
its full suite when the current development commit already has a validated
tag. The full suite includes the checks that previously ran on pull requests,
including generated sources, migrations, frontend verification, race shards,
security, browser visual tests, portability, and packaging snapshots.

The release workflow checks the annotated tag against the tagged full commit
and requires authenticated successful `scheduled-full-ci` evidence. Configure
the GitHub Actions repository variable
`DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON` to `["scheduled-full-ci"]`.
Repository branch rulesets require no status checks. The provenance verifier
also inspects active rulesets so any unexpected required check remains binding.
A missing or malformed variable, missing status, stale status ID, or fabricated
tag annotation prevents signing and publication.

GoReleaser generates the package manifests during snapshots and skips
publishing when the corresponding package-manager token is unavailable. The
Scoop bucket uses `SCOOP_BUCKET_GITHUB_TOKEN`; Winget uses
`WINGET_GITHUB_TOKEN`. The release workflow is explicitly dispatched after tag
publication because tags pushed by `GITHUB_TOKEN` do not start another workflow
automatically.

The updater fails closed if the signature, provenance checksum, repository,
tag, full commit, or successful mandatory-check evidence is absent or
inconsistent. Before replacement it executes the staged binary and requires
its version and full commit to match the signed provenance. After restart,
startup recovery requires both identities from the running instance before it
marks the update healthy or removes rollback material.

## Host-admin update boundaries

Detent enforces signed provenance for its release-managed self-update path,
including an explicit release swap requested for a `go install` binary. It does
not automatically replace binaries managed by Homebrew, Scoop, Winget, deb/rpm
packages, or `go install`; automatic update reports the appropriate external
command instead. For a Go-managed binary, an explicit `detent update --yes` (or
legacy Go-install option) now reports the prepared source archive build path.
Go module archives no longer contain conversation build output. Using
`detent update --from-release` instead switches to the release-managed path and
requires its provenance checks.

Package-manager upgrades, prepared-source builds, manual binary copies,
and service-manager deployment are host-administrator actions outside signed
release provenance enforcement. Administrators are responsible for validating
the package source and confirming the restarted binary's full commit for those
paths.

Release progress and blockers use fingerprinted comments on the first sorted
originating issue reference, without creating new coordination issues. Comment
reconciliation reads all pages directly, avoiding search-index lag. Origin
references are preserved in annotated tag metadata for reporting after restart.
Tag publication reconciles the exact tag target before mutation and after an
uncertain response; a tag pointing elsewhere is a failure. These operations rely
on Detent's single service owner per project; concurrent evaluations within that
owner are serialized.

### Generated sources

The Makefile pins sqlc in `SQLC_VERSION`; `make generate`, `make sqlc`, and
`make setup` use that version. Commit regenerated SQL output with query changes.
`make check-generated` checks SQL output with `sqlc diff` and checks the generated
configuration reference without rewriting either. The local `make check-fast` target runs it in each PR worktree, and the
scheduled full suite runs it on the pinned `develop` commit.
GoReleaser builds committed Go sources and regenerates the conversation client
through `make app` from that exact tag's npm lockfile using Node 24. Conversation
output is ignored in feature commits. Staging and private operator builds use
the same entry point before Go compilation.

GoReleaser publishes `detent_<version>_source.tar.gz`, containing the exact
tagged source, every generated conversation asset and `BUILD_LDFLAGS` with the
version, full commit and build date. It is included in the existing signed
checksums alongside the binary archives. Go-only consumers build this prepared
archive; automatic GitHub source archives and Go module downloads do not
contain ignored output. The tag and signed provenance still refer to the
validated source commit, never a later generated-output commit. This trades
module-proxy installation convenience for source-only feature integration and
a complete Go-only release-source build.

The shell and PowerShell installers use this checksum-verified source archive
when the platform binary asset is unavailable. Local checkout installation
regenerates assets before compiling; prepared source installation uses its
packaged assets and build identity. Private operator source compositions use
`make build`, or `make app` before a custom Go build. No public prebuilt bundle
is substituted for private source.
