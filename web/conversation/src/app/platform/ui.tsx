import { Link } from "@tanstack/react-router";
import { CircleDashedIcon, RotateCwIcon } from "lucide-react";
import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../components/ui/dialog.tsx";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "../../components/ui/empty.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageContainer, type WorkspacePageWidth } from "../../components/WorkspacePageContainer.tsx";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { SidebarTrigger, useSidebar } from "../../components/ui/sidebar.tsx";
import { cn } from "../../lib/utils.ts";
import type { AccountError } from "../account/api.ts";
import { ControlError, StatusDot } from "../account/controls.tsx";
import type { Resource } from "../account/useResource.ts";
import { formatRelativeTime } from "../fleet/format.ts";
import { isPlanned, type PlannedEndpoint } from "./api.ts";

export { StatusDot };

export interface Crumb {
  readonly label: string;
  readonly to?: string;
}

function NavigationToggle(): React.ReactElement | null {
  const { isMobile, state } = useSidebar();
  if (!isMobile && state !== "collapsed") return null;
  return <SidebarTrigger aria-label="Open navigation" />;
}

export function PlatformPage({
  crumbs,
  actions,
  proposed = false,
  width = "expanded",
  children,
}: {
  readonly crumbs: readonly Crumb[];
  readonly actions?: React.ReactNode;
  readonly proposed?: boolean;
  readonly width?: WorkspacePageWidth;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col bg-background text-foreground">
      <WorkspacePageHeader className="h-auto min-h-[var(--workspace-topbar-height)] flex-wrap gap-y-1 py-1">
        <NavigationToggle />
        <WorkspaceBreadcrumb ariaLabel="Platform breadcrumb" className="min-w-0 py-2">
          <WorkspaceBreadcrumbItem>
            <Link to="/platform" className="hover:text-foreground">
              Platform
            </Link>
          </WorkspaceBreadcrumbItem>
          {crumbs.map((crumb, index) => {
            const last = index === crumbs.length - 1;
            return (
              <React.Fragment key={`${crumb.label}-${index}`}>
                <WorkspaceBreadcrumbSeparator />
                <WorkspaceBreadcrumbItem current={last} className={last ? "min-w-10" : undefined}>
                  {last || crumb.to === undefined ? (
                    last ? (
                      <h1 className="min-w-0 truncate">{crumb.label}</h1>
                    ) : (
                      <span>{crumb.label}</span>
                    )
                  ) : (
                    <Link to={crumb.to as never} className="hover:text-foreground">
                      {crumb.label}
                    </Link>
                  )}
                </WorkspaceBreadcrumbItem>
              </React.Fragment>
            );
          })}
        </WorkspaceBreadcrumb>
        {proposed ? (
          <Badge variant="outline" size="sm" className="text-muted-foreground" title="Backed by an endpoint the spec proposes">
            Proposed API
          </Badge>
        ) : null}
        <div className="ms-auto flex min-w-0 items-center gap-2">{actions}</div>
      </WorkspacePageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <WorkspacePageContainer width={width}>{children}</WorkspacePageContainer>
      </div>
    </div>
  );
}

/** A row of counters in the work board's stats-strip style. */
export function StatStrip({
  items,
  className,
}: {
  readonly items: readonly { readonly label: string; readonly value: React.ReactNode; readonly tone?: "ok" | "warn" | "error" }[];
  readonly className?: string;
}): React.ReactElement {
  return (
    <dl className={cn("grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3 lg:grid-cols-6", className)}>
      {items.map((item) => (
        <div key={item.label} className="min-w-0">
          <dt className="flex items-center gap-1.5 text-xs text-muted-foreground">
            {item.tone === undefined ? null : <StatusDot tone={item.tone} />}
            {item.label}
          </dt>
          <dd className="mt-1 text-xl font-semibold tabular-nums tracking-[-0.02em]">{item.value}</dd>
        </div>
      ))}
    </dl>
  );
}

