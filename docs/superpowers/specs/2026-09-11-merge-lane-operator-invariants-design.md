# Merge lane, operator routine, and invariants — design

Date: 2026-09-11. Status: approved by the operator in conversation; implementation is filed as flat issues on the Detent board.

## Problem

On the dogfood repository the last 40 merged pull requests took a median 2.7 hours from open to merge (p75 4.7 h, max 29 h). Detent's own merge step is not where the time goes: issues reach Merging within minutes of their first dispatch and leave it in minutes. The time is spent in the loop before a PR is mergeable: every PR was force-pushed at least once, 27 of 40 received review-bot threads on GitHub, and every fix or rebase re-ran a 22-minute Verify job. The 2026-09-10 audit also found that operator remediation (returning issues parked by retired mechanisms, clearing dependency deadlocks, moving stuck Merging issues, filing deduplicated issues, applying doctor-recommended config) was being done by hand or by a Python watcher outside Detent, and that nothing stopped a later change from re-introducing the mechanisms that had been removed.

## Decisions

1. Review happens inside the worker session before the pull request opens. The worker runs the configured review (Codex review through `gate.validator`), resolves findings, runs the gate, then opens a non-draft PR once. GitHub review bots may still comment; their threads do not gate merging.
2. Pull-request CI runs once per PR by default, without labels. The worker opens the PR as a draft while iterating and marks it ready when done; the CI workflow skips draft PRs. Label-gated CI (`gate.ci_trigger_label`, already supported) remains an opt-in for projects that want zero PR runs (parable uses it today).
3. The single merge lane per project is the GitHub merge queue where the repository has one, with batching (merge groups of up to 5 with a short collection wait), and Detent's serialized merge worker elsewhere. Detent detects which applies; it never changes repository settings. Doctor recommends a queue or batching from measured merge rate and CI duration.
4. Pull-request CI is the fast set; the merge group runs the full suite once per batch; portability, installer, and release-snapshot jobs run post-merge on main (already in place for detent).
5. The Verify job is sharded and cached so a single run is well under the 22 minutes measured today.
6. Operations move into Detent proper: an operations page and API (stats, actions taken with evidence, decisions only a human can make) and an operator routine that performs an allowlisted set of remediations when the project enables them. The Dropbox status page becomes an optional HTML export of that page; `monitor.py` and `repair.py` retire.
7. Invariants live in `docs/invariants.md`. Each has an ID, a doctor check that fails fleet health when the running system violates it, and a repository test that fails CI when code or configuration violates it.

## Per-project choice

Everything above is configuration or a workflow convention chosen by the project owner. Detent adapts to the repository's settings (queue or not, batch size, draft convention, label gate, CI layout) and only recommends. Nothing here is forced on a repository that wants CI on every push or human review before merge.

## Components

### Worker convention (WORKFLOW.md, per project)

- Open the PR as a draft before the first push; keep it a draft until the gate and the in-session review are clean; then mark ready. Never force-push after ready unless a queue removal routes the issue to Rework.
- Run the in-session review before marking ready: `gate.validator` with the project's configured model; block on the configured severities; record the review summary in the Workpad.
- Completion (`status: complete`) means: non-draft PR, references the issue, gate green, in-session review clean, no actionable bot thread.

### CI workflow (per repository)

- `pull_request` jobs carry `if: github.event.pull_request.draft == false` (fast set only).
- `merge_group` runs the same fast set plus the full suite.
- `push` to main runs portability, installer, and snapshot jobs.
- Verify is split into shards (race suite partitioned across four runners with the Go build cache keyed on `go.sum`); a single aggregate check name stays required so branch rules do not change per shard.

### Merge lane (Detent)

- Existing: native queue detection, enqueue, removal handling (#2443, #2459, #2465, #2472, #2474). Add: batching awareness (report group size and wait), and a per-issue merge-attempt budget of two queue entries before the issue is routed to Human Review with the queue's reasons (replaces, does not add to, the existing merge fallback budget).
- Doctor: recommend a merge queue when strict protection or measured head invalidation is present; recommend batching when the queue is present and merge rate times CI duration exceeds one; recommend the draft convention when PR CI runs per PR exceed 1.5 on average over 7 days. Recommendations cite the measured numbers.

### Operations page and API (Detent)

- `GET /api/v1/operations` and a dashboard page with three sections:
  - Stats (7-day and 24-hour): merges and closes per day, cycle time from first In Progress to Done (median, p75), clean-attempt rate, tokens per completed issue, distinct issues entering Blocked per night, queue depth and merge-group size, per-project dispatch counts and the top skip reasons.
  - Actions taken by the operator routine since the previous page render, each with issue, action kind, reason, and evidence link.
  - Decisions needed: issues whose latest refusal is a human decision (`detent-human` contracts, explicit human gates), each with the question and the link.
- `detent report --html <path>` renders the same data to a self-contained file; the existing launchd slot may call it to keep the Dropbox page. The page states its data time and the instance that produced it.

### Operator routine (Detent)

- A scheduled routine (default every ten minutes when enabled) with an allowlist in project config: `operator.actions: [return_retired_parks, clear_closed_dependencies, restore_stuck_merging, merge_when_wedged, file_deduped_issue, apply_doctor_config_fix]`. Disabled by default; each action kind is independently enabled.
- Every action is a lane-ledger write with reason `operator_routine:<kind>` and appears on the operations page. Actions never touch GitHub repository settings and never pause or unpause projects.
- `merge_when_wedged` merges a PR only when: gate passed, current-head CI green, no unresolved review threads, the issue has been in Merging for longer than the configured wedge threshold, and neither the queue nor the merge worker has acted in that window. It uses the repository's allowed merge method.

### Invariants (docs/invariants.md)

Initial set, one line each with an ID; the full text is in the document:

- INV-1 The orchestrator is the only writer of tracker lane state.
- INV-2 Failures before an agent's first turn attach to the instance, never to the issue.
- INV-3 No new brake, breaker, lease, park, revocation, reason code, or reconciliation loop without an invariant change.
- INV-4 Merges go through the repository's merge queue when one exists.
- INV-5 Pull-request CI runs at most once per ready head by default.
- INV-6 Workers run with an isolated Codex home; user-level instructions never reach a worker.
- INV-7 Machine-filed issues carry an origin block and a fingerprint; duplicates comment instead of creating.
- INV-8 No strict up-to-date branch protection on a repository Detent merges into.
- INV-9 Retired mechanisms stay retired (lane revocation, indeterminate-lane stops, per-issue parking for infrastructure failures, root-level `rateLimit` in mutations).

Enforcement: `internal/invariants` tests (package-boundary test that only the ledger package calls the tracker lane-write interface; reason-code allowlist; forbidden symbol list for retired mechanisms; workflow-file assertions for the CI layout on this repository) and doctor checks per invariant (`INV-n` in the check name) that read live repository and runtime state. The PR template gains an "Invariants touched" line.

## Testing

- Unit: table-driven tests for the operations aggregation, the routine's action predicates, the merge-attempt budget, and the doctor recommendations from recorded history.
- Integration: recorded GitHub fixtures for queue batching and removal; a dashboard test for the operations page.
- Live acceptance on the dogfood repository: median open-to-merge under 45 minutes and PR CI runs per PR at or below 1.2 over a 3-day window after deployment; zero human-performed remediations in that window that the routine's allowlist covers.

## Out of scope

A Detent-owned speculative merge lane; review after merge; changes to other repositories' review policies beyond the opt-in conventions above.
