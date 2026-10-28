import React from "react";

import { Chart, ChartLegend, ChartPanel } from "../../components/ui/chart.tsx";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { Toggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import type { ReportsReport } from "../../contracts/reports.ts";
import { formatCount, formatTokens, formatUsd } from "../usage/usageFormat.ts";
import {
  bucketCategories,
  csvCell,
  duration,
  NOT_SHIPPED_COLOR,
  OTHER_STAGE_COLOR,
  percentText,
  priorRecorded,
  reportBuckets,
  type ReportsSearch,
  STAGE_GROUPS,
  stageLabel,
  usdDelta,
} from "./model.ts";
import { Dash, IssueLink, LaneBadge, Muted, ShowMore } from "./parts.tsx";

type Stage = ReportsReport["stages"][number];

const GROUP_HEADINGS = {
  stage: ["Stage", "Model · effort"],
  model: ["Model", "Stages"],
  runner: ["Runner", "Runner ID"],
} as const;

export interface CostRow {
  readonly key: string;
  readonly name: string;
  readonly detail: string;
  readonly sessions: number;
  readonly succeeded: number;
  readonly p50: number | null;
  readonly cost: number;
}

export function runnerRows(runners: NonNullable<ReportsReport["runners"]>): readonly CostRow[] {
  return runners
    .map((runner) => ({
      key: runner.runner_id,
      name: runner.name || runner.runner_id,
      detail: runner.runner_id,
      sessions: runner.sessions,
      succeeded: runner.succeeded,
      p50: runner.duration.count > 0 ? runner.duration.p50_seconds : null,
      cost: runner.cost_usd,
    }))
    .toSorted((a, b) => b.cost - a.cost);
}

export function costRows(stages: readonly Stage[], group: "stage" | "model"): readonly CostRow[] {
  const enabled = stages.filter((stage) => !stage.disabled);
  if (group === "stage")
    return enabled
      .map((stage) => ({
        key: `${stage.stage}:${stage.model}:${stage.effort}`,
        name: stageLabel(stage.stage),
        detail: `${stage.model} · ${stage.effort}`,
        sessions: stage.sessions,
        succeeded: stage.succeeded,
        p50: stage.duration.count > 0 ? stage.duration.p50_seconds : null,
        cost: stage.cost_usd,
      }))
      .toSorted((a, b) => b.cost - a.cost);
  const byModel = new Map<string, { sessions: number; succeeded: number; cost: number; stages: Set<string> }>();
  for (const stage of enabled) {
    const row = byModel.get(stage.model) ?? { sessions: 0, succeeded: 0, cost: 0, stages: new Set<string>() };
    row.sessions += stage.sessions;
    row.succeeded += stage.succeeded;
    row.cost += stage.cost_usd;
    row.stages.add(stageLabel(stage.stage));
    byModel.set(stage.model, row);
  }
  return [...byModel.entries()]
    .map(([model, row]) => ({ key: model, name: model, detail: [...row.stages].join(", "), sessions: row.sessions, succeeded: row.succeeded, p50: null, cost: row.cost }))
    .toSorted((a, b) => b.cost - a.cost);
}

/** A bucket's spend: the Hub attributes each usage row to its attempt's stage, or to not-shipped work, so the parts sum to Total spend. */
export function stageStack(bucket: { stage_cost: Readonly<Record<string, number>>; not_shipped_cost: number }) {
  const known = new Set<string>(STAGE_GROUPS.flatMap((group) => group.stages));
  const groups = STAGE_GROUPS.map((group) => group.stages.reduce((sum, stage) => sum + (bucket.stage_cost[stage] ?? 0), 0));
  const other = Object.entries(bucket.stage_cost)
    .filter(([stage]) => !known.has(stage))
    .reduce((sum, [, value]) => sum + value, 0);
  return { groups, other, notShipped: bucket.not_shipped_cost };
}

export function costCsv(report: ReportsReport): string {
  const lines = [["Section", "Name", "Detail", "Attempts", "Succeeded", "Spend USD"].join(",")];
  for (const row of costRows(report.stages, "stage"))
    lines.push(["Stage", row.name, row.detail, row.sessions, row.succeeded, row.cost.toFixed(2)].map(csvCell).join(","));
  for (const item of report.spend)
    lines.push(["Issue", item.title || report.titles[item.work_item_id] || item.work_item_id, item.state ?? "", item.attempts ?? "", "", item.cost_usd.toFixed(2)].map(csvCell).join(","));
  return `${lines.join("\r\n")}\r\n`;
}

export function CostTab({
  report,
  search,
  onSearch,
}: {
  readonly report: ReportsReport;
  readonly search: ReportsSearch;
  readonly onSearch: (patch: Partial<ReportsSearch>) => void;
}): React.ReactElement {
  const [showAll, setShowAll] = React.useState(false);
  const compare = search.compare;
  const usageMissing = report.unavailable.includes("recorded_usage");
  const shippedKnown = report.spend.some((item) => item.shipped !== undefined);
  const perShipped = report.completion.done > 0 ? report.completion.cost_usd / report.completion.done : null;
  const priorPerShipped = priorRecorded(report) && report.previous.done > 0 ? report.previous.cost_usd / report.previous.done : undefined;
  const p50 = report.completion.cost_p50_usd ?? null;
  const p90 = report.completion.cost_p90_usd ?? null;
  const buckets = reportBuckets(report);
  const seriesKnown = report.series !== undefined;
  const notShipped = seriesKnown ? buckets.reduce((sum, bucket) => sum + bucket.not_shipped_cost, 0) : null;
  const categories = bucketCategories(buckets);
  const stacks = buckets.map(stageStack);
  const hasOther = stacks.some((stack) => stack.other > 0);
  const rows =
    search.group === "runner" ? (report.runners === undefined ? [] : runnerRows(report.runners)) : costRows(report.stages, search.group);
  const rowsTotal = rows.reduce((sum, row) => sum + row.cost, 0);
  const issues = report.spend.filter((item) =>
    search.issues === "all" ? true : search.issues === "shipped" ? item.shipped === true : item.shipped === false,
  );
  const legend = [
    ...STAGE_GROUPS.map((group) => ({ label: group.label, color: group.color })),
    ...(hasOther ? [{ label: "Other", color: OTHER_STAGE_COLOR }] : []),
    { label: "Not shipped", color: NOT_SHIPPED_COLOR },
  ];
  return (
    <div className="flex flex-col gap-6">
      <MetricTileGroup aria-label="Cost headline" className="lg:grid-cols-4">
        <MetricTile
          label="Total spend"
          value={usageMissing ? null : formatUsd(report.cost_usd)}
          delta={compare ? (usageMissing ? null : usdDelta(report.cost_usd, report.previous_cost_usd)) : undefined}
        />
        <MetricTile
          label="Per shipped issue"
          value={usageMissing || perShipped === null ? null : formatUsd(perShipped)}
          delta={compare ? usdDelta(usageMissing ? null : perShipped, priorPerShipped) : undefined}
          detail={p50 === null || p90 === null ? <>p50 <Dash /> · p90 <Dash /></> : `p50 ${formatUsd(p50)} · p90 ${formatUsd(p90)}`}
        />
        <MetricTile
          label="Spent on work that did not ship"
          value={usageMissing || notShipped === null ? null : formatUsd(notShipped)}
          delta={
            usageMissing || notShipped === null || report.cost_usd <= 0
              ? undefined
              : { text: `${percentText((100 * notShipped) / report.cost_usd)} of spend`, direction: "flat", tone: "bad" }
          }
          detail="attempts on issues not shipped in this range"
        />
        <MetricTile
          label="Tokens"
          value={usageMissing ? null : formatTokens(report.tokens)}
          detail={usageMissing || report.input <= 0 ? undefined : `${percentText((100 * report.cached) / report.input)} served from cache`}
        />
      </MetricTileGroup>

      <ChartPanel title="Spend per day by stage" actions={<ChartLegend items={legend} />}>
        {usageMissing || !seriesKnown ? (
          <Muted>
            <Dash /> Spend by stage per day is not recorded.
          </Muted>
        ) : (
          <Chart
            ariaLabel="Spend per bucket, stacked by stage, with spend on work that did not ship"
            categories={categories}
            height={230}
            formatValue={(value) => formatUsd(value)}
            series={[
              ...STAGE_GROUPS.map((group, index) => ({
                id: group.id,
                label: group.label,
                color: group.color,
                kind: "bar" as const,
                stack: "spend",
                values: stacks.map((stack) => stack.groups[index] ?? 0),
              })),
              ...(hasOther ? [{ id: "other", label: "Other", color: OTHER_STAGE_COLOR, kind: "bar" as const, stack: "spend", values: stacks.map((stack) => stack.other) }] : []),
              { id: "not-shipped", label: "Not shipped", color: NOT_SHIPPED_COLOR, kind: "bar" as const, stack: "spend", values: stacks.map((stack) => stack.notShipped) },
            ]}
          />
        )}
      </ChartPanel>

      <ChartPanel
        title="By stage and model"
        actions={
          <ToggleGroup
            aria-label="Group by"
            value={[search.group]}
            onValueChange={(value) => {
              const next = value[0];
              if (next === "stage" || next === "model" || next === "runner") onSearch({ group: next });
            }}
          >
            <Toggle value="stage">Stage</Toggle>
            <Toggle value="model">Model</Toggle>
            <Toggle value="runner">Runner</Toggle>
          </ToggleGroup>
        }
      >
        {search.group === "runner" && report.runners === undefined ? (
          <Muted>
            <Dash /> Spend by runner is not recorded.
          </Muted>
        ) : usageMissing ? (
          <Muted>
            <Dash /> Recorded usage is not available.
          </Muted>
        ) : rows.length === 0 ? (
          <Muted>No recorded sessions in this range.</Muted>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{GROUP_HEADINGS[search.group][0]}</TableHead>
                <TableHead>{GROUP_HEADINGS[search.group][1]}</TableHead>
                <TableHead className="text-right">Attempts</TableHead>
                <TableHead className="text-right">Succeeded</TableHead>
                <TableHead className="text-right">Time p50</TableHead>
                <TableHead className="text-right">Spend</TableHead>
                <TableHead className="text-right">Share</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row) => (
                <TableRow key={row.key}>
                  <TableCell className="font-medium">{row.name}</TableCell>
                  <TableCell className="text-muted-foreground">{row.detail}</TableCell>
                  <TableCell className="text-right tabular-nums">{formatCount(row.sessions)}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.sessions > 0 ? percentText((100 * row.succeeded) / row.sessions) : <Dash />}</TableCell>
                  <TableCell className="text-right tabular-nums">{row.p50 === null ? <Dash /> : duration(row.p50)}</TableCell>
                  <TableCell className="text-right tabular-nums">{formatUsd(row.cost)}</TableCell>
                  <TableCell className="text-right tabular-nums">{rowsTotal > 0 ? percentText((100 * row.cost) / rowsTotal) : <Dash />}</TableCell>
                </TableRow>
              ))}
              {search.group === "runner" ? null : report.stages
                .filter((stage) => stage.disabled)
                .map((stage) => (
                  <TableRow key={`${stage.stage}:disabled`}>
                    <TableCell className="font-medium">{stageLabel(stage.stage)}</TableCell>
                    <TableCell colSpan={6} className="text-muted-foreground">
                      Disabled by project gates
                    </TableCell>
                  </TableRow>
                ))}
            </TableBody>
          </Table>
        )}
      </ChartPanel>

      <ChartPanel
        title="Most expensive issues"
        actions={
          <ToggleGroup
            aria-label="Which issues"
            value={[search.issues]}
            onValueChange={(value) => {
              const next = value[0];
              if (next === "all" || next === "shipped" || next === "not-shipped") onSearch({ issues: next });
            }}
          >
            <Toggle value="all">All</Toggle>
            <Toggle value="shipped" disabled={!shippedKnown}>
              Shipped
            </Toggle>
            <Toggle value="not-shipped" disabled={!shippedKnown}>
              Not shipped
            </Toggle>
          </ToggleGroup>
        }
      >
        {usageMissing ? (
          <Muted>
            <Dash /> Issue spend is not recorded.
          </Muted>
        ) : issues.length === 0 ? (
          <Muted>No issue spend in this range.</Muted>
        ) : (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Issue</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead className="text-right">Attempts</TableHead>
                  <TableHead className="text-right">Spend</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(showAll ? issues.slice(0, 50) : issues.slice(0, 5)).map((item) => (
                  <TableRow key={item.work_item_id}>
                    <TableCell className="max-w-0 w-full">
                      <IssueLink id={item.work_item_id} report={report} className="block" />
                    </TableCell>
                    <TableCell>{item.state ? <LaneBadge lane={item.state} /> : <Dash />}</TableCell>
                    <TableCell className="text-right tabular-nums">{item.attempts === undefined ? <Dash /> : formatCount(item.attempts)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatUsd(item.cost_usd)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <ShowMore shown={5} total={issues.length} expanded={showAll} label="Show top 50" onToggle={() => setShowAll((value) => !value)} />
          </>
        )}
      </ChartPanel>
    </div>
  );
}
