import React from "react";

import { Chart, ChartLegend, ChartPanel, ProportionBar } from "../../components/ui/chart.tsx";
import { useMediaQuery } from "../../hooks/useMediaQuery.ts";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import type { ReportsReport } from "../../contracts/reports.ts";
import { formatCount, formatUsd } from "../usage/usageFormat.ts";
import {
  bucketCategories,
  countDelta,
  duration,
  durationDelta,
  HELD_LANES,
  hoursLabel,
  laneColor,
  percentText,
  pointsDelta,
  priorRecorded,
  rangeOption,
  reportBuckets,
  type ReportsSearch,
  usdDelta,
} from "./model.ts";
import { Dash, IssueLink, LaneBadge, Muted, TabLink } from "./parts.tsx";

export function costPerShipped(cost: number | undefined, done: number): number | null {
  return cost === undefined || done <= 0 ? null : cost / done;
}

const EXCLUDED_LANES = new Set(["Backlog", "Triage"]);

/** Open work in the lanes Detent works and the lanes people hold, longest in lane first; never Backlog or Triage. */
export function openWork(report: ReportsReport) {
  if (report.open_work !== undefined) return { ...report.open_work, total: report.open_work.count + report.open_work.held };
  const items = report.analytics.lane_residence.aging.items
    .filter((item) => !EXCLUDED_LANES.has(item.lane))
    .toSorted((a, b) => b.hours - a.hours);
  const held = items.filter((item) => HELD_LANES.has(item.lane)).length;
  return {
    items,
    count: items.length - held,
    held,
    stuck: items.filter((item) => item.hours > 24).length,
    previous_stuck: undefined,
    total: items.length,
  };
}

export function ageTone(item: { hours: number; lane_p90_hours?: number | undefined }): string {
  if (item.hours > 24) return "text-destructive-foreground";
  if (item.lane_p90_hours !== undefined && item.hours > item.lane_p90_hours) return "text-warning-foreground";
  return "text-muted-foreground";
}

