import * as React from "react";

import { cn } from "~/lib/utils";

export interface ChartCategory {
  readonly key: string;
  readonly label: string;
  readonly shortLabel?: string;
}

export interface ChartSeries {
  readonly id: string;
  readonly label: string;
  readonly color: string;
  readonly kind: "bar" | "line" | "band";
  readonly values: readonly (number | null)[];
  readonly upper?: readonly (number | null)[];
  readonly stack?: string;
  readonly dashed?: boolean;
}

export interface ChartMarker {
  readonly label: string;
  readonly color: string;
  readonly at: readonly boolean[];
}

export interface ChartLegendItem {
  readonly label: string;
  readonly color: string;
  readonly dashed?: boolean;
}

const AXIS_MIN_WIDTH = 44;
const AXIS_CHAR_WIDTH = 6.4;
const LABEL_HEIGHT = 24;
const TOP = 8;
const MIN_LABEL_SPACING = 52;

function niceStep(raw: number): number {
  if (raw <= 0) return 1;
  const power = 10 ** Math.floor(Math.log10(raw));
  const fraction = raw / power;
  const nice = fraction <= 1 ? 1 : fraction <= 2 ? 2 : fraction <= 2.5 ? 2.5 : fraction <= 5 ? 5 : 10;
  return nice * power;
}

/** Evenly spaced axis ticks from zero that cover `max`. */
export function chartTicks(max: number, count = 3): readonly number[] {
  const step = niceStep(max / count);
  const ticks = [0];
  while (ticks[ticks.length - 1]! < max - step * 0.0001 || ticks.length < 2) ticks.push(ticks.length * step);
  return ticks;
}

interface Column {
  readonly series: readonly ChartSeries[];
}

function columnsOf(series: readonly ChartSeries[]): readonly Column[] {
  const columns: { key: string; series: ChartSeries[] }[] = [];
  for (const item of series) {
    if (item.kind !== "bar") continue;
    const key = item.stack ?? item.id;
    const existing = columns.find((column) => column.key === key);
    if (existing) existing.series.push(item);
    else columns.push({ key, series: [item] });
  }
  return columns;
}

