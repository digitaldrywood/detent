import React from "react";
import { createRoot } from "react-dom/client";
import { EntryApiContext } from "../../web/conversation/src/app/entry/EntryScreens.tsx";
import { createMemoryHistory, RouterProvider } from "@tanstack/react-router";
import { makeEntryRouter } from "../../web/conversation/src/app/entry/router.tsx";
import { makeEntryApi } from "../../web/conversation/src/app/entry/api.ts";
import fixture from "./platform-preview-data.json";
import audit from "./platform-audit-data.json";
import users from "./platform-users-data.json";

const params = new URLSearchParams(location.search);

if (location.protocol === "file:") {
  let features: string[] = [];
  let grants: unknown[] = [];
  let revision = 1;
  const plan = fixture.entitlements.base;
  let credits = fixture.detail.ai_credits;
  const creditCommands = new Set<string>();
  globalThis.fetch = async (input, init) => {
    const path = String(input);
    let value: unknown;
    if (path.endsWith("/ai-credits") && init?.method === "POST") {
      const change = JSON.parse(String(init.body));
      if (!creditCommands.has(change.idempotency_key)) {
        const amount = Number(change.amount_usd) * 1_000_000;
        credits = { ...credits, balance_micros: credits.balance_micros + amount, history: [
          { amount_micros: amount, kind: "complimentary", actor: "staff@example.test", reason: change.reason, at: "2026-10-12T12:00:00Z" }, ...credits.history,
        ] };
        creditCommands.add(change.idempotency_key);
      }
      value = { organization_id: fixture.detail.organization.id, balance_micros: credits.balance_micros };
    } else if (path.endsWith("/entitlements")) {
      if (init?.method === "POST") {
        const change = JSON.parse(String(init.body));
        features = change.action === "grant" ? ["model_choice"] : [];
        grants = change.action === "grant" ? [{
          id: "model_choice_grant", plan, scope: ["model_choice"], starts_at: "2026-10-05T12:00:00Z",
          expires_at: change.expires_at, reason: change.reason, granted_by: "admin@example.test", granted_at: "2026-10-05T12:00:00Z",
        }] : [];
        revision += 1;
        value = { action: change.action, grant_id: "model_choice_grant" };
      } else {
        value = { ...fixture.entitlements, revision, features, grants };
      }
    } else if (path.startsWith("/api/cloud/platform/organizations/")) {
      const role = params.get("role") ?? "admin";
      value = { ...fixture.detail, ai_credits: credits, credit_balance_micros: credits.balance_micros, can_grant: ["admin", "billing"].includes(role),
        organization: { ...fixture.detail.organization, can_support: ["admin", "support"].includes(role) },
        entitlements: { ...fixture.entitlements, revision, features, grants } };
    } else if (path === "/api/cloud/session" || path === "/api/cloud/organizations") {
      value = {
        ...fixture.account, platform_role: params.get("role") ?? "admin",
        organizations: params.has("no-organizations") ? [] : fixture.account.organizations,
      };
    } else if (path === "/api/cloud/platform/members") {
      value = { members: [], revision: 1, self: { email: fixture.account.email, role: params.get("role") ?? "admin" } };
    } else if (path.startsWith("/api/cloud/platform/audit")) {
      const filters = new URL(path, "https://platform.detent.test").searchParams;
      const rows = audit.rows.filter((row) =>
        (!filters.get("tenant") || row.organization_id === filters.get("tenant")) &&
        (!filters.get("actor") || row.actor.toLowerCase().includes(filters.get("actor")!.toLowerCase())) &&
        (!filters.get("event") || row.event === filters.get("event")) &&
        (!filters.get("from") || row.at >= filters.get("from")!) &&
        (!filters.get("to") || row.at.slice(0, 10) <= filters.get("to")!),
      );
      const older = filters.has("cursor");
      value = { ...audit, rows: older ? rows.slice(3) : rows.slice(0, 3), next_cursor: !older && rows.length > 3 ? "older-page" : "" };
    } else if (path.startsWith("/api/cloud/platform/users?")) {
      const query = new URL(path, "https://platform.detent.test").searchParams.get("q") ?? "";
      const matches = users.users.filter((user) => user.email.includes(query));
      value = { ...users, total: matches.length, users: matches };
    } else if (path.endsWith("/organizations")) {
      value = { ...fixture.organizations, can_grant: !location.search.includes("staff") };
    } else if (path.endsWith("/allowlist")) {
      value = fixture.allowlist;
    } else {
      value = fixture.health;
    }
    return new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
  };
}

createRoot(document.getElementById("root")!).render(
  <EntryApiContext.Provider value={makeEntryApi()}>
    <RouterProvider router={makeEntryRouter(location.protocol === "file:" ? createMemoryHistory({
      initialEntries: [params.get("path") ?? "/platform/tenants"],
    }) : undefined)} />
  </EntryApiContext.Provider>,
);
