import React from "react";
import { createRoot } from "react-dom/client";
import { createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { ClientContext } from "../../web/conversation/src/app/client.ts";
import { RunnersSettings } from "../../web/conversation/src/app/fleet/RunnersSection.tsx";
import type { ConversationClient } from "../../web/conversation/src/runtime/bootstrap.ts";
import fleet from "../../web/conversation/src/contracts/fixtures/account-fleet.json";

const client = {
  account: {
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
  let preview = { ...fleet, editable: !new URLSearchParams(location.search).has("readOnly"), runners: [
    { ...fleet.runners[0], claim_refusal_reason: "" },
    { ...fleet.runners[1], display_name: "Build runner", health: "needs_attention", host_capacity: 4, claim_refusal_reason: "", problems: [problem] },
    { ...fleet.runners[1], id: "runner_outside", display_name: "Night runner", health: "outside_hours", host_capacity: 1, claim_refusal_reason: "" },
  ].map((runner) => ({ ...runner, state: "active", revision: 1, routing: {
    display_name: runner.display_name, state: "active", capacity_limit: runner.capacity_limit,
    project_ids: ["proj_preview", "prj_unreadable"], home_project_ids: ["proj_preview"], tags: ["linux"],
    isolation_tier: "sandbox", host_services: ["tcp:127.0.0.1:8080"],
    availability: { timezone: "America/Chicago", windows: ["Mon-Fri 09:00-17:00"], hard_deadline: "30m" },
    spillover: { mode: "after", after_minutes: 5 },
  } })) };
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/routing") && init?.method === "PUT") {
      const id = String(input).split("/").at(-2);
      const { expected_revision, ...routing } = JSON.parse(String(init.body));
      preview = { ...preview, runners: preview.runners.map((runner) => runner.id === id
        ? { ...runner, routing, state: routing.state, capacity_limit: routing.capacity_limit, revision: expected_revision + 1 }
        : runner) };
      return new Response("{}", { status: 200 });
    }
    const enrollment = String(input).endsWith("/runner-enrollments");
    return new Response(JSON.stringify(enrollment
      ? { id: "preview_enrollment", token: "preview_token", expires_at: "2026-09-10T12:24:31Z" }
      : preview), { status: enrollment ? 201 : 200 });
  };
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
