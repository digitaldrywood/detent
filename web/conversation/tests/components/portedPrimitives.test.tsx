// @vitest-environment jsdom
//
// Render, variant, and keyboard/ARIA coverage for the design-system
// primitives in src/components/ui.
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Calendar } from "../../src/components/ui/calendar.tsx";
import {
  CollapsibleSectionHeader,
  SectionHeaderStatus,
} from "../../src/components/ui/collapsible-section-header.tsx";
import { ColorHueSlider, ColorSaturationValuePlane } from "../../src/components/ui/color-picker.tsx";
import { Dialog } from "../../src/components/ui/dialog.tsx";
import { DiscoveryList, DiscoveryListRow } from "../../src/components/ui/discovery-list.tsx";
import { DraftInput } from "../../src/components/ui/draft-input.tsx";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "../../src/components/ui/input-group.tsx";
import {
  MiddleTruncate,
  splitForMiddleTruncate,
} from "../../src/components/ui/middle-truncate.tsx";
import {
  NumberField,
  NumberFieldDecrement,
  NumberFieldGroup,
  NumberFieldIncrement,
  NumberFieldInput,
} from "../../src/components/ui/number-field.tsx";
import { QRCodeSvg } from "../../src/components/ui/qr-code.tsx";
import { Radio, RadioGroup } from "../../src/components/ui/radio-group.tsx";
import { StandalonePage, StandalonePageHeader } from "../../src/components/ui/standalone-page.tsx";
import {
  WizardFooter,
  WizardHeader,
  WizardPanel,
  WizardPopup,
  WizardSteps,
} from "../../src/components/ui/wizard.tsx";
import type { HsvColor } from "../../src/lib/color.ts";

afterEach(() => {
  cleanup();
});

describe("Calendar", () => {
  it("renders a month grid with the selected day and navigation", () => {
    const onSelect = vi.fn();
    render(
      <Calendar
        defaultMonth={new Date(2026, 9, 1)}
        selected={new Date(2026, 9, 6)}
        onSelect={onSelect}
      />,
    );
    const grid = screen.getByRole("grid");
    expect(grid.closest("[data-slot=calendar]")).not.toBeNull();
    expect(screen.getByText("October 2026")).toBeTruthy();
    const selected = within(grid).getByRole("button", { name: /October 6/ });
    expect(selected.closest("[data-selected]")).not.toBeNull();

    fireEvent.click(within(grid).getByRole("button", { name: /October 9/ }));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect((onSelect.mock.calls[0]![0] as Date).getDate()).toBe(9);

    fireEvent.click(screen.getByRole("button", { name: /next month/i }));
    expect(screen.getByText("November 2026")).toBeTruthy();
  });

  it("moves focus between days with the arrow keys", () => {
    render(<Calendar defaultMonth={new Date(2026, 9, 1)} selected={new Date(2026, 9, 6)} />);
    const grid = screen.getByRole("grid");
    const day = within(grid).getByRole("button", { name: /October 6/ });
    act(() => day.focus());
    fireEvent.keyDown(day, { key: "ArrowRight" });
    expect(document.activeElement?.getAttribute("aria-label") ?? "").toMatch(/October 7/);
  });

  it("merges caller classNames onto the defaults", () => {
    const { container } = render(
      <Calendar defaultMonth={new Date(2026, 9, 1)} classNames={{ month: "custom-month" }} />,
    );
    expect(container.querySelector(".custom-month")?.className).toContain("w-full");
  });

  it.each([
    [true, 1],
    [false, 0],
  ] as const)("showOutsideDays=%s renders %i trailing September day", (showOutsideDays, count) => {
    render(<Calendar defaultMonth={new Date(2026, 9, 1)} showOutsideDays={showOutsideDays} />);
    expect(screen.queryAllByRole("button", { name: /September 30/ })).toHaveLength(count);
  });
});

