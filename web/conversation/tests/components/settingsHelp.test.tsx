// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ProjectSettingsView, draftOf } from "../../src/app/account/ProjectSettings.tsx";
import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import { SettingsHelp } from "../../src/app/settings/SettingsHelp.tsx";
import type { FleetResponse, ProjectIntegration, RunnerRouting, PolicyApproval } from "../../src/contracts/account.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import policyFixture from "../../src/contracts/fixtures/account-policy.json";
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
  it.each(["conflict", "historical", "resolved"] as const)("shows shared policy %s without another approval action", async (state) => {
    const policy: PolicyApproval = { ...policyFixture, policy: { ...policyFixture.policy, configuration: { behavior: {}, prompt: "Run the work." } } };
    renderProject(true, { policy, observedPolicies: state === "resolved" ? [] : [{
      policy: { ...policy.policy, policy_id: "policy_old" }, runner_id: "runner_air", runner_ids: ["runner_air", "runner_pro"], observed_at: "2026-10-05T17:00:00Z",
      conflict: state === "conflict", previously_approved: state === "historical",
    }] });
    await screen.findByText("Shared project configuration");
    expect(screen.queryByRole("button", { name: /Approve updated policy/ })).toBeNull();
    if (state === "resolved") {
      expect(screen.queryByText("Runners have conflicting project configurations.")).toBeNull();
      expect(screen.getByText("Authorized runners consume this approved project configuration.")).toBeTruthy();
    } else {
      expect(screen.getByText("Runners have conflicting project configurations.")).toBeTruthy();
      expect(screen.getByText("Review policy from runner_air, runner_pro")).toBeTruthy();
      expect(screen.getByText(/Load the approved shared configuration on the reported runners/)).toBeTruthy();
    }
  });

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
    { name: "installed binding", installed: true, repository: "acme/orders", checkout: "", profile: "native", transport: true, visible: true },
    { name: "not installed binding", installed: false, repository: "acme/orders", checkout: "", profile: "native", transport: true, visible: true },
    { name: "runner checkout", installed: false, repository: "", checkout: "acme/checkout", profile: "native", transport: true, visible: true },
    { name: "checkout takes precedence", installed: true, repository: "acme/orders", checkout: "acme/checkout", profile: "native", transport: true, visible: true },
    { name: "unbound project", installed: false, repository: "", checkout: "", profile: "native", transport: true, visible: false },
    { name: "compatibility project", installed: true, repository: "acme/orders", checkout: "", profile: "github_compatible", transport: true, visible: false },
    { name: "unavailable transport", installed: false, repository: "acme/orders", checkout: "", profile: "native", transport: false, visible: false },
    { name: "unreported installation", installed: undefined, repository: "acme/orders", checkout: "", profile: "native", transport: true, visible: false },
  ])("shows GitHub App installation for $name to a reader", async (state) => {
    const url = "https://github.com/apps/detent-cloud/installations/new";
    renderProject(false, { integration: {
      ...integration,
      profile: state.profile,
      repository: state.repository,
      checkout_repository: state.checkout,
      github_transport_available: state.transport,
      github_app_slug: "detent-cloud",
      github_app_install_url: url,
      github_app_installed: state.installed,
    } });
    await screen.findByRole("heading", { name: "Example settings" });
    if (state.visible) {
      expect(screen.getByRole("heading", { name: "GitHub App" })).toBeTruthy();
      expect(screen.getByText(`Detent Cloud is ${state.installed ? "installed" : "not installed"} on ${state.checkout || state.repository}`, { exact: true })).toBeTruthy();
      const link = screen.getByRole("link", { name: state.installed ? "Manage installation" : "Install the Detent Cloud GitHub App" });
      expect(link.getAttribute("href")).toBe(url);
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    } else {
      expect(screen.queryByRole("heading", { name: "GitHub App" })).toBeNull();
      expect(screen.queryByText(/Detent Cloud is (not )?installed on/)).toBeNull();
      expect(screen.queryByRole("link", { name: /installation|Install the Detent Cloud GitHub App/ })).toBeNull();
    }
  });

  it.each([
    [true, "approved"], [false, "approved"],
    [true, "missing"], [false, "missing"],
    [true, "pending"], [false, "pending"],
  ] as const)("keeps policy state visible and technical details closed (%s, %s)", async (canManage, state) => {
    const policy = policyFixture as PolicyApproval;
    const onApprovePolicy = vi.fn();
    renderProject(canManage, {
      policy: state === "missing" ? null : policy,
      observedPolicies: state === "pending" ? [{ policy: { ...policy.policy, policy_id: "pol_updated" }, runner_id: "runner_example", observed_at: "2026-10-05T00:00:00Z" }] : [],
      onApprovePolicy,
    });
    expect(await screen.findByText(state === "pending" ? "Needs approval" : state === "missing" ? "Not approved" : "Approved")).toBeTruthy();
    const ownership = screen.getByText("Field ownership").closest("details")!;
    expect(ownership.open).toBe(false);
    expect(ownership.textContent).toContain(Object.values(integration.authority!)[0]);
    if (state !== "missing") {
      const approval = screen.getByText("Approval details").closest("details")!;
      expect(approval.open).toBe(false);
      expect(approval.textContent).toContain(policy.approved_by);
    }
    if (state === "pending") {
      const review = screen.getByText("Review policy from runner_example").closest("details")!;
      expect(review.open).toBe(false);
      const user = userEvent.setup();
      await user.click(screen.getByText("Review policy from runner_example"));
      expect(review.open).toBe(true);
      expect(review.textContent).toContain(JSON.stringify({ ...policy.policy, policy_id: "pol_updated" }, null, 2));
      if (canManage) {
        await user.click(screen.getByRole("button", { name: "Approve updated policy pol_updated" }));
        expect(onApprovePolicy).toHaveBeenCalledWith("pol_updated");
      } else {
        expect(screen.queryByRole("button", { name: /Approve/ })).toBeNull();
        expect(screen.getByText(/An owner or admin must approve the reported policy/)).toBeTruthy();
      }
    }
  });

  it.each([
    ["Repository and pull request integration", /Disabling it.*immutable repository binding/],
    ["Intake", /New GitHub issues enter Triage automatically.*App is installed.*existing issues.*manually/],
    ["Intake", /Compatibility projects import selected GitHub issues.*manual intake.*Disabled prevents new manual imports/, "github_compatible"],
    ["Projection", /from Detent to the linked GitHub issue.*Disabled stops new summary writes/],
    ["Authority", /native profile.*Detent ownership.*github_compatible profile.*GitHub ownership/],
    ["Repository policy", /runner upgrade.*stale.*Execution is blocked/],
    ["Runner routing", /matching a tag never grants project access/],
  ])("explains %s even for a reader", async (label, copy, profile = "native") => {
    const user = userEvent.setup();
    renderProject(false, { integration: { ...integration, profile } });
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
      project_ids: ["prj_example"], isolation_tier: "sandbox", host_services: [],
      availability: { timezone: "", windows: [], hard_deadline: "" },
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
