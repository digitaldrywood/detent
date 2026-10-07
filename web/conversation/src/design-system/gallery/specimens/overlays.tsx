// Overlays: dialog, alert dialog, sheet, popover, menu, tooltip, preview card.
//
// Each overlay's first specimen renders it open (`defaultOpen`), so the page
// shows the popup without a click; open modals pass `initialFocus={false}` so
// a frame loading never pulls focus into itself. The specimens after it open
// on click (or hover, for tooltip and preview card) with the real focus trap
// and Escape handling. Portals land in the frame document's body, so they
// take the frame's theme.
import {
  ArchiveIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  CopyIcon,
  GitBranchIcon,
  InfoIcon,
  MoreHorizontalIcon,
  PencilIcon,
  SmileIcon,
  Trash2Icon,
  XIcon,
} from "lucide-react";
import React from "react";

import {
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "~/components/ui/alert-dialog";
import { Button } from "~/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
  DialogTrigger,
} from "~/components/ui/dialog";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import {
  Menu,
  MenuCheckboxItem,
  MenuGroup,
  MenuGroupLabel,
  MenuItem,
  MenuItemLabel,
  MenuPopup,
  MenuRadioGroup,
  MenuRadioItem,
  MenuSeparator,
  MenuShortcut,
  MenuSub,
  MenuSubPopup,
  MenuSubTrigger,
  MenuTrigger,
} from "~/components/ui/menu";
import {
  Popover,
  PopoverClose,
  PopoverDescription,
  PopoverPopup,
  PopoverTitle,
  PopoverTrigger,
} from "~/components/ui/popover";
import { PreviewCard, PreviewCardPopup, PreviewCardTrigger } from "~/components/ui/preview-card";
import {
  Sheet,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetPanel,
  SheetPopup,
  SheetTitle,
  SheetTrigger,
  SheetClose,
} from "~/components/ui/sheet";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "~/components/ui/select";
import { Switch } from "~/components/ui/switch";
import {
  Tooltip,
  TooltipPopup,
  TooltipProvider,
  TooltipScrollDismissArea,
  TooltipTrigger,
} from "~/components/ui/tooltip";

import { Cell, LONG_LABEL, Row, type GalleryDoc } from "../specimen";

const OVERLAY_HEIGHT = 460;

function RenameDialogBody() {
  return (
    <>
      <DialogHeader>
        <DialogTitle>Rename project</DialogTitle>
        <DialogDescription>The new name shows in the sidebar and on the board.</DialogDescription>
      </DialogHeader>
      <DialogPanel>
        <div className="flex flex-col gap-2">
          <Label htmlFor="ds-dialog-name-open">Name</Label>
          <Input id="ds-dialog-name-open" defaultValue="detent" />
        </div>
      </DialogPanel>
      <DialogFooter>
        <DialogClose render={<Button variant="ghost" />}>Cancel</DialogClose>
        <DialogClose render={<Button />}>Save</DialogClose>
      </DialogFooter>
    </>
  );
}

function MediaDialog({ open }: { readonly open: boolean }) {
  const [index, setIndex] = React.useState(0);
  const shots = ["Board, light", "Board, dark", "Usage chart"] as const;
  return (
    <Dialog defaultOpen={open}>
      <DialogTrigger render={<Button variant="outline" />}>Open screenshot</DialogTrigger>
      <DialogPopup variant="media" showCloseButton={false} initialFocus={open ? false : undefined}>
        <DialogTitle className="sr-only">Screenshot {index + 1} of {shots.length}</DialogTitle>
        <div className="relative">
          <div className="flex aspect-video w-[min(36rem,80vw)] items-end rounded-lg bg-[linear-gradient(135deg,#64748b,#0f172a)] p-4 text-sm text-white/90">
            {shots[index]}
          </div>
          <Button
            variant="media-navigation"
            size="icon-lg"
            className="start-2"
            aria-label="Previous screenshot"
            disabled={index === 0}
            onClick={() => setIndex((current) => Math.max(0, current - 1))}
          >
            <ChevronLeftIcon />
          </Button>
          <Button
            variant="media-navigation"
            size="icon-lg"
            className="end-2"
            aria-label="Next screenshot"
            disabled={index === shots.length - 1}
            onClick={() => setIndex((current) => Math.min(shots.length - 1, current + 1))}
          >
            <ChevronRightIcon />
          </Button>
          <DialogClose
            render={<Button variant="media-close" size="icon-sm" className="absolute -end-2 -top-2" />}
            aria-label="Close"
          >
            <XIcon />
          </DialogClose>
        </div>
      </DialogPopup>
    </Dialog>
  );
}

