import React from "react";

import { cn } from "../../lib/utils.ts";
import { type SlashCommand } from "../adapters/slashCommands.ts";

export interface SlashCommandMenuProps {
  readonly open: boolean;
  readonly commands: readonly SlashCommand[];
  /** The row Enter would run. */
  readonly highlighted: number;
  /** The id the editor's `aria-activedescendant` and `aria-controls` use. */
  readonly domId: string;
  readonly onHighlight: (index: number) => void;
  readonly onRun: (command: SlashCommand) => void;
}

export function slashOptionId(domId: string, name: string): string {
  return `${domId}-slash-${name}`;
}

export function SlashCommandMenu(props: SlashCommandMenuProps): React.ReactElement | null {
  const highlighted = React.useRef<HTMLDivElement>(null);
  React.useEffect(() => {
    highlighted.current?.scrollIntoView?.({ block: "nearest" });
  }, [props.highlighted, props.open]);
  if (!props.open) return null;
  return (
    <div
      data-testid="slash-menu"
      // Aligned with the prompt below it: the editor's body is `px-3 sm:px-4`,
      // and this pairs `px-1 sm:px-2` with the rows' own `px-2` to land the
      // command names on the same left edge as the text the reader is typing.
      className="flex w-full min-w-0 flex-col px-1 pt-2 sm:px-2"
    >
      <div
        role="listbox"
        aria-label="Slash commands"
        id={`${props.domId}-slash-list`}
        className="flex max-h-64 min-w-0 flex-col overflow-y-auto"
      >
        {props.commands.map((command, index) => {
          const active = index === props.highlighted;
          return (
            <div
              key={command.name}
              ref={active ? highlighted : undefined}
              role="option"
              id={slashOptionId(props.domId, command.name)}
              aria-selected={active}
              data-testid={`slash-command-${command.name}`}
              data-highlighted={active ? "true" : undefined}
              className={cn(
                "flex min-w-0 cursor-pointer select-none items-baseline gap-2 rounded-sm px-2 py-1.5 text-sm outline-none",
                active ? "bg-foreground/[0.09] text-foreground" : "text-foreground",
              )}
              onPointerDown={(event) => event.preventDefault()}
              onPointerMove={() => props.onHighlight(index)}
              onClick={() => props.onRun(command)}
            >
              <span className="shrink-0 font-medium">/{command.name}</span>
              <span className="min-w-0 flex-1 truncate text-muted-foreground text-xs">
                {command.description}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
