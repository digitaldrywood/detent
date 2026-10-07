// Actions: button, toggle, toggle group, panel tab close button, refresh icon.
import type { VariantProps } from "class-variance-authority";
import {
  AlignCenterIcon,
  AlignLeftIcon,
  AlignRightIcon,
  BoldIcon,
  ChevronDownIcon,
  FileTextIcon,
  ItalicIcon,
  PlusIcon,
  TerminalIcon,
} from "lucide-react";
import React from "react";

import { Button, InlineButton, SplitButton, type buttonVariants } from "~/components/ui/button";
import { Menu, MenuItem, MenuPopup, MenuTrigger } from "~/components/ui/menu";
import { PanelTabCloseButton } from "~/components/ui/panel-tab-close-button";
import { RefreshIcon } from "~/components/ui/refresh-icon";
import { Spinner } from "~/components/ui/spinner";
import { Toggle, type toggleVariants } from "~/components/ui/toggle";
import { ToggleGroup, ToggleGroupItem, ToggleGroupSeparator } from "~/components/ui/toggle-group";

import { Cell, keysOf, LONG_LABEL, Matrix, Row, type GalleryDoc } from "../specimen";

type ButtonVariant = NonNullable<VariantProps<typeof buttonVariants>["variant"]>;
type ButtonSize = NonNullable<VariantProps<typeof buttonVariants>["size"]>;
type ToggleVariant = NonNullable<VariantProps<typeof toggleVariants>["variant"]>;
type ToggleSize = NonNullable<VariantProps<typeof toggleVariants>["size"]>;
type InlineTone = NonNullable<React.ComponentProps<typeof InlineButton>["tone"]>;

type RefreshSize = NonNullable<React.ComponentProps<typeof RefreshIcon>["size"]>;

const REFRESH_SIZES = keysOf<RefreshSize>({ xs: true, sm: true, md: true, lg: true });

const INLINE_TONES = keysOf<InlineTone>({ default: true, muted: true, destructive: true, picker: true });

const BUTTON_VARIANTS = keysOf<ButtonVariant>({
  default: true,
  secondary: true,
  outline: true,
  ghost: true,
  "ghost-muted": true,
  "ghost-destructive": true,
  glass: true,
  link: true,
  destructive: true,
  "destructive-outline": true,
  "warning-outline": true,
  overlay: true,
  "media-close": true,
  "media-navigation": true,
});

/** Variants that sit on media: the gallery gives them a backdrop to sit on. */
const MEDIA_VARIANTS: ReadonlySet<ButtonVariant> = new Set<ButtonVariant>([
  "overlay",
  "media-close",
  "media-navigation",
]);

const BUTTON_TEXT_SIZES = keysOf<Exclude<ButtonSize, `icon${string}`>>({
  micro: true,
  compact: true,
  xs: true,
  sm: true,
  "sm-multiline": true,
  default: true,
  lg: true,
  xl: true,
});

const BUTTON_ICON_SIZES = keysOf<Extract<ButtonSize, `icon${string}`>>({
  "icon-tiny": true,
  "icon-micro": true,
  "icon-xs": true,
  "icon-sm": true,
  icon: true,
  "icon-lg": true,
  "icon-xl": true,
});

const TOGGLE_VARIANTS = keysOf<ToggleVariant>({
  default: true,
  outline: true,
  ghost: true,
  segmented: true,
  pill: true,
});

const TOGGLE_SIZES = keysOf<ToggleSize>({
  segmented: true,
  compact: true,
  xs: true,
  sm: true,
  default: true,
  lg: true,
});

function OverlayBackdrop({ children }: { readonly children: React.ReactNode }) {
  // Media buttons sit on images; give them something to sit on. The backdrop is
  // `relative` so `media-navigation`, which positions itself, stays inside it.
  return (
    <div className="relative min-h-12 min-w-36 rounded-md bg-[linear-gradient(135deg,#64748b,#0f172a)] p-2">
      {children}
    </div>
  );
}

function LoadingButton() {
  return (
    <Button disabled>
      <Spinner />
      Saving
    </Button>
  );
}

