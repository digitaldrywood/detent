// The board toolbar (artifact screen 1's `.toolbar`).
//
// Left to right, exactly the artifact's row: a search field with a `/` hint,
// Filters, Sort showing its current value, the freshness chip, then the
// Board/List segmented control and the Lanes menu on the right.
//
// Two honesty notes the artifact does not have to make and this does:
//
//  - search is over the loaded board. The hub has no text search on work
//    items, so the field filters what came back and says so in its title
//    rather than pretending to have asked the server.
//  - a filter with more than one value is applied here, not there. The hub
//    rejects a repeated query parameter, so one value is pushed to the server
//    and the rest are applied to the result; the menu says which.
import { FilterIcon, KanbanIcon, LayoutListIcon, SearchIcon, XIcon } from "lucide-react";
import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import { Kbd } from "../../../components/ui/kbd.tsx";
import {
  Menu,
  MenuCheckboxItem,
  MenuGroup,
  MenuGroupLabel,
  MenuPopup,
  MenuRadioGroup,
  MenuRadioItem,
  MenuSeparator,
  MenuTrigger,
} from "../../../components/ui/menu.tsx";
import { cn } from "../../../lib/utils.ts";
import type { Lane } from "../lib/model.ts";
import {
  activeFilterCount,
  DEFAULT_VIEW_STATE,
  FILTER_KEYS,
  FILTER_LABELS,
  isDefaultViewState,
  laneVisible,
  SORT_LABELS,
  toggleFilter,
  toggleLane,
  WORK_SORTS,
  type FilterKey,
  type WorkSort,
  type WorkViewMode,
  type WorkViewState,
} from "../lib/viewState.ts";

const TRIGGER =
  "inline-flex h-7 shrink-0 cursor-pointer items-center gap-1.5 whitespace-nowrap rounded-[var(--control-radius)] border border-input bg-popover px-2 font-medium text-xs text-foreground shadow-xs/5 outline-none ring-ring hover:bg-accent/50 focus-visible:ring-2 sm:h-6 dark:bg-input/32 [&_svg]:size-3.5 [&_svg]:shrink-0 [&_svg]:text-muted-foreground";

export interface ToolbarFacets {
  readonly state: readonly string[];
  readonly label: readonly string[];
  readonly assignee: readonly string[];
  readonly priority: readonly string[];
}

export interface WorkToolbarProps {
  readonly view: WorkViewState;
  readonly onChange: (next: WorkViewState) => void;
  readonly lanes: readonly Lane[];
  readonly facets: ToolbarFacets;
  readonly searchRef?: React.Ref<HTMLInputElement>;
  /** The freshness chip, rendered as given so the shell owns its vocabulary. */
  readonly freshness: React.ReactNode;
  /** How many issues the facet lists were built from. */
  readonly loadedCount: number;
}

