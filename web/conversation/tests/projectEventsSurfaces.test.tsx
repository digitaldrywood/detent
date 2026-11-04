// @vitest-environment jsdom
import React from "react";
import { act, cleanup, render, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ClientContext } from "../src/app/client.ts";
import type { ConversationClient } from "../src/runtime/bootstrap.ts";
import { useBoard, workHttpFor } from "../src/app/work/lib/useWork.ts";
import { useIssue } from "../src/app/work/IssuePage.tsx";
import { useWorkspace } from "../src/app/adapters/workspaces.ts";
import { useIssueSurfaces } from "../src/app/adapters/surfaces.ts";
import { useActionRuns } from "../src/app/adapters/actionRuns.ts";
import { useSidebarHumanActions } from "../src/app/adapters/sidebarHumanActions.ts";
import { HealthFindingsProvider, useHealthFindings } from "../src/app/diagnostics/healthFindings.tsx";
import { ActivityRoute } from "../src/app/activity/ActivityPage.tsx";
import { DEFAULT_VIEW_STATE } from "../src/app/work/lib/viewState.ts";
import { WorkApiError } from "../src/app/work/lib/workHttp.ts";
import { hintIssueProject } from "../src/app/work/lib/issueQuery.ts";
import { PROJECT_SNAPSHOT_BUFFER } from "../src/app/work/lib/projectEvents.ts";
import { clearBoardCache } from "../src/app/work/lib/boardStore.ts";
import type { Action, ActionRun, ChangeDetail, NativeIssue, NativeProject, Workspace } from "../src/contracts/work.ts";
import issueFixture from "../src/contracts/fixtures/work-item.json";
import projectFixture from "../src/contracts/fixtures/work-project.json";

const mocks = vi.hoisted(() => ({ api: { activity: vi.fn(), fleet: vi.fn() },
  projects: [{ id: "p", name: "Example project", profile: "native", can_write: true }], names: new Map() }));
vi.mock("../src/app/account/context.ts", () => ({ useAccountApi: () => mocks.api,
  useAccountBootstrap: () => ({ projects: mocks.projects, organization: { name: "Example organization" } }) }));
vi.mock("../src/app/work/lib/runnerNames.ts", () => ({ useRunnerNames: () => mocks.names,
  shortRunnerId: (id: string) => id, runnerDisplay: (_names: unknown, id: string) => id }));
vi.mock("../src/app/pageTitle.ts", () => ({ usePageTitle: () => {} }));
vi.mock("@tanstack/react-router", async (original) => ({ ...await original<object>(),
  Link: ({ children }: { children: React.ReactNode }) => <span>{children}</span> }));
class Source extends EventTarget {
  static instances: Source[] = [];
  readyState = 1;
  close = vi.fn();
  constructor(readonly url: string) { super(); Source.instances.push(this); }
  emit(type: string, data: unknown, id: number) {
    this.dispatchEvent(new MessageEvent(type, { data: JSON.stringify(data), lastEventId: String(id) }));
  }
}
afterEach(() => { cleanup(); clearBoardCache(); vi.restoreAllMocks(); vi.unstubAllGlobals(); vi.useRealTimers(); });

