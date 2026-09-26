// The work API client, against the mock hub.
//
// These are the rules that are easy to get wrong and expensive to get wrong
// late: the revision is a string, a filter may not repeat, a cursor is opaque,
// and a refused move is a `409` whose body — in hosted mode — cannot tell the
// client what the current revision is.
import { afterAll, beforeAll, beforeEach, describe, expect, it } from "vitest";

import { startMockHub, type MockHub } from "../dev/mock-hub.ts";
import { loadBootstrap } from "../src/runtime/bootstrap.ts";
import { toWorkItemView, transitionsFrom } from "../src/app/work/lib/fromWire.ts";
import { boardStats, searchItems, sortItems } from "../src/app/work/lib/model.ts";
import { moveItem } from "../src/app/work/lib/useWork.ts";
import {
  makeWorkHttp,
  serverFilter,
  WorkApiError,
  type WorkHttp,
} from "../src/app/work/lib/workHttp.ts";

const PROJECT = "proj_alpha";

let hub: MockHub;
let http: WorkHttp;

beforeAll(async () => {
  hub = await startMockHub({ port: 0 });
  const bootstrap = await loadBootstrap(hub.url);
  http = makeWorkHttp({
    origin: hub.url,
    apiBase: bootstrap.api_base,
    csrfToken: bootstrap.csrf_token,
  });
});

afterAll(async () => {
  await hub?.close();
});

beforeEach(async () => {
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

  it("never sends two values for one filter, because the hub refuses them", async () => {
    expect(serverFilter([])).toBeUndefined();
    expect(serverFilter(["Todo"])).toBe("Todo");
    expect(serverFilter(["Todo", "Blocked"])).toBeUndefined();

    // Proof the refusal is real, and not an assumption this client makes.
    const direct = await fetch(
      `${hub.url}/api/v2/organizations/org_mock/projects/${PROJECT}/work-items?state=Todo&state=Blocked`,
    );
    expect(direct.status).toBe(422);
  });

  it("carries the revision as a string, not a number", async () => {
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 1 });
    expect(typeof page.items[0]!.revision).toBe("string");
  });
});

describe("the derived board", () => {
  it("counts every issue exactly once", async () => {
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
    expect(stats.running + stats.ready + stats.waiting + stats.blocked + stats.completed).toBe(
      items.length,
    );
  });

  it("gives the board and the list the same order", async () => {
    const project = await http.getProject(PROJECT);
    const page = await http.listWorkItems({ projectId: PROJECT, limit: 200 });
    const items = page.items.map((issue) => toWorkItemView(issue, project.name));
    const once = sortItems(items, "priority").map((item) => item.id);
    const twice = sortItems([...items].reverse(), "priority").map((item) => item.id);
    expect(twice).toEqual(once);
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
    ).rejects.toMatchObject({ status: 422 });
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