describe("CollapsibleSectionHeader", () => {
  it.each([
    ["muted", "text-sidebar-muted-foreground/60"],
    ["info", "text-blue-600"],
    ["emphasized", "text-sidebar-foreground/80"],
    ["accent", "text-primary"],
  ] as const)("applies the %s tone", (tone, expected) => {
    render(
      <CollapsibleSectionHeader expanded={false} tone={tone}>
        Threads
      </CollapsibleSectionHeader>,
    );
    expect(screen.getByRole("button", { name: "Threads" }).className).toContain(expected);
  });

  it("reports and toggles its expanded state", () => {
    function Harness() {
      const [expanded, setExpanded] = useState(false);
      return (
        <CollapsibleSectionHeader
          expanded={expanded}
          onClick={() => setExpanded((value) => !value)}
          accessory={<SectionHeaderStatus>3 failing</SectionHeaderStatus>}
        >
          Pinned
        </CollapsibleSectionHeader>
      );
    }
    render(<Harness />);
    const button = screen.getByRole("button", { name: /Pinned/ });
    expect(button.getAttribute("type")).toBe("button");
    expect(button.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByText("3 failing").className).toContain("text-3xs");
  });
});

describe("Color picker", () => {
  function PlaneHarness({ initial }: { initial: HsvColor }) {
    const [value, setValue] = useState(initial);
    return (
      <>
        <ColorSaturationValuePlane label="Accent" value={value} onChange={setValue} />
        <output data-testid="hsv">{`${value.s.toFixed(2)},${value.v.toFixed(2)}`}</output>
      </>
    );
  }

  it.each([
    ["ArrowRight", {}, "saturation", "0.52,0.50"],
    ["ArrowLeft", { shiftKey: true }, "saturation", "0.40,0.50"],
    ["Home", {}, "saturation", "0.00,0.50"],
    ["End", {}, "brightness", "0.50,1.00"],
    ["ArrowDown", {}, "brightness", "0.50,0.48"],
  ] as const)("plane: %s adjusts %s", (key, modifiers, axis, expected) => {
    render(<PlaneHarness initial={{ h: 200, s: 0.5, v: 0.5 }} />);
    expect(screen.getByRole("group", { name: "Accent saturation and brightness" })).toBeTruthy();
    const slider = screen.getByRole("slider", { name: `Accent ${axis}` });
    expect(slider.getAttribute("aria-valuetext")).toBe("50%");
    fireEvent.keyDown(slider, { key, ...modifiers });
    expect(screen.getByTestId("hsv").textContent).toBe(expected);
  });

  function HueHarness({ initial }: { initial: number }) {
    const [value, setValue] = useState(initial);
    return <ColorHueSlider label="Hue" value={value} onChange={setValue} />;
  }

  it.each([
    [10, "ArrowRight", {}, "11"],
    [10, "ArrowUp", { shiftKey: true }, "20"],
    [0, "ArrowLeft", {}, "359"],
    [355, "ArrowRight", { shiftKey: true }, "5"],
    [10, "Enter", {}, "10"],
  ] as const)("hue %i: %s", (initial, key, modifiers, expected) => {
    render(<HueHarness initial={initial} />);
    const slider = screen.getByRole("slider", { name: "Hue" });
    expect(slider.getAttribute("tabindex")).toBe("0");
    expect(slider.getAttribute("aria-valuemax")).toBe("360");
    fireEvent.keyDown(slider, { key, ...modifiers });
    expect(slider.getAttribute("aria-valuenow")).toBe(expected);
  });
});

