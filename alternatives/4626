// @vitest-environment jsdom
//
// Render, variant, and keyboard/ARIA coverage for the layout and data-display primitives
// in src/components/ui.
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { CollapsibleSectionHeader, SectionHeaderStatus } from "../../../src/components/ui/collapsible-section-header.tsx";
import { DiscoveryList, DiscoveryListRow } from "../../../src/components/ui/discovery-list.tsx";
import { MiddleTruncate, splitForMiddleTruncate } from "../../../src/components/ui/middle-truncate.tsx";

afterEach(() => {
  cleanup();
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
