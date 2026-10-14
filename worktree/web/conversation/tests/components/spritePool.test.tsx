// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ClientContext } from "../../src/app/client.ts";
import { RunnersSettings } from "../../src/app/fleet/RunnersSection.tsx";
import type { ConversationClient } from "../../src/runtime/bootstrap.ts";
import type { SpritePool } from "../../src/contracts/account.ts";
import fleet from "../../src/contracts/fixtures/account-fleet.json";

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
});

const member = { name: "build-sprite", state: "enrolled", runner_id: "runner_build", bootstrap_log: "", idle_since: "" };
const emptyPool = { min_runners: 0, max_runners: 0, idle_seconds: 300, bootstrap: "", revision: 1, members: [] as typeof member[] };

function mountPool(pool: typeof SpritePool.Type = emptyPool, token: boolean | "failed" = false) {
  vi.stubGlobal("fetch", vi.fn(async (input: string, init?: RequestInit) => {
    if (String(input).endsWith("/sprite-pool")) {
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
      const setup = controls.getByRole(token === true ? "button" : "link", { name: "Set up Sprites" });
      if (token === true) {
        fireEvent.click(setup);
        expect(await screen.findByRole("dialog", { name: "Organization Sprite pool" })).toBeDefined();
        await userEvent.keyboard("{Escape}");
        await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
      } else {
        expect(setup.getAttribute("href")).toBe("/organizations/org_build/settings/integrations#sprites");
      }
    } else {
      expect(controls.getByRole("button", { name: "Configure pool" })).toBeDefined();
      expect(row.textContent).toContain(`Floor 0 · Ceiling ${pool.max_runners} · Idle 300s · ${pool.members.length} members`);
      if (warning) {
        expect(row.textContent).toContain(state === "missing"
          ? "No organization Sprites token is set, so the Hub cannot start Sprites."
          : "Could not check the organization’s Sprites token.");
        expect(controls.getByRole("link", { name: state === "missing" ? "Set a Sprites token" : "Review the token" }).getAttribute("href"))
          .toBe("/organizations/org_build/settings/integrations#sprites");
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
