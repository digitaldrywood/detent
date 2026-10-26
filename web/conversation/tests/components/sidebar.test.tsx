// @vitest-environment jsdom
import {
  render,
  act,
  cleanup,
  fireEvent,
  screen,
  within,
  renderHook,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SidebarDestinations } from "../../src/app/adapters/sidebarDestinations.tsx";
import { SidebarDataProvider, type SidebarShellData } from "../../src/app/adapters/sidebarData.tsx";
import { SidebarProvider } from "../../src/components/ui/sidebar.tsx";
import { ProjectSetupContext } from "../../src/app/account/projectSetup.tsx";
import type { Onboarding, ProjectIntegration } from "../../src/contracts/account.ts";
import type { Resource } from "../../src/app/account/useResource.ts";
import { subscribeProjectEvents } from "../../src/app/work/lib/projectEvents.ts";
import { HealthFindingsProvider, useHealthFindings } from "../../src/app/diagnostics/healthFindings.tsx";
import { useSidebarFindings } from "../../src/app/adapters/sidebarFindings.ts";
import { resetUpdateCheckState } from "../../src/app/adapters/detentUpdates.ts";
import * as settings from "../../src/hooks/useSettings.ts";
import { setReleaseNotesUnread } from "../../src/app/account/releaseNotes.ts";
import { AccountBootstrap } from "../../src/contracts/account.ts";
import * as Schema from "effect/Schema";
import accountFixture from "../../src/contracts/fixtures/account-bootstrap.json";
import { makeClient } from "../../src/runtime/bootstrap.ts";
import { ClientContext } from "../../src/app/client.ts";
import type { Bootstrap } from "../../src/contracts/index.ts";
import bootstrapFixture from "../../src/contracts/fixtures/bootstrap.json";
import { conversation } from "./builders.ts";
import { renderSidebar, resetSidebarState, rowFor } from "./sidebarHarness.tsx";
import { useSidebarHumanActions } from "../../src/app/adapters/sidebarHumanActions.ts";
import itemFixture from "../../src/contracts/fixtures/work-item.json";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  resetUpdateCheckState();
});
beforeEach(resetSidebarState);

const running = conversation({
  id: "conv_running",
  title: "Lock renewal",
  work_item_id: "wi_1",
  last_message_at: "2026-09-09T11:59:00Z",
});
const needsYou = conversation({
  id: "conv_needs",
  title: "Migration question",
  work_item_id: "wi_2",
  last_message_at: "2026-09-09T11:00:00Z",
  execution: { ...running.execution, status: "waiting_input" },
});
const recentChat = conversation({
  id: "conv_chat",
  title: "Scratch ideas",
  work_item_id: null,
  work_item: null,
  last_message_at: "2026-09-09T11:30:00Z",
  execution: { ...running.execution, status: "idle" },
});

function sidebar(): HTMLElement {
  return screen.getByRole("complementary", { name: "Conversations" });
}

