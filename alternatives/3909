import React from "react";
import { createRoot } from "react-dom/client";
import { createRootRoute, createRoute, createRouter, RouterProvider, Outlet } from "@tanstack/react-router";
import { ClientContext } from "../../web/conversation/src/app/client.ts";
import { SettingsRoute } from "../../web/conversation/src/app/settings/Settings.tsx";
import { useNavigate } from "@tanstack/react-router";
import { SettingsScreen } from "../../web/conversation/src/app/routes.account.screens.tsx";
import { DraftStore } from "../../web/conversation/src/runtime/state/drafts.ts";
import type { ConversationClient } from "../../web/conversation/src/runtime/bootstrap.ts";
import fleet from "../../web/conversation/src/contracts/fixtures/account-fleet.json";
import spritePool from "./providers-runners-sprite-pool.json";

const params = new URLSearchParams(location.search);
const poolState = params.get("poolState");
const projects = [
  { id: "proj_preview", name: "Preview project", can_write: true },
  ...((params.has("multipleProjects") || (window as unknown as { previewMultipleProjects?: boolean }).previewMultipleProjects) ? [{ id: "proj_second", name: "Second project", can_write: true }, { id: "proj_viewer", name: "Read-only project", can_write: false }] : []),
];
const client = {
  bootstrap: { organization: { id: "org_preview" }, actor: { principal_id: "owner_preview" } },
  drafts: new DraftStore(),
  account: {
    version: params.get("version") ?? "v0.9.1",
    organization: { id: "org_preview", name: "Example Organization" },
    organizations: [{ current: true, public_url: "https://runners.detent.test" }],
    actor: { can_manage: poolState !== "permissions", can_manage_runners: true },
    projects,
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
  ].map((runner) => ({ ...runner, state: "active", revision: 1, can_edit_projects: true, routing: {
    display_name: runner.display_name, state: "active", capacity_limit: runner.capacity_limit,
    project_ids: ["proj_preview", "prj_unreadable"], tags: ["linux"],
    isolation_tier: "sandbox", host_services: ["tcp:127.0.0.1:8080"],
    availability: { timezone: "America/Chicago", windows: ["Mon-Fri 09:00-17:00"], hard_deadline: "30m" },
  } })) };
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/sprite-pool")) {
      if (poolState === "failure") return new Response(JSON.stringify({ code: "unavailable", message: "Pool request failed" }), { status: 503 });
      return new Response(JSON.stringify(poolState === "not-set-up" ? { ...spritePool, min_runners: 0, max_runners: 0, idle_seconds: 300, bootstrap: "", revision: 0, members: [] } : spritePool));
    }
    if (String(input).endsWith("/routing") && init?.method === "PUT") {
      const id = String(input).split("/").at(-2);
      const { expected_revision, ...routing } = JSON.parse(String(init.body));
      preview = { ...preview, runners: preview.runners.map((runner) => runner.id === id
        ? { ...runner, routing, state: routing.state, capacity_limit: routing.capacity_limit, revision: expected_revision + 1 }
        : runner) };
      return new Response("{}", { status: 200 });
    }
    if (String(input).endsWith("/secrets/fly_sprites_token")) return new Response(JSON.stringify({ kind: "fly_sprites_token", present: poolState !== "missing-token" && poolState !== null }));
    const enrollment = String(input).endsWith("/runner-enrollments");
    return new Response(JSON.stringify(enrollment
      ? { id: "preview_enrollment", token: "preview_token", expires_at: "2026-09-10T12:24:31Z" }
      : preview), { status: enrollment ? 201 : 200 });
  };
}

function RunnersListScreen() {
  const navigate = useNavigate();
  return <SettingsRoute section="runners" onNavigate={(to) => { void navigate({ to } as never); }} />;
}

const route = createRootRoute({
  component: () => (
    <ClientContext.Provider value={client}>
      <div className="flex h-dvh min-w-0 flex-col bg-background text-foreground">
        <Outlet />
      </div>
    </ClientContext.Provider>
  ),
});
const router = createRouter({ routeTree: route.addChildren([createRoute({ getParentRoute: () => route, path: "/settings/runners", component: RunnersListScreen }), createRoute({ getParentRoute: () => route, path: "/settings/runners/$runner", component: () => <SettingsScreen /> })]) });
createRoot(document.getElementById("root")!).render(<RouterProvider router={router} />);
