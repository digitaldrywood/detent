import { SavedViewControls } from "./SavedViews.tsx";
import { useSavedViews, type SavedView } from "./lib/savedViews.tsx";
// Moving a card is optimistic and conflict-aware. The card lands in the new
// lane immediately, the transition goes to the hub with the revision the card
// was rendered from, and a `409` puts the card back where it was, reloads, and
// says so. The hosted `409` carries no `current_revision`, so re-reading is
// the only correct response — guessing a revision would turn a refused move
// into a silent overwrite.
import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { ChevronDownIcon, DownloadIcon, HistoryIcon, LoaderCircleIcon, PlusIcon, SparklesIcon } from "lucide-react";

import { ToggleGroup, ToggleGroupItem } from "../../components/ui/toggle-group.tsx";
import { Button, InlineButton, SplitButton } from "../../components/ui/button.tsx";
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuShortcut, MenuTrigger } from "../../components/ui/menu.tsx";
import { useSidebar } from "../../components/ui/sidebar.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";
import { useClient } from "../client.ts";
import { usePageTitle } from "../pageTitle.ts";
import { transitionsFrom } from "./lib/fromWire.ts";
import { boardStats, sortItems, type WorkItemView } from "./lib/model.ts";
import { moveItem, useBoard, useWorkHttp } from "./lib/useWork.ts";
import { useViewState } from "./lib/useViewState.ts";
import { effectiveSort, laneVisible, tabMatches, toggleCollapsed, WORK_TABS, type WorkTab, type WorkViewState } from "./lib/viewState.ts";
import { BoardLane } from "./components/BoardLane.tsx";
import { ImportIssuesDialog } from "./ImportIssues.tsx";
import { FirstRunPanel } from "./components/FirstRun.tsx";
import {
  ISSUE_CREATION_NEEDS_WRITE,
  newIssueProjects,
  useNewIssue,
  useNewIssueScope,
} from "./NewIssue.tsx";
import { NEW_CHAT_KEYSHORTCUTS, NEW_ISSUE_KEYSHORTCUTS } from "../lib/shortcuts.ts";
import { CompletedCounter } from "./components/CompletedCounter.tsx";
import { LiveCounter } from "./components/LiveCounter.tsx";
import { toastManager } from "../../components/ui/toast.tsx";
import { WorkList } from "./components/WorkList.tsx";
import { WorkToolbar } from "./components/WorkToolbar.tsx";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import { useShell } from "../App.tsx";
import { usePublishProjectCounts, useWorkSidebarSearch } from "../adapters/sidebarData.tsx";
import { boardConnectionChip } from "./lib/freshness.ts";
import { allProjectsLabel } from "../lib/projectScope.ts";
import { useUiStateStore } from "../../uiStateStore.ts";
import { sidebarProjectScopeKey } from "../adapters/shell.ts";
import { prefetchIssue } from "./lib/issueQuery.ts";

const HOVER_INTENT_MS = 150;

