import React from "react";
import { cn } from "../../../lib/utils.ts";

export const PILL_TONE = {
  mute: "border-border bg-muted text-muted-foreground",
  ok: "border-transparent bg-success/12 text-success-foreground",
  warn: "border-transparent bg-warning-surface text-warning-foreground",
  err: "border-transparent bg-error-surface text-error-foreground",
  info: "border-transparent bg-info/12 text-info-foreground",
} as const;

export type PillTone = keyof typeof PILL_TONE;

export function Pill({
  tone = "mute",
  className,
  children,
  ...rest
}: {
  tone?: PillTone;
  className?: string;
  children: React.ReactNode;
} & React.HTMLAttributes<HTMLSpanElement>): React.ReactElement {
  return (
    <span
      className={cn(
        "inline-flex h-5 shrink-0 items-center gap-1.5 whitespace-nowrap rounded-md border px-1.5 font-medium text-[11px] leading-none [&_svg]:size-3",
        PILL_TONE[tone],
        className,
      )}
      {...rest}
    >
      {children}
    </span>
  );
}