function useWidth(fallback: number): [React.RefObject<HTMLDivElement | null>, number] {
  const ref = React.useRef<HTMLDivElement | null>(null);
  const [width, setWidth] = React.useState(fallback);
  React.useLayoutEffect(() => {
    const element = ref.current;
    if (!element) return;
    const measure = () => {
      const next = element.getBoundingClientRect().width;
      if (next > 0) setWidth(Math.round(next));
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  return [ref, width];
}

function linePath(points: readonly ({ x: number; y: number } | null)[]): string {
  let path = "";
  let open = false;
  for (const point of points) {
    if (point === null) {
      open = false;
      continue;
    }
    path += `${open ? "L" : "M"}${point.x.toFixed(1)},${point.y.toFixed(1)}`;
    open = true;
  }
  return path;
}

function ChartLegend({ items, className }: { readonly items: readonly ChartLegendItem[]; readonly className?: string }) {
  return (
    <ul className={cn("flex flex-wrap items-center gap-x-3.5 gap-y-1 text-muted-foreground text-xs", className)} data-slot="chart-legend">
      {items.map((item) => (
        <li key={item.label} className="inline-flex items-center gap-1.5">
          <ChartSwatch color={item.color} dashed={item.dashed} />
          {item.label}
        </li>
      ))}
    </ul>
  );
}

function ChartSwatch({ color, dashed = false }: { readonly color: string; readonly dashed?: boolean | undefined }) {
  return (
    <span
      aria-hidden
      className={cn("inline-block size-2.5 shrink-0 rounded-xs", dashed && "border border-dashed")}
      style={dashed ? { borderColor: color } : { background: color }}
    />
  );
}

/**
 * Bars (grouped or stacked), lines and a range band over shared categories.
 * The hover or keyboard readout lists every series for the focused category.
 */
function Chart({
  categories,
  series,
  height = 220,
  ariaLabel,
  formatValue = (value: number) => String(Math.round(value * 10) / 10),
  className,
  marker,
  renderReadout,
}: {
  readonly categories: readonly ChartCategory[];
  readonly series: readonly ChartSeries[];
  readonly height?: number;
  readonly ariaLabel: string;
  readonly formatValue?: (value: number) => string;
  readonly className?: string;
  readonly marker?: ChartMarker;
  readonly renderReadout?: (index: number) => React.ReactNode;
}) {
  const [ref, width] = useWidth(640);
  const [active, setActive] = React.useState<number | null>(null);
  const count = Math.max(1, categories.length);
  const columns = columnsOf(series);
  const totals = categories.map((_, index) => {
    const stacked = columns.map((column) => column.series.reduce((sum, item) => sum + (item.values[index] ?? 0), 0));
    const lines = series.filter((item) => item.kind !== "bar").flatMap((item) => [item.values[index] ?? 0, item.upper?.[index] ?? 0]);
    return Math.max(0, ...stacked, ...lines);
  });
  const ticks = chartTicks(Math.max(...totals, 0) || 1);
  const top = ticks[ticks.length - 1] ?? 1;
  const AXIS_WIDTH = Math.max(AXIS_MIN_WIDTH, Math.ceil(Math.max(...ticks.map((tick) => formatValue(tick).length)) * AXIS_CHAR_WIDTH) + 8);
  const plotWidth = Math.max(1, width - AXIS_WIDTH);
  const plotBottom = height - LABEL_HEIGHT;
  const slot = plotWidth / count;
  const y = (value: number) => plotBottom - (value / top) * (plotBottom - TOP);
  const center = (index: number) => AXIS_WIDTH + slot * (index + 0.5);
  const primaryWidth = Math.max(1, Math.min(56, slot * (columns.length > 1 ? 0.32 : 0.36)));
  const secondaryWidth = Math.max(1, Math.min(24, slot * 0.13));
  const gap = slot > 24 ? 4 : 1;
  const groupWidth = columns.reduce((sum, _, index) => sum + (index === 0 ? primaryWidth : secondaryWidth + gap), 0);
  const useShort = categories.some((category) => category.shortLabel !== undefined) && slot < 44;
  const labelEvery = Math.max(1, Math.ceil((useShort ? 20 : MIN_LABEL_SPACING) / slot));
  const sparseLabels = useShort && categories.some((category) => category.shortLabel === "") && categories.some((category) => Boolean(category.shortLabel));
  const empty = series.every((item) => item.values.every((value) => value === null || value === 0));

  const readout = active === null ? null : categories[active];
  return (
    <div ref={ref} className={cn("relative w-full min-w-0", className)} data-slot="chart">
      <svg
        width="100%"
        height={height}
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={ariaLabel}
        tabIndex={0}
        className="block overflow-visible rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
        onKeyDown={(event) => {
          if (event.key === "ArrowRight") setActive((index) => Math.min(count - 1, (index ?? -1) + 1));
          else if (event.key === "ArrowLeft") setActive((index) => Math.max(0, (index ?? count) - 1));
          else if (event.key === "Escape") setActive(null);
          else return;
          event.preventDefault();
        }}
        onBlur={() => setActive(null)}
        onMouseLeave={() => setActive(null)}
      >
        <g stroke="var(--border)" strokeWidth={1}>
          {ticks.map((tick) => (
            <line key={tick} x1={AXIS_WIDTH} x2={width} y1={y(tick)} y2={y(tick)} />
          ))}
        </g>
        <g fill="var(--muted-foreground)" fontSize={11}>
          {ticks.map((tick) => (
            <text key={tick} x={0} y={y(tick) + 4} className="tabular-nums">
              {formatValue(tick)}
            </text>
          ))}
        </g>
        {marker?.at.map((on, index) => on ? <rect key={`marker-${categories[index]?.key ?? index}`} x={AXIS_WIDTH + slot * index} y={TOP} width={slot} height={plotBottom - TOP} fill={marker.color} opacity={0.16} /> : null)}
        {active === null ? null : (
          <rect x={AXIS_WIDTH + slot * active} y={TOP} width={slot} height={plotBottom - TOP} fill="var(--accent)" opacity={0.6} />
        )}
        {series
          .filter((item) => item.kind === "band")
          .map((item) => {
            const upper = categories.map((_, index) => {
              const value = item.upper?.[index];
              return value == null ? null : { x: center(index), y: y(value) };
            });
            const lower = categories.map((_, index) => {
              const value = item.values[index];
              return value == null ? null : { x: center(index), y: y(value) };
            });
            const points = [...upper.filter((point) => point !== null), ...lower.filter((point) => point !== null).toReversed()];
            return (
              <polygon
                key={item.id}
                points={points.map((point) => `${point!.x.toFixed(1)},${point!.y.toFixed(1)}`).join(" ")}
                fill={item.color}
                opacity={0.18}
              />
            );
          })}
        {categories.map((category, index) => {
          let x = center(index) - groupWidth / 2;
          return (
            <g key={category.key}>
              {columns.map((column, columnIndex) => {
                const barWidth = columnIndex === 0 ? primaryWidth : secondaryWidth;
                const left = columnIndex === 0 ? x : x + gap;
                x = left + barWidth;
                let base = 0;
                const stacked = column.series.length > 1;
                return column.series.map((item) => {
                  const value = item.values[index] ?? 0;
                  const from = base;
                  base += value;
                  if (value <= 0) return null;
                  return (
                    <rect
                      key={item.id}
                      x={left}
                      y={y(base)}
                      width={barWidth}
                      height={Math.max(0, y(from) - y(base))}
                      rx={!stacked && barWidth >= 8 ? 3 : 0}
                      fill={item.color}
                    />
                  );
                });
              })}
            </g>
          );
        })}
        {series
          .filter((item) => item.kind === "line" || item.kind === "band")
          .map((item) => (
            <path
              key={item.id}
              d={linePath(categories.map((_, index) => {
                const value = item.values[index];
                return value == null ? null : { x: center(index), y: y(value) };
              }))}
              fill="none"
              stroke={item.color}
              strokeWidth={item.kind === "band" ? 2.5 : 1.5}
              strokeDasharray={item.dashed ? "4 4" : undefined}
              strokeLinejoin="round"
            />
          ))}
        <g fill="var(--muted-foreground)" fontSize={11} textAnchor="middle">
          {categories.map((category, index) =>
            (sparseLabels ? Boolean(category.shortLabel) : index % labelEvery === 0) ? (
              <text key={category.key} x={center(index)} y={height - 6}>
                {useShort ? (category.shortLabel ?? category.label) : category.label}
              </text>
            ) : null,
          )}
        </g>
        {categories.map((category, index) => (
          <rect
            key={category.key}
            x={AXIS_WIDTH + slot * index}
            y={TOP}
            width={slot}
            height={plotBottom - TOP}
            fill="transparent"
            onMouseEnter={() => setActive(index)}
          />
        ))}
        {empty ? (
          <text x={AXIS_WIDTH + plotWidth / 2} y={(TOP + plotBottom) / 2} textAnchor="middle" fill="var(--muted-foreground)" fontSize={12}>
            Nothing recorded in this range
          </text>
        ) : null}
      </svg>
      {readout === undefined || readout === null || active === null ? null : (
        <div
          role="status"
          className={cn("pointer-events-none absolute top-0 z-10 min-w-36 rounded-md border border-border bg-popover px-2.5 py-1.5 text-popover-foreground text-xs shadow-md", renderReadout !== undefined && "w-full max-w-80")}
          style={{
            left: renderReadout === undefined
              ? Math.min(Math.max(0, center(active) - 72), Math.max(0, width - 160))
              : Math.min(Math.max(0, center(active) - 160), Math.max(0, width - 320)),
          }}
        >
          <div className="mb-1 font-medium">{readout.label}</div>
          {marker?.at[active] ? (
            <div className="flex items-center gap-1.5 text-muted-foreground">
              <ChartSwatch color={marker.color} />
              {marker.label}
            </div>
          ) : null}
          {series.map((item) => {
            const value = item.values[active];
            const upper = item.upper?.[active];
            return (
              <div key={item.id} className="flex items-center justify-between gap-3">
                <span className="inline-flex items-center gap-1.5 text-muted-foreground">
                  <ChartSwatch color={item.color} dashed={item.dashed} />
                  {item.label}
                </span>
                <span className="tabular-nums">
                  {value == null ? "—" : formatValue(value)}
                  {upper == null ? "" : ` – ${formatValue(upper)}`}
                </span>
              </div>
            );
          })}
          {renderReadout?.(active)}
        </div>
      )}
    </div>
  );
}

/** One horizontal bar split into proportional segments, each a labelled share. */
function ProportionBar({
  segments,
  ariaLabel,
  className,
}: {
  readonly segments: readonly { readonly label: string; readonly value: number; readonly color: string }[];
  readonly ariaLabel: string;
  readonly className?: string;
}) {
  const total = segments.reduce((sum, segment) => sum + Math.max(0, segment.value), 0);
  return (
    <div role="img" aria-label={ariaLabel} className={cn("flex h-3.5 w-full gap-0.5 overflow-hidden rounded-sm bg-muted", className)} data-slot="proportion-bar">
      {total > 0
        ? segments.map((segment) =>
            segment.value > 0 ? (
              <span key={segment.label} title={segment.label} style={{ flex: segment.value, background: segment.color }} />
            ) : null,
          )
        : null}
    </div>
  );
}

/** A labelled horizontal bar whose length is `value / max`. */
function BarMeter({
  value,
  max,
  color,
  className,
}: {
  readonly value: number;
  readonly max: number;
  readonly color: string;
  readonly className?: string;
}) {
  const share = max > 0 ? Math.min(100, Math.max(0, (100 * value) / max)) : 0;
  return (
    <span aria-hidden className={cn("block h-2 w-full overflow-hidden rounded-full bg-muted", className)} data-slot="bar-meter">
      <span className="block h-full rounded-full" style={{ width: `${share}%`, background: color }} />
    </span>
  );
}

/** A titled chart card: heading and actions, then the chart and any rows below it. */
function ChartPanel({
  title,
  actions,
  footnote,
  children,
  className,
  ...props
}: Omit<React.ComponentProps<"section">, "title"> & {
  readonly title: string;
  readonly actions?: React.ReactNode;
  readonly footnote?: React.ReactNode;
}) {
  return (
    <section
      aria-label={title}
      className={cn("flex min-w-0 flex-col gap-3.5 rounded-xl border border-border bg-card px-4 py-4 text-card-foreground sm:px-5", className)}
      data-slot="chart-panel"
      {...props}
    >
      <div className="flex min-w-0 flex-wrap items-center justify-between gap-x-3 gap-y-2">
        <h2 className="font-medium text-sm">{title}</h2>
        {actions}
      </div>
      {children}
      {footnote === undefined ? null : <p className="text-muted-foreground text-xs">{footnote}</p>}
    </section>
  );
}

export { BarMeter, Chart, ChartLegend, ChartPanel, ChartSwatch, ProportionBar };
