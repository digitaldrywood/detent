import { cva, type VariantProps } from "class-variance-authority";
import type * as React from "react";

import { cn } from "~/lib/utils";

const metricTileVariants = cva("flex min-w-0 flex-col gap-1", {
  defaultVariants: { size: "default" },
  variants: {
    size: {
      default: "gap-1.5 p-4",
      compact: "",
    },
  },
});

const VALUE_CLASS = {
  default: "font-semibold tabular-nums tracking-tight text-xl sm:text-2xl",
  compact: "font-semibold tabular-nums tracking-tight text-xl",
} as const;

/** The change against a prior period. `null` means the prior value is not recorded. */
export type MetricDelta = {
  readonly text: string;
  readonly direction: "up" | "down" | "flat";
  readonly tone: "good" | "bad" | "neutral";
} | null;

const NOT_RECORDED = "Not recorded";

function MetricTileGroup({ className, ...props }: React.ComponentProps<"section">) {
  return (
    <section
      className={cn(
        "grid min-w-0 grid-cols-2 gap-px overflow-hidden rounded-xl border border-border bg-border max-sm:[&>:last-child:nth-child(odd)]:col-span-2 sm:grid-cols-[repeat(auto-fit,minmax(10rem,1fr))] *:bg-card",
        className,
      )}
      data-slot="metric-tile-group"
      {...props}
    />
  );
}

function MetricTile({
  label,
  value,
  delta,
  detail,
  size,
  className,
  ...props
}: Omit<React.ComponentProps<"div">, "children"> &
  VariantProps<typeof metricTileVariants> & {
    readonly label: React.ReactNode;
    readonly value: React.ReactNode | null;
    readonly delta?: MetricDelta;
    readonly detail?: React.ReactNode;
  }) {
  return (
    <div className={cn(metricTileVariants({ size }), className)} data-slot="metric-tile" {...props}>
      {size === "compact" ? null : <span className="text-muted-foreground text-xs">{label}</span>}
      {value === null ? (
        <span className={cn(VALUE_CLASS[size ?? "default"], "text-muted-foreground")} title={NOT_RECORDED}>
          <span aria-hidden>—</span>
          <span className="sr-only">{NOT_RECORDED}</span>
        </span>
      ) : (
        <span className={VALUE_CLASS[size ?? "default"]}>{value}</span>
      )}
      {size === "compact" ? <span className="text-muted-foreground text-xs">{label}</span> : null}
      {delta === undefined ? null : delta === null ? (
        <span className="text-muted-foreground text-xs tabular-nums" title={NOT_RECORDED}>
          <span aria-hidden>— vs prior</span>
          <span className="sr-only">Change: {NOT_RECORDED}</span>
        </span>
      ) : (
        <span
          className={cn(
            "text-xs tabular-nums",
            delta.tone === "good" && "text-success-foreground",
            delta.tone === "bad" && "text-destructive-foreground",
            delta.tone === "neutral" && "text-muted-foreground",
          )}
        >
          {delta.direction === "flat" ? null : <span aria-hidden>{delta.direction === "up" ? "▲ " : "▼ "}</span>}
          {delta.text}
        </span>
      )}
      {detail === undefined ? null : <span className="text-muted-foreground text-xs">{detail}</span>}
    </div>
  );
}

export { MetricTile, MetricTileGroup };
