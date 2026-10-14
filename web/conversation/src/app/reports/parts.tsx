import { Link } from "@tanstack/react-router";
import { ArrowRightIcon, ChevronDownIcon, ChevronRightIcon } from "lucide-react";
import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import type { ReportsReport } from "../../contracts/reports.ts";
import { issueTitle, reportsSearchParams, type ReportsSearch, type ReportTab } from "./model.ts";

export const NOT_RECORDED = "Not recorded";

/** A missing value: a dash, with the reason in its tooltip and accessible name. */
export function Dash(): React.ReactElement {
  return (
    <span className="text-muted-foreground" title={NOT_RECORDED}>
      <span aria-hidden>—</span>
      <span className="sr-only">{NOT_RECORDED}</span>
    </span>
  );
}

export function Muted({ children }: { readonly children: React.ReactNode }): React.ReactElement {
  return <p className="text-muted-foreground text-xs">{children}</p>;
}

export function IssueLink({
  id,
  report,
  className = "",
}: {
  readonly id: string;
  readonly report: ReportsReport;
  readonly className?: string;
}): React.ReactElement {
  const title = issueTitle(report, id);
  return (
    <Link
      to="/work/i/$workItemId"
      params={{ workItemId: id }}
      search={{ tab: "diagnostics" } as never}
      title={title}
      className={`min-w-0 truncate text-foreground hover:underline hover:underline-offset-2 ${className}`}
    >
      {title}
    </Link>
  );
}

export function LaneBadge({ lane }: { readonly lane: string }): React.ReactElement {
  return (
    <Badge variant={lane === "Done" ? "success" : "secondary"} size="sm" className="shrink-0">
      {lane}
    </Badge>
  );
}

/** "Open Flow →": a card's link into the tab that owns its detail, keeping the range and view. */
export function TabLink({
  tab,
  search,
  children,
}: {
  readonly tab: ReportTab;
  readonly search: ReportsSearch;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <Link
      to={tab === "overview" ? "/reports" : "/reports/$tab"}
      params={(tab === "overview" ? {} : { tab }) as never}
      search={reportsSearchParams(search) as never}
      className="inline-flex items-center gap-1 rounded-sm text-muted-foreground text-xs outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
    >
      {children}
      <ArrowRightIcon aria-hidden className="size-3.5" />
    </Link>
  );
}

export function ShowMore({
  shown,
  total,
  label,
  onToggle,
  expanded,
}: {
  readonly shown: number;
  readonly total: number;
  readonly label: string;
  readonly expanded: boolean;
  readonly onToggle: () => void;
}): React.ReactElement | null {
  if (total <= shown && !expanded) return null;
  return (
    <div>
      <Button size="compact" variant="outline" onClick={onToggle}>
        {expanded ? "Show fewer" : label}
      </Button>
    </div>
  );
}

/** A row that expands in place to show its detail. */
export function ExpandRow({
  summary,
  children,
  label,
}: {
  readonly summary: React.ReactNode;
  readonly children: React.ReactNode;
  readonly label: string;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const id = React.useId();
  return (
    <div className="border-border border-t first:border-t-0">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={id}
        aria-label={label}
        onClick={() => setOpen((value) => !value)}
        className="flex w-full min-w-0 cursor-pointer items-center gap-3 py-2.5 text-left text-sm outline-none hover:bg-accent/40 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset"
      >
        <span className="flex min-w-0 flex-1 items-center gap-3">{summary}</span>
        {open ? (
          <ChevronDownIcon aria-hidden className="size-4 shrink-0 text-muted-foreground" />
        ) : (
          <ChevronRightIcon aria-hidden className="size-4 shrink-0 text-muted-foreground" />
        )}
      </button>
      {open ? (
        <div id={id} className="flex flex-col gap-1.5 pb-3 pl-1 text-muted-foreground text-xs">
          {children}
        </div>
      ) : null}
    </div>
  );
}
