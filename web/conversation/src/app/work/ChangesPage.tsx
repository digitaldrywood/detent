import { ChevronDownIcon, ChevronRightIcon, ExternalLinkIcon, GitPullRequestIcon } from "lucide-react";
import React from "react";
import { useNavigate } from "@tanstack/react-router";

import { Empty, EmptyHeader, EmptyMedia, EmptyTitle } from "../../components/ui/empty.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { Tabs, TabsList, TabsPanel, TabsTab } from "../../components/ui/tabs.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { cn } from "../../lib/utils.ts";
import { useShell } from "../App.tsx";
import { useClient } from "../client.ts";
import { ProjectGlyph } from "../components/ProjectGlyph.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { ageLabel, issueNumber } from "./lib/format.ts";
import type { WorkItemView } from "./lib/model.ts";
import { useBoard, useNow } from "./lib/useWork.ts";
import { DEFAULT_VIEW_STATE } from "./lib/viewState.ts";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import type { ConnectionChip } from "../App.tsx";

const REVIEW_STATES = [
  { value: "needs_evidence", label: "Needs evidence", dot: "bg-warning", text: "text-warning-foreground" },
  { value: "stale_policy", label: "Stale policy", dot: "bg-error", text: "text-error-foreground" },
  { value: "reviewed", label: "Reviewed", dot: "bg-success", text: "text-success-foreground" },
  { value: "draft", label: "Draft", dot: "bg-muted-foreground", text: "text-foreground" },
] as const;

type ReviewState = (typeof REVIEW_STATES)[number]["value"];
type ReviewFilter = "all" | ReviewState;

