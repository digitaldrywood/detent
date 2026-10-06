// Forms: input, textarea, label, checkbox, switch, select, combobox, autocomplete.
import { Autocomplete as AutocompletePrimitive } from "@base-ui/react/autocomplete";
import { GitBranchIcon, SearchIcon } from "lucide-react";
import React from "react";

import {
  Autocomplete,
  AutocompleteEmpty,
  AutocompleteInput,
  AutocompleteItem,
  AutocompleteList,
  AutocompleteListHeading,
  AutocompleteListVirtualized,
  AutocompletePopup,
} from "~/components/ui/autocomplete";
import { Checkbox } from "~/components/ui/checkbox";
import {
  Combobox,
  ComboboxChip,
  ComboboxChips,
  ComboboxChipsInput,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
  ComboboxPopup,
  ComboboxValue,
} from "~/components/ui/combobox";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import {
  Select,
  SelectGroup,
  SelectGroupLabel,
  SelectItem,
  SelectPopup,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import { Switch } from "~/components/ui/switch";
import { Textarea } from "~/components/ui/textarea";

import { Cell, LONG_LABEL, Row, type GalleryDoc } from "../specimen";

const INPUT_SIZES = ["compact", "sm", "default", "lg"] as const;
const TEXTAREA_SIZES = ["sm", "default", "lg"] as const;

const LANES = ["Backlog", "Todo", "In progress", "In review", "Done"] as const;
const PROJECTS = ["detent", "detent-cloud", "docs", "runner", "website"] as const;

/** Enough branches that only a window of them is ever in the DOM. */
export const BRANCHES: readonly string[] = Array.from(
  { length: 1_000 },
  (_, index) => `feat/issue-${String(3_000 + index)}-${["lock-renewal", "usage-chart", "gallery", "runner-probe"][index % 4]}`,
);

const ROW_HEIGHT = 32;
const WINDOW_HEIGHT = 224;
const OVERSCAN = 4;

/**
 * A minimal external virtualizer for the `*ListVirtualized` parts: fixed row
 * height, only the visible window (plus overscan) mounted. The list part owns
 * no scroll container, so this element is it. Rows carry their `index` so the
 * primitive's keyboard highlight addresses rows that are not mounted.
 */
export function WindowedRows<Item>({
  items,
  renderRow,
  scrollRef,
}: {
  readonly items: readonly Item[];
  readonly renderRow: (item: Item, index: number) => React.ReactNode;
  readonly scrollRef?: React.RefObject<HTMLDivElement | null>;
}): React.ReactElement {
  const [scrollTop, setScrollTop] = React.useState(0);
  const total = items.length * ROW_HEIGHT;
  const first = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN);
  const last = Math.min(items.length, Math.ceil((scrollTop + WINDOW_HEIGHT) / ROW_HEIGHT) + OVERSCAN);
  return (
    <div
      ref={scrollRef}
      role="presentation"
      className="overflow-y-auto overscroll-contain px-1"
      style={{ height: Math.min(WINDOW_HEIGHT, total) }}
      onScroll={(event) => setScrollTop(event.currentTarget.scrollTop)}
    >
      <div role="presentation" className="relative" style={{ height: total }}>
        {items.slice(first, last).map((item, offset) => (
          <div
            key={first + offset}
            role="presentation"
            className="absolute inset-x-0"
            style={{ top: (first + offset) * ROW_HEIGHT, height: ROW_HEIGHT }}
          >
            {renderRow(item, first + offset)}
          </div>
        ))}
      </div>
    </div>
  );
}

/** Keeps a keyboard-highlighted row of a `WindowedRows` list in view. */
export function scrollRowIntoView(scroller: HTMLDivElement | null, index: number): void {
  if (scroller === null) return;
  const top = index * ROW_HEIGHT;
  if (top < scroller.scrollTop) scroller.scrollTop = top;
  else if (top + ROW_HEIGHT > scroller.scrollTop + scroller.clientHeight) {
    scroller.scrollTop = top + ROW_HEIGHT - scroller.clientHeight;
  }
}

function BranchRows({ scrollRef }: { readonly scrollRef: React.RefObject<HTMLDivElement | null> }) {
  const filtered = AutocompletePrimitive.useFilteredItems<string>();
  return (
    <WindowedRows
      items={filtered}
      scrollRef={scrollRef}
      renderRow={(branch, index) => (
        <AutocompleteItem value={branch} index={index} className="h-full">
          <GitBranchIcon />
          <span className="truncate">{branch}</span>
        </AutocompleteItem>
      )}
    />
  );
}

