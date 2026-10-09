// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { makeAccountApi } from "../../src/app/account/api.ts";
import { SpritePoolCard } from "../../src/app/account/SpritePoolCard.tsx";
import { SpritesCard } from "../../src/app/account/SpritesCard.tsx";

vi.mock("../../src/app/account/context.ts", () => ({ useAccountApi: () => api, useAccountBootstrap: () => null }));

const TOKEN = "detent-test/org/id/VALUE-SENTINEL";
let present = false;
let refuse = false;
const writes: RequestInit[] = [];
let pool = { min_runners: 0, max_runners: 0, idle_seconds: 300, bootstrap: "", revision: 0, members: [] };
const api = makeAccountApi({
  origin: "", apiBase: "/api/v2/organizations/test", csrfToken: "csrf-sentinel",
  fetch: async (url, init) => {
    if (url.endsWith("/sprite-pool")) {
      if (init?.method === "PUT") {
        writes.push(init);
        pool = { ...pool, ...JSON.parse(String(init.body)), revision: pool.revision + 1 };
      }
      return new Response(JSON.stringify(pool));
    }
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
afterEach(cleanup);

describe("Sprites credential input", () => {
  it.each([false, true])("clears the write-only field when refusal is %s", async (rejected) => {
    present = false; refuse = rejected; writes.length = 0;
    render(<SpritesCard projectId="project" canManage />);
    await screen.findByText("No token set");
    const input = screen.getByLabelText("Sprites organization token") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(input.autocomplete).toBe("off");
    fireEvent.change(input, { target: { value: TOKEN } });
    fireEvent.click(screen.getByRole("button", { name: "Set token" }));
    expect(input.value).toBe("");
    if (rejected) {
      await screen.findByRole("alert");
      expect(document.body.textContent).not.toContain(TOKEN);
    } else {
      await screen.findByText("Connected to detent-test");
      expect(screen.getByRole("button", { name: "Replace token" })).toBeTruthy();
      fireEvent.change(input, { target: { value: TOKEN + "-replacement" } });
      fireEvent.click(screen.getByRole("button", { name: "Replace token" }));
      await waitFor(() => expect(writes.length).toBe(2));
      await waitFor(() => expect(screen.getByRole("button", { name: "Remove" }).hasAttribute("disabled")).toBe(false));
      expect(input.value).toBe("");
      fireEvent.click(screen.getByRole("button", { name: "Remove" }));
      await screen.findByText("No token set");
      expect(writes.map((entry) => entry.method)).toEqual(["PUT", "PUT", "DELETE"]);
      expect(document.body.textContent).not.toContain(TOKEN);
    }
    expect(JSON.parse(writes[0]!.body as string)).toEqual({ token: TOKEN });
  });

  it("shows the binding without secret controls to a reader", async () => {
    present = true; refuse = false;
    render(<SpritesCard projectId="project" canManage={false} />);
    await screen.findByText("Connected to detent-test");
    expect(screen.queryByLabelText("Sprites organization token")).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
  });
});

describe("Sprite pool settings", () => {
  it.each(["", "printf setup"])("enables a pool with bootstrap %j", async (bootstrap) => {
    writes.length = 0;
    pool = { min_runners: 0, max_runners: 0, idle_seconds: 300, bootstrap, revision: 0, members: [] };
    render(<SpritePoolCard projectId="project" canManage />);
    await screen.findByRole("button", { name: "Save pool" });
    await waitFor(() => expect((screen.getByLabelText("Extra bootstrap (optional)") as HTMLTextAreaElement).value).toBe(bootstrap));
    fireEvent.change(screen.getByLabelText("Maximum runners"), { target: { value: "2" } });
    fireEvent.change(screen.getByLabelText("Minimum runners"), { target: { value: "2" } });
    expect((screen.getByLabelText("Extra bootstrap (optional)") as HTMLTextAreaElement).required).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Save pool" }));
    await waitFor(() => expect(writes.length).toBe(1));
    expect(JSON.parse(String(writes[0]!.body))).toEqual({ min_runners: 2, max_runners: 2, idle_seconds: 300, bootstrap, revision: 0 });
    await waitFor(() => expect(screen.getByRole("button", { name: "Save pool" }).hasAttribute("disabled")).toBe(false));
  });
});
