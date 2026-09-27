// Pull requests (the sidebar's Browse → Pull requests destination).
//
// There is no project-scoped changes endpoint. `GET .../work-items/:item/changes`
// is the only list the hub serves and it is scoped to one work item, so this
// page is assembled the only way it can be: from the issues the board already
// read, keeping the ones that carry a change. That is honest but partial, and
// the page says so rather than implying it is the whole review queue.
//
// The hub agent's note is in the README under "What the hub does not serve":
// a `GET /api/v2/organizations/:org/projects/:project/changes` returning
// `Page[ChangeRequest]` with its summary would replace this whole file.
import { ExternalLinkIcon, GitPullRequestIcon } from "lucide-react";
import React from "react";
import { useNavigate } from "@tanstack/react-router";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
import { useShell } from "../App.tsx";
import { ageLabel, issueNumber } from "./lib/format.ts";
import { useBoard, useNow } from "./lib/useWork.ts";
import { DEFAULT_VIEW_STATE } from "./lib/viewState.ts";
import { Pill, type PillTone } from "./components/IssueCard.tsx";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import type { ConnectionChip } from "../App.tsx";

function reviewTone(review: string): PillTone {
  switch (review) {
    case "ready":
    case "merged":
      return "ok";
    case "blocked":
      return "err";
    case "pending":
      return "warn";
    default:
      return "mute";
  }
}

export function ChangesPage(): React.ReactElement {
  const shell = useShell();
  const navigate = useNavigate();
  const now = useNow();
  const projectId = shell.projectId === "" ? null : shell.projectId;
  const board = useBoard(projectId, DEFAULT_VIEW_STATE);
  const withChanges = board.items.filter((item) => item.change !== null);

  // Not the board's chip: this page is knowingly partial, because the hub
  // serves changes per work item only (see the scope note below), so what it
  // has to say about its own freshness is that fact and not the stream's.
  const connection: ConnectionChip = board.loading
    ? {
        tone: "",
        label: "Loading",
        detail: "reading changes",
        action: null,
        tooltip: "Loading: reading this board's issues to find their changes.",
      }
    : {
        tone: "dc-warn",
        label: "Partial",
        detail: `${board.enriched} issues read for changes`,
        action: { label: "Reload", onClick: board.reload },
        tooltip:
          "Partial: only the most recently updated unfinished issues were read for changes, Reload reads them again.",
      };

  return (
    <>
      <WorkTopBar
        context="Browse"
        title="Pull requests"
        meta={`${withChanges.length} open`}
        connection={connection}
      />
      <div className="min-h-0 flex-1 overflow-auto px-5 py-4">
        <p className="mb-3 text-muted-foreground text-xs" data-testid="changes-scope-note">
          Built from the {board.enriched} most recently updated unfinished issues on this board. The
          hub serves changes per work item only, so this is not the whole review queue.
        </p>
        {withChanges.length === 0 ? (
          <p className="py-12 text-center text-muted-foreground text-sm" data-testid="changes-empty">
            No change requests on the issues read so far.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-24">Change</TableHead>
                <TableHead>Title</TableHead>
                <TableHead className="w-24">Issue</TableHead>
                <TableHead className="w-28">Review</TableHead>
                <TableHead className="w-16 text-right">Age</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {withChanges.map((item) => {
                const change = item.change!;
                return (
                  <TableRow key={change.id} data-testid="changes-row">
                    <TableCell className="font-mono text-muted-foreground tabular-nums">
                      <span className="inline-flex items-center gap-1">
                        <GitPullRequestIcon className="size-3" />
                        {change.number === null ? "—" : `#${change.number}`}
                      </span>
                    </TableCell>
                    <TableCell className="max-w-0 whitespace-normal">
                      {change.url === null ? (
                        change.title
                      ) : (
                        <a
                          className="inline-flex items-center gap-1 underline-offset-4 hover:underline"
                          href={change.url}
                          rel="noreferrer noopener"
                          target="_blank"
                        >
                          {change.title}
                          <ExternalLinkIcon className="size-3 text-muted-foreground" />
                        </a>
                      )}
                    </TableCell>
                    <TableCell>
                      <button
                        type="button"
                        data-testid="changes-issue-link"
                        onClick={() =>
                          void navigate({
                            to: "/work/i/$workItemId",
                            params: { workItemId: item.id },
                          })
                        }
                        className="cursor-pointer font-mono text-muted-foreground tabular-nums underline-offset-4 hover:underline"
                      >
                        {issueNumber(item.identifier, item.number)}
                      </button>
                    </TableCell>
                    <TableCell>
                      <Pill tone={reviewTone(change.review)}>
                        {change.review === "" ? "unknown" : change.review}
                      </Pill>
                    </TableCell>
                    <TableCell className="text-right text-muted-foreground tabular-nums">
                      {ageLabel(item.updatedAt, now)}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </div>
    </>
  );
}