function VirtualizedAutocomplete() {
  const scrollRef = React.useRef<HTMLDivElement | null>(null);
  return (
    <div className="w-80 max-w-full">
      <Autocomplete
        items={BRANCHES}
        virtualized
        defaultOpen
        onItemHighlighted={(_item, { reason, index }) => {
          if (reason === "keyboard") scrollRowIntoView(scrollRef.current, index);
        }}
      >
        <AutocompleteInput placeholder="Search branches" aria-label="Search branches" startAddon={<GitBranchIcon />} />
        <AutocompletePopup>
          <AutocompleteEmpty>No branch matches.</AutocompleteEmpty>
          <AutocompleteListVirtualized>
            <AutocompleteListHeading>{BRANCHES.length.toLocaleString("en-US")} branches</AutocompleteListHeading>
            <BranchRows scrollRef={scrollRef} />
          </AutocompleteListVirtualized>
        </AutocompletePopup>
      </Autocomplete>
    </div>
  );
}

export const input: GalleryDoc = {
  meta: { name: "Input", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "sizes",
      title: "Sizes",
      render: () => (
        <Row>
          {INPUT_SIZES.map((size) => (
            <Cell key={size} label={size} className="w-48">
              <Input size={size} placeholder="Search issues" aria-label={`Input ${size}`} />
            </Cell>
          ))}
        </Row>
      ),
    },
    {
      id: "states",
      title: "States",
      note: "Invalid is `aria-invalid`; disabled dims the whole control. Focus the field for the ring.",
      render: () => (
        <Row>
          <Cell label="empty" className="w-56">
            <Input placeholder="Placeholder" aria-label="Empty" />
          </Cell>
          <Cell label="filled" className="w-56">
            <Input defaultValue="detent/design-system" aria-label="Filled" />
          </Cell>
          <Cell label="invalid" className="w-56">
            <Input defaultValue="not a branch name" aria-invalid aria-label="Invalid" />
          </Cell>
          <Cell label="disabled" className="w-56">
            <Input defaultValue="Read only for viewers" disabled aria-label="Disabled" />
          </Cell>
          <Cell label="long value" className="w-56">
            <Input defaultValue={LONG_LABEL} aria-label="Long value" />
          </Cell>
          <Cell label="type=search" className="w-56">
            <Input type="search" placeholder="Search" aria-label="Search" />
          </Cell>
          <Cell label='font="mono"' className="w-56">
            <Input font="mono" defaultValue="sha256:8f2c41a0b7d8" aria-label="Digest" />
          </Cell>
        </Row>
      ),
    },
    {
      id: "with-label",
      title: "With label",
      render: () => (
        <div className="flex w-72 flex-col gap-2">
          <Label htmlFor="ds-input-branch">Branch name</Label>
          <Input id="ds-input-branch" placeholder="feat/design-system" />
        </div>
      ),
    },
  ],
};

export const textarea: GalleryDoc = {
  meta: { name: "Textarea", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "sizes",
      title: "Sizes",
      note: "The field grows with its content (`field-sizing-content`).",
      render: () => (
        <Row>
          {TEXTAREA_SIZES.map((size) => (
            <Cell key={size} label={size} className="w-60">
              <Textarea size={size} placeholder="Describe the change" aria-label={`Textarea ${size}`} />
            </Cell>
          ))}
        </Row>
      ),
    },
    {
      id: "states",
      title: "States",
      render: () => (
        <Row>
          <Cell label="filled" className="w-60">
            <Textarea defaultValue={`${LONG_LABEL}.\n\nA second paragraph.`} aria-label="Filled" />
          </Cell>
          <Cell label="invalid" className="w-60">
            <Textarea defaultValue="Too short" aria-invalid aria-label="Invalid" />
          </Cell>
          <Cell label="disabled" className="w-60">
            <Textarea defaultValue="Locked while the run is active" disabled aria-label="Disabled" />
          </Cell>
        </Row>
      ),
    },
  ],
};

export const label: GalleryDoc = {
  meta: { name: "Label", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "pairings",
      title: "Paired with controls",
      render: () => (
        <div className="flex flex-col gap-3">
          <Label>
            <Checkbox defaultChecked />
            Auto-merge when checks pass
          </Label>
          <Label>
            <Switch defaultChecked />
            Notify me on review
          </Label>
          <div className="flex w-64 flex-col gap-2">
            <Label htmlFor="ds-label-title">Title</Label>
            <Input id="ds-label-title" defaultValue="Port the gallery" />
          </div>
        </div>
      ),
    },
  ],
};

