import React from "react";

import { BarMeter, Chart, ChartLegend, ChartPanel } from "../../components/ui/chart.tsx";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import type { ReportsReport } from "../../contracts/reports.ts";
import { formatCount } from "../usage/usageFormat.ts";
import { bucketCategories, countDelta, percentText, pointsDelta, reportBuckets, type ReportsSearch } from "./model.ts";
import { Dash, ExpandRow, IssueLink, Muted } from "./parts.tsx";
import { REWORK_GROUPS, reworkByGroup, reworkGroupOf } from "./rework.ts";

const STACK_ORDER = ["refused", "gate", "review", "conflict", "other"] as const;
const KIND_WORD: Readonly<Record<string, string>> = { infrastructure: "infrastructure", issue: "issue" };

/** The Hub's rework classes, largest first, with Not classified last. */
export function reworkRows(report: ReportsReport) {
  return report.rework_classes.toSorted(
    (a, b) => Number(a.class === "Not classified") - Number(b.class === "Not classified") || b.count - a.count,
  );
}

export function QualityTab({ report, search }: { readonly report: ReportsReport; readonly search: ReportsSearch }): React.ReactElement {
  const compare = search.compare;
  const quality = report.analytics.quality;
  const firstTry = report.unavailable.includes("landed_first_try") ? null : report.first_try_percent;
  const buckets = reportBuckets(report);
  const seriesKnown = report.series !== undefined;
  const categories = bucketCategories(buckets);
  const kinds = Object.fromEntries(report.rework_classes.map((row) => [row.class, row.kind]));
  const byDay = buckets.map((bucket) => reworkByGroup(bucket.rework_classes, kinds));
  const rows = reworkRows(report);
  const max = Math.max(1, ...rows.map((row) => row.count));
  return (
    <div className="flex flex-col gap-6">
      <MetricTileGroup aria-label="Quality headline" className="lg:grid-cols-4">
        <MetricTile
          label="Landed first try"
          value={firstTry === null ? null : percentText(firstTry)}
          delta={compare ? pointsDelta(firstTry, report.previous_first_try_percent) : undefined}
          detail={`of ${formatCount(report.completion.done)} shipped`}
        />
        <MetricTile
          label="Sent back for issue reasons"
          value={quality?.rework_percent == null ? null : percentText(quality.rework_percent)}
          delta={compare ? pointsDelta(quality?.rework_percent ?? null, report.prior?.rework_percent, false) : undefined}
          detail="review findings, missing criteria"
        />
        <MetricTile
          label="Bounced by infrastructure"
          value={quality === undefined ? null : formatCount(quality.infrastructure_returns)}
          delta={compare ? (quality === undefined ? null : countDelta(quality.infrastructure_returns, report.prior?.infrastructure_returns, false)) : undefined}
          detail="landing errors · counted against the runner, not the issue"
        />
        <MetricTile
          label="Escapes reported"
          value={quality === undefined ? null : formatCount(quality.escapes)}
          detail={`of ${formatCount(report.completion.done)} shipped · from escape reports`}
        />
      </MetricTileGroup>

      <ChartPanel
        title="Why work went back, per day"
        actions={<ChartLegend items={REWORK_GROUPS.map((group) => ({ label: group.label, color: group.color }))} />}
        footnote="Landing refused is infrastructure: attributed to the runner and excluded from the issue rework rate."
      >
        {seriesKnown ? (
          <Chart
            ariaLabel="Rework returns per bucket, stacked by cause"
            categories={categories}
            height={220}
            series={STACK_ORDER.map((id) => REWORK_GROUPS.find((group) => group.id === id)!).map((group) => ({
              id: group.id,
              label: group.label,
              color: group.color,
              kind: "bar" as const,
              stack: "causes",
              values: byDay.map((day) => day[group.id]),
            }))}
          />
        ) : (
          <Muted>
            <Dash /> Rework returns per day are not recorded.
          </Muted>
        )}
      </ChartPanel>

      <ChartPanel title="Rework causes" actions={<span className="text-muted-foreground text-xs">Select a cause to list its issues</span>}>
        {rows.length === 0 ? (
          <Muted>No rework in this range.</Muted>
        ) : (
          <div className="flex flex-col">
            {rows.map((row) => {
              const unclassified = row.class === "Not classified";
              const group = reworkGroupOf(row.class, row.kind);
              return (
                <ExpandRow
                  key={row.class}
                  label={`${row.class}, ${KIND_WORD[row.kind] ?? "unclassified"}: ${formatCount(row.count)} returns, ${formatCount(row.issues)} ${row.issues === 1 ? "issue" : "issues"}`}
                  summary={
                    <>
                      <span className={`w-36 shrink-0 truncate sm:w-64 ${unclassified ? "text-muted-foreground" : ""}`} title={row.class}>
                        {row.class}
                      </span>
                      <BarMeter value={row.count} max={max} color={group.color} className="min-w-8 flex-1" />
                      <span className="w-10 shrink-0 text-right tabular-nums">{formatCount(row.count)}</span>
                    </>
                  }
                >
                  {row.work_items && row.work_items.length > 0 ? (
                    row.work_items.map((id) => <IssueLink key={id} id={id} report={report} className="block text-xs" />)
                  ) : (
                    <span>
                      Issues <Dash />
                    </span>
                  )}
                  <span>{`${formatCount(row.issues)} ${row.issues === 1 ? "issue" : "issues"} in total`}</span>
                </ExpandRow>
              );
            })}
          </div>
        )}
      </ChartPanel>
    </div>
  );
}
