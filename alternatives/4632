// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Switch } from "../../../src/components/ui/switch";

// jsdom has no PointerEvent; the Base UI switch dispatches one on click.
class PointerEventShim extends MouseEvent {
  readonly pointerId: number;
  constructor(type: string, init: PointerEventInit = {}) {
    super(type, init);
    this.pointerId = init.pointerId ?? 0;
  }
}

beforeEach(() => {
  vi.stubGlobal("PointerEvent", PointerEventShim);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function ControlledSwitch({
  initialChecked,
  initialMixed = false,
}: {
  initialChecked: boolean;
  initialMixed?: boolean;
}) {
  const [checked, setChecked] = useState(initialChecked);
  const [mixed, setMixed] = useState(initialMixed);

  return (
    <Switch
      aria-label="Enable notifications"
      checked={checked}
      mixed={mixed}
      onCheckedChange={(nextChecked) => {
        setMixed(false);
        setChecked(nextChecked);
      }}
    />
  );
}

describe("Switch accessibility", () => {
  it.each([
    { initialChecked: false, initialMixed: false, before: "false", after: true },
    { initialChecked: true, initialMixed: false, before: "true", after: false },
    { initialChecked: false, initialMixed: true, before: "mixed", after: true },
  ])(
    "exposes $before before activation and $after after activation",
    async ({ initialChecked, initialMixed, before, after }) => {
      const { container } = render(
        <ControlledSwitch initialChecked={initialChecked} initialMixed={initialMixed} />,
      );
      const control = screen.getByRole("switch", { name: "Enable notifications" });
      expect(control.getAttribute("aria-checked")).toBe(before);

      await userEvent.click(control);

      expect(control.getAttribute("aria-checked")).toBe(String(after));
      expect(container.querySelector<HTMLInputElement>('input[type="checkbox"]')?.checked).toBe(
        after,
      );
    },
  );
});

describe("Switch mixed state", () => {
  it.each([
    { mixed: true, dataMixed: "", thumbCentred: true },
    { mixed: false, dataMixed: null, thumbCentred: false },
  ])("mixed=$mixed marks the root and centres the thumb", ({ mixed, dataMixed, thumbCentred }) => {
    render(<Switch aria-label="Toggle" defaultChecked mixed={mixed} />);
    const control = screen.getByRole("switch", { name: "Toggle" });
    expect(control.getAttribute("data-mixed")).toBe(dataMixed);
    const thumb = control.querySelector('[data-slot="switch-thumb"]');
    expect(thumb?.className.includes("opacity-70")).toBe(thumbCentred);
  });
});