export const checkbox: GalleryDoc = {
  meta: { name: "Checkbox", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "states",
      title: "States",
      render: () => (
        <Row>
          <Cell label="unchecked">
            <Checkbox aria-label="Unchecked" />
          </Cell>
          <Cell label="checked">
            <Checkbox defaultChecked aria-label="Checked" />
          </Cell>
          <Cell label="indeterminate">
            <Checkbox indeterminate aria-label="Indeterminate" />
          </Cell>
          <Cell label="invalid">
            <Checkbox aria-invalid aria-label="Invalid" />
          </Cell>
          <Cell label="disabled">
            <Checkbox disabled aria-label="Disabled" />
          </Cell>
          <Cell label="checked + disabled">
            <Checkbox defaultChecked disabled aria-label="Checked disabled" />
          </Cell>
        </Row>
      ),
    },
    {
      id: "long",
      title: "Long label",
      render: () => (
        <div className="max-w-80">
          <Label className="items-start">
            <Checkbox defaultChecked className="mt-0.5" />
            {LONG_LABEL}
          </Label>
        </div>
      ),
    },
  ],
};

export const switchDoc: GalleryDoc = {
  meta: { name: "Switch", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "states",
      title: "Sizes × states",
      render: () => (
        <div className="flex flex-col gap-4">
          {(["default", "sm"] as const).map((size) => (
            <Row key={size}>
              <Cell label={`${size} off`}>
                <Switch size={size} aria-label="Off" />
              </Cell>
              <Cell label={`${size} on`}>
                <Switch size={size} defaultChecked aria-label="On" />
              </Cell>
              <Cell label={`${size} disabled`}>
                <Switch size={size} disabled aria-label="Disabled" />
              </Cell>
              <Cell label={`${size} on + disabled`}>
                <Switch size={size} defaultChecked disabled aria-label="On disabled" />
              </Cell>
              <Cell label={`${size} mixed`}>
                <Switch size={size} mixed aria-label="Mixed" />
              </Cell>
            </Row>
          ))}
        </div>
      ),
    },
    {
      id: "mixed",
      title: "Mixed: the selection disagrees",
      note: "`mixed` centres the thumb on a muted track and reports `aria-checked=\"mixed\"` when the targets of a bulk toggle disagree. It is presentational: the owner decides what a click does.",
      render: () => (
        <div className="flex flex-col gap-3">
          <Label>
            <Switch mixed />
            Auto-merge (2 of 5 projects)
          </Label>
          <Label>
            <Switch size="sm" mixed />
            Notify on review (some threads)
          </Label>
        </div>
      ),
    },
  ],
};

function LaneSelect({
  size,
  variant,
  disabled,
}: {
  readonly size?: "xs" | "sm" | "default" | "lg" | "compact";
  readonly variant?: "default" | "ghost";
  readonly disabled?: boolean;
}) {
  return (
    <Select<string> defaultValue="Todo" disabled={disabled ?? false}>
      <SelectTrigger size={size ?? "default"} variant={variant ?? "default"} className="w-40" aria-label="Lane">
        <SelectValue />
      </SelectTrigger>
      <SelectPopup>
        <SelectGroup>
          <SelectGroupLabel>Lane</SelectGroupLabel>
          {LANES.map((lane) => (
            <SelectItem key={lane} value={lane} disabled={lane === "Done"}>
              {lane}
            </SelectItem>
          ))}
        </SelectGroup>
        <SelectSeparator />
        <SelectItem value="Archived">Archived</SelectItem>
      </SelectPopup>
    </Select>
  );
}

export const select: GalleryDoc = {
  meta: { name: "Select", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "variants",
      title: "Variants × sizes",
      note: "Click to open; arrows move, Enter selects, Escape closes and returns focus. `Done` is a disabled item.",
      minHeight: 300,
      render: () => (
        <div className="flex flex-col gap-4">
          {(["default", "ghost"] as const).map((variant) => (
            <Row key={variant}>
              {(["xs", "compact", "sm", "default", "lg"] as const).map((size) => (
                <Cell key={size} label={`${variant} / ${size}`}>
                  <LaneSelect variant={variant} size={size} />
                </Cell>
              ))}
            </Row>
          ))}
          <Row>
            <Cell label="disabled">
              <LaneSelect disabled />
            </Cell>
            <Cell label="placeholder">
              <Select<string>>
                <SelectTrigger className="w-40" aria-label="Project">
                  <SelectValue placeholder="Choose a project" />
                </SelectTrigger>
                <SelectPopup>
                  {PROJECTS.map((project) => (
                    <SelectItem key={project} value={project}>
                      {project}
                    </SelectItem>
                  ))}
                </SelectPopup>
              </Select>
            </Cell>
          </Row>
        </div>
      ),
    },
  ],
};

