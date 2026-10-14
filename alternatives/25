---
name: golangci-go-toolchain-alignment
description: Diagnose golangci-lint panics or false lint floods caused by a mismatch between the active Go toolchain and the Go version used to build the linter.
when_to_use: Use when golangci-lint reports that a file requires a newer Go version, panics in go/types while loading packages, or suddenly reports widespread findings after a host tool upgrade.
---

# Align golangci-lint with the project Go toolchain

Treat `file requires newer Go version` from `go/types` as a tool provenance
failure until the project source proves otherwise. Record `go version`,
`go env GOVERSION GOROOT`, and `golangci-lint version`; the latter reports the
Go version embedded in the linter binary.

Compare those values with the versions pinned by the repository's setup target
and CI workflow. A linter built with an older Go release can ask a newer active
`go` command to select version-gated dependency files, then fail when its
embedded type checker reads them. Conversely, an unpinned newer linter can
enable or strengthen analyzers and flood an otherwise clean branch with
unrelated findings.

Build the repository-pinned linter into the task's disposable directory using
the repository-pinned Go toolchain. Put that disposable directory first on
`PATH` only for the validation command; do not replace the host's global tools.
Rerun the unchanged gate with both versions aligned. Fix findings introduced by
the current diff, but do not expand the task to repair baseline findings that
appear only under an unpinned tool pair.

If the aligned pair still fails, investigate the reported source normally. Do
not dismiss a reproducible canonical-toolchain failure as an environment issue.
