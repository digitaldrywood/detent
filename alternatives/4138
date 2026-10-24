import { ChevronDownIcon, FolderIcon, GitBranchIcon, LinkIcon } from "lucide-react";
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { ComposerSurface } from "../../components/chat/ComposerSurface.tsx";
import {
  Menu,
  MenuGroup,
  MenuGroupLabel,
  MenuItem,
  MenuPopup,
  MenuTrigger,
} from "../../components/ui/menu.tsx";
import { cn } from "../../lib/utils.ts";
import type { IssuePullRequest } from "../adapters/issuePullRequest.ts";
import { issueNumberLabel } from "../adapters/sidebarThreads.ts";

export interface ComposerContextStripProps {
  /** The project this conversation runs in. */
  readonly projectName: string | null;
  /** The linked issue's identifier, or `null` while the chat is unlinked. */
  readonly issueIdentifier: string | null;
  /** The linked issue's lane, when the conversation carries one. */
  readonly issueLane?: string | null;
  /** Opens the handoff form. Absent when linking is not available. */
  readonly onCreateIssue?: (() => void) | null;
  /** Opens the linked issue. Absent while the chat is unlinked. */
  readonly onOpenIssue?: (() => void) | null;
  /**
   * True on the new-chat surface, before anything has been sent. The right
   * side says nothing there: a draft has no issue by definition.
   */
  readonly draft?: boolean;
  /** The linked issue's pull request, when the hub knows of one. */
  readonly pullRequest?: IssuePullRequest | null;
}

export function ComposerContextStrip(props: ComposerContextStripProps): React.ReactElement {
  const linked = props.issueIdentifier !== null;
  const scopeLabel = linked
    ? `Issue ${issueNumberLabel(props.issueIdentifier as string)}`
    : `Project ${props.projectName ?? "unknown"}`;

  return (
    <ComposerSurface.ContextStrip
      className="gap-1 font-normal text-muted-foreground/70 text-xs"
      data-testid="composer-context-strip"
    >
      <div className={cn("flex min-h-7 min-w-10 flex-1 items-center gap-1 sm:min-h-6")}>
        <Menu>
          <MenuTrigger
            render={<Button variant="ghost" size="xs" />}
            className="min-w-0 max-w-[48%] flex-initial justify-start font-normal text-muted-foreground/70 text-xs! hover:text-foreground/80"
            data-composer-context-control
            aria-label="Conversation scope"
            data-testid="composer-scope-menu"
          >
            <FolderIcon className="size-3 shrink-0" />
            <span
              data-composer-label
              className="min-w-0 max-w-[240px] group-data-[compact]/composer-context:max-w-0"
            >
              <span
                data-composer-label-motion
                className="block w-full min-w-0 max-w-[240px] truncate transition-opacity duration-180 ease-[cubic-bezier(0.32,0.72,0,1)] group-data-[compact]/composer-context:opacity-0 motion-reduce:transition-none"
              >
                {scopeLabel}
              </span>
            </span>
            <ChevronDownIcon className="size-3 shrink-0 opacity-50" />
          </MenuTrigger>
          <MenuPopup align="start" side="top" className="w-64">
            <MenuGroup>
              <MenuGroupLabel>Conversation</MenuGroupLabel>
              {linked ? (
                <MenuItem disabled={props.onOpenIssue == null} onClick={() => props.onOpenIssue?.()}>
                  <span className="flex min-w-0 items-center gap-1.5">
                    <LinkIcon className="size-3" />
                    <span className="min-w-0 truncate">Open issue</span>
                  </span>
                </MenuItem>
              ) : (
                <MenuItem
                  disabled={props.onCreateIssue == null}
                  onClick={() => props.onCreateIssue?.()}
                >
                  <span className="flex min-w-0 items-center gap-1.5">
                    <LinkIcon className="size-3" />
                    <span className="min-w-0 truncate">Create linked issue</span>
                  </span>
                </MenuItem>
              )}
            </MenuGroup>
          </MenuPopup>
        </Menu>
      </div>

      <div
        className="flex min-w-0 flex-initial items-center justify-end gap-1 @3xl/composer-surface:ml-auto"
        data-composer-context-control
        data-testid="composer-context-issue"
      >
        {props.draft === true ? null : linked ? (
          <LinkedContext issueLane={props.issueLane ?? null} pullRequest={props.pullRequest ?? null} />
        ) : (
          <span className="inline-flex h-7 min-w-0 max-w-full items-center gap-1 rounded-md border border-transparent px-[calc(--spacing(2)-1px)] font-normal text-muted-foreground/70 text-xs sm:h-6">
            <span className="min-w-0 truncate">No linked issue</span>
          </span>
        )}
      </div>
    </ComposerSurface.ContextStrip>
  );
}

function LinkedContext(props: {
  readonly issueLane: string | null;
  readonly pullRequest: IssuePullRequest | null;
}): React.ReactElement {
  const number = props.pullRequest?.number ?? null;
  const branch = props.pullRequest?.branch ?? null;
  const url = props.pullRequest?.url ?? "";
  if (number === null && branch === null) {
    return <ContextChip>{props.issueLane ?? ""}</ContextChip>;
  }
  return (
    <>
      {number === null ? null : (
        <a
          href={url}
          target="_blank"
          rel="noreferrer noopener"
          data-testid="composer-context-pull-request"
          title={props.pullRequest?.draft === true ? "Draft pull request" : "Pull request"}
          className="inline-flex shrink-0 items-center gap-0.5 rounded px-1 py-0.5 font-medium text-[11px] text-info-foreground tabular-nums hover:underline"
        >
          <LinkIcon className="size-3" />
          <span
            data-composer-label
            className="min-w-0 max-w-48 overflow-hidden group-data-[compact]/composer-context:max-w-0"
          >
            <span
              data-composer-label-motion
              className="block w-full min-w-0 max-w-48 truncate transition-opacity duration-180 ease-[cubic-bezier(0.32,0.72,0,1)] group-data-[compact]/composer-context:opacity-0 motion-reduce:transition-none"
            >
              #{number}
            </span>
          </span>
        </a>
      )}
      {branch === null ? null : (
        <ContextChip testId="composer-context-branch" title={branch}>
          <GitBranchIcon className="size-3 shrink-0" />
          {branch}
        </ContextChip>
      )}
    </>
  );
}

function ContextChip(props: {
  readonly children: React.ReactNode;
  readonly testId?: string;
  readonly title?: string;
}): React.ReactElement {
  return (
    <span className="flex min-w-0">
      <span
        data-testid={props.testId}
        title={props.title}
        className={cn(
          "inline-flex h-7 min-w-0 max-w-full items-center gap-1 rounded-md border border-transparent px-[calc(--spacing(2)-1px)] font-normal text-muted-foreground/70 text-xs sm:h-6",
        )}
      >
        <span
          data-composer-label
          className="min-w-0 max-w-[240px] group-data-[compact]/composer-context:max-w-0"
        >
          <span
            data-composer-label-motion
            className="flex w-full min-w-0 max-w-[240px] items-center gap-1 truncate transition-opacity duration-180 ease-[cubic-bezier(0.32,0.72,0,1)] group-data-[compact]/composer-context:opacity-0 motion-reduce:transition-none"
          >
            {props.children}
          </span>
        </span>
      </span>
    </span>
  );
}
