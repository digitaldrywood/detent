// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ProjectSettingsView, draftOf, draftChanged } from "../../src/app/account/ProjectSettings.tsx";
import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import { SettingsHelp } from "../../src/app/settings/SettingsHelp.tsx";
import type { FleetResponse, ProjectIntegration, RunnerRouting, PolicyApproval } from "../../src/contracts/account.ts";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";
import policyFixture from "../../src/contracts/fixtures/account-policy.json";
import integrationFixture from "../../src/contracts/fixtures/account-integration.json";

// nwsapi in jsdom 26 recurses when Floating UI checks :fullscreen.
// jsdom has no fullscreen mode; leave all other selector behavior intact.
beforeEach(() => {
  vi.stubGlobal("PointerEvent", MouseEvent);
  const matches = Element.prototype.matches;
  vi.spyOn(Element.prototype, "matches").mockImplementation(function (this: Element, selector) {
    return selector === ":fullscreen" ? false : matches.call(this, selector);
  });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

const integration = integrationFixture as ProjectIntegration;
const fleet = fleetFixture as unknown as FleetResponse;

function renderProject(canManage = true, overrides: Partial<React.ComponentProps<typeof ProjectSettingsView>> = {}) {
  const root = createRootRoute({
    component: () => (
      <ProjectSettingsView
        tab="repository"
        integration={integration}
        policy={null}
        canManage={canManage}
        draft={draftOf(integration)}
        observedPolicies={[]}
        onDraftChange={vi.fn()}
        onSave={vi.fn()}
        saving={false}
        saveError={null}
        onOpenFleet={vi.fn()}
        {...overrides}
      />
    ),
  });
  const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ["/"] }) });
  render(<RouterProvider router={router} />);
}

