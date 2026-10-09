// @vitest-environment jsdom
//
// The account screens' presentational contracts: the login card, the wizard
// stepper and the organization table. (The settings navigation moved into the
// window's sidebar — `tests/components/settings.test.tsx` covers it.) Each is
// rendered
// on its own with plain props, so what is under test is the screen's own
// behaviour rather than the client it would otherwise be wired to.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import React from "react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";

import { decodeAccountBootstrap, type Member, type OnboardingStep } from "../../src/contracts/account.ts";
import { SidebarWorkspacePicker, WorkspacePicker } from "../../src/components/sidebar/SidebarWorkspacePicker.tsx";
import { ClientContext } from "../../src/app/client.ts";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import accountFixture from "../../src/contracts/fixtures/account-bootstrap.json";
import { SidebarProvider } from "../../src/components/ui/sidebar.tsx";
import {
  LoginCard,
  loginErrorMessage,
  OIDC_START,
  OIDC_SIGN_UP,
} from "../../src/app/account/Login.tsx";
import {
  InviteForm,
  MembersTable,
  OrganizationSwitcher,
  refusalMessage,
  SupportBanner,
} from "../../src/app/account/Organization.tsx";
import { firstUnreadyStep, orderedSteps, parsePolicyDescriptor, Stepper } from "../../src/app/account/Setup.tsx";
import { hubUrlNamesOrganization, parseCapacity, runnerNameFits, registerCommand, runnerHubUrl, shellArgument } from "../../src/app/fleet/EnrollRunner.tsx";
import { EntrySignIn } from "../../src/app/entry/EntryScreens.tsx";
import { AccountError } from "../../src/app/account/api.ts";
import { noApprovedPolicy, saveMessage, WorkflowSettings } from "../../src/app/account/ProjectSettings.tsx";
import { allowanceRows, allowanceLabel } from "../../src/app/settings/Settings.tsx";
import approval from "../../src/contracts/fixtures/account-policy.json";
import { WorkflowRevisions } from "../../src/app/account/WorkflowRevisions.tsx";

afterEach(cleanup);

