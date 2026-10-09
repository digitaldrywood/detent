// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import changeFixture from "../../src/contracts/fixtures/work-change-detail.json";
import historyFixture from "../../src/contracts/fixtures/work-history.json";
import attemptsFixture from "../../src/contracts/fixtures/work-attempt-list.json";
import itemFixture from "../../src/contracts/fixtures/work-item.json";
import projectFixture from "../../src/contracts/fixtures/work-project.json";
import attemptDiffFixture from "../../src/contracts/fixtures/work-attempt-diff.json";
import type {
  AttemptDiff,
  ChangeDetail,
  CollaborationEvent,
  NativeAttempt,
  NativeIssue,
  NativeProject,
} from "../../src/contracts/work.ts";
import { BoardLane } from "../../src/app/work/components/BoardLane.tsx";
import { IssueCard } from "../../src/app/work/components/IssueCard.tsx";
import { DiffSurface } from "../../src/app/components/surfaces/DiffSurface.tsx";
import { diffSource, readAttemptDiff, type DiffSource } from "../../src/app/adapters/surfaces.ts";
import { WorkList } from "../../src/app/work/components/WorkList.tsx";
import { WorkTopBar } from "../../src/app/work/components/WorkTopBar.tsx";
import { CompletedCounter } from "../../src/app/work/components/CompletedCounter.tsx";
import { toAttemptView, toWorkItemView, transitionsFrom } from "../../src/app/work/lib/fromWire.ts";
import { boardStats, isBlocked, isLive, type Lane, type WorkItemView } from "../../src/app/work/lib/model.ts";

import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { ClientContext } from "../../src/app/client.ts";
import { WorkBoard } from "../../src/app/work/WorkBoard.tsx";
import { SidebarSearchProvider } from "../../src/app/adapters/sidebarData.tsx";
import { Input } from "../../src/components/ui/input.tsx";
import { SidebarProvider } from "../../src/components/ui/sidebar.tsx";
import { DEFAULT_VIEW_STATE, parseViewState, serializeViewState } from "../../src/app/work/lib/viewState.ts";
import { clearBoardCache } from "../../src/app/work/lib/boardStore.ts";
import { useBoard, useNow } from "../../src/app/work/lib/useWork.ts";
import { resetRunnerNamesForTests } from "../../src/app/work/lib/runnerNames.ts";
import { workPaginationFixture } from "../workPaginationFixture.ts";
import { plainSearchOptions } from "../../src/app/lib/searchParams.ts";

const clockRenders = vi.hoisted(() => ({ card: 0, toolbar: 0, list: 0 }));

vi.mock("../../src/app/work/components/IssueCard.tsx", async (original) => {
  const module = await original<typeof import("../../src/app/work/components/IssueCard.tsx")>();
  return { ...module, IssueCard: (props: Parameters<typeof module.IssueCard>[0]) => {
    clockRenders.card++;
    return module.IssueCard(props);
  } };
});

vi.mock("../../src/app/work/components/WorkToolbar.tsx", async (original) => {
  const module = await original<typeof import("../../src/app/work/components/WorkToolbar.tsx")>();
  return { ...module, WorkToolbar: (props: Parameters<typeof module.WorkToolbar>[0]) => {
    clockRenders.toolbar++;
    return module.WorkToolbar(props);
  } };
});

vi.mock("../../src/app/work/components/WorkList.tsx", async (original) => {
  const module = await original<typeof import("../../src/app/work/components/WorkList.tsx")>();
  return { ...module, WorkList: (props: Parameters<typeof module.WorkList>[0]) => {
    clockRenders.list++;
    return module.WorkList(props);
  } };
});

afterEach(() => { cleanup(); clearBoardCache(); });

const NOW = Date.parse("2026-09-09T12:00:00Z");
const PROJECT = projectFixture as unknown as NativeProject;
const ATTEMPTS = (attemptsFixture as { items: unknown[] }).items as unknown as NativeAttempt[];
const HISTORY = (historyFixture as { items: unknown[] }).items as unknown as CollaborationEvent[];
const CHANGE = changeFixture as unknown as ChangeDetail;
const ATTEMPT_DIFF = attemptDiffFixture as unknown as AttemptDiff;

function item(overrides: Partial<WorkItemView> = {}): WorkItemView {
  return {
    ...toWorkItemView(itemFixture as unknown as NativeIssue, "parable"),
    ...overrides,
  };
}

const LANES: readonly Lane[] = PROJECT.states.map((state) => ({
  id: state.name,
  name: state.name,
  terminal: state.terminal,
  dispatchable: state.dispatchable,
  category: state.terminal ? "completed" : state.dispatchable ? "unstarted" : "started",
}));

