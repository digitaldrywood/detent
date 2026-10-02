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
import { StatsRow } from "../../src/app/work/components/StatsRow.tsx";
import { toAttemptView, toWorkItemView, transitionsFrom } from "../../src/app/work/lib/fromWire.ts";
import { boardStats, isBlocked, isLive, type Lane, type WorkItemView } from "../../src/app/work/lib/model.ts";

import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { ClientContext } from "../../src/app/client.ts";
import { WorkBoard } from "../../src/app/work/WorkBoard.tsx";
import { DEFAULT_VIEW_STATE, parseViewState, serializeViewState } from "../../src/app/work/lib/viewState.ts";
import { useBoard } from "../../src/app/work/lib/useWork.ts";
import { resetRunnerNamesForTests } from "../../src/app/work/lib/runnerNames.ts";
import { workPaginationFixture } from "../workPaginationFixture.ts";

afterEach(cleanup);

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
    expect(screen.getByTestId("issue-age").textContent).not.toBe("");
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

  it("keeps imported terminal inventory visibly distinct from native completion", () => {
    const imported = toWorkItemView({
      ...itemFixture as unknown as NativeIssue,
      state: "Done", terminal: true,
      provenance: { provider: "github", external_id: "91", author_id: "source_author" },
    }, "parable");
    render(
      <>
        <IssueCard item={imported} showProject now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />
        <IssueCard item={item({ id: "native", state: "Done", terminal: true })} showProject
          now={NOW} onOpen={vi.fn()} moves={[]} onMove={vi.fn()} />
      </>,
    );
    expect(screen.getAllByText("Source history")).toHaveLength(1);
    expect(screen.getByText("Source history").getAttribute("title")).toBe("Imported from github");
    expect(screen.getAllByText("Done")).toHaveLength(2);
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

  it("carries the PR chip when a change is linked", () => {
    render(
      <IssueCard
        item={item({
          change: {
            id: "change_1",
            number: 3372,
            title: "fix: renewal",
            state: "blocked",
            url: null,
            review: "blocked",
          },
        })}
        showProject={false}
        now={NOW}
        onOpen={vi.fn()}
        moves={[]}
        onMove={vi.fn()}
      />,
    );
    expect(screen.getByTestId("pr-chip").textContent).toContain("PR #3372");
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
        collapsed={false}
        onToggleCollapsed={vi.fn()}
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

  it("hides its cards when collapsed but keeps its count", () => {
    renderLane({ collapsed: true });
    expect(screen.queryByTestId("issue-card")).toBeNull();
    expect(screen.getByTestId("lane-count-Todo").textContent).toBe("1");
  });

  it("accepts a drop only when the card may land here", () => {
    const { onDrop } = renderLane();
    const body = screen.getByTestId("lane-body-Todo");
    fireEvent.dragOver(body);
    fireEvent.drop(body);
    expect(onDrop).toHaveBeenCalledTimes(1);

    cleanup();
    renderLane({ onDrop: null });
    const refusing = screen.getByTestId("lane-body-Todo");
    fireEvent.drop(refusing);
    // Nothing to assert but the absence of a call: the handler is not wired.
    expect(refusing).not.toBeNull();
  });

  it.each([true, false])("uses configured terminal=%s before counting and styling live work", (terminal) => {
    const running = item({ state: "Retired", terminal: false, attempt: toAttemptView(ATTEMPTS) });
    renderLane({
      lane: { id: "Retired", name: "Retired", terminal, category: terminal ? "completed" : "started" },
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
        items={[item(), item({ id: "wi_2", title: "Second", state: "Todo", blockedBy: [] })]}
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

  it("opens an issue from its title", () => {
    const onOpen = vi.fn();
    render(
      <WorkList
        items={[item()]}
        showProject={false}
        now={NOW}
        onOpen={onOpen}
        movesFor={() => []}
        onMove={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByTestId("work-list-open"));
    expect(onOpen).toHaveBeenCalledTimes(1);
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

describe("the stats row", () => {
  it("counts loaded work in four categories when scoped totals are absent", () => {
    const items = [
      item({ id: "a", state: "Todo", blockedBy: [] }),
      item({ id: "b", state: "In Progress", blockedBy: [], attempt: toAttemptView(ATTEMPTS) }),
      item({ id: "c", state: "Blocked" }),
      item({ id: "d", state: "Done", blockedBy: [] }),
    ];
    render(<StatsRow stats={boardStats(items, LANES)} hasMore={false} loadedCount={4} loading={false} onLoadMore={vi.fn()} />);
    expect(screen.getByTestId("stat-running").textContent).toBe("1 running");
    expect(screen.getByTestId("stat-ready").textContent).toBe("1 ready");
    expect(screen.getByTestId("stat-waiting").textContent).toBe("1 open");
    expect(screen.getByTestId("stat-completed").textContent).toBe("1 done");
    expect(screen.queryByRole("button", { name: /^Load / })).toBeNull();
  });

  it("keeps scoped counts while refreshing and offers continuation only for incomplete results", () => {
    const totals = { lanes: {}, running: 0, ready: 9, waiting: 70, completed: 126, total: 205, asOf: "2026-10-02T17:17:00Z", truncated: true };
    const onLoadMore = vi.fn();
    const props = { stats: boardStats([], LANES), totals, hasMore: true, loadedCount: 145, loading: false, onLoadMore };
    const mounted = render(<StatsRow {...props} />);
    const counts = screen.getByTestId("work-stats");
    expect(counts.textContent).toContain("0 running·9 ready·70 open·126 done");
    const more = screen.getByRole("button", { name: "Load 60 more · 145 of 205" });
    expect(counts.contains(more)).toBe(true);
    fireEvent.click(more);
    expect(onLoadMore).toHaveBeenCalledTimes(1);
    mounted.rerender(<StatsRow {...props} loading />);
    expect(counts.getAttribute("aria-busy")).toBe("true");
    expect(screen.getByTestId("stat-ready").parentElement!.className).toContain("opacity-50");
    expect(screen.getByTestId("stat-ready").textContent).toBe("9 ready");
    expect((more as HTMLButtonElement).disabled).toBe(true);
    mounted.rerender(<StatsRow {...props} hasMore={false} loadedCount={205} />);
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
  const root = createRootRoute({ component: Outlet });
  const all = createRoute({ getParentRoute: () => root, path: "/work", component: () => <WorkBoard projectId={null} /> });
  const project = createRoute({ getParentRoute: () => root, path: "/work/p/$projectId", component: () => {
    const { projectId } = project.useParams();
    return <WorkBoard projectId={projectId} />;
  } });
  const history = createMemoryHistory({ initialEntries: [path] });
  const router = createRouter({ routeTree: root.addChildren([all, project]), history });
  render(<ClientContext.Provider value={fixture.client}><RouterProvider router={router} /></ClientContext.Provider>);
  return { ...fixture, router, history };
}

async function settledWork() {
  await waitFor(() => expect(screen.getByTestId("work-stats").getAttribute("aria-busy")).toBe("false"));
}

describe("the filter-first Work surface", () => {
  it("keeps active workers visible with full-scope totals and appends matching results in both views", async () => {
    const { router, requests } = await pagedWork();
    await settledWork();
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
    expect(screen.getAllByTestId("connection-chip")).toHaveLength(1);
    expect(within(screen.getByTestId("work-toolbar")).queryByRole("status")).toBeNull();
    expect(screen.getByTestId("stat-running").textContent).toBe("1 running");
    expect(screen.queryByRole("navigation", { name: "Work pages" })).toBeNull();
    expect(screen.queryByRole("button", { name: /First page|Previous|Next page/ })).toBeNull();
    expect(screen.getByTestId("lane-count-Todo").textContent).toBe("8");
    expect(screen.getByTestId("lane-count-In Progress").textContent).toBe("2");
    expect(screen.getByTestId("lane-count-Done").textContent).toBe("131");
    expect(screen.getByTestId("stat-completed").textContent).toBe("131 done");
    expect(within(screen.getByTestId("work-stats")).getByRole("button", { name: /^Load \d+ more · \d+ of 141$/ })).not.toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /^Load / }));
    await waitFor(() => expect(screen.queryByRole("button", { name: /^Load / })).toBeNull());
    await settledWork();
    expect(router.state.location.searchStr).toBe("");
    const before = requests.length;
    fireEvent.click(screen.getByRole("radio", { name: "List" }));
    await screen.findByTestId("work-list");
    expect(screen.getAllByTestId("work-list-row")).toHaveLength(141);
    expect(screen.getByText("Older title needle a")).not.toBeNull();
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
    expect(requests).toHaveLength(before);
    expect(requests.filter((request) => request.url.pathname.endsWith("/work-items")).every((request) =>
      request.url.searchParams.get("limit") === "100" && request.url.searchParams.get("include") === "work")).toBe(true);
  });

  it.each(["Older title needle", "alpha#3421", "older-label"])("finds older %s matches beyond the initial inventory and open selection", async (q) => {
    const fixture = await pagedWork();
    await settledWork();
    fireEvent.click(screen.getByRole("radio", { name: "List" }));
    await screen.findByTestId("work-list");
    expect(screen.queryByText("Older title needle a")).toBeNull();
    fireEvent.change(screen.getByTestId("work-search"), { target: { value: q } });
    await settledWork();
    expect(screen.getByText("Older title needle a")).not.toBeNull();
    expect(screen.queryByText("Observed later-page worker")).toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe(q === "alpha#3421" ? "1 done" : "2 done");
    expect(parseViewState(fixture.router.state.location.searchStr).q).toBe(q);
    expect(fixture.requests.findLast((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))?.url.searchParams.get("q")).toBe(q);
  });

  it("applies multiple values in all dimensions to items and counts without expanding the selected project", async () => {
    const view = parseViewState("view=list&q=Older title needle&state=Todo,Done&label=choice-a,choice-b&assignee=operator-a,operator-b&priority=Urgent,High");
    const fixture = await pagedWork(`/work/p/proj_alpha?${serializeViewState(view)}`);
    await settledWork();
    expect(screen.getByText("Older title needle a")).not.toBeNull();
    expect(screen.getByText("Older title needle b")).not.toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe("2 done");
    fireEvent.click(screen.getByTestId("filters-trigger"));
    for (const filter of ["label-choice-b", "assignee-operator-b", "priority-High"]) {
      fireEvent.click(await screen.findByTestId(`filter-${filter}`));
      await settledWork();
      expect(screen.getByTestId("stat-completed").textContent).toBe("1 done");
      fireEvent.click(await screen.findByTestId(`filter-${filter}`));
      await settledWork();
      expect(screen.getByTestId("stat-completed").textContent).toBe("2 done");
    }
    fireEvent.keyDown(document, { key: "Escape" });
    const reads = fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items"));
    expect(reads.length).toBeGreaterThan(0);
    expect(reads.every((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))).toBe(true);
    const params = reads.at(-1)!.url.searchParams;
    expect(params.getAll("state")).toEqual(["Done", "Todo"]);
    expect(params.getAll("label")).toEqual(["choice-a", "choice-b"]);
    expect(params.getAll("assignee")).toEqual(["operator-a", "operator-b"]);
    expect(params.getAll("priority")).toEqual(["1", "0"]);
    fireEvent.click(screen.getByRole("radio", { name: "Board" }));
    await screen.findByTestId("work-board");
    expect(parseViewState(fixture.router.state.location.searchStr)).toEqual({ ...view, view: "board" });
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items"))).toHaveLength(reads.length);
  });

  it("keeps a second filter choice available after the first narrows results", async () => {
    const fixture = await pagedWork("/work/p/proj_alpha?view=list&q=Older+title+needle");
    await settledWork();
    fireEvent.click(screen.getByTestId("filters-trigger"));
    fireEvent.click(await screen.findByTestId("filter-label-choice-a"));
    await settledWork();
    expect(screen.getByTestId("stat-completed").textContent).toBe("1 done");
    fireEvent.click(await screen.findByTestId("filter-label-choice-b"));
    await settledWork();
    expect(screen.getByTestId("stat-completed").textContent).toBe("2 done");
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
    const nextClient = { ...fixture.client, http: { ...fixture.client.http, csrfToken: "other-client" } };
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
    expect(screen.getByRole("button", { name: /^Load \d+ more · \d+ of 134$/ })).not.toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /^Load / }));
    await waitFor(() => expect(screen.queryByRole("button", { name: /^Load / })).toBeNull());
    await settledWork();
    expect(screen.getAllByTestId("work-list-row")).toHaveLength(134);
    expect(screen.getByText("Queue item 1")).not.toBeNull();
    expect(screen.getByText("Queue item 136")).not.toBeNull();
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items")).every((request) =>
      request.url.pathname.endsWith("/proj_alpha/work-items"))).toBe(true);
  });

  it("restores linked filters and discards legacy transport cursors", async () => {
    const fixture = await pagedWork('/work?view=list&q=older-label&pages=legacy-cursor');
    await settledWork();
    expect(screen.getByText("Older title needle a")).not.toBeNull();
    expect(screen.getByTestId("stat-completed").textContent).toBe("2 done");
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/work-items")).every((request) =>
      !request.url.searchParams.has("cursor"))).toBe(true);
  });

  it.each(["search", "multi-select", "archived"])("aborts stale continuation and resets cursors when %s changes", async (change) => {
    const fixture = await pagedWork();
    await settledWork();
    const deferred = fixture.deferPage();
    fireEvent.click(screen.getByRole("button", { name: /^Load / }));
    await deferred.waiting;
    const pending = fixture.requests.findLast((request) => request.url.searchParams.has("cursor"))!;
    if (change === "search") fireEvent.change(screen.getByTestId("work-search"), { target: { value: "older-label" } });
    else if (change === "archived") fireEvent.click(screen.getByTestId("work-archived"));
    else {
      const view = parseViewState(fixture.router.state.location.searchStr);
      await act(() => fixture.router.navigate({ to: "/work", search: Object.fromEntries(new URLSearchParams(serializeViewState({ ...view, state: ["Todo", "In Progress"] }))) }));
    }
    await settledWork();
    expect(pending.signal?.aborted).toBe(true);
    const newest = fixture.requests.findLast((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))!;
    expect(newest.url.searchParams.has("cursor")).toBe(false);
    await act(async () => { deferred.release(); });
    if (change !== "multi-select") expect(screen.queryByText("Observed later-page worker")).toBeNull();
  });

  it("refuses revoked scope without retaining stale cards or counts", async () => {
    const fixture = await pagedWork();
    await settledWork();
    await fixture.control({ revoked: true });
    fireEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(await screen.findByTestId("work-error")).not.toBeNull();
    expect(screen.queryByTestId("issue-card")).toBeNull();
    expect(screen.queryByTestId("work-list-row")).toBeNull();
    expect(screen.getByTestId("stat-running").textContent).toBe("0 running");
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
    const fixture = await pagedWork("/work", (fetch) => async (input, init) => {
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
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
    expect(fixture.requests.filter((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))).toHaveLength(2);

    const refreshing = fixture.deferPage();
    fireEvent.click(screen.getByRole("button", { name: "Load more matching work" }));
    await refreshing.waiting;
    const continuation = fixture.requests.findLast((request) => request.url.searchParams.has("cursor"))!;
    for (const sequence of [43, 44]) {
      act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: String(sequence) })));
      await act(() => new Promise((resolve) => setTimeout(resolve, 450)));
      expect(continuation.signal?.aborted).toBe(false);
      expect(screen.getByText("Observed later-page worker")).not.toBeNull();
      expect(screen.getByTestId("stat-running").textContent).toContain("1observed running");
    }
    await act(async () => { refreshing.release(); });
    await settledWork();
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
  });

  it("refreshes a bounded current selection after activity within the observation budget", async () => {
    const sources: EventTarget[] = [];
    vi.stubGlobal("EventSource", class extends EventTarget {
      readyState = 1;
      constructor() { super(); sources.push(this); }
      close() {}
    });
    const fixture = await pagedWork();
    await settledWork();
    act(() => { for (const source of sources) source.dispatchEvent(new MessageEvent("activity", { data: "40" })); });
    fireEvent.click(screen.getByRole("button", { name: /^Load / }));
    await waitFor(() => expect(screen.queryByRole("button", { name: /^Load / })).toBeNull());
    const before = fixture.requests.length;
    await fixture.control({ openOverflow: true });
    act(() => sources[0]!.dispatchEvent(new MessageEvent("activity", { data: "41" })));
    await waitFor(() => expect(fixture.requests.slice(before).some((request) => request.url.pathname.endsWith("/proj_alpha/work-items"))).toBe(true));
    await settledWork();
    const refreshed = fixture.requests.slice(before);
    const alphaReads = refreshed.filter((request) => request.url.pathname.endsWith("/proj_alpha/work-items"));
    expect(alphaReads).toHaveLength(2);
    expect(alphaReads[0]!.url.searchParams.has("cursor")).toBe(false);
    expect(alphaReads[1]!.url.searchParams.has("cursor")).toBe(true);
    expect(screen.getByTestId("stat-loaded").textContent).toContain("141 loaded items / 141 matching");
    expect(refreshed.filter((request) => request.url.pathname.endsWith("/attempts"))).toHaveLength(24);
    expect(refreshed.filter((request) => request.url.pathname.endsWith("/changes"))).toHaveLength(24);
    expect(screen.getByText("Observed later-page worker")).not.toBeNull();
  });
});
