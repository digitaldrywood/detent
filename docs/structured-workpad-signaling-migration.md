# Workpad signaling contract

Detent appends the [canonical handoff](templates/blocked-handoff.md) to worker
prompts. That Markdown is embedded directly in the binary; the runner substitutes
only the current attempt ID and generation. WORKFLOW authors should remove their
copies of status examples, blocker syntax, human-question instructions, and lane
ownership rules, and refer to the appended section instead. Keep project-specific
validation and deliverable requirements in WORKFLOW.md.

The [operator WORKFLOW patch](templates/detent-orchestration-workflow.patch)
records the matching migration for the separately managed Detent orchestration
repository. It removes the duplicate protocol and contradictory lane ownership
instructions while preserving the operator's existing edits and admission rules.
Admission Criteria are project-owned WORKFLOW text, not a runner-appended block;
this change does not alter admission policy.

## Optional blocker fields

The common dependency example in the canonical handoff needs only `ref` and
`reason`. The parser supplies an issue-state predicate, orchestrator ownership,
and tick rechecks. `blocked_by` names GitHub's native dependency relation, not a
Workpad YAML field. Resolve the blocker issue's REST ID, then POST it as `issue_id`
to `repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by`. Keep an issue-body
`Depends on: owner/repo#123` line as the durable fallback. Refs accept `#N` or
`owner/repo#N`, with positive N, never a URL or bare number.

For non-default checks, blockers may also specify:

- `owner`: `orchestrator` or `human`.
- `recheck_interval`: `tick` or a positive Go duration; `expires_at`: RFC3339 time.
- `predicate`: `type` (alias `kind`), `ref` (inherits the blocker ref),
  `state`/`states`, `check`, `present`, `scope`, `resource`, `condition`, `fingerprint`.

Predicate types and required fields:

| Type | Required fields |
| --- | --- |
| `issue_state` | `ref` (may inherit) |
| `pull_request_state` | `state` or `states` |
| `check_presence` | `check`, `present` |
| `budget_capacity` | `scope`; condition defaults to `exhausted` |
| `config_fingerprint` | `fingerprint` |

Unknown YAML keys are ignored with diagnostics. In particular `repository`,
`pull_request`, `head_sha`, and `checks` do not constrain predicates. Reason-only
blockers are unverifiable and never auto-clear. When configured blocked recovery
applies to recoverable PR maintenance, `reason_code` may be `merge_conflict`,
`stale_base`, or `missing_current_head_ci`; never use these for human-only parking.

Operational completion still requires issue-body authorization before dispatch,
concrete completion evidence, and the supplied attempt identity as described in
the canonical handoff. Adding authorization at completion cannot bypass the PR gate.

## Prompt size verification

`TestPromptWrapperBytes` renders the first worker user message through BuildPrompt
with the repository's capped skills list, default follow-up and skill-creation
settings, workspace isolation, and completion identity. It subtracts only the
WORKFLOW render and requires the remaining wrapper to be under 6,000 UTF-8 bytes.
Issue bodies, project instructions, and recalled notes/knowledge are variable
content rather than fixed wrapper overhead. For a live rollout, measure the same
sections of its first user message after the updated binary is deployed; existing
rollouts retain the old text.

### Symbolic blocker references

Scheme-prefixed refs such as `instance:chrome-devtools` and
`go-workflow:ship-state-bootstrap` are accepted as instance-owned, unverifiable
blockers. Their scheme and reason remain visible in existing blocker and dispatch
diagnostics. They are never looked up as issue dependencies, and an inferred
issue-state predicate is removed. A corrected Workpad must clear the report;
these refs cannot infer tool availability or automatically clear from issue state.
Malformed refs without a scheme retain their existing diagnostics. Valid blockers
and status fields are preserved when ref diagnostics are the only defect.
