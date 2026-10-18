import React from "react";
import { CheckIcon, XIcon } from "lucide-react";

import { Button } from "../../components/ui/button.tsx";
import { Calendar } from "../../components/ui/calendar.tsx";
import {
  Combobox,
  ComboboxEmpty,
  ComboboxItem,
  ComboboxList,
  ComboboxPopup,
  ComboboxSearchInput,
  ComboboxTrigger,
} from "../../components/ui/combobox.tsx";
import { Popover, PopoverPopup, PopoverTrigger } from "../../components/ui/popover.tsx";
import { Toggle } from "../../components/ui/toggle.tsx";
import type { ReportsFacets, ReportsFilters } from "../../contracts/reports.ts";
import { formatDayShort } from "../usage/usageFormat.ts";
import { customRangeDays, MAX_CUSTOM_DAYS, PRIORITY_OPTIONS, type ReportFilterKey, stageLabel, TYPE_OPTIONS } from "./model.ts";

export interface FilterOption {
  readonly value: string;
  readonly label: string;
}

/** The choices a chip offers: the Hub's facets, with any selected value the facets no longer list. */
export function filterOptions(key: ReportFilterKey, facets: ReportsFacets | undefined, selected: readonly string[]): readonly FilterOption[] {
  const base: readonly FilterOption[] =
    key === "priorities"
      ? PRIORITY_OPTIONS
      : key === "types"
        ? TYPE_OPTIONS
        : key === "labels"
          ? (facets?.labels ?? []).map((value) => ({ value, label: value }))
          : key === "filedBy"
            ? (facets?.filed_by ?? []).map((facet) => ({ value: facet.id, label: facet.name || facet.id }))
            : key === "runners"
              ? (facets?.runners ?? []).map((facet) => ({ value: facet.id, label: facet.name || facet.id }))
              : key === "models"
                ? (facets?.models ?? []).map((value) => ({ value, label: value }))
                : (facets?.stages ?? []).map((value) => ({ value, label: stageLabel(value) }));
  const missing = selected.filter((value) => !base.some((option) => option.value === value)).map((value) => ({ value, label: value }));
  return [...base, ...missing];
}

/** A chip that reads its set values: "Label: cloud" or "Label: 3 selected". */
export function chipSummary(label: string, options: readonly FilterOption[], selected: readonly string[]): string {
  if (selected.length === 0) return label;
  if (selected.length > 1) return `${label}: ${selected.length} selected`;
  return `${label}: ${options.find((option) => option.value === selected[0])?.label ?? selected[0]}`;
}

export const anyFilterSet = (filters: ReportsFilters) => Object.values(filters).some((values) => values.length > 0);

/** One filter chip: a multi-select Combobox, and a × that clears it once set. */
export function ReportFilterChip({
  label,
  options,
  selected,
  onChange,
  triggerLabel,
}: {
  readonly triggerLabel?: string;
  readonly label: string;
  readonly options: readonly FilterOption[];
  readonly selected: readonly string[];
  readonly onChange: (values: readonly string[]) => void;
}): React.ReactElement {
  const anchor = React.useRef<HTMLSpanElement>(null);
  const value = options.filter((option) => selected.includes(option.value));
  const summary = chipSummary(label, options, selected);
  return (
    <span ref={anchor} className="inline-flex min-w-0 items-center gap-0.5 pointer-coarse:gap-4">
      <Combobox
        multiple
        items={options}
        value={value}
        onValueChange={(next) => onChange(next.map((option) => option.value))}
        itemToStringLabel={(option) => option.label}
        isItemEqualToValue={(a, b) => a.value === b.value}
      >
        <ComboboxTrigger
          render={triggerLabel ? <Button size="compact" variant="ghost-muted" aria-label={triggerLabel} /> : <Toggle size="compact" variant="pill-outline" pressed={selected.length > 0} className="max-w-56" aria-label={`${summary}, filter`} />}
        >
          <span className="truncate">{triggerLabel ?? summary}</span>
        </ComboboxTrigger>
        <ComboboxPopup anchor={anchor} align="start" className="w-64">
          <ComboboxSearchInput aria-label={`Search ${label.toLowerCase()} values`} placeholder={`Search ${label.toLowerCase()}…`} />
          <ComboboxEmpty>Nothing to filter by.</ComboboxEmpty>
          <ComboboxList>
            {(option: FilterOption) => (
              <ComboboxItem key={option.value} value={option}>
                {selected.includes(option.value) ? <CheckIcon aria-hidden /> : <span aria-hidden className="size-4 shrink-0" />}
                <span className="min-w-0 flex-1 truncate">{option.label}</span>
              </ComboboxItem>
            )}
          </ComboboxList>
        </ComboboxPopup>
      </Combobox>
      {selected.length > 0 ? (
        <Button size="icon-xs" variant="ghost-muted" aria-label={`Clear ${label} filter`} onClick={() => onChange([])}>
          <XIcon aria-hidden />
        </Button>
      ) : null}
    </span>
  );
}

const pad = (value: number) => String(value).padStart(2, "0");
/** The calendar day a Date picked in the Calendar stands for, as a UTC YYYY-MM-DD day. */
export const calendarDay = (date: Date) => `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
const calendarDate = (day: string) => new Date(Number(day.slice(0, 4)), Number(day.slice(5, 7)) - 1, Number(day.slice(8, 10)));
const utcToday = () => calendarDate(new Date().toISOString().slice(0, 10));

export const customRangeLabel = (from: string, to: string) => (from === to ? formatDayShort(from) : `${formatDayShort(from)} – ${formatDayShort(to)}`);

/** A from/to date picker for a custom range of up to 90 UTC days, applied together. */
export function CustomRangePicker({
  from,
  to,
  onApply,
  trigger,
}: {
  readonly from: string | null;
  readonly to: string | null;
  readonly onApply: (from: string, to: string) => void;
  readonly trigger: React.ReactElement<Record<string, unknown>>;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const [range, setRange] = React.useState<{ from: Date | undefined; to?: Date | undefined } | undefined>(undefined);
  const today = utcToday();
  const start = range?.from ? calendarDay(range.from) : null;
  const end = range?.to ? calendarDay(range.to) : start;
  const days = start !== null && end !== null ? customRangeDays(start, end) : null;
  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        if (next) setRange(from !== null && to !== null ? { from: calendarDate(from), to: calendarDate(to) } : undefined);
        setOpen(next);
      }}
    >
      <PopoverTrigger render={trigger} />
      <PopoverPopup align="end" aria-label="Custom range">
        <div className="flex flex-col gap-3">
          <Calendar mode="range" selected={range} onSelect={setRange} disabled={{ after: today }} defaultMonth={range?.from ?? today} />
          <p className="text-muted-foreground text-xs" role="status">
            {start === null
              ? `Choose the first and last day, up to ${MAX_CUSTOM_DAYS} days, in UTC.`
              : days === null
                ? `A custom range spans at most ${MAX_CUSTOM_DAYS} days.`
                : `${customRangeLabel(start, end!)} · ${days} ${days === 1 ? "day" : "days"}, UTC`}
          </p>
          <div className="flex justify-end gap-2">
            <Button size="compact" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              size="compact"
              disabled={days === null}
              onClick={() => {
                if (start === null || end === null || days === null) return;
                onApply(start, end);
                setOpen(false);
              }}
            >
              Apply
            </Button>
          </div>
        </div>
      </PopoverPopup>
    </Popover>
  );
}