/** The router navigates on a promise; this lets it land. */
async function settleNavigation(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

function expandSettled(): void {
  fireEvent.click(screen.getByTestId("sidebar-settled-shelf-toggle"));
}

describe("the thread sidebar", () => {
  it("renders one shared UI row per conversation", async () => {
    await renderSidebar({ conversations: [running, needsYou, recentChat] });
    for (const title of [
      "Lock renewal",
      "Migration question",
      "Scratch ideas",
    ]) {
      expect(rowFor(sidebar(), title), title).not.toBeNull();
    }
  });

  it("carries `#number · lane` in the branch slot", async () => {
    await renderSidebar({ conversations: [running] });
    expect(rowFor(sidebar(), "Lock renewal")?.textContent).toContain(
      "#3363 · Running",
    );
  });

  it("leaves the branch slot empty for a chat nobody is waiting on", async () => {
    await renderSidebar({ conversations: [recentChat] });
    const row = rowFor(sidebar(), "Scratch ideas");
    expect(row?.textContent).toContain("Scratch ideas");
    expect(row?.textContent).not.toContain("·");
  });

  it("shelves a settled conversation under the Settled shelf", async () => {
    const settled = conversation({
      id: "conv_settled",
      title: "Put away",
      work_item_id: null,
      work_item: null,
      status: "settled",
      last_message_at: "2026-09-09T11:55:00Z",
      execution: { ...running.execution, status: "completed" },
    });
    await renderSidebar({ conversations: [recentChat, settled] });
    // Collapsed, the shelf still counts what is on it.
    expect(
      screen.getByTestId("sidebar-settled-shelf-toggle").textContent,
    ).toContain("Settled (1)");
    expect(rowFor(sidebar(), "Put away")).toBeNull();
    expandSettled();
    expect(rowFor(sidebar(), "Put away")).not.toBeNull();
    // An active chat stays in the card block above it.
    expect(rowFor(sidebar(), "Scratch ideas")).not.toBeNull();
  });

  it("moves a row to the Settled shelf when the reader settles it", async () => {
    await renderSidebar({ conversations: [recentChat] });
    expect(screen.getByTestId("sidebar-settled-shelf-toggle").textContent).toContain("Settled (0)");

    fireEvent.click(screen.getByRole("button", { name: "Settle thread" }));
    await settleNavigation();

    expect(screen.getByTestId("sidebar-settled-shelf-toggle").textContent).toContain("Settled (1)");
    expandSettled();
    const row = rowFor(sidebar(), "Scratch ideas");
    expect(row?.getAttribute("data-testid")).toBe("sidebar-row-slim");

    fireEvent.click(screen.getByRole("button", { name: "Un-settle thread" }));
    await settleNavigation();
    expect(rowFor(sidebar(), "Scratch ideas")?.getAttribute("data-testid")).toBe(
      "sidebar-row-card",
    );
  });

  it("offers no archive affordance at all", async () => {
    await renderSidebar({ conversations: [recentChat] });
    expect(screen.queryByTestId("show-archived")).toBeNull();
    expect(screen.queryByText(/archive/i)).toBeNull();
  });

  it("hides pinning and snoozing, which the hub has nothing behind", async () => {
    await renderSidebar({ conversations: [running, recentChat] });
    expect(screen.queryByLabelText("Snooze thread")).toBeNull();
    expect(screen.queryByLabelText("Pin thread")).toBeNull();
    expect(screen.queryByTestId("sidebar-snoozed-header")).toBeNull();
  });

  it("marks the open conversation with the active row surface", async () => {
    await renderSidebar(
      { conversations: [recentChat], activeConversationId: "conv_chat" },
      { path: "/chat/c/conv_chat" },
    );
    expect(rowFor(sidebar(), "Scratch ideas")?.className).toContain(
      "bg-sidebar-row-active",
    );
  });

  it("opens a conversation on Detent's route", async () => {
    const { navigated } = await renderSidebar({ conversations: [running] });
    fireEvent.click(screen.getByText("Lock renewal"));
    await settleNavigation();
    expect(navigated).toContain("/chat/c/conv_running");
  });

  // `Sidebar.logic.ts`'s rule: title-only, order preserved. The shell folds
  // the hub's own hits into the list first (`state/entities.ts`), so a
  // conversation past the first page is found by the same rule.
  it("filters by title while searching and folds in server results", async () => {
    await renderSidebar({
      conversations: [
        recentChat,
        conversation({ id: "conv_old", title: "Ancient thread" }),
      ],
      serverResults: [
        conversation({ id: "conv_server", title: "Ancient server hit" }),
      ],
    });
    fireEvent.change(screen.getByRole("combobox", { name: "Search threads" }), {
      target: { value: "ancient" },
    });
    const results = screen.getByRole("listbox", {
      name: "Thread search results",
    });
    expect(within(results).getByText("Ancient thread")).toBeTruthy();
    expect(within(results).getByText("Ancient server hit")).toBeTruthy();
    expect(within(results).queryByText("Scratch ideas")).toBeNull();
  });

  it("says nothing matched rather than showing an empty list", async () => {
    await renderSidebar({ conversations: [recentChat] });
    fireEvent.change(screen.getByRole("combobox", { name: "Search threads" }), {
      target: { value: "zzzz" },
    });
    expect(screen.getByText("No threads found")).toBeTruthy();
  });

  it("offers exactly one new-chat action", async () => {
    const { onNewChat } = await renderSidebar({
      conversations: [recentChat],
      projects: [{ id: "proj_alpha", name: "alpha", can_write: true }],
    });
    const actions = screen.getAllByLabelText("New thread");
    expect(actions).toHaveLength(1);
    fireEvent.click(actions[0]!);
    await settleNavigation();
    expect(onNewChat).toHaveBeenCalled();
  });

  // The brand row is the product, not the tenant.
  it("reads Detent Cloud in the brand row", async () => {
    await renderSidebar();
    const brand = screen.getByLabelText("Go to Detent Cloud");
    expect(brand.textContent).toContain("Detent");
    expect(brand.textContent).toContain("Cloud");
    expect(brand.getAttribute("href")).toBe("/work");
  });

  it.each([
    ["Secondary Example Organization", "All Secondary Example Organization projects"],
    ["Tertiary Example Organization", "All Tertiary Example Organization projects"],
    ["", "All projects"],
  ])("names the %s project scope and reports project and all-project selections", async (organizationName, label) => {
    const onProjectChange = vi.fn();
    await renderSidebar({ onProjectChange, organizationName });
    expect(
      screen.getByLabelText("Filter threads by project").textContent,
    ).toContain(label);
    expect(screen.getByLabelText("Filter threads by project").getAttribute("title")).toBe(label);
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: "beta" }));
    expect(onProjectChange).toHaveBeenLastCalledWith("proj_beta");
    expect(screen.getByLabelText("Filter threads by project").textContent).toContain("beta");
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: label }));
    expect(onProjectChange).toHaveBeenLastCalledWith("");
  });

  it("updates the current scope and reset option when the organization changes", async () => {
    const onProjectChange = vi.fn();
    const fixture = await renderSidebar({ onProjectChange, organizationName: "Secondary Example Organization" });
    fixture.setOrganization("Tertiary Example Organization");
    expect(screen.getByLabelText("Filter threads by project").textContent).toContain("All Tertiary Example Organization projects");
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: "beta" }));
    fixture.setOrganization("Secondary Example Organization");
    expect(screen.getByLabelText("Filter threads by project").textContent).toContain("beta");
    expect(onProjectChange).toHaveBeenLastCalledWith("proj_beta");
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: "All Secondary Example Organization projects" }));
    expect(onProjectChange).toHaveBeenLastCalledWith("");
  });

  it("carries the footer utility menu, pointed at Detent's destinations", async () => {
    for (const [label, destination] of [
      ["Settings", "/settings"],

      ["Pull Requests", "/work/changes"],
      ["Usage", "/usage"],
    ] as const) {

      const { navigated } = await renderSidebar();
      const footer = within(
        document.querySelector("[data-slot='sidebar-footer']") as HTMLElement,
      );
      fireEvent.click(footer.getByRole("button", { name: label }));
      await settleNavigation();
      expect(navigated, label).toContain(destination);
      expect(
        within(
          document.querySelector("[data-slot='sidebar-footer']") as HTMLElement,
        ).getByRole("button", { name: "Back" }),
      ).toBeTruthy();
      cleanup();
    }
  });

  it("offers the update check in the status slot, and no second transport chip", async () => {

    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("", { status: 404 })),
    );
    await renderSidebar();
    expect(screen.queryByTestId("sidebar-connection")).toBeNull();
    expect(
      await screen.findByRole("button", { name: "Check for updates" }),
    ).toBeTruthy();
  });

  it("gives every thread row the hover preview card", async () => {
    await renderSidebar({ conversations: [running] });
    const row = rowFor(sidebar(), "Lock renewal");
    expect(row?.closest("[data-slot='tooltip-trigger']")).toBeTruthy();
  });
});