function ChangeRow({ item, showProject, now }: {
  item: WorkItemView;
  showProject: boolean;
  now: number;
}): React.ReactElement {
  const navigate = useNavigate();
  const change = item.change!;
  const backend = item.attempt?.backend;
  const agent = backend ? backend.charAt(0).toUpperCase() + backend.slice(1) : "";
  return (
    <TableRow
      data-testid="changes-row"
      className={cn("grid items-center gap-x-2 px-4 py-2 sm:table-row sm:p-0",
        showProject ? "grid-cols-[auto_auto_auto_1fr_auto]" : "grid-cols-[auto_auto_1fr_auto]")}
    >
      {showProject && (
        <TableCell className="min-w-0 p-0 sm:p-2">
          <span className="flex items-center gap-1.5 text-muted-foreground">
            <ProjectGlyph projectId={item.projectId} projectName={item.projectName} className="size-3.5 shrink-0" />
            <span className="hidden truncate sm:inline">{item.projectName}</span>
          </span>
        </TableCell>
      )}
      <TableCell className="p-0 text-muted-foreground tabular-nums sm:p-2">
        <span className="inline-flex items-center gap-1">
          <GitPullRequestIcon aria-hidden className="hidden size-3 sm:inline" />
          {change.number === null ? "—" : `#${change.number}`}
        </span>
      </TableCell>
      <TableCell className="col-span-full row-start-1 min-w-0 p-0 pb-0.5 sm:p-2">
        <div className="flex min-w-0 items-center gap-1.5">
          <button
            type="button"
            data-testid="changes-title-link"
            onClick={() => void navigate({
              to: "/work/i/$workItemId/changes/$changeId",
              params: { workItemId: item.id, changeId: change.id },
              search: { project: item.projectId },
            })}
            className="min-w-0 cursor-pointer truncate text-left text-sm underline-offset-4 hover:underline focus-visible:ring-2 focus-visible:ring-ring sm:text-xs"
          >
            {change.title}
          </button>
          {change.url !== null && (
            <a
              className="inline-flex shrink-0 text-muted-foreground hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
              href={change.url}
              rel="noreferrer noopener"
              target="_blank"
              aria-label="Open the pull request on the host"
            >
              <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>
      </TableCell>
      <TableCell className="p-0 sm:p-2">
        <button
          type="button"
          data-testid="changes-issue-link"
          onClick={() => void navigate({ to: "/work/i/$workItemId", params: { workItemId: item.id } })}
          className="cursor-pointer text-muted-foreground tabular-nums underline-offset-4 hover:underline focus-visible:ring-2 focus-visible:ring-ring"
        >
          {issueNumber(item.identifier, item.number)}
        </button>
      </TableCell>
      <TableCell className="p-0 text-muted-foreground sm:p-2">{agent}</TableCell>
      <TableCell className="p-0 text-right text-muted-foreground tabular-nums sm:p-2">
        {ageLabel(item.updatedAt, now)}
      </TableCell>
    </TableRow>
  );
}

export function ChangesPage(): React.ReactElement {
  const shell = useShell();
  const client = useClient();
  const now = useNow();
  const projectId = shell.projectId === "" ? null : shell.projectId;
  const project = client.bootstrap.projects.find((candidate) => candidate.id === projectId);
  usePageTitle("Pull requests", project?.name);
  const board = useBoard(projectId, DEFAULT_VIEW_STATE);
  const [filter, setFilter] = React.useState<ReviewFilter>("all");
  const [collapsed, setCollapsed] = React.useState<ReadonlySet<ReviewState>>(new Set());
  const showProject = projectId === null;
  const withChanges = board.items.filter((item) => item.change !== null
    && !["landed", "merged"].includes(item.change.state)
    && REVIEW_STATES.some((state) => state.value === item.change!.review));
  const groups = REVIEW_STATES.map((state) => ({ ...state,
    items: withChanges.filter((item) => item.change!.review === state.value),
  }));
  const connection: ConnectionChip = board.loading
    ? { tone: "", label: "Loading", detail: null, action: null,
        tooltip: "Loading: reading this board's issues to find their changes." }
    : { tone: "dc-warn", label: "Partial", detail: null,
        action: { label: "Reload", onClick: board.reload },
        tooltip: "Only changes on the most recently updated unfinished issues are shown. Reload reads them again." };
  const columns = showProject ? 6 : 5;
  const tableHeader = (
    <TableHeader className="hidden sm:table-header-group">
      <TableRow>
        {showProject && <TableHead className="w-40">Project</TableHead>}
        <TableHead className="w-20">PR</TableHead>
        <TableHead>Title</TableHead>
        <TableHead className="w-20">Issue</TableHead>
        <TableHead className="w-24">Agent</TableHead>
        <TableHead className="w-16 text-right">Updated</TableHead>
      </TableRow>
    </TableHeader>
  );
  return (
    <>
      <WorkTopBar context="Browse" title="Pull requests"
        meta={board.loading ? null : `${withChanges.length} open`} connection={connection} />
      <div className="flex min-h-0 flex-1 flex-col overflow-auto sm:px-5 sm:py-4">
        {board.loading ? (
          <div role="status" aria-label="Loading pull requests" aria-busy="true">
            <div className="mb-3 flex gap-2 overflow-hidden px-4 py-3 sm:p-0" aria-hidden>
              {["w-14", "w-32", "w-28", "w-24", "w-16"].map((width) => (
                <Skeleton key={width} className={cn("h-8 shrink-0", width)} />
              ))}
            </div>
            <Table className="table-fixed max-sm:block">
              {tableHeader}
              <TableBody className="max-sm:block">
                {Array.from({ length: 6 }, (_, index) => (
                  <TableRow key={index} data-testid="changes-skeleton-row"
                    className="max-sm:flex max-sm:flex-wrap max-sm:gap-2 max-sm:px-4 max-sm:py-3">
                    {Array.from({ length: columns }, (_, column) => (
                      <TableCell key={column} className={cn("py-3", column === (showProject ? 2 : 1)
                        ? "max-sm:order-first max-sm:w-full" : "max-sm:p-0")}>
                        <Skeleton className={column === (showProject ? 2 : 1) ? "h-3 w-3/4" : "h-3 w-10"} />
                      </TableCell>
                    ))}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        ) : (
          <Tabs value={filter} onValueChange={(value) => setFilter(value as ReviewFilter)} variant="segmented"
            className="flex-1">
            <div className="min-w-0 px-4 pt-3 sm:p-0">
            <TabsList aria-label="Review state" className="mb-3">
              {[{ value: "all", label: "All", items: withChanges }, ...groups].map((tab) => (
                <TabsTab key={tab.value} value={tab.value}>
                  <span className="inline-flex items-center gap-1.5">
                    {filter === tab.value && tab.value !== "all" && (
                      <span aria-hidden className={cn("size-1.5 rounded-full", groups.find((group) => group.value === tab.value)?.dot)} />
                    )}
                    {tab.label}<span className="text-muted-foreground tabular-nums">{tab.items.length}</span>
                  </span>
                </TabsTab>
              ))}
            </TabsList>
            </div>
            <TabsPanel value={filter} className="flex flex-1 flex-col">
              {(filter === "all" ? withChanges.length === 0 : groups.find((group) => group.value === filter)?.items.length === 0) ? (
                <Empty data-testid="changes-empty">
                  <EmptyHeader>
                    <EmptyMedia><GitPullRequestIcon aria-hidden className="size-8 text-muted-foreground" /></EmptyMedia>
                    <EmptyTitle>No open pull requests</EmptyTitle>
                  </EmptyHeader>
                </Empty>
              ) : (
                <div className="sm:overflow-hidden sm:rounded-lg sm:border sm:border-border">
                  <Table className="table-fixed max-sm:block">
                    {tableHeader}
                    {groups.filter((group) => group.items.length > 0 && (filter === "all" || filter === group.value)).map((group) => {
                      const isCollapsed = collapsed.has(group.value);
                      const header = (
                        <button type="button" aria-expanded={!isCollapsed}
                          className={cn("flex w-full cursor-pointer items-center gap-2 px-4 py-2 text-left font-medium text-xs sm:px-3 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring", group.text)}
                          onClick={() => setCollapsed((previous) => {
                            const next = new Set(previous);
                            if (next.has(group.value)) next.delete(group.value);
                            else next.add(group.value);
                            return next;
                          })}>
                          {isCollapsed ? <ChevronRightIcon aria-hidden className="size-3 text-muted-foreground" /> : <ChevronDownIcon aria-hidden className="size-3 text-muted-foreground" />}
                          <span aria-hidden className={cn("size-2 rounded-full", group.dot)} />
                          {group.label}<span className="text-muted-foreground tabular-nums">{group.items.length}</span>
                        </button>
                      );
                      return (
                        <TableBody key={group.value} className="max-sm:block">
                          {filter === "all" && (
                            <TableRow className="bg-muted/30 max-sm:block">
                              <TableCell colSpan={columns} className="p-0 max-sm:block">
                                {group.value === "stale_policy" ? (
                                  <Tooltip>
                                    <TooltipTrigger render={header} aria-expanded={!isCollapsed} />
                                    <TooltipPopup role="tooltip" side="bottom" align="start" className="max-w-80 whitespace-normal">
                                      The project's policy changed after this round was published. A new round is needed.
                                    </TooltipPopup>
                                  </Tooltip>
                                ) : header}
                              </TableCell>
                            </TableRow>
                          )}
                          {(filter !== "all" || !isCollapsed) && group.items.map((item) => (
                            <ChangeRow key={item.id} item={item} showProject={showProject} now={now} />
                          ))}
                        </TableBody>
                      );
                    })}
                  </Table>
                </div>
              )}
            </TabsPanel>
          </Tabs>
        )}
      </div>
    </>
  );
}
