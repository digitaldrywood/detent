// The work API client, against the mock hub.
//
// These are the rules that are easy to get wrong and expensive to get wrong
// late: the revision is a string, filters preserve every selected value, a cursor is opaque,
// and a refused move is a `409` whose body — in hosted mode — cannot tell the
// client what the current revision is.
import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";
import * as Schema from "effect/Schema";

import { startMockHub, type MockHub } from "../dev/mock-hub.ts";
import { loadBootstrap } from "../src/runtime/bootstrap.ts";
import { NativeIssue } from "../src/contracts/work.ts";
import { toAttemptView, toWorkItemView, transitionsFrom } from "../src/app/work/lib/fromWire.ts";
import { boardStats, searchItems, sortItems, type WorkItemView } from "../src/app/work/lib/model.ts";
import type { WorkSort } from "../src/app/work/lib/viewState.ts";
import { moveItem } from "../src/app/work/lib/useWork.ts";
import {
  makeWorkHttp,
  WorkApiError,
  type WorkHttp,
} from "../src/app/work/lib/workHttp.ts";

const PROJECT = "proj_alpha";

let hub: MockHub;
let http: WorkHttp;
const confirmed: NativeIssue[] = [];

beforeAll(async () => {
  hub = await startMockHub({ port: 0 });
  const bootstrap = await loadBootstrap(hub.url);
  http = makeWorkHttp({
    origin: hub.url,
    apiBase: bootstrap.api_base,
    csrfToken: bootstrap.csrf_token,
    onConfirmed: (issue) => { confirmed.push(issue); },
  });
});

afterAll(async () => {
  await hub?.close();
});

beforeEach(async () => {
  confirmed.length = 0;
  await fetch(`${hub.url}/__mock/work/reset`, { method: "POST" });
});

describe("reading a project", () => {
  it("decodes the workflow, in the workflow's own order", async () => {
    const project = await http.getProject(PROJECT);
    expect(project.name).toBe("alpha");
    expect(project.states.map((state) => state.name)).toEqual([
      "Backlog",
      "Todo",
      "In Progress",
      "In Review",
      "Blocked",
      "Merging",
      "Done",
    ]);
    expect(project.states.at(-1)?.terminal).toBe(true);
  });

  it("offers only the transitions the current state allows", async () => {
    const project = await http.getProject(PROJECT);
    expect(transitionsFrom(project, "Todo")).toEqual(["Backlog", "In Progress", "Blocked"]);
    // The order is the workflow's, not the transitions array's.
    expect(transitionsFrom(project, "Done")).toEqual([]);
  });
});

describe("listing work items", () => {
  it("pages by cursor and stops when the cursor runs out", async () => {
    const first = await http.listWorkItems({ projectId: PROJECT, limit: 10 });
    expect(first.items).toHaveLength(10);
    expect(first.next_cursor).toBeDefined();

    const second = await http.listWorkItems({
      projectId: PROJECT,
      limit: 10,
      cursor: first.next_cursor!,
    });
    expect(second.items).toHaveLength(10);
    expect(second.items[0]!.number).toBeGreaterThan(first.items.at(-1)!.number);

    const all = await http.listWorkItems({ projectId: PROJECT, limit: 200 });
    expect(all.next_cursor).toBeUndefined();
    expect(all.items.length).toBe(32);
  });

  it("filters by one state on the server", async () => {
    const page = await http.listWorkItems({ projectId: PROJECT, state: "In Review" });
    expect(page.items.length).toBeGreaterThan(0);
    expect(page.items.every((issue) => issue.state === "In Review")).toBe(true);
  });

  it("sends multiple state selections to the scoped API", async () => {
    const page = await http.listWorkItems({ projectId: PROJECT, state: ["Todo", "In Review"], includeWork: true });
    expect(page.work).toBeDefined();
    expect(page.items.length).toBeGreaterThan(0);
    expect(new Set(page.items.map((issue) => issue.state))).toEqual(new Set(["Todo", "In Review"]));
  });

  it("carries the revision as a string, not a number", async () => {
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 1 });
    expect(typeof page.items[0]!.revision).toBe("string");
  });
});

