// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import { RunnersSettings } from "../../src/app/fleet/RunnersSection.tsx";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import type { SpritePool, ProjectSpriteAllocation } from "../../src/contracts/account.ts";
import { ProjectSpriteControls } from "../../src/app/account/ProjectSpriteControls.tsx";
import fleet from "../../src/contracts/fixtures/account-fleet.json";

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
});

const initialAllocation = { project_id: "project_example", managed: true, desired_count: 2, allocated_count: 2, shortfall: 0, provision_failures: 0, revision: "7" };

function mountProjectControls(options: {
  canManage?: boolean;
  allocation?: typeof ProjectSpriteAllocation.Type;
  pool?: typeof SpritePool.Type;
  token?: boolean;
  mutate?: (input: Record<string, unknown>) => Promise<Response>;
} = {}) {
  let allocation = options.allocation ?? initialAllocation;
  let pool = options.pool ?? { ...emptyPool, max_runners: 4 };
  const writes: Record<string, unknown>[] = [];
  const paths: string[] = [];
  const fetcher = vi.fn(async (input: string, init?: RequestInit) => {
    paths.push(String(input));
    if (String(input).endsWith("/sprite-pool/allocation")) {
      if (init?.method === "PUT") {
        const body = JSON.parse(String(init.body));
        writes.push(body);
        if (options.mutate) return options.mutate(body);
        expect(body.expected_revision).toBe(allocation.revision);
        expect(init.headers).toMatchObject({ "X-CSRF-Token": "csrf" });
        allocation = { ...allocation, managed: true, desired_count: body.desired_count, revision: String(Number(allocation.revision) + 1), provision_failures: 0 };
      }
      return new Response(JSON.stringify(allocation));
    }
    if (String(input).endsWith("/sprite-pool")) return new Response(JSON.stringify(pool));
    if (String(input).endsWith("/secrets/fly_sprites_token")) return new Response(JSON.stringify({ kind: "fly_sprites_token", present: options.token ?? true }));
    throw new Error(`Unexpected request ${input}`);
  });
  vi.stubGlobal("fetch", fetcher);
  const client = {
    account: {
      organization: { id: "org_example", name: "Example organization" },
      projects: [{ id: "project_example", name: "Example project", can_write: options.canManage ?? true }],
      base_path: "/example-org",
    },
    http: { origin: "", apiBase: "/api/v2/organizations/org_example", csrfToken: "csrf" },
  } as unknown as ConversationClient;
  const router = createRouter({
    routeTree: createRootRoute({ component: () => <ProjectSpriteControls projectId="project_example" canManage={options.canManage ?? true} /> }),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(<ClientContext.Provider value={client}><RouterProvider router={router as never} /></ClientContext.Provider>);
  return { writes, paths, setAllocation: (value: typeof allocation) => { allocation = value; }, setPool: (value: typeof pool) => { pool = value; } };
}

describe("Project Sprite count controls", () => {
  it("uses the project command for increment, decrement, Off and reload", async () => {
    const f = mountProjectControls();
    await screen.findByText("Desired 2 · Actual 2 · Ready 0");
    fireEvent.click(screen.getByRole("button", { name: "Increase project Sprites" }));
    await screen.findByText("Desired 3 · Actual 2 · Ready 0");
    fireEvent.click(screen.getByRole("button", { name: "Decrease project Sprites" }));
    await screen.findByText("Desired 2 · Actual 2 · Ready 0");
    fireEvent.click(screen.getByRole("button", { name: "Off" }));
    await screen.findByText("Desired 0 · Actual 2 · Ready 0");
    expect(screen.getByText(/Retained checkpoints or other provider storage/)).toBeDefined();
    expect((screen.getByRole("button", { name: "Decrease project Sprites" }) as HTMLButtonElement).disabled).toBe(true);
    expect(f.writes.map((body) => body.desired_count)).toEqual([3, 2, 0]);
    expect(f.writes.map((body) => body.expected_revision)).toEqual(["7", "8", "9"]);
    expect(new Set(f.writes.map((body) => body.idempotency_key)).size).toBe(3);
    expect(f.paths.every((path) => path.includes("/projects/project_example/") || path.endsWith("/secrets/fly_sprites_token"))).toBe(true);
    f.setAllocation({ ...initialAllocation, revision: "11", desired_count: 1 });
    fireEvent.click(screen.getByRole("button", { name: "Reload Sprites" }));
    await screen.findByText("Desired 1 · Actual 2 · Ready 0");
  });

  it.each([false, true])("preserves read-only and unprepared states (canManage=%s)", async (canManage) => {
    const f = mountProjectControls({ canManage, token: false, pool: emptyPool, allocation: { ...initialAllocation, managed: false, desired_count: 0, revision: "0" } });
    await screen.findByText("Desired 0 · Actual 2 · Ready 0");
    expect((screen.getByRole("button", { name: "Increase project Sprites" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByRole("link", { name: "Set the organization Sprites token" }).getAttribute("href")).toBe("/example-org/settings/integrations#sprites");
    expect(screen.getByText(/Subscription logins cannot be copied/)).toBeDefined();
    expect(screen.getByText(/Provider billing stays as configured/)).toBeDefined();
    expect(f.writes).toEqual([]);
  });

  it("does not offer writes to a non-manager on a prepared project", async () => {
    const f = mountProjectControls({ canManage: false });
    await screen.findByText("Desired 2 · Actual 2 · Ready 0");
    for (const name of ["Increase project Sprites", "Decrease project Sprites", "Off"]) {
      expect((screen.getByRole("button", { name }) as HTMLButtonElement).disabled).toBe(true);
    }
    expect(f.writes).toEqual([]);
  });

  it("coalesces repeated clicks and retries an uncertain result with the original key", async () => {
    let rejectRequest!: (reason: Error) => void;
    const pending = new Promise<Response>((_resolve, reject) => { rejectRequest = reject; });
    const mutate = vi.fn().mockImplementationOnce(() => pending).mockResolvedValueOnce(new Response(JSON.stringify({ ...initialAllocation, desired_count: 3, revision: "8" })));
    const f = mountProjectControls({ mutate });
    const increase = await screen.findByRole("button", { name: "Increase project Sprites" });
    await waitFor(() => expect((increase as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(increase);
    fireEvent.click(increase);
    expect(f.writes).toHaveLength(1);
    rejectRequest(new TypeError("Connection lost"));
    fireEvent.click(await screen.findByRole("button", { name: "Retry request" }));
    await waitFor(() => expect(f.writes).toHaveLength(2));
    expect(f.writes[1]).toEqual(f.writes[0]);
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });

  it.each([403, 409])("requires reload after a refused write (%s)", async (status) => {
    const f = mountProjectControls({ mutate: async () => new Response(JSON.stringify({ code: "refused", message: "Access or revision changed" }), { status }) });
    await screen.findByText("Desired 2 · Actual 2 · Ready 0");
    fireEvent.click(screen.getByRole("button", { name: "Increase project Sprites" }));
    await screen.findByRole("alert");
    expect((screen.getByRole("button", { name: "Increase project Sprites" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Retry request" })).toBeNull();
    if (status === 409) expect(screen.getByText(/Reload before choosing a new total/)).toBeDefined();
    f.setAllocation({ ...initialAllocation, desired_count: 4, revision: "9" });
    fireEvent.click(screen.getByRole("button", { name: "Reload Sprites" }));
    await screen.findByText("Desired 4 · Actual 2 · Ready 0");
  });

  it("shows only this project's readiness, exact login commands and draining progress", async () => {
    const login = { ...member, name: "example-login", allocation_project_id: "project_example", status: "awaiting_login", waiting_for: [{ check: "provider", command: "claude auth login" }, { check: "github", command: "gh auth login" }, { check: "github", command: "gh auth setup-git" }] };
    const f = mountProjectControls({ pool: { ...emptyPool, max_runners: 5, members: [login, { ...member, name: "example-ready", allocation_project_id: "project_example", status: "ready" }, { ...member, name: "other-project", allocation_project_id: "project_other", status: "ready" }] } });
    await screen.findByText("Desired 2 · Actual 2 · Ready 1");
    expect(screen.getByText("claude auth login")).toBeDefined();
    expect(screen.getByText("gh auth login")).toBeDefined();
    expect(screen.getByText("gh auth setup-git")).toBeDefined();
    expect(screen.getByText(/stays not ready until both/)).toBeDefined();
    expect(screen.queryByText(/other-project/)).toBeNull();
    f.setPool({ ...emptyPool, max_runners: 5, members: [{ ...login, state: "deleting", status: "draining", waiting_for: [] }] });
    fireEvent.click(screen.getByRole("button", { name: "Off" }));
    await screen.findByText("Desired 0 · Actual 2 · Ready 0 · Draining 1");
    expect(screen.getByText(/finish active work and publish durable recovery/)).toBeDefined();
  });

  it("recovers partial provisioning failures using the same desired count", async () => {
    const f = mountProjectControls({ allocation: { ...initialAllocation, allocated_count: 1, shortfall: 1, provision_failures: 3 }, pool: { ...emptyPool, max_runners: 4, members: [{ ...member, allocation_project_id: "project_example", status: "unreachable" }] } });
    await screen.findByText(/Shortfall 1:/);
    expect(screen.getByText(/Unreachable · recovery needed/)).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Retry provisioning" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Retry provisioning" })).toBeNull());
    expect(f.writes[0]).toMatchObject({ desired_count: 2, expected_revision: "7" });
  });
});

const member = { name: "build-sprite", state: "enrolled", runner_id: "runner_build", bootstrap_log: "", idle_since: "" };
const emptyPool = { min_runners: 0, max_runners: 0, idle_seconds: 300, bootstrap: "", revision: 1, members: [] as typeof member[] };

function mountPool(pool: typeof SpritePool.Type = emptyPool, token: boolean | "failed" = false, saveError?: string) {
  vi.stubGlobal("fetch", vi.fn(async (input: string, init?: RequestInit) => {
    if (String(input).endsWith("/sprite-pool")) {
      if (init?.method === "PUT" && saveError) return new Response(JSON.stringify({ code: "invalid_request", message: saveError }), { status: 422 });
      if (init?.method === "PUT") pool = { ...pool, ...JSON.parse(String(init.body)), revision: pool.revision + 1 };
      return new Response(JSON.stringify(pool));
    }
    if (String(input).endsWith("/secrets/fly_sprites_token")) {
      if (token === "failed") return new Response("{}", { status: 503 });
      return new Response(JSON.stringify({ kind: "fly_sprites_token", present: token }));
    }
    return new Response(JSON.stringify({ ...fleet, runners: [{ ...fleet.runners[0], health: "online" }] }));
  }));
  const client = {
    account: {
      organization: { id: "org_build", name: "Build" },
      actor: { can_manage: true, can_manage_runners: false },
      projects: [{ id: "project_build", name: "detent.build", can_write: true }, { id: "project_detent", name: "detent", can_write: true }],
      base_path: "/organizations/org_build",
    },
    http: { origin: "", apiBase: "/api/v2/organizations/org_build", csrfToken: "csrf" },
  } as unknown as ConversationClient;
  const router = createRouter({
    routeTree: createRootRoute({ component: RunnersSettings }),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  render(<ClientContext.Provider value={client}><RouterProvider router={router as never} /></ClientContext.Provider>);
}

describe("Sprite pool setup and attention", () => {
  it("explains an unsupported setup instead of hiding the preparation requirement", async () => {
    const message = "Sprite pools require hosted configuration and the project secret store";
    mountPool(emptyPool, true, message);
    fireEvent.click(await screen.findByRole("button", { name: "Set up Sprites" }));
    fireEvent.change(await screen.findByLabelText("Maximum runners"), { target: { value: "4" } });
    fireEvent.click(screen.getByRole("button", { name: "Save pool" }));
    expect(await screen.findByText(message)).toBeDefined();
  });

  it.each([
    { name: "unused without a token", pool: emptyPool, token: false, state: "not_set_up" },
    { name: "unused with a token", pool: emptyPool, token: true, state: "not_set_up" },
    { name: "unused with a failed check", pool: emptyPool, token: "failed", state: "not_set_up" },
    { name: "only deleted members", pool: { ...emptyPool, members: [{ ...member, state: "deleted" }] }, token: false, state: "not_set_up" },
    { name: "enabled without a token", pool: { ...emptyPool, max_runners: 4 }, token: false, state: "missing" },
    { name: "enabled with a failed check", pool: { ...emptyPool, max_runners: 4 }, token: "failed", state: "failed" },
    { name: "zero ceiling with an enrolled member", pool: { ...emptyPool, members: [member] }, token: false, state: "missing" },
    { name: "enabled and healthy", pool: { ...emptyPool, max_runners: 4 }, token: true, state: "healthy" },
  ] as const)("$name", async ({ pool, token, state }) => {
    mountPool(pool, token);
    const row = await screen.findByTestId("sprite-pool-row");
    const warning = state === "missing" || state === "failed";
    await waitFor(() => expect(row.textContent).not.toContain("Checking Sprites token"));
    await waitFor(() => expect(screen.getByRole("link", { name: `Needs attention ${warning ? 1 : 0}` })).toBeDefined());
    const controls = within(row);
    if (state === "not_set_up") {
      expect(row.textContent).toContain("Not set up. Sprites start runners for every project when work is queued.");
      expect(row.textContent).not.toContain("Floor");
      expect(row.querySelector(".text-warning-foreground, .text-destructive, [role=alert]")).toBeNull();
      fireEvent.click(controls.getByRole("button", { name: "Set up Sprites" }));
      const dialog = await screen.findByRole("dialog", { name: "Organization Sprite pool" });
      expect((within(dialog).getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("0");
      expect(within(dialog).queryByRole("alert")).toBeNull();
      await userEvent.keyboard("{Escape}");
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    } else {
      expect(controls.getByRole("button", { name: "Configure pool" })).toBeDefined();
      expect(row.textContent).toContain(`Floor 0 · Ceiling ${pool.max_runners} · Idle 300s · ${pool.members.length} members`);
      if (warning) {
        expect(row.textContent).toContain(state === "missing"
          ? "No organization Sprites token is set, so the Hub cannot start Sprites."
          : "Could not check the organization’s Sprites token.");
        expect(controls.getByRole("link", { name: state === "missing" ? "Set a Sprites token" : "Review the token" }).getAttribute("href"))
          .toBe("/organizations/org_build/settings/integrations#sprites");
        fireEvent.click(controls.getByRole("button", { name: "Configure pool" }));
        const dialog = await screen.findByRole("dialog");
        expect(within(dialog).getByRole("status").textContent).toContain(state === "missing" ? "No organization Sprites token is set" : "Could not check the organization’s Sprites token");
        expect(within(dialog).getByRole("link", { name: state === "missing" ? "Set a Sprites token" : "Review the token" }).getAttribute("href")).toBe("/organizations/org_build/settings/integrations#sprites");
        expect((within(dialog).getByLabelText("Maximum runners") as HTMLInputElement).value).toBe(String(pool.max_runners));
        await userEvent.keyboard("{Escape}");
        await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      } else {
        expect(row.querySelector(".text-warning-foreground")).toBeNull();
      }
    }
    fireEvent.click(screen.getByRole("link", { name: `Needs attention ${warning ? 1 : 0}` }));
    await waitFor(() => expect(screen.queryByTestId("sprite-pool-row") !== null).toBe(warning));
  });

  it("updates attention after disabling an enabled pool", async () => {
    mountPool({ ...emptyPool, max_runners: 4 });
    await screen.findByRole("link", { name: "Needs attention 1" });
    fireEvent.click(screen.getByRole("button", { name: "Configure pool" }));
    fireEvent.change(await screen.findByLabelText("Maximum runners"), { target: { value: "0" } });
    fireEvent.click(screen.getByRole("button", { name: "Save pool" }));
    await waitFor(() => expect(screen.getByTestId("sprite-pool-row").textContent).toContain("Not set up."));
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await screen.findByRole("link", { name: "Needs attention 0" });
    expect(screen.getByTestId("sprite-pool-row").textContent).toContain("Not set up.");
  });
});
