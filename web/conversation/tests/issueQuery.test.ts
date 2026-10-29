import { afterEach, describe, expect, it } from "vitest";

import { clearQueryCache } from "../src/app/lib/queryCache.ts";
import { hintIssueProject, readIssue, readOlderIssue, type IssueData } from "../src/app/work/lib/issueQuery.ts";
import type { WorkHttp } from "../src/app/work/lib/workHttp.ts";

afterEach(() => clearQueryCache());

function recordingHttp(projectId: string, archived = false) {
  let settled = 0;
  const calls: { name: string; project: string | null; wave: number }[] = [];
  const call = <T>(name: string, project: string | null, value: T) => {
    calls.push({ name, project, wave: settled });
    return Promise.resolve().then(() => { settled++; return value; });
  };
  const http = {
    getWorkItemById: (id: string) => call("item", null, { work_item_id: id, project_id: projectId, ...(archived ? { archive_summary: { revision: "1", attempts: [] } } : {}) }),
    getProject: (id: string) => call("project", id, { project_id: id, states: [] }),
    listAttempts: (id: string) => call("attempts", id, { items: [{ attempt_id: `attempt_${id}`, fencing_token: "10" }] }),
    listHistory: (input: { projectId: string }) => call("history", input.projectId, { items: [] }),
    listComments: (input: { projectId: string }) => call("comments", input.projectId, { items: [] }),
    listChanges: (id: string) => call("changes", id, [{ change_id: "change_1", title: "Change" }]),
    getChange: (id: string) => call("change", id, { summary: { status: "open" } }),
  } as unknown as WorkHttp;
  return { http, calls };
}

describe("issue first paint", () => {
  it("loads the archived summary and live comments without opening run or Change sections", async () => {
    const { http, calls } = recordingHttp("project_archived", true);
    const data = await readIssue(http, "wi_archived", null)(new AbortController().signal, () => {});
    expect(data?.issue.archive_summary?.revision).toBe("1");
    expect(data?.attempts).toEqual([]);
    expect(data?.changes[0]?.record.change_id).toBe("change_1");
    expect(calls.map((entry) => entry.name)).toContain("comments");
    expect(calls.map((entry) => entry.name)).not.toContain("attempts");
    expect(calls.map((entry) => entry.name)).not.toContain("change");
  });
  it.each([
    { name: "an unknown project", hint: undefined, waves: 2 },
    { name: "a known project", hint: "project_a", waves: 1 },
    { name: "a stale project hint", hint: "project_old", waves: 2 },
    { name: "an inaccessible stale project hint", hint: "project_old", waves: 2, refused: true },
  ])("takes $waves sequential request waves for $name and loads change details after it", async ({ hint, waves, refused }) => {
    const workItemId = `wi_${hint ?? "none"}`;
    if (hint !== undefined) hintIssueProject(workItemId, hint);
    const recording = recordingHttp("project_a");
    const { calls } = recording;
    const http = refused ? { ...recording.http, getProject: (id: string, signal?: AbortSignal) => id === hint ? Promise.reject(new Error("Project not found")) : recording.http.getProject(id, signal) } : recording.http;
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
  it("reads one newest page per list and follows cursors only on demand", async () => {
    const { http } = recordingHttp("project_paged");
    hintIssueProject("wi_paged", "project_paged");
    const cursors: (string | undefined)[] = [];
    const attempts: (string | undefined)[] = [];
    const comments: (string | undefined)[] = [];
    const paged = {
      ...http,
      listHistory: async (input: Parameters<WorkHttp["listHistory"]>[0]) => {
        expect(input.direction).toBe("newest");
        expect(input.view).toBe("timeline");
        cursors.push(input.cursor);
        return { items: [{ event_id: input.cursor ? "old" : "new", aggregate_sequence: input.cursor ? "1" : "101" }], ...(input.cursor ? {} : { next_cursor: "older-history" }) };
      },
      listAttempts: async (...args: Parameters<WorkHttp["listAttempts"]>) => {
        expect(args[5]).toBe("newest");
        attempts.push(args[4]);
        return { items: [{ attempt_id: args[4] ? "old" : "new", fencing_token: args[4] ? "1" : "101" }], ...(args[4] ? {} : { next_cursor: "older-attempts" }) };
      },
      listComments: async (input: Parameters<WorkHttp["listComments"]>[0]) => {
        expect(input.direction).toBe("newest");
        comments.push(input.cursor);
        return { items: [{ comment_id: input.cursor ? "old" : "new" }], ...(input.cursor ? {} : { next_cursor: "older-comments" }) };
      },
    } as unknown as WorkHttp;
    const data = await readIssue(paged, "wi_paged", null)(new AbortController().signal, () => {});
    expect(cursors).toEqual([undefined]);
    expect(attempts).toEqual([undefined]);
    expect(comments).toEqual([undefined]);
    const older = await readOlderIssue(paged, data!);
    expect(cursors).toEqual([undefined, "older-history"]);
    expect(attempts).toEqual([undefined, "older-attempts"]);
    expect(comments).toEqual([undefined, "older-comments"]);
    expect(older.history.map((entry) => entry.event_id)).toEqual(["old", "new"]);
    expect(older.attempts.map((entry) => entry.attempt_id)).toEqual(["old", "new"]);
    expect(older.comments.map((entry) => entry.comment_id)).toEqual(["old", "new"]);
    await readOlderIssue(paged, older);
    expect(cursors).toHaveLength(2);
    expect(attempts).toHaveLength(2);
    expect(comments).toHaveLength(2);
  });

});