it("renders only human actions while retaining the diagnostic findings count", async () => {
  const findings = [
    { id: "hf_1", class: "instance", summary: "Runner attention", next_action: "Check runner", subject: { kind: "runner", id: "runner_1" }, opened_at: "2026-10-06T09:00:00Z", resolved_at: null, severity: "attention", label: "Studio", to: "/fleet" },
    { id: "hf_2", class: "human", summary: "Human attention", next_action: "Check issue", subject: { kind: "work_item", id: "wi_2" }, opened_at: "2026-10-06T10:00:00Z", resolved_at: null, severity: "attention", label: "#2 Migration question", to: "/work/i/wi_2?tab=diagnostics" },
  ] as const;
  const action = { id: "wi_2", label: "#2 Migration question", to: "/work/i/wi_2", opened_at: "2026-10-06T10:00:00Z" };
  const harness = await renderSidebar({ findings, conversations: [needsYou], humanActions: [action] });
  const group = screen.getByTestId("sidebar-needs-you");
  expect(within(group).getAllByRole("button").map((row) => row.dataset.testid)).toEqual([
    "sidebar-needs-you-shelf-toggle", "attention-issue-wi_2",
  ]);
  expect(screen.getByTestId("diagnostics-findings-count").textContent).toBe("2");
  expect(screen.queryByTestId("finding-hf_1")).toBeNull();
  expect(screen.queryByTestId("finding-hf_2")).toBeNull();
  const row = screen.getByTestId("attention-issue-wi_2");
  expect(row.querySelectorAll("time")).toHaveLength(1);
  fireEvent.click(row);
  expect(harness.onNavigate).toHaveBeenLastCalledWith(action.to);
  fireEvent.click(screen.getByTestId("sidebar-needs-you-shelf-toggle"));
  expect(screen.queryByTestId("attention-issue-wi_2")).toBeNull();
});