describe("DiscoveryList", () => {
  it("renders rows as buttons with title, description and action", () => {
    const onClick = vi.fn();
    render(
      <DiscoveryList>
        <DiscoveryListRow
          icon={<span data-testid="icon" />}
          title="localhost:5173"
          description="Vite dev server"
          action={<span>Open</span>}
          onClick={onClick}
        />
        <DiscoveryListRow icon={null} title="Disabled" description="Off" disabled />
      </DiscoveryList>,
    );
    const row = screen.getByRole("button", { name: /localhost:5173/ });
    expect(row.getAttribute("type")).toBe("button");
    expect(within(row).getByText("Vite dev server")).toBeTruthy();
    expect(within(row).getByText("Open")).toBeTruthy();
    fireEvent.click(row);
    expect(onClick).toHaveBeenCalledTimes(1);
    expect(
      (screen.getByRole("button", { name: /Disabled/ }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });
});

describe("DraftInput", () => {
  it.each([
    ["blur", (input: HTMLElement) => fireEvent.blur(input), ["Work"]],
    ["Enter", (input: HTMLElement) => fireEvent.keyDown(input, { key: "Enter" }), ["Work"]],
  ] as const)("commits the buffered draft on %s", (_label, finish, expected) => {
    const onCommit = vi.fn();
    render(<DraftInput aria-label="Name" value="Home" onCommit={onCommit} />);
    const input = screen.getByRole("textbox", { name: "Name" }) as HTMLInputElement;
    act(() => input.focus());
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "Work" } });
    expect(input.value).toBe("Work");
    expect(onCommit).not.toHaveBeenCalled();
    act(() => finish(input));
    if (document.activeElement !== input) fireEvent.blur(input);
    expect(onCommit.mock.calls.map((call) => call[0])).toEqual(expected);
  });

  it("does not commit an unchanged value", () => {
    const onCommit = vi.fn();
    render(<DraftInput aria-label="Name" value="Home" onCommit={onCommit} />);
    const input = screen.getByRole("textbox", { name: "Name" });
    fireEvent.focus(input);
    fireEvent.blur(input);
    expect(onCommit).not.toHaveBeenCalled();
  });
});

describe("InputGroup", () => {
  it.each([
    ["default", "border-input"],
    ["ghost", "border-transparent"],
  ] as const)("renders the %s variant as a group", (variant, expected) => {
    render(
      <InputGroup variant={variant} aria-label="Search">
        <InputGroupInput aria-label="Query" />
      </InputGroup>,
    );
    const group = screen.getByRole("group", { name: "Search" });
    expect(group.getAttribute("data-slot")).toBe("input-group");
    expect(group.className).toContain(expected);
  });

  it.each(["inline-start", "inline-end", "block-start", "block-end"] as const)(
    "focuses the input when the %s addon is pressed",
    (align) => {
      render(
        <InputGroup>
          <InputGroupInput aria-label="Query" />
          <InputGroupAddon align={align}>
            <span>Addon</span>
          </InputGroupAddon>
        </InputGroup>,
      );
      const addon = screen.getByText("Addon").parentElement!;
      expect(addon.getAttribute("data-align")).toBe(align);
      fireEvent.mouseDown(screen.getByText("Addon"));
      expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Query" }));
    },
  );

  it("leaves interactive addon content in control of focus", () => {
    render(
      <InputGroup>
        <InputGroupInput aria-label="Query" />
        <InputGroupAddon align="inline-end">
          <button type="button">Clear</button>
        </InputGroupAddon>
      </InputGroup>,
    );
    fireEvent.mouseDown(screen.getByRole("button", { name: "Clear" }));
    expect(document.activeElement).not.toBe(screen.getByRole("textbox", { name: "Query" }));
  });
});

describe("MiddleTruncate", () => {
  it.each([
    ["fix/cache-main-20260918-180825", undefined, { head: "fix/cache-main-20260", tail: "918-180825" }],
    ["src/components/ui/wizard.tsx", undefined, { head: "src/components/ui/", tail: "wizard.tsx" }],
    ["a/xyz", undefined, null],
    ["abcdefghijklmnopqrstuvwxyz", undefined, { head: "abcdefghijklmnop", tail: "qrstuvwxyz" }],
    ["abcdefghij", 3, { head: "abcdefg", tail: "hij" }],
    ["short", undefined, null],
    ["abcdefghij", 0, null],
  ] as const)("splits %s (tail %s)", (value, tail, expected) => {
    expect(splitForMiddleTruncate(value, tail)).toEqual(
      expected === null ? null : { head: expected.head, tail: expected.tail },
    );
  });

  it("keeps the full value as title and text", () => {
    const value = "src/components/ui/middle-truncate.tsx";
    const { container, rerender } = render(<MiddleTruncate value={value} />);
    const root = container.firstElementChild!;
    expect(root.getAttribute("title")).toBe(value);
    expect(root.textContent).toBe(value);
    expect(root.children).toHaveLength(2);
    rerender(<MiddleTruncate value={value} showTitle={false} />);
    expect(container.firstElementChild!.hasAttribute("title")).toBe(false);
  });
});

