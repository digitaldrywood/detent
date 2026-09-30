import { GitPullRequestIcon } from "lucide-react";
import React from "react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import { cn } from "../../../lib/utils.ts";
import { ageLabel, elapsedLabel, issueNumber, projectHue } from "../lib/format.ts";
import type { WorkItemView } from "../lib/model.ts";
import { LaneMenu } from "./LaneMenu.tsx";
import { Pill, priorityTone, statusPill } from "./IssueCard.tsx";

export function WorkList({
  items,
  showProject,
  now,
  onOpen,
  movesFor,
  onMove,
}: {
  items: readonly WorkItemView[];
  showProject: boolean;
  now: number;
  onOpen: (item: WorkItemView) => void;
  movesFor: (item: WorkItemView) => readonly string[];
  onMove: (item: WorkItemView, toState: string) => void;
}): React.ReactElement {
  if (items.length === 0) {
    return (
      <p className="px-5 py-12 text-center text-muted-foreground text-sm" data-testid="work-list-empty">
        No issues match this view.
      </p>
    );
  }
  return (
    <div className="px-5 pb-6" data-testid="work-list">
      <Table>
        <TableHeader>
          <TableRow>
            {showProject ? <TableHead className="w-40">Project</TableHead> : null}
            <TableHead className="w-20">Issue</TableHead>
            <TableHead>Title</TableHead>
            <TableHead className="w-32">Lane</TableHead>
            <TableHead className="w-36">Status</TableHead>
            <TableHead className="w-28">Worker</TableHead>
            <TableHead className="w-24">Priority</TableHead>
            <TableHead className="w-16 text-right">Age</TableHead>
            <TableHead className="w-8" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((item) => {
            const status = statusPill(item);
            return (
              <TableRow key={item.id} data-testid="work-list-row" data-work-item={item.id}>
                {showProject ? (
                  <TableCell className="text-muted-foreground">
                    <span className="inline-flex min-w-0 items-center gap-1.5">
                      <span
                        aria-hidden
                        className="size-2 shrink-0 rounded-full"
                        style={{
                          backgroundColor: `oklch(0.72 0.15 ${projectHue(item.projectId)})`,
                        }}
                      />
                      <span className="truncate">{item.projectName}</span>
                    </span>
                  </TableCell>
                ) : null}
                <TableCell className="font-mono text-muted-foreground tabular-nums">
                  {issueNumber(item.identifier, item.number)}
                </TableCell>
                <TableCell className="max-w-0 whitespace-normal">
                  <button
                    type="button"
                    data-testid="work-list-open"
                    onClick={() => onOpen(item)}
                    className="cursor-pointer rounded-sm text-left outline-none ring-ring focus-visible:ring-2 hover:underline"
                  >
                    {item.title}
                  </button>
                  {item.change === null ? null : (
                    <span className="ml-2 inline-flex items-center gap-1 text-muted-foreground text-[11px]">
                      <GitPullRequestIcon className="size-3" />
                      {item.change.number === null ? "Change" : `PR #${item.change.number}`}
                    </span>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground">{item.state}</TableCell>
                <TableCell>
                  {status === null ? (
                    <span className="text-muted-foreground">—</span>
                  ) : (
                    <Pill tone={status.tone}>{status.label}</Pill>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {item.attempt?.running === true ? (
                    <span className={cn("inline-flex items-center gap-1.5")}>
                      <span
                        aria-hidden
                        className="size-1.5 rounded-full bg-success motion-safe:animate-status-pulse"
                      />
                      <span className="tabular-nums">
                        {elapsedLabel(item.attempt.startedAt, now)}
                      </span>
                    </span>
                  ) : (
                    "—"
                  )}
                </TableCell>
                <TableCell>
                  {item.priority === null ? (
                    <span className="text-muted-foreground">—</span>
                  ) : (
                    <Pill tone={priorityTone(item.priority)}>{item.priority}</Pill>
                  )}
                </TableCell>
                <TableCell className="text-right text-muted-foreground tabular-nums">
                  {ageLabel(item.updatedAt, now)}
                </TableCell>
                <TableCell>
                  <LaneMenu item={item} lanes={movesFor(item)} onMove={onMove} />
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}
