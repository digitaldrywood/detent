// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ProjectSettingsView, draftOf } from "../../src/app/account/ProjectSettings.tsx";
import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import { SettingsHelp } from "../../src/app/settings/SettingsHelp.tsx";
import type { FleetResponse, ProjectIntegration, RunnerRouting } from "../../src/contracts/account.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import integrationFixture from "../../src/contracts/fixtures/account-integration.json";

// nwsapi in jsdom 26 recurses when Floating UI checks :fullscreen.
// jsdom has no fullscreen mode; leave all other selector behavior intact.
beforeEach(() => {
  const matches = Element.prototype.matches;
  vi.spyOn(Element.prototype, "matches").mockImplementation(function (this: Element, selector) {
    return selector === ":fullscreen" ? false : matches.call(this, selector);
  });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

const integration = integrationFixture as ProjectIntegration;
const fleet = fleetFixture as unknown as FleetResponse;

function renderProject(canManage = true, overrides: Partial<React.ComponentProps<typeof ProjectSettingsView>> = {}) {
  const root = createRootRoute({
    component: () => (
      <ProjectSettingsView
        projectName="Example"
        integration={integration}
        policy={null}
        canManage={canManage}
        draft={draftOf(integration)}
        observedPolicies={[]}
        onDraftChange={vi.fn()}
        onSave={vi.fn()}
        onApprovePolicy={vi.fn()}
        onApprovePastedPolicy={vi.fn()}
        saving={false}
        approving={false}
        saveError={null}
        approveError={null}
        onOpenFleet={vi.fn()}
        {...overrides}
      />
    ),
  });
  const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ["/"] }) });
  render(<RouterProvider router={router} />);
}

describe("settings help interactions", () => {
  it.each(["", "Acme/Private"])("uses runner checkout settings when Hub transport is unavailable (%s)", async (repository) => {
    const onOpenSetup = vi.fn();
    renderProject(true, {
      integration: { ...integration, repository: "", github_transport_available: false, checkout_repository: repository },
      onOpenSetup,
    });
    expect((await screen.findByText(/Hub GitHub integration is unavailable/)).textContent).toContain("runner’s approved repository policy");
    expect(screen.queryByRole("switch", { name: "Repository and pull request integration" })).toBeNull();
    expect(screen.queryByLabelText("Intake", { exact: true })).toBeNull();
    expect(screen.queryByLabelText("Projection", { exact: true })).toBeNull();
    if (repository) {
      expect(screen.getByText(repository)).toBeTruthy();
    } else {
      await userEvent.setup().click(screen.getByRole("button", { name: "Associate runner checkout" }));
      expect(onOpenSetup).toHaveBeenCalledOnce();
    }
  });

  it("opens by keyboard, describes the dialog, dismisses with Escape and restores focus", async () => {
    const user = userEvent.setup();
    render(<SettingsHelp label="Example setting">A useful explanation.</SettingsHelp>);
    const trigger = screen.getByRole("button", { name: "About Example setting" });
    await user.tab();
    expect(document.activeElement).toBe(trigger);
    await user.keyboard("{Enter}");
    const dialog = await screen.findByRole("dialog", { name: "Example setting" });
    expect(dialog.getAttribute("aria-describedby")).toBeTruthy();
    expect(document.getElementById(dialog.getAttribute("aria-describedby")!)?.textContent).toBe("A useful explanation.");
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });

  it("opens on hover and supports tap-style click and outside dismissal", async () => {
    const user = userEvent.setup();
    render(<><SettingsHelp label="Example setting">A useful explanation.</SettingsHelp><button>Outside</button></>);
    const trigger = screen.getByRole("button", { name: "About Example setting" });
    await user.hover(trigger);
    expect(await screen.findByRole("dialog", { name: "Example setting" })).toBeTruthy();
    await user.unhover(trigger);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await user.click(trigger);
    expect(await screen.findByRole("dialog", { name: "Example setting" })).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Outside" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});

describe("project settings help", () => {
  it.each([
    ["Repository and pull request integration", /Disabling it.*immutable repository binding/],
    ["Intake", /Manual intake.*when you request it.*Disabled prevents new manual imports/],
    ["Projection", /from Detent to the linked GitHub issue.*Disabled stops new summary writes/],
    ["Authority", /native profile.*Detent ownership.*github_compatible profile.*GitHub ownership/],
    ["Repository policy", /runner upgrade.*stale.*Execution is blocked/],
    ["Runner routing", /matching a tag never grants project access/],
  ])("explains %s even for a reader", async (label, copy) => {
    const user = userEvent.setup();
    renderProject(false);
    await user.click(await screen.findByRole("button", { name: `About ${label}` }));
    expect((await screen.findByRole("dialog", { name: label })).textContent).toMatch(copy);
    expect(screen.queryByRole("button", { name: "About Repository" })).toBeNull();
  });
});

describe("runner settings help", () => {
  it("explains capacity and tag routing inline without submitting", async () => {
    const user = userEvent.setup();
    const save = vi.fn();
    const routing: RunnerRouting = {
      display_name: "Example runner", tags: ["linux"], state: "active", capacity_limit: 6,
      project_ids: ["prj_example"], home_project_ids: [], isolation_tier: "sandbox", host_services: [],
      availability: { timezone: "", windows: [], hard_deadline: "" },
      spillover: { mode: "never", after_minutes: 0 },
    };
    render(<RunnersSectionView fleet={{ ...fleet, editable: true, runners: [{ ...fleet.runners[0]!, routing }] }} onSaveRouting={save} />);
    await user.click(screen.getByRole("button", { name: `Manage ${fleet.runners[0]!.display_name}` }));
    const sheet = screen.getByRole("dialog");
    expect(sheet.textContent).toContain("The lower of the two wins");
    expect(sheet.textContent).toContain("Tags pick from allowed runners; they never grant a project.");
    expect((screen.getByRole("spinbutton", { name: "Jobs at once" }) as HTMLInputElement).value).toBe("6");
    expect(screen.getByRole("button", { name: "Remove tag linux" })).toBeTruthy();
    expect(save).not.toHaveBeenCalled();
  });
});