it("mounts board, IssuePage, workspaces, ActivityPage, sidebarHumanActions, healthFindings, surfaces and actionRuns on one resumable stream", async () => {
  vi.stubGlobal("EventSource", Source);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  Source.instances = [];
  const client = { http: { origin: "", apiBase: "/api/v2/organizations/example", csrfToken: "" },
    bootstrap: { organization: { id: "example" }, projects: mocks.projects, actor: { principal_id: "person" } } } as unknown as ConversationClient;
  const http = workHttpFor(client);
  const issue = { ...issueFixture, work_item_id: "wi_example", project_id: "p", change_sequence: 10 } as NativeIssue;
  const project = { ...projectFixture, project_id: "p" } as NativeProject;
  const workspace = { id: "ws", project_id: "p", organization_id: "example", work_item_id: "wi_example", state: "starting",
    ref: "develop", requires: ["files"], capabilities: { files: true, terminal: false, diff: false, preview: false, exec: false, git: false }, read_only: true, idle_timeout_seconds: 1800,
    expires_at: "2026-10-27T12:00:00Z", created_by: "person", revision: 1, created_at: "2026-10-27T10:00:00Z",
    updated_at: "2026-10-27T10:00:00Z" } as Workspace;
  const run = { id: "run", action_id: "action", workspace_id: "ws", command: "true", status: "queued", exit_code: null,
    started_at: null, finished_at: null, output_bytes: 0, truncated: false, revision: 1,
    created_at: "2026-10-27T10:00:00Z", updated_at: "2026-10-27T10:00:00Z" } as ActionRun;
  const reads = {
    project: vi.spyOn(http, "getProject").mockResolvedValue(project),
    board: vi.spyOn(http, "listWorkItems").mockResolvedValue({ items: [issue], sequence: 10, change_sequence: 10 }),
    issue: vi.spyOn(http, "getWorkItemById").mockResolvedValue(issue),
    head: vi.spyOn(http, "getWorkItem").mockResolvedValue(issue),
    workspace: vi.spyOn(http, "listWorkspaces").mockResolvedValue({ workspaces: [workspace], change_sequence: 10 }),
    health: vi.spyOn(http, "listHealthFindings").mockResolvedValue({ items: [], last_tick_at: null, change_sequence: 10 }),
    runs: vi.spyOn(http, "listActionRuns").mockResolvedValue({ items: [run], change_sequence: 10 }),
  };
  vi.spyOn(http, "listAttempts").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listHistory").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listComments").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listChanges").mockResolvedValue([]);
  vi.spyOn(http, "getWorkItemDiff").mockResolvedValue({ diff: null });
  vi.spyOn(http, "getExplanation").mockResolvedValue({ current_lane: { name: issue.state }, required_gate: {} } as never);
  vi.spyOn(http, "listActions").mockResolvedValue({ items: [{ id: "action", name: "Example action" } as Action] });
  mocks.api.activity.mockResolvedValue({ organization_id: "example", observed_at: "2026-10-27T10:00:00Z",
    window: { from: "2026-10-26T10:00:00Z", to: "2026-10-27T10:00:00Z", bucket_ns: 1 },
    running: [], finished: [], running_issues: [], finished_issues: [], typical_durations: [], population_limit: 100,
    partial: false, change_sequences: { p: 10 } });
  mocks.api.fleet.mockResolvedValue({ runners: [] });
  let board!: ReturnType<typeof useBoard>;
  let detail!: ReturnType<typeof useIssue>;
  let workspaceState!: ReturnType<typeof useWorkspace>;
  let runs!: ReturnType<typeof useActionRuns>;
  let surfaces!: ReturnType<typeof useIssueSurfaces>;
  function Probe() {
    board = useBoard("p", DEFAULT_VIEW_STATE);
    detail = useIssue(http, "wi_example");
    workspaceState = useWorkspace({ projectId: "p", workItemId: "wi_example", requires: ["files"] });
    runs = useActionRuns({ projectId: "p", workspaceId: "ws", workspaceLive: false, relayUrl: null, mintTicket: null, showRecorded: true });
    surfaces = useIssueSurfaces("p", "wi_example");
    useSidebarHumanActions(mocks.projects);
    useHealthFindings();
    return <ActivityRoute />;
  }
  const view = render(<ClientContext.Provider value={client}><HealthFindingsProvider projects={mocks.projects}><Probe /></HealthFindingsProvider></ClientContext.Provider>);
  await waitFor(() => {
    expect(board.resolved).toBe(true);
    expect(detail.data?.issue.work_item_id).toBe("wi_example");
    expect(workspaceState.state).toBe("starting");
    expect(runs.runs).toHaveLength(1);
    expect(surfaces.loading).toBe(false);
    expect(mocks.api.activity).toHaveBeenCalledOnce();
  });
  expect(Source.instances).toHaveLength(1);
  const counts = () => Object.values(reads).map((read) => read.mock.calls.length);
  const before = counts();
  vi.useFakeTimers();
  await act(async () => {
    Source.instances[0]!.emit("checkpoint", {}, 50);
    Source.instances[0]!.dispatchEvent(new Event("error"));
    await vi.advanceTimersByTimeAsync(1000);
  });
  expect(Source.instances).toHaveLength(2);
  expect(Source.instances[1]!.url).toContain("since=50");
  expect(counts()).toEqual(before);
  await act(async () => {
    Source.instances[1]!.emit("workspace.ready", { ...workspace, state: "ready", revision: 2 }, 70);
    Source.instances[1]!.emit("action_run.running", { ...run, status: "running", revision: 2 }, 90);
    Source.instances[1]!.emit("action_run.running", { ...run, status: "running", revision: 2 }, 91);
  });
  expect(workspaceState.state).toBe("ready");
  expect(runs.runs[0]!.status).toBe("running");
  expect(counts()).toEqual(before);
  await act(async () => Source.instances[1]!.emit("health.findings", { entity_id: "f", entity_revision: 2 }, 110));
  expect(reads.health).toHaveBeenCalledTimes(before[5]! + 1);
  expect(reads.issue).toHaveBeenCalledTimes(before[2]!);
  expect(reads.board).toHaveBeenCalledTimes(before[1]!);
  await act(async () => Source.instances[1]!.emit("board", { entity_id: "wi_other", entity_revision: 112 }, 112));
  expect(reads.issue).toHaveBeenCalledTimes(before[2]!);
  expect(reads.head).toHaveBeenCalledTimes(before[3]!);
  const projectsBeforeLane = reads.project.mock.calls.length;
  await act(async () => Source.instances[1]!.emit("lane", { entity_id: "Todo", entity_revision: 113 }, 113));
  expect(reads.project.mock.calls.length).toBeGreaterThan(projectsBeforeLane);
  const beforeReset = counts();
  const activityReads = mocks.api.activity.mock.calls.length;
  await act(async () => {
    Source.instances[1]!.emit("reset", {}, 120);
    Source.instances[1]!.emit("reset", {}, 120);
  });
  expect(reads.issue).toHaveBeenCalledTimes(beforeReset[2]! + 1);
  expect(reads.head).toHaveBeenCalledTimes(beforeReset[3]! + 1);
  expect(reads.workspace).toHaveBeenCalledTimes(beforeReset[4]! + 1);
  expect(reads.health).toHaveBeenCalledTimes(beforeReset[5]! + 1);
  expect(reads.runs).toHaveBeenCalledTimes(beforeReset[6]! + 1);
  expect(mocks.api.activity).toHaveBeenCalledTimes(activityReads + 1);
  view.unmount();
  expect(Source.instances[1]!.close).toHaveBeenCalledOnce();
  expect(vi.getTimerCount()).toBe(0);
});

