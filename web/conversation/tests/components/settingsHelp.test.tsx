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

function renderProject(canManage = true) {
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
      />
    ),
  });
  const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ["/"] }) });
  render(<RouterProvider router={router} />);
}

describe("settings help interactions", () => {
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
  it.each([
    ["Shared host capacity", /6 slots.*other runners use 4.*only 2 remain/],
    ["Runner limit and reported capacity", /smaller of reported capacity.*saved limit of 6.*at most 4 jobs/],
    ["codex provider capacity", /compatible with its provider and model.*2 free provider slots/],
    ["codex account capacity", /runner and host room/],
  ])("explains %s on the fleet readout", async (label, copy) => {
    const user = userEvent.setup();
    render(<RunnersSectionView fleet={{ ...fleet, runners: [fleet.runners[0]!] }} />);
    await user.click(screen.getByRole("button", { name: `About ${label}` }));
    expect((await screen.findByRole("dialog", { name: label })).textContent).toMatch(copy);
  });

  it("explains editable routing and capacity without changing or submitting the form", async () => {
    const user = userEvent.setup();
    const save = vi.fn();
    const routing: RunnerRouting = {
      display_name: "Example runner", tags: ["linux"], state: "active", capacity_limit: 6,
      project_ids: ["prj_example"], home_project_ids: [], isolation_tier: "sandbox", host_services: [],
      availability: { timezone: "", windows: [], hard_deadline: "" },
      spillover: { mode: "never", after_minutes: 0 },
    };
    render(<RunnersSectionView fleet={{ ...fleet, runners: [{ ...fleet.runners[0]!, routing }] }} onSaveRouting={save} />);
    await user.click(screen.getByText("Edit runner settings"));
    for (const [label, copy] of [
      ["Tags", /do not grant access to a project/],
      ["Authorized project IDs", /authorized to work.*does not grant project access/],
      ["Runner capacity limit", /limit of 6 allows up to 6 jobs.*reports 4.*limit is 4.*0 pauses new jobs/],
    ] as const) {
      await user.click(screen.getByRole("button", { name: `About ${label}` }));
      expect((await screen.findByRole("dialog", { name: label })).textContent).toMatch(copy);
      await user.keyboard("{Escape}");
    }
    expect((screen.getByRole("spinbutton", { name: "Runner capacity limit" }) as HTMLInputElement).value).toBe("6");
    expect((screen.getByRole("textbox", { name: "Tags" }) as HTMLInputElement).value).toBe("linux");
    expect(save).not.toHaveBeenCalled();
  });
});