export const dialog: GalleryDoc = {
  meta: { name: "Dialog", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "open",
      title: "Open: form dialog",
      note: "Rendered open with `initialFocus={false}`, so loading the page does not move focus into the frame. Tab moves into the dialog; Escape closes it and the trigger reopens it.",
      minHeight: 400,
      render: () => (
        <Dialog defaultOpen>
          <DialogTrigger render={<Button variant="outline" />}>Rename project</DialogTrigger>
          <DialogPopup initialFocus={false}>
            <RenameDialogBody />
          </DialogPopup>
        </Dialog>
      ),
    },
    {
      id: "media",
      title: "Media variant",
      note: "`variant=\"media\"` drops the glass card for an image on a dark backdrop, with `media-navigation` and `media-close` buttons. Rendered open; the arrows step through the shots.",
      minHeight: 420,
      render: () => <MediaDialog open />,
    },
    {
      id: "form",
      title: "Form dialog",
      note: "Focus moves into the dialog and is trapped; Escape or the close button returns it to the trigger.",
      minHeight: OVERLAY_HEIGHT,
      render: () => (
        <Dialog>
          <DialogTrigger render={<Button variant="outline" />}>Rename project</DialogTrigger>
          <DialogPopup>
            <DialogHeader>
              <DialogTitle>Rename project</DialogTitle>
              <DialogDescription>The new name shows in the sidebar and on the board.</DialogDescription>
            </DialogHeader>
            <DialogPanel>
              <div className="flex flex-col gap-2">
                <Label htmlFor="ds-dialog-name">Name</Label>
                <Input id="ds-dialog-name" defaultValue="detent" />
              </div>
            </DialogPanel>
            <DialogFooter>
              <DialogClose render={<Button variant="ghost" />}>Cancel</DialogClose>
              <DialogClose render={<Button />}>Save</DialogClose>
            </DialogFooter>
          </DialogPopup>
        </Dialog>
      ),
    },
    {
      id: "long",
      title: "Scrolling body, bare footer",
      minHeight: OVERLAY_HEIGHT,
      render: () => (
        <Dialog>
          <DialogTrigger render={<Button variant="outline" />}>Open long dialog</DialogTrigger>
          <DialogPopup>
            <DialogHeader>
              <DialogTitle>{LONG_LABEL}</DialogTitle>
            </DialogHeader>
            <DialogPanel>
              {Array.from({ length: 14 }, (_, index) => (
                <p key={index} className="mb-3 text-muted-foreground text-sm">
                  Paragraph {index + 1}. The panel scrolls and fades at its edges while the header and
                  footer stay put.
                </p>
              ))}
            </DialogPanel>
            <DialogFooter variant="bare">
              <DialogClose render={<Button />}>Done</DialogClose>
            </DialogFooter>
          </DialogPopup>
        </Dialog>
      ),
    },
    {
      id: "nested",
      title: "Select and menu inside a dialog",
      note: "Popups opened from a dialog layer above it (z-[130] over z-50) and close one level per Escape: the popup first, then the dialog.",
      minHeight: 560,
      render: () => (
        <Dialog>
          <DialogTrigger render={<Button variant="outline" />}>Move issue</DialogTrigger>
          <DialogPopup>
            <DialogHeader>
              <DialogTitle>Move issue</DialogTitle>
              <DialogDescription>Choose the lane, or pick another action.</DialogDescription>
            </DialogHeader>
            <DialogPanel>
              <div className="flex flex-col gap-4">
                <div className="flex flex-col gap-2">
                  <Label id="ds-nested-lane">Lane</Label>
                  <Select<string> defaultValue="Todo">
                    <SelectTrigger aria-labelledby="ds-nested-lane" className="w-48">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectPopup>
                      {["Backlog", "Todo", "In Progress", "In Review", "Done"].map((lane) => (
                        <SelectItem key={lane} value={lane}>
                          {lane}
                        </SelectItem>
                      ))}
                    </SelectPopup>
                  </Select>
                </div>
                <Menu>
                  <MenuTrigger render={<Button variant="outline" size="sm" className="self-start" />}>
                    <MoreHorizontalIcon aria-hidden />
                    More actions
                  </MenuTrigger>
                  <MenuPopup align="start">
                    <MenuItem>
                      <CopyIcon aria-hidden />
                      Copy link
                    </MenuItem>
                    <MenuItem>
                      <ArchiveIcon aria-hidden />
                      Archive
                    </MenuItem>
                    <MenuSeparator />
                    <MenuItem variant="destructive">
                      <Trash2Icon aria-hidden />
                      Delete
                    </MenuItem>
                  </MenuPopup>
                </Menu>
              </div>
            </DialogPanel>
            <DialogFooter>
              <DialogClose render={<Button variant="ghost" />}>Cancel</DialogClose>
              <DialogClose render={<Button />}>Move</DialogClose>
            </DialogFooter>
          </DialogPopup>
        </Dialog>
      ),
    },
  ],
};

