# In-repository invariant checks

Per Cory's September 11 scope decision on #2417, this is repository-local
behavioral enforcement. Detent uses the operator's own GitHub login. No separate
account, token, App, server-side trust boundary, or approval workflow is required.
This tool does not protect itself against edits or authenticate policy exceptions.

Run `go run ./tools/invariantcheck` (also `make check-invariants`). `make check`
and CI run this gate. Doctor runs the same command for configured source
repositories containing `invariants/policy.json`; an absent manifest means the
repository has not opted in, and produces no invariant success claim.

The only option is `-policy <path>`, defaulting to `invariants/policy.json`.
The JSON object contains `tests`, mapping relative Go package paths to nonempty
lists of exact top-level test names. Unknown fields and empty manifests fail.
The checker runs fixed `go test -json -count=1 -run <anchored names> <package>`
arguments directly, without a shell. Exit codes: 0 means every named test passed;
1 means invalid manifest, execution failure, or absent/failed/skipped evidence;
2 means invalid CLI arguments. A skipped subtest also fails the gate. Output is
human-readable diagnostics; consumers should use the exit code.

Add package-boundary tests by registering their package and exact names here.
#2479 owns `docs/invariants.md` and additional package-boundary coverage, building
on this interface. The current manifest reuses question persistence/concurrency,
scoped replies, independent rework, bounded retry, retained work, and release and
update verification tests. These tests exercise specific behavior; they cannot
prove every natural-language product decision or prevent an operator credential
from changing repository controls. Release and deployment paths retain their
existing enforcement; this issue adds no server-side guarantee.
