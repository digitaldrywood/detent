import React from "react";
import { createRoot } from "react-dom/client";
import { createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { ClientContext } from "../../web/conversation/src/app/client.ts";
import type { ConversationClient } from "../../web/conversation/src/runtime/bootstrap.ts";
import { IntegrationsSettings } from "../../web/conversation/src/app/settings/Settings.tsx";

const client = {
  account: {
    organization: { id: "org_preview", name: "Threefold" },
    actor: { can_manage: !new URLSearchParams(location.search).has("viewer") },
    projects: [],
    base_path: "",
  },
  http: { origin: "", apiBase: "/api/v2/organizations/org_preview", csrfToken: "preview" },
} as unknown as ConversationClient;

const route = createRootRoute({ component: () => <ClientContext.Provider value={client}>
  <div className="flex h-dvh min-w-0 flex-col bg-background text-foreground">
    <header className="border-b border-border px-5 py-3"><h1 className="text-sm">Settings · Integrations</h1></header>
    <IntegrationsSettings project={null} />
  </div>
</ClientContext.Provider> });
const router = createRouter({ routeTree: route });
createRoot(document.getElementById("root")!).render(<RouterProvider router={router} />);
