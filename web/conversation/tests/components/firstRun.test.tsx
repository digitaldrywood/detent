// @vitest-environment jsdom
import { RegistryProvider } from "@effect/atom-react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { RouterProvider, createMemoryHistory } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  startMockHub,
  type AccountMode,
  type MockHub,
  type OrganizationMode,
} from "../../dev/mock-hub.ts";
import { ClientContext } from "../../src/app/client.ts";
import {
  NewProjectProvider,
  PROJECT_CREATION_UNAVAILABLE,
  createdProjectId,
  projectBoardPath,
  useNewProject,
} from "../../src/app/projects/NewProject.tsx";
import { makeRouter } from "../../src/app/router.tsx";
import {
  FirstRunChecklist,
  ISSUE_CREATION_UNAVAILABLE,
  NEEDS_PROJECT,
  RUNNER_ACCESS_NEEDED,
  RUNNER_ENROLLMENT_UNAVAILABLE,
  RUNNERS_LOADING,
  RUNNERS_UNAVAILABLE,
  SETUP_LOADING,
  SETUP_NEEDS_ADMIN,
  SETUP_UNAVAILABLE,
  setupStepsLeftLabel,
  firstIssueState,
  firstRunSteps,
  type FirstRunFacts,
} from "../../src/app/work/components/FirstRun.tsx";
import { newIssueProject, newIssueProjects, newIssueState } from "../../src/app/work/NewIssue.tsx";
import type { AccountProject } from "../../src/contracts/account.ts";
import { loadBootstrap, makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { fetchEventStreamTransport } from "../../src/runtime/rpc/sse.ts";

Object.defineProperty(globalThis, "scrollTo", { value: () => {}, writable: true });

let hub: MockHub | undefined;
let client: ConversationClient | undefined;
const nativeFetch = globalThis.fetch;

beforeEach(() => vi.stubGlobal("fetch", sameRealmFetch));

afterEach(async () => {
  cleanup();
  client?.handles.clear();
  client = undefined;
  await hub?.close();
  hub = undefined;
  vi.unstubAllGlobals();
});

const sameRealmFetch: typeof globalThis.fetch = (input, init) => {
  const { signal: _abort, ...rest } = (init ?? {}) as RequestInit;
  return nativeFetch(input as string, rest);
};

async function startHub(options: {
  readonly organization?: OrganizationMode;
  readonly account?: AccountMode;
}): Promise<ConversationClient> {
  hub = await startMockHub({
    deltaDelayMs: 0,
    heartbeatMs: 5_000,
    coordinator: "hub",
    organization: options.organization ?? "empty",
    account: options.account ?? "write",
  });
  const bootstrap = await loadBootstrap(hub.url);
  client = makeClient({
    origin: hub.url,
    bootstrap,
    transport: fetchEventStreamTransport(sameRealmFetch),
    heartbeatTimeoutMs: 20_000,
  });
  return client;
}

async function mountShell(
  path = "/work",
  options: { readonly organization?: OrganizationMode; readonly account?: AccountMode } = {},
) {
  const mounted = await startHub(options);
  const router = makeRouter(createMemoryHistory({ initialEntries: [path] }));
  render(
    <RegistryProvider>
      <ClientContext.Provider value={mounted}>
        <RouterProvider router={router} />
      </ClientContext.Provider>
    </RegistryProvider>,
  );
  if (!path.startsWith("/settings")) {
    await screen.findByLabelText(path.startsWith("/work") ? "Search issues" : "Search threads", undefined, { timeout: 10_000 });
  }
  return { router };
}

function step(id: "project" | "runner" | "issue"): HTMLElement {
  return screen.getByTestId(`first-run-step-${id}`);
}

const FACTS: FirstRunFacts = {
  projects: 0,
  runners: 0,
  runnerState: "action_required",
  issues: 0,
  setupStepsLeft: 4,
  canManageProjects: true,
  canEnrollRunners: true,
  canWriteIssues: false,
};

const LEFT = setupStepsLeftLabel(4);

describe("firstRunSteps", () => {
  it.each([
    {
      name: "a new organization",
      facts: FACTS,
      done: [false, false, false, false],
      blocked: [null, NEEDS_PROJECT, NEEDS_PROJECT, NEEDS_PROJECT],
      notes: [null, null, null, null],
    },
    {
      name: "a project and nothing else",
      facts: { ...FACTS, projects: 1, canWriteIssues: true },
      done: [true, false, false, false],
      blocked: [null, null, null, null],
      notes: [null, null, LEFT, null],
    },
    {
      name: "a project with a ready execution runner",
      facts: { ...FACTS, projects: 1, runners: 1, runnerState: "ready" as const, canWriteIssues: true },
      done: [true, true, false, false],
      blocked: [null, null, null, null],
      notes: [null, null, LEFT, null],
    },
    {
      name: "an organization runner that does not serve this project",
      facts: { ...FACTS, projects: 1, runners: 1, canWriteIssues: true },
      done: [true, false, false, false],
      blocked: [null, null, null, null],
      notes: [null, null, LEFT, null],
    },
    {
      name: "everything done",
      facts: { ...FACTS, projects: 2, runners: 1, runnerState: "ready" as const, issues: 3, setupStepsLeft: 0, canWriteIssues: true },
      done: [true, true, true, true],
      blocked: [null, null, null, null],
      notes: [null, null, null, null],
    },
    {
      name: "a reader who manages nothing, before any project",
      facts: { ...FACTS, canManageProjects: false, canEnrollRunners: false },
      done: [false, false, false, false],
      blocked: [PROJECT_CREATION_UNAVAILABLE, NEEDS_PROJECT, NEEDS_PROJECT, NEEDS_PROJECT],
      notes: [null, null, null, null],
    },
    {
      name: "a reader who manages nothing, on a read-only project",
      facts: { ...FACTS, projects: 1, canManageProjects: false, canEnrollRunners: false },
      done: [true, false, false, false],
      blocked: [PROJECT_CREATION_UNAVAILABLE, RUNNER_ENROLLMENT_UNAVAILABLE, SETUP_NEEDS_ADMIN, ISSUE_CREATION_UNAVAILABLE],
      notes: [null, null, LEFT, null],
    },
  ])("reports $name", ({ facts, done, blocked, notes }) => {
    const steps = firstRunSteps(facts);
    expect(steps.map((entry) => entry.id)).toEqual(["project", "runner", "setup", "issue"]);
    expect(steps.map((entry) => entry.done)).toEqual(done);
    expect(steps.map((entry) => entry.blockedReason)).toEqual(blocked);
    expect(steps.map((entry) => entry.note)).toEqual(notes);
  });

  it.each([
    { setupStepsLeft: "loading" as const, reason: SETUP_LOADING },
    { setupStepsLeft: "unavailable" as const, reason: SETUP_UNAVAILABLE },
  ])("holds the setup step while onboarding is $setupStepsLeft", ({ setupStepsLeft, reason }) => {
    const steps = firstRunSteps({ ...FACTS, projects: 1, runners: 1, runnerState: setupStepsLeft, setupStepsLeft });
    for (const index of [1, 2]) {
      expect(steps[index]?.done).toBe(false);
      expect(steps[index]?.blockedReason).toBe(reason);
      expect(steps[index]?.note).toBeNull();
    }
  });

  it("uses the wording Settings → Projects uses for the count", () => {
    expect(setupStepsLeftLabel(1)).toBe("1 setup step left");
    expect(setupStepsLeftLabel(3)).toBe("3 setup steps left");
  });

  it.each([
    { runners: "loading" as const, reason: RUNNERS_LOADING },
    { runners: "unavailable" as const, reason: RUNNERS_UNAVAILABLE },
  ])("holds the runner step while the fleet is $runners", ({ runners, reason }) => {
    const runner = firstRunSteps({ ...FACTS, projects: 1, runners, canWriteIssues: true })[1];
    expect(runner?.done).toBe(false);
    expect(runner?.blockedReason).toBe(reason);
    expect(runner?.note).toBeNull();
  });

  it("sends an owner without runner access to grant it", () => {
    const runner = firstRunSteps({
      ...FACTS,
      projects: 1,
      canEnrollRunners: false,
      canWriteIssues: true,
    })[1];
    expect(runner?.blockedReason).toBeNull();
    expect(runner?.actionLabel).toBe("Grant runner access");
    expect(runner?.note).toBe(RUNNER_ACCESS_NEEDED);
  });
});

describe("FirstRunChecklist", () => {
  it.each([
    { facts: FACTS, progress: "0 of 4 done", doneCount: 0 },
    { facts: { ...FACTS, projects: 1, runners: 1, canWriteIssues: true }, progress: "1 of 4 done", doneCount: 1 },
    {
      facts: { ...FACTS, projects: 1, runners: 1, runnerState: "ready" as const, issues: 1, setupStepsLeft: 0, canWriteIssues: true },
      progress: "4 of 4 done",
      doneCount: 4,
    },
  ])("shows $progress and checks off the finished steps", ({ facts, progress, doneCount }) => {
    const onAction = vi.fn();
    render(<FirstRunChecklist steps={firstRunSteps(facts)} onAction={onAction} />);
    expect(screen.getByTestId("first-run-progress").textContent).toBe(progress);
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe(String(doneCount));
    const finished = screen
      .getAllByRole("listitem")
      .filter((item) => item.getAttribute("data-done") === "true");
    expect(finished).toHaveLength(doneCount);
    expect(screen.queryAllByText("Done")).toHaveLength(doneCount);
  });

  it("runs a step's action and refuses a blocked one", () => {
    const onAction = vi.fn();
    render(<FirstRunChecklist steps={firstRunSteps({ ...FACTS, runners: 1 })} onAction={onAction} />);
    fireEvent.click(within(step("project")).getByRole("button", { name: "New project" }));
    expect(onAction).toHaveBeenCalledWith("project");

    const enroll = within(step("runner")).getByRole("button", { name: "Enroll a runner" });
    expect((enroll as HTMLButtonElement).disabled).toBe(true);
    expect(within(step("runner")).getByText(`${NEEDS_PROJECT}.`)).toBeTruthy();
    fireEvent.click(enroll);
    expect(onAction).toHaveBeenCalledTimes(1);
  });
});

describe("firstIssueState", () => {
  it("stages native work in the first lane and preserves compatibility defaults", () => {
    const project = {
      id: "proj_example",
      name: "Example Studio",
      profile: "native",
      can_write: true,
      can_manage_runners: true,
      states: [
        { name: "Backlog", terminal: false, dispatchable: false },
        { name: "Todo", terminal: false, dispatchable: true },
        { name: "Done", terminal: true, dispatchable: false },
      ],
    } satisfies AccountProject;
    expect(firstIssueState(project)).toBe("Backlog");
    expect(firstIssueState({ ...project, profile: "github_compatible" })).toBe("Todo");
    expect(
      firstIssueState({ ...project, states: project.states.filter((s) => !s.dispatchable) }),
    ).toBe("Backlog");
    expect(firstIssueState(undefined)).toBe("");
  });
});

describe("the new project entry points", () => {
  it("replaces the empty board with the first-run checklist", async () => {
    await mountShell("/work");
    const panel = await screen.findByTestId("first-run", undefined, { timeout: 5_000 });
    expect(within(panel).getByTestId("first-run-progress").textContent).toBe("0 of 4 done");
    expect(step("project").getAttribute("data-done")).toBe("false");
    expect(screen.queryByTestId("work-board")).toBeNull();
    expect(screen.queryByText(/Every lane is hidden/)).toBeNull();
  }, 30_000);

  it("opens the New project dialog from the empty state", async () => {
    const { router } = await mountShell("/work");
    await screen.findByTestId("first-run", undefined, { timeout: 5_000 });
    fireEvent.click(within(step("project")).getByRole("button", { name: "New project" }));
    const dialog = await screen.findByRole("dialog", { name: "New project" }, { timeout: 5_000 });
    expect(within(dialog).getByLabelText("Name")).toBeTruthy();
    expect(router.state.location.pathname).toBe("/work");
  }, 30_000);

  it("opens the New project dialog from the sidebar's empty Projects section", async () => {
    const { router } = await mountShell("/work");
    const sidebar = document.querySelector("aside.dc-side") as HTMLElement;
    await within(sidebar).findByText("No projects yet", undefined, { timeout: 5_000 });
    fireEvent.click(within(sidebar).getByRole("button", { name: "New project" }));
    await screen.findByRole("dialog", { name: "New project" }, { timeout: 5_000 });
    expect(router.state.location.pathname).toBe("/work");
  }, 30_000);

  it("opens the same dialog from Settings → Projects", async () => {
    await mountShell("/settings/projects");
    const button = await screen.findByRole("button", { name: "New project" }, { timeout: 5_000 });
    fireEvent.click(button);
    await screen.findByRole("dialog", { name: "New project" }, { timeout: 5_000 });
  }, 30_000);

  it("tells a reader who cannot manage projects why, everywhere", async () => {
    await mountShell("/work", { account: "read_only" });
    await screen.findByTestId("first-run", undefined, { timeout: 5_000 });
    const create = within(step("project")).getByRole("button", { name: "New project" });
    expect((create as HTMLButtonElement).disabled).toBe(true);
    expect(within(step("project")).getByText(`${PROJECT_CREATION_UNAVAILABLE}.`)).toBeTruthy();

    const sidebar = document.querySelector("aside.dc-side") as HTMLElement;
    expect(within(sidebar).getByTestId("sidebar-new-project-unavailable").textContent).toBe(
      PROJECT_CREATION_UNAVAILABLE,
    );
    expect(within(sidebar).queryByRole("button", { name: "New project" })).toBeNull();
    expect(screen.queryByRole("dialog", { name: "New project" })).toBeNull();
  }, 30_000);

  it("keeps the board and drops the checklist once there are issues", async () => {
    await mountShell("/work", { organization: "seeded" });
    await screen.findByTestId("work-board", undefined, { timeout: 10_000 });
    await screen.findAllByText("checkout: renewal waits on a healthy handoff", undefined, {
      timeout: 10_000,
    });
    expect(screen.queryByTestId("first-run")).toBeNull();
  }, 30_000);
});

describe("NewProjectProvider", () => {
  function Opener(): React.ReactElement {
    const { openNewProject } = useNewProject();
    return (
      <button type="button" onClick={openNewProject}>
        Open
      </button>
    );
  }

  it("creates the project and hands its id on", async () => {
    const mounted = await startHub({});
    const onCreated = vi.fn();
    render(
      <ClientContext.Provider value={mounted}>
        <NewProjectProvider onCreated={onCreated}>
          <Opener />
        </NewProjectProvider>
      </ClientContext.Provider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    const dialog = await screen.findByRole("dialog", { name: "New project" }, { timeout: 5_000 });
    fireEvent.change(within(dialog).getByLabelText("Name"), {
      target: { value: "Example Studio" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create project" }));

    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1), { timeout: 5_000 });
    const id = onCreated.mock.calls[0]?.[0] as string;
    expect(id).toMatch(/^proj_/);
    const bootstrap = await loadBootstrap(hub!.url);
    expect(bootstrap.projects.map((project) => project.name)).toContain("Example Studio");
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "New project" })).toBeNull());
  }, 30_000);

  it("reads the created id and builds the board path", () => {
    expect(createdProjectId({ id: "prj_1" })).toBe("prj_1");
    expect(createdProjectId({ project_id: "prj_2", name: "Example Studio" })).toBe("prj_2");
    expect(createdProjectId({ id: "" })).toBeNull();
    expect(createdProjectId(null)).toBeNull();
    expect(createdProjectId("prj_1")).toBeNull();
    expect(projectBoardPath("prj 1")).toBe("/work/p/prj%201");
  });
});