it.each(["success", "failure", "unmount"])("replaces a reset snapshot without hiding a current failure (%s)", async (outcome) => {
  vi.stubGlobal("EventSource", Source);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  Source.instances = [];
  const http = workHttpFor({ http: { origin: "", apiBase: "/api/v2/organizations/reset-example", csrfToken: "" }, bootstrap: { organization: { id: "reset-example" }, projects: [], actor: { principal_id: "person" } } } as unknown as ConversationClient);
  const issue = { ...issueFixture, work_item_id: `wi_reset_${outcome}`, project_id: "reset-project", change_sequence: 20 } as NativeIssue;
  let releaseFirst!: (value: { items: [] }) => void;
  let releaseSecond!: (value: NativeIssue) => void;
  let rejectSecond!: (error: Error) => void;
  const first = new Promise<{ items: [] }>((resolve) => { releaseFirst = resolve; });
  const second = new Promise<NativeIssue>((resolve, reject) => { releaseSecond = resolve; rejectSecond = reject; });
  const head = vi.spyOn(http, "getWorkItemById").mockResolvedValueOnce(issue).mockResolvedValueOnce(issue).mockImplementation(() => second);
  const attempts = vi.spyOn(http, "listAttempts").mockImplementationOnce(() => first).mockResolvedValue({ items: [] });
  vi.spyOn(http, "getProject").mockResolvedValue({ ...projectFixture, project_id: issue.project_id } as NativeProject);
  vi.spyOn(http, "listHistory").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listComments").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listChanges").mockResolvedValue([]);
  let detail!: ReturnType<typeof useIssue>;
  const errors: string[] = [];
  function Probe() {
    detail = useIssue(http, issue.work_item_id);
    if (detail.error !== null) errors.push(detail.error);
    return null;
  }
  const view = render(<Probe />);
  try {
    await waitFor(() => expect(attempts).toHaveBeenCalledOnce());
    await act(async () => { Source.instances[0]!.emit("reset", {}, 21); });
    await waitFor(() => expect(head).toHaveBeenCalledTimes(3));
    expect(detail.error).toBeNull();
    expect(detail.loading).toBe(false);
    expect(detail.data?.issue).toBe(issue);
    if (outcome === "unmount") {
      view.unmount();
      expect(head.mock.calls[2]?.[1]?.aborted).toBe(true);
      return;
    }
    await act(async () => {
      releaseFirst({ items: [] });
      if (outcome === "failure") rejectSecond(new Error("Backend unavailable"));
      else releaseSecond({ ...issue, title: "Current issue", change_sequence: 22 });
    });
    await waitFor(() => {
      if (outcome === "failure") expect(detail.error).toBe("Backend unavailable");
      else expect(detail.data?.issue.title).toBe("Current issue");
    });
    expect(errors).not.toContain("Issue snapshot superseded");
    expect(head).toHaveBeenCalledTimes(3);
  } finally {
    view.unmount();
    await act(async () => { releaseFirst({ items: [] }); releaseSecond(issue); });
  }
});

