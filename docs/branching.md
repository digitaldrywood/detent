# Branching

[Back to README](../README.md#documentation)

Detent uses two long-lived branches.

- `develop` is the default and integration branch. Start every change from
  current `origin/develop` and open the pull request against `develop`; it lands
  through the merge queue. Pushes to `develop` run the full CI suite plus the
  integration checks (portability, Windows core, installer smoke, GoReleaser
  snapshot), so `develop` carries the same evidence `main` does.
- `main` is production. Release tags point only at `origin/main`, the release
  coordinator inspects `main` regardless of the repository default branch, and
  installers and onboarding read from `main`. Failed integration checks file
  tracking issues only for `main`.

`develop` is the staging line and `main` is what production hosts install and
self-update to.

## Promotion

Promote `develop` to `main` with one pull request from `develop` into `main`
titled `release: promote develop`. Land it as a merge commit whose parents are
the previous `main` head and the promoted `develop` head, as
`digitaldrywood/pyroapex` did for its develop-to-main release pull request.
The merge commit keeps every `develop` commit reachable from `main`, so the next
promotion diff contains only new work and the release coordinator can map each
commit back to the pull request and issue that produced it. A squash promotion
would collapse that history into one commit that references no issues and
would make the next promotion conflict wherever `develop` changed the same
lines again, so the `main` merge queue must permit a merge commit for
promotion. After the promotion merges, the release coordinator tags `main` as
described in [Release](release.md).

## Hotfixes

A hotfix may target `main` directly when production cannot wait for the next
promotion. Bring the same change into `develop` right after it merges, with a
pull request from `main` into `develop` or an equivalent change, before the
next promotion.
