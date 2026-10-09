# Release

[Back to README](../README.md#documentation)

Enrolled runners learn the target version on every heartbeat after its signed
release assets are published. They stop taking new work, finish active leases,
install that exact release and restart. A newer release replaces an update that
was refused or interrupted. Older runners keep taking work during the update: the
Hub refuses claims only from runners below its minimum supported runner version,
which changes only with a release that breaks runner and Hub compatibility.
A failed update is reported on the next heartbeat and the runner card requests
human help with the manual reinstall command:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh
```

Restart the runner service after a manual reinstall.

Runners on 0.117.50 and earlier need a one-time manual reinstall of the release
that introduces heartbeat updates if their scheduled updater has stalled. Run
`detent update --yes --from-release` on the runner host. If the old binary cannot
coordinate with its running service, install the signed release using the
[installation instructions](../README.md#installation) and restart the service.

GitHub Actions checks the current `develop` SHA hourly on a pinned commit. Each
new SHA receives the complete suite once across scheduled and manual dispatch
runs. Preflight skips a SHA with any completed non-cancelled run of `ci.yml`,
regardless of event or conclusion, so launchd dispatches deduplicate with cron.
A manual dispatch can set `force: true` to rerun that SHA; selecting `fail_job`
also forces the suite and can force exactly one job to fail to verify diagnostic
filing. It does not run on pull requests.

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

Develop builds (`develop-` versions) on `https://staging.cloud.detent.build`
start from a disposable snapshot of the released registry, auth store and
managed tenant files. SQLite snapshots include committed WAL data and are taken
without applying migrations to the source databases. The shared entry retains
the existing release registry ownership lock, routes tenants through preview
sockets, and runs startup migrations only in the copy under
`state_directory/.develop`. A fresh develop startup discards any previous copy;
normal shutdown removes it as well. Staging writes made by a develop build are
disposable. Released builds use the original state, so reverting a develop-only
migration cannot advance the database schema that the next release must open.
Preview startup requires managed tenant allocation and refuses to route unmanaged
tenants or fall back to migrating the release databases if copying fails.

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

## Verify release signatures

The release job requires `MACOS_SIGN_P12` (base64 Developer ID Application
certificate), `MACOS_SIGN_PASSWORD`, `MACOS_NOTARY_KEY` (base64 App Store Connect
API key), `MACOS_NOTARY_KEY_ID`, and `MACOS_NOTARY_ISSUER_ID`. Missing signing or
notarization secrets stop the release before publication.
GoReleaser's Darwin post-build hook runs quill with the fixed signing identifier
and waits for notarization before archiving. Rejected or timed-out submissions
stop the release. This hook uses the quill CLI because `notarize.macos` derives
the signing identifier from the binary filename and cannot override it.
Snapshots skip the hook and require no Apple credentials.

### macOS binaries

Extract the darwin archive for your architecture and inspect its binary on a
Mac:

```sh
codesign -dv --verbose=4 ./detent
codesign --verify --strict ./detent
codesign -dr - ./detent
```

Expect `Identifier=build.detent.detent`, `TeamIdentifier=5UMY5CG893`, a
Developer ID Application authority for Drywood Creek Consulting, Inc., a secure
timestamp, and the `runtime` flag. Verification must exit successfully. The
designated requirement should bind the fixed identifier to the Apple Developer
ID certificate and team, so successive signed releases retain that identity.

An operator with the App Store Connect key can confirm Apple's accepted
submissions for both release architectures:

```sh
xcrun notarytool history \
  --key ./AuthKey.p8 \
  --key-id "$MACOS_NOTARY_KEY_ID" \
  --issuer "$MACOS_NOTARY_ISSUER_ID"
xcrun notarytool info "$SUBMISSION_ID" \
  --key ./AuthKey.p8 \
  --key-id "$MACOS_NOTARY_KEY_ID" \
  --issuer "$MACOS_NOTARY_ISSUER_ID"
```

Match the submission IDs from the release job to history and require `Accepted`
for each architecture. To verify App Management grant continuity, install one
signed release, grant its requested permission, then replace it with the next
signed release at the same path and repeat the operation that required the
grant. It should succeed without another permission prompt. A migration from
an older ad-hoc signed binary may require a new grant once.

### Checksums

Each release publishes the existing minisign signature
`detent_<version>_checksums.txt.minisig` and an additional keyless cosign bundle
`detent_<version>_checksums.txt.sigstore.json`. Minisign keys, release provenance,
and the self-updater's minisign verification remain unchanged. Linux artifacts
are covered by the signed checksums; Windows executable signing is not enabled.

Download the checksums, cosign bundle, and desired archives from the same
release. Set `tag` to the exact release tag you intend to trust, then verify
with cosign v3:

```sh
tag=v1.2.3
checksums="detent_${tag#v}_checksums.txt"
cosign verify-blob \
  --bundle "$checksums.sigstore.json" \
  --certificate-identity "https://github.com/digitaldrywood/detent/.github/workflows/release.yml@refs/tags/$tag" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "$checksums"
```

Require successful signature verification before comparing downloaded files to
the checksums. On Linux use `sha256sum --check --ignore-missing "$checksums"`;
on macOS use `shasum -a 256 <archive>` and compare the result to that archive's
entry. The bundle includes the signing certificate and transparency-log
evidence. Trust the exact workflow and tag identity above, rather than an
identity copied from an unverified certificate. See the
[GoReleaser cosign configuration](https://goreleaser.com/customization/sign/sign/#signing-with-cosign)
and [Sigstore blob signing documentation](https://docs.sigstore.dev/cosign/signing/signing_with_blobs/).

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
`make check-generated` checks committed SQL output with `sqlc diff` and
generates ignored configuration references. The scheduled full suite runs it on
the pinned `develop` commit. Local checks remain optional diagnostics
under [AGENTS.md validation](../AGENTS.md#validation), without a mandatory
PR-worktree gate or local status publication.
GoReleaser builds committed Go sources and generates documentation, dashboard
CSS and the conversation client through `make assets generate-docs` from that
exact tag's npm lockfiles using Node 24. Conversation output is ignored in feature commits. Staging and private operator builds use
the same entry point before Go compilation.

GoReleaser publishes `detent_<version>_source.tar.gz`, containing the exact
tagged source, generated documentation, dashboard CSS, every conversation asset
and `BUILD_LDFLAGS` with the version, full commit and build date. It is included in the existing signed
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
`make build`, or `make assets` before a custom Go build. No public prebuilt bundle
is substituted for private source.