export const button: GalleryDoc = {
  meta: { name: "Button", kind: "primitive", group: "actions" },
  specimens: [
    {
      id: "split",
      title: "Split primary action",
      render: () => (
        <SplitButton aria-label="Chat actions">
          <Button>Ask</Button>
          <Menu>
            <MenuTrigger render={<Button size="icon" aria-label="More chat actions" />}>
              <ChevronDownIcon />
            </MenuTrigger>
            <MenuPopup align="end">
              <MenuItem className="min-h-11 sm:min-h-11">Ask Detent</MenuItem>
              <MenuItem className="min-h-11 sm:min-h-11">New issue</MenuItem>
              <MenuItem className="min-h-11 sm:min-h-11">Recent chats</MenuItem>
            </MenuPopup>
          </Menu>
        </SplitButton>
      ),
    },
    {
      id: "variants",
      title: "Variants × states",
      note: "Hover is forced with `data-pressed`, the attribute the variants share with :hover. Tab into the frame for the focus ring.",
      render: () => (
        <Matrix
          rows={BUTTON_VARIANTS}
          columns={["rest", "hover (forced)", "disabled", "with icon"] as const}
          render={(variant, state) => {
            const node =
              state === "rest" ? (
                <Button variant={variant}>Save</Button>
              ) : state === "hover (forced)" ? (
                <Button variant={variant} data-pressed="">
                  Save
                </Button>
              ) : state === "disabled" ? (
                <Button variant={variant} disabled>
                  Save
                </Button>
              ) : (
                <Button variant={variant}>
                  <PlusIcon />
                  New issue
                </Button>
              );
            return MEDIA_VARIANTS.has(variant) ? <OverlayBackdrop>{node}</OverlayBackdrop> : node;
          }}
        />
      ),
    },
    {
      id: "sizes",
      title: "Sizes",
      note: "Text sizes, then icon-only sizes. Below `sm` the default sizes grow a step for touch.",
      render: () => (
        <div className="flex flex-col gap-4">
          <Row>
            {BUTTON_TEXT_SIZES.map((size) => (
              <Cell key={size} label={size}>
                {size === "sm-multiline" ? (
                  <div className="w-36">
                    <Button size={size} variant="outline">
                      <PlusIcon />
                      A label that wraps onto two lines
                    </Button>
                  </div>
                ) : (
                  <Button size={size} variant="outline">
                    <PlusIcon />
                    Button
                  </Button>
                )}
              </Cell>
            ))}
          </Row>
          <Row>
            {BUTTON_ICON_SIZES.map((size) => (
              <Cell key={size} label={size}>
                <Button size={size} variant="outline" aria-label="Add">
                  <PlusIcon />
                </Button>
              </Cell>
            ))}
          </Row>
        </div>
      ),
    },
    {
      id: "edge",
      title: "Loading, long label, as link",
      render: () => (
        <Row>
          <Cell label="loading (Spinner + disabled)">
            <LoadingButton />
          </Cell>
          <Cell label="long label (no wrap)" className="max-w-full">
            <div className="max-w-72 overflow-hidden">
              <Button variant="outline">{LONG_LABEL}</Button>
            </div>
          </Cell>
          <Cell label="render={<a />}">
            <Button variant="link" render={<a href="#specimen" />}>
              Open issue
            </Button>
          </Cell>
        </Row>
      ),
    },
    {
      id: "inline",
      title: "InlineButton tones in a sentence",
      note: "A text action inside running prose. `picker` opens a menu; its dotted underline marks it as a choice.",
      render: () => (
        <div className="flex max-w-xl flex-col gap-2 text-sm">
          {INLINE_TONES.map((tone) => (
            <p key={tone} className="m-0 text-muted-foreground">
              <span className="me-2 font-mono text-2xs">{tone}</span>
              The run finished with two warnings.{" "}
              <InlineButton tone={tone}>
                {tone === "destructive" ? "Discard changes" : tone === "picker" ? "Codex" : "View log"}
                {tone === "picker" ? <ChevronDownIcon className="size-3.5" /> : null}
              </InlineButton>{" "}
              to continue.
            </p>
          ))}
        </div>
      ),
    },
  ],
};

