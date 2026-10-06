// Overlays: dialog, alert dialog, sheet, popover, menu, tooltip, preview card.
//
// Every overlay opens on click (or hover, for tooltip and preview card) with
// its real focus trap and Escape handling. Portals land in the frame
// document's body, so they take the frame's theme.
import {
  ArchiveIcon,
  CopyIcon,
  GitBranchIcon,
  InfoIcon,
  MoreHorizontalIcon,
  PencilIcon,
  Trash2Icon,
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
import { Switch } from "~/components/ui/switch";
import { Tooltip, TooltipPopup, TooltipProvider, TooltipTrigger } from "~/components/ui/tooltip";

import { Cell, LONG_LABEL, Row, type GalleryDoc } from "../specimen";

const OVERLAY_HEIGHT = 460;

export const dialog: GalleryDoc = {
  meta: { name: "Dialog", kind: "primitive", group: "overlays" },
  specimens: [
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
  ],
};

export const alertDialog: GalleryDoc = {
  meta: { name: "Alert dialog", kind: "primitive", group: "overlays" },
  specimens: [
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

export const popover: GalleryDoc = {
  meta: { name: "Popover", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "basic",
      title: "With title, form and close",
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

function MenuSpecimen() {
  const [showArchived, setShowArchived] = React.useState(true);
  const [sort, setSort] = React.useState("updated");
  return (
    <Menu>
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

export const menu: GalleryDoc = {
  meta: { name: "Menu", kind: "primitive", group: "overlays" },
  specimens: [
    {
      id: "full",
      title: "Items, shortcut, disabled, submenu, checkbox, radio, destructive",
      note: "Arrow keys move, Right opens the submenu, Escape closes one level at a time.",
      minHeight: 440,
      render: () => <MenuSpecimen />,
    },
  ],
};

export const tooltip: GalleryDoc = {
  meta: { name: "Tooltip", kind: "primitive", group: "overlays" },
  specimens: [
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