const mocks = vi.hoisted(() => ({
  listHealthFindings: vi.fn(),
  getWorkItem: vi.fn(),
  getProject: vi.fn(),
  listWorkItems: vi.fn(),
  getExplanation: vi.fn(),
  runnerNames: new Map<string, { display: string; host: string }>(),
  realHttp: false,
  eventsUrl: (id: string) => `/projects/${id}/events`,
}));
vi.mock("../../src/app/work/lib/useWork.ts", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../src/app/work/lib/useWork.ts")>();
  return { ...actual, useWorkHttp: () => mocks.realHttp ? actual.useWorkHttp() : mocks };
});
vi.mock("../../src/app/work/lib/runnerNames.ts", () => ({
  useRunnerNames: () => mocks.runnerNames,
  runnerDisplay: (names: typeof mocks.runnerNames, id: string) => names.get(id)?.display ?? (id === "runner_1" ? "Studio" : id),
}));

class Source extends EventTarget {
  static instances: Source[] = [];
  close = vi.fn();
  constructor(readonly url: string) { super(); Source.instances.push(this); }
}

afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); vi.resetAllMocks(); mocks.runnerNames = new Map(); mocks.realHttp = false; Source.instances = []; });

it("bounds findings reads to mount and health ticks across renders and runner name updates", async () => {
  vi.useFakeTimers();
  vi.stubGlobal("EventSource", Source);
  const projects = ["p1", "p2", "p3", "p4"].map((id) => ({ id, name: `Project ${id}`, can_write: true }));
  let tick = "2026-10-06T10:00:00Z";
  let items = [{ id: "hf_runner", class: "instance", summary: "Runner attention", next_action: "Check runner", subject: { kind: "runner", id: "runner_1" }, opened_at: tick, severity: "attention", resolved_at: null }];
  const request = vi.fn(async (_url: RequestInfo | URL) => new Response(JSON.stringify({ items, last_tick_at: tick })));
  vi.stubGlobal("fetch", request);
  mocks.realHttp = true;
  const client = makeClient({ bootstrap: { ...bootstrapFixture, projects } as Bootstrap });
  const { result, rerender, unmount } = renderHook(() => useSidebarFindings(projects.map((project) => ({ ...project }))), {
    wrapper: ({ children }) => <ClientContext.Provider value={client}><HealthFindingsProvider projects={projects}>{children}</HealthFindingsProvider></ClientContext.Provider>,
  });
  const source = Source.instances[0]!;
  await act(async () => {
    source.dispatchEvent(new Event("open"));
    source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
  });
  expect(result.current[0]?.label).toBe("Studio");
  const expectReads = (perProject: number) => {
    for (const project of projects) expect(request.mock.calls.filter(([url]) => String(url).includes(`/projects/${project.id}/health/findings`))).toHaveLength(perProject);
  };
  expectReads(1);
  for (let second = 0; second < 60; second++) {
    mocks.runnerNames = new Map([["runner_1", { display: `Runner ${second}`, host: "example-host" }]]);
    rerender();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
      source.dispatchEvent(new MessageEvent("activity", { data: String(second) }));
      source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
    });
    expectReads(1);
  }
  expect(result.current[0]?.label).toBe("Runner 59");
  expect(Source.instances).toHaveLength(1);
  items = [];
  tick = "2026-10-06T10:01:00Z";
  await act(async () => source.dispatchEvent(new MessageEvent("health.findings", { data: tick })));
  expectReads(2);
  expect(result.current).toEqual([]);
  items = [{ id: "hf_opened", class: "instance", summary: "New runner attention", next_action: "Check runner", subject: { kind: "runner", id: "runner_1" }, opened_at: tick, severity: "attention", resolved_at: null }];
  tick = "2026-10-06T10:02:00Z";
  await act(async () => {
    source.dispatchEvent(new Event("open"));
    source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
  });
  expectReads(3);
  expect(result.current.map((finding) => [finding.id, finding.label, finding.to])).toEqual([["hf_opened", "Runner 59", "/fleet"]]);
  unmount();
  expect(source.close).toHaveBeenCalledOnce();
});

