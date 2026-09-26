// @vitest-environment jsdom
//
// The shared entry's screens: chooser, creation, provisioning progress and
// invitation joins, each against a scripted entry API, plus the entry router.
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createMemoryHistory } from "@tanstack/react-router";
import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AccountError } from "../../src/app/account/api.ts";
import {
  currentStepIndex,
  type EntryApi,
  makeEntryApi,
  PROVISIONING_STEPS,
  SIGN_IN_ORGANIZATIONS,
} from "../../src/app/entry/api.ts";
import {
  CreateOrganization,
  EntryApiContext,
  JoinInvitation,
  OrganizationChooser,
  ProvisioningProgress,
} from "../../src/app/entry/EntryScreens.tsx";
import { isEntrySurface, makeEntryRouter } from "../../src/app/entry/router.tsx";

if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

const assign = vi.fn();

beforeEach(() => {
  assign.mockReset();
  vi.stubGlobal("location", { assign, search: "", pathname: "/" });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const listing = {
  email: "dana@example.test",
  csrf: "csrf-token",
  organizations: [{ id: "org_a", name: "Alpha", url: "/organizations/org_a/organization" }],
  pending: [{ id: "org_b", name: "Beta", url: "/organizations/org_b/provisioning", state: "failed" }],
  can_create: true,
};

function fakeApi(overrides: Partial<EntryApi> = {}): EntryApi {
  return {
    organizations: vi.fn(async () => listing),
    session: vi.fn(async () => ({ email: listing.email, csrf: listing.csrf, can_create: true })),
    provisioning: vi.fn(async () => ({ id: "org_b", name: "Beta", state: "allocating", step: "admission", error: "", can_resume: false })),
    createOrganization: vi.fn(async () => ({ next: "/organizations/org_new/provisioning" })),
    resume: vi.fn(async () => ({ next: "/organizations/org_b/provisioning" })),
    joinInvitation: vi.fn(async () => ({ next: "/auth/oidc/start?organization=org_a" })),
    ...overrides,
  };
}

function renderWith(api: EntryApi, element: React.ReactElement) {
  return render(<EntryApiContext.Provider value={api}>{element}</EntryApiContext.Provider>);
}

describe("organization chooser", () => {
  it("lists organizations as links, pending ones with their state, and the actions", async () => {
    const navigate = vi.fn();
    renderWith(fakeApi(), <OrganizationChooser onNavigate={navigate} />);
    const link = await screen.findByRole("link", { name: /Alpha/ });
    expect(link.getAttribute("href")).toBe("/organizations/org_a/organization");
    expect(screen.getByText("Needs attention")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Beta/ }));
    expect(navigate).toHaveBeenCalledWith("/organizations/org_b/provisioning");
    fireEvent.click(screen.getByRole("button", { name: "Create organization" }));
    expect(navigate).toHaveBeenCalledWith("/organizations/new");
    const signOut = screen.getByRole("button", { name: "Sign out" }).closest("form");
    expect(signOut?.getAttribute("action")).toBe("/logout");
    expect((signOut?.querySelector('input[name="csrf"]') as HTMLInputElement).value).toBe("csrf-token");
  });

  it("hides creation where the entry does not offer it", async () => {
    renderWith(
      fakeApi({ organizations: vi.fn(async () => ({ ...listing, can_create: false, pending: [] })) }),
      <OrganizationChooser onNavigate={vi.fn()} />,
    );
    await screen.findByRole("link", { name: /Alpha/ });
    expect(screen.queryByRole("button", { name: "Create organization" })).toBeNull();
  });

  it("sends an expired session back to sign-in", async () => {
    renderWith(
      fakeApi({
        organizations: vi.fn(async () => {
          throw new AccountError({ status: 401, code: "unauthenticated", message: "Sign in" });
        }),
      }),
      <OrganizationChooser onNavigate={vi.fn()} />,
    );
    await waitFor(() => expect(assign).toHaveBeenCalledWith(SIGN_IN_ORGANIZATIONS));
  });
});