describe("the board card", () => {
  it("names the issue, its number and its age", () => {
    render(
      <IssueCard
        item={item()}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        moves={[]}
        onMove={vi.fn()}
      />,
    );
    expect(screen.getByTestId("issue-card-open").textContent).toContain(
      "Checkout lock renewal waits on a healthy handoff",
    );
    expect(screen.getByText("#3363")).not.toBeNull();
    expect(screen.getByTestId("issue-age").textContent).toMatch(/^Updated \d/);
    expect(screen.getByTestId("issue-age").getAttribute("title")).toContain(item().updatedAt);
    expect(screen.getByTestId("issue-card").getAttribute("title")).toBeNull();
    expect(screen.getByTestId("issue-card").hasAttribute("data-live")).toBe(false);
  });

  it("shows the project dot only in the all-projects scope", () => {
    const { unmount } = render(
      <IssueCard item={item()} showProject now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />,
    );
    expect(screen.getByTestId("project-dot")).not.toBeNull();
    expect(screen.getByText("parable")).not.toBeNull();
    unmount();

    render(
      <IssueCard
        item={item()}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        moves={[]}
        onMove={vi.fn()}
      />,
    );
    expect(screen.queryByTestId("project-dot")).toBeNull();
  });

  it("renders the worker strip only while an attempt is running", () => {
    const running = item({ attempt: toAttemptView(ATTEMPTS) });
    const { unmount } = render(
      <IssueCard
        item={running}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        moves={[]}
        onMove={vi.fn()}
      />,
    );
    const strip = screen.getByTestId("worker-strip");
    expect(strip.textContent).toContain("gpt-6-astra");
    expect(strip.textContent).toContain("codex");
    unmount();

    render(
      <IssueCard
        item={item({ attempt: toAttemptView([ATTEMPTS[0]!]) })}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        moves={[]}
        onMove={vi.fn()}
      />,
    );
    expect(screen.queryByTestId("worker-strip")).toBeNull();
  });

  it.each([
    { state: "Done", priority: "Urgent", attemptStatus: "failed", showProject: true },
    { state: "Done", priority: "High", attemptStatus: "interrupted", showProject: false },
    { state: "Cancelled", priority: "Urgent", attemptStatus: "running", showProject: true },
    { state: "Retired", priority: "High", attemptStatus: "running", showProject: false },
    { state: "Failed", priority: "Urgent", attemptStatus: "failed", showProject: true },
  ] as const)("closes $state before $attemptStatus activity and $priority priority", ({
    state, priority, attemptStatus, showProject,
  }) => {
    const issue = toWorkItemView(
      { ...itemFixture as unknown as NativeIssue, state, terminal: true },
      "parable",
      { attempts: [{ ...ATTEMPTS[0]!, status: attemptStatus }] },
    );
    const closed = { ...issue, priority };
    const onOpen = vi.fn();
    render(
      <IssueCard item={closed} showProject={showProject} now={NOW}
        onOpen={onOpen} moves={["Todo"]} onMove={vi.fn()} moving />,
    );
    const card = screen.getByTestId("issue-card");
    const cue = screen.getByText(state);
    expect(cue.className).toContain("text-muted-foreground");
    expect(cue.getAttribute("title")).toBe(`Last attempt: ${attemptStatus}`);
    expect(cue.getAttribute("aria-label")).toBe(`${state}. Last attempt: ${attemptStatus}`);
    expect(screen.getByLabelText(`Priority: ${priority}`).className).toContain("bg-muted");
    expect(card.getAttribute("data-live")).toBeNull();
    expect(card.className).toContain("bg-muted");
    expect(card.className).not.toMatch(/border-success|animate-pulse|opacity-/);
    expect(card.innerHTML).not.toMatch(/bg-success|text-success|bg-error|text-error|bg-warning|text-warning/);
    expect(screen.queryByTestId("worker-strip")).toBeNull();
    expect(screen.queryByText("Running")).toBeNull();
    expect(screen.queryByText(/Blocked/)).toBeNull();
    expect(screen.queryByText("Attempt failed")).toBeNull();
    expect(screen.queryByText("Interrupted")).toBeNull();
    expect(closed.attempt?.status).toBe(attemptStatus);
    expect(closed.attempt?.running).toBe(attemptStatus === "running");
    expect(closed.blockedBy).toEqual(issue.blockedBy);
    expect(isLive(closed)).toBe(false);
    expect(isBlocked(closed)).toBe(false);
    if (showProject) {
      expect(screen.getByTestId("project-dot").getAttribute("style")).toBeNull();
      expect(screen.getByText("parable")).not.toBeNull();
    } else {
      expect(card.querySelector(".grayscale")).not.toBeNull();
      expect(screen.getByText("parable").className).toBe("sr-only");
    }
    const open = screen.getByTestId("issue-card-open");
    expect(open.className).toContain("focus-visible:ring-2");
    open.focus();
    expect(document.activeElement).toBe(open);
    fireEvent.click(open);
    expect(onOpen).toHaveBeenCalledWith(closed);
    expect(screen.getByTestId("lane-menu-trigger")).not.toBeNull();
  });

  it.each([
    { attemptStatus: "running", priority: "Urgent", blockedBy: [], label: "Running", tone: "text-success-foreground" },
    { attemptStatus: "failed", priority: "High", blockedBy: ["wi_blocker"], label: "Blocked · 1", tone: "text-error-foreground" },
    { attemptStatus: "failed", priority: "Urgent", blockedBy: [], label: "Attempt failed", tone: "text-error-foreground" },
    { attemptStatus: "interrupted", priority: "High", blockedBy: [], label: "Interrupted", tone: "text-warning-foreground" },
  ] as const)("keeps nonterminal $label treatment", ({ attemptStatus, priority, blockedBy, label, tone }) => {
    const attempt = toAttemptView([{ ...ATTEMPTS[0]!, status: attemptStatus }]);
    render(
      <IssueCard item={item({ terminal: false, attempt, priority, blockedBy })}
        showProject now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />,
    );
    expect(screen.getByText(label).className).toContain(tone);
    expect(screen.getByLabelText(`Priority: ${priority}`).className).toContain(
      priority === "Urgent" ? "text-error-foreground" : "text-warning-foreground",
    );
    expect(screen.getByTestId("project-dot").getAttribute("style")).toContain("oklch");
    expect(screen.queryByTestId("worker-strip") !== null).toBe(attemptStatus === "running");
    expect(screen.getByTestId("issue-card").className.includes("border-success")).toBe(attemptStatus === "running");
  });

  it.each([
    { state: "Done", terminal: true, sourceDates: {}, created: "unavailable", updated: "unavailable" },
    { state: "Todo", terminal: false, sourceDates: { created_at: "bad date", updated_at: "malformed" }, created: "unavailable", updated: "unavailable" },
    { state: "Done", terminal: true, sourceDates: { created_at: "2026-08-12T12:00:00Z", updated_at: "2026-08-26T12:00:00Z" }, created: "4w ago", updated: "2w ago" },
    { state: "Todo", terminal: false, sourceDates: { created_at: "2026-08-12T12:00:00Z", updated_at: "2026-08-26T12:00:00Z" }, created: "4w ago", updated: "2w ago" },
  ])("distinguishes imported $state source dates ($created, $updated) from native updates", ({ state, terminal, sourceDates, created, updated }) => {
    const native = {
      ...itemFixture as unknown as NativeIssue,
      state, terminal,
      created_at: "2026-09-09T08:00:00Z",
      updated_at: "2026-09-09T10:00:00Z",
      provenance: { provider: "github", external_id: "91", author_id: "source_author", observed_at: "2026-09-09T08:00:00Z", ...sourceDates },
    };
    const imported = toWorkItemView(native, "parable");
    expect(imported.createdAt).toBe(native.created_at);
    expect(imported.updatedAt).toBe(native.updated_at);
    expect(imported.source).toEqual({ externalId: "91", createdAt: sourceDates.created_at ?? null, updatedAt: sourceDates.updated_at ?? null, observedAt: native.provenance.observed_at });
    render(
      <>
        <IssueCard item={imported} showProject now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />
        <IssueCard item={item({ id: "native", state: "Done", terminal: true })} showProject
          now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />
      </>,
    );
    const badge = screen.getByText(terminal ? "Source history" : "Imported");
    expect(badge.getAttribute("title")).toContain("Imported from github · 91");
    expect(badge.getAttribute("title")).toContain(`Original source created: ${created}`);
    expect(badge.getAttribute("title")).toContain(`Original source updated: ${updated}`);
    expect(badge.getAttribute("title")).toContain("Source observed: 4h ago");
    const ages = screen.getAllByTestId("issue-age");
    expect(ages[0]!.textContent).toBe("Native update 2h");
    expect(ages[0]!.getAttribute("title")).toContain("Native update: 2h ago (2026-09-09T10:00:00Z)");
    expect(ages[0]!.getAttribute("title")).toContain(`Original source updated: ${updated}`);
    expect(ages[1]!.textContent).toMatch(/^Updated \d/);
    expect(ages[1]!.getAttribute("title")).not.toContain("source");
    expect(screen.queryByText(/shipped|landed/i)).toBeNull();
  });

  it("draws no progress bar, because the hub serves no progress", () => {
    const running = item({ attempt: toAttemptView(ATTEMPTS) });
    expect(running.attempt?.progress).toBeNull();
    const { container } = render(
      <IssueCard
        item={running}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        moves={[]}
        onMove={vi.fn()}
      />,
    );
    expect(container.querySelector('[role="progressbar"]')).toBeNull();
  });

  it("says how many dependencies block it", () => {
    render(
      <IssueCard item={item()} showProject={false} now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />,
    );
    expect(screen.getByText("Blocked · 1")).not.toBeNull();
  });

  it.each(["board", "list"] as const)("links only the PR and preserves it through %s updates", (mode) => {
    const onOpen = vi.fn();
    const onParentClick = vi.fn();
    const surface = (value: WorkItemView) => <div onClick={onParentClick}>
      {mode === "board"
        ? <IssueCard item={value} showProject={false} now={NOW} onOpen={onOpen} moves={[]} onMove={vi.fn()} />
        : <WorkList items={[value]} showProject={false} now={NOW} onOpen={onOpen} movesFor={() => []} onMove={vi.fn()} />}
    </div>;
    const { rerender } = render(surface(item()));
    expect(screen.queryByTestId("pr-chip")).toBeNull();
    const linked = toWorkItemView({ ...itemFixture as unknown as NativeIssue,
      pull_request: { number: 3372, url: "https://github.com/digitaldrywood/detent/pull/3372" } }, "parable");
    rerender(surface(linked));
    const link = screen.getByRole("link", { name: "PR #3372" });
    expect(link.getAttribute("href")).toBe("https://github.com/digitaldrywood/detent/pull/3372");
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    fireEvent.click(link);
    expect(onOpen).not.toHaveBeenCalled();
    expect(onParentClick).not.toHaveBeenCalled();
    for (const state of ["In Review", "Merging", "Done"]) {
      rerender(surface({ ...linked, state, revision: state, terminal: state === "Done", change: null }));
      expect(screen.getByRole("link", { name: "PR #3372" })).toBe(link);
    }
    fireEvent.click(screen.getByTestId(mode === "board" ? "issue-card-open" : "work-list-open"));
    expect(onOpen).toHaveBeenCalledTimes(1);
    for (const change of [null, { id: "change_1", number: null, title: "Native change", state: "reviewed", url: null, review: "ready" },
      { id: "change_1", number: 3372, title: "Unlinked change", state: "reviewed", url: null, review: "ready" }]) {
      rerender(surface(item({ change })));
      expect(screen.queryByTestId("pr-chip")).toBeNull();
      expect(screen.queryByText("Change")).toBeNull();
    }
  });

  it("opens the issue from the title", () => {
    const onOpen = vi.fn();
    render(
      <IssueCard item={item()} showProject={false} now={NOW} onOpen={onOpen} moves={[]} onMove={vi.fn()} />,
    );
    fireEvent.click(screen.getByTestId("issue-card-open"));
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it("offers a keyboard move for every lane the workflow allows, and no others", async () => {
    const onMove = vi.fn();
    const moves = transitionsFrom(PROJECT, "In Progress");
    render(
      <IssueCard
        item={item({ state: "In Progress" })}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        moves={moves}
        onMove={onMove}
      />,
    );
    // Base UI menus are driven with synchronous events: `findBy` polling
    // against the positioner's continuous updates takes seconds per query.
    fireEvent.click(screen.getByTestId("lane-menu-trigger"));
    expect(screen.getByRole("menu", { name: "Move to" })).not.toBeNull();
    expect(screen.getByTestId("lane-menu-item-In Review")).not.toBeNull();
    expect(screen.queryByTestId("lane-menu-item-Done")).toBeNull();
    fireEvent.click(screen.getByTestId("lane-menu-item-In Review"));
    expect(onMove).toHaveBeenCalledWith(expect.objectContaining({ id: item().id }), "In Review");
  });

  // Escape and focus return are asserted in a real browser
  // (tests/visual/work.spec.js): while a Base UI menu is open, jsdom spends
  // seconds per frame on its positioner, so a wait inside it times out.

  it("has no move menu at all when the workflow allows none", () => {
    render(
      <IssueCard item={item()} showProject={false} now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />,
    );
    expect(screen.queryByTestId("lane-menu-trigger")).toBeNull();
  });
});

describe("a lane", () => {
  function renderLane(overrides: Partial<React.ComponentProps<typeof BoardLane>> = {}) {
    const onDrop = vi.fn();
    render(
      <BoardLane
        lane={LANES.find((lane) => lane.name === "Todo")!}
        collapsed={false}
        onToggleCollapsed={vi.fn()}
        items={[item({ state: "Todo" })]}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        movesFor={() => []}
        onMove={vi.fn()}
        movingIds={new Set()}
        draggingId={null}
        onDragStart={vi.fn()}
        onDragEnd={vi.fn()}
        onDrop={onDrop}
        {...overrides}
      />,
    );
    return { onDrop };
  }

  it("names itself and counts what is in it", () => {
    renderLane();
    expect(screen.getByTestId("lane-count-Todo").textContent).toBe("1");
    expect(screen.getByRole("region", { name: "Todo" })).not.toBeNull();
  });

  it("says so when it is empty", () => {
    renderLane({ items: [], emptyLabel: "Nothing is merging." });
    expect(screen.getByText("Nothing is merging.")).not.toBeNull();
  });

  it("collapses to a counted rail with a labelled toggle and no card body or creation control", () => {
    const onToggleCollapsed = vi.fn();
    renderLane({ collapsed: true, onToggleCollapsed, total: 130, onCreate: vi.fn(),
      lane: { id: "Blocked", name: "Blocked", terminal: false, dispatchable: false, category: "started" } });
    const toggle = screen.getByRole("button", { name: "Expand Blocked" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(onToggleCollapsed).toHaveBeenCalledOnce();
    expect(screen.getByTestId("board-lane").className).toContain("w-11");
    expect(screen.getByTestId("lane-count-Blocked").textContent).toBe("130");
    expect(screen.getByTestId("lane-count-Blocked").className).toContain("bg-error-surface");
    expect(screen.queryByTestId("lane-body-Blocked")).toBeNull();
    expect(screen.queryByTestId("lane-new-issue-Blocked")).toBeNull();
    expect(screen.queryByTestId("issue-card")).toBeNull();
  });

  it.each([false, true])("accepts a drop only when the card may land here (collapsed=%s)", (collapsed) => {
    const { onDrop } = renderLane({ collapsed });
    const body = screen.getByTestId("board-lane");
    fireEvent.dragOver(body);
    expect(body.className).toContain("outline-dashed");
    fireEvent.drop(body);
    expect(onDrop).toHaveBeenCalledTimes(1);
    expect(body.className).not.toContain("outline-dashed");

    cleanup();
    renderLane({ onDrop: null, collapsed });
    const refusing = screen.getByTestId("board-lane");
    fireEvent.dragOver(refusing);
    fireEvent.drop(refusing);
    expect(refusing.className).not.toContain("outline-dashed");
    expect(onDrop).toHaveBeenCalledTimes(1);
  });

  it.each([true, false])("uses configured terminal=%s before counting and styling live work", (terminal) => {
    const running = item({ state: "Retired", terminal: false, attempt: toAttemptView(ATTEMPTS) });
    renderLane({
      lane: { id: "Retired", name: "Retired", terminal, dispatchable: false, category: terminal ? "completed" : "started" },
      items: [running],
    });
    const lane = screen.getByTestId("board-lane");
    expect(lane.getAttribute("data-terminal")).toBe(terminal ? "true" : null);
    expect(lane.className).not.toContain("opacity-");
    expect(screen.queryByText("1 live") !== null).toBe(!terminal);
    expect(screen.queryByText("Running") !== null).toBe(!terminal);
    expect(screen.queryByTestId("worker-strip") !== null).toBe(!terminal);
    expect(screen.getByTestId("issue-card").getAttribute("data-terminal")).toBe(terminal ? "true" : null);
    if (terminal) expect(within(screen.getByTestId("issue-card")).getByText("Retired").className).toContain("text-muted-foreground");
    expect(running.attempt?.running).toBe(true);
  });

});

describe("the list view", () => {
  it("renders one row per issue with the same signals as the card", () => {
    render(
      <WorkList
        items={[item(), item({ id: "wi_2", title: "Second", state: "Todo", blockedBy: [],
          attempt: { ...toAttemptView(ATTEMPTS)!, runner: "Build Mac" } })]}
        showProject
        now={NOW}
        onOpen={vi.fn()}
        movesFor={() => []}
        onMove={vi.fn()}
      />,
    );
    const rows = screen.getAllByTestId("work-list-row");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText("Blocked · 1")).not.toBeNull();
    expect(within(rows[0]!).getByText("High")).not.toBeNull();
    expect(within(rows[1]!).getByText("Todo")).not.toBeNull();
    expect(within(rows[1]!).getByText("Build Mac")).not.toBeNull();
  });

  it("retains neutral terminal status and history in the shared list presentation", () => {
    render(
      <WorkList items={[item({ state: "Cancelled", terminal: true, attempt: toAttemptView(ATTEMPTS) })]}
        showProject now={NOW} onOpen={vi.fn()} movesFor={() => []} onMove={vi.fn()} />,
    );
    const row = screen.getByTestId("work-list-row");
    expect(row.innerHTML).not.toMatch(/bg-success|animate-status-pulse|bg-warning|text-warning/);
    expect(screen.getByLabelText("Cancelled. Last attempt: running").className).toContain("bg-muted");
    expect(screen.getByLabelText("Priority: High").className).toContain("text-muted-foreground");
  });

  it("renders terminal-entry age and the landed change in the Closed list", () => {
    const item = { ...toWorkItemView(itemFixture as unknown as NativeIssue, "alpha"), state: "Cancelled", terminal: true, updatedAt: new Date(NOW).toISOString(),
      closedAt: new Date(NOW - 2 * 3_600_000).toISOString(), listChange: { branch: "develop", headSha: "abcdef1234567890" } };
    render(<WorkList items={[item]} tab="closed" showProject={false} now={NOW} onOpen={() => {}} movesFor={() => []} onMove={() => {}} />);
    expect(screen.getByRole("columnheader", { name: "Closed" })).not.toBeNull();
    expect(screen.queryByRole("columnheader", { name: "Status" })).toBeNull();
    expect(screen.getByText("2h ago")).not.toBeNull();
    expect(screen.getByText("develop · abcdef1")).not.toBeNull();
    expect(screen.getByText("Cancelled")).not.toBeNull();
  });

  it("navigates issue titles with the keyboard and opens the focused issue", () => {
    const onOpen = vi.fn();
    const items = [item(), item({ id: "wi_2", title: "Second" }), item({ id: "wi_3", title: "Third" })];
    render(
      <WorkList
        items={items}
        showProject={false}
        now={NOW}
        onOpen={onOpen}
        movesFor={() => []}
        onMove={vi.fn()}
      />,
    );
    const buttons = screen.getAllByTestId("work-list-open");
    buttons[0]!.focus();
    for (const [key, index] of [["ArrowDown", 1], ["End", 2], ["ArrowDown", 2], ["ArrowUp", 1], ["Home", 0], ["ArrowUp", 0]] as const) {
      fireEvent.keyDown(document.activeElement!, { key });
      expect(document.activeElement).toBe(buttons[index]);
    }
    fireEvent.keyDown(buttons[0]!, { key: "End", ctrlKey: true });
    expect(document.activeElement).toBe(buttons[0]);
    fireEvent.click(buttons[0]!);
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(onOpen).toHaveBeenCalledWith(items[0]);
  });

  it("says when nothing matches", () => {
    render(
      <WorkList
        items={[]}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        movesFor={() => []}
        onMove={vi.fn()}
      />,
    );
    expect(screen.getByTestId("work-list-empty")).not.toBeNull();
  });
});

it("preserves meta, actions and Reload order for non-board top bars", () => {
  const reload = vi.fn();
  render(<WorkTopBar context="Work" title="Changes" meta="24 changes"
    connection={{ tone: "dc-warn", label: "Reconnecting", detail: "data as of 1:04 PM", tooltip: "Trying again", action: { label: "Reload", onClick: reload } }}
    actions={<button>Review changes</button>} />);
  expect(screen.getByText("24 changes")).not.toBeNull();
  const action = screen.getByRole("button", { name: "Review changes" });
  const chip = screen.getByTestId("connection-chip");
  const reloadButton = screen.getByRole("button", { name: "Reload" });
  expect(action.compareDocumentPosition(chip) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(chip.compareDocumentPosition(reloadButton) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  fireEvent.click(reloadButton);
  expect(reload).toHaveBeenCalledOnce();
});

describe("completed counts", () => {
  it("partitions loaded work by dispatchable lanes and includes custom terminal states", () => {
    const items = [
      item({ id: "a", state: "Todo", blockedBy: [] }),
      item({ id: "b", state: "In Progress", blockedBy: [], attempt: toAttemptView(ATTEMPTS) }),
      item({ id: "c", state: "Blocked" }),
      item({ id: "d", state: "Done", blockedBy: [] }),
      item({ id: "e", state: "Done", sourceProvider: "github" }),
      item({ id: "f", state: "Retired", sourceProvider: "github" }),
      item({ id: "g", state: "Cancelled" }),
      item({ id: "h", state: "Todo", sourceProvider: "github", blockedBy: [] }),
      item({ id: "i", state: "Backlog", blockedBy: [] }),
      item({ id: "j", state: "Todo", blockedBy: ["dependency"] }),
      item({ id: "k", state: "Repair", blockedBy: [] }),
      item({ id: "l", state: "Repair", blockedBy: [], attempt: toAttemptView(ATTEMPTS) }),
      item({ id: "m", state: "Holding", attempt: null }),
      item({ id: "n", state: "Merging", attempt: null }),
    ];
    const lanes = [...LANES.map((lane) => lane.name === "Merging" ? { ...lane, dispatchable: true } : lane),
      { id: "Repair", name: "Repair", terminal: false, dispatchable: true, category: "started" },
      { id: "Holding", name: "Holding", terminal: false, dispatchable: false, category: "unstarted" },
      { id: "Retired", name: "Retired", terminal: true, dispatchable: false, category: "completed" },
      { id: "Cancelled", name: "Cancelled", terminal: true, dispatchable: false, category: "cancelled" }];
    const stats = boardStats(items, lanes);
    expect(stats.completed).toBe(4);
    expect(stats.importedClosed).toBe(2);
    render(<CompletedCounter count={stats.completed} completedWindow="all" onCompletedWindowChange={vi.fn()} loading={false} />);
    expect(stats.running + stats.waiting + stats.needAttention + stats.backlog).toBe(stats.open);
    expect(stats.open).toBe(10);
    expect(screen.getByTestId("stat-completed").textContent).toBe("4 completed · All time");
    expect(screen.getByTestId("stat-completed").getAttribute("title")).toContain("including cancelled and custom terminal states");
    expect(screen.queryByRole("button", { name: /^Load / })).toBeNull();
  });

  it("partitions server lane totals with custom holding lanes and matches the local fallback", async () => {
    const fixture = workPaginationFixture();
    const lanes = [
      { state: "Backlog", total: 10, running: 0 },
      { state: "Todo", total: 6, running: 2 },
      { state: "In Progress", total: 4, running: 1 },
      { state: "Rework", total: 3, running: 1 },
      { state: "Merging", total: 2, running: 0 },
      { state: "Triage", total: 2, running: 0 },
      { state: "Blocked", total: 1, running: 0 },
      { state: "Human Review", total: 2, running: 0 },
      { state: "Holding", total: 4, running: 0 },
      { state: "Done", total: 7, running: 1 },
      { state: "Cancelled", total: 2, running: 0 },
    ];
    const states = lanes.map((lane) => ({
      name: lane.state,
      terminal: ["Done", "Cancelled"].includes(lane.state),
      dispatchable: ["Todo", "In Progress", "Rework", "Merging"].includes(lane.state),
      transitions: [],
    }));
    vi.stubGlobal("fetch", async (...args: Parameters<typeof fixture.fetch>) => {
      const response = await fixture.fetch(...args);
      const url = new URL(args[0], "http://fixture.test");
      if (url.pathname.endsWith("/projects/proj_alpha")) {
        return Response.json({ ...await response.json(), states });
      }
      if (url.pathname.endsWith("/work-items")) {
        return Response.json({ ...await response.json(), work: {
          lanes, completed: 9, total: 43, as_of: "2026-10-07T12:00:00Z", truncated: false, items: [],
        } });
      }
      return response;
    });
    let current!: ReturnType<typeof useBoard>;
    function Probe() { current = useBoard("proj_alpha", { ...DEFAULT_VIEW_STATE, completedWindow: "all" }); return <div />; }
    render(<ClientContext.Provider value={fixture.client}><Probe /></ClientContext.Provider>);
    await waitFor(() => expect(current.resolved).toBe(true));
    expect(current.error).toBeNull();
    expect(current.totals).toMatchObject({ running: 4, waiting: 11, needAttention: 9, backlog: 10, open: 34, completed: 9, total: 43 });
    const localItems = lanes.flatMap((lane) => Array.from({ length: lane.total }, (_, index) => item({
      id: `${lane.state}-${index}`, state: lane.state,
      terminal: ["Done", "Cancelled"].includes(lane.state),
      attempt: index < lane.running ? toAttemptView(ATTEMPTS) : null,
    })));
    const fallback = boardStats(localItems, current.lanes);
    for (const key of ["running", "waiting", "needAttention", "backlog", "open", "completed", "total"] as const) {
      expect(fallback[key]).toBe(current.totals![key]);
    }
    expect(fallback.running + fallback.waiting + fallback.needAttention + fallback.backlog).toBe(34);
  });

  it.each([{ loadedCount: 100, total: 143 }, { loadedCount: 108, total: 569 }])("keeps scoped counts while refreshing without global pagination ($loadedCount of $total)", ({ total }) => {
    const totals = { lanes: {}, running: 2, waiting: 3, needAttention: 5, backlog: 7, open: 17, completed: total - 17, total, asOf: "2026-10-02T17:17:00Z", truncated: true };
    const props = { count: totals.completed, loading: false, onCompletedWindowChange: vi.fn() };
    const mounted = render(<CompletedCounter {...props} />);
    const counts = screen.getByTestId("stat-completed");
    expect(counts.textContent).toContain(`${totals.completed} completed · 48h`);
    expect(within(counts).queryByRole("button", { name: /^Load / })).toBeNull();
    mounted.rerender(<CompletedCounter {...props} loading />);
    expect(counts.closest("[aria-busy]")?.getAttribute("aria-busy")).toBe("true");
    expect(counts.textContent).toContain(`${totals.completed} completed · 48h`);
    mounted.rerender(<CompletedCounter {...props} />);
    expect(screen.queryByRole("button", { name: /^Load / })).toBeNull();
  });
});

describe("the Diff surface", () => {
  function renderSurface(source: DiffSource = { kind: "version" }) {
    const onReload = vi.fn();
    render(
      <DiffSurface
        change={CHANGE}
        source={source}
        attempts={ATTEMPTS}
        history={HISTORY}
        now={NOW}
        onReload={onReload}
      />,
    );
    return { onReload };
  }

  it("names the round and the head it is anchored to", () => {
    renderSurface();
    expect(screen.getByTestId("diff-round").textContent).toContain("Round 2");
    expect(screen.getByTestId("diff-round").textContent).toContain("a41f0c2");
  });

  // §18.5. With no stored attempt diff the surface falls back to the round the
  // change request published, and says which of the two it is showing — the
  // hub does serve per-file diffs now, so the old "no file list" apology would
  // be untrue.
  it("falls back to the round when no attempt has posted a diff", () => {
    renderSurface();
    expect(screen.getByTestId("diff-empty").textContent).toContain(
      "No attempt has posted a diff for this issue yet",
    );
    expect(screen.queryByTestId("diff-attempt")).toBeNull();
    // What it does have is shown instead of nothing.
    expect(screen.getByText("digitaldrywood/detent")).not.toBeNull();
    expect(screen.getByTestId("diff-pr-link").textContent).toContain("3372");
  });

  it("draws the stored attempt diff, file list and all", () => {
    renderSurface({ kind: "attempt", diff: ATTEMPT_DIFF });
    const files = screen.getByTestId("diff-files");
    expect(files.textContent).toContain("3 changed files");
    expect(within(files).getByText("main.go")).not.toBeNull();
    expect(within(files).getByText(".env.local")).not.toBeNull();
    // The round card is the fallback, so it is not drawn over a real diff.
    expect(screen.queryByTestId("diff-round-card")).toBeNull();
    expect(screen.queryByTestId("diff-empty")).toBeNull();
    expect(screen.getAllByTestId("diff-file-section")).toHaveLength(3);
    expect(screen.getByTestId("diff-file-denied").textContent).toContain("denylist");
    // The head in the sub-header is the diff's, not the version's.
    expect(screen.getByTestId("diff-round").textContent).toContain(
      ATTEMPT_DIFF.head_sha.slice(0, 7),
    );
  });

  it("shows the worker's checkpoint", () => {
    renderSurface();
    const card = screen.getByTestId("diff-state-card");
    expect(card.textContent).toContain("resume_session");
    expect(card.textContent).toContain("git_push");
    expect(card.textContent).toContain("gpt-6-astra");
  });

  it("shows the verdict, the checks and the reviews", () => {
    renderSurface();
    expect(screen.getByTestId("diff-verdict-card").textContent).toContain("blocked");
    expect(screen.getByTestId("diff-verdict-card").textContent).toContain(
      "A reviewer requested changes",
    );
    expect(screen.getAllByTestId("diff-check")).toHaveLength(1);
  });

  it("shows the history newest first", () => {
    renderSurface();
    const rows = screen.getAllByTestId("diff-activity-row");
    expect(rows).toHaveLength(HISTORY.length);
    expect(rows[0]!.textContent).toContain("change.version_published");
  });

  it("re-reads the change on demand", () => {
    const { onReload } = renderSurface();
    fireEvent.click(screen.getByRole("button", { name: "Reload the change" }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });
});

// decisions.md §18.5. Two sources feed one surface, and only one of them has
// files in it. These assert the order directly rather than through a mounted
// panel: which source wins, and what is said when neither is there.
describe("the Diff surface's source order", () => {
  it("prefers a stored attempt diff over the change version", () => {
    expect(
      diffSource({
        attemptDiff: ATTEMPT_DIFF,
        change: CHANGE,
        codeAvailability: "available",
        linkedToIssue: true,
      }),
    ).toEqual({ kind: "attempt", diff: ATTEMPT_DIFF });
  });

  it("falls back to the version card when no attempt has posted a diff", () => {
    expect(
      diffSource({
        attemptDiff: null,
        change: CHANGE,
        codeAvailability: "available",
        linkedToIssue: true,
      }),
    ).toEqual({ kind: "version" });
  });

  it("names the reason when there is neither a diff nor a change", () => {
    const noChange = diffSource({
      attemptDiff: null,
      change: null,
      codeAvailability: null,
      linkedToIssue: true,
    });
    expect(noChange.kind).toBe("unavailable");
    expect(noChange.kind === "unavailable" ? noChange.reason : "").toContain(
      "No attempt has posted a diff",
    );

    const unlinked = diffSource({
      attemptDiff: null,
      change: null,
      codeAvailability: null,
      linkedToIssue: false,
    });
    expect(unlinked.kind === "unavailable" ? unlinked.reason : "").toContain("not linked");
  });

  it("says so when the round's own artifact is gone", () => {
    const gone = diffSource({
      attemptDiff: null,
      change: CHANGE,
      codeAvailability: "expired",
      linkedToIssue: true,
    });
    expect(gone.kind === "unavailable" ? gone.reason : "").toContain("not available");
  });

  // One request, issue-addressed: asking the attempt-addressed read per
  // attempt would take a 404 — and a browser console error — for every attempt
  // that never checkpointed, on every issue open.
  it("reads the issue's diff in one request", async () => {
    const asked: string[] = [];
    const http = {
      getWorkItemDiff: (_project: string, itemId: string) => {
        asked.push(itemId);
        return Promise.resolve({ diff: ATTEMPT_DIFF });
      },
    } as unknown as Parameters<typeof readAttemptDiff>[0];
    await expect(readAttemptDiff(http, "prj_8c1d", "wi_3363")).resolves.toEqual(ATTEMPT_DIFF);
    expect(asked).toEqual(["wi_3363"]);
  });

  it("reports no diff when the hub says the issue has none", async () => {
    const http = {
      getWorkItemDiff: () => Promise.resolve({ diff: null }),
    } as unknown as Parameters<typeof readAttemptDiff>[0];
    await expect(readAttemptDiff(http, "prj_8c1d", "wi_3363")).resolves.toBeNull();
  });

  // A diff whose file list is empty is not a diff a reader can read, so the
  // surface falls back rather than drawing an empty tree.
  it("treats a diff with no files as no diff", async () => {
    const http = {
      getWorkItemDiff: () => Promise.resolve({ diff: { ...ATTEMPT_DIFF, files: [] } }),
    } as unknown as Parameters<typeof readAttemptDiff>[0];
    await expect(readAttemptDiff(http, "prj_8c1d", "wi_3363")).resolves.toBeNull();
  });
});

vi.mock("../../src/app/App.tsx", () => ({
  useShell: () => ({ connection: { tone: "dc-ok", label: "Connected", detail: null, tooltip: "Fixture" } }),
}));

afterEach(() => { vi.unstubAllGlobals(); globalThis.localStorage.clear(); resetRunnerNamesForTests(); });

async function pagedWork(path = "/work", wrapFetch?: (fetch: ReturnType<typeof workPaginationFixture>["fetch"]) => ReturnType<typeof workPaginationFixture>["fetch"]) {
  const fixture = workPaginationFixture();
  await fixture.control();
  vi.stubGlobal("fetch", wrapFetch?.(fixture.fetch) ?? fixture.fetch);
  const root = createRootRoute({ component: () => <SidebarProvider><SidebarSearchProvider work><aside className="dc-side"><Input nativeInput type="search" aria-label="Search threads" /></aside><Outlet /></SidebarSearchProvider></SidebarProvider> });
  const all = createRoute({ getParentRoute: () => root, path: "/work", component: () => <WorkBoard projectId={null} /> });
  const project = createRoute({ getParentRoute: () => root, path: "/work/p/$projectId", component: () => {
    const { projectId } = project.useParams();
    return <WorkBoard projectId={projectId} />;
  } });
  const history = createMemoryHistory({ initialEntries: [path] });
  const router = createRouter({ routeTree: root.addChildren([all, project]), history, ...plainSearchOptions });
  render(<ClientContext.Provider value={fixture.client}><RouterProvider router={router} /></ClientContext.Provider>);
  return { ...fixture, router, history };
}

async function settledWork() {
  await waitFor(() => expect(screen.getByTestId("stat-completed").closest("[aria-busy]")?.getAttribute("aria-busy")).toBe("false"));
}

describe("the archive period hint", () => {
  it.each([
    [30, 7, "Closed issues archive after 30 days, cancelled after 7."],
    [14, null, "Completed issues archive after 14 days."],
    [null, 60, "Cancelled issues archive after 60 days."],
    [null, null, null],
  ] as const)("reads configured periods %s/%s in List view", async (completed, cancelled, hint) => {
    await pagedWork("/work/p/proj_alpha?view=list", (fetch) => async (input, init) => {
      if (new URL(input, "http://fixture.test").pathname.endsWith("/integration")) {
        return Response.json({ profile: "native", revision: "1", intake: "disabled", projection: "disabled", repository_enabled: false,
          archive_completed_after_days: completed, archive_cancelled_after_days: cancelled });
      }
      return fetch(input, init);
    });
    await settledWork();
    if (hint === null) {
      expect(screen.queryByText(/issues archive after/)).toBeNull();
    } else {
      expect(await screen.findByText(hint)).toBeTruthy();
    }
  });
});

describe("the Work clock render boundary", () => {
  it("shares one timer and releases it after the final label unmounts", () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    vi.setSystemTime(NOW);
    const start = vi.spyOn(globalThis, "setInterval");
    const stop = vi.spyOn(globalThis, "clearInterval");
    function Label() {
      return <span>{useNow()}</span>;
    }
    try {
      const first = render(<Label />);
      const second = render(<Label />);
      expect(start).toHaveBeenCalledTimes(1);
      const timer = start.mock.results[0]!.value;
      act(() => vi.advanceTimersByTime(2_000));
      expect(first.container.textContent).toBe(String(NOW + 2_000));
      expect(second.container.textContent).toBe(String(NOW + 2_000));
      first.unmount();
      expect(stop).not.toHaveBeenCalled();
      second.unmount();
      expect(stop).toHaveBeenCalledOnce();
      expect(stop).toHaveBeenCalledWith(timer);
      const next = render(<Label />);
      expect(start).toHaveBeenCalledTimes(2);
      next.unmount();
      expect(stop).toHaveBeenCalledTimes(2);
    } finally {
      cleanup();
      start.mockRestore();
      stop.mockRestore();
      vi.useRealTimers();
    }
  });

  it.each(["board", "list"])("ticks only time labels in %s view", async (view) => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    vi.setSystemTime(NOW);
    try {
      await pagedWork(`/work?view=${view}`);
      await settledWork();
      const trigger = screen.getByRole("button", { name: "Filters" });
      trigger.focus();
      fireEvent.click(trigger);
      const popup = await screen.findByRole("menu");
      await act(async () => { vi.advanceTimersByTime(100); });
      expect(popup.contains(document.activeElement)).toBe(true);
      const focus = document.activeElement;
      const before = { ...clockRenders };
      const cards = view === "board" ? screen.getAllByTestId("issue-card") : screen.getAllByTestId("work-list-row");
      const first = cards[0]!;
      const text = first.textContent;
      const reads = document.querySelectorAll("[data-work-item]").length;
      await act(async () => { vi.advanceTimersByTime(2_000); });
      expect(clockRenders).toEqual(before);
      expect(document.activeElement).toBe(focus);
      expect(screen.getByRole("menu")).toBe(popup);
      expect(document.querySelectorAll("[data-work-item]")).toHaveLength(reads);
      expect((view === "board" ? screen.getAllByTestId("issue-card") : screen.getAllByTestId("work-list-row"))[0]).toBe(first);
      expect(first.textContent).not.toBe(text);
    } finally {
      cleanup();
      vi.useRealTimers();
    }
  });
});

describe("the cached Work read", () => {
  it("shows a cold spinner, then core cards while optional details remain pending", async () => {
    let releaseBase!: () => void;
    let releaseDetails!: () => void;
    const base = new Promise<void>((resolve) => { releaseBase = resolve; });
    const details = new Promise<void>((resolve) => { releaseDetails = resolve; });
    await pagedWork("/work", (fetch) => async (input, init) => {
      if (String(input).includes("/work-items?") || /\/projects\/[^/]+$/.test(String(input))) await base;
      if (/\/(attempts|changes)(\?|$)/.test(String(input))) await details;
      return fetch(input, init);
    });
    const spinner = await screen.findByText("Loading work…");
    expect(spinner.getAttribute("role")).toBe("status");
    expect(spinner.parentElement?.getAttribute("aria-busy")).toBe("true");
    expect(screen.queryByTestId("work-stats")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Lanes/ })).toBeNull();
    expect(screen.queryByText(/Every lane is hidden|Nothing matches|Create your first/)).toBeNull();
    await act(async () => { releaseBase(); });
    await settledWork();
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
    expect(screen.queryByText("Loading work…")).toBeNull();
    await act(async () => { releaseDetails(); });
  });

  it("shares concurrent base reads and shows memory cards on the first return render", async () => {
    const fixture = workPaginationFixture();
    await fixture.control();
    let hold = false;
    let release!: () => void;
    const pending = new Promise<void>((resolve) => { release = resolve; });
    vi.stubGlobal("fetch", async (...args: Parameters<typeof fixture.fetch>) => {
      if (hold) await pending;
      return fixture.fetch(...args);
    });
    const snapshots: ReturnType<typeof useBoard>[] = [];
    function Probe() {
      const board = useBoard(null, DEFAULT_VIEW_STATE);
      snapshots.push(board);
      return <div>{board.items.length}</div>;
    }
    const mounted = render(<ClientContext.Provider value={fixture.client}><Probe /><Probe /></ClientContext.Provider>);
    await waitFor(() => expect(snapshots.at(-1)!.resolved).toBe(true));
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))).toHaveLength(3);
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/proj_alpha"))).toHaveLength(1);
    mounted.unmount();
    hold = true;
    const before = snapshots.length;
    render(<ClientContext.Provider value={fixture.client}><Probe /></ClientContext.Provider>);
    expect(snapshots[before]!.items.length).toBeGreaterThan(0);
    expect(snapshots[before]!.loading).toBe(false);
    expect(snapshots[before]!.live).toBe(false);
    expect(snapshots.at(-1)!.items.length).toBeGreaterThan(0);
    await act(async () => { release(); });
  });

  it("keeps the successful read timestamp when stream activity starts a held refresh", async () => {
    const sources: EventTarget[] = [];
    vi.stubGlobal("EventSource", class extends EventTarget {
      readyState = 1;
      constructor() { super(); sources.push(this); }
      close() {}
    });
    const fixture = workPaginationFixture();
    await fixture.control();
    let hold = false;
    let release!: () => void;
    const pending = new Promise<void>((resolve) => { release = resolve; });
    vi.stubGlobal("fetch", async (...args: Parameters<typeof fixture.fetch>) => {
      if (hold) await pending;
      return fixture.fetch(...args);
    });
    let current!: ReturnType<typeof useBoard>;
    function Probe() { current = useBoard(null, DEFAULT_VIEW_STATE); return <div />; }
    render(<ClientContext.Provider value={fixture.client}><Probe /></ClientContext.Provider>);
    await waitFor(() => expect(current.resolved).toBe(true));
    const stamp = current.asOf;
    hold = true;
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "40" })));
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "41" })));
    await waitFor(() => expect(current.refreshing).toBe(true));
    expect(current.asOf).toBe(stamp);
    expect(current.loading).toBe(false);
    await act(async () => { release(); });
  });
});

