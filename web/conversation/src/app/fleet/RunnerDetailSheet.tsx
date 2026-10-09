import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Radio, RadioGroup } from "../../components/ui/radio-group.tsx";
import { Sheet, SheetClose, SheetDescription, SheetFooter, SheetHeader, SheetPopup, SheetTitle } from "../../components/ui/sheet.tsx";
import type { FleetRunner, RunnerRouting } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { PathValue } from "../account/controls.tsx";
import { RUNNER_UPGRADE_COMMAND } from "../lib/detentUpdates.ts";
import { AccountError } from "../account/api.ts";
import { formatLocalTime, formatRelativeTime } from "./format.ts";
import { parseRunnerWindow, serializeRunnerWindow, type RunnerHours } from "./runnerSchedule.ts";
import { runnerAccessOptions } from "./runnerAccess.ts";

export interface RunnerProject {
  readonly id: string;
  readonly name: string;
}

const control = "w-full min-w-0 rounded-md border border-border bg-background px-3 py-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring";
const days = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
const dayRanges = days.flatMap((from) => days.map((until) => `${from}-${until}`));
const takingWork = [["active", "Active", "Takes new work"], ["draining", "Draining", "Finishes what it has"], ["disabled", "Disabled", "Takes nothing"]] as const;

function Section({ title, children }: { readonly title: string; readonly children: React.ReactNode }): React.ReactElement {
  return <section className="space-y-3"><h3 className="text-sm font-medium">{title}</h3>{children}</section>;
}

function runnerRouting(runner: FleetRunner): RunnerRouting {
  return runner.routing ?? {
    display_name: runner.display_name, state: runner.state, capacity_limit: runner.capacity_limit,
    project_ids: [], tags: [],
    isolation_tier: runner.isolation_tier ?? "sandbox", host_services: [],
    availability: runner.availability ?? { timezone: "", windows: [], hard_deadline: "" },
  };
}