it.each(["mount", "health event"])("retries the current health tick after a failed %s refresh", async (phase) => {
  vi.stubGlobal("EventSource", Source);
  const projects = ["p1", "p2"].map((id) => ({ id, name: `Project ${id}`, can_write: true }));
  let tick = "2026-10-06T10:00:00Z";
  let fail = phase === "mount";
  let reject: (reason: Error) => void = () => {};
  mocks.listHealthFindings.mockImplementation(async (id) => {
    if (id === "p2" && fail) return new Promise((_resolve, rejectRequest) => { reject = rejectRequest; });
    return { items: [{ id: `hf_${id}`, class: "flow", summary: "Project attention", next_action: "Check project", subject: { kind: "project", id }, opened_at: tick, severity: "attention", resolved_at: null }], last_tick_at: tick };
  });
  const { result, rerender } = renderHook(() => ({ findings: useSidebarFindings(projects), health: useHealthFindings() }), {
    wrapper: ({ children }) => <HealthFindingsProvider projects={projects}>{children}</HealthFindingsProvider>,
  });
  const source = Source.instances[0]!;
  if (phase === "health event") {
    await waitFor(() => expect(result.current.findings.map((finding) => finding.id)).toEqual(["hf_p1", "hf_p2"]));
    fail = true;
    tick = "2026-10-06T10:01:00Z";
  }
  const attempts = phase === "mount" ? 1 : 2;
  if (phase === "health event") await act(async () => {
    source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
    source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
  });
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes(attempts * 2);
  await act(async () => reject(new Error("Network unavailable")));
  expect(result.current.health.error).toBe(true);
  expect(result.current.findings.map((finding) => finding.id).sort()).toEqual(phase === "mount" ? ["hf_p1"] : ["hf_p1", "hf_p2"]);
  rerender();
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes(attempts * 2);
  fail = false;
  await act(async () => {
    source.dispatchEvent(new Event("open"));
    source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
    source.dispatchEvent(new MessageEvent("health.findings", { data: tick }));
  });
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes((attempts + 1) * 2);
  expect(result.current.health.error).toBe(false);
  expect(result.current.health.read?.last_tick_at).toBe(tick);
  expect(result.current.findings.map((finding) => [finding.id, finding.to])).toEqual([["hf_p1", "/diagnostics"], ["hf_p2", "/diagnostics"]]);
  await act(async () => source.dispatchEvent(new MessageEvent("health.findings", { data: tick })));
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes((attempts + 1) * 2);
});

