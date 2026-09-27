import { EllipsisIcon } from "lucide-react";
import React from "react";

import { cn } from "../../../lib/utils.ts";
import type { WorkItemView } from "../lib/model.ts";

export function LaneMenu({
  item,
  lanes,
  onMove,
  className,
}: {
  item: WorkItemView;
  lanes: readonly string[];
  onMove: (item: WorkItemView, toState: string) => void;
  className?: string;
}): React.ReactElement | null {
  const [open, setOpen] = React.useState(false);
  const trigger = React.useRef<HTMLButtonElement>(null);
  const first = React.useRef<HTMLButtonElement>(null);
  const container = React.useRef<HTMLDivElement>(null);

  React.useEffect(() => {
    if (open) first.current?.focus();
  }, [open]);

  React.useEffect(() => {
    if (!open) return;
    const onPointerDown = (event: MouseEvent) => {
      if (container.current?.contains(event.target as Node) === true) return;
      setOpen(false);
    };
    globalThis.document?.addEventListener("mousedown", onPointerDown);
    return () => globalThis.document?.removeEventListener("mousedown", onPointerDown);
  }, [open]);

  if (lanes.length === 0) return null;

  const close = (focusTrigger: boolean) => {
    setOpen(false);
    if (focusTrigger) trigger.current?.focus();
  };

  return (
    <div
      className="relative"
      ref={container}
      onKeyDown={(event) => {
        if (event.key !== "Escape" || !open) return;
        event.stopPropagation();
        close(true);
      }}
    >
      <button
        type="button"
        ref={trigger}
        aria-label={`Move ${item.title}`}
        aria-haspopup="menu"
        aria-expanded={open}
        data-testid="lane-menu-trigger"
        onClick={() => setOpen((current) => !current)}
        className={cn(
          "inline-flex size-5 shrink-0 cursor-pointer items-center justify-center rounded-sm text-muted-foreground outline-none ring-ring hover:bg-accent hover:text-foreground focus-visible:ring-2",
          className,
        )}
      >
        <EllipsisIcon className="size-3.5" />
      </button>
      {open ? (
        <div
          role="menu"
          aria-label="Move to"
          className="dropdown-glass absolute end-0 top-full z-50 mt-1 w-44 rounded-lg border border-border p-1 shadow-lg"
        >
          <p className="px-2 py-1 text-muted-foreground text-xs">Move to</p>
          {lanes.map((lane, index) => (
            <button
              key={lane}
              type="button"
              role="menuitem"
              ref={index === 0 ? first : undefined}
              data-testid={`lane-menu-item-${lane}`}
              onClick={() => {
                close(true);
                onMove(item, lane);
              }}
              className="flex h-8 w-full cursor-pointer items-center rounded-md px-2 text-left text-sm outline-none hover:bg-accent focus-visible:bg-accent"
            >
              {lane}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
