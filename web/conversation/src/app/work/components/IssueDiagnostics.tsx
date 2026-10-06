import React from "react";
import { Tabs } from "@base-ui/react/tabs";
import { Badge } from "../../../components/ui/badge.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { toggleVariants } from "../../../components/ui/toggle.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../../components/ui/table.tsx";
import type { IssueExplanation } from "../../../contracts/diagnostics.ts";
import type { CollaborationEvent, NativeAttempt, NativeIssue } from "../../../contracts/work.ts";
import { attemptIdentityLabel, attemptStage, diagnosticsVerdict, durationLabel, failureText, groupAttempts, laneClock } from "../lib/diagnostics.ts";
import { useWorkHttp } from "../lib/useWork.ts";

export function IssueDetailTabs({ native, diagnostics, children }: { readonly native: boolean; readonly diagnostics: React.ReactNode; readonly children: React.ReactNode }) {
  const [value, setValue] = React.useState("timeline");
  if (!native) return <>{children}</>;
  return <Tabs.Root value={value} onValueChange={setValue} className="min-w-0 space-y-5">
    <Tabs.List aria-label="Issue detail tabs" className="flex w-fit gap-0.5 rounded-lg bg-input/40 p-0.5">
      {["timeline", "diagnostics"].map((tab) => <Tabs.Tab key={tab} value={tab} data-pressed={value === tab ? "" : undefined} className={toggleVariants({ variant: "default", size: "segmented" })}>{tab === "timeline" ? "Timeline" : "Diagnostics"}</Tabs.Tab>)}
    </Tabs.List>
    <Tabs.Panel value="timeline" className="min-w-0 space-y-5">{children}</Tabs.Panel>
    <Tabs.Panel value="diagnostics" data-testid="issue-diagnostics-panel" className="min-w-0">{diagnostics}</Tabs.Panel>
  </Tabs.Root>;
}

function Kv({ name, children }: { readonly name: string; readonly children: React.ReactNode }) {
  return <div data-slot="kv" className="grid grid-cols-[minmax(0,1fr)_minmax(0,2fr)] gap-3 border-b border-border py-2 last:border-0">
    <dt className="text-muted-foreground">{name}</dt><dd className="min-w-0 break-words">{children}</dd>
  </div>;
}

function Section({ title, children }: { readonly title: string; readonly children: React.ReactNode }) {
  return <section className="min-w-0 space-y-3"><h3 className="text-sm font-medium">{title}</h3>{children}</section>;
}

function usageLabel(group: readonly NativeAttempt[], cost: boolean): string {
  if (group.some((attempt) => !attempt.usage?.length)) return "unavailable";
  const entries = group.flatMap((attempt) => attempt.usage ?? []);
  if (!cost) return entries.reduce((sum, usage) => sum + usage.input + usage.output, 0).toLocaleString();
  const currencies = new Set(entries.map((usage) => usage.currency));
  if (currencies.size !== 1 || entries.some((usage) => usage.cost_coverage === "unavailable" || usage.reported_cost_micros === undefined && usage.cost_estimate === 0 && usage.input + usage.output > 0)) return "unavailable";
  const estimated = entries.some((usage) => usage.reported_cost_micros === undefined);
  const total = entries.reduce((sum, usage) => sum + (usage.reported_cost_micros === undefined ? usage.cost_estimate : usage.reported_cost_micros / 1_000_000), 0);
  return `${estimated ? "~" : ""}${total.toFixed(4)} ${entries[0]!.currency}`;
}

const LANE_COLORS: Readonly<Record<string, string>> = {
  todo: "bg-muted-foreground", "in progress": "bg-info", rework: "bg-warning",
  blocked: "bg-destructive", "human review": "bg-warning", merging: "bg-primary", done: "bg-success",
};

