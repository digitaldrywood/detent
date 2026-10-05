# In-repository invariant checks

This is repository-local behavioral enforcement. Detent uses the operator's
own GitHub login. No separate account, token, App, server-side trust boundary,
or approval workflow is required.
This tool does not protect itself against edits or authenticate policy exceptions.

Run `go run ./tools/invariantcheck` (also `make check-invariants`). The optional
`make check` targets and scheduled CI include this check; ordinary submission
and merge follow [AGENTS.md validation](../AGENTS.md#validation). Doctor runs
the same command for configured source repositories containing
`invariants/policy.json`; an absent manifest means the
repository has not opted in, and produces no invariant success claim.

The only option is `-policy <path>`, defaulting to `invariants/policy.json`.
The JSON object contains `tests`, mapping relative Go package paths to nonempty
lists of exact top-level test names. Unknown fields and empty manifests fail.
The checker runs fixed `go test -json -count=1 -run <anchored names> <package>`
arguments directly, without a shell. Exit codes: 0 means every named test passed;
1 means invalid manifest, execution failure, or absent/failed/skipped evidence;
2 means invalid CLI arguments. A skipped subtest also fails the gate. Output is
human-readable diagnostics; consumers should use the exit code.

The [repository invariants](../docs/invariants.md) register package-boundary,
reason-vocabulary, retired-mechanism, and workflow tests from `internal/invariants`
through this same interface. The manifest names the behavioral tests to execute;
it does not require or inspect a per-change `docs/invariants.md` edit. Only a
change to an invariant's rule or its enforcing check edits that document.
Per-change rationale, evidence and verification belong in the Change/PR
description or issue comments through the authorized tracker owner (INV-16).
These tests exercise specific behavior; they cannot prove every
natural-language product decision or prevent an operator credential
from changing repository controls. Release and deployment paths retain their
existing enforcement.
