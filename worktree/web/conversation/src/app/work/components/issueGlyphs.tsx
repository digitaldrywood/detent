import { CircleDashedIcon } from "lucide-react";
import React from "react";

import { cn } from "../../../lib/utils.ts";

/** `unstarted` is a hollow ring, `started` a half ring, `completed` solid. */
export function StateGlyph({
  category,
  className,
}: {
  readonly category: string;
  readonly className?: string;
}): React.ReactElement {
  const tone = category === "completed" ? "--color-success" : "--color-primary";
  return (
    <span
      aria-hidden
      data-testid="state-glyph"
      data-category={category}
      className={cn("size-3.5 shrink-0 rounded-full border-2 box-border", className)}
      style={{
        borderColor: `var(${tone})`,
        background:
          category === "completed"
            ? `var(${tone})`
            : category === "started"
              ? `conic-gradient(var(${tone}) 0 50%, transparent 50% 100%)`
              : "transparent",
      }}
    />
  );
}

/**
 * Linear's priority glyphs.
 *
 * Three rising bars filled to the level for a level; an exclamation in a
 * filled square for Urgent, which is the one Linear singles out; three dashes
 * for no priority at all. Detent's names are Urgent / High / Normal / Low
 * (`PRIORITY_NAMES`), so the middle bar count is Detent's vocabulary over
 * Linear's shapes.
 */
export function PriorityGlyph({
  priority,
  className,
}: {
  readonly priority: string | null;
  readonly className?: string;
}): React.ReactElement {
  if (priority === "Urgent") {
    return (
      <span
        aria-hidden
        data-testid="priority-glyph"
        data-priority="Urgent"
        className={cn("flex size-3.5 shrink-0 items-center justify-center", className)}
      >
        <svg viewBox="0 0 14 14" className="size-3.5 text-destructive" fill="none">
          <rect x="0.5" y="0.5" width="13" height="13" rx="3" fill="currentColor" />
          <rect x="6.25" y="3" width="1.5" height="5" rx="0.75" fill="var(--color-background)" />
          <rect x="6.25" y="9.25" width="1.5" height="1.75" rx="0.75" fill="var(--color-background)" />
        </svg>
      </span>
    );
  }
  if (priority === null) {
    return (
      <span
        aria-hidden
        data-testid="priority-glyph"
        data-priority="none"
        className={cn("flex size-3.5 shrink-0 flex-col items-center justify-center gap-[2px]", className)}
      >
        {[0, 1, 2].map((index) => (
          <span key={index} className="h-[1.5px] w-2.5 rounded-full bg-muted-foreground" />
        ))}
      </span>
    );
  }
  const level = priority === "High" ? 3 : priority === "Normal" ? 2 : priority === "Low" ? 1 : 0;
  return (
    <span
      aria-hidden
      data-testid="priority-glyph"
      data-priority={priority}
      className={cn("flex size-3.5 shrink-0 items-end justify-center gap-[2px] text-foreground", className)}
    >
      {[4, 7, 10].map((height, index) => (
        <span
          key={height}
          className={cn("w-[3px] rounded-[1px] bg-current", index >= level && "opacity-25")}
          style={{ height }}
        />
      ))}
    </span>
  );
}

/** A label's dot. The colour is the hub's, derived from the label's name. */
export function LabelDot({
  color,
  className,
}: {
  readonly color: string;
  readonly className?: string;
}): React.ReactElement {
  return (
    <span
      aria-hidden
      data-testid="label-dot"
      className={cn("size-2 shrink-0 rounded-full", className)}
      style={{ backgroundColor: color }}
    />
  );
}

/** The dotted person Linear puts on an unassigned issue and on "Assign". */
export function NoAssigneeGlyph({ className }: { readonly className?: string }): React.ReactElement {
  return <CircleDashedIcon aria-hidden className={cn("size-3.5 shrink-0 text-muted-foreground", className)} />;
}

/** Two letters from an email or a name, which is the whole avatar we have. */
export function initialsOf(value: string): string {
  const name = value.split("@")[0] ?? value;
  const parts = name.split(/[.\-_\s]+/).filter((part) => part.length > 0);
  if (parts.length === 0) return value.slice(0, 2).toUpperCase();
  if (parts.length === 1) return parts[0]!.slice(0, 2).toUpperCase();
  return (parts[0]![0]! + parts[1]![0]!).toUpperCase();
}

/**
 * An initials avatar. The hub serves no avatar image for a hosted member —
 * `listHostedMembers` answers an id, an email, a role and the grants — so
 * initials are the honest portrait rather than a gravatar request that would
 * leak an email address to a third party.
 */
export function AssigneeAvatar({
  name,
  className,
}: {
  readonly name: string;
  readonly className?: string;
}): React.ReactElement {
  return (
    <span
      aria-hidden
      data-testid="assignee-avatar"
      className={cn(
        "flex size-3.5 shrink-0 items-center justify-center rounded-full bg-primary/15 font-medium text-[7px] text-primary uppercase",
        className,
      )}
    >
      {initialsOf(name)}
    </span>
  );
}
