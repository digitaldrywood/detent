// @vitest-environment jsdom
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
  renderHook,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { useSidebarFindings } from "../../src/app/adapters/sidebarFindings.ts";
import { resetUpdateCheckState } from "../../src/app/adapters/detentUpdates.ts";
import { conversation } from "./builders.ts";
import { renderSidebar, resetSidebarState, rowFor } from "./sidebarHarness.tsx";

afterEach(() => {
  cleanup();
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

  // §17.2: the brand row is the product, not the tenant.
  it("reads Detent Cloud in the brand row", async () => {
    await renderSidebar();
    const brand = screen.getByLabelText("Go to Detent Cloud");
    expect(brand.textContent).toContain("Detent");
    expect(brand.textContent).toContain("Cloud");
    expect(brand.getAttribute("href")).toBe("/work");
  });

  it("names the project scope and reports project and all-project selections", async () => {
    const onProjectChange = vi.fn();
    await renderSidebar({ onProjectChange });
    expect(
      screen.getByLabelText("Filter threads by project").textContent,
    ).toContain("All projects");
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: "beta" }));
    expect(onProjectChange).toHaveBeenLastCalledWith("proj_beta");
    fireEvent.click(screen.getByLabelText("Filter threads by project"));
    fireEvent.click(await screen.findByRole("option", { name: "All projects" }));
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


it("places finding rows below attention issues with one age signal and navigation", async () => {
  const findings = [
    { id: "hf_1", class: "instance", summary: "Runner attention", next_action: "Check runner", subject: { kind: "runner", id: "runner_1" }, opened_at: "2026-10-06T09:00:00Z", resolved_at: null, severity: "attention", label: "Studio", to: "/fleet" },
    { id: "hf_2", class: "human", summary: "Human attention", next_action: "Check issue", subject: { kind: "work_item", id: "wi_2" }, opened_at: "2026-10-06T10:00:00Z", resolved_at: null, severity: "attention", label: "#2 Migration question", to: "/work/i/wi_2?tab=diagnostics" },
  ] as const;
  const harness = await renderSidebar({ findings, conversations: [needsYou] });
  const group = screen.getByTestId("sidebar-needs-you");
  expect(within(group).getAllByRole("button").map((row) => row.dataset.testid)).toEqual([
    "sidebar-needs-you-shelf-toggle", "attention-issue-conv_needs", "finding-hf_1", "finding-hf_2",
  ]);
  expect(screen.getByTestId("diagnostics-findings-count").textContent).toBe("2");
  for (const finding of findings) {
    const row = screen.getByTestId(`finding-${finding.id}`);
    expect(row.querySelectorAll("time")).toHaveLength(1);
    expect(row.querySelector("[data-slot=badge]")).toBeNull();
    fireEvent.click(row);
    expect(harness.onNavigate).toHaveBeenLastCalledWith(finding.to);
  }
  fireEvent.click(screen.getByTestId("sidebar-needs-you-shelf-toggle"));
  expect(screen.queryByTestId("finding-hf_1")).toBeNull();
});

const mocks = vi.hoisted(() => ({
  listHealthFindings: vi.fn(),
  getWorkItem: vi.fn(),
  eventsUrl: (id: string) => `/projects/${id}/events`,
}));
vi.mock("../../src/app/work/lib/useWork.ts", () => ({ useWorkHttp: () => mocks }));
vi.mock("../../src/app/work/lib/runnerNames.ts", () => ({
  useRunnerNames: () => undefined,
  runnerDisplay: (_names: unknown, id: string) => id === "runner_1" ? "Studio" : id,
}));

class Source extends EventTarget {
  static instances: Source[] = [];
  close = vi.fn();
  constructor(readonly url: string) { super(); Source.instances.push(this); }
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.resetAllMocks(); Source.instances = []; });

it("reads every page, deduplicates projects, filters severity and resolves through the stream", async () => {
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
  const { result, unmount } = renderHook(() => useSidebarFindings([{ id: "p1", name: "Detent", can_write: true }, { id: "p2", name: "Other", can_write: true }]));
  await waitFor(() => expect(result.current.map((f) => f.id)).toEqual(["hf_project", "hf_runner", "hf_item"]));
  expect(result.current.map((f) => [f.label, f.to])).toEqual([
    ["Detent", "/diagnostics"], ["Studio", "/fleet"], ["#550 Open findings", "/work/i/wi_1?tab=diagnostics"],
  ]);
  expect(mocks.listHealthFindings).toHaveBeenCalledWith("p1", "page2", expect.any(AbortSignal));
  resolved = true;
  act(() => { for (const source of Source.instances) source.dispatchEvent(new MessageEvent("health.findings", { data: "new-tick" })); });
  await waitFor(() => expect(result.current).toEqual([]));
  const reads = mocks.listHealthFindings.mock.calls.length;
  act(() => { for (const source of Source.instances) source.dispatchEvent(new MessageEvent("health.findings", { data: "new-tick" })); });
  expect(mocks.listHealthFindings).toHaveBeenCalledTimes(reads);
  unmount();
  for (const source of Source.instances) expect(source.close).toHaveBeenCalledOnce();
});

it("discards an older response after a live refresh", async () => {
  vi.stubGlobal("EventSource", Source);
  let finish: (value: unknown) => void = () => {};
  mocks.listHealthFindings.mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }))
    .mockResolvedValueOnce({ items: [], last_tick_at: null });
  const { result } = renderHook(() => useSidebarFindings([{ id: "p1", name: "Detent", can_write: true }]));
  act(() => Source.instances[0]!.dispatchEvent(new MessageEvent("health.findings", { data: "tick" })));
  await waitFor(() => expect(mocks.listHealthFindings).toHaveBeenCalledTimes(2));
  await act(async () => finish({ items: [{ id: "stale", class: "flow", summary: "Flow attention", next_action: "Check flow", subject: { kind: "project", id: "p1" }, opened_at: "2026-10-06T00:00:00Z", severity: "attention", resolved_at: null }], last_tick_at: null }));
  expect(result.current).toEqual([]);
});
