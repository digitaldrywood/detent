import React from "react";

import { Chart, ChartLegend, ChartPanel, type ChartCategory, type ChartLegendItem, type ChartMarker, type ChartSeries } from "../../components/ui/chart.tsx";
import { MetricTile, MetricTileGroup } from "../../components/ui/metric-tile.tsx";
import { Toggle as GroupToggle, ToggleGroup } from "../../components/ui/toggle-group.tsx";
import type { RunnerHostHour } from "../../contracts/account.ts";
import { AccountError } from "../account/api.ts";

const GIB = 1024 ** 3;
const RANGES = [["24h", "24h", 24], ["7d", "7d", 168], ["30d", "30d", 720]] as const;
type Range = (typeof RANGES)[number][0];

export type LoadHostHistory = (runner: string, from: string, to: string) => Promise<readonly RunnerHostHour[]>;

function rangeBounds(range: Range): { from: string; to: string } {
  const hours = RANGES.find(([value]) => value === range)![2];
  const end = Math.floor(Date.now() / 3_600_000) * 3_600_000 + 3_600_000;
  return { from: new Date(end - hours * 3_600_000).toISOString(), to: new Date(end).toISOString() };
}

function cpu(hour: RunnerHostHour): number | null {
  return hour.cpu_sample_count > 0 && hour.cpu_busy_average_percent !== undefined ? hour.cpu_busy_average_percent : null;
}

function loadPerCore(hour: RunnerHostHour): number | null {
  return hour.load_sample_count > 0 && hour.logical_cores > 0 ? hour.load1_max / hour.logical_cores : null;
}

function percentile(values: readonly number[], p: number): number | null {
  if (values.length === 0) return null;
  const sorted = values.toSorted((a, b) => a - b);
  return sorted[Math.ceil(p * sorted.length) - 1]!;
}

function zoned(timezone: string): string {
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: timezone });
    return timezone;
  } catch {
    return "UTC";
  }
}