describe("the derived board", () => {
  it("counts every open and terminal issue without claiming queue eligibility", async () => {
    const project = await http.getProject(PROJECT);
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 200 });
    const items = page.items.map((issue) => toWorkItemView(issue, project.name));
    const lanes = project.states.map((state) => ({
      id: state.name,
      name: state.name,
      terminal: state.terminal,
      category: state.terminal ? "completed" : state.dispatchable ? "unstarted" : "started",
    }));
    const stats = boardStats(items, lanes);
    expect(stats.total).toBe(items.length);
    expect(stats.open + stats.completed).toBe(items.length);
    expect(stats.open).toBe(28);
    expect(stats.queued).toBe(10);
  });

  it.each(["default", "priority", "updated", "created", "title"] as const)("gives the board and the list the same %s order", async (sort) => {
    const project = await http.getProject(PROJECT);
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 200 });
    const items = page.items.map((issue) => toWorkItemView(issue, project.name));
    const once = sortItems(items, sort).map((item) => item.id);
    const twice = sortItems([...items].reverse(), sort).map((item) => item.id);
    expect(twice).toEqual(once);
  });

  it.each([
    { name: "non-terminal priority before activity", terminal: false, priorities: ["Low", "High", "Urgent", "Normal", null], activity: ["2026-10-05", "2026-10-04", "2026-10-03", "2026-10-02", "2026-10-06"], expected: [3, 2, 4, 1, 5] },
    { name: "non-terminal activity ties", terminal: false, priorities: ["High", "High", "High"], activity: ["2026-10-03", "2026-10-05", "2026-10-04"], expected: [2, 3, 1] },
    { name: "terminal activity before priority", terminal: true, priorities: ["Urgent", "Low", null], activity: ["2026-10-03", "2026-10-04", "2026-10-05"], expected: [3, 2, 1] },
    { name: "terminal identifier ties", terminal: true, priorities: ["Low", "Urgent", "High"], activity: ["2026-10-05", "2026-10-05", "2026-10-05"], expected: [1, 2, 3] },
    { name: "non-terminal identifier ties", terminal: false, priorities: ["High", "High", "High"], activity: ["2026-10-05", "2026-10-05", "2026-10-05"], expected: [1, 2, 3] },
    { name: "terminal missing and invalid activity", terminal: true, priorities: ["Urgent", "High", "Low", null], activity: [null, "invalid", "2026-10-05", undefined], expected: [3, 1, 2, 4] },
    { name: "non-terminal missing and invalid activity", terminal: false, priorities: [null, null, null, null], activity: [null, "invalid", "2026-10-05", undefined], expected: [3, 1, 2, 4] },
    { name: "offset timestamps compared as instants", terminal: true, priorities: ["High", "High", "High"], activity: ["2026-10-05T12:00:00+02:00", "2026-10-05T11:00:00Z", "2026-10-05T10:00:00Z"], expected: [2, 1, 3] },
  ])("defaults to $name", async ({ terminal, priorities, activity, expected }) => {
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 1 });
    const items = priorities.map((priority, index) => {
      const issue = Schema.decodeUnknownSync(NativeIssue)({ ...page.items[0]!, last_activity_at: activity[index] });
      const item = toWorkItemView(issue, "alpha");
      expect(item.lastActivityAt).toBe(activity[index] ?? null);
      return { ...item, identifier: `alpha#${index + 1}`, priority, terminal };
    });
    for (const input of [items, [...items].reverse()]) {
      expect(sortItems(input, "default").map((item) => item.identifier)).toEqual(expected.map((number) => `alpha#${number}`));
    }
  });

  it.each([
    { sort: "default", expected: [2, 1, 3] },
    { sort: "priority", expected: [3, 1, 2] },
    { sort: "updated", expected: [1, 2, 3] },
    { sort: "created", expected: [3, 2, 1] },
    { sort: "title", expected: [2, 1, 3] },
  ] satisfies { sort: WorkSort; expected: number[] }[])("keeps $sort ordering independent of other timestamps", async ({ sort, expected }) => {
    const page = await http.listWorkItems({ projectId: PROJECT, state: "In Progress", limit: 1 });
    const attempts = await http.listAttempts(PROJECT, page.items[0]!.work_item_id);
    const base = toWorkItemView(page.items[0]!, "alpha");
    const items: WorkItemView[] = [
      { ...base, identifier: "alpha#1", priority: "High", title: "B", terminal: false, lastActivityAt: "2026-10-03", updatedAt: "2026-10-05", createdAt: "2026-10-03", attempt: toAttemptView(attempts.items) },
      { ...base, identifier: "alpha#2", priority: "High", title: "A", terminal: false, lastActivityAt: "2026-10-04", updatedAt: "2026-10-04", createdAt: "2026-10-04", attempt: null },
      { ...base, identifier: "alpha#3", priority: "Urgent", title: "C", terminal: true, lastActivityAt: "2026-10-05", updatedAt: "2026-10-03", createdAt: "2026-10-05", attempt: null },
    ];
    expect(sortItems(items, sort).map((item) => item.identifier)).toEqual(expected.map((number) => `alpha#${number}`));
  });

  it("searches title, identifier and label over the loaded board", async () => {
    const project = await http.getProject(PROJECT);
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 200 });
    const items = page.items.map((issue) => toWorkItemView(issue, project.name));
    expect(searchItems(items, "renewal").length).toBeGreaterThan(0);
    expect(searchItems(items, "renewal").every((item) => item.title.includes("renewal"))).toBe(
      true,
    );
    expect(searchItems(items, "").length).toBe(items.length);
    expect(searchItems(items, "no such issue anywhere")).toHaveLength(0);
  });
});

