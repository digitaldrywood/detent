// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Alert, AlertDescription } from "../../../src/components/ui/alert";
import { badgeVariants } from "../../../src/components/ui/badge";
import { Button, buttonVariants, InlineButton } from "../../../src/components/ui/button";
import { Collapsible, CollapsiblePanel } from "../../../src/components/ui/collapsible";
import { Dialog, DialogPopup, DialogTitle } from "../../../src/components/ui/dialog";
import { Empty, EmptyTitle } from "../../../src/components/ui/empty";
import { Input } from "../../../src/components/ui/input";
import { Menu, MenuItem, MenuItemLabel, MenuPopup, MenuTrigger } from "../../../src/components/ui/menu";
import { Popover, PopoverPopup, PopoverTrigger } from "../../../src/components/ui/popover";
import { RefreshIcon } from "../../../src/components/ui/refresh-icon";
import { ScrollArea } from "../../../src/components/ui/scroll-area";
import { Skeleton } from "../../../src/components/ui/skeleton";
import { Spinner } from "../../../src/components/ui/spinner";
import { toggleVariants } from "../../../src/components/ui/toggle";
import { cn } from "../../../src/lib/utils";

// nwsapi in jsdom recurses when Floating UI checks :fullscreen.
beforeEach(() => {
  const matches = Element.prototype.matches;
  vi.spyOn(Element.prototype, "matches").mockImplementation(function (this: Element, selector) {
    return selector === ":fullscreen" ? false : matches.call(this, selector);
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function classes(element: Element | null | undefined): string[] {
  return (element?.getAttribute("class") ?? "").split(/\s+/).filter(Boolean);
}

describe("cn", () => {
  it.each([
    { input: ["text-2xs", "text-muted-foreground"], expected: "text-2xs text-muted-foreground" },
    { input: ["text-3xs", "text-foreground"], expected: "text-3xs text-foreground" },
    { input: ["text-sm", "text-4xs"], expected: "text-4xs" },
    { input: ["text-5xs", "text-xs"], expected: "text-xs" },
  ])("merges $input into $expected", ({ input, expected }) => {
    expect(cn(...input)).toBe(expected);
  });
});

describe("buttonVariants", () => {
  it.each([
    { variant: "ghost-destructive" as const, token: "[:hover,[data-pressed]]:text-destructive" },
    { variant: "media-close" as const, token: "bg-black/65" },
    { variant: "media-navigation" as const, token: "-translate-y-1/2" },
    { variant: "glass" as const, token: "rounded-full" },
    { variant: "ghost-muted" as const, token: "[--control-icon-color:currentColor]" },
  ])("variant $variant includes $token", ({ variant, token }) => {
    expect(buttonVariants({ variant }).split(" ")).toContain(token);
  });

  it.each([
    { size: "icon-tiny" as const, token: "size-4" },
    { size: "sm-multiline" as const, token: "whitespace-normal" },
  ])("size $size includes $token", ({ size, token }) => {
    expect(buttonVariants({ size }).split(" ")).toContain(token);
  });

  it("dims aria-disabled buttons without removing them from the tab order", () => {
    render(<Button aria-disabled="true">Save</Button>);
    const button = screen.getByRole("button", { name: "Save" });
    expect(classes(button)).toEqual(
      expect.arrayContaining(["aria-disabled:cursor-not-allowed", "aria-disabled:opacity-64"]),
    );
    expect(button.hasAttribute("disabled")).toBe(false);
  });
});

describe("InlineButton", () => {
  it.each([
    { tone: undefined, token: "text-foreground" },
    { tone: "muted" as const, token: "text-muted-foreground" },
    { tone: "destructive" as const, token: "text-destructive/80" },
    { tone: "picker" as const, token: "decoration-dotted" },
  ])("tone $tone includes $token", ({ tone, token }) => {
    render(<InlineButton tone={tone}>Open</InlineButton>);
    const button = screen.getByRole("button", { name: "Open" });
    expect(button.getAttribute("data-slot")).toBe("inline-button");
    expect(button.getAttribute("type")).toBe("button");
    expect(classes(button)).toContain(token);
  });
});

describe("badge and toggle variants", () => {
  it.each([
    { name: "badge label", value: badgeVariants({ variant: "label" }), token: "bg-[color-mix(in_srgb,var(--label)_8%,transparent)]" },
    { name: "toggle pill", value: toggleVariants({ variant: "pill" }), token: "rounded-full" },
    { name: "toggle aria-disabled", value: toggleVariants({}), token: "aria-disabled:opacity-64" },
  ])("$name includes $token", ({ value, token }) => {
    expect(value.split(" ")).toContain(token);
  });
});

describe("Alert", () => {
  it.each([
    { surface: undefined, variant: undefined, dataVariant: "default", glass: false },
    { surface: "glass" as const, variant: "warning" as const, dataVariant: "warning", glass: true },
    { surface: "default" as const, variant: "sidebar" as const, dataVariant: "sidebar", glass: false },
  ])("surface=$surface variant=$variant", ({ surface, variant, dataVariant, glass }) => {
    render(
      <Alert surface={surface} variant={variant}>
        <AlertDescription>Body</AlertDescription>
      </Alert>,
    );
    const alert = screen.getByRole("alert");
    expect(alert.getAttribute("data-variant")).toBe(dataVariant);
    expect(classes(alert).includes("alert-glass")).toBe(glass);
  });
});

describe("Skeleton, Spinner and RefreshIcon sizing", () => {
  it.each([
    { shape: undefined, token: "rounded-sm" },
    { shape: "card" as const, token: "rounded-lg" },
    { shape: "pill" as const, token: "rounded-full" },
  ])("skeleton shape $shape renders $token", ({ shape, token }) => {
    const { container } = render(<Skeleton shape={shape} />);
    expect(classes(container.querySelector('[data-slot="skeleton"]'))).toContain(token);
  });

  it.each([
    { size: undefined, tone: undefined, present: [], absent: ["size-3", "size-4", "text-muted-foreground"] },
    { size: "xs" as const, tone: undefined, present: ["size-3"], absent: [] },
    { size: "lg" as const, tone: "muted" as const, present: ["size-5", "text-muted-foreground"], absent: [] },
  ])("spinner size=$size tone=$tone", ({ size, tone, present, absent }) => {
    render(<Spinner size={size} tone={tone} />);
    const spinner = screen.getByRole("status");
    for (const token of present) expect(classes(spinner)).toContain(token);
    for (const token of absent) expect(classes(spinner)).not.toContain(token);
  });

  it.each([
    { size: "sm" as const, refreshing: false, present: ["size-3.5"], absent: ["motion-safe:visible-animate-spin"] },
    { size: "md" as const, refreshing: true, present: ["size-4", "motion-safe:visible-animate-spin"], absent: [] },
  ])("refresh icon size=$size refreshing=$refreshing", ({ size, refreshing, present, absent }) => {
    const { container } = render(<RefreshIcon size={size} refreshing={refreshing} />);
    const icon = container.querySelector("svg");
    for (const token of present) expect(classes(icon)).toContain(token);
    for (const token of absent) expect(classes(icon)).not.toContain(token);
  });
});

describe("Empty", () => {
  it.each([
    { size: undefined, token: "md:p-12" },
    { size: "compact" as const, token: "min-h-64" },
    { size: "hero" as const, token: "[&_[data-slot=empty-title]]:text-2xl" },
  ])("size $size includes $token", ({ size, token }) => {
    const { container } = render(
      <Empty size={size}>
        <EmptyTitle>Nothing here</EmptyTitle>
      </Empty>,
    );
    expect(classes(container.querySelector('[data-slot="empty"]'))).toContain(token);
  });
});

describe("Input", () => {
  it.each([
    { font: undefined, type: "text", present: [], absent: ["font-mono", "[appearance:textfield]"] },
    { font: "mono" as const, type: "text", present: ["font-mono", "tabular-nums"], absent: [] },
    { font: undefined, type: "number", present: ["[appearance:textfield]"], absent: ["font-mono"] },
  ])("font=$font type=$type", ({ font, type, present, absent }) => {
    const { container } = render(<Input aria-label="Field" font={font} type={type} />);
    const wrapper = container.querySelector('[data-slot="input-control"]');
    const input = screen.getByLabelText("Field");
    const all = [...classes(wrapper), ...classes(input)];
    for (const token of present) expect(all).toContain(token);
    for (const token of absent) expect(all).not.toContain(token);
  });
});

describe("ScrollArea", () => {
  it.each([
    // Without overflow Base UI keeps the viewport out of the tab order.
    { radius: undefined, viewportTabIndex: undefined, rootToken: "rounded-[inherit]", tabIndex: "-1" },
    { radius: "none" as const, viewportTabIndex: 0, rootToken: "rounded-none", tabIndex: "0" },
  ])("radius=$radius viewportTabIndex=$viewportTabIndex", ({ radius, viewportTabIndex, rootToken, tabIndex }) => {
    const { container } = render(
      <ScrollArea radius={radius} viewportTabIndex={viewportTabIndex}>
        <p>content</p>
      </ScrollArea>,
    );
    const viewport = container.querySelector('[data-slot="scroll-area-viewport"]');
    expect(classes(viewport?.parentElement)).toContain(rootToken);
    expect(classes(viewport)).toContain("transition-shadow");
    expect(viewport?.getAttribute("tabindex")).toBe(tabIndex);
  });
});

describe("CollapsiblePanel", () => {
  it.each([
    { animate: true, animated: true },
    { animate: false, animated: false },
  ])("animate=$animate", ({ animate, animated }) => {
    const { container } = render(
      <Collapsible defaultOpen>
        <CollapsiblePanel animate={animate}>Body</CollapsiblePanel>
      </Collapsible>,
    );
    const panel = container.querySelector('[data-slot="collapsible-panel"]');
    expect(classes(panel)).toContain("overflow-hidden");
    expect(classes(panel).includes("transition-[height]")).toBe(animated);
  });
});

describe("MenuItem", () => {
  it.each([
    { density: undefined, variant: undefined, present: ["min-h-8"], absent: ["min-h-10"] },
    { density: "touch" as const, variant: undefined, present: ["min-h-10"], absent: [] },
    { density: undefined, variant: "ghost" as const, present: ["w-full"], absent: [] },
  ])("density=$density variant=$variant", ({ density, variant, present, absent }) => {
    render(
      <Menu defaultOpen>
        <MenuTrigger>Actions</MenuTrigger>
        <MenuPopup>
          <MenuItem density={density} variant={variant}>
            <MenuItemLabel>Rename</MenuItemLabel>
          </MenuItem>
        </MenuPopup>
      </Menu>,
    );
    const item = screen.getByRole("menuitem", { name: "Rename" });
    expect(item.getAttribute("data-density")).toBe(density ?? "default");
    expect(item.querySelector('[data-slot="menu-item-label"]')).not.toBeNull();
    for (const token of present) expect(classes(item)).toContain(token);
    for (const token of absent) expect(classes(item)).not.toContain(token);
  });

  it("caps the popup to the viewport with one minimum width", () => {
    render(
      <Menu defaultOpen>
        <MenuTrigger>Actions</MenuTrigger>
        <MenuPopup>
          <MenuItem>Rename</MenuItem>
        </MenuPopup>
      </Menu>,
    );
    const popup = document.querySelector('[data-slot="menu-popup"]');
    expect(classes(popup)).toEqual(
      expect.arrayContaining(["min-w-[min(10rem,calc(100vw-2rem))]", "max-w-[calc(100vw-2rem)]"]),
    );
  });
});

describe("PopoverPopup", () => {
  it.each([
    { width: undefined, padding: undefined, popupToken: null, viewportToken: "py-4" },
    { width: "sm" as const, padding: "compact" as const, popupToken: "w-64", viewportToken: "py-2" },
    { width: "lg" as const, padding: "none" as const, popupToken: "w-96", viewportToken: "py-0" },
  ])("width=$width padding=$padding", ({ width, padding, popupToken, viewportToken }) => {
    render(
      <Popover defaultOpen>
        <PopoverTrigger>Open</PopoverTrigger>
        <PopoverPopup width={width} padding={padding}>
          Body
        </PopoverPopup>
      </Popover>,
    );
    const popup = document.querySelector('[data-slot="popover-popup"]');
    const viewport = document.querySelector('[data-slot="popover-viewport"]');
    if (popupToken) expect(classes(popup)).toContain(popupToken);
    expect(classes(viewport)).toContain(viewportToken);
  });
});

describe("DialogPopup", () => {
  it.each([
    { variant: undefined, glass: true, token: "max-w-lg" },
    { variant: "media" as const, glass: false, token: "max-w-[92vw]" },
  ])("variant=$variant", ({ variant, glass, token }) => {
    render(
      <Dialog defaultOpen>
        <DialogPopup variant={variant}>
          <DialogTitle>Preview</DialogTitle>
        </DialogPopup>
      </Dialog>,
    );
    const popup = document.querySelector('[data-slot="dialog-popup"]');
    expect(classes(popup).includes("dialog-glass")).toBe(glass);
    expect(classes(popup)).toContain(token);
    expect(classes(screen.getByText("Preview"))).toContain("wrap-anywhere");
  });
});
