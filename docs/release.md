# Release

[Back to README](../README.md#documentation)

Cut releases from `main` by pushing a semver tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

Tags matching `v*` trigger the release workflow, which runs GoReleaser and
publishes the GitHub Release archives, checksums, Homebrew formula, and Windows
package-manager manifests. Scoop publishing targets
`digitaldrywood/scoop-bucket`; Winget publishing pushes to the
`digitaldrywood/winget-pkgs` fork and opens a pull request against
`microsoft/winget-pkgs`. GoReleaser generates the manifests during snapshots and
skips publishing when `SCOOP_BUCKET_GITHUB_TOKEN` or `WINGET_GITHUB_TOKEN` is
not configured.

CI runs `GoReleaser Snapshot` after merges to `main`, on `v*` release tags, on
pull requests, on the nightly schedule, and from manual workflow dispatch so
release packaging is validated before the PR merge lane and after it lands.
Required branch checks must not pass as path- or event-dependent no-ops on pull
requests when the same check name runs real validation on `main`.