it("reads current human holds without promoting dependency waits or stale health findings", async () => {
  vi.stubGlobal("EventSource", Source);
  vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-10-20T12:00:00Z"));
  const projects = [{ id: "p1", name: "Example project", can_write: true }, { id: "p2", name: "Read only", can_write: false }];
  const issue = (id: string, state = "Blocked") => ({ ...itemFixture, project_id: "p1", work_item_id: id, state, terminal: false });
  let items = [issue("wi_dependency"), issue("wi_old"), issue("wi_recent"), issue("wi_answered"), issue("wi_resumed", "Todo"),
    { ...issue("wi_contract"), issue_contract: { human_action: "Approve the product decision" } }];
  let answered = false;
  mocks.getProject.mockResolvedValue({ states: [
    { name: "Blocked", terminal: false, dispatchable: false },
    { name: "Todo", terminal: false, dispatchable: true },
    { name: "Backlog", terminal: false, dispatchable: false },
  ] });
  mocks.listWorkItems.mockImplementation(async () => ({ items }));
  mocks.getExplanation.mockImplementation(async (_project, id) => ({
    current_lane: { name: id === "wi_resumed" ? "Todo" : "Blocked", freshness: "available" },
    required_gate: { state: "unavailable", source_state: "unavailable" },
    latest_transition: { at: id === "wi_old" ? "2026-10-17T12:00:00Z" : "2026-10-20T10:00:00Z" },
    native_runtime: { attempt: { updated_at: "2026-10-20T11:59:00Z", disposition: {
      status: "blocked", human_action: !["wi_dependency", "wi_answered", "wi_contract"].includes(id) && !answered,
    } } },
  }));
  const { result } = renderHook(() => useSidebarHumanActions(projects));
  await waitFor(() => expect(result.current.map((action) => action.id)).toEqual(["wi_old", "wi_contract", "wi_recent"]));
  expect(mocks.getProject.mock.calls.map(([id]) => id)).toEqual(["p1"]);
  expect(mocks.listWorkItems).toHaveBeenCalledWith(expect.objectContaining({ state: ["Blocked"] }));
  await renderSidebar({ humanActions: result.current, conversations: [] });
  for (const [id, start, label] of [
    ["wi_old", "2026-10-17T12:00:00Z", "3d"],
    ["wi_recent", "2026-10-20T10:00:00Z", "2h"],
  ]) {
    expect(within(screen.getByTestId(`attention-issue-${id}`)).getByText(label!).getAttribute("datetime")).toBe(start);
  }
  items = [issue("wi_old", "Todo"), issue("wi_dependency")];
  await act(async () => Source.instances[0]!.dispatchEvent(new MessageEvent("activity", { data: "1" })));
  await waitFor(() => expect(result.current).toEqual([]));
  items = [issue("wi_recent")];
  await act(async () => Source.instances[0]!.dispatchEvent(new MessageEvent("activity", { data: "2" })));
  await waitFor(() => expect(result.current.map((action) => action.id)).toEqual(["wi_recent"]));
  const explained = mocks.getExplanation.mock.calls.length;
  await act(async () => Source.instances[0]!.dispatchEvent(new MessageEvent("activity", { data: "2b" })));
  await waitFor(() => expect(mocks.listWorkItems.mock.calls.length).toBeGreaterThan(3));
  expect(mocks.getExplanation.mock.calls.length).toBe(explained);
  answered = true;
  items = [{ ...issue("wi_recent"), revision: "answered" }];
  await act(async () => Source.instances[0]!.dispatchEvent(new MessageEvent("activity", { data: "3" })));
  await waitFor(() => expect(result.current).toEqual([]));
});

it("keeps health findings and failed or waiting conversations out of Needs you without a current human action", async () => {
  await renderSidebar({ findings: [{ id: "hf_stall", class: "flow", severity: "attention", summary: "Delivery stalled", next_action: "Inspect flow", subject: { kind: "project", id: "p1" }, opened_at: "2026-10-20T10:00:00Z", resolved_at: null, label: "Example project", to: "/diagnostics" }], conversations: [needsYou, conversation({ execution: { ...running.execution, status: "failed" } })] });
  expect(screen.queryByTestId("sidebar-needs-you")).toBeNull();
  expect(screen.getByTestId("diagnostics-findings-count").textContent).toBe("1");
});

