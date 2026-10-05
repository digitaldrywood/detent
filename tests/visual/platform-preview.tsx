import React from "react";
import { createRoot } from "react-dom/client";
import { EntryApiContext } from "../../web/conversation/src/app/entry/EntryScreens.tsx";
import { PlatformConsole } from "../../web/conversation/src/app/entry/PlatformConsole.tsx";
import { makeEntryApi } from "../../web/conversation/src/app/entry/api.ts";
import fixture from "./platform-preview-data.json";

if (location.protocol === "file:") {
  let features: string[] = [];
  let grants: unknown[] = [];
  let revision = 1;
  const plan = fixture.entitlements.base;
  globalThis.fetch = async (input, init) => {
    const path = String(input);
    let value: unknown;
    if (path.endsWith("/entitlements")) {
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
  <EntryApiContext.Provider value={makeEntryApi()}><PlatformConsole /></EntryApiContext.Provider>,
);