describe("attempts and changes", () => {
  it("reports a live attempt as running, and only for an issue that has one", async () => {
    const inProgress = await http.listWorkItems({ projectId: PROJECT, state: "In Progress" });
    const attempts = await http.listAttempts(PROJECT, inProgress.items[0]!.work_item_id);
    expect(attempts.items.at(-1)?.status).toBe("running");
    expect(attempts.items.at(-1)?.identity?.model).toBe("gpt-6-astra");

    const backlog = await http.listWorkItems({ projectId: PROJECT, state: "Backlog" });
    const none = await http.listAttempts(PROJECT, backlog.items[0]!.work_item_id);
    expect(none.items).toHaveLength(0);
  });

  it("decodes the changes list, which is a bare array with no envelope", async () => {
    const inReview = await http.listWorkItems({ projectId: PROJECT, state: "In Review" });
    const item = inReview.items[0]!.work_item_id;
    const changes = await http.listChanges(PROJECT, item);
    expect(Array.isArray(changes)).toBe(true);
    expect(changes).toHaveLength(1);

    const detail = await http.getChange(PROJECT, item, changes[0]!.change_id);
    expect(detail.summary.status).toBe("blocked");
    expect(detail.versions[0]!.external?.url).toContain("github.com");
    // A version number is a `,string` integer.
    expect(typeof detail.versions[0]!.number).toBe("string");
  });

  it("reads history with the fields the activity list renders", async () => {
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 1 });
    const history = await http.listHistory({
      projectId: PROJECT,
      itemId: page.items[0]!.work_item_id,
    });
    expect(history.items.map((event) => event.type)).toContain("workflow.transitioned");
    expect(history.items.at(-1)?.data.to_state).toBeDefined();
  });
});

