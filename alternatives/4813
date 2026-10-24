// @vitest-environment jsdom
//
// Render, variant, and keyboard/ARIA coverage for the form primitives
// in src/components/ui.
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Calendar } from "../../../src/components/ui/calendar.tsx";
import { ColorHueSlider, ColorSaturationValuePlane } from "../../../src/components/ui/color-picker.tsx";
import { DraftInput } from "../../../src/components/ui/draft-input.tsx";
import { InputGroup, InputGroupAddon, InputGroupInput } from "../../../src/components/ui/input-group.tsx";
import { NumberField, NumberFieldDecrement, NumberFieldGroup, NumberFieldIncrement, NumberFieldInput } from "../../../src/components/ui/number-field.tsx";
import { Radio, RadioGroup } from "../../../src/components/ui/radio-group.tsx";
import type { HsvColor } from "../../../src/lib/color.ts";

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