export function PlannedState({ error }: { readonly error: PlannedEndpoint }): React.ReactElement {
  return (
    <Empty className="rounded-xl border border-dashed border-border/70 md:p-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <CircleDashedIcon />
        </EmptyMedia>
        <EmptyTitle>Not available yet</EmptyTitle>
        <EmptyDescription>
          This view needs <code className="font-mono text-xs">{error.endpoint}</code>, which the entry does not serve.
          It is listed in docs/platform-admin.md.
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

export function FailedState({
  error,
  onRetry,
}: {
  readonly error: AccountError;
  readonly onRetry: () => void;
}): React.ReactElement {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-3 rounded-xl border border-destructive/30 bg-destructive/8 px-4 py-3 text-[13px] text-destructive-foreground"
    >
      <span className="min-w-0 flex-1">{error.message}</span>
      <Button size="xs" variant="outline" onClick={onRetry}>
        <RotateCwIcon />
        Try again
      </Button>
    </div>
  );
}

export function LoadingRows({ rows = 4, label }: { readonly rows?: number; readonly label: string }): React.ReactElement {
  return (
    <div role="status" aria-label={label} className="flex flex-col gap-3 rounded-xl border border-border/60 bg-card/40 p-4">
      {Array.from({ length: rows }, (_, index) => (
        <Skeleton key={index} className={cn("h-4", index % 2 === 0 ? "w-3/4" : "w-1/2")} />
      ))}
    </div>
  );
}

/** The four states every read renders: loading, planned, failed, and the value. */
export function Loadable<A>({
  resource,
  label,
  rows,
  children,
}: {
  readonly resource: Resource<A>;
  readonly label: string;
  readonly rows?: number;
  readonly children: (value: A) => React.ReactNode;
}): React.ReactElement {
  if (resource.value !== undefined) return <>{children(resource.value)}</>;
  if (resource.error !== null) {
    if (isPlanned(resource.error)) return <PlannedState error={resource.error} />;
    return <FailedState error={resource.error} onRetry={() => void resource.refresh()} />;
  }
  return <LoadingRows label={`Loading ${label}`} rows={rows} />;
}

export function EmptyNote({ children }: { readonly children: React.ReactNode }): React.ReactElement {
  return <p className="px-3 py-6 text-center text-sm text-muted-foreground sm:px-4">{children}</p>;
}

export function stateTone(state: string): "ok" | "warn" | "error" | "idle" {
  if (state === "ready" || state === "active" || state === "healthy" || state === "delivered") return "ok";
  if (state === "failed" || state === "payment_failed" || state === "quarantined" || state === "disputed") return "error";
  if (
    state === "requested" ||
    state === "allocating" ||
    state === "deleting" ||
    state === "grace" ||
    state === "pending" ||
    state === "stale" ||
    state === "canceling"
  ) {
    return "warn";
  }
  return "idle";
}

export function StateLabel({ state, label }: { readonly state: string; readonly label?: string }): React.ReactElement {
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
      <StatusDot tone={stateTone(state)} />
      {label ?? state.replaceAll("_", " ")}
    </span>
  );
}

export function Ago({ at, now }: { readonly at: string | null; readonly now?: number }): React.ReactElement {
  if (at === null) return <span className="text-muted-foreground">Never</span>;
  return (
    <time dateTime={at} title={at}>
      {formatRelativeTime(at, now)}
    </time>
  );
}

export function day(value: string | null): string {
  if (value === null) return "Never";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toISOString().slice(0, 10);
}

export function formatCount(value: number): string {
  return new Intl.NumberFormat("en-US", { notation: value >= 10_000 ? "compact" : "standard" }).format(value);
}

/**
 * Every console action that changes something: a reason, a confirm button that
 * names the change, and the outcome inline. The reason is required because the
 * spec audits every staff action with one.
 */
export function ReasonDialog({
  trigger,
  title,
  description,
  confirm,
  destructive = false,
  disabled = false,
  disabledReason,
  onConfirm,
}: {
  readonly trigger: string;
  readonly title: string;
  readonly description: string;
  readonly confirm: string;
  readonly destructive?: boolean;
  readonly disabled?: boolean;
  readonly disabledReason?: string;
  readonly onConfirm: (reason: string) => Promise<string>;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const [reason, setReason] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [done, setDone] = React.useState<string | null>(null);
  const [pending, setPending] = React.useState(false);
  const id = React.useId();

  const onOpenChange = (next: boolean) => {
    setOpen(next);
    if (next) {
      setReason("");
      setError(null);
    }
  };

  const onSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (reason.trim() === "") {
      setError("Give a reason. It is recorded in the audit log.");
      return;
    }
    setPending(true);
    setError(null);
    try {
      setDone(await onConfirm(reason.trim()));
      setOpen(false);
    } catch (cause) {
      setError(isPlanned(cause) ? cause.message : cause instanceof Error ? cause.message : String(cause));
    } finally {
      setPending(false);
    }
  };

  return (
    <span className="inline-flex flex-col items-end gap-1">
      <Dialog open={open} onOpenChange={onOpenChange}>
        <Button
          size="sm"
          variant={destructive ? "destructive-outline" : "outline"}
          disabled={disabled}
          title={disabled ? disabledReason : undefined}
          onClick={() => onOpenChange(true)}
        >
          {trigger}
        </Button>
        <DialogPopup>
          <form className="flex min-h-0 flex-1 flex-col" onSubmit={onSubmit} noValidate>
            <DialogHeader>
              <DialogTitle>{title}</DialogTitle>
              <DialogDescription>{description}</DialogDescription>
            </DialogHeader>
            <DialogPanel>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={`${id}-reason`}>Reason</Label>
                <Textarea
                  id={`${id}-reason`}
                  required
                  maxLength={500}
                  value={reason}
                  onChange={(event) => setReason(event.currentTarget.value)}
                />
                <ControlError message={error} />
              </div>
            </DialogPanel>
            <DialogFooter>
              <DialogClose render={<Button variant="outline">Cancel</Button>} />
              <Button type="submit" variant={destructive ? "destructive" : "default"} disabled={pending}>
                {confirm}
              </Button>
            </DialogFooter>
          </form>
        </DialogPopup>
      </Dialog>
      {done === null ? null : (
        <span role="status" className="text-xs text-muted-foreground">
          {done}
        </span>
      )}
    </span>
  );
}