describe("the property pickers' reads and writes", () => {
  async function firstIssue() {
    const page = await http.listWorkItems({ projectId: PROJECT });
    return page.items[0]!;
  }

  it("reads a label catalogue with a colour and a count per label", async () => {
    const catalogue = await http.listLabels(PROJECT);
    expect(catalogue.items.length).toBeGreaterThan(0);
    for (const label of catalogue.items) {
      expect(label.color).toMatch(/^#[0-9a-f]{6}$/);
      expect(label.count).toBeGreaterThan(0);
    }
    // Busiest first, so the picker's suggestions are the project's own.
    const counts = catalogue.items.map((label) => label.count);
    expect(counts).toEqual([...counts].toSorted((a, b) => b - a));
    // A prefix the hub owns has a row of its own and is never a user label.
    expect(catalogue.items.map((label) => label.name)).not.toContain("effort:medium");
  });

  it("attaches a label the catalogue has never seen, which is how one is made", async () => {
    const issue = await firstIssue();
    const labelled = await http.patchWorkItem({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "label-1",
      expectedRevision: issue.revision,
      labels: [...issue.labels, "flaky"],
    });
    expect(labelled.labels).toContain("flaky");
    const catalogue = await http.listLabels(PROJECT);
    expect(catalogue.items.map((label) => label.name)).toContain("flaky");
  });

  it("assigns a member and unassigns again", async () => {
    const issue = await firstIssue();
    const assigned = await http.patchWorkItem({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "assign-1",
      expectedRevision: issue.revision,
      assignees: ["owner@example.test"],
    });
    expect(assigned.assignees).toEqual(["owner@example.test"]);
    const cleared = await http.patchWorkItem({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "assign-2",
      expectedRevision: assigned.revision,
      assignees: [],
    });
    expect(cleared.assignees).toEqual([]);
  });

  // The one write the endpoint could not take before: an omitted member means
  // "leave alone", so removing a priority needs a word of its own.
  it("removes a priority with \"none\" and leaves it alone when it is omitted", async () => {
    const issue = await firstIssue();
    const urgent = await http.patchWorkItem({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "priority-1",
      expectedRevision: issue.revision,
      priority: 0,
    });
    expect(urgent.priority).toBe(0);

    const retitled = await http.patchWorkItem({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "priority-2",
      expectedRevision: urgent.revision,
      title: `${urgent.title} (edited)`,
    });
    expect(retitled.priority).toBe(0);

    const none = await http.patchWorkItem({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "priority-3",
      expectedRevision: retitled.revision,
      priority: "none",
    });
    expect(none.priority).toBeUndefined();
  });

  it("adds and removes a relation through the dependency endpoint", async () => {
    const page = await http.listWorkItems({ projectId: PROJECT });
    const [issue, other] = [page.items[0]!, page.items[1]!];
    const linked = await http.setDependency({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "relation-1",
      expectedRevision: issue.revision,
      relatedWorkItemId: other.work_item_id,
      operation: "add",
    });
    expect(linked.dependencies).toContain(other.work_item_id);

    const unlinked = await http.setDependency({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "relation-2",
      expectedRevision: linked.revision,
      relatedWorkItemId: other.work_item_id,
      operation: "remove",
    });
    expect(unlinked.dependencies).not.toContain(other.work_item_id);
  });
});

describe("moving an issue", () => {
  async function firstTodo() {
    const page = await http.listWorkItems({ projectId: PROJECT, state: "Todo" });
    return page.items[0]!;
  }

  it("moves with the revision the card was rendered from", async () => {
    const issue = await firstTodo();
    const moved = await http.transition({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "move-1",
      expectedRevision: issue.revision,
      state: "In Progress",
    });
    expect(moved.state).toBe("In Progress");
    expect(Number(moved.revision)).toBe(Number(issue.revision) + 1);
    expect(confirmed).toEqual([moved]);
  });

  it("refuses a move the workflow does not allow", async () => {
    const issue = await firstTodo();
    await expect(
      http.transition({
        projectId: PROJECT,
        itemId: issue.work_item_id,
        key: "move-2",
        expectedRevision: issue.revision,
        state: "Done",
      }),
    ).rejects.toMatchObject({
      status: 422,
      code: "transition_not_allowed",
      message: "This status change is not allowed by the project's workflow.",
    });
  });

  it("refuses a stale revision with a conflict that names no revision", async () => {
    const issue = await firstTodo();
    await http.transition({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "move-3",
      expectedRevision: issue.revision,
      state: "In Progress",
    });
    // The second move still believes the first revision, as a stale card does.
    const failure = await http
      .transition({
        projectId: PROJECT,
        itemId: issue.work_item_id,
        key: "move-4",
        expectedRevision: issue.revision,
        state: "In Review",
      })
      .catch((cause: unknown) => cause);
    expect(failure).toBeInstanceOf(WorkApiError);
    const error = failure as WorkApiError;
    expect(error.conflict).toBe(true);
    // Hosted mode strips it, so a client that relied on it would break there.
    expect(error.currentRevision).toBeNull();
  });

  it("turns a scripted conflict into a recoverable outcome, not a throw", async () => {
    const issue = await firstTodo();
    await fetch(`${hub.url}/__mock/work/conflict`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ item: issue.work_item_id }),
    });
    const item = toWorkItemView(issue, "alpha");
    const outcome = await moveItem(http, item, "In Progress", "alpha");
    expect(outcome.ok).toBe(false);
    if (outcome.ok) return;
    expect(outcome.conflict).toBe(true);

    // Re-reading is the only recovery, and it works: the hub advanced the
    // revision, and the fresh card moves.
    const fresh = await http.getWorkItem(PROJECT, issue.work_item_id);
    expect(fresh.revision).not.toBe(issue.revision);
    const retry = await moveItem(http, toWorkItemView(fresh, "alpha"), "In Progress", "alpha");
    expect(retry.ok).toBe(true);
  });

  it("reports an unknown issue as not found rather than as a conflict", async () => {
    const outcome = await moveItem(
      http,
      { ...toWorkItemView((await firstTodo()), "alpha"), id: "wi_missing" },
      "In Progress",
      "alpha",
    );
    expect(outcome.ok).toBe(false);
    if (outcome.ok) return;
    expect(outcome.conflict).toBe(false);
  });
});

