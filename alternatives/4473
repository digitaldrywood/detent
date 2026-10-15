import { describe, expect, it } from "vitest";
import * as Schema from "effect/Schema";
import { CollaborationEvent, NativeAttempt, NativeIssue } from "../../src/contracts/work.ts";
import { IssueExplanation } from "../../src/contracts/diagnostics.ts";
import { attemptIdentityLabel, diagnosticsVerdict, groupAttempts, laneClock } from "../../src/app/work/lib/diagnostics.ts";

function attempt(id: string, second: number, overrides: Record<string, unknown> = {}) {
  return Schema.decodeUnknownSync(NativeAttempt)({ attempt_id: id, lease_id: "lease", fencing_token: "1", run_id: id, policy_id: "policy", status: "failed", started_at: new Date(second * 1000).toISOString(), updated_at: new Date((second + 10) * 1000).toISOString(), identity: { role: "implementation", backend: "codex", model: "model" }, terminal_failure: { summary: "Input exceeds maximum length" }, ...overrides });
}

function transition(sequence: number, second: number, lane: string) {
  return Schema.decodeUnknownSync(CollaborationEvent)({ event_id: `event-${sequence}`, organization_id: "org", project_id: "project", aggregate_type: "work_item", aggregate_id: "wi_item", aggregate_sequence: String(sequence), type: "workflow.transitioned", schema_version: 1, recorded_at: new Date(second * 1000).toISOString(), actor: { kind: "human", principal_id: "owner" }, data: { to_state: lane } });
}

describe("diagnostics", () => {
  it.each([
    { name: "normalizes whitespace", records: [attempt("a", 1), attempt("b", 2, { terminal_failure: { summary: "  Input\n exceeds  maximum length " } })], counts: [2] },
    { name: "keeps stages separate", records: [attempt("a", 1), attempt("b", 2, { identity: { role: "merging", backend: "codex", model: "model" } })], counts: [1, 1] },
    { name: "keeps different failures separate", records: [attempt("a", 1), attempt("b", 2, { terminal_failure: { summary: "Protocol error" } })], counts: [1, 1] },
    { name: "does not cross intervening successes", records: [attempt("a", 1), attempt("b", 2, { status: "succeeded" }), attempt("c", 3)], counts: [1, 1, 1] },
    { name: "does not collapse unknown signatures", records: [attempt("a", 1, { terminal_failure: undefined }), attempt("b", 2, { terminal_failure: undefined })], counts: [1, 1] },
    { name: "sorts before comparing adjacency", records: [attempt("b", 2), attempt("a", 1)], counts: [2] },
  ])("$name", ({ records, counts }) => {
    expect(groupAttempts(records).map((group) => group.length)).toEqual(counts);
  });

  it("excludes initial Backlog, stops at Done and unions overlapping attempts", () => {
    const history = [transition(1, 0, "Backlog"), transition(2, 100, "Todo"), transition(3, 160, "In Progress"), transition(4, 220, "Done")];
    const clock = laneClock(history, [attempt("a", 160, { updated_at: new Date(200_000).toISOString() }), attempt("b", 180, { updated_at: new Date(220_000).toISOString() })], 1_000_000, true);
    expect(clock).toMatchObject({ total: 120_000, working: 60_000, system: 60_000, transitions: 3, lanes: [{ lane: "Todo", duration: 60_000 }, { lane: "In Progress", duration: 60_000 }] });
    expect(laneClock([transition(1, 0, "Backlog")], [], 1000, false)).toBeNull();
  });

  it("retains activity, usage and identity while discarding instruction contents", () => {
    const decoded = attempt("a", 1, { runtime: { phase: "completed", identity: { resolved_model: { value: "model" }, reasoning_effort: { value: "high" } }, activity: { stage: "code", coverage: "recorded", instructions: [{ name: "AGENTS.md", sha256: "digest", contents: "secret" }] } }, usage: [{ input: 3, output: 2, cost_estimate: 0.2, currency: "USD" }] });
    expect(decoded.runtime?.activity?.instructions).toEqual([{ name: "AGENTS.md", sha256: "digest" }]);
    expect(decoded.usage?.[0]?.input).toBe(3);
    expect(decoded.runtime?.identity?.reasoning_effort?.value).toBe("high");
  });

  it("names missing merge identity and uses recorded runtime identity when present", () => {
    const merge = attempt("merge", 1, { identity: { role: "merging", backend: "codex", model: "" } });
    expect(attemptIdentityLabel(merge)).toBe("identity not recorded");
    expect(attemptIdentityLabel({ ...merge, runtime: { phase: "completed", identity: { resolved_model: { value: "model" }, reasoning_effort: { value: "high" } } } })).toBe("model · high");
  });

  it("excludes time and attempt overlap after returning to Backlog", () => {
    const history = [transition(1, 100, "Todo"), transition(2, 110, "Backlog"), transition(3, 200, "Todo")];
    const clock = laneClock(history, [attempt("a", 100, { updated_at: new Date(210_000).toISOString() })], 210_000, false);
    expect(clock).toMatchObject({ total: 20_000, working: 20_000, system: 0, lanes: [{ lane: "Todo", duration: 20_000 }] });
  });

  it("reserves verdict error styling for a recorded human action", () => {
    const issue = Schema.decodeUnknownSync(NativeIssue)({ work_item_id: "wi_item", organization_id: "org", project_id: "project", number: 1, revision: "1", profile: "native", title: "Held", web_url: "", body: "", state: "Blocked", terminal: false, labels: [], assignees: [], actor: { kind: "human", principal_id: "owner" }, created_at: "at", updated_at: "at", dependencies: [], blockers: [], external_references: [] });
    const explanation = Schema.decodeUnknownSync(IssueExplanation)({ observed_at: "at", current_lane: { name: "Blocked", freshness: "available" }, eligibility: { state: "refused", source_state: "available", current: { state: "refused", reason: "Capacity exhausted", at: "at" } }, required_gate: { state: "pending", source_state: "available" }, reasons: [], sources: [] });
    expect(diagnosticsVerdict(issue, explanation, undefined).variant).toBe("secondary");
    expect(diagnosticsVerdict(issue, { ...explanation, required_gate: { ...explanation.required_gate, human_action: "Reduce instruction size." } }, undefined).text).toBe("This issue is Blocked: Capacity exhausted.");
    expect(diagnosticsVerdict(issue, { ...explanation, required_gate: { ...explanation.required_gate, human_action: "Reduce instruction size." } }, undefined).variant).toBe("error");
    expect(diagnosticsVerdict({ ...issue, state: "Done", terminal: true }, null, undefined).text).toBe("This issue is Done.");
  });
});