describe("create organization", () => {
  it("submits once with the session token and a stable key, then follows the next step", async () => {
    const api = fakeApi();
    const navigate = vi.fn();
    renderWith(api, <CreateOrganization onNavigate={navigate} />);
    await waitFor(() => expect(api.session).toHaveBeenCalled());
    fireEvent.change(screen.getByLabelText("Organization name"), { target: { value: "  Delta  " } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Create organization" }));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/organizations/org_new/provisioning"));
    const call = vi.mocked(api.createOrganization).mock.calls[0]![0];
    expect(call.name).toBe("Delta");
    expect(call.csrf).toBe("csrf-token");
    expect(call.key.length).toBeGreaterThanOrEqual(16);
  });

  it("shows the entry's refusal", async () => {
    renderWith(
      fakeApi({
        createOrganization: vi.fn(async () => {
          throw new AccountError({ status: 429, code: "quota_reached", message: "Your account has reached its organization limit." });
        }),
      }),
      <CreateOrganization onNavigate={vi.fn()} />,
    );
    fireEvent.change(screen.getByLabelText("Organization name"), { target: { value: "Delta" } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Create organization" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Create organization" }));
    expect((await screen.findByRole("alert")).textContent).toContain("organization limit");
  });
});

describe("provisioning progress", () => {
  it("shows a failed setup with its message and resumes it", async () => {
    const provisioning = vi
      .fn()
      .mockResolvedValueOnce({ id: "org_b", name: "Beta", state: "failed", step: "provider_organization", error: "Setup stopped (owner_membership_failed).", can_resume: true })
      .mockResolvedValue({ id: "org_b", name: "Beta", state: "allocating", step: "owner_membership", error: "", can_resume: false });
    const api = fakeApi({ provisioning });
    renderWith(api, <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} pollMs={10_000} />);
    expect(await screen.findByText("Setup stopped.")).toBeTruthy();
    expect(screen.getByText(/owner_membership_failed/)).toBeTruthy();
    const current = screen.getByText("Making you the owner").closest("li");
    expect(current?.getAttribute("aria-current")).toBe("step");
    await waitFor(() => expect((screen.getByRole("button", { name: "Resume setup" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Resume setup" }));
    await waitFor(() => expect(api.resume).toHaveBeenCalledWith({ organization: "org_b", csrf: "csrf-token" }));
    expect(await screen.findByText("Setting up…")).toBeTruthy();
  });

  it("opens the organization once it is ready", async () => {
    renderWith(
      fakeApi({ provisioning: vi.fn(async () => ({ id: "org_b", name: "Beta", state: "ready", step: "publish", error: "", can_resume: false, next: "/organizations/org_b/" })) }),
      <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} />,
    );
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/organizations/org_b/"));
  });

  it("keeps actions available when the organization list is unavailable", async () => {
    const api = fakeApi({
      organizations: vi.fn(async () => {
        throw new AccountError({ status: 503, code: "membership_unavailable", message: "down" });
      }),
    });
    renderWith(api, <JoinInvitation onNavigate={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Invitation token"), { target: { value: "inv_1" } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Join organization" }) as HTMLButtonElement).disabled).toBe(false));
  });

  it("never overlaps status reads when responses are slower than the poll interval", async () => {
    let inFlight = 0;
    let maxInFlight = 0;
    let calls = 0;
    const provisioning = vi.fn(async () => {
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      calls++;
      await new Promise((resolve) => globalThis.setTimeout(resolve, 30));
      inFlight--;
      return calls >= 3
        ? { id: "org_b", name: "Beta", state: "ready", step: "publish", error: "", can_resume: false, next: "/organizations/org_b/work" }
        : { id: "org_b", name: "Beta", state: "allocating", step: "admission", error: "", can_resume: false };
    });
    renderWith(fakeApi({ provisioning }), <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} pollMs={5} />);
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/organizations/org_b/work"));
    expect(maxInFlight).toBe(1);
  });

  it("polls while setup is running", async () => {
    const provisioning = vi.fn(async () => ({ id: "org_b", name: "Beta", state: "allocating", step: "admission", error: "", can_resume: false }));
    renderWith(fakeApi({ provisioning }), <ProvisioningProgress organization="org_b" onNavigate={vi.fn()} pollMs={20} />);
    await waitFor(() => expect(provisioning.mock.calls.length).toBeGreaterThan(2));
  });

  it("orders checkpoints from the last completed step", () => {
    expect(currentStepIndex("")).toBe(0);
    expect(currentStepIndex("admission")).toBe(1);
    expect(currentStepIndex("publish")).toBe(PROVISIONING_STEPS.length - 1);
  });
});

