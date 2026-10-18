import { Link } from "@tanstack/react-router";
import React from "react";

import { BarMeter, Chart, ChartLegend, ChartPanel, ChartSwatch, ProportionBar } from "../../components/ui/chart.tsx";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import type { BootstrapProject } from "../../contracts/index.ts";
import type { OrganizationReports, ReportsFacets } from "../../contracts/reports.ts";
import { formatCount, formatDayShort, formatHourShort, formatUsd } from "../usage/usageFormat.ts";
import { filterOptions, ReportFilterChip } from "./filters.tsx";
import { countDelta, duration, percentText, pointsDelta, rangeOption, reportsSearchParams, type ReportsSearch, usdDelta } from "./model.ts";
import { Dash, Muted } from "./parts.tsx";

const FILERS = [
  { kind: "people", label: "People", detail: "filed in the app or by MCP", color: "var(--info)" },
  { kind: "agents", label: "Agents", detail: "filed during agent work", color: "var(--warning)" },
  { kind: "automation", label: "Automation", detail: "CI and health reports", color: "var(--muted-foreground)" },
] as const;
const signed = (value: number) => `${value > 0 ? "+" : ""}${formatCount(value)}`;

export function OrganizationOverview({ report, search, projects, options, onSearch, onProjectChange }: {
  readonly report: OrganizationReports;
  readonly search: ReportsSearch;
  readonly projects: readonly BootstrapProject[];
  readonly options?: ReportsFacets | undefined;
  readonly onSearch: (patch: Partial<ReportsSearch>) => void;
  readonly onProjectChange?: ((id: string) => void) | undefined;
}): React.ReactElement {
  const current = report.current;
  const previous = report.prior_recorded ? report.previous : undefined;
  const compare = search.compare;
  const compareChart = compare && report.prior_recorded;
  const prior = rangeOption(search.range).prior;
  const projectName = (id: string) => projects.find((project) => project.id === id)?.name ?? id;
  const daily = Date.parse(report.window.to) - Date.parse(report.window.from) >= 6 * 86_400_000;
  const categories = report.series.map((bucket) => ({ key: bucket.from, label: daily ? formatDayShort(bucket.from.slice(0, 10)) : formatHourShort(bucket.from, "UTC") }));
  const filers = FILERS.map((filer) => ({ ...filer, value: report.filed_by.find((actor) => actor.kind === filer.kind)?.current ?? 0 }));
  const unknownFiled = report.filed_by.filter((actor) => !FILERS.some((filer) => filer.kind === actor.kind)).reduce((sum, actor) => sum + actor.current, 0);
  const people = report.completed_by.filter((actor) => actor.kind === "people").reduce((sum, actor) => sum + actor.current, 0);
  const runners = report.completed_by.filter((actor) => actor.kind === "runner");
  const unknownCompleted = report.completed_by.filter((actor) => actor.kind !== "runner" && actor.kind !== "people").reduce((sum, actor) => sum + actor.current, 0);
  const action = (key: "filedBy" | "runners", label: string) => (
    <ReportFilterChip label={key === "filedBy" ? "Filed by" : "Runner"} triggerLabel={label} options={filterOptions(key, options, search.filters[key])} selected={search.filters[key]} onChange={(values) => onSearch({ filters: { ...search.filters, [key]: values } })} />
  );
  return (
    <div className="flex min-w-0 flex-col gap-6">
      <MetricTileGroup aria-label="Headline metrics" className="lg:grid-cols-6">
        <MetricTile label="Filed" value={formatCount(current.filed)} delta={compare ? countDelta(current.filed, previous?.filed, true, ` vs ${prior}`) : undefined} detail="new issues, all projects" />
        <MetricTile label="Completed" value={formatCount(current.completed)} delta={compare ? countDelta(current.completed, previous?.completed, true, ` vs ${prior}`) : undefined} detail={`plus ${formatCount(current.cancelled)} cancelled`} />
        <MetricTile label="Net backlog" value={signed(current.net_backlog)} delta={compare ? countDelta(current.net_backlog, previous?.net_backlog, false, ` vs ${prior}`) : undefined} detail="filed minus closed" />
        <MetricTile label="Changes landed" value={formatCount(current.changes_landed)} delta={compare ? countDelta(current.changes_landed, previous?.changes_landed, true, ` vs ${prior}`) : undefined} detail="merged into the target branch" />
        <MetricTile label="Cost per completed" value={current.cost_per_completed_usd === null ? null : formatUsd(current.cost_per_completed_usd)} delta={compare ? usdDelta(current.cost_per_completed_usd, previous?.cost_per_completed_usd) : undefined} detail={current.cost_per_completed_usd === null ? undefined : `${formatUsd(current.completion_cost_usd)} lifetime, completed issues`} />
        <MetricTile label="Rework" value={current.rework_percent === null ? null : percentText(current.rework_percent)} delta={compare ? pointsDelta(current.rework_percent, previous?.rework_percent, false) : undefined} detail="worked issues sent back" />
      </MetricTileGroup>
      <ChartPanel title="Filed vs completed per day" actions={<ChartLegend items={[
        { label: "Filed", color: "var(--info)" },
        { label: "Completed", color: "var(--success)" },
        ...(compareChart ? [{ label: `${prior.replace(/^p/, "P")} completed`, color: "var(--muted-foreground)", dashed: true }] : []),
      ]} />} footnote="Hover a day for counts by project. Bucket: day for 7d+, hour for 24h.">
        <Chart ariaLabel="Filed and completed issues per bucket" categories={categories} height={260} series={[
          { id: "filed", label: "Filed", color: "var(--info)", kind: "bar", values: report.series.map((bucket) => bucket.filed) },
          { id: "completed", label: "Completed", color: "var(--success)", kind: "bar", values: report.series.map((bucket) => bucket.completed) },
          ...(compareChart ? [{ id: "prior", label: "Prior completed", color: "var(--muted-foreground)", kind: "line" as const, dashed: true, values: report.series.map((_, index) => report.previous_series[index]?.completed ?? null) }] : []),
        ]} renderReadout={(index) => (
          <div className="mt-2 border-border border-t pt-1">
            {Object.entries(report.series[index]?.projects ?? {}).map(([id, counts]) => (
              <div key={id} className="flex items-center justify-between gap-3">
                <span className="max-w-40 truncate">{projectName(id)}</span>
                <span className="tabular-nums">{counts.filed} filed · {counts.completed} completed</span>
              </div>
            ))}
          </div>
        )} />
      </ChartPanel>
      <div className="grid min-w-0 gap-6 lg:grid-cols-2">
        <ChartPanel title="Who filed" actions={action("filedBy", "Filter by filer")}>
          {current.filed === 0 ? <Muted>No issues filed in this range.</Muted> : <ProportionBar ariaLabel="Issues filed by People, Agents and Automation" segments={[...filers, ...(unknownFiled > 0 ? [{ label: "Not recorded", value: unknownFiled, color: "var(--muted-foreground)" }] : [])]} />}
          <div className="flex min-w-0 flex-col">
            {filers.map((filer) => (
              <div key={filer.kind} className="flex min-w-0 items-center gap-3 border-border border-t py-2.5 text-sm">
                <ChartSwatch color={filer.color} /><span className="flex-1">{filer.label}</span>
                <span className="hidden text-muted-foreground text-xs sm:inline">{filer.detail}</span>
                <span className="tabular-nums">{formatCount(filer.value)}</span>
              </div>
            ))}
            {unknownFiled > 0 ? <Muted>Not recorded: {formatCount(unknownFiled)}</Muted> : null}
          </div>
        </ChartPanel>
        <ChartPanel title="Who completed" actions={action("runners", "Filter by runner")}>
          {current.completed === 0 ? <Muted>No issues completed in this range.</Muted> : null}
          <div className="flex min-w-0 flex-col">
            {runners.map((runner, index) => (
              <div key={runner.id ?? index} className="flex min-w-0 items-center gap-3 border-border border-t py-2.5 text-sm">
                {runner.id ? <Link to="/reports" search={reportsSearchParams({ ...search, project: "all", filters: { ...search.filters, runners: [runner.id] } }) as never} className="min-w-0 flex-1 truncate hover:underline">{runner.name || runner.id}</Link> : <span className="min-w-0 flex-1"><Dash /></span>}
                <BarMeter value={runner.current} max={current.completed} color="var(--success)" className="w-1/3" />
                <span className="tabular-nums">{formatCount(runner.current)}</span>
              </div>
            ))}
            <div className="flex min-w-0 items-center gap-3 border-border border-t py-2.5 text-sm">
              <span className="min-w-0 flex-1">Hand-landed by people</span><BarMeter value={people} max={current.completed} color="var(--success)" className="w-1/3" /><span className="tabular-nums">{formatCount(people)}</span>
            </div>
            {unknownCompleted > 0 ? <Muted>Not recorded: {formatCount(unknownCompleted)}</Muted> : null}
          </div>
        </ChartPanel>
      </div>
      <ChartPanel title="By project">
        {report.projects.length === 0 ? <Muted>No projects match these filters.</Muted> : (
          <Table aria-label="Productivity by project">
            <TableHeader><TableRow>{["Project", "Filed", "Completed", "Net backlog", "Changes landed", "Cycle p50", "Cost per completed", "Rework"].map((label, index) => <TableHead key={label} className={index === 0 ? "" : "text-right"}>{label}</TableHead>)}</TableRow></TableHeader>
            <TableBody>{report.projects.map(({ project_id: id, current: metrics }) => (
              <TableRow key={id}>
                <TableCell><Link to="/reports" search={reportsSearchParams({ ...search, project: id, projects: [] }) as never} onClick={() => onProjectChange?.(id)} className="whitespace-nowrap font-medium hover:underline">{projectName(id)}</Link></TableCell>
                <TableCell className="text-right tabular-nums">{formatCount(metrics.filed)}</TableCell>
                <TableCell className="text-right tabular-nums">{formatCount(metrics.completed)}</TableCell>
                <TableCell className="text-right tabular-nums">{signed(metrics.net_backlog)}</TableCell>
                <TableCell className="text-right tabular-nums">{formatCount(metrics.changes_landed)}</TableCell>
                <TableCell className="text-right whitespace-nowrap tabular-nums">{metrics.cycle_p50_seconds === null ? <Dash /> : duration(metrics.cycle_p50_seconds)}</TableCell>
                <TableCell className="text-right tabular-nums">{metrics.cost_per_completed_usd === null ? <Dash /> : formatUsd(metrics.cost_per_completed_usd)}</TableCell>
                <TableCell className="text-right tabular-nums">{metrics.rework_percent === null ? <Dash /> : percentText(metrics.rework_percent)}</TableCell>
              </TableRow>
            ))}</TableBody>
          </Table>
        )}
      </ChartPanel>
    </div>
  );
}
