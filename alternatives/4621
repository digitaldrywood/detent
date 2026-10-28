import * as Schema from "effect/Schema";
import { describe, expect, it } from "vitest";

import { makeAccountApi } from "../src/app/account/api.ts";
import { ActivityReport } from "../src/contracts/activity.ts";
import { NativeAttempt } from "../src/contracts/work.ts";
import attempts from "../src/contracts/fixtures/work-attempt-list.json";
import fleet from "../src/contracts/fixtures/account-fleet.json";

const report = {
  organization_id: "org_fixture",
  observed_at: "2026-10-01T00:05:00Z",
  window: { from: "2026-09-24T00:05:00Z", to: "2026-10-01T00:05:00Z", bucket_ns: 86400000000000 },
  running: [{
    work_item_id: "wi_fixture", number: 1, title: "Activity", project_id: "prj_fixture", project_name: "Project",
    runner_id: "runner_fixture", attempt_id: "attempt_fixture", stage: "code", phase: "implement",
    stage_started_at: "2026-10-01T00:00:00Z", stage_elapsed_seconds: 300, started_at: "2026-09-30T23:59:00Z",
    session_id: "session_fixture", workspace_ids: ["workspace_fixture"], change_id: "change_fixture",
    pull_request_url: "https://example.test/pull/1", partial: false,
  }],
  finished: [{
    work_item_id: "wi_finished", number: 2, title: "Finished", project_id: "prj_fixture", project_name: "Project",
    runner_id: "runner_fixture", attempt_id: "attempt_finished", stage: "plan", started_at: "2026-10-01T00:00:00Z",
    outcome: "succeeded", finished_at: "2026-10-01T00:01:00Z", stage_duration_seconds: 60, workspace_ids: [], partial: false,
  }],
  typical_durations: [{ project_id: "prj_fixture", stage: "code", count: 3, seconds: 540, p50_seconds: 180, p90_seconds: 276, partial: false }],
  running_issues: [{
    work_item_id: "wi_fixture", number: 1, title: "Activity", project_id: "prj_fixture", project_name: "Project",
    clock_started_at: "2026-10-01T00:00:00Z", clock_lane: "Ready", current_lane: "Build",
    lanes: [{ name: "Ready", role: "active", seconds: 60, entries: 1, current: false }, { name: "Build", role: "active", seconds: 240, entries: 2, current: true }],
    total_seconds: 300, worked_seconds: 200, worked_running: true,
    worked_by_role: [{ role: "security_audit", attempts: 2, seconds: 200, running: true }],
    runner_id: "runner_fixture", runner_ids: ["runner_fixture", "runner_previous"], partial: false,
  }],
  finished_issues: [], issue_typical_durations: [], issues_partial: false,
  population_limit: 1000,
  partial: false,
};

describe("activity read", () => {
  it("decodes stage timing, outcomes and typical durations through the API client", async () => {
    const requests: string[] = [];
    const controller = new AbortController();
    const signals: (AbortSignal | null | undefined)[] = [];
    const api = makeAccountApi({ origin: "", apiBase: "/api/v2/organizations/org_fixture", csrfToken: "", fetch: async (url, init) => {
      requests.push(url);
      signals.push(init?.signal);
      return Response.json(url.endsWith("/fleet") ? fleet : report);
    } });
    const got = await api.activity({ project_id: "prj_fixture", runner_id: "runner/fixture", limit: 2 }, controller.signal);
    expect(got).toEqual(report);
    expect(requests).toEqual(["/api/v2/organizations/org_fixture/activity?project_id=prj_fixture&runner_id=runner%2Ffixture&limit=2"]);
    expect(signals).toEqual([controller.signal]);
    await api.fleet(controller.signal);
    expect(signals).toEqual([controller.signal, controller.signal]);
    expect(Schema.decodeUnknownSync(ActivityReport)({ ...report, typical_durations: [] }).typical_durations).toEqual([]);
  });

  it("preserves phase history and role when decoding the existing per-item attempt", () => {
    const phases = [{ name: "implement", started_at: "2026-10-01T00:00:00Z", finished_at: "2026-10-01T00:01:00Z" }, { name: "validate", started_at: "2026-10-01T00:02:00Z" }];
    const decoded = Schema.decodeUnknownSync(NativeAttempt)({ ...attempts.items[0], runtime: { phase: "validate", phases, phases_dropped: 1, identity: { role: "code" } } });
    expect(decoded.runtime?.phases).toEqual(phases);
    expect(decoded.runtime?.identity?.role).toBe("code");
    expect(decoded.runtime?.phases_dropped).toBe(1);
  });
});
