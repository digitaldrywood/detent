import { afterEach, describe, expect, it } from "vitest";

import { clearQueryCache } from "../src/app/lib/queryCache.ts";
import { hintIssueProject, readIssue, type IssueData } from "../src/app/work/lib/issueQuery.ts";
import type { WorkHttp } from "../src/app/work/lib/workHttp.ts";

afterEach(() => clearQueryCache());

function recordingHttp(projectId: string) {
  let settled = 0;
  const calls: { name: string; project: string | null; wave: number }[] = [];
  const call = <T>(name: string, project: string | null, value: T) => {
    calls.push({ name, project, wave: settled });
    return new Promise<T>((resolve) => setTimeout(() => { settled++; resolve(value); }, 0));
  };
  const http = {
    getWorkItemById: (id: string) => call("item", null, { work_item_id: id, project_id: projectId }),
    getProject: (id: string) => call("project", id, { project_id: id, states: [] }),
    listAttempts: (id: string) => call("attempts", id, { items: [{ attempt_id: `attempt_${id}` }] }),
    listHistory: (input: { projectId: string }) => call("history", input.projectId, { items: [] }),
    listComments: (input: { projectId: string }) => call("comments", input.projectId, { items: [] }),
    listChanges: (id: string) => call("changes", id, [{ change_id: "change_1", title: "Change" }]),
    getChange: (id: string) => call("change", id, { summary: { status: "open" } }),
  } as unknown as WorkHttp;
  return { http, calls };
}

describe("issue first paint", () => {
  it.each([
    { name: "an unknown project", hint: undefined, waves: 2 },
    { name: "a known project", hint: "project_a", waves: 1 },
    { name: "a stale project hint", hint: "project_old", waves: 2 },
  ])("takes $waves sequential request waves for $name and loads change details after it", async ({ hint, waves }) => {
    const workItemId = `wi_${hint ?? "none"}`;
    if (hint !== undefined) hintIssueProject(workItemId, hint);
    const { http, calls } = recordingHttp("project_a");
    const painted: (IssueData | null)[] = [];
    const data = await readIssue(http, workItemId, null)(new AbortController().signal, (value) => painted.push(value));
    const firstPaint = calls.filter((entry) => entry.name !== "change" && entry.project !== "project_old");
    expect(new Set(firstPaint.map((entry) => entry.wave)).size).toBe(waves);
    expect(painted).toHaveLength(1);
    expect(painted[0]?.changes[0]?.pending).toBe(true);
    const detail = calls.find((entry) => entry.name === "change")!;
    expect(detail.wave).toBeGreaterThan(Math.max(...firstPaint.map((entry) => entry.wave)));
    expect(data?.change).not.toBeNull();
    expect(data?.project.project_id).toBe("project_a");
    expect(data?.attempts.map((attempt) => attempt.attempt_id)).toEqual(["attempt_project_a"]);
  });
});
