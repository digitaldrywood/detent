// @vitest-environment jsdom
//
// The work surfaces' components: the board card, the lane, the list row, the
// issue card's pills and the review dock's tabs.
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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
  it("counts the issues on screen and nothing else", () => {
    const items = [
      item({ id: "a", state: "Todo", blockedBy: [] }),
      item({ id: "b", state: "In Progress", blockedBy: [], attempt: toAttemptView(ATTEMPTS) }),
      item({ id: "c", state: "Blocked" }),
      item({ id: "d", state: "Done", blockedBy: [] }),
    ];
    const stats = boardStats(items, LANES);
    render(<StatsRow stats={stats} truncated={false} enriched={2} />);
    expect(screen.getByTestId("stat-running").textContent).toContain("1");
    expect(screen.getByTestId("stat-blocked").textContent).toContain("1");
    expect(screen.getByTestId("stat-completed").textContent).toContain("1");
    expect(screen.getByTestId("stat-coverage").textContent).toContain("4 issues");
  });

  it("says when it is only looking at the first page", () => {
    render(<StatsRow stats={boardStats([], LANES)} truncated enriched={0} />);
    expect(screen.getByTestId("stat-coverage").textContent).toContain("first page");
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