describe("the live Work continuation intent", () => {
  it.each(["board", "list"] as const)("cancels a %s continuation when the actual filter selection changes", async (mode) => {
    const fixture = workPaginationFixture();
    await fixture.control(mode === "board" ? { backlogOverflow: true } : {});
    vi.stubGlobal("fetch", fixture.fetch);
    let current!: ReturnType<typeof useBoard>;
    let changeView!: React.Dispatch<React.SetStateAction<typeof DEFAULT_VIEW_STATE>>;
    function Probe() {
      const [view, setView] = React.useState<typeof DEFAULT_VIEW_STATE>({ ...DEFAULT_VIEW_STATE, view: mode, tab: "all" });
      changeView = setView;
      current = useBoard(null, view);
      return <div>{current.items.length}</div>;
    }
    render(<ClientContext.Provider value={fixture.client}><Probe /></ClientContext.Provider>);
    await waitFor(() => expect(current.loading).toBe(false));
    expect(current.totals).toMatchObject(mode === "board"
      ? { running: 1, waiting: 8, needAttention: 1, backlog: 405, open: 415, completed: 131, total: 546 }
      : { running: 1, waiting: 8, needAttention: 1, backlog: 0, open: 10, completed: 131, total: 141 });
    const deferred = fixture.deferPage();
    act(() => mode === "board" ? current.loadBacklog() : current.loadMore());
    await deferred.waiting;
    const continuation = fixture.requests.findLast((request) => request.url.searchParams.has("cursor"))!;
    const label = mode === "board" ? "overflow" : "older-label";
    const count = mode === "board" ? 200 : 2;
    act(() => changeView({ ...DEFAULT_VIEW_STATE, view: mode, tab: "all", q: label }));
    await waitFor(() => expect(current.items).toHaveLength(count));
    expect(continuation.signal?.aborted).toBe(true);
    expect(current.items.every((item) => item.labels.includes(label))).toBe(true);
    await act(async () => { deferred.release(); });
    expect(current.items).toHaveLength(count);
  });


  it("accepts Load more during background refresh and clears revoked scope", async () => {
    const sources: EventTarget[] = [];
    vi.stubGlobal("EventSource", class extends EventTarget {
      readyState = 1;
      constructor() { super(); sources.push(this); }
      close() {}
    });
    const fixture = workPaginationFixture();
    await fixture.control();
    let hold = false;
    let started!: () => void;
    let release!: () => void;
    const waiting = new Promise<void>((resolve) => { started = resolve; });
    const deferred = new Promise<void>((resolve) => { release = resolve; });
    vi.stubGlobal("fetch", async (...args: Parameters<typeof fixture.fetch>) => {
      if (hold && String(args[0]).includes("/proj_alpha/work-items")) {
        hold = false;
        started();
        await deferred;
      }
      return fixture.fetch(...args);
    });
    let current!: ReturnType<typeof useBoard>;
    function Probe() {
      current = useBoard(null, { ...DEFAULT_VIEW_STATE, view: "list", tab: "all" });
      return <div>{current.items.length}</div>;
    }
    render(<ClientContext.Provider value={fixture.client}><Probe /></ClientContext.Provider>);
    await waitFor(() => expect(current.loading).toBe(false));
    hold = true;
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "40" })));
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "41" })));
    await waiting;
    expect(current.loading).toBe(false);
    expect(current.items).toHaveLength(104);
    act(() => current.loadMore());
    await waitFor(() => expect(current.items).toHaveLength(141));
    await act(async () => { release(); });
    await fixture.control({ revoked: true });
    act(() => current.reload());
    await waitFor(() => expect(current.error).not.toBeNull());
    expect(current.items).toHaveLength(0);
    expect(current.totals).toBeNull();
  });


  it.each(["refresh-first", "continuation-first"])("keeps a continuation requested in the same batch as %s activity", async (order) => {
    const sources: EventTarget[] = [];
    vi.stubGlobal("EventSource", class extends EventTarget {
      readyState = 1;
      constructor() { super(); sources.push(this); }
      close() {}
    });
    const fixture = workPaginationFixture();
    await fixture.control();
    vi.stubGlobal("fetch", fixture.fetch);
    let current!: ReturnType<typeof useBoard>;
    function Probe() {
      current = useBoard(null, { ...DEFAULT_VIEW_STATE, view: "list", tab: "all" });
      return <div>{current.items.length}</div>;
    }
    render(<ClientContext.Provider value={fixture.client}><Probe /></ClientContext.Provider>);
    await waitFor(() => expect(current.loading).toBe(false));
    expect(current.items.length).toBeGreaterThan(100);
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "40" })));
    vi.useFakeTimers();
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "41" })));
    act(() => {
      if (order === "refresh-first") vi.advanceTimersByTime(400);
      current.loadMore();
      if (order === "continuation-first") vi.advanceTimersByTime(400);
    });
    vi.useRealTimers();
    await waitFor(() => expect(current.items).toHaveLength(141));
    expect(fixture.requests.some((request) => request.url.searchParams.has("cursor"))).toBe(true);
  });
});