it("shares project events while reading every page, filtering and resolving findings", async () => {
  vi.stubGlobal("EventSource", Source);
  const finding = (id: string, kind: string, subject: string, age: number, severity = "attention", resolved_at: string | null = null) => ({
    id, class: "flow", summary: "Flow attention", next_action: "Check flow", subject: { kind, id: subject }, opened_at: `2026-10-06T${String(age).padStart(2, "0")}:00:00Z`, severity, resolved_at,
  });
  const runner = finding("hf_runner", "runner", "runner_1", 10);
  const item = finding("hf_item", "work_item", "wi_1", 11);
  const project = finding("hf_project", "project", "p1", 9);
  let resolved = false;
  mocks.listHealthFindings.mockImplementation(async (id, cursor) => {
    if (resolved) return { items: [], last_tick_at: null };
    if (id === "p2") return { items: [runner], last_tick_at: null };
    if (cursor) return { items: [project], last_tick_at: null };
    return { items: [item, runner, finding("hf_watch", "project", "p1", 8, "watch"), finding("hf_closed", "project", "p1", 7, "attention", "2026-10-06T12:00:00Z")], next_cursor: "page2", last_tick_at: null };
  });
  mocks.getWorkItem.mockResolvedValue({ number: 550, title: "Open findings" });
  const onActivity = vi.fn();
  const unsubscribe = subscribeProjectEvents(mocks, "p1", { activity: onActivity });
  const { result, unmount } = renderHook(() => useSidebarFindings([{ id: "p1", name: "Example project", can_write: true }, { id: "p2", name: "Other", can_write: true }]), { wrapper: ({ children }) => <HealthFindingsProvider projects={[{ id: "p1", name: "Example project", can_write: true }, { id: "p2", name: "Other", can_write: true }]}>{children}</HealthFindingsProvider> });
  await waitFor(() => expect(result.current.map((f) => f.id)).toEqual(["hf_project", "hf_runner", "hf_item"]));
  expect(Source.instances.map((source) => source.url)).toEqual(["/projects/p1/events"]);
  expect(result.current.map((f) => [f.label, f.to])).toEqual([
    ["Example project", "/diagnostics"], ["Studio", "/fleet"], ["#550 Open findings", "/work/i/wi_1?tab=diagnostics"],
  ]);
  expect(mocks.listHealthFindings).toHaveBeenCalledWith("p1", "page2", expect.any(AbortSignal), undefined);
  resolved = true;
  act(() => { for (const source of Source.instances) source.dispatchEvent(new MessageEvent("health.findings", { data: "new-tick" })); });
  await waitFor(() => expect(result.current).toEqual([]));
  expect(mocks.listHealthFindings.mock.calls.filter(([id]) => id === "p2")).toHaveLength(2);
  const reads = mocks.listHealthFindings.mock.calls.length;
  act(() => { for (const source of Source.instances) source.dispatchEvent(new MessageEvent("health.findings", { data: "new-tick" })); });
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes(reads);
  const source = Source.instances[0]!;
  const activity = new MessageEvent("activity", { data: "42" });
  act(() => source.dispatchEvent(activity));
  expect(onActivity).toHaveBeenCalledWith(activity);
  const lateActivity = vi.fn();
  const unsubscribeLate = subscribeProjectEvents(mocks, "p1", { activity: lateActivity });
  expect(lateActivity).toHaveBeenCalledWith(activity);
  unsubscribeLate();
  unmount();
  expect(source.close).not.toHaveBeenCalled();
  act(() => source.dispatchEvent(new MessageEvent("health.findings", { data: "after-unmount" })));
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes(reads);
  act(() => source.dispatchEvent(new MessageEvent("activity", { data: "43" })));
  expect(onActivity).toHaveBeenCalledTimes(2);
  unsubscribe();
  expect(source.close).toHaveBeenCalledOnce();
});

it("discards an older response after a live refresh", async () => {
  vi.stubGlobal("EventSource", Source);
  let finish: (value: unknown) => void = () => {};
  mocks.listHealthFindings.mockResolvedValueOnce({ items: [], last_tick_at: null })
    .mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }))
    .mockResolvedValueOnce({ items: [], last_tick_at: null });
  const { result } = renderHook(() => useSidebarFindings([{ id: "p1", name: "Example project", can_write: true }]), { wrapper: ({ children }) => <HealthFindingsProvider projects={[{ id: "p1", name: "Example project", can_write: true }]}>{children}</HealthFindingsProvider> });
  await act(async () => Source.instances[0]!.dispatchEvent(new MessageEvent("health.findings", { data: "older" })));
  await waitFor(() => expect(mocks.listHealthFindings).toHaveBeenCalledTimes(2));
  await act(async () => Source.instances[0]!.dispatchEvent(new MessageEvent("health.findings", { data: "newer" })));
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes(3);
  await act(async () => finish({ items: [{ id: "stale", class: "flow", summary: "Flow attention", next_action: "Check flow", subject: { kind: "project", id: "p1" }, opened_at: "2026-10-06T00:00:00Z", severity: "attention", resolved_at: null }], last_tick_at: null }));
  expect(result.current).toEqual([]);
});


