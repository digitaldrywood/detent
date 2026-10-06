const at = (time) => `2026-10-01T${time}Z`;

async function installDiagnostics(page, fixture, state, unavailable = false, hash = "") {
  const terminal = state === "Done";
  const action = "Reduce the instruction payload before resuming this issue.";
  const reason = terminal ? "The reviewed change landed" : "Instruction payload exceeds the provider limit";
  await page.clock.install({ time: new Date(at("10:10:00")) });
  await page.route(`**/work-items/${fixture.work_item}`, async (route) => {
    const response = await route.fetch();
    const issue = await response.json();
    await route.fulfill({ json: { ...issue, title: "Investigate instruction payload limit", body: "Make the held issue and its recorded attempts understandable.", state, terminal, dependencies: [], blockers: [], updated_at: at("10:08:12") } });
  });
  const attempt = (index) => ({
    attempt_id: `attempt-${index}`, lease_id: "lease", fencing_token: "1", run_id: `run-${index}`, policy_id: "policy",
    status: terminal ? "succeeded" : "failed", started_at: at(`10:0${6 + index}:00`), updated_at: at(`10:0${6 + index}:12`),
    identity: { role: "code", backend: "codex", model: "gpt-6.1-sol" }, runtime_freshness: "last_known",
    outcome: terminal ? "complete" : "provider error",
    ...(terminal ? {} : { terminal_failure: { summary: index === 1 ? "codex turn/start:\n Input exceeds the maximum length of 1048576" : "codex turn/start: Input exceeds the maximum length of 1048576" }, disposition: { status: "blocked", human_action: true, final_summary: action } }),
    usage: [{ input: 1200, output: 800, cost_estimate: 0.01, currency: "USD" }],
    runtime: { phase: "completed", identity: { resolved_model: { value: "gpt-6.1-sol" }, reasoning_effort: { value: "high" } }, activity: { stage: "code", coverage: "recorded", instructions: [{ name: "AGENTS.md", sha256: "a".repeat(64) }, { name: "CLAUDE.md", sha256: "b".repeat(64) }], timing_summary: { breakdown: { elapsed_seconds: 12, observed_seconds: 10, unknown_seconds: 2, concurrent_seconds: 0, by_kind_seconds: { tool_execution: 4, provider_response: 6 } } } } },
  });
  const attempts = terminal ? [attempt(0)] : [attempt(0), attempt(1), attempt(2)];
  await page.route("**/work-items/*/attempts?*", (route) => unavailable ? route.fulfill({ status: 503, json: { message: "Attempts unavailable" } }) : route.fulfill({ json: { items: attempts } }));
  await page.route("**/work-items/*/history?*", (route) => route.fulfill({ json: { items: [
    ["2026-09-30T21:00:00Z", "Backlog", "Created"], [at("10:00:00"), "Todo", "Admitted"], [at("10:06:00"), "In Progress", "Runner claimed"], [at("10:08:12"), state, reason],
  ].map(([recorded_at, to_state, transitionReason], index) => ({ event_id: `transition-${index}`, organization_id: "org", project_id: fixture.project_id, aggregate_type: "work_item", aggregate_id: fixture.work_item, aggregate_sequence: String(index + 1), type: "workflow.transitioned", schema_version: 1, recorded_at, actor: { kind: "human", principal_id: "owner" }, data: { to_state, reason: transitionReason } })) } }));
  await page.route("**/work-items/*/changes", (route) => route.fulfill({ json: [] }));
  await page.route("**/work-items/*/explanation", (route) => unavailable ? route.fulfill({ status: 503, json: { message: "Explanation unavailable" } }) : route.fulfill({ json: {
    observed_at: at("10:10:00"), current_lane: { name: state, freshness: "available" }, latest_transition: { at: at("10:08:12"), reason },
    eligibility: { state: "refused", source_state: "available", current: { state: "refused", reason: terminal ? "Terminal lane" : reason, at: at("10:08:12") } },
    required_gate: { state: terminal ? "passed" : "pending", source_state: "available", reason: terminal ? "landed" : "No current change version", ...(terminal ? {} : { human_action: action }) },
    reasons: [], sources: [{ name: "native_item", state: "available" }, { name: "native_attempt", state: "available" }, { name: "runtime", state: "last_known" }, { name: "historical_scheduler_decision", state: "unavailable" }],
    native_runtime: { capacity: [{ runner_id: "runner-studio", observed_at: at("10:08:12"), available: 2, health: "healthy", exclusions: [] }], unavailable: ["historical_scheduler_decision"] },
  } }));
  await page.goto(fixture.accounts.owner);
  await page.goto(new URL(`/work/i/${fixture.work_item}${hash}`, fixture.url).toString());
}

module.exports = { installDiagnostics };
