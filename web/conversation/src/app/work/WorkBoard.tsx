// The Work board and list (artifact screen 1, design inventory A.1, task W01).
//
// One surface, two shapes. The filters, the search, the sort and the result
// set are shared; `?view=board|list` chooses whether they are stacked into
// lanes or into rows. That is deliberate: a reader who switches views is
// asking to see the same issues differently, not to run a different query.
//
// Moving a card is optimistic and conflict-aware. The card lands in the new
// lane immediately, the transition goes to the hub with the revision the card
// was rendered from, and a `409` puts the card back where it was, reloads, and
// says so. The hosted `409` carries no `current_revision`, so re-reading is
// the only correct response — guessing a revision would turn a refused move
// into a silent overwrite.
import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { LoaderCircleIcon, PlusIcon } from "lucide-react";

import { Button } from "../../components/ui/button.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";
import { useClient } from "../client.ts";
import { usePageTitle } from "../pageTitle.ts";
import { boardScopeMeta } from "./lib/format.ts";
import { transitionsFrom } from "./lib/fromWire.ts";
import { boardStats, sortItems, type WorkItemView } from "./lib/model.ts";
import { moveItem, useBoard, useWorkHttp } from "./lib/useWork.ts";
import { useViewState } from "./lib/useViewState.ts";
import { laneVisible, toggleCollapsed, type WorkViewState } from "./lib/viewState.ts";
import { BoardLane } from "./components/BoardLane.tsx";
import { FirstRunPanel } from "./components/FirstRun.tsx";
import {
  ISSUE_CREATION_NEEDS_WRITE,
  newIssueProjects,
  useNewIssue,
  useNewIssueScope,
} from "./NewIssue.tsx";
import { NEW_ISSUE_KEYSHORTCUTS } from "../lib/shortcuts.ts";
import { StatsRow } from "./components/StatsRow.tsx";
import { toastManager } from "../../components/ui/toast.tsx";
import { WorkList } from "./components/WorkList.tsx";
import { WorkToolbar } from "./components/WorkToolbar.tsx";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import { useShell } from "../App.tsx";
import { boardConnectionChip } from "./lib/freshness.ts";

function applyFilters(
  items: readonly WorkItemView[],
  view: WorkViewState,
): readonly WorkItemView[] {
  return items.filter((item) => {
    if (view.lanes !== null && !view.lanes.includes(item.state)) return false;
    if (view.state.length > 0 && !view.state.includes(item.state)) return false;
    if (view.priority.length > 0 && (item.priority === null || !view.priority.includes(item.priority)))
      return false;
    if (view.label.length > 0 && !view.label.some((label) => item.labels.includes(label)))
      return false;
    if (
      view.assignee.length > 0 &&
      !view.assignee.some((assignee) => item.assignees.includes(assignee))
    )
      return false;
    return true;
  });
}

function archivePeriodsHint(completed: number | null, cancelled: number | null): string | null {
  if (completed !== null && cancelled !== null) return `Closed issues archive after ${completed} days, cancelled after ${cancelled}.`;
  if (completed !== null) return `Completed issues archive after ${completed} days.`;
  if (cancelled !== null) return `Cancelled issues archive after ${cancelled} days.`;
  return null;
}

function narrowed(view: WorkViewState): boolean {
  return (
    view.archived === true ||
    view.q.trim().length > 0 ||
    view.state.length > 0 ||
    view.priority.length > 0 ||
    view.label.length > 0 ||
    view.assignee.length > 0
  );
}