it.each([false, true])("opens the current issue before related reads settle (lane traffic: %s)", async (laneTraffic) => {
  vi.stubGlobal("EventSource", Source);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  Source.instances = [];
  const http = workHttpFor({ http: { origin: "", apiBase: "/api/v2/organizations/first-paint", csrfToken: "" }, bootstrap: { organization: { id: "first-paint" }, projects: [] } } as unknown as ConversationClient);
  const issue = { ...issueFixture, work_item_id: "wi_first_paint", project_id: "first-paint", change_sequence: 10 } as NativeIssue;
  hintIssueProject(issue.work_item_id, issue.project_id);
  let releaseProject!: (value: NativeProject) => void;
  let releaseRelated!: (value: { items: [] }) => void;
  const project = { ...projectFixture, project_id: issue.project_id } as NativeProject;
  const projectBarrier = new Promise<NativeProject>((resolve) => { releaseProject = resolve; });
  const relatedBarrier = new Promise<{ items: [] }>((resolve) => { releaseRelated = resolve; });
  const head = vi.spyOn(http, "getWorkItemById").mockResolvedValue(issue);
  const projectRead = vi.spyOn(http, "getProject").mockReturnValue(projectBarrier);
  const attempts = vi.spyOn(http, "listAttempts").mockReturnValue(relatedBarrier);
  vi.spyOn(http, "listHistory").mockReturnValue(relatedBarrier);
  vi.spyOn(http, "listComments").mockReturnValue(relatedBarrier);
  vi.spyOn(http, "listChanges").mockResolvedValue([]);
  let detail!: ReturnType<typeof useIssue>;
  function Probe() { detail = useIssue(http, issue.work_item_id); return null; }
  const view = render(<Probe />);
  try {
    await act(async () => {});
    expect(attempts).toHaveBeenCalledOnce();
    await act(async () => {
      for (let i = 0; laneTraffic && i <= PROJECT_SNAPSHOT_BUFFER; i++) Source.instances[0]!.emit("lane", { entity_id: "Other lane", entity_revision: 20 + i }, 20 + i);
    });
    expect(head).toHaveBeenCalledOnce();
    await act(async () => releaseProject(project));
    expect(detail.data?.issue.title).toBe(issue.title);
    expect(detail.loading).toBe(false);
    expect(detail.error).toBeNull();
    const updatedProject = { ...project, require_dependencies: !project.require_dependencies };
    projectRead.mockResolvedValue(updatedProject);
    await act(async () => Source.instances[0]!.emit("lane", { entity_id: "Todo", entity_revision: 500 }, 500));
    expect(detail.data?.project.require_dependencies).toBe(updatedProject.require_dependencies);
    expect(detail.data?.issue.blockers).toEqual(issue.blockers);
    await act(async () => releaseRelated({ items: [] }));
    expect(detail.data?.project).toBe(updatedProject);
    const settledHeads = head.mock.calls.length;
    await act(async () => Source.instances[0]!.emit("work_item", { entity_id: issue.work_item_id, entity_revision: 501 }, 501));
    expect(head).toHaveBeenCalledTimes(settledHeads + 1);
    expect(detail.error).toBeNull();
    expect(detail.data?.issue).toBe(issue);
  } finally {
    view.unmount();
    await act(async () => { releaseProject(project); releaseRelated({ items: [] }); });
  }
});

