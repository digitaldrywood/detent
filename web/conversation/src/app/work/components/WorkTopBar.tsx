import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../../components/ui/tooltip.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../../components/WorkspaceBreadcrumb.tsx";
import { cn } from "../../../lib/utils.ts";
import { ProjectGlyph } from "../../components/ProjectGlyph.tsx";
import type { ConnectionChip } from "../../App.tsx";
import { COLLAPSED_SIDEBAR_TITLEBAR_INSET_CLASS } from "../../../workspaceTitlebar.ts";

const TONE_DOT: Record<string, string> = {
  "dc-ok": "bg-success",
  "dc-warn": "bg-warning",
  "dc-err": "bg-error",
  "": "bg-muted-foreground",
};

const TONE_TEXT: Record<string, string> = {
  "dc-ok": "text-success-foreground",
  "dc-warn": "text-warning-foreground",
  "dc-err": "text-error-foreground",
  "": "text-muted-foreground",
};

export function FreshnessChip({
  chip,
  testId = "connection-chip",
}: {
  chip: ConnectionChip;
  testId?: string;
}): React.ReactElement {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <span
            role="status"
            tabIndex={0}
            data-testid={testId}
            className={cn(
              "inline-flex h-7 shrink-0 items-center gap-1.5 rounded-full border border-border px-2 font-medium text-xs sm:h-6",
              TONE_TEXT[chip.tone] ?? TONE_TEXT[""],
            )}
          />
        }
      >
        <span
          aria-hidden
          className={cn(
            "size-1.5 shrink-0 rounded-full",
            TONE_DOT[chip.tone] ?? TONE_DOT[""],
            chip.tone === "dc-ok" && "motion-safe:animate-status-pulse",
          )}
        />
        {chip.label}
        {chip.detail === null ? null : (
          <span className="hidden text-muted-foreground sm:inline">· {chip.detail}</span>
        )}
      </TooltipTrigger>
      <TooltipPopup side="bottom" className="max-w-72 whitespace-normal leading-tight">
        {chip.tooltip}
      </TooltipPopup>
    </Tooltip>
  );
}

export interface WorkTopBarProps {
  /** The leading breadcrumb segment, e.g. `Work` or the project's name. */
  readonly context: string;
  /** The current segment. It is the route's one `h1` (B.14). */
  readonly title: string;
  readonly meta?: string | null;
  /** Rendered before the identifier in the current segment, e.g. `#3363`. */
  readonly identifier?: string | null;
  readonly connection: ConnectionChip;
  readonly actions?: React.ReactNode;
}

export function WorkTopBar(props: WorkTopBarProps): React.ReactElement {
  return (
    <header
      data-work-header
      className={cn(
        "@container/header-actions flex h-[var(--workspace-topbar-height)] shrink-0 items-center gap-2 border-border border-b px-3 sm:gap-3 sm:px-4",
        COLLAPSED_SIDEBAR_TITLEBAR_INSET_CLASS,
      )}
    >

      <WorkspaceBreadcrumb
        ariaLabel="Breadcrumb"
        className="min-w-0 flex-1 overflow-clip [overflow-clip-margin:2px]"
      >
        <WorkspaceBreadcrumbItem className="hidden shrink sm:flex">
          <span className="inline-flex min-w-0 max-w-full items-center gap-1.5">
            <ProjectGlyph className="size-3.5 shrink-0" projectName={props.context} />
            <span className="max-w-40 truncate">{props.context}</span>
          </span>
        </WorkspaceBreadcrumbItem>
        <WorkspaceBreadcrumbSeparator className="hidden sm:flex" />
        <WorkspaceBreadcrumbItem current className="min-w-10 flex-1">
          <h1 className="flex min-w-0 items-baseline gap-1.5 truncate font-medium text-sm">
            {props.identifier == null ? null : (
              <span className="shrink-0 font-mono text-muted-foreground text-xs tabular-nums">
                {props.identifier}
              </span>
            )}
            <span className="min-w-0 truncate">{props.title}</span>
          </h1>
        </WorkspaceBreadcrumbItem>
        {props.meta == null ? null : (
          <WorkspaceBreadcrumbItem className="hidden shrink-0 @2xl/header-actions:flex">
            <span className="text-muted-foreground text-xs">{props.meta}</span>
          </WorkspaceBreadcrumbItem>
        )}
      </WorkspaceBreadcrumb>

      <div className="flex shrink-0 items-center justify-end gap-1.5 @3xl/header-actions:gap-2">
        {props.actions}
        <FreshnessChip chip={props.connection} />
        {props.connection.action == null ? null : (
          <Button variant="ghost" size="sm" onClick={props.connection.action.onClick}>
            {props.connection.action.label}
          </Button>
        )}
      </div>
    </header>
  );
}