export const alertDialog: GalleryDoc = {
  meta: { name: "Alert dialog", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "open",
      title: "Open: destructive confirmation",
      note: "Rendered open with `initialFocus={false}`. Choose either action to close it; the trigger reopens it.",
      minHeight: 320,
      render: () => (
        <AlertDialog defaultOpen>
          <AlertDialogTrigger render={<Button variant="destructive-outline" />}>
            <Trash2Icon />
            Delete worktree
          </AlertDialogTrigger>
          <AlertDialogPopup initialFocus={false}>
            <AlertDialogHeader>
              <AlertDialogTitle>Delete this worktree?</AlertDialogTitle>
              <AlertDialogDescription>
                Uncommitted changes in feat/design-system are lost. This cannot be undone.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogClose render={<Button variant="ghost" />}>Cancel</AlertDialogClose>
              <AlertDialogClose render={<Button variant="destructive" />}>Delete</AlertDialogClose>
            </AlertDialogFooter>
          </AlertDialogPopup>
        </AlertDialog>
      ),
    },
    {
      id: "destructive",
      title: "Destructive confirmation",
      note: "No outside-click dismissal: the reader has to choose.",
      minHeight: OVERLAY_HEIGHT,
      render: () => (
        <AlertDialog>
          <AlertDialogTrigger render={<Button variant="destructive-outline" />}>
            <Trash2Icon />
            Delete worktree
          </AlertDialogTrigger>
          <AlertDialogPopup>
            <AlertDialogHeader>
              <AlertDialogTitle>Delete this worktree?</AlertDialogTitle>
              <AlertDialogDescription>
                Uncommitted changes in feat/design-system are lost. This cannot be undone.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogClose render={<Button variant="ghost" />}>Cancel</AlertDialogClose>
              <AlertDialogClose render={<Button variant="destructive" />}>Delete</AlertDialogClose>
            </AlertDialogFooter>
          </AlertDialogPopup>
        </AlertDialog>
      ),
    },
  ],
};