describe("existing thread removal menu", () => {
  it.each([
    { action: "archive", label: "Archive thread", outcome: "cancel" },
    { action: "delete", label: "Delete", outcome: "cancel" },
    { action: "archive", label: "Archive thread", outcome: "failure" },
    { action: "delete", label: "Delete", outcome: "failure" },
    { action: "archive", label: "Archive thread", outcome: "success" },
    { action: "delete", label: "Delete", outcome: "success" },
  ])("$action $outcome preserves confirmation and navigation", async ({ action, label, outcome }) => {
    const chat = conversation({ id: "conv_removal", project_id: "proj_alpha", title: "Menu removal", execution: { ...recentChat.execution, status: "idle" } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(outcome !== "cancel");
    vi.spyOn(settings, "useClientSettings").mockImplementation((select) => select({
      ...settings.getClientSettings(), confirmThreadArchive: true, confirmThreadDelete: true,
    }));
    const request = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => outcome === "failure"
      ? new Response(JSON.stringify({ code: "stale_execution", message: "Finish the running turn first" }), { status: 409 })
      : action === "archive"
        ? new Response(JSON.stringify({ ...chat, archived: true, revision: chat.revision + 1 }), { status: 200 })
        : new Response(null, { status: 204 }));
    const client = makeClient({ bootstrap: bootstrapFixture as Bootstrap, fetch: request });
    const h = await renderSidebar({ conversations: [chat], activeConversationId: chat.id }, { client });
    fireEvent.contextMenu(rowFor(sidebar(), chat.title)!);
    await act(async () => {
      await new Promise<void>((resolve) => window.requestAnimationFrame(() => resolve()));
    });
    fireEvent.click(await screen.findByRole("button", { name: label }));
    await waitFor(() => expect(confirm).toHaveBeenCalled());
    expect(confirm.mock.calls[0]?.[0]).toContain(chat.title);
    if (outcome === "cancel") {
      expect(request).not.toHaveBeenCalled();
      expect(h.onNavigate).not.toHaveBeenCalled();
      expect(rowFor(sidebar(), chat.title)).not.toBeNull();
      return;
    }
    await waitFor(() => expect(request).toHaveBeenCalledOnce());
    expect(String(request.mock.calls[0]?.[0])).toContain(`/projects/proj_alpha/conversations/${chat.id}${action === "archive" ? "/archive" : ""}`);
    if (outcome === "failure") {
      await screen.findByText(action === "archive" ? "Failed to archive thread" : "Failed to delete thread");
      expect(screen.getByText("Finish the running turn first")).toBeTruthy();
      expect(h.onNavigate).not.toHaveBeenCalled();
      expect(rowFor(sidebar(), chat.title)).not.toBeNull();
    } else {
      await waitFor(() => expect(h.onNavigate).toHaveBeenCalledWith("/chat/p/proj_alpha"));
    }
  });
});

it.each([true, false])("shows the settings gear unread dot from bootstrap (%s) and observes mark-seen", async (has_unread) => {
  const account = Schema.decodeUnknownSync(AccountBootstrap)({ ...accountFixture, has_unread });
  const client = makeClient({ bootstrap: bootstrapFixture as unknown as Bootstrap, account });
  await renderSidebar({}, { client });
  const gear = screen.getByRole("button", { name: has_unread ? "Settings, new release notes" : "Settings" });
  expect(gear.querySelector('[aria-hidden="true"].bg-primary') !== null).toBe(has_unread);
  act(() => setReleaseNotesUnread(account, false));
  expect(screen.getByRole("button", { name: "Settings" }).querySelector('[aria-hidden="true"].bg-primary')).toBeNull();
});

function readyResource<A>(value: A): Resource<A> {
  return { value, error: null, loading: false, refresh: vi.fn(async () => undefined), set: vi.fn() };
}

it("shows the first unready setup step for the active project and hides setup once ready", () => {
  const onNavigate = vi.fn();
  const data: SidebarShellData = {
    organizationName: "Example organization", projects: [], activeProjectId: "proj_example",
    onProjectChange: vi.fn(), conversations: [], serverResults: [], activeConversationId: null,
    onSelect: vi.fn(), onNewChat: vi.fn(), onRename: vi.fn(), attention: new Set(),
    navigation: { activePath: "/chat", onNavigate },
  };
  const onboarding: Onboarding = {
    progress: { revision: "1", repository: "existing", doctor: false, provider: false, artifacts: "local" },
    runners: [], artifact_services: [], ready: false,
    steps: [
      { name: "Runner", state: "ready", detail: "" },
      { name: "Repository", state: "ready", detail: "" },
      { name: "Checkout", state: "action_required", detail: "" },
      { name: "Run files", state: "ready", detail: "" },
    ],
  };
  const integration = readyResource({ checkout_repository: "example-org/example-project" } as ProjectIntegration);
  const view = (ready: boolean) => <SidebarDataProvider value={data}>
    <ProjectSetupContext.Provider value={{ projectId: "proj_example", onboarding: readyResource({ ...onboarding, ready }), integration }}>
      <SidebarProvider><SidebarDestinations /></SidebarProvider>
    </ProjectSetupContext.Provider>
  </SidebarDataProvider>;
  const mounted = render(view(false));
  const setup = screen.getByRole("button", { name: "Setup 3 of 4" });
  fireEvent.click(setup);
  expect(onNavigate).toHaveBeenCalledWith("/projects/proj_example/setup");
  mounted.rerender(view(true));
  expect(screen.queryByRole("button", { name: /^Setup/ })).toBeNull();
});