describe("the footer workspace picker", () => {
  const current = { id: "org_a", name: "Threefold Solutions" };
  const other = { id: "org_b", name: "Another workspace with a long name" };

  it.each(["admin", "support", "billing", "viewer", "", undefined])("offers the platform console only to platform staff (%s)", async (platform_role) => {
    const account = decodeAccountBootstrap({
      ...accountFixture,
      actor: { ...accountFixture.actor, role: "member", can_manage: false, platform_role },
    });
    const root = createRootRoute({
      component: () => <SidebarProvider><ul><SidebarWorkspacePicker /></ul></SidebarProvider>,
    });
    const router = createRouter({
      routeTree: root,
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(
      <ClientContext.Provider value={{
        http: { origin: "", apiBase: account.api_base, csrfToken: account.csrf_token },
        account,
      } as ConversationClient}>
        <RouterProvider router={router as never} />
      </ClientContext.Provider>,
    );
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: /Switch workspace/ }));
    await screen.findByRole("menu");
    const entry = screen.queryByRole("menuitem", { name: "Platform console" });
    if (platform_role) {
      expect(entry?.getAttribute("href")).toBe("/platform/tenants");
      const activate = vi.fn((event: MouseEvent) => event.preventDefault());
      entry!.addEventListener("click", activate);
      await user.click(entry!);
      expect(activate).toHaveBeenCalledOnce();
      await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    } else {
      expect(entry).toBeNull();
      expect(screen.getByRole("menuitem", { name: "Add a workspace" })).toBeTruthy();
      expect(screen.getByRole("menuitem", { name: "Manage current workspace" })).toBeTruthy();
    }
  });

  function mount() {
    const onSelect = vi.fn();
    const onManage = vi.fn();
    const picker = (error: string | null = null) => (
      <SidebarProvider>
        <ul>
          <WorkspacePicker
            current={current}
            organizations={[current, other]}
            onSelect={onSelect}
            onManage={onManage}
            addHref="/organizations/new"
            error={error}
          />
        </ul>
        <button>Outside</button>
      </SidebarProvider>
    );
    const view = render(picker());
    return {
      onSelect,
      onManage,
      user: userEvent.setup(),
      reportError: (error: string) => view.rerender(picker(error)),
    };
  }

  it("filters complete names and selects another workspace with the keyboard", async () => {
    const { onSelect, user } = mount();
    const trigger = screen.getByRole("button", { name: /Switch workspace/ });
    trigger.focus();
    await user.keyboard("{Enter}");
    const input = await screen.findByRole("searchbox", { name: "Search workspaces" });
    await user.type(input, "  LONG name");
    expect(screen.queryByRole("menuitem", { name: current.name })).toBeNull();
    expect(screen.getByRole("menuitem", { name: other.name }).title).toBe(other.name);
    await user.keyboard("{ArrowDown}{Enter}");
    expect(onSelect).toHaveBeenCalledExactlyOnceWith(other.id);
    expect(screen.queryByRole("menu", { name: "Workspaces" })).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(trigger));
  });

  it("marks the current workspace and closes it without switching", async () => {
    const { onSelect, user } = mount();
    const trigger = screen.getByRole("button", { name: /Switch workspace/ });
    await user.click(trigger);
    const row = await screen.findByRole("menuitem", { name: current.name });
    expect(row.getAttribute("aria-current")).toBe("true");
    await user.click(row);
    expect(onSelect).not.toHaveBeenCalled();
    await waitFor(() => expect(document.activeElement).toBe(trigger));
  });

  it("reopens a failed switch so the authenticated owner's refusal is visible", async () => {
    const { user, reportError } = mount();
    await user.click(screen.getByRole("button", { name: /Switch workspace/ }));
    await user.click(await screen.findByRole("menuitem", { name: other.name }));
    reportError("You no longer have access to this organization.");
    expect((await screen.findByRole("alert")).textContent).toBe("You no longer have access to this organization.");
    expect(screen.getByRole("menuitem", { name: current.name }).getAttribute("aria-current")).toBe("true");
  });

  it("dismisses search with Escape or an outside click and retains the organization actions", async () => {
    const { onManage, user } = mount();
    const trigger = screen.getByRole("button", { name: /Switch workspace/ });
    await user.click(trigger);
    await user.type(await screen.findByRole("searchbox"), "missing");
    expect(screen.getByRole("status").textContent).toBe("No workspaces found.");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(document.activeElement).toBe(trigger));
    await user.click(trigger);
    expect((await screen.findByRole("searchbox") as HTMLInputElement).value).toBe("");
    expect(screen.getByRole("menuitem", { name: "Add a workspace" }).getAttribute("href")).toBe("/organizations/new");
    await user.click(screen.getByRole("menuitem", { name: "Manage current workspace" }));
    expect(onManage).toHaveBeenCalledOnce();
    await user.click(trigger);
    await screen.findByRole("menu");
    await user.click(screen.getByRole("button", { name: "Outside" }));
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
  });
});

// Base UI dispatches a synthetic click through `PointerEvent`, which jsdom
// does not implement. The event's own behaviour is not what these tests are
// about, so the constructor is filled in with the one jsdom does have.
if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

const MEMBERS: Member[] = [
  {
    id: "mem_owner",
    user_id: "user_michael",
    email: "michael@threefold.solutions",
    role: "owner",
    status: "active",
    grants: [{ project_id: "proj_parable", write: true, runner: true }],
  },
  {
    id: "mem_viewer",
    user_id: "user_sam",
    email: "sam@example.test",
    role: "viewer",
    status: "active",
    grants: [],
  },
];

const PROJECTS = [{ id: "proj_parable", name: "parable", can_write: true, can_manage_runners: true }];

function renderMembers(overrides: Partial<React.ComponentProps<typeof MembersTable>> = {}) {
  const onRoleChange = vi.fn();
  const onGrantChange = vi.fn();
  const onRemove = vi.fn();
  render(
    <MembersTable
      members={MEMBERS}
      projects={PROJECTS}
      canManage
      actorRole="owner"
      busyMember={null}
      errorFor={() => null}
      onRoleChange={onRoleChange}
      onGrantChange={onGrantChange}
      onRemove={onRemove}
      {...overrides}
    />,
  );
  return { onRoleChange, onGrantChange, onRemove };
}