function applyFilters(
  items: readonly WorkItemView[],
  view: WorkViewState,
): readonly WorkItemView[] {
  return items.filter((item) => {
    if (view.view === "board" && view.lanes !== null && !view.lanes.includes(item.state)) return false;
    if (view.view === "list" && !tabMatches(view.tab, item)) return false;
    if (view.view === "board" && view.state.length > 0 && !view.state.includes(item.state)) return false;
    if (view.priority.length > 0 && (item.priority === null || !view.priority.includes(item.priority)))
      return false;
    if (view.view === "list" && view.type.length > 0 && (item.type === undefined || !view.type.includes(item.type)))
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
    (view.view === "list" && view.type.length > 0) ||
    view.label.length > 0 ||
    view.assignee.length > 0
  );
}

export function WorkBoard({ projectId, savedView }: { projectId: string | null; savedView?: SavedView }): React.ReactElement {
  const navigate = useNavigate();
  const shell = useShell();
  const { setOpenMobile } = useSidebar();
  const client = useClient();
  const project = client.bootstrap.projects.find((candidate) => candidate.id === projectId);
  usePageTitle(savedView?.name ?? "Work", project?.name);
  const http = useWorkHttp();
  const workingSetKeys = useUiStateStore((state) => state.sidebarProjectWorkingSetKeys);
  const workingProjects = React.useMemo(() => client.bootstrap.projects
    .filter((entry) => workingSetKeys.includes(sidebarProjectScopeKey(entry)))
    .sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" })), [client.bootstrap.projects, workingSetKeys]);
  const workingIds = React.useMemo(() => workingProjects.length > 0 &&
    (projectId === null || workingProjects.some((entry) => entry.id === projectId))
    ? workingProjects.map((entry) => entry.id) : undefined, [workingProjects, projectId]);
  const [view, setView] = useViewState(workingIds === undefined ? projectId : null, savedView);
  const board = useBoard(projectId, view, workingIds, savedView !== undefined);
  const setCurrentSetup = useSavedViews()?.setCurrentSetup;
  React.useLayoutEffect(() => {
    setCurrentSetup?.({ project_id: projectId, project_ids: workingIds ?? (projectId === null ? [] : [projectId]), state: view });
  }, [setCurrentSetup, projectId, workingIds, view]);
  React.useEffect(() => () => setCurrentSetup?.(null), [setCurrentSetup]);
  usePublishProjectCounts(board.projectCounts);
  const currentView = React.useRef(view);
  currentView.current = view;
  const setQuery = React.useCallback((q: string) => setView({ ...currentView.current, q }), [setView]);
  useWorkSidebarSearch(view.q, setQuery);
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
    return sortItems(applyFilters(overridden, view), effectiveSort(view), view.q.trim().length > 0);
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

  // Hover intent: a card's first-paint read starts after the pointer rests on
  // it briefly, and is cancelled when the pointer leaves first.
  const pending = React.useRef<{ timer: ReturnType<typeof setTimeout>; controller: AbortController } | null>(null);
  const intentEnd = React.useCallback(() => {
    if (pending.current === null) return;
    clearTimeout(pending.current.timer);
    pending.current.controller.abort();
    pending.current = null;
  }, []);
  const intent = React.useCallback((item: WorkItemView) => {
    intentEnd();
    const controller = new AbortController();
    pending.current = {
      controller,
      timer: setTimeout(() => prefetchIssue(http, item.id, item.projectId, controller.signal), HOVER_INTENT_MS),
    };
  }, [http, intentEnd]);
  React.useEffect(() => () => {
    if (pending.current !== null) clearTimeout(pending.current.timer);
  }, []);

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
      ? (workingIds === undefined ? allProjectsLabel(client.bootstrap.organization?.name) : `${workingProjects.length} projects`)
      : (board.project?.name ?? projects.get(projectId) ?? projectId);
  const laneNames = board.lanes.map((lane) => lane.name);
  const visible = board.lanes.filter((lane) => laneVisible(view, lane));
  const noProjects = client.bootstrap.projects.length === 0;
  // A project board creates into its own project only; the all-projects board
  // into any project the reader can write to.
  const creatable = newIssueProjects(
    (accountBootstrap?.projects ?? []).filter((project) => projectId === null || project.id === projectId),
    null,
    accountBootstrap?.actor.role,
  );
  const canCreateHere = newIssue.canCreate && creatable.length > 0;
  const importTarget = projectId !== null && canCreateHere ? projectId : null;
  const firstRun =
    noProjects ||
    (board.resolved && board.error === null && board.items.length === 0 && !narrowed(view) &&
      !board.hasMore && !board.backlogHasMore && (board.totals?.total ?? 0) === 0);
  const [importProject, setImportProject] = React.useState<string | null>(null);
  const firstRunPanel = firstRun ? (
    <FirstRunPanel projectId={projectId} issues={board.items.length} onIssueCreated={board.reload} onImport={setImportProject} />
  ) : null;

  const ask = () => {
    if (projectId === null) void navigate({ to: "/chat" });
    else void navigate({ to: "/chat/p/$projectId", params: { projectId } });
  };

  const topBar = (
    <WorkTopBar
      context={savedView?.name ?? "Work"}
      title={scopeName}
      titleTooltip={projectId === null && workingIds !== undefined ? workingProjects.map((entry) => entry.name).join(", ") : scopeName}
      scopeGroup={projectId === null && workingIds !== undefined}
      completed={noProjects ? null : (
        <div className="flex shrink-0 items-center gap-1">
          <LiveCounter count={board.resolved ? stats.running : null} loading={board.loading} />
          <CompletedCounter
            count={board.totals?.completed ?? null}
            cancelled={board.totals?.cancelled ?? null}
            windows={board.totals?.shippedWindows ?? null}
            completedWindow={view.completedWindow}
            onCompletedWindowChange={(completedWindow) => setView({ ...view, completedWindow })}
            loading={board.loading}
          />
        </div>
      )}
      connection={chip}
      actions={noProjects ? null : (
        <>
          {projectId === null ? null : (
            <Button
              size="sm"
              variant="outline"
              className="hidden md:inline-flex"
              data-testid="board-import-issues"
              disabled={importTarget === null}
              title={importTarget === null ? ISSUE_CREATION_NEEDS_WRITE : "Import issues"}
              onClick={() => { if (importTarget !== null) setImportProject(importTarget); }}
            >
              <DownloadIcon />
              Import issues
            </Button>
          )}
          <Button
            size="sm"
            className="hidden md:inline-flex"
            data-testid="board-new-issue"
            aria-keyshortcuts={NEW_ISSUE_KEYSHORTCUTS}
            disabled={!canCreateHere}
            title={canCreateHere ? "New issue (C)" : ISSUE_CREATION_NEEDS_WRITE}
            onClick={() => newIssue.openNewIssue({ projectId })}
          >
            <PlusIcon />
            New issue
          </Button>
        </>
      )}
      mobileActions={
        <SplitButton className="md:hidden" aria-label="Work actions">
          <Button aria-keyshortcuts={NEW_CHAT_KEYSHORTCUTS} onClick={ask}>
            <SparklesIcon />
            Ask
          </Button>
          <Menu>
            <MenuTrigger render={<Button size="icon" aria-label="More work actions" />}>
              <ChevronDownIcon />
            </MenuTrigger>
            <MenuPopup align="end">
              <MenuItem className="min-h-11 sm:min-h-11" onClick={ask} aria-keyshortcuts={NEW_CHAT_KEYSHORTCUTS}>
                <SparklesIcon />
                Ask Detent
                <MenuShortcut>⇧⌘N</MenuShortcut>
              </MenuItem>
              <MenuItem
                className="min-h-11 sm:min-h-11"
                disabled={!canCreateHere}
                title={canCreateHere ? "New issue (C)" : ISSUE_CREATION_NEEDS_WRITE}
                aria-keyshortcuts={NEW_ISSUE_KEYSHORTCUTS}
                onClick={() => newIssue.openNewIssue({ projectId })}
              >
                <PlusIcon />
                New issue
                <MenuShortcut>C</MenuShortcut>
              </MenuItem>
              {projectId === null ? null : (
                <MenuItem
                  className="min-h-11 sm:min-h-11"
                  disabled={importTarget === null}
                  onClick={() => { if (importTarget !== null) setImportProject(importTarget); }}
                >
                  <DownloadIcon />
                  Import issues
                </MenuItem>
              )}
              <MenuSeparator />
              <MenuItem className="min-h-11 sm:min-h-11" onClick={() => setOpenMobile(true)}>
                <HistoryIcon />
                Recent chats
              </MenuItem>
            </MenuPopup>
          </Menu>
        </SplitButton>
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

  const backlog = Object.entries(board.totals?.lanes ?? {}).reduce(
    (total, [lane, count]) => total + (lane.toLowerCase() === "backlog" ? count : 0), 0,
  );
  const tabCounts = board.totals === null ? null : {
    active: board.totals.open - backlog,
    backlog,
    closed: board.totals.total - board.totals.open,
    all: board.totals.total,
  };

  return (
    <>
      {topBar}

      {board.workflows.length > 0 ? <WorkToolbar
        view={view}
        savedViewActive={savedView !== undefined}
        savedControls={<SavedViewControls state={view} projectId={projectId} lanes={board.lanes} active={savedView} onChange={setView} />}
        onChange={setView}
        lanes={board.lanes}
        totals={board.totals?.lanes}
        facets={{
          state: laneNames,
          label: board.labels,
          assignee: board.assignees,
          priority: board.priorities,
        }}
      /> : null}

      {view.view === "list" ? (
        <div className="flex shrink-0 flex-wrap items-center justify-between gap-3 px-3 pb-3 sm:px-5">
          <ToggleGroup size="segmented" aria-label="Issue list tabs" value={[view.tab]}
            onValueChange={(values) => { if (values[0]) setView({ ...view, tab: values[0] as WorkTab }); }}>
            {WORK_TABS.map((tab) => (
              <ToggleGroupItem key={tab} value={tab} data-testid={`list-tab-${tab}`}>
                {tab[0]!.toUpperCase() + tab.slice(1)} <span className="text-muted-foreground tabular-nums">{tabCounts?.[tab] ?? "—"}</span>
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
          <div className="ml-auto flex flex-wrap items-center justify-end gap-x-1 text-xs text-muted-foreground">
            {archiveHint === null ? null : <span>{archiveHint}</span>}
            <InlineButton className="shrink-0 text-xs" aria-pressed={view.archived === true}
              onClick={() => setView({ ...view, archived: view.archived !== true })}>
              {view.archived === true ? "Hide archived" : "Show archived"}
            </InlineButton>
          </div>
        </div>
      ) : null}

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
          <WorkList
            items={items}
            tab={view.tab}
            hasMore={board.hasMore}
            loading={board.loading}
            onLoadMore={board.loadMore}
            showProject={projectId === null}
            onOpen={open}
            onIntent={intent}
            onIntentEnd={intentEnd}
            movesFor={movesFor}
            onMove={move}
          />
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
                  total={lane.name.toLowerCase() === "backlog" ? board.backlogTotal : board.totals?.lanes[lane.name] ?? (board.totals === null ? undefined : 0)}
                  onShowMore={lane.name.toLowerCase() === "backlog" && board.backlogHasMore ? board.loadBacklog : null}
                  loadingMore={board.backlogLoading}
                  showProject={projectId === null}
                  onOpen={open}
                  onIntent={intent}
                  onIntentEnd={intentEnd}
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
                    newIssueProjects(creatable, lane.name, accountBootstrap?.actor.role).length > 0
                      ? () => newIssue.openNewIssue({ projectId, state: lane.name })
                      : null
                  }
                />
              ))
            )}
          </div>
        ))}
      </div>
      {importProject === null ? null : <ImportIssuesDialog key={importProject} projectId={importProject} onClose={() => { setImportProject(null); board.reload(); }} />}
    </>
  );
}
