import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Chart, ChartLegend, ChartPanel } from "../../components/ui/chart.tsx";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import type { ReportsReport } from "../../contracts/reports.ts";
import { formatRelativeTimeLabel } from "../../timestampFormat.ts";
import { formatCount, formatDateTimeShort, formatUsd } from "../usage/usageFormat.ts";
import { bucketCategories, countDelta, failureKind, KIND_COLORS, percentText, reportBuckets, type ReportsSearch } from "./model.ts";
import { Dash, ExpandRow, IssueLink, Muted } from "./parts.tsx";

const KIND_BADGE = {
  infrastructure: { label: "Infrastructure", variant: "info" },
  issue: { label: "Issue", variant: "warning" },
  unknown: { label: "Unknown", variant: "secondary" },
} as const;

const KIND_ORDER = { infrastructure: 0, issue: 1, unknown: 2 } as const;

/** The Hub's failure classes for a kind filter: infrastructure first, Not classified last. */
export function failureRows(report: ReportsReport, kind: ReportsSearch["kind"]) {
  return report.failure_classes
    .map((item) => ({ ...item, kind: failureKind(item.kind) }))
    .filter((item) => kind === "all" || item.kind === kind)
    .toSorted((a, b) => KIND_ORDER[a.kind] - KIND_ORDER[b.kind] || b.count - a.count);
}