export const sheet: GalleryDoc = {
  meta: { name: "Sheet", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "open",
      title: "Open: right sheet",
      note: "Rendered open with `initialFocus={false}`. The backdrop, Escape or Close dismisses it; the trigger reopens it.",
      minHeight: OVERLAY_HEIGHT,
      render: () => (
        <Sheet defaultOpen>
          <SheetTrigger render={<Button variant="outline" />}>Issue details</SheetTrigger>
          <SheetPopup side="right" initialFocus={false}>
            <SheetHeader>
              <SheetTitle>Issue details</SheetTitle>
              <SheetDescription>detent#142 · In review</SheetDescription>
            </SheetHeader>
            <SheetPanel>
              <p className="text-muted-foreground text-sm">
                Port the design-system gallery so every primitive renders in both themes.
              </p>
            </SheetPanel>
            <SheetFooter>
              <SheetClose render={<Button variant="ghost" />}>Close</SheetClose>
            </SheetFooter>
          </SheetPopup>
        </Sheet>
      ),
    },
    {
      id: "sides",
      title: "Sides and variants",
      minHeight: OVERLAY_HEIGHT,
      render: () => (
        <Row>
          {(["right", "left", "top", "bottom"] as const).map((side) => (
            <Sheet key={side}>
              <SheetTrigger render={<Button variant="outline" />}>{side}</SheetTrigger>
              <SheetPopup side={side}>
                <SheetHeader>
                  <SheetTitle>Issue details</SheetTitle>
                  <SheetDescription>Opened from the {side}.</SheetDescription>
                </SheetHeader>
                <SheetPanel>
                  <p className="text-muted-foreground text-sm">Sheet body content.</p>
                </SheetPanel>
                <SheetFooter>
                  <SheetClose render={<Button variant="ghost" />}>Close</SheetClose>
                </SheetFooter>
              </SheetPopup>
            </Sheet>
          ))}
          <Sheet>
            <SheetTrigger render={<Button variant="outline" />}>right, inset</SheetTrigger>
            <SheetPopup side="right" variant="inset">
              <SheetHeader>
                <SheetTitle>Inset sheet</SheetTitle>
              </SheetHeader>
              <SheetPanel>
                <p className="text-muted-foreground text-sm">Floats off the edge.</p>
              </SheetPanel>
            </SheetPopup>
          </Sheet>
        </Row>
      ),
    },
  ],
};

const POPOVER_WIDTHS = ["sm", "md", "lg"] as const;
const POPOVER_PADDINGS = ["default", "compact", "none"] as const;

