// @vitest-environment jsdom
//
// Render, variant, and keyboard/ARIA coverage for the entry-surface primitives
// in src/components/ui.
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { Dialog } from "../../../src/components/ui/dialog.tsx";
import { QRCodeSvg } from "../../../src/components/ui/qr-code.tsx";
import { StandalonePage, StandalonePageHeader } from "../../../src/components/ui/standalone-page.tsx";
import { WizardFooter, WizardHeader, WizardPanel, WizardPopup, WizardSteps } from "../../../src/components/ui/wizard.tsx";

afterEach(() => {
  cleanup();
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
