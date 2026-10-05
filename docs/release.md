# Release

[Back to README](../README.md#documentation)

GitHub Actions runs the complete suite hourly on a pinned `develop` commit.
The scheduled workflow uses the default branch's cron. Scheduled and manual
runs validate the pinned current `develop` commit even if it already has a
release-provenance tag. It does not run on pull requests. A manual dispatch can
force exactly one job to fail to verify diagnostic filing.

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

A failed scheduled job reports through the existing native scheduled reporter
for `digitaldrywood/detent` and its selected Detent Cloud project, following
[deployment and release failure reporting](../AGENTS.md#deployment-and-release-failure-reporting).
Under the human-approved Detent scheduled reporting policy, new proven source
or test failures blocking deployment or the scheduled validated release enter
Todo at least High when backed by reliable, pinned evidence. The selected
workflow must have dispatchable, nonterminal Todo and nondispatchable,
nonterminal Backlog, neither operator-only. Unknown instance diagnostics enter
Backlog.
The reporter matches open fingerprints and imported occurrences before filing,
appends occurrences to matching work, and raises unset, Normal or Low priority
through the existing expected-revision priority owner, preserving High and
Urgent. Origin stamps, stable problem fingerprints, pinned commit, run, attempt,
job and source evidence, occurrence replay identity, and imported history remain
durable. Reused items retain existing lanes, human questions, migration/operator
holds and terminal history; a new occurrence never moves or reopens them.
Priority updates alone do not authorize admission.

Unknown setup, startup, download, network, backend, protocol and authentication
failures remain instance-owned
intake; they authorize no source repair and consume no issue failure allowance.
A historical pinned failure does not prove that the current head fails or
staging is down; repair workers verify the failure on their current base.
A green run appends validation evidence through the same native
comment owner; it does not close issues or establish repair, landing, review
approval or completion. Other trackers and projects retain their chosen
reporting priority, admission and validation policies.

Ordinary Detent merging never waits for the scheduled suite, blocking CI or a
local-gate status. The repository's configured local gate is `true` and publishes
no validation status; focused diagnostics remain available. Validated release
tags still require all configured scheduled jobs to succeed. The full suite
includes the checks that previously ran on pull requests, including generated
sources, migrations, frontend verification, race shards,
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
configuration reference without rewriting either. The scheduled full suite
runs it on the pinned `develop` commit. Local checks remain optional diagnostics
under [AGENTS.md validation](../AGENTS.md#validation), without a mandatory
PR-worktree gate or local status publication.
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
