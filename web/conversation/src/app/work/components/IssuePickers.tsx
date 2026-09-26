import { CheckIcon } from "lucide-react";
import React from "react";

import { AutocompleteInput } from "../../../components/ui/autocomplete.tsx";
import { Checkbox } from "../../../components/ui/checkbox.tsx";
import {
  Command,
  CommandGroup,
  CommandGroupLabel,
  CommandItem,
  CommandList,
} from "../../../components/ui/command.tsx";
import { Kbd } from "../../../components/ui/kbd.tsx";
import { Popover, PopoverPopup, PopoverTrigger } from "../../../components/ui/popover.tsx";
import { cn } from "../../../lib/utils.ts";

/** One row of a picker. */
export interface PickerRow {
  readonly key: string;
  readonly label: string;
  /** The glyph in front: a state ring, a priority glyph, an avatar, a dot. */
  readonly glyph?: React.ReactNode;
  /** A second line under the label, for a related issue's state. */
  readonly detail?: string;
  /** Linear's digit hint. It is also a live shortcut while the picker is open. */
  readonly digit?: string;
  readonly selected?: boolean;

  readonly checkbox?: boolean;
  /**
   * A row that is listed but cannot be taken right now. It stays on the list
   * with its reason in `detail` rather than disappearing (decisions.md §16):
   * a workflow that does not allow a move is a fact about the workflow, and a
   * reader is owed it.
   */
  readonly disabled?: boolean;
  readonly onSelect: () => void;
  readonly testId?: string;
}

export interface PickerGroup {
  /** "" renders the rows with no heading, which is Linear's first group. */
  readonly label: string;
  readonly rows: readonly PickerRow[];
}

export interface PropertyPickerProps {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  /** The properties row the picker hangs off. */
  readonly trigger: React.ReactNode;
  readonly triggerClassName?: string;
  readonly triggerTestId?: string;
  readonly triggerDisabled?: boolean;
  /** The sentence in the header: "Change status…". */
  readonly placeholder: string;

  readonly hint: string;
  readonly query: string;
  readonly onQuery: (query: string) => void;
  readonly groups: readonly PickerGroup[];
  /** Shown where a query matches nothing and there is nothing to create. */
  readonly empty?: React.ReactNode;
  readonly label: string;
  readonly testId: string;
  readonly width?: string;
}

/**
 * A row's digit, if it has one. Collected across every group so a shortcut is
 * unambiguous however the rows are split up.
 */
function digitRows(groups: readonly PickerGroup[]): Map<string, PickerRow> {
  const rows = new Map<string, PickerRow>();
  for (const group of groups) {
    for (const row of group.rows) {
      if (row.digit === undefined || row.disabled === true) continue;
      if (!rows.has(row.digit)) rows.set(row.digit, row);
    }
  }
  return rows;
}

export function PropertyPicker(props: PropertyPickerProps): React.ReactElement {
  const digits = React.useMemo(() => digitRows(props.groups), [props.groups]);
  const empty = props.groups.every((group) => group.rows.length === 0);

  return (
    <Popover open={props.open} onOpenChange={props.onOpenChange}>
      <PopoverTrigger
        className={props.triggerClassName}
        data-testid={props.triggerTestId}
        disabled={props.triggerDisabled}
      >
        {props.trigger}
      </PopoverTrigger>
      <PopoverPopup
        side="bottom"
        align="start"
        sideOffset={6}
        aria-label={props.label}
        data-testid={props.testId}
        className={cn("max-w-[calc(100vw-2rem)]", props.width ?? "w-[264px]")}
        viewportClassName="p-0"
      >
        <Command
          mode="none"
          value={props.query}
          onValueChange={(value) => props.onQuery(typeof value === "string" ? value : "")}
        >
          {/* Linear's header: the sentence, and the letter that opened it. */}
          <div className="flex items-center gap-2 border-border/60 border-b px-2.5 py-1.5">
            <AutocompleteInput
              autoFocus
              placeholder={props.placeholder}
              aria-label={props.placeholder}
              size="sm"
              className="border-transparent! bg-transparent! shadow-none before:hidden has-focus-visible:ring-0 placeholder:text-muted-foreground *:data-[slot=autocomplete-input]:h-7 *:data-[slot=autocomplete-input]:px-0 *:data-[slot=autocomplete-input]:text-[13px]"
              onKeyDown={(event) => {
                if (event.metaKey || event.ctrlKey || event.altKey) return;
                const row = digits.get(event.key);
                if (row === undefined) return;
                event.preventDefault();
                row.onSelect();
              }}
            />
            <Kbd className="shrink-0">{props.hint}</Kbd>
          </div>
          <CommandList className="max-h-72 not-empty:p-1">
            {empty
              ? null
              : props.groups.map((group) =>
                  group.rows.length === 0 ? null : (
                    <CommandGroup key={group.label === "" ? "__ungrouped" : group.label}>
                      {group.label === "" ? null : (
                        <CommandGroupLabel className="px-2 pt-1.5 pb-1 text-[11px] text-muted-foreground">
                          {group.label}
                        </CommandGroupLabel>
                      )}
                      {group.rows.map((row) => (
                        <PickerItem key={row.key} row={row} />
                      ))}
                    </CommandGroup>
                  ),
                )}
          </CommandList>
          {empty && props.empty !== undefined ? (
            <div className="px-3 py-3 text-[13px] text-muted-foreground">{props.empty}</div>
          ) : null}
        </Command>
      </PopoverPopup>
    </Popover>
  );
}

function PickerItem({ row }: { readonly row: PickerRow }): React.ReactElement {
  return (
    <CommandItem
      value={row.key}
      data-testid={row.testId}
      aria-selected={row.selected === true}
      aria-disabled={row.disabled === true ? "true" : undefined}
      className={cn(
        "cursor-pointer gap-2 rounded-sm px-2 py-1.5 text-[13px]",
        row.disabled === true && "cursor-default opacity-64",
      )}
      onMouseDown={(event) => event.preventDefault()}
      onClick={() => {
        if (row.disabled === true) return;
        row.onSelect();
      }}
    >
      {row.checkbox === true ? (
        <Checkbox
          checked={row.selected === true}
          // The row is the control: a checkbox with its own handler would fire
          // twice on a click and swallow the keyboard's Enter.
          tabIndex={-1}
          aria-hidden
          className="pointer-events-none size-3.5"
        />
      ) : null}
      {row.glyph}
      <span className="flex min-w-0 flex-1 flex-col">
        <span className="truncate">{row.label}</span>
        {row.detail === undefined || row.detail === "" ? null : (
          <span className="truncate text-[11px] text-muted-foreground">{row.detail}</span>
        )}
      </span>
      {row.checkbox !== true && row.selected === true ? (
        <CheckIcon data-testid="picker-check" className="size-3.5 shrink-0 text-muted-foreground" />
      ) : null}
      {row.digit === undefined ? null : (
        <span className="shrink-0 text-[11px] text-muted-foreground/70 tabular-nums">{row.digit}</span>
      )}
    </CommandItem>
  );
}