it.each(["source", "route"])("rejects late related results after a %s change while publishing current dependencies", async (transition) => {
  vi.stubGlobal("EventSource", Source);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  Source.instances = [];
  const http = workHttpFor({ http: { origin: "", apiBase: "/api/v2/organizations/coherence", csrfToken: "" }, bootstrap: { organization: { id: "coherence" }, projects: [] } } as unknown as ConversationClient);
  const original = { ...issueFixture, work_item_id: "wi_original", project_id: "coherence", change_sequence: 10 } as NativeIssue;
  const next = { ...original, work_item_id: transition === "route" ? "wi_next" : original.work_item_id, title: "Current source", body: "Updated body", revision: "5", change_sequence: 30, dependencies: [], blockers: [], ignore_dependencies: true } as NativeIssue;
  hintIssueProject(original.work_item_id, original.project_id);
  hintIssueProject(next.work_item_id, next.project_id);
  let releaseOld!: (value: { items: [] }) => void;
  let releaseCurrent!: (value: { items: [] }) => void;
  const oldRelated = new Promise<{ items: [] }>((resolve) => { releaseOld = resolve; });
  const currentRelated = new Promise<{ items: [] }>((resolve) => { releaseCurrent = resolve; });
  const head = vi.spyOn(http, "getWorkItemById").mockResolvedValueOnce(original).mockResolvedValue(next);
  vi.spyOn(http, "getProject").mockResolvedValue({ ...projectFixture, project_id: original.project_id } as NativeProject);
  vi.spyOn(http, "listAttempts").mockReturnValueOnce(oldRelated).mockReturnValue(currentRelated);
  vi.spyOn(http, "listHistory").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listComments").mockResolvedValue({ items: [] });
  const oldChange = { change_id: "change_old", title: "Obsolete change" };
  vi.spyOn(http, "listChanges").mockResolvedValueOnce([oldChange] as never).mockResolvedValue([]);
  const change = vi.spyOn(http, "getChange").mockResolvedValue(null as never);
  let detail!: ReturnType<typeof useIssue>;
  function Probe({ id }: { id: string }) { detail = useIssue(http, id); return null; }
  const view = render(<Probe id={original.work_item_id} />);
  try {
    await act(async () => {});
    expect(detail.data?.issue).toBe(original);
    if (transition === "route") view.rerender(<Probe id={next.work_item_id} />);
    else await act(async () => Source.instances[0]!.emit("work_item", { entity_id: original.work_item_id, entity_revision: 5 }, 30));
    await act(async () => {});
    expect(head).toHaveBeenCalledTimes(2);
    expect(detail.data?.issue.body).toBe(next.body);
    expect(detail.data?.issue.blockers).toEqual([]);
    expect(detail.data?.issue.ignore_dependencies).toBe(true);
    expect(detail.loading).toBe(false);
    await act(async () => releaseOld({ items: [] }));
    expect(detail.data?.issue).toBe(next);
    expect(detail.data?.changes).toEqual([]);
    expect(change).not.toHaveBeenCalled();
    await act(async () => releaseCurrent({ items: [] }));
    expect(detail.error).toBeNull();
    expect(detail.data?.issue).toBe(next);
  } finally {
    view.unmount();
    await act(async () => { releaseOld({ items: [] }); releaseCurrent({ items: [] }); });
  }
});