export function HostLoad({ runnerId, load, timezone }: {
  readonly runnerId: string;
  readonly load: LoadHostHistory;
  readonly timezone: string;
}): React.ReactElement | null {
  const [range, setRange] = React.useState<Range>("7d");
  const [hours, setHours] = React.useState<readonly RunnerHostHour[] | undefined>(undefined);
  const [error, setError] = React.useState<"hidden" | string | null>(null);
  const zone = zoned(timezone);

  React.useEffect(() => {
    let current = true;
    const { from, to } = rangeBounds(range);
    setHours(undefined);
    load(runnerId, from, to).then(
      (value) => { if (current) { setHours(value); setError(null); } },
      (cause: unknown) => { if (current) setError(cause instanceof AccountError && cause.isAccessError ? "hidden" : "Host load could not be loaded."); },
    );
    return () => { current = false; };
  }, [runnerId, range, load]);

  if (error === "hidden") return null;

  const rows = hours ?? [];
  const day = (hour: string) => new Date(hour).toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: zone });
  const cpuValues = rows.map(cpu);
  const busy = cpuValues.filter((value): value is number => value !== null);
  const loads = rows.map(loadPerCore);
  const peakLoad = Math.max(0, ...loads.filter((value): value is number => value !== null));
  const memoryHours = rows.filter((hour) => hour.memory_sample_count > 0);
  const minFree = memoryHours.length > 0 ? Math.min(...memoryHours.map((hour) => hour.memory_available_min_bytes)) : null;
  const totalMemory = Math.max(0, ...memoryHours.map((hour) => hour.memory_total_bytes));
  const lowDisk = rows.filter((hour) => hour.disk_sample_count > 0 && hour.disk_free_min_bytes * 10 < hour.disk_total_min_bytes)
    .reduce<RunnerHostHour | undefined>((lowest, hour) => lowest === undefined || hour.disk_free_min_bytes < lowest.disk_free_min_bytes ? hour : lowest, undefined);
  const categories: ChartCategory[] = rows.map((hour, index) => {
    const at = new Date(hour.hour);
    const hourOfDay = Number(at.toLocaleString("en-US", { hour: "numeric", hourCycle: "h23", timeZone: zone }));
    const newDay = index === 0 || day(rows[index - 1]!.hour) !== day(hour.hour);
    const dayOfMonth = Number(at.toLocaleString("en-US", { day: "numeric", timeZone: zone }));
    const shortLabel = range === "24h"
      ? index === 0 || hourOfDay % 6 === 0 ? at.toLocaleTimeString("en-US", { hour: "numeric", timeZone: zone }) : ""
      : newDay && hourOfDay === 0 && (range === "7d" ? dayOfMonth % 2 === 0 : dayOfMonth % 5 === 0) ? day(hour.hour) : "";
    return { key: hour.hour, label: at.toLocaleString("en-US", { month: "short", day: "numeric", hour: "numeric", timeZone: zone }), shortLabel };
  });
  const cpuSeries: ChartSeries[] = [
    { id: "cpu", label: "CPU busy", color: "var(--primary)", kind: "bar", stack: "cpu", values: cpuValues.map((value) => value !== null && value < 90 ? value : null) },
    { id: "saturated", label: "90% or more", color: "var(--warning)", kind: "bar", stack: "cpu", values: cpuValues.map((value) => value !== null && value >= 90 ? value : null) },
    { id: "cpu-line", label: "90% saturation", color: "var(--muted-foreground)", kind: "line", dashed: true, values: cpuValues.map(() => 90) },
  ];
  const loadSeries: ChartSeries[] = [
    { id: "load", label: "Load per core", color: "var(--primary)", kind: "line", values: loads },
    { id: "load-line", label: "All cores busy", color: "var(--muted-foreground)", kind: "line", dashed: true, values: loads.map(() => 1) },
  ];
  const memorySeries: ChartSeries[] = [
    { id: "swap", label: "Swap used", color: "var(--warning)", kind: "bar", values: rows.map((hour) => hour.swap_used_max_bytes / GIB) },
    { id: "free", label: "Lowest free memory", color: "var(--success)", kind: "line", values: rows.map((hour) => hour.memory_sample_count > 0 ? hour.memory_available_min_bytes / GIB : null) },
  ];
  const pressure: ChartMarker = { label: "Pressure warning", color: "var(--destructive)", at: rows.map((hour) => hour.pressure_warn_count + hour.pressure_critical_count > 0) };
  const legend = (series: readonly ChartSeries[]): ChartLegendItem[] => series.map(({ label, color, dashed }) => ({ label, color, ...(dashed ? { dashed } : {}) }));
  const percent = (value: number) => `${Math.round(value)}%`;
  const gigabytes = (value: number) => `${Math.round(value)} GB`;

  return (
    <section className="space-y-3" aria-label="Host load" data-testid="host-load">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium">Host load</h3>
        <ToggleGroup aria-label="Host load range" value={[range]} onValueChange={(value) => { const next = RANGES.find(([option]) => option === value[0]); if (next) setRange(next[0]); }}>
          {RANGES.map(([value, label]) => <GroupToggle key={value} value={value}>{label}</GroupToggle>)}
        </ToggleGroup>
      </div>
      {typeof error === "string" ? <p role="status" className="text-xs text-muted-foreground">{error}</p> : hours === undefined ? <p role="status" className="text-xs text-muted-foreground">Loading host load</p> : hours.length === 0 ? <p className="text-xs text-muted-foreground">No host history in this range yet.</p> : <>
        <MetricTileGroup className="sm:grid-cols-4">
          <MetricTile label="CPU p95" value={busy.length > 0 ? percent(percentile(busy, 0.95)!) : null} />
          <MetricTile label="Load / core peak" value={loads.some((value) => value !== null) ? (Math.round(peakLoad * 10) / 10).toString() : null} />
          <MetricTile label="Min free memory" value={minFree === null ? null : gigabytes(minFree / GIB)} />
          <MetricTile label="Hours ≥ 90% CPU" value={busy.filter((value) => value >= 90).length.toString()} />
        </MetricTileGroup>
        <div className="grid min-w-0 gap-3 lg:grid-cols-3">
        <ChartPanel title="CPU busy" actions={<span className="text-xs text-muted-foreground">hourly average</span>}>
          <Chart ariaLabel="CPU busy percent by hour" categories={categories} series={cpuSeries} height={140} formatValue={percent} />
          <ChartLegend items={legend(cpuSeries)} />
        </ChartPanel>
        <ChartPanel title="Load per core" actions={<span className="text-xs text-muted-foreground">hourly peak of load ÷ cores</span>}>
          <Chart ariaLabel="Peak load per core by hour" categories={categories} series={loadSeries} height={140} formatValue={(value) => `${Math.round(value * 10) / 10}×`} />
          <ChartLegend items={legend(loadSeries)} />
        </ChartPanel>
        <ChartPanel title="Memory" actions={totalMemory > 0 ? <span className="text-xs text-muted-foreground">{gigabytes(totalMemory / GIB)} total</span> : undefined}>
          <Chart ariaLabel="Free memory and swap by hour" categories={categories} series={memorySeries} height={140} formatValue={gigabytes} marker={pressure} />
          <ChartLegend items={[...legend(memorySeries), { label: pressure.label, color: pressure.color }]} />
        </ChartPanel>
        </div>
        {lowDisk ? <p className="border-l-2 border-warning pl-2.5 text-xs">Free disk fell to {(lowDisk.disk_free_min_bytes / 1e9).toFixed(1)} GB on {day(lowDisk.hour)}. Low disk is reported here and does not change the suggestion.</p> : null}
      </>}
    </section>
  );
}