describe("where a new issue goes", () => {
  const project = (id: string, can_write: boolean): AccountProject => ({
    id,
    name: id,
    profile: "native",
    can_write,
    can_manage_runners: false,
    states: [
      { name: "Backlog", terminal: false, dispatchable: false },
      { name: "Todo", terminal: false, dispatchable: true },
      { name: "Done", terminal: true, dispatchable: false },
    ],
  });

  it.each([
    { name: "the requested writable project", requested: "b", want: "b" },
    { name: "the first writable project when none was requested", requested: null, want: "b" },
    { name: "the first writable project instead of a read-only one", requested: "a", want: "b" },
    { name: "the first writable project for an unknown id", requested: "missing", want: "b" },
  ])("picks $name", ({ requested, want }) => {
    const projects = [project("a", false), project("b", true), project("c", true)];
    expect(newIssueProject(projects, requested)?.id).toBe(want);
  });

  it.each([
    { name: "every writable project without a lane", state: null, want: ["b", "c"] },
    { name: "only writable projects with the lane", state: "Review", want: ["c"] },
    { name: "none when no writable project has the lane", state: "Missing", want: [] },
  ])("offers $name", ({ state, want }) => {
    const review = project("c", true);
    const projects = [
      project("a", false),
      project("b", true),
      { ...review, states: [...review.states, { name: "Review", terminal: false, dispatchable: true }] },
    ];
    expect(newIssueProjects(projects, state).map((entry) => entry.id)).toEqual(want);
  });

  it("picks no project when none is writable", () => {
    expect(newIssueProject([project("a", false)], "a")).toBeUndefined();
  });

  it.each([
    { name: "the lane it was started from", requested: "Backlog", want: "Backlog" },
    { name: "the native backlog without a request", requested: null, want: "Backlog" },
    { name: "the native backlog for a lane the project lacks", requested: "Review", want: "Backlog" },
  ])("starts in $name", ({ requested, want }) => {
    expect(newIssueState(project("a", true), requested)).toBe(want);
  });
});