describe("settings help interactions", () => {
  it.each([true, false])("edits duplicate checking with manage permission=%s", async (canManage) => {
    const onDraftChange = vi.fn();
    renderProject(canManage, { tab: "general", onDraftChange });
    const toggle = await screen.findByRole("switch", { name: "Check for duplicate issues" });
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    await userEvent.setup().click(toggle);
    if (canManage) {
      const next = { ...draftOf(integration), duplicateChallenge: false };
      expect(onDraftChange).toHaveBeenCalledWith(next);
      expect(draftChanged(next, draftOf(integration))).toBe(true);
    } else {
      expect(onDraftChange).not.toHaveBeenCalled();
    }
  });

  it("edits the archive periods and preserves Never", async () => {
    const user = userEvent.setup();
    const onDraftChange = vi.fn();
    renderProject(true, { tab: "general", onDraftChange });
    const completed = await screen.findByRole("combobox", { name: "Archive completed issues" });
    const cancelled = screen.getByRole("combobox", { name: "Archive cancelled issues" });
    expect((completed as HTMLSelectElement).value).toBe("30");
    expect((cancelled as HTMLSelectElement).value).toBe("7");
    await user.selectOptions(completed, "14");
    expect(onDraftChange).toHaveBeenLastCalledWith({ ...draftOf(integration), archiveCompletedAfterDays: 14 });
    await user.selectOptions(cancelled, "never");
    const never = { ...draftOf(integration), archiveCancelledAfterDays: null };
    expect(onDraftChange).toHaveBeenLastCalledWith(never);
    expect(draftChanged(never, draftOf(integration))).toBe(true);
    expect(draftOf({ ...integration, archive_cancelled_after_days: null }).archiveCancelledAfterDays).toBeNull();
  });

  it.each([false, true])("disables archive controls when management is unavailable or saving (%s)", async (saving) => {
    renderProject(saving, { tab: "general", saving });
    expect((await screen.findByRole("combobox", { name: "Archive completed issues" }) as HTMLSelectElement).disabled).toBe(true);
    expect((screen.getByRole("combobox", { name: "Archive cancelled issues" }) as HTMLSelectElement).disabled).toBe(true);
  });

  it.each(["conflict", "historical", "resolved"] as const)("shows shared policy %s without another approval action", async (state) => {
    const policy: PolicyApproval = { ...policyFixture, policy: { ...policyFixture.policy, configuration: { behavior: {}, prompt: "Run the work." } } };
    renderProject(true, { tab: "workflow", policy, observedPolicies: state === "resolved" ? [] : [{
      policy: { ...policy.policy, policy_id: "policy_old" }, runner_id: "runner_air", runner_ids: ["runner_air", "runner_pro"], observed_at: "2026-10-05T17:00:00Z",
      conflict: state === "conflict", previously_approved: state === "historical",
    }] });
    await screen.findByText("Reported");
    expect(screen.queryByRole("button", { name: /Approve updated policy/ })).toBeNull();
    expect(screen.queryByText("Runners have conflicting project configurations.")).toBeNull();

  });

  it.each(["", "Acme/Private"])("uses runner checkout settings when Hub transport is unavailable (%s)", async (repository) => {
    const onOpenSetup = vi.fn();
    renderProject(true, {
      integration: { ...integration, repository: "", github_transport_available: false, checkout_repository: repository },
      onOpenSetup,
    });
    expect((await screen.findByText(/Hub GitHub integration is unavailable/)).textContent).toContain("runner’s repository configuration");
    expect(screen.queryByRole("switch", { name: "Pull requests" })).toBeNull();
    expect(screen.queryByLabelText("Intake", { exact: true })).toBeNull();
    expect(screen.queryByLabelText("Projection", { exact: true })).toBeNull();
    if (repository) {
      expect(screen.getByText(repository)).toBeTruthy();
    } else {
      await userEvent.setup().click(screen.getByRole("button", { name: "Associate runner checkout" }));
      expect(onOpenSetup).toHaveBeenCalledOnce();
    }
  });

  it.each([true, false])("renders new issues with App installed=%s and an unchanged draft", async (installed) => {
    const current: ProjectIntegration = { ...integration, profile: "native", intake: "automatic", manual_import_enabled: false,
      github_app_installed: installed, github_app_install_url: "https://github.com/apps/detent-cloud/installations/new",
      projection: "summary", repository_enabled: true };
    renderProject(true, { integration: current, draft: draftOf(current), intakeHref: "/projects/prj_example/setup" });
    await screen.findByText("Automatic");
    const expected = { duplicateChallenge: true, importAllowed: false, projection: "summary", repositoryEnabled: true, archiveCompletedAfterDays: 30, archiveCancelledAfterDays: 7 };
    expect(draftOf(current)).toEqual(expected);
    expect(draftChanged(draftOf(current), expected)).toBe(false);
    expect((screen.getByRole("button", { name: "Save changes" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("combobox", { name: "Intake" })).toBeNull();
    expect((screen.getByRole("switch", { name: "Existing issues" }) as HTMLInputElement).getAttribute("aria-checked")).toBe("false");
    const row = screen.getByRole("heading", { name: "New issues" }).closest('[data-slot="settings-row"]')!;
    expect(row.textContent).toContain(installed ? "Every issue opened on example-org/example-project enters Triage." : "Waiting for the GitHub App.");
    expect(row.querySelector('span[aria-hidden="true"]')).toBeTruthy();
    expect(screen.getByRole("link", { name: installed ? "Manage installation" : "Install the Detent Cloud GitHub App" }).getAttribute("href")).toContain("github.com/apps/detent-cloud");
    expect(screen.getByRole("link", { name: "GitHub issue import in project setup" }).getAttribute("href")).toBe("/projects/prj_example/setup");
    const headings = screen.getAllByRole("heading").map((heading) => heading.textContent).filter((text) =>
      ["GitHub App", "New issues", "Existing issues", "Pull requests", "Work summaries", "Authority"].includes(text ?? ""));
    expect(headings).toEqual(["GitHub App", "New issues", "Existing issues", "Pull requests", "Work summaries", "Authority"]);
    expect(screen.queryByRole("heading", { name: "Issue flow" })).toBeNull();
  });

  it.each([
    ["Pull requests", { repositoryEnabled: false }],
    ["Work summaries", { projection: "disabled" }],
  ] as const)("updates the draft for %s while preserving manual import", async (label, update) => {
    const current = { ...integration, intake: "automatic", manual_import_enabled: true };
    const onDraftChange = vi.fn();
    renderProject(true, { integration: current, draft: draftOf(current), onDraftChange });
    await userEvent.setup().click(await screen.findByRole("switch", { name: label }));
    expect(onDraftChange).toHaveBeenCalledWith({ ...draftOf(current), ...update });
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
    await screen.findByRole("heading", { name: "Repository", level: 2 });
    if (state.visible) {
      expect(screen.getByRole("heading", { name: "GitHub App" })).toBeTruthy();
      expect(screen.getByText(state.installed ? `Installed on ${state.checkout || state.repository}.` : "Not installed. New issues will not reach this project until it is.")).toBeTruthy();
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
    { name: "native bound", profile: "native", repository: "acme/orders", checkout_repository: "", intake: "automatic", importAllowed: false, disabled: false },
    { name: "native bound imports allowed", profile: "native", repository: "acme/orders", checkout_repository: "", intake: "automatic", importAllowed: true, disabled: false },
    { name: "native checkout", profile: "native", repository: "", checkout_repository: "acme/orders", intake: "automatic", importAllowed: false, disabled: false },
    { name: "native unbound", profile: "native", repository: "", checkout_repository: "", intake: "disabled", importAllowed: false, disabled: true },
    { name: "compatibility", profile: "github_compatible", repository: "acme/orders", checkout_repository: "", intake: "manual", importAllowed: true, disabled: false },
  ])("shows only editable imports for $name", async ({ name: _name, importAllowed, disabled, ...fields }) => {
    const current = { ...integration, ...fields, manual_import_enabled: importAllowed };
    const onDraftChange = vi.fn();
    renderProject(true, { integration: current, draft: draftOf(current), onDraftChange });
    await screen.findByRole("heading", { name: "Existing issues" });
    expect(screen.queryByRole("combobox", { name: "Intake" })).toBeNull();
    const control = screen.queryByRole("switch", { name: "Existing issues" });
    if (!fields.repository && !fields.checkout_repository) {
      expect(control).toBeNull();
      expect(screen.getByText("Associate a runner checkout to configure repository integration.")).toBeTruthy();
      return;
    }
    expect(control!.getAttribute("aria-checked")).toBe(String(importAllowed));
    expect(control!.getAttribute("aria-disabled") === "true").toBe(disabled);
    if (!disabled) {
      await userEvent.setup().click(control!);
      expect(onDraftChange).toHaveBeenCalledWith({ ...draftOf(current), importAllowed: !importAllowed });
    }
    if (fields.profile === "github_compatible") {
      expect(screen.getByText("GitHub owns these issues until cutover.")).toBeTruthy();
      expect(screen.getByRole("switch", { name: "Work summaries" }).getAttribute("aria-disabled")).toBe("true");
    }
  });

  it.each([
    [true, "approved"], [false, "approved"],
    [true, "missing"], [false, "missing"],
    [true, "pending"], [false, "pending"],
  ] as const)("reports configuration without approval (%s, %s)", async (canManage, state) => {
    const policy = policyFixture as PolicyApproval;
    renderProject(canManage, {
      tab: "workflow",
      policy: state === "missing" ? null : policy,
      observedPolicies: state === "pending" ? [{ policy: { ...policy.policy, policy_id: "pol_updated" }, runner_id: "runner_example", observed_at: "2026-10-05T00:00:00Z" }] : [],
    });
    expect(await screen.findByText(state === "missing" ? "Waiting for runner" : "Reported")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Approve|Paste policy/ })).toBeNull();

  });

  it.each([
    ["Pull requests", /Disabling it.*immutable repository binding/],
    ["New issues", /receives GitHub.*App.*nothing to turn on/],
    ["Existing issues", /import older GitHub issues.*Turning this off prevents manual imports/, "github_compatible"],
    ["Work summaries", /each work item.*linked GitHub issue.*Turning this off stops new summary writes/],
    ["Authority", /native profile.*Detent ownership.*github_compatible profile.*GitHub ownership/],
    ["Runner routing", /matching a tag never grants project access/],
  ])("explains %s even for a reader", async (label, copy, profile = "native") => {
    const user = userEvent.setup();
    const onOpenFleet = vi.fn();
    renderProject(false, { tab: label === "Runner routing" ? "runners" : "repository", integration: { ...integration, profile }, onOpenFleet });
    await user.click(await screen.findByRole("button", { name: `About ${label}` }));
    expect((await screen.findByRole("dialog", { name: label })).textContent).toMatch(copy);
    expect(screen.queryByRole("button", { name: "About Repository" })).toBeNull();
    if (label === "Runner routing") {
      await user.keyboard("{Escape}");
      await user.click(screen.getByRole("button", { name: "Open fleet" }));
      expect(onOpenFleet).toHaveBeenCalledOnce();
    }
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
    render(<RunnersSectionView fleet={{ ...fleet, editable: true, runners: [{ ...fleet.runners[0]!, editable: true, routing }] }} onSaveRouting={save} />);
    await user.click(screen.getByRole("button", { name: `Manage ${fleet.runners[0]!.display_name}` }));
    const sheet = screen.getByRole("dialog");
    expect(sheet.textContent).toContain("The lower of the two wins");
    expect(sheet.textContent).toContain("Tags pick from allowed runners; they never grant a project.");
    expect((screen.getByRole("spinbutton", { name: "Jobs at once" }) as HTMLInputElement).value).toBe("6");
    expect(screen.getByRole("button", { name: "Remove tag linux" })).toBeTruthy();
    expect(save).not.toHaveBeenCalled();
  });
});