export const toggle: GalleryDoc = {
  meta: { name: "Toggle", kind: "primitive", group: "actions" },
  specimens: [
    {
      id: "variants",
      title: "Variants × pressed",
      render: () => (
        <Matrix
          rows={TOGGLE_VARIANTS}
          columns={["off", "pressed", "disabled"] as const}
          render={(variant, state) => (
            <Toggle
              variant={variant}
              aria-label="Bold"
              defaultPressed={state === "pressed"}
              disabled={state === "disabled"}
            >
              <BoldIcon />
            </Toggle>
          )}
        />
      ),
    },
    {
      id: "sizes",
      title: "Sizes",
      render: () => (
        <Row>
          {TOGGLE_SIZES.map((size) => (
            <Cell key={size} label={size}>
              <Toggle size={size} variant="outline" defaultPressed>
                <ItalicIcon />
                Italic
              </Toggle>
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};

export const toggleGroup: GalleryDoc = {
  meta: { name: "Toggle group", kind: "primitive", group: "actions" },
  specimens: [
    {
      id: "variants",
      title: "Variants",
      note: "Arrow keys move between items; one value is pressed at a time unless `multiple`.",
      render: () => (
        <div className="flex flex-col gap-4">
          {TOGGLE_VARIANTS.map((variant) => (
            <Cell key={variant} label={variant}>
              <ToggleGroup variant={variant} defaultValue={["left"]}>
                <ToggleGroupItem value="left" aria-label="Align left">
                  <AlignLeftIcon />
                </ToggleGroupItem>
                {variant === "outline" ? <ToggleGroupSeparator /> : null}
                <ToggleGroupItem value="center" aria-label="Align center">
                  <AlignCenterIcon />
                </ToggleGroupItem>
                {variant === "outline" ? <ToggleGroupSeparator /> : null}
                <ToggleGroupItem value="right" aria-label="Align right">
                  <AlignRightIcon />
                </ToggleGroupItem>
              </ToggleGroup>
            </Cell>
          ))}
        </div>
      ),
    },
    {
      id: "segmented-text",
      title: "Segmented with labels, disabled item, vertical",
      render: () => (
        <Row>
          <Cell label="segmented">
            <ToggleGroup defaultValue={["board"]}>
              <ToggleGroupItem value="board">Board</ToggleGroupItem>
              <ToggleGroupItem value="list">List</ToggleGroupItem>
              <ToggleGroupItem value="archive" disabled>
                Archive
              </ToggleGroupItem>
            </ToggleGroup>
          </Cell>
          <Cell label="outline, vertical">
            <ToggleGroup variant="outline" orientation="vertical" defaultValue={["a"]}>
              <ToggleGroupItem value="a">First</ToggleGroupItem>
              <ToggleGroupItem value="b">Second</ToggleGroupItem>
              <ToggleGroupItem value="c">Third</ToggleGroupItem>
            </ToggleGroup>
          </Cell>
        </Row>
      ),
    },
  ],
};

function TabRow({ label, icon }: { readonly label: string; readonly icon: React.ReactNode }) {
  const [closed, setClosed] = React.useState(false);
  if (closed) {
    return (
      <Button variant="link" size="xs" onClick={() => setClosed(false)}>
        Reopen {label}
      </Button>
    );
  }
  return (
    <div className="group/tab flex h-7 items-center gap-1.5 rounded-md border px-2 text-xs hover:bg-accent">
      <PanelTabCloseButton label={`Close ${label}`} tooltip="Close tab" onClick={() => setClosed(true)}>
        {icon}
      </PanelTabCloseButton>
      <span>{label}</span>
    </div>
  );
}

export const panelTabCloseButton: GalleryDoc = {
  meta: { name: "Panel tab close button", kind: "primitive", group: "actions" },
  specimens: [
    {
      id: "tabs",
      title: "Inside a group/tab row",
      note: "The tab's icon swaps for the close glyph on row hover or keyboard focus.",
      minHeight: 120,
      render: () => (
        <Row>
          <TabRow label="Terminal" icon={<TerminalIcon className="size-3" />} />
          <TabRow label="README.md" icon={<FileTextIcon className="size-3" />} />
        </Row>
      ),
    },
  ],
};

export const refreshIcon: GalleryDoc = {
  meta: { name: "Refresh icon", kind: "primitive", group: "actions" },
  specimens: [
    {
      id: "states",
      title: "Idle and refreshing",
      render: () => (
        <Row>
          <Cell label="idle">
            <Button variant="ghost" size="icon-sm" aria-label="Refresh">
              <RefreshIcon />
            </Button>
          </Cell>
          <Cell label="refreshing">
            <Button variant="ghost" size="icon-sm" aria-label="Refreshing" disabled>
              <RefreshIcon refreshing />
            </Button>
          </Cell>
          <Cell label="with label">
            <Button variant="outline" size="sm">
              <RefreshIcon refreshing />
              Syncing
            </Button>
          </Cell>
        </Row>
      ),
    },
    {
      id: "sizes",
      title: "Sizes",
      note: "`size` for a glyph outside a Button; inside one the button sizes it.",
      render: () => (
        <Row>
          {REFRESH_SIZES.map((size) => (
            <Cell key={size} label={size}>
              <RefreshIcon size={size} />
            </Cell>
          ))}
        </Row>
      ),
    },
  ],
};