describe("the filter-first Work surface", () => {
  it.each(["4", "42", "true", "false", "null", "4.0", "1e3", "9007199254740993", '"4"', '[4]', '{"number":4}'])("preserves the literal search %s through URL edits", async (q) => {
    const fixture = await pagedWork();
    await settledWork();
    const search = screen.getByRole("searchbox", { name: "Search issues" });
    for (const value of [q, q.slice(0, -1), q]) {
      fireEvent.change(search, { target: { value } });
      await waitFor(() => expect((search as HTMLInputElement).value).toBe(value));
      await settledWork();
      expect(new URLSearchParams(fixture.router.state.location.searchStr).get("q") ?? "").toBe(value);
      expect(fixture.requests.findLast((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))?.url.searchParams.get("q") ?? "").toBe(value);
    }
  });

  it.each(["linked", "restored"])("preserves every %s view parameter when editing a numeric query", async (source) => {
    const query = "archived=true&view=list&tab=all&q=4&state=42&label=true&assignee=null&priority=4&sort=title&lanes=false&collapsed=0&completed=7d";
    const fixture = await pagedWork(source === "linked" ? `/work?${query}` : "/work", (fetch) => async (input, init) => {
      if (source === "restored" && String(input).endsWith("/work-view-preference") && (init?.method ?? "GET") === "GET") {
        return Response.json({ query });
      }
      return fetch(input, init);
    });
    const search = await screen.findByRole("searchbox", { name: "Search issues" });
    await waitFor(() => expect((search as HTMLInputElement).value).toBe("4"));
    await settledWork();
    expect(new URLSearchParams(fixture.router.state.location.searchStr).get("q")).toBe("4");
    fireEvent.change(search, { target: { value: "42" } });
    await settledWork();
    expect(parseViewState(fixture.router.state.location.searchStr)).toEqual({
      archived: true, view: "list", tab: "all", q: "42", state: ["42"], label: ["true"],
      assignee: ["null"], priority: ["4"], sort: "title", lanes: ["false"], collapsed: ["0"], completedWindow: "7d",
    });
  });

  it("opens the completed window menu and requests a fresh count for the selection", async () => {
    const { requests, router } = await pagedWork();
    await settledWork();
    expect(within(screen.getByTestId("work-toolbar")).queryByText(/Completed/)).toBeNull();
    expect(screen.getByTestId("stat-completed").tagName).toBe("BUTTON");
    fireEvent.click(screen.getByTestId("stat-completed"));
    fireEvent.click(await screen.findByRole("menuitemradio", { name: /^7 days$/ }));
    await settledWork();
    expect(parseViewState(router.state.location.searchStr).completedWindow).toBe("7d");
    expect(requests.filter((request) => request.url.pathname.endsWith("/work-items")).some((request) => request.url.searchParams.get("completed_window") === "7d")).toBe(true);
    expect(screen.getByTestId("stat-completed").textContent).toContain("completed · 7d");
  });

  it.each(["Todo", ""])("ignores the %s board lane scope in the Active list", async (lanes) => {
    await pagedWork(`/work?lanes=${lanes}`);
    await settledWork();
    const boardIds = screen.queryAllByTestId("issue-card").map((card) => card.getAttribute("data-work-item"));
    if (lanes === "") expect(boardIds).toHaveLength(0);
    else expect(boardIds.length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole("radio", { name: "List" }));
    await settledWork();
    const listIds = screen.queryAllByTestId("work-list-row").map((row) => row.getAttribute("data-work-item"));
    expect(listIds).toHaveLength(10);
    expect(screen.queryByTestId("lanes-trigger")).toBeNull();
  });

  it("keeps active workers visible with full-scope totals while List starts in Active", async () => {
    const { router, requests } = await pagedWork();
    await settledWork();
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
    expect(screen.getAllByTestId("connection-chip")).toHaveLength(1);
    expect(screen.getByTestId("lane-count-Todo").textContent).toBe("8");
    expect(screen.getByTestId("lane-count-In Progress").textContent).toBe("2");
    expect(screen.queryByTestId("lane-count-Done")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Load more/ })).toBeNull();
    expect(router.state.location.searchStr).toBe("");
    const before = requests.length;
    fireEvent.click(screen.getByRole("radio", { name: "List" }));
    await screen.findByTestId("work-list");
    expect(screen.getAllByTestId("work-list-row")).toHaveLength(10);
    expect(screen.queryByText("Older title needle a")).toBeNull();
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
    expect(requests.length).toBeGreaterThan(before);
    expect(requests.slice(before).filter((request) => request.url.pathname.endsWith("/work-items")).every((request) =>
      ["1", "100"].includes(request.url.searchParams.get("limit")!) && request.url.searchParams.get("include") === "work")).toBe(true);
  });

  it.each([
    { q: "Older title needle", linked: false },
    { q: "alpha#3421", linked: false },
    { q: "3421", linked: false },
    { q: "3421", linked: true },
    { q: "older-label", linked: false },
  ])("finds older $q matches beyond the initial inventory and open selection (linked=$linked)", async ({ q, linked }) => {
    const fixture = await pagedWork(linked ? `/work?view=list&tab=all&q=${q}` : "/work");
    await settledWork();
    if (!linked) {
      fireEvent.click(screen.getByRole("radio", { name: "List" }));
      await screen.findByTestId("work-list");
      expect(screen.queryByText("Older title needle a")).toBeNull();
      fireEvent.click(screen.getByTestId("list-tab-all"));
      await settledWork();
      fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), { target: { value: q } });
      await settledWork();
    }
    expect((screen.getByRole("searchbox", { name: "Search issues" }) as HTMLInputElement).value).toBe(q);
    expect(screen.getByText("Older title needle a")).not.toBeNull();
    expect(screen.queryByText("Observed later-page worker")).toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe(q === "alpha#3421" || q === "3421" ? "1 completed · 48h" : "2 completed · 48h");
    expect(parseViewState(fixture.router.state.location.searchStr).q).toBe(q);
    expect(fixture.requests.findLast((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))?.url.searchParams.get("q")).toBe(q);
    if (q === "Older title needle") {
      fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), { target: { value: "Older title needle b" } });
      await settledWork();
      expect(screen.getByTestId("stat-completed").textContent).toBe("1 completed · 48h");
      expect(screen.getByTestId("stat-completed").getAttribute("title")).toContain("Choose the completed window.");
    }
  });

  it("applies multiple values in all dimensions to items and counts without expanding the selected project", async () => {
    const view = parseViewState("view=list&tab=all&q=Older title needle&state=Todo,Done&label=choice-a,choice-b&assignee=operator-a,operator-b&priority=Urgent,High");
    const fixture = await pagedWork(`/work/p/proj_alpha?${serializeViewState(view)}`);
    await settledWork();
    expect(screen.getByText("Older title needle a")).not.toBeNull();
    expect(screen.getByText("Older title needle b")).not.toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe("2 completed · 48h");
    fireEvent.click(screen.getByTestId("filters-trigger"));
    for (const filter of ["label-choice-b", "assignee-operator-b", "priority-High"]) {
      fireEvent.click(await screen.findByTestId(`filter-${filter}`));
      await settledWork();
      expect(screen.getByTestId("stat-completed").textContent).toBe("1 completed · 48h");
      fireEvent.click(await screen.findByTestId(`filter-${filter}`));
      await settledWork();
      expect(screen.getByTestId("stat-completed").textContent).toBe("2 completed · 48h");
    }
    fireEvent.keyDown(document, { key: "Escape" });
    const reads = fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items"));
    expect(reads.length).toBeGreaterThan(0);
    expect(reads.every((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))).toBe(true);
    const params = reads.at(-1)!.url.searchParams;
    expect(params.getAll("state")).toEqual([]);
    expect(params.getAll("label")).toEqual(["choice-a", "choice-b"]);
    expect(params.getAll("assignee")).toEqual(["operator-a", "operator-b"]);
    expect(params.getAll("priority")).toEqual(["1", "0"]);
    fireEvent.click(screen.getByRole("radio", { name: "Board" }));
    await screen.findByTestId("work-board");
    expect(parseViewState(fixture.router.state.location.searchStr)).toEqual({ ...view, view: "board" });
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items")).length).toBeGreaterThan(reads.length);
  });

  it("keeps a second filter choice available after the first narrows results", async () => {
    const fixture = await pagedWork("/work/p/proj_alpha?view=list&tab=all&q=Older+title+needle");
    await settledWork();
    fireEvent.click(screen.getByTestId("filters-trigger"));
    fireEvent.click(await screen.findByTestId("filter-label-choice-a"));
    await settledWork();
    expect(screen.getByTestId("stat-completed").textContent).toBe("1 completed · 48h");
    fireEvent.click(await screen.findByTestId("filter-label-choice-b"));
    await settledWork();
    expect(screen.getByTestId("stat-completed").textContent).toBe("2 completed · 48h");
    expect(fixture.requests.findLast((request) => request.url.pathname.endsWith("/work-items"))!.url.searchParams.getAll("label"))
      .toEqual(["choice-a", "choice-b"]);
  });

  it("does not expose the previous client's cards or choices during a refused read", async () => {
    const fixture = workPaginationFixture();
    await fixture.control();
    let release!: () => void;
    const waiting = new Promise<void>((resolve) => { release = resolve; });
    let blocked = false;
    vi.stubGlobal("fetch", async (...args: Parameters<typeof fixture.fetch>) => {
      if (blocked) await waiting;
      return fixture.fetch(...args);
    });
    const observed: { items: number; labels: number }[] = [];
    function BoardProbe() {
      const board = useBoard("proj_alpha", DEFAULT_VIEW_STATE);
      observed.push({ items: board.items.length, labels: board.labels.length });
      return <div data-testid="client-read">{board.error ?? `${board.items.length} items`}</div>;
    }
    const mounted = render(<ClientContext.Provider value={fixture.client}><BoardProbe /></ClientContext.Provider>);
    await waitFor(() => expect(observed.at(-1)!.items).toBeGreaterThan(0));
    await fixture.control({ revoked: true });
    blocked = true;
    const before = observed.length;
    const nextClient = { ...fixture.client, bootstrap: { ...fixture.client.bootstrap, actor: { ...fixture.client.bootstrap.actor, principal_id: "other-account" } } };
    mounted.rerender(<ClientContext.Provider value={nextClient}><BoardProbe /></ClientContext.Provider>);
    expect(observed.slice(before).every((value) => value.items === 0 && value.labels === 0)).toBe(true);
    blocked = false;
    await act(async () => { release(); });
    await waitFor(() => expect(screen.getByTestId("client-read").textContent).not.toMatch(/items$/));
    expect(observed.slice(before).every((value) => value.items === 0 && value.labels === 0)).toBe(true);
  });

  it("loads all matching open work beyond the transport limit without replaying other projects", async () => {
    const fixture = await pagedWork("/work/p/proj_alpha?view=list&state=Todo,In+Progress&q=Queue+item");
    await settledWork();
    await fixture.control({ openOverflow: true });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    await settledWork();
    expect(screen.queryByRole("button", { name: /^Load more|Show more/ })).toBeNull();
    expect(fixture.requests.some((request) => request.url.searchParams.has("cursor"))).toBe(true);
    expect(screen.getAllByTestId("work-list-row")).toHaveLength(134);
    expect(screen.getByText("Queue item 1")).not.toBeNull();
    expect(screen.getByText("Queue item 136")).not.toBeNull();
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items")).every((request) =>
      request.url.pathname.endsWith("/proj_alpha/work-items"))).toBe(true);
  });

  it("restores linked filters and discards legacy transport cursors", async () => {
    const fixture = await pagedWork('/work?view=list&tab=all&q=older-label&pages=legacy-cursor');
    await settledWork();
    expect(screen.getByText("Older title needle a")).not.toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe("2 completed · 48h");
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items")).every((request) =>
      !request.url.searchParams.has("cursor"))).toBe(true);
  });

  it.each(["search", "multi-select", "archived"])("aborts stale continuation and resets cursors when %s changes", async (change) => {
    const fixture = await pagedWork("/work?view=list&tab=all");
    await settledWork();
    const deferred = fixture.deferPage();
    fireEvent.click(screen.getByTestId("work-list-more"));
    await deferred.waiting;
    const pending = fixture.requests.findLast((request) => request.url.searchParams.has("cursor"))!;
    if (change === "search") fireEvent.change(screen.getByRole("searchbox", { name: "Search issues" }), { target: { value: "older-label" } });
    else if (change === "archived") fireEvent.click(screen.getByRole("button", { name: "Show archived" }));
    else {
      const view = parseViewState(fixture.router.state.location.searchStr);
      await act(() => fixture.router.navigate({ to: "/work", search: Object.fromEntries(new URLSearchParams(serializeViewState({ ...view, label: ["older-label", "choice-a"] }))) }));
    }
    await settledWork();
    expect(pending.signal?.aborted).toBe(true);
    const newest = fixture.requests.findLast((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))!;
    expect(newest.url.searchParams.has("cursor")).toBe(false);
    await act(async () => { deferred.release(); });
    if (change !== "multi-select") expect(screen.queryByText("Observed later-page worker")).toBeNull();
  });

  it("refuses revoked scope without retaining stale cards or counts", async () => {
    const fixture = await pagedWork("/work?view=list&tab=all");
    await settledWork();
    await fixture.control({ revoked: true });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(await screen.findByTestId("work-error")).not.toBeNull();
    expect(screen.queryByTestId("issue-card")).toBeNull();
    expect(screen.queryByTestId("work-list-row")).toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe("— completed · 48h");
  });

  it("finishes a slow first read and retains usable work during continuous activity", async () => {
    const sources: EventTarget[] = [];
    vi.stubGlobal("EventSource", class extends EventTarget {
      readyState = 1;
      constructor() { super(); sources.push(this); }
      close() {}
    });
    let release!: () => void;
    let started!: () => void;
    let pendingSignal: AbortSignal | undefined;
    let hold = true;
    const waiting = new Promise<void>((resolve) => { started = resolve; });
    const deferred = new Promise<void>((resolve) => { release = resolve; });
    const fixture = await pagedWork("/work?view=list&tab=all", (fetch) => async (input, init) => {
      if (hold && String(input).includes("/proj_alpha/work-items")) {
        hold = false;
        pendingSignal = init?.signal ?? undefined;
        started();
        await deferred;
      }
      return fetch(input, init);
    });
    await waiting;
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "40" })));
    for (const sequence of [41, 42]) {
      act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: String(sequence) })));
      await act(() => new Promise((resolve) => setTimeout(resolve, 450)));
      expect(pendingSignal?.aborted).toBe(false);
    }
    await act(async () => { release(); });
    await settledWork();
    expect(screen.getAllByText("Queue item 1")).toHaveLength(2);
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))).toHaveLength(4);

    const refreshing = fixture.deferPage();
    fireEvent.click(screen.getByTestId("work-list-more"));
    await refreshing.waiting;
    const continuation = fixture.requests.findLast((request) => request.url.searchParams.has("cursor"))!;
    for (const sequence of [43, 44]) {
      act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: String(sequence) })));
      await act(() => new Promise((resolve) => setTimeout(resolve, 450)));
      expect(continuation.signal?.aborted).toBe(false);
      expect(screen.getAllByText("Queue item 1")).toHaveLength(2);
    }
    await act(async () => { refreshing.release(); });
    await settledWork();
    expect(screen.getAllByText("Queue item 1")).toHaveLength(2);
  });

  it("refreshes a bounded current selection after activity within the observation budget", async () => {
    const sources: EventTarget[] = [];
    vi.stubGlobal("EventSource", class extends EventTarget {
      readyState = 1;
      constructor() { super(); sources.push(this); }
      close() {}
    });
    const fixture = await pagedWork("/work?view=list&tab=all");
    await settledWork();
    act(() => { for (const source of sources) source.dispatchEvent(new MessageEvent("activity", { data: "40" })); });
    fireEvent.click(screen.getByTestId("work-list-more"));
    await waitFor(() => expect(screen.queryByTestId("work-list-more")).toBeNull());
    const before = fixture.requests.length;
    await fixture.control({ openOverflow: true });
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "41" })));
    await waitFor(() => expect(fixture.requests.slice(before).some((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))).toBe(true));
    await settledWork();
    const refreshed = fixture.requests.slice(before);
    const alphaReads = refreshed.filter((request) => request.url.pathname.endsWith("/proj_alpha/work-items") && request.url.searchParams.get("limit") === "100");
    expect(alphaReads).toHaveLength(2);
    expect(alphaReads[0]!.url.searchParams.has("cursor")).toBe(false);
    expect(alphaReads[1]!.url.searchParams.has("cursor")).toBe(true);
    expect(screen.queryByTestId("work-list-more")).toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe("1 completed · 48h");
    expect(refreshed.filter((request) => request.url.pathname.endsWith("/attempts"))).toHaveLength(17);
    expect(refreshed.filter((request) => request.url.pathname.endsWith("/changes"))).toHaveLength(17);
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
  });
});
