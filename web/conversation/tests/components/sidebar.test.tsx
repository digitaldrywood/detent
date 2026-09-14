// @vitest-environment jsdom
import {
  act,
  cleanup,
  fireEvent,
  screen,
  within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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

  it("names the project scope in the project combobox", async () => {
    await renderSidebar();
    expect(
      screen.getByLabelText("Filter threads by project").textContent,
    ).toContain("All projects");
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