export const popover: GalleryDoc = {
  meta: { name: "Popover", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "open",
      title: "Open: title, form and close",
      note: "Rendered open (`defaultOpen`, `initialFocus={false}`). A click outside closes it; the trigger reopens it.",
      minHeight: 300,
      render: () => (
        <Popover defaultOpen>
          <PopoverTrigger render={<Button variant="outline" />}>Branch settings</PopoverTrigger>
          <PopoverPopup width="sm" align="start" initialFocus={false}>
            <PopoverTitle>Branch</PopoverTitle>
            <PopoverDescription>Where the agent commits its work.</PopoverDescription>
            <div className="mt-3 flex flex-col gap-3">
              <Input defaultValue="feat/design-system" aria-label="Branch" />
              <Label>
                <Switch defaultChecked />
                Push on completion
              </Label>
              <PopoverClose render={<Button size="sm" />}>Apply</PopoverClose>
            </div>
          </PopoverPopup>
        </Popover>
      ),
    },
    {
      id: "widths",
      title: "Widths: sm, md, lg",
      note: "`width` fixes the popup's width (capped to the viewport). Each opens to the right of its trigger.",
      minHeight: 340,
      render: () => (
        <div className="flex flex-col gap-20">
          {POPOVER_WIDTHS.map((width) => (
            <Popover key={width} defaultOpen>
              <PopoverTrigger render={<Button variant="outline" size="sm" className="self-start" />}>
                width=&quot;{width}&quot;
              </PopoverTrigger>
              <PopoverPopup width={width} side="right" align="start" initialFocus={false}>
                <PopoverDescription>A {width} popover holds a sentence or a short form.</PopoverDescription>
              </PopoverPopup>
            </Popover>
          ))}
        </div>
      ),
    },
    {
      id: "padding",
      title: "Padding: default, compact, none",
      note: "`compact` suits dense content; `none` is for content that draws its own frame edge to edge.",
      minHeight: 360,
      render: () => (
        <div className="flex flex-col gap-24">
          {POPOVER_PADDINGS.map((padding) => (
            <Popover key={padding} defaultOpen>
              <PopoverTrigger render={<Button variant="outline" size="sm" className="self-start" />}>
                padding=&quot;{padding}&quot;
              </PopoverTrigger>
              <PopoverPopup width="sm" padding={padding} side="right" align="start" initialFocus={false}>
                {padding === "none" ? (
                  <div className="bg-muted px-3 py-2 font-mono text-xs">internal/lock/lease.go:42</div>
                ) : (
                  <div className="flex items-center gap-1.5">
                    {["+1", "eyes", "rocket"].map((reaction) => (
                      <Button key={reaction} variant="outline" size="xs">
                        <SmileIcon />
                        {reaction}
                      </Button>
                    ))}
                  </div>
                )}
              </PopoverPopup>
            </Popover>
          ))}
        </div>
      ),
    },
    {
      id: "panel",
      title: "Panel variant",
      note: "`variant=\"panel\"` drops the glass card: the popup is a transparent layer as wide as its anchor (up to the thread details panel width), and its content draws its own surface.",
      minHeight: 260,
      render: () => (
        <Popover defaultOpen>
          <PopoverTrigger render={<Button variant="outline" className="w-72 justify-start" />}>
            Thread details
          </PopoverTrigger>
          <PopoverPopup variant="panel" align="start" initialFocus={false}>
            <div className="flex flex-col gap-2 rounded-xl border bg-card p-3 text-sm shadow-lg/5">
              <span className="font-medium">Flaky checkout lock renewal</span>
              <span className="text-muted-foreground text-xs">detent · Codex · 14 messages</span>
            </div>
          </PopoverPopup>
        </Popover>
      ),
    },
    {
      id: "basic",
      title: "With title, form and close",
      note: "Opens on click.",
      minHeight: 320,
      render: () => (
        <Row>
          <Popover>
            <PopoverTrigger render={<Button variant="outline" />}>Branch settings</PopoverTrigger>
            <PopoverPopup className="w-72">
              <PopoverTitle>Branch</PopoverTitle>
              <PopoverDescription>Where the agent commits its work.</PopoverDescription>
              <div className="mt-3 flex flex-col gap-3">
                <Input defaultValue="feat/design-system" aria-label="Branch" />
                <Label>
                  <Switch defaultChecked />
                  Push on completion
                </Label>
                <PopoverClose render={<Button size="sm" />}>Apply</PopoverClose>
              </div>
            </PopoverPopup>
          </Popover>
          <Popover>
            <PopoverTrigger render={<Button variant="ghost" size="icon-sm" aria-label="Info" />}>
              <InfoIcon />
            </PopoverTrigger>
            <PopoverPopup tooltipStyle side="right">
              Tooltip-styled popover: compact, opened on click.
            </PopoverPopup>
          </Popover>
        </Row>
      ),
    },
  ],
};