export function RunnerDetailSheet({ runner, projects, editable, now, onClose, onSave, onReload }: {
  readonly runner: FleetRunner;
  readonly projects: readonly RunnerProject[];
  readonly editable: boolean;
  readonly now?: number;
  readonly onClose: () => void;
  readonly onSave?: (runner: FleetRunner, routing: RunnerRouting) => Promise<void>;
  readonly onReload?: () => Promise<void>;
}): React.ReactElement {
  const [draft, setDraft] = React.useState(() => runnerRouting(runner));
  const [hours, setHours] = React.useState<readonly RunnerHours[]>(() => draft.availability.windows.map(parseRunnerWindow));
  const [scheduled, setScheduled] = React.useState(draft.availability.windows.length > 0);
  const [tag, setTag] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const canEdit = editable && runner.routing !== undefined && onSave !== undefined;
  const savedRouting = runnerRouting(runner);
  const knownIds = new Set(projects.map((project) => project.id));
  const unknownIds = [...new Set([...savedRouting.project_ids, ...draft.project_ids])].filter((id) => !knownIds.has(id));
  const choices = [...projects, ...unknownIds.map((id) => ({ id, name: id }))];
  const timezoneOptions = React.useMemo(() => [...new Set(["", "UTC", draft.availability.timezone, ...Intl.supportedValuesOf("timeZone")])], [draft.availability.timezone]);

  React.useEffect(() => {
    const next = runnerRouting(runner);
    setDraft(next);
    setHours(next.availability.windows.map(parseRunnerWindow));
    setScheduled(next.availability.windows.length > 0);
    setTag("");
  }, [runner.id, runner.revision]);

  function update(next: Partial<RunnerRouting>): void {
    setDraft((current) => ({ ...current, ...next }));
  }
  function toggleProject(id: string, checked: boolean): void {
    update({ project_ids: checked ? [...new Set([...draft.project_ids, id])] : draft.project_ids.filter((value) => value !== id) });
  }
  function addTags(): readonly string[] {
    const tags = [...new Set([...draft.tags, ...tag.split(/[\s,]+/).filter(Boolean)])];
    update({ tags });
    setTag("");
    return tags;
  }
  function changeHours(index: number, next: Partial<RunnerHours>): void {
    setHours((current) => current.map((row, i) => i === index ? { ...row, ...next } : row));
  }
  async function submit(isolationTier = draft.isolation_tier): Promise<void> {
    if (!canEdit || saving || onSave === undefined) return;
    const next: RunnerRouting = {
      ...draft, isolation_tier: isolationTier, tags: addTags(), host_services: draft.host_services.map((value) => value.trim()).filter(Boolean),
      availability: { ...draft.availability, windows: scheduled ? hours.map(serializeRunnerWindow) : [], hard_deadline: scheduled ? draft.availability.hard_deadline : "" },
    };
    setSaving(true);
    setError("");
    try {
      await onSave(runner, next);
      onClose();
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict && onReload !== undefined) {
        try {
          await onReload();
          setError("This runner changed while you were editing. Its settings have been reloaded. Review them and save again.");
        } catch (reloadCause) {
          setError(`This runner changed while you were editing. Could not reload it: ${reloadCause instanceof Error ? reloadCause.message : "Try again."}`);
        }
      } else setError(cause instanceof Error ? cause.message : "Runner settings could not be saved.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Sheet open onOpenChange={(open) => { if (!open && !saving) onClose(); }}>
      <SheetPopup className="h-dvh w-full max-w-none sm:max-w-xl">
        <SheetHeader className="border-b border-border/60 pr-12">
          <SheetTitle className="flex items-center gap-2 break-words text-lg">
            <span aria-hidden="true" className={cn("size-2 shrink-0 rounded-full", runner.health === "healthy" ? "bg-success" : runner.health === "needs_attention" ? "bg-warning" : "bg-muted-foreground")} />
            {runner.display_name}
          </SheetTitle>
          <SheetDescription>{runner.hostname}, {runner.os} {runner.architecture}{runner.version ? `, Detent ${runner.version}` : ""}</SheetDescription>
          <p className="text-xs text-muted-foreground">Last check-in <time dateTime={runner.last_heartbeat_at}>{formatRelativeTime(runner.last_heartbeat_at, now)}</time></p>
        </SheetHeader>
        <form className="flex min-h-0 flex-1 flex-col" onSubmit={(event) => { event.preventDefault(); void submit(); }}>
          <div className="min-h-0 flex-1 space-y-7 overflow-y-auto p-6 text-sm">
            {runner.claim_refusal_reason ? <div role="alert" data-testid="host-update" className="space-y-2 rounded-lg border border-warning/30 bg-warning/8 p-3">
              <p>{runner.claim_refusal_reason}</p><PathValue value={RUNNER_UPGRADE_COMMAND} />
            </div> : null}
            {runner.sprite?.wake_failed || (runner.sprite && !runner.sprite.can_wake) ? <p role="alert" className="text-warning-foreground">{runner.sprite.wake_failed ? "The Hub could not wake this Sprite. Check the Sprite and its project’s Sprites token." : "No Sprites token is set for an accessible project; the Hub cannot wake this Sprite."}</p> : null}
            {runner.problems?.map((problem, index) => (
              <div key={`${problem.code}-${index}`} role="alert" className="space-y-2 rounded-lg border border-warning/30 bg-warning/8 p-3">
                <p className="font-medium">{problem.message}</p><p>{problem.fix_hint}</p>
                {problem.code === "tier_unavailable" && canEdit ? runnerAccessOptions.filter(({ tier }) => tier !== savedRouting.isolation_tier && Object.keys(runner.backend_isolation ?? {}).length > 0 && Object.values(runner.backend_isolation ?? {}).every((tiers) => tiers.includes(tier))).map(({ tier, label }) => (
                  <Button key={tier} type="button" variant="outline" size="sm" disabled={saving} onClick={() => void submit(tier)}>Switch to {label}</Button>
                )) : null}
                <p className="text-xs text-muted-foreground">Seen since <time dateTime={problem.first_seen}>{formatLocalTime(problem.first_seen)}</time></p>
              </div>
            ))}
            <fieldset disabled={saving} className="min-w-0 space-y-7">
              <Section title="Taking work">
                {canEdit ? (
                  <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
                    {takingWork.map(([state, name, hint]) => (
                      <label key={state} className={cn("flex cursor-pointer items-start gap-2 rounded-lg border p-3", draft.state === state ? "border-primary bg-primary/5" : "border-border")}>
                        <input type="radio" name="state" value={state} checked={draft.state === state} onChange={() => update({ state })} className="mt-1" />
                        <span><span className="block font-medium">{name}</span><span className="text-xs text-muted-foreground">{hint}</span></span>
                      </label>
                    ))}
                  </div>
                ) : <p>{takingWork.find(([state]) => state === draft.state)?.slice(1).join(" · ") ?? draft.state}</p>}
              </Section>
              <Section title="Capacity">
                {canEdit ? (
                  <div className="flex items-center gap-2">
                    <label htmlFor="runner-jobs" className="flex-1">Jobs at once</label>
                    <Button aria-label="Decrease jobs at once" type="button" size="icon-sm" variant="outline" disabled={draft.capacity_limit <= 0} onClick={() => update({ capacity_limit: Math.max(0, draft.capacity_limit - 1) })}>−</Button>
                    <input id="runner-jobs" className={cn(control, "w-24 text-center")} type="number" min={0} max={10000} step={1} required value={draft.capacity_limit} onChange={(event) => update({ capacity_limit: Number(event.target.value) })} />
                    <Button aria-label="Increase jobs at once" type="button" size="icon-sm" variant="outline" disabled={draft.capacity_limit >= 10000} onClick={() => update({ capacity_limit: Math.min(10000, draft.capacity_limit + 1) })}>+</Button>
                  </div>
                ) : <p>Jobs at once: {draft.capacity_limit}</p>}
                <p className="text-xs text-muted-foreground">The runner reports room for {runner.reported_capacity}. The lower of the two wins, and runners on the same machine share its slots. 0 stops new work.</p>
              </Section>
              <Section title="Allowed projects">
                <div className="divide-y divide-border/50 rounded-lg border border-border/60">
                  {choices.filter((project) => canEdit || draft.project_ids.includes(project.id)).map((project) => (
                    <div key={project.id} className="min-w-0 space-y-2 p-3">
                      {canEdit ? <>
                        <label className="flex min-w-0 items-center gap-2"><input type="checkbox" disabled={runner.can_edit_projects !== true} checked={draft.project_ids.includes(project.id)} onChange={(event) => toggleProject(project.id, event.target.checked)} /><span className="break-all">{project.name}</span></label>
                      </> : <><span className="break-all">{project.name}</span></>}
                      {runner.project_checkouts?.[project.id] ? <div className="space-y-1 text-xs text-muted-foreground">
                        <p>{({ ready: "Checkout ready", missing: "Checkout unavailable", setup_failed: "Checkout setup failed", preparing: "Preparing checkout" } as Record<string, string>)[runner.project_checkouts[project.id]!.status] ?? "Preparing checkout"}</p>
                        {runner.project_checkouts[project.id]!.message ? <p className="break-words">{runner.project_checkouts[project.id]!.message}</p> : null}
                        {runner.project_checkouts[project.id]!.fix_command ? <PathValue value={runner.project_checkouts[project.id]!.fix_command!} /> : null}
                        {runner.project_checkouts[project.id]!.status === "missing" ? <p>The runner retries automatically after repository access is fixed.</p> : null}
                      </div> : null}
                    </div>
                  ))}
                </div>
                {!canEdit && runner.routing === undefined ? <p className="text-xs text-muted-foreground">Project access is not reported.</p> : null}
              </Section>
              <Section title="Tags">
                <div className="flex flex-wrap gap-2">
                  {draft.tags.map((value) => (
                    <span key={value} className="inline-flex max-w-full items-center gap-1 rounded-full border border-border bg-muted px-2 py-1 text-xs">
                      <span className="break-all">{value}</span>
                      {canEdit ? <button type="button" aria-label={`Remove tag ${value}`} onClick={() => update({ tags: draft.tags.filter((old) => old !== value) })} className="rounded-full px-1 focus-visible:outline-2 focus-visible:outline-ring">×</button> : null}
                    </span>
                  ))}
                </div>
                {canEdit ? <div className="flex gap-2"><input aria-label="Add tag" className={control} value={tag} onChange={(event) => setTag(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter" || event.key === ",") { event.preventDefault(); addTags(); } }} /><Button type="button" variant="outline" onClick={addTags}>Add</Button></div> : draft.tags.length === 0 ? <p className="text-muted-foreground">{runner.routing === undefined ? "Tags are not reported." : "No tags"}</p> : null}
                <p className="text-xs text-muted-foreground">Repository policy can require tags. Tags pick from allowed runners; they never grant a project.</p>
              </Section>
              <Section title="Schedule">
                {canEdit ? (
                  <select aria-label="Availability" className={control} value={scheduled ? "hours" : "always"} onChange={(event) => { const only = event.target.value === "hours"; setScheduled(only); if (only && hours.length === 0) setHours([{ days: "Mon-Fri", from: "09:00", until: "17:00" }]); }}>
                    <option value="always">Always available</option><option value="hours">Only during these hours</option>
                  </select>
                ) : <p>{scheduled ? "Only during these hours" : "Always available"}</p>}
                {scheduled ? <>
                  {canEdit ? <>
                    <div className="space-y-3">{hours.map((row, index) => (
                      <div key={index} className="grid grid-cols-2 gap-2 rounded-lg border border-border/60 p-3">
                        <label className="col-span-2">Day range<select aria-label={`Day range ${index + 1}`} className={control} value={row.days} onChange={(event) => changeHours(index, { days: event.target.value })}>{dayRanges.map((range) => <option key={range}>{range}</option>)}</select></label>
                        <label>From<input aria-label={`From ${index + 1}`} className={control} type="time" required value={row.from} onChange={(event) => changeHours(index, { from: event.target.value })} /></label>
                        <label>Until<input aria-label={`Until ${index + 1}`} className={control} required pattern="([01][0-9]|2[0-3]):[0-5][0-9]|24:00" placeholder="17:00" value={row.until} onChange={(event) => changeHours(index, { until: event.target.value })} /></label>
                        <Button className="col-span-2 w-fit" type="button" variant="ghost" size="sm" aria-label={`Remove hours ${index + 1}`} onClick={() => setHours((current) => current.filter((_, i) => i !== index))}>Remove hours</Button>
                      </div>
                    ))}</div>
                    <Button type="button" variant="outline" size="sm" disabled={hours.length >= 64} onClick={() => setHours((current) => [...current, { days: "Mon-Fri", from: "09:00", until: "17:00" }])}>Add hours</Button>
                  </> : <ul className="space-y-1">{draft.availability.windows.map((value, index) => <li key={index}>{value}</li>)}</ul>}
                  {canEdit ? <label className="block">Time zone<select className={control} value={draft.availability.timezone} onChange={(event) => update({ availability: { ...draft.availability, timezone: event.target.value } })}>{timezoneOptions.map((zone) => <option key={zone} value={zone}>{zone || "Runner's local time zone"}</option>)}</select></label> : <p className="text-muted-foreground">{draft.availability.timezone || "Runner's local time zone"}</p>}
                  {canEdit ? <label className="block">Stop running work <input aria-label="Stop running work after hours end" className={cn(control, "my-1")} value={draft.availability.hard_deadline} placeholder="30m" onChange={(event) => update({ availability: { ...draft.availability, hard_deadline: event.target.value } })} /> after hours end</label> : draft.availability.hard_deadline ? <p>Stop running work {draft.availability.hard_deadline} after hours end</p> : null}
                </> : null}

              </Section>
              <Section title="Agent access">
                {canEdit ? <RadioGroup aria-label="Agent access" value={draft.isolation_tier} onValueChange={(value) => update({ isolation_tier: String(value) })}>{runnerAccessOptions.map(({ tier, label, hint }) => (
                  <label key={tier} className="flex cursor-pointer items-start gap-2">
                    <Radio value={tier} aria-labelledby={`runner-access-${tier}`} aria-describedby={`runner-access-${tier}-hint`} className="mt-1" />
                    <span><span id={`runner-access-${tier}`} className="block font-medium">{label}</span><span id={`runner-access-${tier}-hint`} className="text-xs text-muted-foreground">{hint}</span></span>
                  </label>
                ))}</RadioGroup> : <p>{runnerAccessOptions.find(({ tier }) => tier === draft.isolation_tier)?.label ?? draft.isolation_tier}</p>}
                <p className="text-xs text-muted-foreground">Changes apply on the runner’s next configuration refresh; no re-enrollment is needed.</p>
                {canEdit ? <label className="block">Host services the sandbox may reach<textarea className={cn(control, "mt-1")} rows={2} value={draft.host_services.join("\n")} onChange={(event) => update({ host_services: event.target.value.split("\n") })} placeholder="tcp:127.0.0.1:8080" /></label> : <div><p>Host services the sandbox may reach</p>{runner.routing === undefined ? <p className="text-muted-foreground">Host services are not reported.</p> : <ul className="text-muted-foreground">{draft.host_services.map((value) => <li key={value} className="break-all">{value}</li>)}</ul>}</div>}
              </Section>
            </fieldset>
            <Section title="Running work"><ul className="space-y-2">{runner.leases.map((lease) => <li key={lease.lease_id} className="break-words"><span className="font-medium">#{lease.work_item_id}</span> {lease.title}</li>)}</ul>{runner.leases.length === 0 ? <p className="text-muted-foreground">No running work</p> : null}</Section>
            <Section title="Provider accounts"><ul className="space-y-2 text-xs text-muted-foreground">{runner.provider_capacity.map((capacity) => <li key={`${capacity.provider}-${capacity.account_alias}`} className="break-words">{capacity.provider} · {capacity.account_alias} · {capacity.availability} · {capacity.state} · {capacity.used} of {capacity.max_concurrent}</li>)}</ul></Section>
          </div>
          {canEdit ? (
            <SheetFooter className="block space-y-3">
              {error ? <p role="alert" className="text-sm text-destructive">{error}</p> : null}
              <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
                <SheetClose render={<Button type="button" variant="outline" disabled={saving}>Cancel</Button>} />
                <Button type="submit" disabled={saving || (scheduled && hours.length === 0)}>{saving ? "Saving…" : "Save runner"}</Button>
              </div>
            </SheetFooter>
          ) : null}
        </form>
      </SheetPopup>
    </Sheet>
  );
}