describe("join with invitation", () => {
  it("joins and follows the organization sign-in", async () => {
    const api = fakeApi();
    renderWith(api, <JoinInvitation onNavigate={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Invitation token"), { target: { value: " inv_1 " } });
    await waitFor(() => expect((screen.getByRole("button", { name: "Join organization" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Join organization" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/auth/oidc/start?organization=org_a"));
    expect(api.joinInvitation).toHaveBeenCalledWith({ token: "inv_1", csrf: "csrf-token" });
  });
});

describe("entry API", () => {
  it("reads JSON and posts forms with the CSRF token", async () => {
    const calls: { url: string; init: RequestInit | undefined }[] = [];
    const api = makeEntryApi({
      fetch: async (url, init) => {
        calls.push({ url, init });
        return new Response(JSON.stringify(url.startsWith("/api") ? listing : { next: "/next" }), { status: 200 });
      },
    });
    await api.organizations();
    await api.createOrganization({ name: "Delta", key: "key_0123456789abcdef", csrf: "csrf-token" });
    expect(calls[0]!.url).toBe("/api/cloud/organizations");
    expect((calls[0]!.init!.headers as Record<string, string>).Accept).toBe("application/json");
    expect(calls[1]!.url).toBe("/organizations");
    expect((calls[1]!.init!.headers as Record<string, string>)["X-CSRF-Token"]).toBe("csrf-token");
    const body = new URLSearchParams(calls[1]!.init!.body as string);
    expect(body.get("name")).toBe("Delta");
    expect(body.get("creation_key")).toBe("key_0123456789abcdef");
    expect(body.get("csrf")).toBe("csrf-token");
  });

  it("decodes the entry's error body", async () => {
    const api = makeEntryApi({
      fetch: async () => new Response(JSON.stringify({ code: "intent_conflict", message: "Different name" }), { status: 409 }),
    });
    await expect(api.createOrganization({ name: "x", key: "k", csrf: "c" })).rejects.toMatchObject({
      status: 409,
      code: "intent_conflict",
      message: "Different name",
    });
  });
});

describe("entry router", () => {
  it.each(["/organizations", "/organizations/new", "/organizations/org_b/provisioning", "/invitations/join", "/"])(
    "resolves %s",
    async (path) => {
      const router = makeEntryRouter(createMemoryHistory({ initialEntries: [path] }));
      await router.load();
      expect(router.state.location.pathname).toBe(path);
    },
  );

  it("sends unknown paths to the chooser", async () => {
    const router = makeEntryRouter(createMemoryHistory({ initialEntries: ["/elsewhere"] }));
    await router.load();
    expect(router.state.location.pathname).toBe("/organizations");
  });

  it("recognizes the entry surface marker", () => {
    const meta = (content: string | null) => ({
      querySelector: () => (content === null ? null : { getAttribute: () => content }),
    });
    expect(isEntrySurface(meta("entry") as never)).toBe(true);
    expect(isEntrySurface(meta(null) as never)).toBe(false);
  });
});

describe("organization client behind the shared entry", () => {
  it("knows its organization from the base path", async () => {
    const { behindSharedEntry, currentSharedOrganization } = await import("../../src/app/entry/shared.ts");
    expect(behindSharedEntry("/organizations/org_a")).toBe(true);
    expect(behindSharedEntry("")).toBe(false);
    expect(currentSharedOrganization("/organizations/org_a")).toBe("org_a");
    expect(currentSharedOrganization("")).toBe("");
  });
});