describe("NumberField", () => {
  it.each(["sm", "default", "lg"] as const)("renders the %s size", (size) => {
    const { container } = render(
      <NumberField size={size} defaultValue={3}>
        <NumberFieldGroup>
          <NumberFieldDecrement aria-label="Decrease" />
          <NumberFieldInput aria-label="Count" />
          <NumberFieldIncrement aria-label="Increase" />
        </NumberFieldGroup>
      </NumberField>,
    );
    expect(container.querySelector("[data-slot=number-field]")?.getAttribute("data-size")).toBe(
      size,
    );
  });

  it("steps with the buttons and the arrow keys", () => {
    render(
      <NumberField defaultValue={3} min={0} max={5}>
        <NumberFieldGroup>
          <NumberFieldDecrement aria-label="Decrease" />
          <NumberFieldInput aria-label="Count" />
          <NumberFieldIncrement aria-label="Increase" />
        </NumberFieldGroup>
      </NumberField>,
    );
    const input = screen.getByRole("textbox", { name: "Count" }) as HTMLInputElement;
    expect(input.value).toBe("3");
    fireEvent.click(screen.getByRole("button", { name: "Increase" }));
    expect(input.value).toBe("4");
    fireEvent.click(screen.getByRole("button", { name: "Decrease" }));
    expect(input.value).toBe("3");
    act(() => input.focus());
    fireEvent.keyDown(input, { key: "ArrowUp" });
    expect(input.value).toBe("4");
    fireEvent.keyDown(input, { key: "End" });
    expect(input.value).toBe("5");
  });
});

describe("QRCodeSvg", () => {
  it.each([
    ["L", 128],
    ["M", 96],
    ["Q", 64],
    ["H", 200],
  ] as const)("renders an accessible %s-level code at %ipx", (level, size) => {
    const { container } = render(
      <QRCodeSvg value="https://example.com/pair" level={level} size={size} title="Pair" />,
    );
    const svg = screen.getByRole("img", { name: "Pair" });
    expect(svg.getAttribute("width")).toBe(String(size));
    expect(container.querySelector("path")?.getAttribute("d")).toMatch(/^M/);
  });

  it("adds the margin to the view box and stays decorative without a title", () => {
    const { container } = render(<QRCodeSvg value="detent" marginSize={4} />);
    const svg = container.querySelector("svg")!;
    expect(svg.hasAttribute("role")).toBe(false);
    const [, , width] = svg.getAttribute("viewBox")!.split(" ").map(Number);
    expect(width).toBe(21 + 8);
  });
});

describe("RadioGroup", () => {
  it("checks a radio on click and moves with the arrow keys", () => {
    // Base UI's radio re-dispatches clicks as PointerEvents, which jsdom lacks.
    vi.stubGlobal("PointerEvent", MouseEvent);
    const onValueChange = vi.fn();
    render(
      <RadioGroup aria-label="Effort" defaultValue="low" onValueChange={onValueChange}>
        <label>
          <Radio value="low" /> Low
        </label>
        <label>
          <Radio value="high" /> High
        </label>
        <label>
          <Radio value="max" disabled /> Max
        </label>
      </RadioGroup>,
    );
    const group = screen.getByRole("radiogroup", { name: "Effort" });
    expect(group.getAttribute("data-slot")).toBe("radio-group");
    const [low, high, max] = within(group).getAllByRole("radio");
    expect(low!.getAttribute("aria-checked")).toBe("true");
    expect(max!.getAttribute("aria-disabled")).toBe("true");

    fireEvent.click(high!);
    expect(high!.getAttribute("aria-checked")).toBe("true");
    expect(onValueChange.mock.calls.at(-1)?.[0]).toBe("high");

    act(() => high!.focus());
    fireEvent.keyDown(high!, { key: "ArrowUp" });
    // Roving tabindex: the arrow key hands the tab stop to the previous radio.
    expect(low!.getAttribute("tabindex")).toBe("0");
    expect(high!.getAttribute("tabindex")).toBe("-1");
    vi.unstubAllGlobals();
  });
});