function MenuSpecimen({ open = false }: { readonly open?: boolean }) {
  const [showArchived, setShowArchived] = React.useState(true);
  const [sort, setSort] = React.useState("updated");
  return (
    <Menu defaultOpen={open} modal={open ? false : undefined}>
      <MenuTrigger render={<Button variant="outline" size="icon-sm" aria-label="Thread actions" />}>
        <MoreHorizontalIcon />
      </MenuTrigger>
      <MenuPopup align="start">
        <MenuGroup>
          <MenuGroupLabel>Thread</MenuGroupLabel>
          <MenuItem>
            <PencilIcon />
            Rename
            <MenuShortcut>R</MenuShortcut>
          </MenuItem>
          <MenuItem>
            <CopyIcon />
            Copy link
            <MenuShortcut>⌘C</MenuShortcut>
          </MenuItem>
          <MenuItem disabled>
            <GitBranchIcon />
            Open pull request
          </MenuItem>
          <MenuSub>
            <MenuSubTrigger>
              <ArchiveIcon />
              Move to
            </MenuSubTrigger>
            <MenuSubPopup>
              <MenuItem>Backlog</MenuItem>
              <MenuItem>Todo</MenuItem>
              <MenuItem>In review</MenuItem>
            </MenuSubPopup>
          </MenuSub>
        </MenuGroup>
        <MenuSeparator />
        <MenuCheckboxItem checked={showArchived} onCheckedChange={setShowArchived}>
          Show archived
        </MenuCheckboxItem>
        <MenuSeparator />
        <MenuGroup>
          <MenuGroupLabel>Sort by</MenuGroupLabel>
          <MenuRadioGroup value={sort} onValueChange={setSort}>
            <MenuRadioItem value="updated">Last updated</MenuRadioItem>
            <MenuRadioItem value="created">Created</MenuRadioItem>
          </MenuRadioGroup>
        </MenuGroup>
        <MenuSeparator />
        <MenuItem variant="destructive">
          <Trash2Icon />
          Delete
        </MenuItem>
      </MenuPopup>
    </Menu>
  );
}

function TouchMenu() {
  return (
    <Menu defaultOpen modal={false}>
      <MenuTrigger render={<Button variant="outline" size="sm" />}>Touch density</MenuTrigger>
      <MenuPopup align="start">
        {["Rename", "Copy link", "Move to…"].map((label) => (
          <MenuItem key={label} density="touch">
            {label === "Rename" ? <PencilIcon /> : label === "Copy link" ? <CopyIcon /> : <ArchiveIcon />}
            <MenuItemLabel>{label}</MenuItemLabel>
          </MenuItem>
        ))}
        <MenuItem density="touch">
          <GitBranchIcon />
          <MenuItemLabel>{LONG_LABEL}</MenuItemLabel>
        </MenuItem>
        <MenuSeparator />
        <MenuItem variant="ghost">
          <Trash2Icon />
          Ghost item
        </MenuItem>
      </MenuPopup>
    </Menu>
  );
}

export const menu: GalleryDoc = {
  meta: { name: "Menu", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "open",
      title: "Open: items, shortcut, disabled, submenu, checkbox, radio, destructive",
      note: "Rendered open (`defaultOpen`, `modal={false}` so the page keeps scrolling). A click outside closes it; the trigger reopens it.",
      minHeight: 440,
      render: () => <MenuSpecimen open />,
    },
    {
      id: "touch",
      title: "Touch density, MenuItemLabel, ghost item",
      note: "`density=\"touch\"` makes rows 40px tall; `MenuItemLabel` trims the label's leading so it centres with the icon and truncates. `variant=\"ghost\"` draws the item as a compact ghost button.",
      minHeight: 300,
      render: () => <TouchMenu />,
    },
    {
      id: "full",
      title: "Items, shortcut, disabled, submenu, checkbox, radio, destructive",
      note: "Arrow keys move, Right opens the submenu, Escape closes one level at a time.",
      minHeight: 440,
      render: () => <MenuSpecimen />,
    },
  ],
};

const TOOLTIP_VARIANTS = ["default", "glass", "code"] as const;

function ScrollDismissSpecimen() {
  return (
    <TooltipProvider>
      <TooltipScrollDismissArea className="h-48 w-72 max-w-full overflow-y-auto rounded-lg border p-2">
        <div className="flex flex-col gap-1">
          {Array.from({ length: 16 }, (_, index) => (
            <Tooltip key={index}>
              <TooltipTrigger render={<Button variant="ghost" size="sm" className="justify-start" />}>
                Step {index + 1}
              </TooltipTrigger>
              <TooltipPopup side="right" variant="code">
                go test ./internal/lock/... -run TestRenew/{index + 1}
              </TooltipPopup>
            </Tooltip>
          ))}
        </div>
      </TooltipScrollDismissArea>
    </TooltipProvider>
  );
}