describe("the activity stream", () => {
  it("emits a bare integer that increases when the board changes", async () => {
    const response = await fetch(`${hub.url}/projects/${PROJECT}/events`);
    expect(response.headers.get("content-type")).toContain("text/event-stream");
    const reader = response.body!.getReader();
    const decoder = new TextDecoder();

    const readFrame = async (): Promise<string> => {
      const { value } = await reader.read();
      return decoder.decode(value);
    };

    const first = await readFrame();
    expect(first).toMatch(/^event: activity\ndata: \d+\n\n$/);
    const before = Number(first.split("data: ")[1]);

    const page = await http.listWorkItems({ projectId: PROJECT, state: "Todo" });
    const issue = page.items[0]!;
    await http.transition({
      projectId: PROJECT,
      itemId: issue.work_item_id,
      key: "move-stream",
      expectedRevision: issue.revision,
      state: "In Progress",
    });

    const second = await readFrame();
    expect(Number(second.split("data: ")[1])).toBeGreaterThan(before);
    await reader.cancel();
  });
});

describe("reviewing a change", () => {
  async function reviewable() {
    const board = await http.listWorkItems({ projectId: PROJECT, limit: 200 });
    for (const item of board.items) {
      const changes = await http.listChanges(PROJECT, item.work_item_id);
      const change = changes.at(-1);
      if (change === undefined) continue;
      const detail = await http.getChange(PROJECT, item.work_item_id, change.change_id);
      return { itemId: item.work_item_id, changeId: change.change_id, detail };
    }
    throw new Error("the mock serves no issue with a change");
  }

  it("reads the diff behind the round from the attempt that published it", async () => {
    const { detail } = await reviewable();
    const version = detail.versions.find(
      (candidate) => candidate.version_id === detail.change.current_version_id,
    )!;
    const diff = await http.getAttemptDiff(PROJECT, version.attempt_id!);
    expect(diff.attempt_id).toBe(version.attempt_id);
    expect(diff.head_sha).toBe(version.head_sha);
    expect(diff.files.map((file) => file.path)).toEqual(["README.md"]);
    await expect(http.getAttemptDiff(PROJECT, "att_nobody")).rejects.toMatchObject({ status: 404 });
  });

  it("records an approval on the current version and the summary follows", async () => {
    const { itemId, changeId, detail } = await reviewable();
    const review = await http.reviewChange({
      projectId: PROJECT,
      itemId,
      changeId,
      versionId: detail.change.current_version_id,
      key: "review_1",
      decision: "approved",
      body: "Looks right.",
      expectedVersionId: detail.change.current_version_id,
    });
    expect(review.decision).toBe("approved");
    expect(review.version_id).toBe(detail.change.current_version_id);
    const after = await http.getChange(PROJECT, itemId, changeId);
    expect(after.reviews.map((entry) => entry.review_id)).toContain(review.review_id);
    expect(after.summary.native_review).toBe("approved");
  });

  it("refuses an approval pinned to a version that is not current", async () => {
    const { itemId, changeId, detail } = await reviewable();
    await expect(
      http.reviewChange({
        projectId: PROJECT,
        itemId,
        changeId,
        versionId: detail.change.current_version_id,
        key: "review_stale",
        decision: "approved",
        body: "",
        expectedVersionId: "version_gone",
      }),
    ).rejects.toSatisfy((error: unknown) => error instanceof WorkApiError && error.conflict);
  });

  it("carries a request for changes with its text", async () => {
    const { itemId, changeId, detail } = await reviewable();
    const review = await http.reviewChange({
      projectId: PROJECT,
      itemId,
      changeId,
      versionId: detail.change.current_version_id,
      key: "review_2",
      decision: "changes_requested",
      body: "Move the link to the end of the row.",
    });
    expect(review.body).toBe("Move the link to the end of the row.");
    const after = await http.getChange(PROJECT, itemId, changeId);
    expect(after.summary.native_review).toBe("changes_requested");
  });

  it("appends discussion to the change", async () => {
    const { itemId, changeId, detail } = await reviewable();
    const comment = await http.discussChange({
      projectId: PROJECT,
      itemId,
      changeId,
      key: "discuss_1",
      body: "Do we want this on mobile too?",
      versionId: detail.change.current_version_id,
    });
    expect(comment.version_id).toBe(detail.change.current_version_id);
    const after = await http.getChange(PROJECT, itemId, changeId);
    expect(after.discussion.map((entry) => entry.comment_id)).toContain(comment.comment_id);
  });
});
