import { UnlockIcon } from "lucide-react";
import React from "react";

import type { NativeDependency } from "../../../contracts/work.ts";
import { Badge } from "../../../components/ui/badge.tsx";
import { Popover, PopoverPopup, PopoverTrigger } from "../../../components/ui/popover.tsx";
import type { WorkItemView } from "../lib/model.ts";
import { dependencyLabel } from "../lib/dependencies.ts";
import { useNow } from "../lib/useWork.ts";
import { Pill } from "./Pill.tsx";
import { StateGlyph } from "./issueGlyphs.tsx";

export function DependencyChips({ links, showRunning = true }: { links: readonly NativeDependency[]; showRunning?: boolean }): React.ReactElement {
  const now = useNow();
  return <div className="flex flex-wrap gap-1.5">
    {links.map((link) => <Badge key={link.work_item_id} variant="outline" size="sm" className="max-w-full gap-1.5"
      title={`${dependencyLabel(link)} · ${link.state}`}
      render={<a href={link.web_url ?? undefined} onClick={(event) => event.stopPropagation()} />}>
      <StateGlyph category={link.terminal ? "completed" : link.started_at || link.dispatchable === false ? "started" : "unstarted"} />
      <span className="max-w-52 truncate">{dependencyLabel(link)}</span>
      {showRunning && link.started_at ? <span className="text-muted-foreground">running {Math.max(0, Math.floor((now - Date.parse(link.started_at)) / 60_000))}m</span> : null}
    </Badge>)}
  </div>;
}

export function DependencyPopover({ item, kind, children, enabled = true }: {
  item: WorkItemView; kind: "blocked" | "unblocks"; children: React.ReactElement; enabled?: boolean;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const links = kind === "blocked" ? (item.blockers ?? []).filter((link) => !link.terminal) : item.dependents ?? [];
  const reason = kind === "blocked" ? item.blockedReason : item.unblocksReason;
  if (!enabled) return children;
  return <Popover open={open} onOpenChange={setOpen}>
    <PopoverTrigger openOnHover onFocus={() => setOpen(true)} className="rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring" render={<button type="button" />}>
      {children}
    </PopoverTrigger>
    <PopoverPopup side="top" align="start" width="md" className="space-y-2">
      {reason ? <p className="text-sm">{reason}</p> : null}
      <DependencyChips links={links} />
    </PopoverPopup>
  </Popover>;
}

export function DependencyPanels({ item, queued }: { item: WorkItemView; queued: boolean }): React.ReactElement | null {
  const blockers = item.blockers ?? [];
  const open = blockers.filter((link) => !link.terminal);
  const landed = blockers.filter((link) => link.terminal);
  const dependents = item.dependents ?? [];
  const latest = landed.toSorted((a, b) => (a.closed_at ?? "").localeCompare(b.closed_at ?? "")).at(-1);
  if (open.length === 0 && dependents.length === 0 && (!queued || item.dependencyReady === false || latest === undefined)) return null;
  return <div className="space-y-3">
    {open.length > 0 ? <section aria-label="Blocked" className="space-y-2 rounded-lg border border-border bg-card p-3">
      <div className="flex flex-wrap items-center gap-2"><Pill tone="err">Blocked</Pill>{item.blockedReason ? <p className="text-sm">{item.blockedReason}</p> : null}</div>
      <DependencyChips links={open} />
    </section> : queued && item.dependencyReady !== false && landed.length > 0 ? <section aria-label="Queued" className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-card p-3 text-sm">
      <Pill tone="warn">Queued</Pill><p>Unblocked when <a className="text-primary hover:underline" href={latest!.web_url}>{dependencyLabel(latest!)}</a> landed; waiting to start.</p>
    </section> : null}
    {dependents.length > 0 ? <section aria-label="Unblocks" className="space-y-2 rounded-lg border border-border bg-card p-3">
      <div className="flex flex-wrap items-center gap-2"><Pill tone="info"><UnlockIcon aria-hidden />Unblocks {dependents.length}</Pill>{item.unblocksReason ? <p className="text-sm">{item.unblocksReason}</p> : null}</div>
      <DependencyChips links={dependents} />
    </section> : null}
  </div>;
}
