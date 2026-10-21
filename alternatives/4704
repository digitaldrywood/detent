// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Sidebar, SidebarProvider, SidebarRail } from "../../../src/components/ui/sidebar";

// jsdom has no PointerEvent; the rail reads pointerId, button and clientX from it.
class PointerEventShim extends MouseEvent {
  readonly pointerId: number;
  constructor(type: string, init: PointerEventInit = {}) {
    super(type, init);
    this.pointerId = init.pointerId ?? 0;
  }
}

const STORAGE_KEY = "sidebar-rail-test-width";

beforeEach(() => {
  vi.stubGlobal("PointerEvent", PointerEventShim);
  const matches = Element.prototype.matches;
  vi.spyOn(Element.prototype, "matches").mockImplementation(function (this: Element, selector) {
    return selector === ":fullscreen" ? false : matches.call(this, selector);
  });
  const captured = new Set<number>();
  Object.assign(HTMLElement.prototype, {
    setPointerCapture: (pointerId: number) => captured.add(pointerId),
    hasPointerCapture: (pointerId: number) => captured.has(pointerId),
    releasePointerCapture: (pointerId: number) => captured.delete(pointerId),
  });
  window.localStorage.removeItem(STORAGE_KEY);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.localStorage.removeItem(STORAGE_KEY);
  document.body.style.removeProperty("cursor");
  document.body.style.removeProperty("user-select");
});

function renderSidebar(options: {
  side: "left" | "right";
  onResize: (width: number) => void;
  shouldAcceptWidth?: () => boolean;
}) {
  const { container } = render(
    <SidebarProvider defaultOpen>
      <Sidebar
        side={options.side}
        resizable={{
          minWidth: 200,
          maxWidth: 400,
          storageKey: STORAGE_KEY,
          onResize: options.onResize,
          ...(options.shouldAcceptWidth ? { shouldAcceptWidth: options.shouldAcceptWidth } : {}),
        }}
      >
        <SidebarRail />
      </Sidebar>
    </SidebarProvider>,
  );
  const wrapper = container.querySelector<HTMLElement>("[data-slot='sidebar-wrapper']");
  if (!wrapper) throw new Error("sidebar wrapper was not rendered");
  return { rail: screen.getByRole("button", { name: "Resize Sidebar" }), wrapper };
}

describe("SidebarRail drag-to-resize", () => {
  it.each([
    { name: "left sidebar widens when dragged right", side: "left" as const, toX: 160, accept: true, expected: 260 },
    { name: "right sidebar widens when dragged left", side: "right" as const, toX: 40, accept: true, expected: 260 },
    { name: "width clamps to the maximum", side: "left" as const, toX: 900, accept: true, expected: 400 },
    { name: "width clamps to the minimum", side: "left" as const, toX: -500, accept: true, expected: 200 },
    { name: "a rejected width keeps the start width", side: "left" as const, toX: 160, accept: false, expected: 200 },
  ])("$name", ({ side, toX, accept, expected }) => {
    const onResize = vi.fn();
    const { rail, wrapper } = renderSidebar({
      side,
      onResize,
      shouldAcceptWidth: () => accept,
    });

    fireEvent.pointerDown(rail, { button: 0, pointerId: 1, clientX: 100 });
    expect(document.body.style.cursor).toBe("col-resize");
    fireEvent.pointerMove(rail, { pointerId: 1, clientX: toX });
    fireEvent.pointerUp(rail, { pointerId: 1, clientX: toX });

    expect(wrapper.style.getPropertyValue("--sidebar-width")).toBe(`${expected}px`);
    expect(onResize).toHaveBeenLastCalledWith(expected);
    expect(window.localStorage.getItem(STORAGE_KEY)).toBe(String(expected));
    expect(document.body.style.cursor).toBe("");
  });

  it("restores a persisted width before paint", () => {
    window.localStorage.setItem(STORAGE_KEY, "320");
    const onResize = vi.fn();
    const { wrapper } = renderSidebar({ side: "left", onResize });
    expect(wrapper.style.getPropertyValue("--sidebar-width")).toBe("320px");
    expect(onResize).toHaveBeenCalledWith(320);
  });

  it("a cancelled drag ignores the cancel position", () => {
    const onResize = vi.fn();
    const { rail } = renderSidebar({ side: "left", onResize });
    fireEvent.pointerDown(rail, { button: 0, pointerId: 7, clientX: 100 });
    fireEvent.pointerCancel(rail, { pointerId: 7, clientX: 300 });
    expect(window.localStorage.getItem(STORAGE_KEY)).toBe("200");
    expect(onResize).toHaveBeenLastCalledWith(200);
  });

  it("a click after a drag does not collapse the sidebar", () => {
    const { rail, wrapper } = renderSidebar({ side: "left", onResize: vi.fn() });
    fireEvent.pointerDown(rail, { button: 0, pointerId: 2, clientX: 100 });
    fireEvent.pointerMove(rail, { pointerId: 2, clientX: 150 });
    fireEvent.pointerUp(rail, { pointerId: 2, clientX: 150 });
    fireEvent.click(rail);
    expect(wrapper.getAttribute("data-sidebar-state")).not.toBe("collapsed");
  });
});