describe("StandalonePage", () => {
  it.each(["pairing", "error", "brand"] as const)("renders the %s tone", (tone) => {
    const { container } = render(
      <StandalonePage tone={tone} masthead={<div>Masthead</div>}>
        <StandalonePageHeader eyebrow="Detent" title="Pair this device" description="Scan it." />
      </StandalonePage>,
    );
    expect(screen.getByRole("heading", { level: 1, name: "Pair this device" })).toBeTruthy();
    expect(screen.getByText("Masthead")).toBeTruthy();
    const section = container.querySelector("section")!;
    expect(section.className).toContain(tone === "brand" ? "bg-card/94" : "bg-card/90");
    const sky = container.querySelector("[class*='--color-sky-500']");
    expect(sky !== null).toBe(tone === "pairing");
  });
});

describe("Wizard", () => {
  it.each([
    ["default", "max-w-xl"],
    ["wide", "max-w-3xl"],
  ] as const)("renders a %s wizard dialog", (size, expected) => {
    render(
      <Dialog open>
        <WizardPopup size={size}>
          <WizardHeader title="Add provider" description="Connect an account." />
          <WizardPanel>
            <p>Panel body</p>
          </WizardPanel>
          <WizardFooter leading={<span>Back</span>}>
            <button type="button">Next</button>
          </WizardFooter>
        </WizardPopup>
      </Dialog>,
    );
    const dialog = screen.getByRole("dialog", { name: "Add provider" });
    expect(dialog.className).toContain(expected);
    expect(within(dialog).getByText("Connect an account.")).toBeTruthy();
    expect(dialog.querySelector("[data-slot=animated-height]")).not.toBeNull();
    expect(within(dialog).getByText("Panel body")).toBeTruthy();
  });

  it("keeps the title accessible when branding replaces it", () => {
    render(
      <Dialog open>
        <WizardPopup>
          <WizardHeader title="Add Codex" identity={<span>Codex logo</span>} />
        </WizardPopup>
      </Dialog>,
    );
    expect(screen.getByRole("dialog", { name: "Add Codex" })).toBeTruthy();
    expect(screen.getByText("Add Codex").className).toContain("sr-only");
  });

  it("marks the current step and navigates between enabled steps", () => {
    const onStepChange = vi.fn();
    render(
      <WizardSteps
        steps={["Provider", "Account", "Review"]}
        currentStep={1}
        summaries={["Codex", null, null]}
        showSummaries
        onStepChange={onStepChange}
        isStepDisabled={(step) => step === 2}
      />,
    );
    const list = screen.getByRole("list", { name: "Setup progress" });
    const [provider, account, review] = within(list).getAllByRole("button");
    expect(provider!.getAttribute("aria-label")).toBe("Provider, step 1, Codex");
    expect(provider!.textContent).toContain("Provider: Codex");
    expect(account!.getAttribute("aria-current")).toBe("step");
    expect((review as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(provider!);
    expect(onStepChange).toHaveBeenCalledWith(0);
  });

  it("renders read-only steps without buttons", () => {
    render(<WizardSteps steps={["One", "Two"]} currentStep={0} />);
    const list = screen.getByRole("list", { name: "Setup progress" });
    expect(within(list).queryAllByRole("button")).toHaveLength(0);
    expect(within(list).getAllByRole("listitem")).toHaveLength(2);
  });
});
