// @vitest-environment jsdom
//
// The account screens' presentational contracts: the login card, the wizard
// stepper and the organization table. (The settings navigation moved into the
// window's sidebar — `tests/components/settings.test.tsx` covers it.) Each is
// rendered
// on its own with plain props, so what is under test is the screen's own
// behaviour rather than the client it would otherwise be wired to.
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Member, OnboardingStep } from "../../src/contracts/account.ts";
import {
  LoginCard,
  loginErrorMessage,
  OIDC_START,
  OIDC_START_UNSCOPED,
} from "../../src/app/account/Login.tsx";
import {
  grantFor,
  InviteForm,
  MembersTable,
  OrganizationSwitcher,
  refusalMessage,
  SupportBanner,
} from "../../src/app/account/Organization.tsx";
import { firstUnreadyStep, orderedSteps, Stepper } from "../../src/app/account/Setup.tsx";
import { AccountError } from "../../src/app/account/api.ts";
import { noApprovedPolicy, saveMessage } from "../../src/app/account/ProjectSettings.tsx";
import { allowanceRows, allowanceLabel } from "../../src/app/settings/Settings.tsx";

afterEach(cleanup);

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

const PROJECTS = [{ id: "proj_parable", name: "parable" }];

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
  it("offers both ways in, pointed at the hub's own start", () => {
    render(<LoginCard />);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Sign in to Detent");
    expect(screen.getByRole("link", { name: "Continue with WorkOS" }).getAttribute("href")).toBe(
      OIDC_START,
    );
    expect(screen.getByRole("link", { name: "Join with invitation" }).getAttribute("href")).toBe(
      OIDC_START_UNSCOPED,
    );
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

  it("submits the invitation token through the callback when a session exists", () => {
    const onAcceptInvitation = vi.fn();
    render(<LoginCard onAcceptInvitation={onAcceptInvitation} />);
    fireEvent.change(screen.getByLabelText("Have an invitation token?"), {
      target: { value: " inv_abc " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Join" }));
    expect(onAcceptInvitation).toHaveBeenCalledWith("inv_abc");
  });

  it("refuses to submit an empty token", () => {
    const onAcceptInvitation = vi.fn();
    render(<LoginCard onAcceptInvitation={onAcceptInvitation} />);
    fireEvent.click(screen.getByRole("button", { name: "Join" }));
    expect(onAcceptInvitation).not.toHaveBeenCalled();
  });
});

describe("the members table", () => {
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

  it("does not offer ownership to an admin, because the hub refuses it", () => {
    renderMembers({ actorRole: "admin" });
    const select = screen.getByLabelText("Role for sam@example.test") as HTMLSelectElement;
    const owner = within(select).getByRole("option", { name: "Owner" }) as HTMLOptionElement;
    expect(owner.disabled).toBe(true);
  });

  it("reports a grant change as an upsert and an emptied grant as a revoke", () => {
    const { onGrantChange } = renderMembers();
    fireEvent.click(
      screen.getByLabelText("Write access to parable for sam@example.test"),
    );
    expect(onGrantChange).toHaveBeenCalledWith(MEMBERS[1], "proj_parable", {
      project_id: "proj_parable",
      write: true,
      runner: false,
    });
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
  it("reads the absent grant as no access rather than undefined", () => {
    expect(grantFor(MEMBERS[1]!, "proj_parable")).toEqual({
      project_id: "proj_parable",
      write: false,
      runner: false,
    });
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

  it("sends the invitation with the role that was chosen", () => {
    const onInvite = vi.fn();
    render(<InviteForm onInvite={onInvite} />);
    fireEvent.change(screen.getByLabelText("Email"), { target: { value: "rae@example.test" } });
    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "admin" } });
    fireEvent.click(screen.getByRole("button", { name: "Send invitation" }));
    expect(onInvite).toHaveBeenCalledWith({ email: "rae@example.test", role: "admin" });
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
      "Repository configuration",
      "Local validation",
      "Execution runner",
      "Artifact history",
    ]);
    expect(ordered[0]?.state).toBe("action_required");
    expect(ordered[2]?.state).toBe("ready");
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