export function OverviewTab({ report, search }: { readonly report: ReportsReport; readonly search: ReportsSearch }): React.ReactElement {
  const phone = useMediaQuery("max-sm");
  const compare = search.compare;
  const prior = rangeOption(search.range).prior;
  const usageMissing = report.unavailable.includes("recorded_usage");
  const residenceMissing = report.unavailable.includes("lane_residence_and_unclaimed_waits_unavailable");
  const completion = report.completion;
  const previous = report.previous.partial ? undefined : report.previous;
  const firstTry = report.unavailable.includes("landed_first_try") ? null : report.first_try_percent;
  const perShipped = usageMissing ? null : costPerShipped(completion.cost_usd, completion.done);
  const recorded = priorRecorded(report);
  const priorPerShipped = usageMissing || !recorded ? null : costPerShipped(report.previous.cost_usd, report.previous.done);
  const open = openWork(report);
  const stuck = open.items.filter((item) => item.hours > 24);
  const buckets = reportBuckets(report);
  const categories = bucketCategories(buckets);
  const hasPrior = buckets.some((bucket) => bucket.prior_done !== undefined);
  const shippedSpend = report.spend.filter((item) => item.shipped === true).slice(0, 5);
  const shippedKnown = report.spend.some((item) => item.shipped !== undefined);
  const lanes = completion.lanes.filter((lane) => lane.group !== "excluded" && lane.lane !== "Backlog" && lane.lane !== "Triage");
  const laneSeconds = (name: string) => lanes.filter((lane) => lane.lane === name).reduce((sum, lane) => sum + lane.seconds, 0);
  const segments = [
    { label: "Waiting in Todo", value: laneSeconds("Todo"), color: laneColor("Todo") },
    { label: "In Progress", value: laneSeconds("In Progress"), color: laneColor("In Progress") },
    { label: "Rework", value: laneSeconds("Rework"), color: laneColor("Rework") },
    { label: "Merging", value: laneSeconds("Merging"), color: laneColor("Merging") },
    {
      label: "Held by people",
      value: lanes.filter((lane) => HELD_LANES.has(lane.lane)).reduce((sum, lane) => sum + lane.seconds, 0),
      color: laneColor("Held"),
    },
  ];
  const segmentTotal = segments.reduce((sum, segment) => sum + segment.value, 0);
  const share = (value: number) => (segmentTotal > 0 ? percentText((100 * value) / segmentTotal) : "—");
  const lever = segments.toSorted((a, b) => b.value - a.value)[0];
  const todo = completion.lanes.find((lane) => lane.lane === "Todo");
  return (
    <div className="flex flex-col gap-6">
      <MetricTileGroup aria-label="Headline metrics" className="lg:grid-cols-5">
        <MetricTile
          label="Shipped"
          value={residenceMissing ? null : formatCount(completion.done)}
          delta={compare ? (residenceMissing ? null : countDelta(completion.done, recorded ? report.previous.done : undefined, true, ` vs ${prior}`)) : undefined}
          detail={`plus ${formatCount(completion.cancelled)} cancelled`}
        />
        <MetricTile
          label="Cycle time p50"
          value={residenceMissing || completion.system.count === 0 ? null : duration(completion.system.p50_seconds)}
          delta={compare ? durationDelta(completion.system.p50_seconds, previous?.system.p50_seconds) : undefined}
          detail={completion.system.count === 0 ? "Todo → Done" : `p90 ${duration(completion.system.p90_seconds)} · Todo → Done`}
        />
        <MetricTile
          label="Landed first try"
          value={firstTry === null ? null : percentText(firstTry)}
          delta={compare ? pointsDelta(firstTry, report.previous_first_try_percent) : undefined}
          detail={`of ${formatCount(completion.done)} shipped`}
        />
        <MetricTile
          label="Cost per shipped issue"
          value={perShipped === null ? null : formatUsd(perShipped)}
          delta={compare ? usdDelta(perShipped, priorPerShipped) : undefined}
          detail={perShipped === null ? undefined : `${formatUsd(completion.cost_usd)} lifetime, shipped issues`}
        />
        <MetricTile
          className="max-sm:hidden"
          label="Stuck over 24h"
          value={residenceMissing ? null : formatCount(open.stuck)}
          delta={compare ? countDelta(open.stuck, open.previous_stuck, false) : undefined}
          detail="open, in lane over 24 h"
        />
      </MetricTileGroup>

      <ChartPanel
        title="Shipped per day"
        actions={
          <ChartLegend
            items={[
              { label: "Shipped", color: "var(--success)" },
              ...(phone ? [] : [{ label: "Failed attempts", color: "var(--warning)" }]),
              ...(compare && hasPrior && !phone ? [{ label: rangeOption(search.range).prior.replace(/^p/, "P"), color: "var(--muted-foreground)", dashed: true }] : []),
            ]}
          />
        }
        footnote={`Hover a day for counts. Bucket: day for 7d+, hour for 24h.${residenceMissing ? " Shipped counts are not recorded." : ""}`}
      >
        <Chart
          ariaLabel="Shipped issues and failed attempts per bucket"
          categories={categories}
          height={260}
          series={[
            { id: "shipped", label: "Shipped", color: "var(--success)", kind: "bar", values: buckets.map((bucket) => (residenceMissing ? null : bucket.done)) },
            ...(phone ? [] : [{ id: "failed", label: "Failed attempts", color: "var(--warning)", kind: "bar" as const, values: buckets.map((bucket) => bucket.failed) }]),
            ...(compare && hasPrior && !phone
              ? [{ id: "prior", label: "Prior period", color: "var(--muted-foreground)", kind: "line" as const, dashed: true, values: buckets.map((bucket) => bucket.prior_done ?? null) }]
              : []),
          ]}
        />
      </ChartPanel>

      <div className="grid min-w-0 gap-6 lg:grid-cols-2">
        <ChartPanel title="Where cycle time goes" actions={<TabLink tab="flow" search={search}>Open Flow</TabLink>}>
          {residenceMissing || segmentTotal === 0 ? (
            <Muted>
              <Dash /> {residenceMissing ? "Lane residence is not recorded." : "No completed issues in this range."}
            </Muted>
          ) : (
            <>
              <ProportionBar
                ariaLabel={segments.map((segment) => `${segment.label} ${share(segment.value)}`).join(", ")}
                segments={segments}
              />
              <ChartLegend items={segments.map((segment) => ({ label: `${segment.label} ${share(segment.value)}`, color: segment.color }))} />
              {lever && lever.label === "Waiting in Todo" && todo ? (
                <Muted>Biggest lever: Todo wait. Median issue waits {duration(todo.p50_seconds)} for a slot.</Muted>
              ) : lever ? (
                <Muted>Biggest lever: {lever.label}, {share(lever.value)} of cycle time.</Muted>
              ) : null}
            </>
          )}
        </ChartPanel>
        <ChartPanel title="Most expensive shipped issues" actions={<TabLink tab="cost" search={search}>Open Cost</TabLink>}>
          {usageMissing || !shippedKnown ? (
            <Muted>
              <Dash /> Spend by shipped issue is not recorded.
            </Muted>
          ) : shippedSpend.length === 0 ? (
            <Muted>No shipped issue spend in this range.</Muted>
          ) : (
            <div className="flex flex-col">
              {shippedSpend.map((item) => (
                <div key={item.work_item_id} className="flex min-w-0 items-center gap-3 border-border border-t py-2.5 text-sm">
                  <IssueLink id={item.work_item_id} report={report} className="flex-1" />
                  <span className="whitespace-nowrap tabular-nums">{formatUsd(item.cost_usd)}</span>
                </div>
              ))}
            </div>
          )}
        </ChartPanel>
      </div>

      <ChartPanel title="Stuck right now" actions={<TabLink tab="flow" search={search}>All aging work</TabLink>}>
        {residenceMissing ? (
          <Muted>
            <Dash /> Aging work is not recorded.
          </Muted>
        ) : stuck.length === 0 ? (
          <Muted>Nothing open has been in its lane over 24 h.</Muted>
        ) : (
          <div className="flex flex-col">
            {stuck.slice(0, 4).map((item) => (
              <div key={item.work_item_id} className="flex min-w-0 items-center gap-3 border-border border-t py-2.5 text-sm">
                <LaneBadge lane={item.lane} />
                <IssueLink id={item.work_item_id} report={report} className="flex-1" />
                <span className={`whitespace-nowrap tabular-nums ${ageTone(item)}`}>{hoursLabel(item.hours)}</span>
              </div>
            ))}
          </div>
        )}
      </ChartPanel>
    </div>
  );
}