export function WorkToolbar({
  view,
  onChange,
  lanes,
  facets,
  searchRef,
  freshness,
  loadedCount,
}: WorkToolbarProps): React.ReactElement {
  const filters = activeFilterCount(view);
  const laneNames = React.useMemo(() => lanes.map((lane) => lane.name), [lanes]);
  const visibleLanes = laneNames.filter((lane) => laneVisible(view, lane)).length;

  const values = (key: FilterKey): readonly string[] => facets[key];

  return (
    <div data-testid="work-toolbar" className="flex flex-wrap items-center gap-2 px-5 pt-1.5 pb-3">
      <div className="flex h-7 w-full min-w-0 items-center gap-2 rounded-[var(--control-radius)] border border-input bg-popover px-2 text-muted-foreground text-xs shadow-xs/5 sm:h-6 sm:w-80 dark:bg-input/32">
        <SearchIcon className="size-3.5 shrink-0" />
        <input
          ref={searchRef}
          type="search"
          value={view.q}
          placeholder="Search issues…"
          aria-label="Search the loaded issues"
          title="Searches the issues already loaded; the hub has no work-item text search."
          data-testid="work-search"
          onChange={(event) => onChange({ ...view, q: event.target.value })}
          className="min-w-0 flex-1 border-0 bg-transparent p-0 text-foreground text-xs outline-none placeholder:text-muted-foreground [&::-webkit-search-cancel-button]:appearance-none"
        />
        {view.q.length > 0 ? (
          <Button
            size="icon-micro"
            variant="ghost"
            aria-label="Clear the search"
            onClick={() => onChange({ ...view, q: "" })}
          >
            <XIcon className="size-3" />
          </Button>
        ) : (
          <Kbd aria-hidden className="shrink-0 bg-transparent">
            /
          </Kbd>
        )}
      </div>

      <Menu>
        <MenuTrigger className={TRIGGER} data-testid="filters-trigger">
          <FilterIcon />
          Filters
          {filters > 0 ? (
            <span className="inline-grid h-4 min-w-4 place-items-center rounded bg-primary px-1 text-[10px] text-primary-foreground tabular-nums">
              {filters}
            </span>
          ) : null}
        </MenuTrigger>
        <MenuPopup align="start" className="w-64">
          {/* Every label is inside a `MenuGroup`: Base UI's `GroupLabel`
              reads its group's context and throws without one. */}
          {FILTER_KEYS.map((key) => (
            <React.Fragment key={key}>
              <MenuGroup>
                <MenuGroupLabel>{FILTER_LABELS[key]}</MenuGroupLabel>
                {values(key).length === 0 ? (
                  <p className="px-2 py-1 text-muted-foreground text-xs">
                    Nothing to filter by in the loaded issues.
                  </p>
                ) : (
                  values(key).map((value) => (
                    <MenuCheckboxItem
                      key={value}
                      checked={view[key].includes(value)}
                      data-testid={`filter-${key}-${value}`}
                      onCheckedChange={() => onChange(toggleFilter(view, key, value))}
                    >
                      {value}
                    </MenuCheckboxItem>
                  ))
                )}
              </MenuGroup>
              <MenuSeparator />
            </React.Fragment>
          ))}
          <p className="px-2 py-1 text-muted-foreground text-xs">
            Built from the {loadedCount} issues on this board. One value per field is filtered by
            the hub; any others are applied here.
          </p>
          {isDefaultViewState(view) ? null : (
            <MenuCheckboxItem
              checked={false}
              data-testid="filters-reset"
              onCheckedChange={() => onChange({ ...DEFAULT_VIEW_STATE, view: view.view })}
            >
              Reset everything
            </MenuCheckboxItem>
          )}
        </MenuPopup>
      </Menu>

      <Menu>
        <MenuTrigger className={TRIGGER} data-testid="sort-trigger">
          Sort
          <span className="text-muted-foreground">{SORT_LABELS[view.sort]}</span>
        </MenuTrigger>
        <MenuPopup align="start" className="w-52">
          <MenuRadioGroup
            value={view.sort}
            onValueChange={(value) => onChange({ ...view, sort: value as WorkSort })}
          >
            {WORK_SORTS.map((sort) => (
              <MenuRadioItem key={sort} value={sort} closeOnClick data-testid={`sort-${sort}`}>
                {SORT_LABELS[sort]}
              </MenuRadioItem>
            ))}
          </MenuRadioGroup>
        </MenuPopup>
      </Menu>

      {freshness}

      <span className="flex-1" />

      <div
        role="radiogroup"
        aria-label="Board or list"
        data-testid="view-segmented"
        className="inline-flex h-7 shrink-0 items-center gap-0.5 rounded-[var(--control-radius)] bg-secondary p-0.5 sm:h-6"
      >
        {(
          [
            { value: "board", label: "Board", icon: <KanbanIcon /> },
            { value: "list", label: "List", icon: <LayoutListIcon /> },
          ] as const
        ).map((option) => {
          const selected = view.view === option.value;
          return (
            <button
              key={option.value}
              type="button"
              role="radio"
              aria-checked={selected}
              tabIndex={selected ? 0 : -1}
              data-testid={`view-${option.value}`}
              onClick={() => onChange({ ...view, view: option.value as WorkViewMode })}
              onKeyDown={(event) => {
                if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
                event.preventDefault();
                onChange({
                  ...view,
                  view: view.view === "board" ? "list" : "board",
                });
              }}
              className={cn(
                "inline-flex h-full cursor-pointer items-center gap-1.5 rounded-[calc(var(--control-radius)-2px)] px-2 font-medium text-xs outline-none ring-ring transition-colors focus-visible:ring-2 [&_svg]:size-3.5",
                selected
                  ? "bg-background text-foreground shadow-xs"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {option.icon}
              {option.label}
            </button>
          );
        })}
      </div>

      <Menu>
        <MenuTrigger className={TRIGGER} data-testid="lanes-trigger">
          Lanes
          <span className="text-muted-foreground tabular-nums">
            {visibleLanes}/{laneNames.length}
          </span>
        </MenuTrigger>
        <MenuPopup align="end" className="w-52">
          <MenuGroup>
            <MenuGroupLabel>Show lanes</MenuGroupLabel>
            {laneNames.map((lane) => (
              <MenuCheckboxItem
                key={lane}
                checked={laneVisible(view, lane)}
                data-testid={`lane-toggle-${lane}`}
                onCheckedChange={() => onChange(toggleLane(view, lane, laneNames))}
              >
                {lane}
              </MenuCheckboxItem>
            ))}
          </MenuGroup>
        </MenuPopup>
      </Menu>
    </div>
  );
}
