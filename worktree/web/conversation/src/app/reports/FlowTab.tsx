import React from "react";

import { Chart, ChartLegend, ChartPanel } from "../../components/ui/chart.tsx";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import type { ReportsReport } from "../../contracts/reports.ts";
import { formatCount } from "../usage/usageFormat.ts";
import {
  bucketCategories,
  duration,
  durationDelta,
  HELD_LANES,
  hoursLabel,
  laneColor,
  percentText,
  reportBuckets,
  type ReportsSearch,
} from "./model.ts";
import { ageTone, openWork } from "./OverviewTab.tsx";
import { Dash, IssueLink, LaneBadge, Muted, ShowMore } from "./parts.tsx";

const STACK = [
  { id: "Todo", label: "Todo", lanes: ["Todo"] },
  { id: "In Progress", label: "In Progress", lanes: ["In Progress"] },
  { id: "Rework", label: "Rework", lanes: ["Rework"] },
  { id: "Merging", label: "Merging", lanes: ["Merging"] },
  { id: "Held", label: "Blocked + Human Review", lanes: [...HELD_LANES] },
] as const;

const LANE_ORDER = ["Todo", "Rework", "In Progress", "Merging", "Blocked", "Human Review"];

export function FlowTab({ report, search }: { readonly report: ReportsReport; readonly search: ReportsSearch }): React.ReactElement {
  const [showAll, setShowAll] = React.useState(false);
  const compare = search.compare;
  const lead = search.clock === "created";
  const residenceMissing = report.unavailable.includes("lane_residence_and_unclaimed_waits_unavailable");
  const completion = report.completion;
  const previous = report.previous.partial ? undefined : report.previous;
  const timing = lead ? completion.lead : completion.system;
  const priorTiming = lead ? previous?.lead : previous?.system;
  const todo = completion.lanes.find((lane) => lane.lane === "Todo");
  const priorTodo = previous?.lanes.find((lane) => lane.lane === "Todo");
  const open = openWork(report);
  const aging = open.items;
  const buckets = reportBuckets(report);
  const categories = bucketCategories(buckets);
  const hours = (seconds: number) => seconds / 3600;
  const cycle = buckets.map((bucket) => (lead ? bucket.lead : bucket.cycle));
  const residence = report.analytics.lane_residence.lanes.filter((lane) => LANE_ORDER.includes(lane.lane));
  const residenceTotal = residence.reduce((sum, lane) => sum + lane.seconds, 0);
  const missing = (value: number | null | undefined) => residenceMissing || value === null || value === undefined;
  return (
    <div className="flex flex-col gap-6">
      <MetricTileGroup aria-label="Flow headline" className="lg:grid-cols-5">
        <MetricTile
          label="Cycle time p50"
          value={missing(timing.count || null) ? null : duration(timing.p50_seconds)}
          delta={compare ? durationDelta(timing.p50_seconds, priorTiming?.p50_seconds) : undefined}
        />
        <MetricTile
          label="Cycle time p90"
          value={missing(timing.count || null) ? null : duration(timing.p90_seconds)}
          delta={compare ? durationDelta(timing.p90_seconds, priorTiming?.p90_seconds) : undefined}
        />
        <MetricTile
          label="Todo wait p50"
          value={missing(todo?.count || null) ? null : duration(todo!.p50_seconds)}
          delta={compare ? durationDelta(todo?.p50_seconds ?? 0, priorTodo?.p50_seconds) : undefined}
        />
        <MetricTile
          label="Working time p50"
          value={missing(completion.working.count || null) ? null : duration(completion.working.p50_seconds)}
          delta={compare ? durationDelta(completion.working.p50_seconds, previous?.working.p50_seconds) : undefined}
        />
        <MetricTile
          label="Work in progress"
          value={residenceMissing ? null : formatCount(open.count)}
          detail="open, dispatchable"
        />
      </MetricTileGroup>

      <ChartPanel
        title="Cycle time per day"
        actions={
          <ChartLegend
            items={[
              { label: "p50", color: "var(--primary)" },
              { label: "p50–p90 band", color: "color-mix(in oklab, var(--primary) 35%, var(--background))" },
            ]}
          />
        }
        footnote={lead ? "Clock starts when the issue is created." : "Clock starts at the issue's first Todo."}
      >
        <Chart
          ariaLabel="Cycle time p50 and p90 per bucket"
          categories={categories}
          height={240}
          formatValue={(seconds) => (seconds === 0 ? "0" : duration(seconds))}
          series={[
            {
              id: "cycle",
              label: "p50",
              color: "var(--primary)",
              kind: "band",
              values: cycle.map((value) => (residenceMissing || value.count === 0 ? null : value.p50_seconds)),
              upper: cycle.map((value) => (residenceMissing || value.count === 0 ? null : value.p90_seconds)),
            },
          ]}
        />
      </ChartPanel>

      <ChartPanel
        title="Hours spent in each lane, per day"
        actions={<ChartLegend items={STACK.map((group) => ({ label: group.label, color: laneColor(group.id) }))} />}
      >
        {residenceMissing ? (
          <Muted>
            <Dash /> Lane residence is not recorded.
          </Muted>
        ) : (
          <>
            <Chart
              ariaLabel="Lane hours per bucket, stacked by lane"
              categories={categories}
              height={220}
              formatValue={(value) => `${Math.round(value)} h`}
              series={STACK.map((group) => ({
                id: group.id,
                label: group.label,
                color: laneColor(group.id),
                kind: "bar" as const,
                stack: "lanes",
                values: buckets.map((bucket) => hours(group.lanes.reduce((sum, lane) => sum + (bucket.lane_seconds[lane] ?? 0), 0))),
              }))}
            />
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Lane</TableHead>
                  <TableHead className="text-right">Total</TableHead>
                  <TableHead className="text-right">Share</TableHead>
                  <TableHead className="text-right">p50 per visit</TableHead>
                  <TableHead className="text-right">p90 per visit</TableHead>
                  {compare ? <TableHead className="text-right">vs prior</TableHead> : null}
                </TableRow>
              </TableHeader>
              <TableBody>
                {residence
                  .toSorted((a, b) => b.seconds - a.seconds)
                  .map((lane) => {
                    const before = report.prior?.lane_seconds[lane.lane];
                    const change = before ? Math.round((100 * (lane.seconds - before)) / before) : null;
                    return (
                      <TableRow key={lane.lane}>
                        <TableCell className="font-medium">{lane.lane}</TableCell>
                        <TableCell className="text-right tabular-nums">{duration(lane.seconds)}</TableCell>
                        <TableCell className="text-right tabular-nums">{residenceTotal > 0 ? percentText((100 * lane.seconds) / residenceTotal) : <Dash />}</TableCell>
                        <TableCell className="text-right tabular-nums">{duration(lane.p50_seconds)}</TableCell>
                        <TableCell className="text-right tabular-nums">{duration(lane.p90_seconds)}</TableCell>
                        {compare ? (
                          <TableCell
                            className={`text-right tabular-nums ${change === null || change === 0 ? "" : change > 0 ? "text-destructive-foreground" : "text-success-foreground"}`}
                          >
                            {change === null ? <Dash /> : change === 0 ? "±0%" : `${change > 0 ? "+" : "−"}${Math.abs(change)}%`}
                          </TableCell>
                        ) : null}
                      </TableRow>
                    );
                  })}
              </TableBody>
            </Table>
          </>
        )}
      </ChartPanel>

      <ChartPanel
        title="Aging work in progress"
        actions={<span className="text-muted-foreground text-xs">Amber: past the lane's p90 · Red: over 24 h</span>}
      >
        {residenceMissing ? (
          <Muted>
            <Dash /> Aging work is not recorded.
          </Muted>
        ) : aging.length === 0 ? (
          <Muted>No open work in progress.</Muted>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Issue</TableHead>
                  <TableHead>Lane</TableHead>
                  <TableHead className="text-right">In lane</TableHead>
                  <TableHead className="text-right">Since first Todo</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(showAll ? aging : aging.slice(0, 5)).map((item) => (
                  <TableRow key={item.work_item_id}>
                    <TableCell className="max-w-0 w-full">
                      <IssueLink id={item.work_item_id} report={report} className="block" />
                    </TableCell>
                    <TableCell>
                      <LaneBadge lane={item.lane} />
                    </TableCell>
                    <TableCell className={`text-right tabular-nums ${ageTone(item)}`}>{hoursLabel(item.hours)}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {item.since_todo_hours === undefined ? <Dash /> : hoursLabel(item.since_todo_hours)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <ShowMore
              shown={5}
              total={open.total}
              expanded={showAll}
              label={aging.length < open.total ? `Show top ${formatCount(aging.length)}` : `Show all ${formatCount(open.total)}`}
              onToggle={() => setShowAll((value) => !value)}
            />
          </>
        )}
      </ChartPanel>
    </div>
  );
}