export const tooltip: GalleryDoc = {
  meta: { name: "Tooltip", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "open",
      title: "Open: default, glass and code",
      note: "Rendered open (`defaultOpen`). `code` sets paths and commands in mono and breaks them anywhere, in a wider box.",
      minHeight: 220,
      render: () => (
        <div className="flex flex-col gap-12 py-2">
          {TOOLTIP_VARIANTS.map((variant) => (
            <Tooltip key={variant} defaultOpen>
              <TooltipTrigger render={<Button variant="outline" size="sm" className="self-start" />}>
                {variant}
              </TooltipTrigger>
              <TooltipPopup side="right" variant={variant}>
                {variant === "code"
                  ? "/Users/michael/Development/detent-worktrees/detent/design-system/internal/lock/lease.go"
                  : `A ${variant} tooltip`}
              </TooltipPopup>
            </Tooltip>
          ))}
        </div>
      ),
    },
    {
      id: "scroll-dismiss",
      title: "Scroll dismiss area",
      note: "Hover a step, then scroll the list: `TooltipScrollDismissArea` closes the hovered tooltip on a real scroll instead of leaving it behind the moved trigger.",
      minHeight: 220,
      render: () => <ScrollDismissSpecimen />,
    },
    {
      id: "variants",
      title: "Variants and sides",
      note: "Hover or focus a trigger. A shared provider makes adjacent tooltips open instantly.",
      minHeight: 200,
      render: () => (
        <TooltipProvider>
          <div className="flex flex-wrap items-center gap-3 p-10">
            {(["top", "right", "bottom", "left"] as const).map((side) => (
              <Tooltip key={side}>
                <TooltipTrigger render={<Button variant="outline" size="sm" />}>{side}</TooltipTrigger>
                <TooltipPopup side={side}>Tooltip on the {side}</TooltipPopup>
              </Tooltip>
            ))}
            <Tooltip>
              <TooltipTrigger render={<Button variant="outline" size="sm" />}>glass</TooltipTrigger>
              <TooltipPopup variant="glass">Glass tooltip</TooltipPopup>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger render={<Button variant="outline" size="sm" />}>code</TooltipTrigger>
              <TooltipPopup variant="code">git push origin feat/design-system</TooltipPopup>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger render={<Button variant="outline" size="sm" />}>long</TooltipTrigger>
              <TooltipPopup className="max-w-64">{LONG_LABEL}</TooltipPopup>
            </Tooltip>
          </div>
        </TooltipProvider>
      ),
    },
  ],
};

export const previewCard: GalleryDoc = {
  meta: { name: "Preview card", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "open",
      title: "Open: preview of a link",
      note: "Rendered open (`defaultOpen`). Moving the pointer away closes it; hovering the link reopens it.",
      minHeight: 160,
      render: () => (
        <PreviewCard defaultOpen>
          <PreviewCardTrigger href="#specimen" className="text-primary underline-offset-4 hover:underline">
            DET-142 Design system gallery
          </PreviewCardTrigger>
          <PreviewCardPopup className="w-72" side="bottom">
            <div className="flex flex-col gap-1">
              <span className="font-medium text-sm">DET-142 Design system gallery</span>
              <span className="text-muted-foreground text-xs">In review · detent · updated 4m ago</span>
            </div>
          </PreviewCardPopup>
        </PreviewCard>
      ),
    },
    {
      id: "link",
      title: "Hover preview of a link",
      minHeight: 260,
      render: () => (
        <Cell label="hover the link">
          <PreviewCard>
            <PreviewCardTrigger href="#specimen" className="text-primary underline-offset-4 hover:underline">
              DET-142 Design system gallery
            </PreviewCardTrigger>
            <PreviewCardPopup className="w-72">
              <div className="flex flex-col gap-1">
                <span className="font-medium text-sm">DET-142 Design system gallery</span>
                <span className="text-muted-foreground text-xs">In review · detent · updated 4m ago</span>
              </div>
            </PreviewCardPopup>
          </PreviewCard>
        </Cell>
      ),
    },
  ],
};