export function WorkBoard({ projectId }: { projectId: string | null }): React.ReactElement {
  const navigate = useNavigate();
  const shell = useShell();
  const client = useClient();
  const project = client.bootstrap.projects.find((candidate) => candidate.id === projectId);
  usePageTitle("Work", project?.name);
  const http = useWorkHttp();
  const [view, setView] = useViewState(projectId);
  const board = useBoard(projectId, view);
  const searchRef = React.useRef<HTMLInputElement>(null);
  const newIssue = useNewIssue();
  const accountBootstrap = useAccountBootstrap();
  const accountApi = useAccountApi();
  const integration = useResource(async () => projectId === null ? null : accountApi.integration(projectId), [accountApi, projectId]);
  const archiveHint = integration.value?.profile === "native"
    ? archivePeriodsHint(integration.value.archive_completed_after_days, integration.value.archive_cancelled_after_days)
    : null;
  useNewIssueScope(projectId);

  // An issue created from any entry point, this board's or the palette's,
  // shows up here without waiting for the activity stream.
  const reloadBoard = board.reload;
  const createdSeen = React.useRef(newIssue.created);
  React.useEffect(() => {
    if (newIssue.created === createdSeen.current) return;
    createdSeen.current = newIssue.created;
    reloadBoard();
  }, [newIssue.created, reloadBoard]);

  // Optimistic overrides, keyed by work item. They survive until the next load
  // replaces them, which is what makes a moved card stay moved across the
  // reload the activity stream triggers.
  const [moved, setMoved] = React.useState<ReadonlyMap<string, string>>(new Map());
  const [moving, setMoving] = React.useState<ReadonlySet<string>>(new Set());
  const [dragging, setDragging] = React.useState<WorkItemView | null>(null);

  // An optimistic move stays on the card until the hub's own copy agrees
  // with it; a reload that still carries the old state (the activity tick can
  // land before the transition is readable) must not snap the card back.
  React.useEffect(() => {
    setMoved((current) => {
      let next: Map<string, string> | null = null;
      for (const [id, state] of current) {
        const item = board.items.find((candidate) => candidate.id === id);
        if (item !== undefined && item.state === state) {
          next ??= new Map(current);
          next.delete(id);
        }
      }
      return next ?? current;
    });
  }, [board.items]);

  const items = React.useMemo(() => {
    const overridden = board.items.map((item) => {
      const state = moved.get(item.id);
      return state === undefined ? item : {
        ...item,
        state,
        stateId: state,
        terminal: board.lanes.find((lane) => lane.name === state)?.terminal ?? item.terminal,
      };
    });
    return sortItems(applyFilters(overridden, view), view.sort);
  }, [board.items, board.lanes, moved, view]);

  const stats = React.useMemo(() => boardStats(items, board.lanes), [items, board.lanes]);
  const projects = React.useMemo(
    () => new Map(board.items.map((item) => [item.projectId, item.projectName])),
    [board.items],
  );

  // The transition allow-list per item. It needs the item's own project's
  // workflow, which in the all-projects scope is not one workflow — so a card
  // whose project was not the one loaded for lanes gets the lanes it can reach
  // through its own state's transitions or, failing that, none.
  const transitions = React.useMemo(() => new Map(board.workflows.map((project) => [
    project.project_id,
    new Map(project.states.map((state) => [state.name, transitionsFrom(project, state.name)])),
  ])), [board.workflows]);

  const movesFor = React.useCallback(
    (item: WorkItemView): readonly string[] =>
      item.archived ? [] : transitions.get(item.projectId)?.get(item.state) ?? [],
    [transitions],
  );

  const open = React.useCallback(
    (item: WorkItemView) => {
      void navigate({ to: "/work/i/$workItemId", params: { workItemId: item.id } });
    },
    [navigate],
  );

  const move = React.useCallback(
    (item: WorkItemView, toState: string) => {
      if (item.state === toState) return;
      setMoved((current) => new Map(current).set(item.id, toState));
      setMoving((current) => new Set(current).add(item.id));
      void moveItem(http, item, toState, projects.get(item.projectId) ?? item.projectName).then(
        (outcome) => {
          setMoving((current) => {
            const next = new Set(current);
            next.delete(item.id);
            return next;
          });
          if (outcome.ok) {
            // The hub's answer is the card now, revision included, so the
            // next move from it carries the right expected revision.
            board.applyItem(outcome.item);
            return;
          }
          // The hub's copy is authoritative: drop the overlay so the card
          // shows whatever the board holds, then re-read on a conflict.
          setMoved((current) => {
            const next = new Map(current);
            next.delete(item.id);
            return next;
          });

          toastManager.add({
            type: outcome.conflict ? "warning" : "error",
            title: outcome.conflict
              ? `${item.title} moved somewhere else first`
              : `${item.title} could not be moved`,
            description: outcome.conflict
              ? "Someone changed this issue while you were looking at it. The lane has been reloaded."
              : outcome.message,
            actionProps: { children: "Reload", onClick: board.reload },
          });
          if (outcome.conflict) board.reload();
        },
      );
    },
    [http, projects, board],
  );

  // `/` focuses the board's own search, the way it focuses the sidebar's in
  // the chat surfaces — and never while the reader is typing somewhere else.
  React.useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "/" || event.metaKey || event.ctrlKey || event.altKey) return;
      const target = event.target as HTMLElement | null;
      if (
        target !== null &&
        (target.isContentEditable ||
          ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName))
      )
        return;
      event.preventDefault();
      searchRef.current?.focus();
      searchRef.current?.select();
    };
    globalThis.document?.addEventListener("keydown", onKeyDown);
    return () => globalThis.document?.removeEventListener("keydown", onKeyDown);
  }, []);

  // One source of truth for both chips: the sidebar footer shows the app's
  // own connection state and so does this one, with the board's own stream as
  // the extra fact only this surface has (`lib/freshness.ts`).
  const chip = boardConnectionChip({
    app: shell.connection,
    streaming: board.live,
    loading: board.loading,
    refreshing: board.refreshing,
    cached: board.cached,
    asOf: board.asOf,
    onReload: board.reload,
  });
  const scopeName =
    projectId === null
      ? "All projects"
      : (board.project?.name ?? projects.get(projectId) ?? projectId);
  const laneNames = board.lanes.map((lane) => lane.name);
  const visible = board.lanes.filter((lane) => laneVisible(view, lane));
  const noProjects = client.bootstrap.projects.length === 0;
  // A project board creates into its own project only; the all-projects board
  // into any project the reader can write to.
  const creatable = newIssueProjects(
    (accountBootstrap?.projects ?? []).filter((project) => projectId === null || project.id === projectId),
    null,
  );
  const canCreateHere = newIssue.canCreate && creatable.length > 0;
  const firstRun =
    noProjects ||
    (board.resolved && board.error === null && board.items.length === 0 && !narrowed(view) &&
      !board.hasMore);
  const firstRunPanel = firstRun ? (
    <FirstRunPanel projectId={projectId} issues={board.items.length} onIssueCreated={board.reload} />
  ) : null;

  const topBar = (
    <WorkTopBar
      context="Work"
      title={scopeName}
      meta={board.resolved ? boardScopeMeta(projectId, client.bootstrap.projects.length, items.length) : undefined}
      connection={chip}
      showConnectionDetailOnMobile={chip.label === "Live"}
      actions={
        noProjects ? null : (
          <Button
            size="sm"
            data-testid="board-new-issue"
            aria-keyshortcuts={NEW_ISSUE_KEYSHORTCUTS}
            disabled={!canCreateHere}
            title={canCreateHere ? "New issue (C)" : ISSUE_CREATION_NEEDS_WRITE}
            onClick={() => newIssue.openNewIssue({ projectId })}
          >
            <PlusIcon />
            New issue
          </Button>
        )
      }
    />
  );

  if (noProjects) {
    return (
      <>
        {topBar}
        <div className="min-h-0 flex-1 overflow-auto">{firstRunPanel}</div>
      </>
    );
  }

  return (
    <>
      {topBar}

      {board.workflows.length > 0 ? <WorkToolbar
        view={view}
        onChange={setView}
        lanes={board.lanes}
        totals={board.totals?.lanes}
        facets={{
          state: laneNames,
          label: board.labels,
          assignee: board.assignees,
          priority: board.priorities,
        }}
        searchRef={searchRef}
      /> : null}

      {board.resolved ? <StatsRow
        stats={stats}
        completedWindow={view.completedWindow}
        totals={board.totals}
        hasMore={board.hasMore}
        loadedCount={board.items.length}
        loading={board.loading}
        onLoadMore={board.loadMore}
      /> : null}

      {board.error === null ? null : (
        <div className="mx-5 mb-3 rounded-lg border border-error/32 bg-error-surface px-3 py-2 text-error-foreground text-sm">
          <p data-testid="work-error">{board.error}</p>
          <Button size="xs" variant="outline" className="mt-2" onClick={board.reload}>
            Try again
          </Button>
        </div>
      )}

      <div className="min-h-0 flex-1 overflow-auto" aria-busy={!board.resolved && board.loading}>
        {!board.resolved ? (board.loading ? (
          <div className="flex items-center justify-center gap-2 py-12 text-muted-foreground text-sm" role="status">
            <LoaderCircleIcon aria-hidden="true" className="size-4 animate-spin motion-reduce:animate-none" />
            Loading work…
          </div>
        ) : null) : firstRunPanel ?? (view.view === "list" ? (
          <>
            {archiveHint === null ? null : <p className="px-5 pb-3 text-sm text-muted-foreground">{archiveHint}</p>}
            <WorkList
              items={items}
              showProject={projectId === null}
              onOpen={open}
              movesFor={movesFor}
              onMove={move}
            />
          </>
        ) : (
          <div className="flex h-full items-start gap-3 px-5 pb-5" data-testid="work-board">
            {visible.length === 0 ? (
              <p className="py-12 text-muted-foreground text-sm">
                Every lane is hidden. Use the Lanes menu to bring one back.
              </p>
            ) : (
              visible.map((lane) => (
                <BoardLane
                  key={lane.name}
                  lane={lane}
                  collapsed={view.collapsed.includes(lane.name)}
                  onToggleCollapsed={() => setView(toggleCollapsed(view, lane.name))}
                  items={items.filter((item) => item.state === lane.name)}
                  total={board.totals?.lanes[lane.name] ?? (board.totals === null ? undefined : 0)}
                  showProject={projectId === null}
                  onOpen={open}
                  movesFor={movesFor}
                  onMove={move}
                  movingIds={moving}
                  draggingId={dragging?.id ?? null}
                  onDragStart={setDragging}
                  onDragEnd={() => setDragging(null)}
                  onDrop={
                    dragging !== null && movesFor(dragging).includes(lane.name)
                      ? (target) => {
                          move(dragging, target.name);
                          setDragging(null);
                        }
                      : null
                  }
                  onCreate={
                    newIssue.canCreate &&
                    !lane.terminal &&
                    newIssueProjects(creatable, lane.name).length > 0
                      ? () => newIssue.openNewIssue({ projectId, state: lane.name })
                      : null
                  }
                />
              ))
            )}
          </div>
        ))}
      </div>

    </>
  );
}
