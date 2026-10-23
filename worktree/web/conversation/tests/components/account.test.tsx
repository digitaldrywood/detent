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
import * as Schema from "effect/Schema";
import * as Effect from "effect/Effect";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";

import { decodeAccountBootstrap, type AccountBootstrap, type Member, type OnboardingStep } from "../../src/contracts/account.ts";
import { SidebarWorkspacePicker, WorkspacePicker } from "../../src/components/sidebar/SidebarWorkspacePicker.tsx";
import { ClientContext, ClientProvider, useClient } from "../../src/app/client.ts";
import { makeClient, type ConversationClient } from "../../src/runtime/bootstrap.ts";
import { Bootstrap } from "../../src/contracts/index.ts";
import membersFixture from "../../src/contracts/fixtures/account-members.json";
import { OrganizationMembersProvider } from "../../src/app/account/OrganizationMembers.tsx";
import { MemberAccessSheet, ProjectAccessMatrix } from "../../src/app/account/MemberAccess.tsx";
import accountFixture from "../../src/contracts/fixtures/account-bootstrap.json";
import integrationFixture from "../../src/contracts/fixtures/account-integration.json";
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
  OrganizationMembersRoute,
  OrganizationSwitcher,
  refusalMessage,
  SupportBanner,
} from "../../src/app/account/Organization.tsx";
import { firstUnreadyStep, orderedSteps, Stepper } from "../../src/app/account/Setup.tsx";
import { hubUrlNamesOrganization, parseCapacity, runnerNameFits, registerCommand, runnerHubUrl, shellArgument } from "../../src/app/fleet/EnrollRunner.tsx";
import { EntrySignIn } from "../../src/app/entry/EntryScreens.tsx";
import { AccountError } from "../../src/app/account/api.ts";
import { applyHubPaths, resetHubPaths } from "../../src/runtime/basePath.ts";
import { saveMessage, WorkflowSettings, ProjectDeletionSettings, ProjectSettingsRoute } from "../../src/app/account/ProjectSettings.tsx";
import { allowanceRows, allowanceLabel } from "../../src/app/settings/Settings.tsx";
import emptyApproval from "../../src/contracts/fixtures/account-policy-empty.json";
import { PolicyApproval } from "../../src/contracts/account.ts";

afterEach(cleanup);

