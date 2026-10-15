// @vitest-environment jsdom
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  Tooltip,
  TooltipPopup,
  TooltipScrollDismissArea,
  TooltipTrigger,
} from "../../../src/components/ui/tooltip";

// nwsapi in jsdom recurses when Floating UI checks :fullscreen.
beforeEach(() => {
  const matches = Element.prototype.matches;
  vi.spyOn(Element.prototype, "matches").mockImplementation(function (this: Element, selector) {
    return selector === ":fullscreen" ? false : matches.call(this, selector);
  });
  vi.useFakeTimers();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

function openTooltipText(): string | null {
  return document.querySelector('[data-slot="tooltip-popup"][data-open]')?.textContent ?? null;
}

describe("TooltipScrollDismissArea", () => {
  it.each([
    { interaction: "hover", openBefore: true, openAfter: false },
    { interaction: "delayed hover", openBefore: false, openAfter: false },
    { interaction: "focus", openBefore: true, openAfter: true },
    { interaction: "hover then focus", openBefore: true, openAfter: true },
    { interaction: "outside area", openBefore: true, openAfter: true },
    { interaction: "wheel without scroll", openBefore: true, openAfter: true },
  ])("handles $interaction", async ({ interaction, openBefore, openAfter }) => {
    const onMouseEnter = vi.fn();
    const tooltip = (
      <Tooltip>
        <TooltipTrigger delay={50} onMouseEnter={onMouseEnter}>
          message link
        </TooltipTrigger>
        <TooltipPopup>https://example.com</TooltipPopup>
      </Tooltip>
    );
    render(
      <>
        <TooltipScrollDismissArea>
          <div data-testid="scrollable">{interaction === "outside area" ? null : tooltip}</div>
        </TooltipScrollDismissArea>
        {interaction === "outside area" ? tooltip : null}
      </>,
    );
    const trigger = screen.getByRole("button", { name: "message link" });
    const scrollable = screen.getByTestId("scrollable");

    await act(async () => {
      if (interaction !== "focus") {
        trigger.dispatchEvent(new MouseEvent("mouseover", { bubbles: true }));
        trigger.dispatchEvent(new MouseEvent("mouseenter"));
        trigger.dispatchEvent(new MouseEvent("mousemove", { bubbles: true }));
      }
      if (interaction === "focus" || interaction === "hover then focus") {
        document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab" }));
        trigger.focus();
      }
      if (interaction !== "delayed hover") {
        await vi.advanceTimersByTimeAsync(60);
      }
    });
    expect(onMouseEnter).toHaveBeenCalledTimes(interaction === "focus" ? 0 : 1);
    expect(openTooltipText()).toBe(openBefore ? "https://example.com" : null);

    await act(async () => {
      scrollable.dispatchEvent(
        interaction === "wheel without scroll"
          ? new WheelEvent("wheel", { bubbles: true, deltaY: 100 })
          : new Event("scroll"),
      );
      await vi.advanceTimersByTimeAsync(100);
    });
    expect(openTooltipText()).toBe(openAfter ? "https://example.com" : null);
    if (interaction === "focus" || interaction === "hover then focus") {
      expect(document.activeElement).toBe(trigger);
    }
  });
});

describe("TooltipPopup variants", () => {
  it.each([
    { variant: "default" as const, expected: ["max-w-80", "whitespace-normal"], absent: "font-mono" },
    { variant: "code" as const, expected: ["max-w-120", "font-mono"], absent: "max-w-80" },
    { variant: "glass" as const, expected: ["dropdown-glass", "max-w-80"], absent: "font-mono" },
  ])("$variant applies its wrap width", ({ variant, expected, absent }) => {
    render(
      <Tooltip defaultOpen>
        <TooltipTrigger>trigger</TooltipTrigger>
        <TooltipPopup variant={variant}>content</TooltipPopup>
      </Tooltip>,
    );
    const popup = document.querySelector('[data-slot="tooltip-popup"]');
    for (const token of expected) expect(popup?.className.split(" ")).toContain(token);
    expect(popup?.className.split(" ")).not.toContain(absent);
  });
});