it.each([401, 403, 404])("removes a painted core on a current %s and never restores late related data", async (status) => {
  vi.stubGlobal("EventSource", Source);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  Source.instances = [];
  const http = workHttpFor({ http: { origin: "", apiBase: "/api/v2/organizations/refused-core", csrfToken: "" }, bootstrap: { organization: { id: "refused-core" }, projects: [] } } as unknown as ConversationClient);
  const issue = { ...issueFixture, work_item_id: `wi_refused_${status}`, project_id: "refused-core", change_sequence: 10 } as NativeIssue;
  hintIssueProject(issue.work_item_id, issue.project_id);
  let release!: (value: { items: [] }) => void;
  const related = new Promise<{ items: [] }>((resolve) => { release = resolve; });
  const refused = new WorkApiError({ status, code: status === 404 ? "not_found" : "forbidden", message: `Refused ${status}` });
  const head = vi.spyOn(http, "getWorkItemById").mockResolvedValueOnce(issue).mockRejectedValue(refused);
  vi.spyOn(http, "getProject").mockResolvedValue({ ...projectFixture, project_id: issue.project_id } as NativeProject);
  vi.spyOn(http, "listAttempts").mockReturnValue(related);
  vi.spyOn(http, "listHistory").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listComments").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listChanges").mockResolvedValue([]);
  let detail!: ReturnType<typeof useIssue>;
  function Probe() { detail = useIssue(http, issue.work_item_id); return null; }
  const view = render(<Probe />);
  try {
    await act(async () => {});
    expect(detail.data?.issue).toBe(issue);
    await act(async () => Source.instances[0]!.emit("work_item", { entity_id: issue.work_item_id, entity_revision: 5 }, 20));
    expect(detail.data).toBeNull();
    expect(detail.error).toBe(refused.message);
    expect(detail.loading).toBe(false);
    await act(async () => release({ items: [] }));
    expect(detail.data).toBeNull();
    expect(detail.error).toBe(refused.message);
    head.mockResolvedValue(issue);
    await act(async () => detail.reload());
    expect(detail.data?.issue).toBe(issue);
    expect(detail.error).toBeNull();
  } finally {
    view.unmount();
    await act(async () => release({ items: [] }));
  }
});


it("fences a buffered own-source event and an already-running old change detail", async () => {
  vi.stubGlobal("EventSource", Source);
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  Source.instances = [];
  const http = workHttpFor({ http: { origin: "", apiBase: "/api/v2/organizations/buffered-source", csrfToken: "" }, bootstrap: { organization: { id: "buffered-source" }, projects: [] } } as unknown as ConversationClient);
  const issue = { ...issueFixture, work_item_id: "wi_buffered", project_id: "buffered-source", change_sequence: 10 } as NativeIssue;
  const next = { ...issue, title: "Current source", revision: "5", change_sequence: 20, dependencies: [], blockers: [] } as NativeIssue;
  hintIssueProject(issue.work_item_id, issue.project_id);
  const project = { ...projectFixture, project_id: issue.project_id } as NativeProject;
  let releaseProject!: (value: NativeProject) => void;
  let releaseDetail!: (value: ChangeDetail) => void;
  const projectBarrier = new Promise<NativeProject>((resolve) => { releaseProject = resolve; });
  const detailBarrier = new Promise<ChangeDetail>((resolve) => { releaseDetail = resolve; });
  const head = vi.spyOn(http, "getWorkItemById").mockResolvedValueOnce(issue).mockResolvedValue(next);
  vi.spyOn(http, "getProject").mockReturnValue(projectBarrier);
  vi.spyOn(http, "listAttempts").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listHistory").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listComments").mockResolvedValue({ items: [] });
  vi.spyOn(http, "listChanges").mockResolvedValue([{ change_id: "old_change", title: "Old change" }] as never);
  const change = vi.spyOn(http, "getChange").mockReturnValue(detailBarrier);
  let detail!: ReturnType<typeof useIssue>;
  function Probe() { detail = useIssue(http, issue.work_item_id); return null; }
  const view = render(<Probe />);
  const oldDetail = { summary: { status: "open" } } as ChangeDetail;
  try {
    await act(async () => {});
    expect(detail.data).toBeNull();
    await act(async () => Source.instances[0]!.emit("work_item", { entity_id: issue.work_item_id, entity_revision: 5 }, 20));
    expect(head).toHaveBeenCalledOnce();
    await act(async () => releaseProject(project));
    expect(head).toHaveBeenCalledTimes(2);
    expect(detail.data?.issue).toBe(next);
    expect(detail.data?.issue.blockers).toEqual([]);
    expect(change).toHaveBeenCalledOnce();
    head.mockResolvedValue({ ...next, title: "Newest source", revision: "6", change_sequence: 30 } as NativeIssue);
    vi.mocked(http.listChanges).mockResolvedValue([]);
    await act(async () => Source.instances[0]!.emit("work_item", { entity_id: issue.work_item_id, entity_revision: 6 }, 30));
    expect(detail.data?.issue.title).toBe("Newest source");
    await act(async () => releaseDetail(oldDetail));
    expect(detail.data?.issue.title).toBe("Newest source");
    expect(detail.data?.change).toBeNull();
    expect(detail.data?.changes).toEqual([]);
    expect(detail.error).toBeNull();
  } finally {
    view.unmount();
    await act(async () => { releaseProject(project); releaseDetail(oldDetail); });
  }
});