describe("the footer workspace picker", () => {
  const current = { id: "org_a", name: "Example workspace" };
  const other = { id: "org_b", name: "Another workspace with a long name" };

  afterEach(() => {
    vi.unstubAllGlobals();
    resetHubPaths();
  });

  function mountAccount(account: AccountBootstrap) {
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
    return userEvent.setup();
  }

  it.each(["owner", "admin", "member", "viewer", "reporter"])("shows workspace roles and reporter-only sign out for %s", async (role) => {
    const account = decodeAccountBootstrap({
      ...accountFixture,
      actor: { ...accountFixture.actor, role },
      organizations: [
        { ...accountFixture.organizations[0], role, current: true },
        { ...accountFixture.organizations[0], id: "org_other", name: "Another workspace", role: "admin", current: false },
      ],
    });
    const user = mountAccount(account);
    await user.click(await screen.findByRole("button", { name: /Switch workspace/ }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByRole("menuitem", { name: "Another workspace" }).getAttribute("aria-description")).toBe("admin");
    expect(within(menu).getByRole("menuitem", { name: account.organization.name }).getAttribute("aria-description")).toBe(role);
    expect(within(menu).queryByRole("menuitem", { name: "Sign out" }) !== null).toBe(role === "reporter");
  });

  it.each(["admin", "support", "billing", "viewer", "", undefined])("offers the platform console only to platform staff (%s)", async (platform_role) => {
    const account = decodeAccountBootstrap({
      ...accountFixture,
      actor: { ...accountFixture.actor, role: "member", can_manage: false, platform_role },
    });
    const user = mountAccount(account);
    await user.click(await screen.findByRole("button", { name: /Switch workspace/ }));
    await screen.findByRole("menu");
    expect(screen.queryByRole("menuitem", { name: "Manage current workspace" })).toBeNull();
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
    }
  });

  it.each(["unauthorized", "unavailable", "network", "decode", "success"] as const)("shows the shared organization read's %s state without a partial bootstrap list", async (outcome) => {
    applyHubPaths({ base_path: "/organizations/org_a" });
    const account = decodeAccountBootstrap({
      ...accountFixture,
      organization: { ...accountFixture.organization, ...current },
      organizations: [current, other].map((org) => ({ ...org, public_url: `https://${org.id}.example.test`, current: org.id === current.id })),
      actor: { ...accountFixture.actor, platform_role: "admin" },
    });
    let resolve!: (response: Response) => void;
    let reject!: (cause: Error) => void;
    const response = new Promise<Response>((yes, no) => { resolve = yes; reject = no; });
    const fetch = vi.fn(() => response);
    const assign = vi.fn();
    vi.stubGlobal("fetch", fetch);
    vi.stubGlobal("location", { assign });
    const user = mountAccount(account);
    try {
      await user.click(await screen.findByRole("button", { name: /Switch workspace/ }));
      expect((await screen.findByRole("status")).textContent).toBe("Loading workspaces…");
      expect(screen.getByRole("menuitem", { name: current.name })).toBeTruthy();
      expect(screen.queryByRole("menuitem", { name: other.name })).toBeNull();
      expect(fetch).toHaveBeenCalledExactlyOnceWith("/api/cloud/organizations", expect.objectContaining({ credentials: "same-origin" }));
      await act(async () => {
        if (outcome === "network") reject(new TypeError("Failed to fetch"));
        else if (outcome === "decode") resolve(Response.json({ organizations: [other] }));
        else if (outcome === "success") resolve(Response.json({
          email: "member@example.test", csrf: "entry-csrf",
          organizations: [current, other].map((org) => ({ ...org, url: `/organizations/${org.id}`, state: "ready" })),
          pending: [{ id: "org_pending", name: "Pending workspace", url: "/organizations/org_pending", state: "allocating" }],
        }));
        else resolve(Response.json({ code: outcome === "unauthorized" ? "unauthorized" : "membership_unavailable" }, { status: outcome === "unauthorized" ? 401 : 503 }));
      });
      expect(screen.queryByRole("status")).toBeNull();
      expect(screen.getByRole("menuitem", { name: current.name }).getAttribute("aria-current")).toBe("true");
      expect(screen.getByRole("menuitem", { name: "Add a workspace" }).getAttribute("href")).toBe("/organizations/new");
      expect(screen.getByRole("menuitem", { name: "Platform console" }).getAttribute("href")).toBe("/platform/tenants");
      if (outcome === "success") {
        expect(screen.queryByRole("alert")).toBeNull();
        expect(screen.queryByRole("menuitem", { name: "Pending workspace" })).toBeNull();
        await user.click(screen.getByRole("menuitem", { name: other.name }));
        expect(assign).toHaveBeenCalledExactlyOnceWith("/organizations/org_b");
        expect(fetch).toHaveBeenCalledOnce();
      } else {
        expect(screen.getByRole("alert").textContent).toBe(outcome === "unauthorized"
          ? "Your Cloud session has ended."
          : "Could not load workspaces. Try again shortly.");
        expect(screen.queryByRole("menuitem", { name: other.name })).toBeNull();
        const signIn = screen.queryByRole("menuitem", { name: "Sign in again" });
        if (outcome === "unauthorized") {
          expect(signIn?.getAttribute("href")).toBe("/auth/oidc/start?return=%2Forganizations");
          const activate = vi.fn((event: MouseEvent) => event.preventDefault());
          signIn!.addEventListener("click", activate);
          await user.click(signIn!);
          expect(activate).toHaveBeenCalledOnce();
        } else expect(signIn).toBeNull();
      }
    } finally {
      cleanup();
      resolve(Response.json({ email: "member@example.test", csrf: "", organizations: [] }));
    }
  });

  function mount() {
    const onSelect = vi.fn();
    const picker = (error: string | null = null) => (
      <SidebarProvider>
        <ul>
          <WorkspacePicker
            current={current}
            organizations={[current, other]}
            onSelect={onSelect}
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
    await user.keyboard("{ArrowUp}");
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Add a workspace" }));
    await user.click(input);
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

  it("dismisses search with Escape or an outside click and retains only the add workspace action", async () => {
    const { user } = mount();
    const trigger = screen.getByRole("button", { name: /Switch workspace/ });
    await user.click(trigger);
    await user.type(await screen.findByRole("searchbox"), "missing");
    expect(screen.getByRole("status").textContent).toBe("No workspaces found.");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(document.activeElement).toBe(trigger));
    await user.click(trigger);
    expect((await screen.findByRole("searchbox") as HTMLInputElement).value).toBe("");
    expect(screen.getByRole("menuitem", { name: "Add a workspace" }).getAttribute("href")).toBe("/organizations/new");
    expect(screen.queryByRole("menuitem", { name: "Manage current workspace" })).toBeNull();
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
    user_id: "user_owner",
    email: "owner@example.com",
    role: "owner",
    status: "active",
    grants: [{ project_id: "proj_example_project", write: true, runner: true }],
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

const PROJECTS = [{ id: "proj_example_project", name: "example-project", can_write: true, can_manage_runners: true }];

function renderMembers(overrides: Partial<React.ComponentProps<typeof MembersTable>> = {}) {
  const onRoleChange = vi.fn();
  const onEdit = vi.fn();
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
      roles={membersFixture.roles}
      onEdit={onEdit}
      onRemove={onRemove}
      {...overrides}
    />,
  );
  return { onRoleChange, onEdit, onRemove };
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

async function chooseRole(name: string, value: string): Promise<void> {
  const user = userEvent.setup();
  await user.click(screen.getByRole("combobox", { name }));
  await user.click(await screen.findByRole("option", { name: value }));
}

describe("the members table", () => {
  it.each([
    { email: "unsigned@example.test", name: "Unsigned Member", identity: "unsigned@example.test" },
    { email: "", name: "Unsigned Member", identity: "Unsigned Member" },
    { email: " ", name: " ", identity: "user_unsigned" },
  ])("identifies an unsigned member as $identity", ({ email, name, identity }) => {
    const member: Member = { ...MEMBERS[1]!, user_id: "user_unsigned", email, name, never_signed_in: true };
    renderMembers({ members: [member] });
    const row = screen.getByRole("row", { name: new RegExp(identity) });
    expect(row.textContent).toContain("Hasn't signed in");
    expect(screen.getByRole("combobox", { name: `Role for ${identity}` })).toBeTruthy();
    expect(screen.getByRole("button", { name: `Actions for ${identity}` })).toBeTruthy();
  });

  it("renders new server roles and assigns them without changing the UI", async () => {
    const onRoleChange = vi.fn();
    const member = { ...MEMBERS[1]!, role: "reviewer" };
    renderMembers({ members: [member], roles: [...membersFixture.roles, { slug: "reviewer", name: "Reviewer", description: "Reviews project work." }], onRoleChange });
    await chooseRole("Role for sam@example.test", "Reviewer");
    expect(onRoleChange).toHaveBeenCalledWith(member, "reviewer");
  });

  it("protects the owner role and offers removal through the row menu", async () => {
    const { onRemove } = renderMembers();
    expect(screen.queryByRole("combobox", { name: "Role for owner@example.com" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Actions for sam@example.test" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove from organization" }));
    expect(onRemove).toHaveBeenCalledWith(MEMBERS[1]);
  });

  it("shows a viewer the roles and summaries with no controls", () => {
    renderMembers({ canManage: false });
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("Owner")).toBeTruthy();
  });

  it("does not let an admin assign owner", async () => {
    renderMembers({ actorRole: "admin" });
    fireEvent.click(screen.getByRole("combobox", { name: "Role for sam@example.test" }));
    expect((await screen.findByRole("option", { name: "Owner" })).getAttribute("aria-disabled")).toBe("true");
  });

  it("summarizes all, no and mixed project access and opens the sheet", () => {
    const projects = [...PROJECTS, { ...PROJECTS[0]!, id: "proj_other", name: "other-project" }];
    const member = { ...MEMBERS[1]!, grants: [{ project_id: PROJECTS[0]!.id, write: true, runner: false }, { project_id: "proj_other", write: false, runner: false }] };
    const { onEdit } = renderMembers({ projects, members: [MEMBERS[0]!, member], actorId: MEMBERS[0]!.user_id });
    expect(screen.getByText("You")).toBeTruthy();
    expect(screen.getByRole("button", { name: /sam@example.test: All 2 · Mixed access/ })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /sam@example.test: All 2/ }));
    expect(onEdit).toHaveBeenCalledWith(member);
    expect(screen.queryByLabelText("Access to example-project for sam@example.test")).toBeNull();
  });

  it("surfaces a per-member refusal next to that member", () => {
    renderMembers({ errorFor: (member) => member.id === "mem_owner" ? "An organization keeps at least one owner." : null });
    expect(screen.getByRole("alert").textContent).toContain("keeps at least one owner");
  });
});

describe("the members route", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("filters members, discards cancelled changes, saves access and refreshes after a conflict", async () => {
    let data = { members: MEMBERS.map((member) => ({ ...member })), invitations: [], roles: membersFixture.roles };
    const writes: Array<{ method: string; path: string; body: Record<string, unknown> }> = [];
    let conflict = false;
    vi.stubGlobal("fetch", vi.fn(async (input: string, init: RequestInit) => {
      if (init.method === "GET") return new Response(JSON.stringify(data), { status: 200 });
      const body = JSON.parse(String(init.body)) as Record<string, unknown>;
      writes.push({ method: init.method ?? "GET", path: input, body });
      if (conflict) return new Response(JSON.stringify({ code: "revision_conflict", message: "Access changed. Reload and try again." }), { status: 409 });
      const member = data.members.find((member) => member.id === "mem_viewer")!;
      const next = input.endsWith("/role") ? { ...member, role: String(body.role) } : { ...member, grants: [{ project_id: String(body.project_id), write: body.write === true, runner: body.runner === true }] };
      data = { ...data, members: data.members.map((member) => member.id === next.id ? next : member) };
      return new Response(JSON.stringify(next), { status: 200 });
    }));
    const account = decodeAccountBootstrap({ ...accountFixture, projects: PROJECTS.map((project) => ({ ...accountFixture.projects[0], ...project })) });
    const root = createRootRoute({ component: () => <OrganizationMembersProvider><OrganizationMembersRoute /></OrganizationMembersProvider> });
    const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ["/"] }) });
    render(<ClientContext.Provider value={{ http: { origin: "", apiBase: account.api_base, csrfToken: account.csrf_token }, account } as ConversationClient}><RouterProvider router={router as never} /></ClientContext.Provider>);
    await screen.findByRole("row", { name: /sam@example.test/ });
    fireEvent.change(screen.getByRole("searchbox", { name: "Search members" }), { target: { value: "sam" } });
    expect(screen.queryByRole("row", { name: /owner@example.com/ })).toBeNull();
    await chooseRole("Filter by role", "Owner");
    expect(screen.getByText("No matching members.")).toBeTruthy();
    await chooseRole("Filter by role", "All roles");
    const openAccess = () => fireEvent.click(screen.getByRole("button", { name: /^Project access for sam@example.test/ }));
    openAccess();
    await chooseRole("Role for sam@example.test", "Reporter");
    fireEvent.click(within(screen.getByRole("group", { name: "Access to example-project for sam@example.test" })).getByRole("button", { name: "Read" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(writes).toEqual([]);
    openAccess();
    const group = screen.getByRole("group", { name: "Access to example-project for sam@example.test" });
    expect(within(group).getByRole("button", { name: "None" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(within(group).getByRole("button", { name: "Read" }));
    await chooseRole("Role for sam@example.test", "Reporter");
    fireEvent.click(screen.getByRole("button", { name: "Save access" }));
    await screen.findByRole("button", { name: /sam@example.test: All 1 · Read/ });
    expect(writes.map((write) => write.path.split("/").pop())).toEqual(["role", "grants"]);
    expect(writes[0]!.body.role).toBe("reporter");
    expect(writes[1]!.body).toMatchObject({ project_id: PROJECTS[0]!.id, write: false, runner: false, revoke: false });
    conflict = true;
    openAccess();
    fireEvent.click(within(screen.getByRole("group", { name: "Access to example-project for sam@example.test" })).getByRole("button", { name: "Write" }));
    fireEvent.click(screen.getByRole("button", { name: "Save access" }));
    const dialog = await screen.findByRole("dialog", { name: "Project access" });
    await waitFor(() => expect(within(dialog).getByRole("alert").textContent).toContain("Access changed"));
    await waitFor(() => expect(within(screen.getByRole("group", { name: "Access to example-project for sam@example.test" })).getByRole("button", { name: "Read" }).getAttribute("aria-pressed")).toBe("true"));
  });
});

describe("the project access sheet", () => {
  it("keeps role and grant edits local until Save", async () => {
    const onSave = vi.fn().mockResolvedValue(true);
    const onClose = vi.fn();
    const member = MEMBERS[1]!;
    render(<MemberAccessSheet member={member} projects={PROJECTS} roles={membersFixture.roles} actorRole="owner" pending={false} error={null} onSave={onSave} onRemove={vi.fn()} onClose={onClose} />);
    await chooseRole("Role for sam@example.test", "Reporter");
    const access = screen.getByRole("group", { name: "Access to example-project for sam@example.test" });
    fireEvent.click(within(access).getByRole("button", { name: "Read" }));
    expect(onSave).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Save access" }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith(member, "reporter", [{ project_id: PROJECTS[0]!.id, write: false, runner: false }]));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("updates every project with the all-projects row and revokes runner grants with None", () => {
    const projects = [...PROJECTS, { ...PROJECTS[0]!, id: "proj_other", name: "other-project" }];
    function Matrix() {
      const [grants, setGrants] = React.useState([{ project_id: PROJECTS[0]!.id, write: true, runner: true }]);
      return <ProjectAccessMatrix projects={projects} grants={grants} identity="member" onChange={setGrants} />;
    }
    render(<Matrix />);
    const all = screen.getByRole("group", { name: "Access to All projects for member" });
    fireEvent.click(within(all).getByRole("button", { name: "Read" }));
    for (const name of ["example-project", "other-project"]) expect(within(screen.getByRole("group", { name: `Access to ${name} for member` })).getByRole("button", { name: "Read" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(screen.getByRole("switch", { name: /^Runner management on All projects for member/ }));
    for (const name of ["example-project", "other-project"]) expect(screen.getByRole("switch", { name: new RegExp(`^Runner management on ${name} for member`) }).getAttribute("aria-checked")).toBe("true");
    fireEvent.click(within(all).getByRole("button", { name: "None" }));
    for (const name of ["All projects", "example-project", "other-project"]) {
      const runners = screen.getByRole("switch", { name: new RegExp(`^Runner management on ${name} for member`) });
      expect(runners.getAttribute("aria-disabled")).toBe("true");
      expect(runners.getAttribute("aria-checked")).toBe("false");
    }
  });

  it("disables individual and bulk grants the actor cannot give", () => {
    const projects = [...PROJECTS, { ...PROJECTS[0]!, id: "proj_other", name: "other-project", can_write: false, can_manage_runners: false }];
    render(<ProjectAccessMatrix projects={projects} grants={projects.map((project) => ({ project_id: project.id, write: false, runner: false }))} identity="member" onChange={vi.fn()} />);
    for (const name of ["All projects", "other-project"]) {
      expect(within(screen.getByRole("group", { name: `Access to ${name} for member` })).getByRole("button", { name: "Write" }).getAttribute("aria-disabled")).toBe("true");
      expect(screen.getByRole("switch", { name: new RegExp(`^Runner management on ${name} for member`) }).getAttribute("aria-disabled")).toBe("true");
    }
  });
});

describe("the organization's own rules", () => {
  it("labels an absent grant as no access", () => {
    renderMembers({ canManage: false });
    const viewer = screen.getByRole("row", { name: /sam@example.test/ });
    expect(viewer.textContent).toContain("No projects · Grant access");
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
        actor="support@example.com"
        reason="ticket 4471"
        expiresAt="2026-09-10T22:30:00Z"
        onExit={onExit}
      />,
    );
    const banner = screen.getByRole("alert");
    expect(banner.textContent).toContain("support@example.com");
    expect(banner.textContent).toContain("ticket 4471");
    fireEvent.click(screen.getByRole("button", { name: "Exit support session" }));
    expect(onExit).toHaveBeenCalled();
  });

  it.each([true, false])("sends several addresses with chosen permissions and keeps only failures (%s)", async (success) => {
    const onInvite = vi.fn().mockImplementation(async ({ email }: { email: string }) => success || email !== "rae@example.test");
    render(<InviteForm onInvite={onInvite} projects={PROJECTS} roles={membersFixture.roles} />);
    fireEvent.change(screen.getByLabelText("Email addresses"), { target: { value: "rae@example.test, sam@example.test rae@example.test" } });
    await chooseRole("Role", "Reporter");
    expect(screen.getByText("Reads issues and files new ones into Triage.")).toBeTruthy();
    const access = screen.getByRole("group", { name: "Access to example-project for invitation" });
    expect(within(access).getByRole("button", { name: "None" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(within(access).getByRole("button", { name: "Read" }));
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Send 2 invitations" })); });
    expect(onInvite).toHaveBeenCalledTimes(2);
    for (const email of ["rae@example.test", "sam@example.test"]) expect(onInvite).toHaveBeenCalledWith({ email, role: "reporter", grants: [{ project_id: PROJECTS[0]!.id, write: false, runner: false }] });
    expect(screen.queryByRole("button", { name: "Remove sam@example.test" })).toBeNull();
    expect(screen.getByRole("status").textContent).toContain(success ? "2 invitations sent." : "1 invitation sent. 1 could not be sent.");
    if (!success) {
      expect(screen.getByRole("button", { name: "Remove rae@example.test" })).toBeTruthy();
      await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Send 1 invitation" })); });
      expect(onInvite).toHaveBeenCalledTimes(3);
      expect(onInvite.mock.calls[2]![0].email).toBe("rae@example.test");
    }
  });

  it("accepts pasted addresses as tags and refuses invalid addresses before sending", async () => {
    const onInvite = vi.fn().mockResolvedValue(true);
    render(<InviteForm onInvite={onInvite} projects={PROJECTS} roles={membersFixture.roles} />);
    const input = screen.getByLabelText("Email addresses");
    fireEvent.paste(input, { clipboardData: { getData: () => "one@example.test, two@example.test\ninvalid" } });
    expect(screen.getByRole("button", { name: "Remove one@example.test" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Send 3 invitations" }));
    expect(onInvite).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent).toContain("valid email address");
    fireEvent.click(screen.getByRole("button", { name: "Remove invalid" }));
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Send 2 invitations" })); });
    expect(onInvite).toHaveBeenCalledTimes(2);
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
    { name: "Repository", state: "ready", detail: "" },
    { name: "Checkout", state: "action_required", detail: "" },
    { name: "Runner", state: "ready", detail: "" },
    { name: "Run files", state: "action_required", detail: "" },
  ];

  it("opens on the first step that still needs something", () => {
    expect(firstUnreadyStep(steps)).toBe(1);
  });

  it("stays on the last step once everything is ready", () => {
    expect(firstUnreadyStep(steps.map((step) => ({ ...step, state: "ready" as const })))).toBe(3);
  });

  it("keeps the Hub steps in runner-first order and preserves their readiness", () => {
    const ordered = orderedSteps({
      progress: { revision: "1", repository: "", doctor: false, provider: false, artifacts: "" },
      runners: [],
      artifact_services: [],
      steps: [{ name: "Runner", state: "ready", detail: "d" }],
      ready: false,
    });
    expect(ordered.map((step) => step.name)).toEqual([
      "Runner",
      "Repository",
      "Checkout",
      "Run files",
    ]);
    expect(ordered[0]?.state).toBe("ready");
    expect(ordered[1]?.state).toBe("action_required");
    expect(ordered[3]?.state).toBe("action_required");
  });

  it("says ready or action required in words, not only in colour", () => {
    render(<Stepper steps={steps} activeIndex={1} onSelect={vi.fn()} summaries={{ "Runner": "Example runner", "Repository": "Configuration approved", "Checkout": "Check your runner" }} />);
    for (const step of steps) expect(screen.getByText(step.name, { exact: true })).toBeTruthy();
    expect(screen.getByText("Example runner")).toBeTruthy();
    expect(screen.getByText("Configuration approved")).toBeTruthy();
    expect(screen.getByText("Check your runner")).toBeTruthy();
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
    fireEvent.click(screen.getByRole("button", { name: /Run files/ }));
    expect(onSelect).toHaveBeenCalledWith(3);
  });
});

describe("the project settings", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    resetHubPaths();
  });

  it.each(["success", "refused", "refresh_refused", "refresh_network"] as const)("refreshes project and conversation lists or reloads app home after deletion (%s)", async (outcome) => {
    const user = userEvent.setup();
    const account = decodeAccountBootstrap(accountFixture);
    const project = account.projects[0]!;
    const remaining = { ...project, id: "proj_remaining", name: "Remaining project" };
    const initial = { ...account, projects: [project, remaining] };
    const client = makeClient({ bootstrap: Schema.decodeUnknownSync(Bootstrap)(initial), account: initial });
    let finishConversations!: () => void;
    const conversations = new Promise<void>((resolve) => { finishConversations = resolve; });
    const refreshConversations = vi.spyOn(client.effects, "refreshList").mockImplementation(() => Effect.promise(() => conversations));
    const assign = vi.fn();
    vi.stubGlobal("location", { assign });
    let current = client;
    function Projects() {
      current = useClient();
      return <ul aria-label="Available projects">{current.account?.projects.map((item) => <li key={item.id}>{item.name}</li>)}</ul>;
    }
    let finishDelete!: (response: Response) => void;
    const deletion = new Promise<Response>((resolve) => { finishDelete = resolve; });
    let finishBootstrap!: (response: Response) => void;
    let rejectBootstrap!: (cause: Error) => void;
    const bootstrap = new Promise<Response>((resolve, reject) => { finishBootstrap = resolve; rejectBootstrap = reject; });
    const refresh = vi.fn(() => bootstrap);
    const remove = vi.fn(() => deletion);
    vi.stubGlobal("fetch", vi.fn((input: string, init?: RequestInit) => {
      if (input === "/app/bootstrap") return refresh();
      if (input.endsWith(`/projects/${project.id}`) && init?.method === "DELETE") {
        const body = JSON.parse(String(init.body));
        expect(body.confirm_name).toBe(project.name);
        expect(body.idempotency_key).toEqual(expect.any(String));
        expect(body.idempotency_key.length).toBeGreaterThan(0);
        return remove();
      }
      if (input.endsWith("/integration")) return Promise.resolve(Response.json(integrationFixture));
      return Promise.resolve(Response.json({ code: "not_found", message: "Not available" }, { status: 404 }));
    }));
    const navigate = vi.fn();
    const root = createRootRoute({ component: () => <><Projects /><ProjectSettingsRoute projectId={project.id} tab="danger" onNavigate={navigate} /></> });
    const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ["/"] }) });
    render(<ClientProvider client={client}><RouterProvider router={router as never} /></ClientProvider>);
    await user.click(await screen.findByRole("button", { name: "Delete project" }));
    await user.type(within(screen.getByRole("alertdialog")).getByRole("textbox", { name: `To confirm, type ${project.name} below` }), project.name);
    await user.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Delete project" }));
    expect(remove).toHaveBeenCalledOnce();
    expect(refresh).not.toHaveBeenCalled();
    expect(navigate).not.toHaveBeenCalled();
    if (outcome === "refused") {
      await act(async () => finishDelete(Response.json({ code: "conflict", message: "Finish active work first" }, { status: 409 })));
      expect(within(screen.getByRole("alertdialog")).getByText("Finish active work first")).toBeTruthy();
      expect(refresh).not.toHaveBeenCalled();
      expect(navigate).not.toHaveBeenCalled();
      expect(current.account?.projects).toEqual([project, remaining]);
      expect(refreshConversations).not.toHaveBeenCalled();
      expect(assign).not.toHaveBeenCalled();
      return;
    }
    await act(async () => finishDelete(new Response(null, { status: 204 })));
    expect(refresh).toHaveBeenCalledOnce();
    expect(navigate).not.toHaveBeenCalled();
    if (outcome === "refresh_refused" || outcome === "refresh_network") {
      await act(async () => {
        if (outcome === "refresh_network") rejectBootstrap(new TypeError("Failed to fetch"));
        else finishBootstrap(Response.json({ message: "Unavailable" }, { status: 503 }));
      });
      expect(assign).toHaveBeenCalledExactlyOnceWith("/work");
      expect(navigate).not.toHaveBeenCalled();
      expect(remove).toHaveBeenCalledOnce();
      expect(refreshConversations).not.toHaveBeenCalled();
      return;
    }
    await act(async () => finishBootstrap(Response.json({ ...accountFixture, projects: [remaining] })));
    expect(refreshConversations).toHaveBeenCalledOnce();
    expect(navigate).not.toHaveBeenCalled();
    await act(async () => finishConversations());
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/work"));
    expect(assign).not.toHaveBeenCalled();
    expect(current.bootstrap.projects.map((item) => item.id)).toEqual([remaining.id]);
    expect(current.account?.projects.map((item) => item.id)).toEqual([remaining.id]);
    expect(within(screen.getByRole("list", { name: "Available projects" })).queryByText(project.name)).toBeNull();
    expect(within(screen.getByRole("list", { name: "Available projects" })).getByText(remaining.name)).toBeTruthy();
  });

  it("confirms the named deletion, cancels without a mutation, and keeps contextual failure feedback", async () => {
    const user = userEvent.setup();
    const onDelete = vi.fn();
    const onDismiss = vi.fn();
    const view = render(<ProjectDeletionSettings projectName="Retired project" pending={false} error={null} onDelete={onDelete} onDismiss={onDismiss} />);
    await user.click(screen.getByRole("button", { name: "Delete project" }));
    const dialog = screen.getByRole("alertdialog", { name: "Delete Retired project?" });
    expect(within(dialog).getByText(/deletes its secrets and access grants/)).toBeTruthy();
    expect(within(dialog).getByText(/stored artifacts are retained/)).toBeTruthy();
    const warning = within(dialog).getByRole("alert");
    expect(warning.textContent).toBe("This action cannot be undone.");
    expect(warning.getAttribute("data-variant")).toBe("error");
    const input = within(dialog).getByRole("textbox", { name: "To confirm, type Retired project below" });
    const button = within(dialog).getByRole("button", { name: "Delete project" }) as HTMLButtonElement;
    expect(button.disabled).toBe(true);
    await user.click(button);
    await user.click(input);
    await user.keyboard("{Enter}");
    expect(onDelete).not.toHaveBeenCalled();
    for (const name of ["retired project", "Retired", "Retired project extra"]) {
      await user.clear(input);
      await user.type(input, name);
      expect(button.disabled).toBe(true);
      fireEvent.submit(input.closest("form")!);
      await user.keyboard("{Enter}");
      expect(onDelete).not.toHaveBeenCalled();
    }
    await user.clear(input);
    await user.type(input, "Retired project");
    expect(button.disabled).toBe(false);
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(onDelete).not.toHaveBeenCalled();
    await user.click(screen.getByRole("button", { name: "Delete project" }));
    const reopened = within(screen.getByRole("alertdialog"));
    const reopenedInput = reopened.getByRole("textbox", { name: "To confirm, type Retired project below" }) as HTMLInputElement;
    expect(reopenedInput.value).toBe("");
    expect((reopened.getByRole("button", { name: "Delete project" }) as HTMLButtonElement).disabled).toBe(true);
    await user.type(reopenedInput, "  Retired project  ");
    expect((reopened.getByRole("button", { name: "Delete project" }) as HTMLButtonElement).disabled).toBe(false);
    await user.click(reopened.getByRole("button", { name: "Delete project" }));
    expect(onDelete).toHaveBeenCalledOnce();
    view.rerender(<ProjectDeletionSettings projectName="Retired project" pending error={null} onDelete={onDelete} onDismiss={onDismiss} />);
    expect((screen.getByRole("button", { name: "Deleting…" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(true);
    expect(reopenedInput.disabled).toBe(true);
    await user.keyboard("{Escape}");
    expect(screen.getByRole("alertdialog")).toBeTruthy();
    fireEvent.submit(reopenedInput.closest("form")!);
    expect(onDelete).toHaveBeenCalledOnce();
    view.rerender(<ProjectDeletionSettings projectName="Retired project" pending={false} error="Finish active work first" onDelete={onDelete} onDismiss={onDismiss} />);
    expect(within(screen.getByRole("alertdialog")).getByText("Finish active work first")).toBeTruthy();
    await user.click(reopenedInput);
    await user.keyboard("{Enter}");
    expect(onDelete).toHaveBeenCalledTimes(2);
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await user.click(screen.getByRole("button", { name: "Delete project" }));
    expect((screen.getByRole("textbox", { name: "To confirm, type Retired project below" }) as HTMLInputElement).value).toBe("");
  });

  it.each([false, true])("shows lane details and preserves workflow editing on mobile %s", async (phone) => {
    vi.stubGlobal("PointerEvent", MouseEvent);
    const media = vi.spyOn(window, "matchMedia").mockImplementation((query) => ({ matches: phone, media: query, onchange: null, addListener: vi.fn(), removeListener: vi.fn(), addEventListener: vi.fn(), removeEventListener: vi.fn(), dispatchEvent: vi.fn() }));
    const integration = {
      profile: "native", revision: "2", intake: "disabled", projection: "disabled", repository_enabled: false,
      archive_completed_after_days: 30, archive_cancelled_after_days: 7,
      authority: { workflow: "detent" },
      states: [
        { name: "Backlog", terminal: false, dispatchable: false, operator_only: true, transitions: ["Todo"] },
        { name: "Todo", terminal: false, dispatchable: true, transitions: [] },
        { name: "Blocked", terminal: false, dispatchable: false, transitions: ["Todo"] },
        { name: "Done", terminal: true, dispatchable: false, transitions: [] },
      ],
    };
    const onSave = vi.fn();
    const view = render(<WorkflowSettings integration={integration} canManage saving={false} error={null} onSave={onSave} />);
    expect(within(screen.getByRole("button", { name: /Backlog/ })).getByLabelText("Initial")).toBeTruthy();
    expect(within(screen.getByRole("button", { name: /Todo/ })).getByLabelText("Dispatchable")).toBeTruthy();
    expect(screen.getByText("Waiting for runner").querySelector("span")?.className).toContain("bg-muted-foreground");
    expect(within(screen.getByRole("button", { name: /Blocked/ })).getByLabelText("Holding")).toBeTruthy();
    expect(within(screen.getByRole("button", { name: /Done/ })).getByLabelText("Terminal")).toBeTruthy();
    const user = userEvent.setup();
    if (phone) await user.click(screen.getByRole("button", { name: /Backlog/ }));
    else await user.hover(screen.getByRole("button", { name: /Backlog/ }));
    const tooltip = await screen.findByRole("tooltip");
    expect(within(tooltip).getByText(/Holding/)).toBeTruthy();
    expect(within(tooltip).getAllByRole("listitem").map((item) => item.textContent)).toEqual(["Todo"]);
    if (phone) await user.click(screen.getByRole("button", { name: /Backlog/ }));
    else await user.unhover(screen.getByRole("button", { name: /Backlog/ }));
    const states = [...integration.states, { name: "Rework", terminal: false, dispatchable: true, transitions: ["Todo"] }];
    const markdown = "---\ntracker:\n  kind: hub_native\n  active_states: [Todo, Rework]\n---\nComplete the issue.\n";
    fireEvent.change(screen.getByRole("textbox", { name: "Workflow definition" }), { target: { value: markdown } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect((screen.getByRole("textbox", { name: "Workflow definition" }) as HTMLTextAreaElement).value).not.toBe(markdown);
    fireEvent.change(screen.getByRole("textbox", { name: "Workflow definition" }), { target: { value: markdown } });
    fireEvent.click(screen.getByRole("button", { name: "Save workflow" }));
    expect(onSave).toHaveBeenCalledWith(markdown);
    view.rerender(<WorkflowSettings integration={{ ...integration, states }} canManage={false} saving={false} error={null} onSave={onSave} />);
    expect(screen.queryByRole("textbox", { name: "Workflow definition" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save workflow" })).toBeNull();
    expect(screen.getByRole("button", { name: /Rework/ })).toBeTruthy();
    view.rerender(<WorkflowSettings integration={{ ...integration, states, authority: { workflow: "repository" }, checkout_repository: "example-org/example-project", workflow_source: "detent.yaml", workflow_source_revision: "abc123" }} canManage saving={false} error={null} onSave={onSave} />);
    expect(screen.queryByRole("textbox", { name: "Workflow definition" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save workflow" })).toBeNull();
    expect(screen.getByText(/Defined in/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "detent.yaml" }).getAttribute("href")).toBe("https://github.com/example-org/example-project/blob/abc123/detent.yaml");
    expect(screen.queryByText(/abc123/)).toBeNull();
    media.mockRestore();
    vi.unstubAllGlobals();
  });

  it("reads a successful policy response with nothing approved", () => {
    const result = Schema.decodeUnknownSync(PolicyApproval)(emptyApproval);
    expect(result.policy).toBeNull();
    expect(result.history).toEqual([]);
    expect(result.observed_policies).toEqual([]);
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
    isolationTier: "sandbox" as const,
    name: "",
    capacity: 1,
    service: false,
  };
  it.each([
    { name: "the minimum", change: {}, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc --isolation-tier sandbox" },
    { name: "a plain name", change: { name: "mac-studio" }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc --name mac-studio --isolation-tier sandbox" },
    { name: "a name with spaces", change: { name: " Build host " }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc --name 'Build host' --isolation-tier sandbox" },
    { name: "capacity and service", change: { capacity: 3, service: true }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1 --token det_enroll_abc --capacity 3 --isolation-tier sandbox --service" },
    { name: "a standalone hub", change: { hubUrl: "https://hub.example.com" }, want: "detent hub runner register --url https://hub.example.com --organization org_1 --token det_enroll_abc --isolation-tier sandbox" },
    { name: "a URL naming a longer organization ID", change: { hubUrl: "https://cloud.example.com/organizations/org_1-extra" }, want: "detent hub runner register --url https://cloud.example.com/organizations/org_1-extra --organization org_1 --token det_enroll_abc --isolation-tier sandbox" },
  ])("builds it for $name", ({ change, want }) => {
    expect(registerCommand({ ...input, ...change })).toBe(want);
  });

  it.each([
    { value: "plain", want: "plain" },
    { value: "two words", want: "'two words'" },
    { value: "Owner's Mac", want: "'Owner'\\''s Mac'" },
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
