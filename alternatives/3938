import React from "react";
import type { DiagnosticsReport } from "../../contracts/diagnostics.ts";

export function CapacityChart({
  points,
}: {
  readonly points: NonNullable<DiagnosticsReport["capacity"]>;
}): React.ReactElement {
  const width = 640,
    height = 180,
    left = 0,
    bottom = 180,
    right = 640,
    top = 0;
  const maxSlots = Math.max(1, ...points.map((point) => point.slots));
  const maxTodo = Math.max(1, ...points.map((point) => point.todo ?? 0));
  const x = (index: number) =>
    left + (index / Math.max(1, points.length - 1)) * (right - left);
  const y = (value: number, maximum: number) =>
    bottom - (value / maximum) * (bottom - top);
  const slots = points
    .map((point, index) => `${x(index)},${y(point.slots, maxSlots)}`)
    .join(" ");
  const queue: string[] = [];
  let penDown = false;
  for (const [index, point] of points.entries()) {
    if (point.todo === null) {
      penDown = false;
      continue;
    }
    queue.push(`${penDown ? "L" : "M"}${x(index)},${y(point.todo, maxTodo)}`);
    penDown = true;
  }
  const hour = (value: string) =>
    new Date(value).toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "numeric",
    });
  return (
    <figure className="min-w-0">
      <div className="grid grid-cols-[2rem_minmax(0,1fr)_2rem] gap-x-1 gap-y-2 text-xs text-muted-foreground">
        <div
          aria-hidden
          className="flex h-36 flex-col justify-between text-right sm:h-40"
        >
          {[1, 0.5, 0].map((fraction) => (
            <span key={fraction}>{(fraction * maxSlots).toFixed(1)}</span>
          ))}
        </div>
        <svg
          role="img"
          aria-label="Hourly slots in use and Todo queue depth"
          viewBox={`0 0 ${width} ${height}`}
          preserveAspectRatio="none"
          className="h-36 w-full sm:h-40"
        >
          <title>
            Hourly average slots in use, with Todo depth at the end of each hour
          </title>
          {[0, 0.5, 1].map((fraction) => (
            <g key={fraction}>
              <line
                x1={left}
                x2={right}
                y1={y(fraction * maxSlots, maxSlots)}
                y2={y(fraction * maxSlots, maxSlots)}
                className="stroke-border"
              />
            </g>
          ))}
          <polygon
            points={`${left},${bottom} ${slots} ${right},${bottom}`}
            className="fill-info/10"
          />
          <polyline
            points={slots}
            fill="none"
            className="stroke-info-foreground"
            strokeWidth="2"
          />
          <path
            d={queue.join(" ")}
            fill="none"
            className="stroke-warning-foreground"
            strokeWidth="2"
          />
        </svg>
        <div aria-hidden className="flex h-36 flex-col justify-between sm:h-40">
          {[1, 0.5, 0].map((fraction) => (
            <span key={fraction}>{Math.round(fraction * maxTodo)}</span>
          ))}
        </div>
        {points.length > 0 ? (
          <div className="col-start-2 flex flex-wrap justify-between gap-2">
            <span>{hour(points[0]!.hour)}</span>
            <span>{hour(points.at(-1)!.hour)}</span>
          </div>
        ) : null}
      </div>
      <figcaption className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
        <span className="flex items-center gap-1.5">
          <span className="h-0.5 w-4 bg-info-foreground" />
          Slots · left axis
        </span>
        <span className="flex items-center gap-1.5">
          <span className="h-0.5 w-4 bg-warning-foreground" />
          Todo · right axis
        </span>
      </figcaption>
    </figure>
  );
}
