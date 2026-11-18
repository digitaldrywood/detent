// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { makeAccountApi } from "../../src/app/account/api.ts";
import { SpritePoolCard } from "../../src/app/account/SpritePoolCard.tsx";
import { SpritesCard } from "../../src/app/account/SpritesCard.tsx";

vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api, useAccountBootstrap: () => account }));

const TOKEN = "detent-test/org/id/VALUE-SENTINEL";
let present = false;
let refuse = false;
let poolStatus = 200;
let poolWriteStatus = 200;
let account: { organization: { name: string }; projects: { id: string; name: string }[]; base_path: string } | null = null;
const writes: RequestInit[] = [];
let pool = { min_runners: 0, max_runners: 0, idle_seconds: 300, bootstrap: "", revision: 0, share_idle_capacity: false, members: [] };
const api = makeAccountApi({
  origin: "", apiBase: "/api/v2/organizations/test", csrfToken: "csrf-sentinel",
  fetch: async (url, init) => {
    if (url.endsWith("/sprite-pool")) {
      expect(url).toBe("/api/v2/organizations/test/sprite-pool");
      if (init?.method === "PUT") {
        writes.push(init);
        if (poolWriteStatus !== 200) return new Response(JSON.stringify({ code: "forbidden", message: TOKEN }), { status: poolWriteStatus });
        pool = { ...pool, ...JSON.parse(String(init.body)), revision: pool.revision + 1 };
      }
      return new Response(JSON.stringify(poolStatus === 200 ? pool : { code: "unavailable", message: TOKEN }), { status: poolStatus });
    }
    if (url === "/api/v2/organizations/test/secrets/fly_sprites_token") return new Response(JSON.stringify({ kind: "fly_sprites_token", present }));
    expect(url).toBe("/api/v2/organizations/test/projects/project/secrets/fly_sprites_token");
    if (init?.method !== "GET") {
      writes.push(init!);
      expect((init?.headers as Record<string, string>)["X-CSRF-Token"]).toBe("csrf-sentinel");
      if (refuse) throw new Error(TOKEN);
      present = init?.method === "PUT";
    }
    return new Response(JSON.stringify({ kind: "fly_sprites_token", present, ...(present ? { organization_slug: "detent-test", key_version: 1 } : {}) }), { status: 200 });
  },
});
afterEach(() => { cleanup(); poolStatus = 200; poolWriteStatus = 200; account = null; });