describe("the login card", () => {
  it.each([LoginCard, EntrySignIn])("offers sign-in and account creation with the site navigation (%s)", (Component) => {
    render(<Component />);
    const card = screen.getByRole("region", { name: "Sign in to Detent" });
    expect(within(card).getAllByRole("link").map((link) => link.textContent)).toEqual(["Sign in", "Create account"]);
    expect(within(card).getByRole("link", { name: "Sign in" }).getAttribute("href")).toBe(OIDC_START);
    expect(within(card).getByRole("link", { name: "Create account" }).getAttribute("href")).toBe(OIDC_SIGN_UP);
    expect(document.body.textContent).not.toContain("WorkOS");
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("link", { name: "Join with invitation" })).toBeNull();
    const nav = screen.getByRole("navigation", { name: "Primary" });
    expect(within(nav).getByRole("link", { name: "detent.build home" }).getAttribute("href")).toBe("https://detent.build/");
    for (const [label, path] of [
      ["How it works", "/how-it-works"], ["Why Detent", "/why-detent"],
      ["Dashboard", "/dashboard"], ["Install", "/install"], ["Docs", "/docs"],
      ["Videos", "/videos"], ["Open source", "/open-source"],
    ]) {
      expect(within(nav).getByRole("link", { name: label }).getAttribute("href")).toBe(`https://detent.build${path}`);
    }
  });

  it("says what went wrong in words, never a raw code", () => {
    render(<LoginCard error="no_membership" />);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("not a member of this organization");
    expect(alert.textContent).not.toContain("no_membership");
  });

  it("has no error region when the query string carries none", () => {
    render(<LoginCard error={null} />);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("maps an unknown error code to a plain sentence rather than nothing", () => {
    expect(loginErrorMessage("something_new")).toBe("That sign-in did not complete. Try again.");
    expect(loginErrorMessage(null)).toBeNull();
    expect(loginErrorMessage("   ")).toBeNull();
  });


});

describe("the members table", () => {
  it.each([
    { email: "unsigned@example.test", name: "Unsigned Member", identity: "unsigned@example.test" },
    { email: "", name: "Unsigned Member", identity: "Unsigned Member" },
    { email: " ", name: " ", identity: "user_unsigned" },
  ])("identifies an unsigned member as $identity", ({ email, name, identity }) => {
    const member: Member = { ...MEMBERS[1]!, user_id: "user_unsigned", email, name, never_signed_in: true };
    renderMembers({ members: [member] });
    const row = screen.getByRole("row", { name: new RegExp(identity) });
    expect(row.textContent).toContain("Hasn't signed in yet");
    expect(screen.getByLabelText(`Role for ${identity}`)).toBeInstanceOf(HTMLSelectElement);
    expect((within(row).getByRole("button", { name: "Remove" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("offers a role control and a removal for a manager", () => {
    renderMembers();
    expect(
      screen.getByLabelText("Role for michael@threefold.solutions"),
    ).toBeInstanceOf(HTMLSelectElement);
    expect(screen.getAllByRole("button", { name: "Remove" })).toHaveLength(2);
  });

  it("shows a viewer the roles and none of the controls (§10.11)", () => {
    renderMembers({ canManage: false });
    expect(screen.queryByLabelText("Role for michael@threefold.solutions")).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
    expect(screen.getAllByText("owner")).not.toHaveLength(0);
  });

  it("offers only the role and project permissions the actor can grant", () => {
    renderMembers({ actorRole: "admin", projects: [{ ...PROJECTS[0]!, can_write: false, can_manage_runners: false }] });
    const select = screen.getByLabelText("Role for sam@example.test") as HTMLSelectElement;
    const owner = within(select).getByRole("option", { name: "Owner" }) as HTMLOptionElement;
    expect(owner.disabled).toBe(true);
    const access = screen.getByLabelText("Access to parable for sam@example.test") as HTMLSelectElement;
    expect((within(access).getByRole("option", { name: "Write" }) as HTMLOptionElement).disabled).toBe(true);
    expect(screen.getByLabelText("Runner management on parable for sam@example.test").getAttribute("aria-disabled")).toBe("true");
  });

  it("distinguishes read-only access from no access", () => {
    const { onGrantChange } = renderMembers();
    fireEvent.change(screen.getByLabelText("Access to parable for sam@example.test"), { target: { value: "read" } });
    expect(onGrantChange).toHaveBeenCalledWith(MEMBERS[1], "proj_parable", {
      project_id: "proj_parable",
      write: false,
      runner: false,
    });
    fireEvent.change(screen.getByLabelText("Access to parable for sam@example.test"), { target: { value: "none" } });
    expect(onGrantChange).toHaveBeenLastCalledWith(MEMBERS[1], "proj_parable", null);
  });

  it("surfaces a per-member refusal next to that member", () => {
    renderMembers({
      errorFor: (member) =>
        member.id === "mem_owner" ? "An organization keeps at least one owner." : null,
    });
    expect(screen.getByRole("alert").textContent).toContain("keeps at least one owner");
  });
});

describe("the organization's own rules", () => {
  it("labels an absent grant as no access", () => {
    renderMembers({ canManage: false });
    const viewer = screen.getByRole("row", { name: /sam@example.test/ });
    expect(viewer.textContent).toContain("no access");
    expect(viewer.textContent).not.toContain("read");
  });

  it("explains the last-owner refusal in the reader's terms", () => {
    const error = new AccountError({ status: 409, code: "last_owner", message: "last owner" });
    expect(refusalMessage(error)).toContain("keeps at least one owner");
  });

  it("passes any other refusal through as the hub worded it", () => {
    const error = new AccountError({ status: 422, code: "invalid_request", message: "Bad email" });
    expect(refusalMessage(error)).toBe("Bad email");
  });

  it("hides the switcher until there is somewhere to switch to", () => {
    const { container } = render(
      <OrganizationSwitcher
        organizations={[{ id: "org_a", name: "A", current: true }]}
        onSwitch={vi.fn()}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("switches only to an organization that is not the current one", () => {
    const onSwitch = vi.fn();
    render(
      <OrganizationSwitcher
        organizations={[
          { id: "org_a", name: "A", current: true },
          { id: "org_b", name: "B", current: false },
        ]}
        onSwitch={onSwitch}
      />,
    );
    const select = screen.getByLabelText("Switch organization");
    fireEvent.change(select, { target: { value: "org_b" } });
    expect(onSwitch).toHaveBeenCalledWith("org_b");
  });

  it("names the support actor, the reason and the way out", () => {
    const onExit = vi.fn();
    render(
      <SupportBanner
        actor="support@detent.dev"
        reason="ticket 4471"
        expiresAt="2026-09-10T22:30:00Z"
        onExit={onExit}
      />,
    );
    const banner = screen.getByRole("alert");
    expect(banner.textContent).toContain("support@detent.dev");
    expect(banner.textContent).toContain("ticket 4471");
    fireEvent.click(screen.getByRole("button", { name: "Exit support session" }));
    expect(onExit).toHaveBeenCalled();
  });

  it.each([true, false])("sends the chosen role and resets only on success (%s)", async (success) => {
    const onInvite = vi.fn().mockResolvedValue(success);
    render(<InviteForm onInvite={onInvite} projects={PROJECTS} />);
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "rae@example.test" } });
    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "admin" } });
    const access = screen.getByLabelText("Access to parable for invitation");
    expect((access as HTMLSelectElement).value).toBe("none");
    fireEvent.change(access, { target: { value: "read" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Send invitation" }));
    });
    expect(onInvite).toHaveBeenCalledWith({ email: "rae@example.test", role: "admin", grants: [{ project_id: "proj_parable", write: false, runner: false }] });
    expect((access as HTMLSelectElement).value).toBe(success ? "none" : "read");
    expect((screen.getByLabelText("Email") as HTMLInputElement).value).toBe(success ? "" : "rae@example.test");
    expect((screen.getByLabelText("Role") as HTMLSelectElement).value).toBe(success ? "member" : "admin");
    if (success) expect(screen.getByRole("status").textContent).toContain("Invitation sent to rae@example.test.");
    else expect(screen.queryByRole("status")).toBeNull();
  });
});

describe("the plan's allowances", () => {
  it("marks an allowance the window has already spent as over limit", () => {
    const rows = allowanceRows({
      organization_id: "org",
      base: { id: "pilot_free", version: 1 },
      effective_base: { id: "pilot_free", version: 1 },
      source: "base",
      revision: 1,
      allowances: { api_mutations: 10000, members: 10 },
      usage: { api_mutations: 11840, members: 3 },
      window_ends_at: "2026-09-10T13:00:00Z",
    });
    const mutations = rows.find((row) => row.name === "api_mutations");
    expect(mutations?.overLimit).toBe(true);
    expect(rows.find((row) => row.name === "members")?.overLimit).toBe(false);
  });

  it("reads an allowance name as English", () => {
    expect(allowanceLabel("api_mutations")).toBe("API mutations");
    expect(allowanceLabel("connected_runners")).toBe("connected runners");
  });
});

describe("the wizard stepper", () => {
  const steps: OnboardingStep[] = [
    { name: "Repository configuration", state: "ready", detail: "" },
    { name: "Local validation", state: "action_required", detail: "" },
    { name: "Execution runner", state: "ready", detail: "" },
    { name: "Artifact history", state: "action_required", detail: "" },
  ];

  it("opens on the first step that still needs something", () => {
    expect(firstUnreadyStep(steps)).toBe(1);
  });

  it("stays on the last step once everything is ready", () => {
    expect(firstUnreadyStep(steps.map((step) => ({ ...step, state: "ready" as const })))).toBe(3);
  });

  it("fills in a step the hub did not send, in Evaluate's order", () => {
    const ordered = orderedSteps({
      progress: { revision: "1", repository: "", doctor: false, provider: false, artifacts: "" },
      runners: [],
      artifact_services: [],
      steps: [{ name: "Execution runner", state: "ready", detail: "d" }],
      ready: false,
    });
    expect(ordered.map((step) => step.name)).toEqual([
      "Execution runner",
      "Local validation",
      "Repository configuration",
      "Artifact history",
    ]);
    expect(ordered[0]?.state).toBe("ready");
    expect(ordered[2]?.state).toBe("action_required");
  });

  it("says ready or action required in words, not only in colour", () => {
    render(<Stepper steps={steps} activeIndex={1} onSelect={vi.fn()} />);
    expect(screen.getAllByText("ready")).toHaveLength(2);
    expect(screen.getAllByText("action required")).toHaveLength(2);
  });

  it("marks the active step for a screen reader and lets any step be reached", () => {
    const onSelect = vi.fn();
    render(<Stepper steps={steps} activeIndex={1} onSelect={onSelect} />);
    const current = screen
      .getAllByRole("button")
      .filter((button) => button.getAttribute("aria-current") === "step");
    expect(current).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: /Artifact history/ }));
    expect(onSelect).toHaveBeenCalledWith(3);
  });
});

describe("the project settings", () => {
  it("shows the saved workflow and submits a reviewed edit without allowing viewer edits", () => {
    const integration = {
      profile: "native", revision: "2", intake: "disabled", projection: "disabled", repository_enabled: false,
      archive_completed_after_days: 30, archive_cancelled_after_days: 7,
      authority: { workflow: "detent" },
      states: [
        { name: "Backlog", terminal: false, dispatchable: false, operator_only: true, transitions: ["Todo"] },
        { name: "Todo", terminal: false, dispatchable: true, transitions: [] },
      ],
    };
    const onSave = vi.fn();
    const view = render(<WorkflowSettings integration={integration} canManage saving={false} error={null} onSave={onSave} />);
    expect(screen.getByText("Backlog · Initial · Nondispatchable · Operator only")).toBeTruthy();
    const states = [...integration.states, { name: "Rework", terminal: false, dispatchable: true, transitions: ["Todo"] }];
    const markdown = "---\ntracker:\n  kind: hub_native\n  active_states: [Todo, Rework]\n---\nComplete the issue.\n";
    fireEvent.change(screen.getByRole("textbox", { name: "Workflow definition" }), { target: { value: markdown } });
    fireEvent.click(screen.getByRole("button", { name: "Save workflow" }));
    expect(onSave).toHaveBeenCalledWith(markdown);
    view.rerender(<WorkflowSettings integration={{ ...integration, states }} canManage={false} saving={false} error={null} onSave={onSave} />);
    expect(screen.queryByRole("textbox", { name: "Workflow definition" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save workflow" })).toBeNull();
    expect(screen.getByText("Rework · Dispatchable")).toBeTruthy();
    view.rerender(<WorkflowSettings integration={{ ...integration, states, authority: { workflow: "repository" }, checkout_repository: "acme/parable", workflow_source: "detent.yaml", workflow_source_revision: "abc123" }} canManage saving={false} error={null} onSave={onSave} />);
    expect(screen.queryByRole("textbox", { name: "Workflow definition" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save workflow" })).toBeNull();
    expect(screen.getByText(/Controlled by acme\/parable: detent.yaml at revision abc123/)).toBeTruthy();
  });

  it("reads both shapes of \"nothing is approved\" as an answer, not a failure", () => {
    expect(
      noApprovedPolicy(new AccountError({ status: 404, code: "not_found", message: "" })),
    ).toBe(true);
    // The hosted preview answers this for a project with no matching approval.
    expect(
      noApprovedPolicy(new AccountError({ status: 409, code: "policy_mismatch", message: "" })),
    ).toBe(true);
    expect(
      noApprovedPolicy(new AccountError({ status: 503, code: "unavailable", message: "" })),
    ).toBe(false);
  });

  it("tells the reader to look again after a revision conflict, and never to retry blind", () => {
    const message = saveMessage(
      new AccountError({ status: 409, code: "revision_conflict", message: "conflict" }),
    );
    expect(message).toContain("re-read");
    expect(message).not.toContain("conflict");
  });

  it("passes an ordinary failure through in the hub's own words", () => {
    expect(
      saveMessage(new AccountError({ status: 422, code: "invalid_request", message: "Bad intake" })),
    ).toBe("Bad intake");
    expect(saveMessage(null)).toBeNull();
  });
});

describe("the pasted policy descriptor", () => {
  it.each([
    { name: "an inspect descriptor", text: '{"schema":1,"policy_id":"policy_abc","requirements":{}}', id: "policy_abc" },
    { name: "one surrounded by whitespace", text: '  {"policy_id":"policy_x"}\n', id: "policy_x" },
  ])("accepts $name", ({ text, id }) => {
    expect(parsePolicyDescriptor(text).policy_id).toBe(id);
  });

  it.each([
    { name: "text that is not JSON", text: "policy_abc", message: /not JSON/ },
    { name: "an array", text: "[]", message: /no policy_id/ },
    { name: "an object without an id", text: '{"schema":1}', message: /no policy_id/ },
    { name: "a numeric id", text: '{"policy_id":7}', message: /no policy_id/ },
  ])("refuses $name", ({ text, message }) => {
    expect(() => parsePolicyDescriptor(text)).toThrow(message);
  });
});

describe("the runner hub URL", () => {
  it.each([
    { name: "a standalone hub", url: "https://hub.example.com", base: "", want: "https://hub.example.com" },
    { name: "a tenant behind the shared entry", url: "https://cloud.example.com", base: "/organizations/org_1", want: "https://cloud.example.com/organizations/org_1" },
    { name: "a trailing slash", url: "https://cloud.example.com/", base: "/organizations/org_1/", want: "https://cloud.example.com/organizations/org_1" },
    { name: "a public URL that already names the tenant", url: "https://cloud.example.com/organizations/org_1", base: "/organizations/org_1", want: "https://cloud.example.com/organizations/org_1" },
  ])("addresses $name", ({ url, base, want }) => {
    expect(runnerHubUrl(url, base)).toBe(want);
  });
});

describe("the register command", () => {
  const input = {
    hubUrl: "https://cloud.example.com/organizations/org_1",
    organizationId: "org_1",
    token: "det_enroll_abc",
    name: "",
    capacity: 1,
    service: false,
  };
  it.each([
    { name: "the minimum", change: {}, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc" },
    { name: "a plain name", change: { name: "mac-studio" }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc --name mac-studio" },
    { name: "a name with spaces", change: { name: " Build host " }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc --name 'Build host'" },
    { name: "capacity and service", change: { capacity: 3, service: true }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc --capacity 3 --service" },
    { name: "a standalone hub", change: { hubUrl: "https://hub.example.com" }, want: "detent hub runner register --url https://hub.example.com --organization org_1 --token det_enroll_abc" },
    { name: "a URL naming a longer organization ID", change: { hubUrl: "https://cloud.example.com/organizations/org_1-extra" }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1-extra --organization org_1 --token det_enroll_abc" },
  ])("builds it for $name", ({ change, want }) => {
    expect(registerCommand({ ...input, ...change })).toBe(want);
  });

  it.each([
    { value: "plain", want: "plain" },
    { value: "two words", want: "'two words'" },
    { value: "Cory's Mac", want: "'Cory'\\''s Mac'" },
    { value: "$(rm -rf /)", want: "'$(rm -rf /)'" },
    { value: "", want: "''" },
  ])("quotes $value for the shell", ({ value, want }) => {
    expect(shellArgument(value)).toBe(want);
  });
});

describe("the enrollment inputs", () => {
  it.each([
    { url: "https://cloud.example.com/organizations/org_1", want: true },
    { url: "https://cloud.example.com/organizations/org_1/", want: true },
    { url: "https://cloud.example.com/organizations/org_1-extra", want: false },
    { url: "https://cloud.example.com/x/organizations/org_10", want: false },
    { url: "https://cloud.example.com", want: false },
    { url: "not a url", want: false },
  ])("reads $url as naming org_1: $want", ({ url, want }) => {
    expect(hubUrlNamesOrganization(url, "org_1")).toBe(want);
  });

  it.each([
    { value: "1", want: 1 },
    { value: " 4 ", want: 4 },
    { value: "16", want: 16 },
    { value: "0", want: null },
    { value: "17", want: null },
    { value: "2.9", want: null },
    { value: "", want: null },
    { value: "-1", want: null },
    { value: "1e1", want: null },
  ])("parses capacity $value as $want", ({ value, want }) => {
    expect(parseCapacity(value)).toBe(want);
  });
});

describe("the runner name limit", () => {
  it.each([
    { name: "a".repeat(200), fits: true },
    { name: "a".repeat(201), fits: false },
    { name: "é".repeat(100), fits: true },
    { name: "é".repeat(101), fits: false },
    { name: `  ${"a".repeat(200)}  `, fits: true },
  ])("counts UTF-8 bytes: $fits", ({ name, fits }) => {
    expect(runnerNameFits(name)).toBe(fits);
  });
});

const definition = { ...approval.policy, workflow: { source: "detent.yaml", states: [
  { name: "Todo", dispatchable: true, terminal: false, transitions: [] },
] } };
const props = {
  repository: "acme/orders", canApprove: false, approving: false,
  onApprove: vi.fn(), error: null, onLoadOlder: vi.fn(), loadingOlder: false,
};

it("does not invent an initial diff when a stored predecessor is unavailable", () => {
  render(<WorkflowRevisions {...props} observed={[]} policy={{ ...approval, policy: definition, history: [{
    id: "2", repository: "acme/orders", commit: "b".repeat(40),
    previous_definition_digest: "missing", definition_digest: "current", runner_id: "runner_test",
    applied_by: "runner_test", applied_at: "2026-10-05T12:00:00Z", definition,
  }] }} />);
  expect(screen.getByText("The stored definition for this comparison is unavailable.")).toBeTruthy();
  expect(screen.queryByRole("table")).toBeNull();
});

it("keeps old approvals out of pending revisions and does not label a digest as a commit", () => {
  render(<WorkflowRevisions {...props} policy={null} observed={[
    { policy: { ...definition, policy_id: "old" }, runner_id: "runner_old", observed_at: "2026-10-05T10:00:00Z", previously_approved: true },
    { policy: definition, runner_id: "runner_test", observed_at: "2026-10-05T12:00:00Z" },
  ]} />);
  expect(screen.getAllByRole("heading", { name: "Pending revision" })).toHaveLength(1);
  expect(screen.getByText("Commit not recorded")).toBeTruthy();
  expect(screen.queryByText(definition.source_revision)).toBeNull();
});