export function IssueDiagnostics({ issue, attempts, history, now, attemptsAvailable = true, historyAvailable = true }: {
  readonly issue: NativeIssue;
  readonly attempts: readonly NativeAttempt[];
  readonly history: readonly CollaborationEvent[];
  readonly now: number;
  readonly attemptsAvailable?: boolean;
  readonly historyAvailable?: boolean;
}) {
  const http = useWorkHttp();
  const [explanation, setExplanation] = React.useState<IssueExplanation | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [retry, setRetry] = React.useState(0);
  const latest = groupAttempts(attempts).at(-1)?.at(-1);
  React.useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setExplanation(null);
    void http.getExplanation(issue.project_id, issue.work_item_id).then((result) => {
      if (!cancelled) setExplanation(result);
    }).catch(() => undefined).finally(() => {
      if (!cancelled) setLoading(false);
    });
    return () => { cancelled = true; };
  }, [http, issue.project_id, issue.work_item_id, issue.updated_at, latest?.updated_at, history.at(-1)?.aggregate_sequence, retry]);
  const verdict = diagnosticsVerdict(issue, explanation, latest);
  const clock = historyAvailable ? laneClock(history, attempts, now, issue.terminal) : null;
  const decision = explanation?.eligibility.current;
  const gate = explanation?.required_gate;
  const capacity = explanation?.native_runtime?.capacity;
  const activity = latest?.runtime?.activity;
  const breakdown = activity?.timing_summary?.breakdown;
  const humanAction = gate?.human_action || (latest?.disposition?.human_action && latest.disposition.status.toLowerCase() === issue.state.toLowerCase() ? latest.disposition.final_summary || "Human action requested; details unavailable" : "None recorded");
  const groups = groupAttempts(attempts).toReversed();
  const sources = [
    ...(explanation?.sources ?? [{ name: "explanation", state: "unavailable" }]),
    { name: "attempts", state: attemptsAvailable ? "available" : "unavailable" },
    { name: "history", state: historyAvailable ? "available" : "unavailable" },
    { name: "usage", state: attemptsAvailable && attempts.some((attempt) => attempt.usage?.length) ? "available" : "unavailable" },
    { name: "activity", state: activity === undefined ? "unavailable" : latest?.runtime_freshness || "available" },
    { name: "capacity_at_evaluation", state: "unavailable" },
  ];

  return <div data-testid="issue-diagnostics" className="min-w-0 space-y-6 text-sm">
    <Section title="Verdict">
      {loading ? <p className="text-muted-foreground" role="status">Loading diagnostics…</p> : <>
        <Badge variant={verdict.variant}>{issue.state}</Badge>
        <p>{verdict.text}</p><p className="text-muted-foreground">{verdict.action}</p>
        {explanation === null ? <Button size="compact" variant="outline" onClick={() => setRetry((value) => value + 1)}>Retry diagnostics</Button> : null}
      </>}
    </Section>
    <Section title="Clock">
      <p className="text-xs text-muted-foreground">From the first Todo entry; Backlog excluded.</p>
      {clock === null ? <p className="text-muted-foreground">{historyAvailable ? "No Todo transition recorded." : "History unavailable."}</p> : <>
        <div className="flex h-3 overflow-hidden rounded-sm bg-muted" role="img" aria-label={clock.lanes.map(({ lane, duration }) => `${lane}: ${durationLabel(duration)}`).join(", ")}>
          {clock.lanes.map(({ lane, duration }) => <span key={lane} className={LANE_COLORS[lane.toLowerCase()] || "bg-muted-foreground"} style={{ width: `${clock.total === 0 ? 0 : duration / clock.total * 100}%` }} />)}
        </div>
        <ul className="flex flex-wrap gap-x-4 gap-y-2 text-xs tabular-nums">
          {clock.lanes.map(({ lane, duration }) => <li key={lane} className="flex items-center gap-1.5"><span className={`size-2 rounded-sm ${LANE_COLORS[lane.toLowerCase()] || "bg-muted-foreground"}`} />{lane} {durationLabel(duration)}</li>)}
        </ul>
        <p className="text-xs text-muted-foreground tabular-nums">System {attemptsAvailable ? durationLabel(clock.system) : "unavailable"} · Working {attemptsAvailable ? durationLabel(clock.working) : "unavailable"} · {clock.transitions} transitions</p>
      </>}
    </Section>
    <Section title="Why it is not running">
      <dl className="text-xs">
        <Kv name="Eligibility">{explanation?.eligibility.source_state === "unavailable" && decision?.state === "unknown" ? "unavailable" : decision?.state || explanation?.eligibility.state || "unavailable"}{decision?.reason ? ` · ${decision.reason}` : ""}</Kv>
        <Kv name="Lane">{explanation?.current_lane.name || issue.state}</Kv>
        <Kv name="Last transition">{explanation?.latest_transition ? `${explanation.latest_transition.at} · ${explanation.latest_transition.reason || "Reason unavailable"}` : "unavailable"}</Kv>
        <Kv name="Capacity at evaluation">unavailable{capacity?.length ? <div className="mt-1 text-muted-foreground">Latest runner observations{capacity.map((entry) => <p key={entry.runner_id} className="tabular-nums">{entry.runner_id}: {entry.available} available · {entry.health} · observed {entry.observed_at}{entry.exclusions?.length ? ` · ${entry.exclusions.join(", ")}` : ""}</p>)}</div> : null}</Kv>
        <Kv name="Required gate">{gate?.source_state === "unavailable" || gate === undefined ? "unavailable" : `${gate.state}${gate.reason ? ` · ${gate.reason}` : ""}`}</Kv>
        <Kv name="Dependencies">{issue.dependencies.length ? issue.dependencies.map((id) => <p key={id}>{id} · {issue.blockers.find((dependency) => dependency.work_item_id === id)?.state || "State unavailable"}</p>) : "None recorded"}</Kv>
        <Kv name="Human action">{humanAction}</Kv>
      </dl>
    </Section>
    <Section title="Attempts">
      {!attemptsAvailable ? <p className="text-muted-foreground">Attempts unavailable.</p> : groups.length === 0 ? <p className="text-muted-foreground">No attempts yet.</p> : <Table tabIndex={0} aria-label="Attempts" className="tabular-nums outline-none focus-visible:ring-2 focus-visible:ring-ring">
        <TableHeader><TableRow>{["When", "Stage", "Model · effort", "Duration", "Outcome", "Tokens", "Cost"].map((label) => <TableHead key={label} className={label === "Tokens" || label === "Cost" || label === "Duration" ? "text-right" : undefined}>{label}</TableHead>)}</TableRow></TableHeader>
        <TableBody>{groups.map((group) => {
          const attempt = group[0]!;
          const models = [...new Set(group.map(attemptIdentityLabel))];
          return <TableRow key={attempt.attempt_id}>
            <TableCell className="whitespace-nowrap"><time dateTime={attempt.started_at}><span className="block">{attempt.started_at.slice(0, 10)}</span><span className="block text-muted-foreground">{attempt.started_at.slice(11, 19)} UTC</span></time>{group.length > 1 ? <time dateTime={group.at(-1)!.started_at} className="mt-1 block text-muted-foreground"><span className="block">through {group.at(-1)!.started_at.slice(0, 10)}</span>{group.at(-1)!.started_at.slice(11, 19)} UTC</time> : null}</TableCell>
            <TableCell>{attemptStage(attempt)}</TableCell><TableCell className="whitespace-normal">{models.join(", ")}</TableCell>
            <TableCell className="text-right whitespace-nowrap">{durationLabel(group.reduce((sum, entry) => sum + Math.max(0, (entry.status === "running" ? now : Date.parse(entry.updated_at)) - Date.parse(entry.started_at)), 0))}</TableCell>
            <TableCell className="w-32 whitespace-normal"><Badge variant={attempt.status === "succeeded" ? "success" : attempt.status === "failed" ? "warning" : "secondary"}>{attempt.status}{group.length > 1 ? ` ×${group.length}` : ""}</Badge>{attempt.status === "failed" ? <p className="mt-1 break-words text-muted-foreground">{failureText(attempt) || "Failure detail unavailable"}</p> : null}</TableCell>
            <TableCell className="text-right whitespace-nowrap">{usageLabel(group, false)}</TableCell><TableCell className="text-right whitespace-nowrap">{usageLabel(group, true)}</TableCell>
          </TableRow>;
        })}</TableBody>
      </Table>}
    </Section>
    <Section title="Last attempt activity">
      {breakdown === undefined ? <p className="text-muted-foreground">Activity breakdown unavailable.</p> : <dl className="text-xs tabular-nums">
        <Kv name="Elapsed">{durationLabel(breakdown.elapsed_seconds * 1000)}</Kv>
        {Object.entries(breakdown.by_kind_seconds ?? {}).sort(([a], [b]) => a.localeCompare(b)).map(([kind, seconds]) => <Kv key={kind} name={kind.replaceAll("_", " ")}>{durationLabel(seconds * 1000)}</Kv>)}
        <Kv name="Observed">{durationLabel(breakdown.observed_seconds * 1000)}</Kv><Kv name="Unknown">{durationLabel(breakdown.unknown_seconds * 1000)}</Kv><Kv name="Concurrent">{durationLabel(breakdown.concurrent_seconds * 1000)}</Kv>
      </dl>}
      <h4 className="text-xs font-medium">Instruction snapshots</h4>
      {activity === undefined ? <p className="text-muted-foreground">Instruction snapshots unavailable.</p> : !activity.instructions?.length ? <p className="text-muted-foreground">No instruction snapshots recorded.</p> : <Table tabIndex={0} aria-label="Instruction snapshots" className="table-fixed outline-none focus-visible:ring-2 focus-visible:ring-ring"><TableHeader><TableRow><TableHead className="w-1/3">Name</TableHead><TableHead>SHA-256</TableHead></TableRow></TableHeader><TableBody>{activity.instructions.map((instruction, index) => <TableRow key={`${instruction.name}:${instruction.sha256}:${index}`}><TableCell className="whitespace-normal break-words">{instruction.name}</TableCell><TableCell className="whitespace-normal font-mono break-all">{instruction.sha256}</TableCell></TableRow>)}</TableBody></Table>}
    </Section>
    <Section title="Evidence">
      <dl className="text-xs">{sources.map((source, index) => <Kv key={`${source.name}:${index}`} name={source.name.replaceAll("_", " ")}><Badge variant={source.state === "available" || source.state === "live" ? "success" : "secondary"}>{source.state.replaceAll("_", " ")}</Badge>{"code" in source && source.code ? <span className="ml-2 text-muted-foreground">{source.code}</span> : null}</Kv>)}</dl>
    </Section>
  </div>;
}
