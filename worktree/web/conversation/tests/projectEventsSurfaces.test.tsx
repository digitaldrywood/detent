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
import { clearBoardCache } from "../src/app/work/lib/boardStore.ts";
import type { Action, ActionRun, NativeIssue, NativeProject, Workspace } from "../src/contracts/work.ts";
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
