import React from "react";
import { createRoot } from "react-dom/client";
import { createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { ClientContext } from "../../web/conversation/src/app/client.ts";
import { RunnersSettings } from "../../web/conversation/src/app/fleet/RunnersSection.tsx";
import type { ConversationClient } from "../../web/conversation/src/runtime/bootstrap.ts";
import fleet from "../../web/conversation/src/contracts/fixtures/account-fleet.json";

const client = {
  account: {
    version: "v0.9.1",
    organization: { id: "org_preview", name: "Threefold" },
    organizations: [{ current: true, public_url: "https://runners.detent.test" }],
    actor: { can_manage: true, can_manage_runners: true },
    projects: [{ id: "proj_preview", name: "Preview project" }],
    base_path: "",
  },
  http: { origin: "", apiBase: "/api/v2/organizations/org_preview", csrfToken: "preview" },
} as unknown as ConversationClient;

if (location.protocol === "file:") {
  const problem = { code: "tier_unavailable", message: "Sandbox tooling is unavailable", fix_hint: "Repair the sandbox tooling", first_seen: "2026-09-10T12:00:00Z" };
  const preview = { ...fleet, runners: [
    { ...fleet.runners[0], claim_refusal_reason: "" },
    { ...fleet.runners[1], display_name: "Build runner", health: "needs_attention", host_capacity: 4, claim_refusal_reason: "", problems: [problem] },
    { ...fleet.runners[1], id: "runner_outside", display_name: "Night runner", health: "outside_hours", host_capacity: 1, claim_refusal_reason: "" },
  ] };
  globalThis.fetch = async (input) => new Response(JSON.stringify(String(input).endsWith("/secrets/fly_sprites_token")
    ? { kind: "fly_sprites_token", present: false }
    : String(input).endsWith("/runner-enrollments")
    ? { id: "preview_enrollment", token: "preview_token", expires_at: "2026-09-10T12:24:31Z" }
    : preview), { status: String(input).endsWith("/runner-enrollments") ? 201 : 200 });
}

const route = createRootRoute({
  component: () => (
    <ClientContext.Provider value={client}>
      <div className="flex h-dvh min-w-0 flex-col bg-background text-foreground">
        <header className="border-b border-border px-5 py-3"><h1 className="text-sm">Settings</h1></header>
        <RunnersSettings />
      </div>
    </ClientContext.Provider>
  ),
});
const router = createRouter({ routeTree: route });
createRoot(document.getElementById("root")!).render(<RouterProvider router={router} />);
