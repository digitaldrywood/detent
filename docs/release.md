# Release

[Back to README](../README.md#documentation)

Enrolled runners learn the target version on every heartbeat after its signed
release assets are published. Compatible releases prepare verified execution
artifacts while active workers continue and new work remains eligible. The existing
controller update owner handles atomic installation and coordinated restart.
Same-version signature repair waits for an idle reservation before controller
replacement. Critical security releases, incompatible execution
protocols and intentional operator maintenance use the existing drain owner.
A newer release replaces an update that
was refused or interrupted. Older runners keep taking work during the update: the
Hub refuses claims only from runners below its minimum supported runner version,
which changes only with a release that breaks runner and Hub compatibility.
A failed update is reported on the next heartbeat. For a current release with
missing signature evidence, run `detent update --check --json`: `repair_available`
identifies a same-version repair without claiming a newer version exists.
Run `detent update --yes` to fetch and verify the signed checksums, provenance and
platform archive with the pinned trust key, then atomically reinstall it. When
a local service is running, its existing update owner prepares compatible worker
artifacts while busy and replaces the controller once idle. A same-version Hub
repair remains actionable on subsequent heartbeats until that replacement and
restart complete. Capacity, routing, credentials and workspaces
are preserved. The install marker references a content-addressed cache of the
release archive, signed checksums and provenance. Verification rechecks the pinned-key
signature, checksums, platform, version, commit and extracted binary digest before
reporting a verified running build. Marker fields and cached update receipts alone
cannot establish verification. Losing or changing the cache leaves the build
unverified; the signed updater can recreate it. The compressed archive is read as a bounded snapshot so filesystem changes
cannot race its checksum and extraction; the expanded binary is hashed as a stream.

The Hub's heartbeat update owner also selects its published release for a
same-version unverified release install. An obsolete refused request is
superseded with a request bound to the current observed revision. The replacement
receipt retains the previous failure. After restart, the heartbeat reports the
exact verified running build. A verified CLI repair also supersedes a stale
refusal in the current Hub view while retaining the original receipt.

Older updaters that report only `up_to_date` for an unverified current release do
not support same-version repair. Upgrade the updater to a release containing
this support first. The bootstrap installer's checksum check alone does not
supply pinned-key signature evidence; a bootstrap reinstall is not proof of
verification. Unsupported platforms, unavailable signed assets and invalid
signatures or checksums remain refused. Do not edit verification metadata or
force a restart to clear that state.

Runners on 0.117.50 and earlier need a one-time manual reinstall of the release
that introduces heartbeat updates if their scheduled updater has stalled. Run
`detent update --yes --from-release` on the runner host. If the old binary cannot
coordinate with its running service, install the signed release using the
[installation instructions](../README.md#install) and restart the service.

## Validation and promotion

GitHub Actions validates pinned `develop` commits hourly. Each new SHA receives
the full suite once across scheduled and manual dispatch runs; preflight skips
completed non-cancelled runs regardless of conclusion. A manual dispatch with
`force: true`, or a non-`none` `fail_job`, runs it again. The workflow does not run
on pull requests. Green develop validation posts `develop-full-ci` evidence for
selecting a release snapshot; it does not publish a stable tag or deploy staging.

The scheduled release cut creates `release/vX.Y` from an eligible develop
snapshot. Only fixes already landed on develop are cherry-picked onto that
branch, with `-x` provenance. Pushes do not start CI; after merging a batch of
picks, dispatch the full suite, which runs on the release head's exact commit, independently of develop validation. The successful
finalizer verifies that commit is still the release head, posts authenticated
`scheduled-full-ci` evidence and creates an annotated tag in that release series.
An existing validated tag retains its original evidence. If release dispatch
was interrupted, the finalizer dispatches it when no run exists; retry failed
jobs in an existing release run instead of replacing the tag or its provenance.
See [release branches](branching.md#release-branches) for cutting, stabilizing,
validating and tagging a series, including the first-cut prerequisites.

The explicitly dispatched release workflow verifies the tag's evidence, builds
and signs release artifacts, then deploys the same Hub binary to staging and
production in order. Smoke checks verify version and full commit before the
next environment runs. The sequence is serialized without cancelling an active
release. Develop pushes do not deploy to staging and cannot replace a release
during its smoke validation. A later reviewed PR promotes the tagged snapshot
to `main` as production history; that PR does not build or deploy a new artifact.

Every release, including an urgent operator recovery, follows the release-branch
sequence above. Freeze a develop snapshot, land fixes on develop, and cherry-pick
those fixes with `-x` through PRs to the release branch. Never dispatch a develop
prerelease or substitute `make check-land` for release-branch full-suite evidence.

## Existing develop preview compatibility

The runtime retains support for existing develop builds (`develop-` versions).
This compatibility does not authorize a new staging or production deployment
from develop; use the release-branch workflow above. Develop previews start from
a disposable snapshot of the released registry, auth store and managed tenant
files. SQLite snapshots include committed WAL data and are taken
without applying migrations to the source databases. The shared entry retains
the existing release registry ownership lock, routes tenants through preview
sockets, and runs startup migrations only in the copy under
`state_directory/.develop`. A fresh develop startup discards any previous copy;
normal shutdown removes it as well. Staging writes made by a develop build are
disposable. Released builds use the original state, so reverting a develop-only
migration cannot advance the database schema that the next release must open.
Preview startup requires managed tenant allocation and refuses to route unmanaged
tenants or fall back to migrating the release databases if copying fails.

## Failure reporting and merge validation

A failed scheduled job reports through the existing native scheduled reporter
for `digitaldrywood/detent` and its selected Detent Cloud project, following
[deployment and release failure reporting](../AGENTS.md#deployment-and-release-failure-reporting).
Under the human-approved Detent scheduled reporting policy, release suites
share one open stabilization owner per release series, at least High in Todo.
Each failed job remains a bounded occurrence with its original replay identity,
including source, test, unclassified and infrastructure failures. Develop retains
per-job reporting; other projects retain their configured behavior. Infrastructure and unclassified reports carry the infrastructure label
and retain attribution to the CI instance. Other unknown instance diagnostics
remain Backlog intake. The selected
workflow must have dispatchable, nonterminal Todo and nondispatchable,
nonterminal Backlog, neither operator-only.
The reporter matches the open release-series fingerprint for releases and job
fingerprints and imported occurrences for develop before filing,
appends occurrences to matching work, and raises unset, Normal or Low priority
through the existing expected-revision priority owner, preserving High and
Urgent. Origin stamps, stable problem fingerprints, pinned commit, run, attempt,
job and source evidence, occurrence replay identity, and imported history remain
durable. Matching scheduled Backlog intake is promoted to Todo through the native
workflow owner. Preserve human questions, migration/operator holds, active and
review lanes, and terminal history; a new occurrence never reopens terminal work
or clears a hold. Reference existing repairs without restarting them or
transferring ownership; group fixes by reproduced root cause and distinguish
historical failures from current reproduction. A later failed occurrence after
a release stabilization owner completes starts a new episode; terminal replay
changes nothing. Green evidence reconciles only the matching series and does
not close repairs. Explicit operator cancellation creates no repair issues,
even when the finalizer is active before the run conclusion settles. Genuine
timeouts, infrastructure failures and cancelled jobs without workflow
cancellation remain visible; incomplete suites never permit tagging.
Priority updates alone do not authorize admission.

Unknown setup, startup, download, network, backend, protocol and authentication
failures remain instance-owned. Scheduled infrastructure reports authorize no
source repair without a reproducible source or test diagnostic and consume no
issue failure allowance.
A historical pinned failure does not prove that the current head fails or
staging is down; repair workers verify the failure on their current base.
A green run appends validation evidence through the same native
comment owner; it does not close issues or establish repair, landing, review
approval or completion. Other trackers and projects retain their chosen
reporting priority, admission and validation policies.

Workers must run `make check-land` before source completion and fix every failure.
This includes applicable affected browser journeys. The approved Cloud project
policy uses `gate.run: make check-landing` and `gate.landing_mode: per_landing`.
The short command validates the combined GitHub landing tree with a build,
short tests for changed Go packages and their dependents, and changed-file lint;
module changes widen the checks. The host also uses this approved command for
its source-finalization receipt. That receipt proves the short command, not the
worker's separate full `make check-land` obligation.

Ordinary Detent merging never waits for the scheduled suite or an additional
local-gate status. A failed landing command returns the item to Rework with its
output. Full-suite failures do not impose a repository-wide landing barrier.
Stable tags still require all configured full-suite jobs to succeed. The suite
includes the checks that previously ran on pull requests, including generated
sources, migrations, frontend verification, race shards,
security, browser visual tests, portability, and packaging snapshots.

For stable releases, the release workflow checks the annotated tag against the
tagged full commit and requires authenticated successful `scheduled-full-ci`
evidence from release-branch validation. Configure
the GitHub Actions repository variable
`DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON` to `["scheduled-full-ci"]`.
Repository branch rulesets require no status checks. The provenance verifier
also inspects active rulesets so any unexpected required check remains binding.
A missing or malformed variable, missing status, stale status ID, or fabricated
tag annotation prevents signing and publication. The workflow always requires
`scheduled-full-ci` in addition to configured release-only checks.

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

## Transition from rolling-barrier runners

Before deploying the removal of rolling-barrier execution, drain active work
through the supported runner controls and stop the old runner services on every
participating host. Verify that their barrier, repair and Git processes have
stopped. If a process is stalled, preserve its published checkpoints and handle
it through the host's existing operational controls. A Hub policy change cannot
recall Git credentials or authority already issued to a running process; leaving
an old runner active can allow retired repair or revert code to continue.

Deploy the compatible Hub and runner builds through the supported release path.
Review and re-approve the repository policy in the Hub with
`gate.landing_mode: per_landing` and `gate.run: make check-landing`; changing
`detent.yaml` alone does not replace the approved Cloud policy. Keep the worker
instruction to complete `make check-land`. Confirm the installed and running
runner identities, effective policy revision and available gate command before
resuming claims. Verify that existing work can be explained, claimed and landed
without a rolling-barrier dependency. Do not clear human holds, rewrite reviewed
versions or discard publication receipts as part of the transition.

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
generates ignored configuration references. The source-completion gate and the
full suite run it on their respective source trees; scheduled develop and
release-branch validation pin exact commits. Workers must complete
`make check-land` under [AGENTS.md validation](../AGENTS.md#validation).
No additional local status publication is required.
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