export const combobox: GalleryDoc = {
  meta: { name: "Combobox", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "open",
      title: "Open: list with the selected value",
      note: "Rendered open (`defaultOpen`). The selected item carries the check; arrows move the highlight.",
      minHeight: 280,
      render: () => (
        <div className="w-64">
          <Combobox items={[...PROJECTS]} defaultValue="detent" defaultOpen>
            <ComboboxInput placeholder="Project" aria-label="Project (open)" />
            <ComboboxPopup>
              <ComboboxEmpty>No project matches.</ComboboxEmpty>
              <ComboboxList>
                {(item: string) => (
                  <ComboboxItem key={item} value={item}>
                    {item}
                  </ComboboxItem>
                )}
              </ComboboxList>
            </ComboboxPopup>
          </Combobox>
        </div>
      ),
    },
    {
      id: "single",
      title: "Single value",
      note: "Type to filter; the empty state shows when nothing matches.",
      minHeight: 120,
      render: () => (
        <div className="w-64">
          <Combobox items={[...PROJECTS]} defaultValue="detent">
            <ComboboxInput placeholder="Project" aria-label="Project" showClear />
            <ComboboxPopup>
              <ComboboxEmpty>No project matches.</ComboboxEmpty>
              <ComboboxList>
                {(item: string) => (
                  <ComboboxItem key={item} value={item}>
                    {item}
                  </ComboboxItem>
                )}
              </ComboboxList>
            </ComboboxPopup>
          </Combobox>
        </div>
      ),
    },
    {
      id: "multiple",
      title: "Multiple values as chips",
      minHeight: 120,
      render: () => (
        <div className="w-80">
          <Combobox items={[...LANES]} multiple defaultValue={["Todo", "In review"]}>
            <ComboboxChips>
              <ComboboxValue>
                {(values: string[]) => (
                  <>
                    {values.map((value) => (
                      <ComboboxChip key={value}>{value}</ComboboxChip>
                    ))}
                    <ComboboxChipsInput placeholder={values.length > 0 ? "" : "Lanes"} aria-label="Lanes" />
                  </>
                )}
              </ComboboxValue>
            </ComboboxChips>
            <ComboboxPopup>
              <ComboboxEmpty>No lane matches.</ComboboxEmpty>
              <ComboboxList>
                {(item: string) => (
                  <ComboboxItem key={item} value={item}>
                    {item}
                  </ComboboxItem>
                )}
              </ComboboxList>
            </ComboboxPopup>
          </Combobox>
        </div>
      ),
    },
    {
      id: "disabled",
      title: "Disabled",
      render: () => (
        <div className="w-64">
          <Combobox items={[...PROJECTS]} defaultValue="detent" disabled>
            <ComboboxInput aria-label="Project (disabled)" />
          </Combobox>
        </div>
      ),
    },
  ],
};

export const autocomplete: GalleryDoc = {
  meta: { name: "Autocomplete", kind: "primitive", group: "forms" },
  specimens: [
    {
      id: "open",
      title: "Open: suggestions under the field",
      note: "Rendered open (`defaultOpen`), the way it reads while the reader types.",
      minHeight: 280,
      render: () => (
        <div className="w-72">
          <Autocomplete items={[...PROJECTS]} defaultOpen>
            <AutocompleteInput
              placeholder="Search projects"
              aria-label="Search projects (open)"
              startAddon={<SearchIcon />}
            />
            <AutocompletePopup>
              <AutocompleteEmpty>No suggestions.</AutocompleteEmpty>
              <AutocompleteList>
                {(item: string) => (
                  <AutocompleteItem key={item} value={item}>
                    {item}
                  </AutocompleteItem>
                )}
              </AutocompleteList>
            </AutocompletePopup>
          </Autocomplete>
        </div>
      ),
    },
    {
      id: "virtualized",
      title: "Virtualized list with a heading",
      note: "`AutocompleteListVirtualized` leaves scrolling to an external virtualizer (here a fixed-row window over 1,000 branches; the app pairs it with LegendList). `AutocompleteListHeading` labels the list where rows cannot sit in a group.",
      minHeight: 340,
      render: () => <VirtualizedAutocomplete />,
    },
    {
      id: "basic",
      title: "Free text with suggestions",
      note: "Unlike Combobox the value is the typed text; suggestions only complete it.",
      minHeight: 120,
      render: () => (
        <div className="w-72">
          <Autocomplete items={[...PROJECTS]}>
            <AutocompleteInput
              placeholder="Search projects"
              aria-label="Search projects"
              startAddon={<SearchIcon />}
            />
            <AutocompletePopup>
              <AutocompleteEmpty>No suggestions.</AutocompleteEmpty>
              <AutocompleteList>
                {(item: string) => (
                  <AutocompleteItem key={item} value={item}>
                    {item}
                  </AutocompleteItem>
                )}
              </AutocompleteList>
            </AutocompletePopup>
          </Autocomplete>
        </div>
      ),
    },
    {
      id: "sizes",
      title: "Sizes",
      render: () => (
        <Row>
          {(["sm", "default", "lg"] as const).map((size) => (
            <Cell key={size} label={size} className="w-56">
              <Autocomplete items={[...PROJECTS]}>
                <AutocompleteInput size={size} placeholder="Search" aria-label={`Search ${size}`} showTrigger />
              </Autocomplete>
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};