export function FailuresTab({ report, search }: { readonly report: ReportsReport; readonly search: ReportsSearch }): React.ReactElement {
  const compare = search.compare;
  const usageMissing = report.unavailable.includes("recorded_usage");
  const buckets = reportBuckets(report);
  const categories = bucketCategories(buckets);
  const splitKnown = report.series !== undefined;
  const failed = buckets.reduce((sum, bucket) => sum + bucket.failed, 0);
  const classes = report.failure_classes;
  const classTotal = classes.reduce((sum, item) => sum + item.count, 0);
  const failedSpend = classes.reduce((sum, item) => sum + item.cost_usd, 0);
  const infraCount = classes.filter((item) => failureKind(item.kind) === "infrastructure").reduce((sum, item) => sum + item.count, 0);
  const infraShare = classTotal > 0 ? (100 * infraCount) / classTotal : null;
  const issuesHit = report.failed_issues ?? null;
  const rows = failureRows(report, search.kind);
  const showInfra = search.kind !== "issue";
  const showIssue = search.kind !== "infrastructure";
  const unclassified = buckets.map((bucket) => Math.max(0, bucket.failed - bucket.failed_infrastructure - bucket.failed_issue));
  const showUnclassified = search.kind === "all" && unclassified.some((value) => value > 0);
  return (
    <div className="flex flex-col gap-6">
      <MetricTileGroup aria-label="Failures headline" className="lg:grid-cols-4">
        <MetricTile label="Failed attempts" value={formatCount(failed)} delta={compare ? countDelta(failed, report.previous_failed, false) : undefined} />
        <MetricTile
          label="Spend on failed attempts"
          value={usageMissing ? null : formatUsd(failedSpend)}
          delta={
            usageMissing || report.cost_usd <= 0
              ? undefined
              : { text: `${percentText((100 * failedSpend) / report.cost_usd)} of spend`, direction: "flat", tone: "bad" }
          }
        />
        <MetricTile
          label="Issues hit"
          value={issuesHit === null ? null : formatCount(issuesHit)}
          detail={report.analytics.quality ? `of ${formatCount(report.analytics.quality.worked_items)} worked` : undefined}
        />
        <MetricTile label="Infrastructure share" value={infraShare === null ? null : percentText(infraShare)} detail="attributed to runners" />
      </MetricTileGroup>

      <ChartPanel
        title="Failed attempts per day"
        actions={
          <ChartLegend
            items={
              splitKnown
                ? [
                    ...(showInfra ? [{ label: "Infrastructure", color: KIND_COLORS.infrastructure }] : []),
                    ...(showIssue ? [{ label: "Issue", color: KIND_COLORS.issue }] : []),
                    ...(showUnclassified ? [{ label: "Not classified", color: KIND_COLORS.unknown }] : []),
                  ]
                : [{ label: "Failed attempts", color: KIND_COLORS.unknown }]
            }
          />
        }
        footnote={splitKnown ? undefined : "The infrastructure and issue split per day is not recorded."}
      >
        <Chart
          ariaLabel="Failed attempts per bucket split by infrastructure and issue"
          categories={categories}
          height={200}
          series={
            splitKnown
              ? [
                  ...(showInfra
                    ? [{ id: "infrastructure", label: "Infrastructure", color: KIND_COLORS.infrastructure, kind: "bar" as const, stack: "failed", values: buckets.map((bucket) => bucket.failed_infrastructure) }]
                    : []),
                  ...(showIssue
                    ? [{ id: "issue", label: "Issue", color: KIND_COLORS.issue, kind: "bar" as const, stack: "failed", values: buckets.map((bucket) => bucket.failed_issue) }]
                    : []),
                  ...(showUnclassified
                    ? [{ id: "unclassified", label: "Not classified", color: KIND_COLORS.unknown, kind: "bar" as const, stack: "failed", values: unclassified }]
                    : []),
                ]
              : [{ id: "failed", label: "Failed attempts", color: KIND_COLORS.unknown, kind: "bar" as const, values: buckets.map((bucket) => bucket.failed) }]
          }
        />
      </ChartPanel>

      <ChartPanel
        title="Recurring failures"
        actions={<span className="text-muted-foreground text-xs">Grouped by class · full evidence stays in the issue's run history</span>}
      >
        {rows.length === 0 ? (
          <Muted>No recorded failures in this range.</Muted>
        ) : (
          <div className="flex flex-col">
            <div className="hidden gap-3 pb-2 text-muted-foreground text-xs sm:flex">
              <span className="flex-1">Failure</span>
              <span className="w-28">Kind</span>
              <span className="w-16 text-right">Attempts</span>
              <span className="w-14 text-right">Issues</span>
              <span className="w-16 text-right">Spend</span>
              <span className="w-4" />
            </div>
            {rows.map((row) => {
              const badge = KIND_BADGE[row.kind];
              return (
                <ExpandRow
                  key={row.class}
                  label={`${row.class}, ${badge.label}: ${formatCount(row.count)} attempts, ${formatCount(row.issues)} ${row.issues === 1 ? "issue" : "issues"}, ${usageMissing ? "spend not recorded" : formatUsd(row.cost_usd)}`}
                  summary={
                    <>
                      <span className={`min-w-0 flex-1 truncate ${row.kind === "unknown" ? "text-muted-foreground" : ""}`} title={row.class}>
                        {row.class}
                      </span>
                      <span className="w-28 shrink-0 max-sm:hidden">
                        <Badge size="sm" variant={badge.variant}>
                          {badge.label}
                        </Badge>
                      </span>
                      <span className="w-16 shrink-0 text-right tabular-nums">{formatCount(row.count)}</span>
                      <span className="w-14 shrink-0 text-right tabular-nums max-sm:hidden">{formatCount(row.issues)}</span>
                      <span className="w-16 shrink-0 text-right tabular-nums">{usageMissing ? <Dash /> : formatUsd(row.cost_usd)}</span>
                    </>
                  }
                >
                  {row.first_seen && row.last_seen ? (
                    <span title={`${formatDateTimeShort(row.first_seen)} – ${formatDateTimeShort(row.last_seen)}`}>
                      First seen {formatDateTimeShort(row.first_seen)} · last seen {formatRelativeTimeLabel(row.last_seen)}
                    </span>
                  ) : (
                    <span>
                      First and last seen <Dash />
                    </span>
                  )}
                  <span className="sm:hidden">
                    {badge.label} · {formatCount(row.issues)} {row.issues === 1 ? "issue" : "issues"}
                  </span>
                  <span className="flex min-w-0 flex-col gap-1">
                    Top issues:
                    {row.work_items && row.work_items.length > 0 ? (
                      row.work_items.map((id) => <IssueLink key={id} id={id} report={report} className="block text-xs" />)
                    ) : (
                      <Dash />
                    )}
                  </span>
                  {row.tracking_work_item_id ? (
                    <span className="flex flex-wrap items-center gap-1">
                      Tracking issue: <IssueLink id={row.tracking_work_item_id} report={report} className="text-xs" />
                    </span>
                  ) : null}
                </ExpandRow>
              );
            })}
          </div>
        )}
      </ChartPanel>
    </div>
  );
}
