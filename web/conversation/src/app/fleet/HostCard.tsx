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
import { RUNNER_UPGRADE_COMMAND } from "../lib/detentUpdates.ts";
import { SettingsHelp } from "../settings/SettingsHelp.tsx";
import { RUNNER_HELP } from "./runnerHelp.ts";
import { formatLocalTime, formatRelativeTime } from "./format.ts";

function healthTone(health: string): string {
  if (health === "healthy") return "bg-success motion-safe:animate-status-pulse";
  if (health === "needs_attention") return "bg-warning";
  return "bg-muted-foreground";
}

function Field({
  label,
  help,
  children,
}: {
  readonly label: string;
  readonly help?: { readonly label: string; readonly text: string };
  readonly children: React.ReactNode;
}): React.ReactElement {
  return (
    <div>
      <div className="flex flex-wrap items-center gap-1">
        {label}
        {help ? <SettingsHelp label={help.label}>{help.text}</SettingsHelp> : null}
      </div>
      <b className="block text-[13px] font-medium tabular-nums text-foreground">{children}</b>
    </div>
  );
}

export function HostCard({
  runner,
  now,
  settings,
}: {
  readonly runner: FleetRunner;
  readonly now?: number;
  readonly settings?: React.ReactNode;
}): React.ReactElement {
  const refusal = runner.claim_refusal_reason ?? "";
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
          <div className={cn("text-xs text-muted-foreground", runner.health === "needs_attention" && "text-warning")}>
            {runner.health === "needs_attention" ? <span className="rounded border border-warning/40 px-1.5">Needs attention</span> : runner.health === "outside_hours" ? "Outside hours" : runner.health}
          </div>
        </div>
        <div className="text-right text-xs text-muted-foreground">
          {runner.state} · {runner.hostname} · {runner.os}/{runner.architecture}
        </div>
      </div>

      {(runner.problems?.length ?? 0) > 0 ? (
        <ul aria-label="Runner problems" role={runner.health === "needs_attention" ? "alert" : undefined} className="space-y-2 text-xs">
          {runner.problems?.map((problem) => (
            <li key={problem.code} data-problem={problem.code}>
              <p className="font-medium">{problem.message}</p>
              <p className="text-muted-foreground">{problem.fix_hint}</p>
            </li>
          ))}
        </ul>
      ) : null}

      <div className="grid grid-cols-3 gap-2 text-xs text-muted-foreground">
        <Field label="Slots" help={{ label: "Shared host capacity", text: RUNNER_HELP.host }}>
          {runner.host_used} / {runner.host_capacity} in use
        </Field>
        <Field label="Runner limit" help={{ label: "Runner limit and reported capacity", text: RUNNER_HELP.reported }}>
          {runner.capacity_limit}
          {runner.reported_capacity !== runner.capacity_limit
            ? ` (reports ${runner.reported_capacity})`
            : ""}
        </Field>
        <Field label="Last heartbeat">{formatRelativeTime(runner.last_heartbeat_at, now)}</Field>
      </div>

      {runner.isolation_tier === undefined ? null : (
        <div className="text-xs text-muted-foreground">
          {`Isolation setting: ${runner.isolation_tier === "native-trusted" ? "Trusted only: full host access" : "Sandbox"}`}
          {runner.availability?.windows.length
            ? ` · ${runner.availability.windows.join(", ")} (${runner.availability.timezone})`
            : " · Always available"}
        </div>
      )}

      {(runner.home_project_ids?.length ?? 0) > 0 ? (
        <div className="text-xs text-muted-foreground" data-testid="home-status">
          <span>Home projects: {runner.home_project_ids?.join(", ")}</span>
          {runner.home_status ? <span className="block">{runner.home_status}</span> : null}
        </div>
      ) : null}

      {refusal !== "" ? (
        <div
          data-testid="host-update"
          className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-warning/40 bg-warning/8 px-2.5 py-2 text-xs"
        >
          <DownloadIcon aria-hidden="true" className="size-3.5 shrink-0 text-warning" />
          <span className="font-medium text-foreground">
            {refusal}
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
              <SettingsHelp label={`${capacity.provider} account capacity`}>{RUNNER_HELP.provider}</SettingsHelp>
              <span className="truncate">{capacity.account_alias}</span>
              <span className="ml-auto shrink-0 tabular-nums">
                {capacity.availability} · {capacity.state} · {capacity.used}/
                {capacity.max_concurrent}
              </span>
            </li>
          ))}
        </ul>
      ) : null}
      {settings}
    </article>
  );
}
