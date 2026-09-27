// One runner, as a card.
//
// `host_capacity`/`host_used` are the shared machine's numbers and
// `capacity_limit`/`reported_capacity` the runner's own, so the card shows the
// meter for the host and names the runner's limit beside it rather than
// averaging two different facts into one bar.
import { DownloadIcon, Monitor } from "lucide-react";
import React from "react";

import { cn } from "../../lib/utils.ts";
import type { FleetRunner } from "../../contracts/account.ts";
import { PathValue } from "../account/controls.tsx";
import { RUNNER_UPGRADE_COMMAND, runnerUpdateLabel } from "../lib/detentUpdates.ts";
import { formatLocalTime, formatRelativeTime } from "./format.ts";

/** Healthy pulses; anything degraded is amber; anything unknown is inert. */
function healthTone(health: string): string {
  if (health === "healthy") return "bg-success motion-safe:animate-status-pulse";
  if (health === "stale" || health === "paused" || health === "degraded") return "bg-warning";
  return "bg-muted-foreground";
}

function Field({
  label,
  children,
}: {
  readonly label: string;
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <div>
      {label}
      <b className="block text-[13px] font-medium tabular-nums text-foreground">{children}</b>
    </div>
  );
}

/**
 * Whether this host is not on the hub's build. The footer's pill counts the
 * same runners through `GET /app/updates`, where the hub does the comparison
 * itself; here there are two version strings and nothing that knows how to
 * order them, so the rule is the conservative half of the hub's: a difference
 * between two versions that both exist.
 */
export function hostIsBehind(reported: string, current: string): boolean {
  const from = reported.trim();
  const to = current.trim();
  if (from === "" || to === "" || from === "dev" || to === "dev") return false;
  return from !== to;
}

export function HostCard({
  runner,
  now,
  current = "",
}: {
  readonly runner: FleetRunner;
  readonly now?: number;
  /** The hub's own build, which is the version a runner should be on. */
  readonly current?: string;
}): React.ReactElement {
  const behind = hostIsBehind(runner.version ?? "", current);
  const percent =
    runner.host_capacity > 0
      ? Math.min(100, Math.round((runner.host_used / runner.host_capacity) * 100))
      : 0;
  return (
    <article
      data-testid="host-card"
      className="flex flex-col gap-2.5 rounded-xl border border-border/60 bg-card/40 p-4"
    >
      <div className="flex items-start gap-2">
        <span
          aria-hidden="true"
          className={cn("mt-1.5 size-2 shrink-0 rounded-full", healthTone(runner.health))}
        />
        <Monitor aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-medium">{runner.display_name}</div>
          <div className="text-xs text-muted-foreground">{runner.health}</div>
        </div>
        <div className="text-right text-xs text-muted-foreground">
          {runner.state} · {runner.hostname} · {runner.os}/{runner.architecture}
        </div>
      </div>

      <div className="grid grid-cols-3 gap-2 text-xs text-muted-foreground">
        <Field label="Slots">
          {runner.host_used} / {runner.host_capacity} in use
        </Field>
        <Field label="Runner limit">
          {runner.capacity_limit}
          {runner.reported_capacity !== runner.capacity_limit
            ? ` (reports ${runner.reported_capacity})`
            : ""}
        </Field>
        <Field label="Last heartbeat">{formatRelativeTime(runner.last_heartbeat_at, now)}</Field>
      </div>

      {behind ? (
        // Where the footer's update pill sends a reader. Nothing here can
        // upgrade the host — it is somebody else's machine — so the row says
        // which build it is on, which build it should be on, and hands over
        // the one command that closes the gap.
        <div
          data-testid="host-update"
          className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-warning/40 bg-warning/8 px-2.5 py-2 text-xs"
        >
          <DownloadIcon aria-hidden="true" className="size-3.5 shrink-0 text-warning" />
          <span className="font-medium text-foreground">
            {runnerUpdateLabel(runner.version ?? "", current)}
          </span>
          <span className="ml-auto">
            <PathValue value={RUNNER_UPGRADE_COMMAND} />
          </span>
        </div>
      ) : null}

      <div
        role="progressbar"
        aria-label={`Slots in use on ${runner.display_name}`}
        aria-valuenow={runner.host_used}
        aria-valuemin={0}
        aria-valuemax={runner.host_capacity}
        className="h-[5px] overflow-hidden rounded-[3px] bg-accent"
      >
        <i className="block h-full bg-primary" style={{ width: `${percent}%` }} />
      </div>

      {runner.leases.length > 0 ? (
        <ul className="flex flex-col gap-1 text-xs text-muted-foreground">
          {runner.leases.map((lease) => (
            <li key={lease.lease_id} className="flex min-w-0 items-baseline gap-1.5">
              <span className="shrink-0 font-medium tabular-nums text-foreground">
                #{lease.work_item_id}
              </span>
              <span className="truncate">{lease.title}</span>
              <span className="ml-auto shrink-0">expires {formatLocalTime(lease.expires_at)}</span>
            </li>
          ))}
        </ul>
      ) : null}

      {runner.provider_capacity.length > 0 ? (
        <ul className="flex flex-col gap-1 text-xs text-muted-foreground">
          {runner.provider_capacity.map((capacity) => (
            <li
              key={`${capacity.provider}-${capacity.account_alias}`}
              className="flex min-w-0 items-baseline gap-1.5"
            >
              <span className="shrink-0 text-foreground">{capacity.provider}</span>
              <span className="truncate">{capacity.account_alias}</span>
              <span className="ml-auto shrink-0 tabular-nums">
                {capacity.availability} · {capacity.state} · {capacity.used}/
                {capacity.max_concurrent}
              </span>
            </li>
          ))}
        </ul>
      ) : null}
    </article>
  );
}
