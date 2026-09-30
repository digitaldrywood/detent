import React from "react";

import { selectTriggerVariants } from "../../components/ui/select.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import { cn } from "../../lib/utils.ts";

export interface SelectOption {
  readonly value: string;
  readonly label: string;
  readonly disabled?: boolean;
}

export function NativeSelect({
  value,
  onValueChange,
  options,
  disabled = false,
  className,
  size = "sm",
  ...props
}: Omit<React.ComponentProps<"select">, "value" | "onChange" | "size"> & {
  readonly value: string;
  readonly onValueChange: (value: string) => void;
  readonly options: readonly SelectOption[];
  readonly disabled?: boolean;
  readonly size?: "sm" | "default";
}): React.ReactElement {
  return (
    <select
      {...props}
      value={value}
      disabled={disabled}
      onChange={(event) => onValueChange(event.currentTarget.value)}
      className={cn(
        selectTriggerVariants({ size }),
        "min-w-[170px] appearance-none bg-[length:0] pe-8",
        className,
      )}
    >
      {options.map((option) => (
        <option key={option.value} value={option.value} disabled={option.disabled}>
          {option.label}
        </option>
      ))}
    </select>
  );
}

/**
 * A toggle with its own accessible name. The artifact draws a bare pill with
 * the name only in the row's title; a switch with no name of its own is
 * unusable by anything that does not read the row around it, so the name is
 * attached here and hidden visually.
 */
export function ToggleControl({
  checked,
  onCheckedChange,
  label,
  disabled = false,
  id,
}: {
  readonly checked: boolean;
  readonly onCheckedChange: (checked: boolean) => void;
  readonly label: string;
  readonly disabled?: boolean;
  readonly id?: string;
}): React.ReactElement {
  return (
    <Switch
      id={id}
      aria-label={label}
      checked={checked}
      disabled={disabled}
      onCheckedChange={(next) => onCheckedChange(next)}
    />
  );
}

/** The artifact's `.pathv`: a monospace value with a copy affordance. */
export function PathValue({ value }: { readonly value: string }): React.ReactElement {
  const [copied, setCopied] = React.useState(false);
  return (
    <span className="flex items-center gap-2 font-mono text-xs text-muted-foreground">
      <span className="truncate">{value}</span>
      <button
        type="button"
        className="shrink-0 rounded-sm p-0.5 text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
        aria-label={copied ? `Copied ${value}` : `Copy ${value}`}
        onClick={() => {
          void globalThis.navigator?.clipboard?.writeText(value).catch(() => undefined);
          setCopied(true);
        }}
      >
        <svg
          aria-hidden="true"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          className="size-3.5"
        >
          <rect x="9" y="9" width="11" height="11" rx="2" />
          <path d="M5 15V5a2 2 0 0 1 2-2h10" />
        </svg>
      </button>
      <span className="sr-only" role="status">
        {copied ? "Copied" : ""}
      </span>
    </span>
  );
}

/**
 * The one place a refused or failed mutation is allowed to speak: next to the
 * control that asked for it. `last_owner` is called out because it is a rule
 * rather than a mistake — the reader has not done anything wrong, the
 * organization simply keeps an owner.
 */
export function ControlError({ message }: { readonly message: string | null }): React.ReactElement | null {
  if (message === null) return null;
  return (
    <p role="alert" className="text-[13px] text-destructive-foreground">
      {message}
    </p>
  );
}

/** A status dot with a word beside it: colour is never the only carrier. */
export function StatusDot({
  tone,
  pulse = false,
  className,
}: {
  readonly tone: "ok" | "warn" | "error" | "idle";
  readonly pulse?: boolean;
  readonly className?: string;
}): React.ReactElement {
  return (
    <span
      aria-hidden="true"
      className={cn(
        "inline-block size-2 shrink-0 rounded-full",
        tone === "ok" && "bg-success",
        tone === "warn" && "bg-warning",
        tone === "error" && "bg-destructive",
        tone === "idle" && "bg-muted-foreground/60",
        pulse && "motion-safe:animate-status-pulse",
        className,
      )}
    />
  );
}