describe("Sprites credential input", () => {
  it.each([false, true])("clears the write-only field when refusal is %s", async (rejected) => {
    present = false; refuse = rejected; writes.length = 0;
    render(<SpritesCard projectId="project" canManage />);
    await screen.findByText("Using organization token");
    expect(screen.queryByLabelText("Sprites organization token")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Set override" }));
    const input = screen.getByLabelText("Sprites organization token") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.autocomplete).toBe("off");
    fireEvent.change(input, { target: { value: TOKEN } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(input.value).toBe("");
    if (rejected) {
      await screen.findByRole("alert");
      expect(document.body.textContent).not.toContain(TOKEN);
    } else {
      await screen.findByText(/Connected to detent-test/);
      expect(screen.getByRole("button", { name: "Replace token" })).toBeTruthy();
      expect(screen.queryByLabelText("Sprites organization token")).toBeNull();
      fireEvent.click(screen.getByRole("button", { name: "Replace token" }));
      const replacement = screen.getByLabelText("Sprites organization token") as HTMLInputElement;
      expect(replacement.value).toBe("");
      fireEvent.change(replacement, { target: { value: TOKEN + "-replacement" } });
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await waitFor(() => expect(writes.length).toBe(2));
      await waitFor(() => expect(screen.getByRole("button", { name: "Remove" }).hasAttribute("disabled")).toBe(false));
      expect(input.value).toBe("");
      fireEvent.click(screen.getByRole("button", { name: "Remove" }));
      await screen.findByText("Using organization token");
      expect(writes.map((entry) => entry.method)).toEqual(["PUT", "PUT", "DELETE"]);
      expect(document.body.textContent).not.toContain(TOKEN);
    }
    expect(JSON.parse(writes[0]!.body as string)).toEqual({ token: TOKEN });
  });

  it("shows the binding without secret controls to a reader", async () => {
    present = true; refuse = false;
    render(<SpritesCard projectId="project" canManage={false} />);
    await screen.findByText(/Connected to detent-test/);
    expect(screen.queryByLabelText("Sprites organization token")).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
  });
});

describe("Sprite pool settings", () => {
  it.each(["project", "other-project"])("uses organization settings and token from %s", async (projectId) => {
    present = false;
    account = { organization: { name: "Example Organization" }, projects: [{ id: "project", name: "First project" }, { id: "other-project", name: "Second project" }], base_path: "/organizations/test" };
    pool = { min_runners: 1, max_runners: 3, idle_seconds: 600, bootstrap: "printf saved", revision: 7, share_idle_capacity: projectId === "other-project", members: [] };
    render(<SpritePoolCard projectId={projectId} canManage={false} />);
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("No organization Sprites token is set"));
    expect(screen.getByText(/Example Organization shares this pool across all projects/).textContent).toContain(projectId === "project" ? "First project" : "Second project");
    expect(screen.getByRole("link", { name: "Set a Sprites token" }).getAttribute("href")).toBe("/organizations/test/settings/integrations#sprites");
    expect(screen.getByRole("link", { name: "View members" }).getAttribute("href")).toBe("/organizations/test/settings/organization/members");
    present = true;
    fireEvent.click(screen.getByRole("button", { name: "Reload pool" }));
    await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
    expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3");
    const sharing = screen.getByRole("checkbox", { name: "Share idle Sprites with other projects" });
    expect(sharing.getAttribute("aria-disabled")).toBe("true");
    expect(sharing.getAttribute("aria-checked")).toBe(String(pool.share_idle_capacity));
  });

  it.each([409, 503])("reloads saved settings after a failed PUT returning %s", async (status) => {
    writes.length = 0;
    pool = { min_runners: 1, max_runners: 3, idle_seconds: 600, bootstrap: "printf saved", revision: 7, share_idle_capacity: false, members: [] };
    render(<SpritePoolCard projectId="project" canManage />);
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3"));
    poolWriteStatus = status;
    fireEvent.change(screen.getByLabelText("Maximum runners"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save pool" }));
    expect((await screen.findByRole("alert")).textContent).toContain(status === 409 ? "pool settings changed" : "Could not save the pool");
    expect(screen.queryByLabelText("Maximum runners")).toBeNull();
    expect(document.body.textContent).not.toContain(TOKEN);
    poolWriteStatus = 200;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3"));
    fireEvent.click(screen.getByRole("button", { name: "Save pool" }));
    await waitFor(() => expect(writes.length).toBe(2));
    expect(JSON.parse(String(writes[1]!.body))).toEqual({ min_runners: 1, max_runners: 3, idle_seconds: 600, bootstrap: "printf saved", revision: 7, share_idle_capacity: false, idempotency_key: expect.any(String) });
    await waitFor(() => expect(screen.getByRole("button", { name: "Save pool" }).hasAttribute("disabled")).toBe(false));
  });

  it.each([403, 404, 503])("recovers an organization pool GET returning %s", async (status) => {
    pool = { min_runners: 1, max_runners: 3, idle_seconds: 600, bootstrap: "printf saved", revision: 7, share_idle_capacity: false, members: [] };
    poolStatus = status;
    render(<SpritePoolCard projectId="project" canManage />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(status === 403 ? "Owner or admin access" : "Could not load the pool");
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(screen.queryByLabelText("Maximum runners")).toBeNull();
    poolStatus = 200;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3"));
    expect((screen.getByLabelText("Extra bootstrap (optional)") as HTMLTextAreaElement).value).toBe("printf saved");
  });

  it.each([false, true])("keeps saved settings read-only when write permission is missing after save %s", async (afterSave) => {
    writes.length = 0;
    pool = { min_runners: 1, max_runners: 3, idle_seconds: 600, bootstrap: "printf saved", revision: 7, share_idle_capacity: false, members: [] };
    render(<SpritePoolCard projectId="project" canManage={afterSave} />);
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3"));
    if (afterSave) {
      poolWriteStatus = 403;
      fireEvent.change(screen.getByLabelText("Maximum runners"), { target: { value: "5" } });
      fireEvent.click(screen.getByRole("button", { name: "Save pool" }));
    }
    expect((await screen.findByRole("alert")).textContent).toContain("Owner or admin access");
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3"));
    expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Save pool" })).toBeNull();
    pool = { ...pool, max_runners: 4, revision: 8 };
    fireEvent.click(screen.getByRole("button", { name: "Reload pool" }));
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("4"));
    expect(screen.getByRole("alert").textContent).toContain("Owner or admin access");
    expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Save pool" })).toBeNull();
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(writes.length).toBe(afterSave ? 1 : 0);
  });

  it("restores saved settings on reload even when the revision is unchanged", async () => {
    pool = { min_runners: 1, max_runners: 3, idle_seconds: 600, bootstrap: "printf saved", revision: 7, share_idle_capacity: false, members: [] };
    render(<SpritePoolCard projectId="project" canManage />);
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3"));
    fireEvent.change(screen.getByLabelText("Maximum runners"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Reload pool" }));
    await waitFor(() => expect((screen.getByLabelText("Maximum runners") as HTMLInputElement).value).toBe("3"));
    expect((screen.getByLabelText("Extra bootstrap (optional)") as HTMLTextAreaElement).value).toBe("printf saved");
  });

  it.each(["", "printf setup"])("enables a pool with bootstrap %j", async (bootstrap) => {
    writes.length = 0;
    pool = { min_runners: 0, max_runners: 0, idle_seconds: 300, bootstrap, revision: 0, share_idle_capacity: false, members: [] };
    render(<SpritePoolCard projectId="project" canManage />);
    await screen.findByRole("button", { name: "Save pool" });
    await waitFor(() => expect((screen.getByLabelText("Extra bootstrap (optional)") as HTMLTextAreaElement).value).toBe(bootstrap));
    fireEvent.change(screen.getByLabelText("Maximum runners"), { target: { value: "2" } });
    fireEvent.change(screen.getByLabelText("Minimum runners"), { target: { value: "2" } });
    expect((screen.getByLabelText("Extra bootstrap (optional)") as HTMLTextAreaElement).required).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Save pool" }));
    await waitFor(() => expect(writes.length).toBe(1));
    expect(JSON.parse(String(writes[0]!.body))).toEqual({ min_runners: 2, max_runners: 2, idle_seconds: 300, bootstrap, revision: 0, share_idle_capacity: false, idempotency_key: expect.any(String) });
    await waitFor(() => expect(screen.getByRole("button", { name: "Save pool" }).hasAttribute("disabled")).toBe(false));
  });
});
