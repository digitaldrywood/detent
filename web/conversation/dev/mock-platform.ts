// The platform console's half of the mock hub.
//
// It answers the entry's live platform endpoints and every endpoint
// docs/platform-admin.md proposes, from `src/contracts/fixtures/platform/`.
// Fixture timestamps are shifted so the newest reads as now. Actions are
// accepted and recorded but change nothing.
//
// `GET /__mock/platform?scenario=full|empty|error&role=admin|staff|forbidden|signed-out`
// switches what it answers, so every page's empty and error states can be
// seen in the browser. `MOCK_PLATFORM_SCENARIO` and `MOCK_PLATFORM_ROLE` set
// the starting values.
import type { ServerResponse } from "node:http";

import allowlistFixture from "../src/contracts/fixtures/platform/allowlist.json" with { type: "json" };
import auditFixture from "../src/contracts/fixtures/platform/audit.json" with { type: "json" };
import billingFixture from "../src/contracts/fixtures/platform/billing.json" with { type: "json" };
import entitlementsFixture from "../src/contracts/fixtures/platform/entitlements.json" with { type: "json" };
import healthFixture from "../src/contracts/fixtures/platform/health.json" with { type: "json" };
import organization_recordsFixture from "../src/contracts/fixtures/platform/organization-records.json" with { type: "json" };
import organizationsFixture from "../src/contracts/fixtures/platform/organizations.json" with { type: "json" };
import overviewFixture from "../src/contracts/fixtures/platform/overview.json" with { type: "json" };
import plansFixture from "../src/contracts/fixtures/platform/plans.json" with { type: "json" };
import runnersFixture from "../src/contracts/fixtures/platform/runners.json" with { type: "json" };
import settingsFixture from "../src/contracts/fixtures/platform/settings.json" with { type: "json" };
import support_sessionsFixture from "../src/contracts/fixtures/platform/support-sessions.json" with { type: "json" };
import tenantsFixture from "../src/contracts/fixtures/platform/tenants.json" with { type: "json" };
import usersFixture from "../src/contracts/fixtures/platform/users.json" with { type: "json" };

import { PLATFORM_FIXTURE_NOW } from "../src/contracts/platform.ts";

export type PlatformScenario = "full" | "empty" | "error";
export type PlatformRole = "admin" | "staff" | "forbidden" | "signed-out";

const SCENARIOS: readonly PlatformScenario[] = ["full", "empty", "error"];
const ROLES: readonly PlatformRole[] = ["admin", "staff", "forbidden", "signed-out"];

const ISO = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/;

const FIXTURES: Record<string, unknown> = {
  "allowlist.json": allowlistFixture,
  "audit.json": auditFixture,
  "billing.json": billingFixture,
  "entitlements.json": entitlementsFixture,
  "health.json": healthFixture,
  "organization-records.json": organization_recordsFixture,
  "organizations.json": organizationsFixture,
  "overview.json": overviewFixture,
  "plans.json": plansFixture,
  "runners.json": runnersFixture,
  "settings.json": settingsFixture,
  "support-sessions.json": support_sessionsFixture,
  "tenants.json": tenantsFixture,
  "users.json": usersFixture,
};

function fixture(name: string): unknown {
  return structuredClone(FIXTURES[name]);
}

/** Moves every timestamp in a fixture by the same offset. */
export function shiftTimestamps(value: unknown, offset: number): unknown {
  if (typeof value === "string") {
    return ISO.test(value) ? new Date(Date.parse(value) + offset).toISOString().replace(".000Z", "Z") : value;
  }
  if (Array.isArray(value)) return value.map((item) => shiftTimestamps(item, offset));
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, shiftTimestamps(item, offset)]));
  }
  return value;
}

type Json = Record<string, any>;

function emptied(name: string, value: Json): Json {
  switch (name) {
    case "organizations.json":
      return { ...value, organizations: [] };
    case "overview.json":
      return {
        ...value,
        organizations: { total: 0, by_state: {} },
        users: { total: 0, active_7d: 0 },
        runners: { connected: 0, registered: 0 },
        signups: { last_7d: 0, recent: [] },
        attention: [],
      };
    case "runners.json":
      return { ...value, runners: [], unreachable: [] };
    case "users.json":
      return { users: [] };
    case "plans.json":
      return { plans: [], grants: [] };
    case "billing.json":
      return { mode: null, account_id: null, customers: [], events: [] };
    case "tenants.json":
      return { backups_configured: false, tenants: [] };
    case "audit.json":
      return { entries: [], next_cursor: null };
    case "support-sessions.json":
      return { sessions: [] };
    case "health.json":
      return { ...value, tenants: { expected: 0, running: 0 }, admission: { ...value.admission, tenants: 0, allocating: 0 } };
    default:
      return value;
  }
}

export interface PlatformMock {
  handle: (input: {
    response: ServerResponse;
    url: URL;
    method: string;
    readBody: () => Promise<Record<string, unknown>>;
  }) => Promise<boolean>;
  actions: () => readonly { path: string; body: Record<string, unknown> }[];
}

export function createPlatformMock(
  options: { scenario?: PlatformScenario; role?: PlatformRole; now?: () => number } = {},
): PlatformMock {
  let scenario: PlatformScenario = SCENARIOS.includes(options.scenario as PlatformScenario) ? options.scenario! : "full";
  let role: PlatformRole = ROLES.includes(options.role as PlatformRole) ? options.role! : "admin";
  const now = options.now ?? Date.now;
  const actions: { path: string; body: Record<string, unknown> }[] = [];

  function read(name: string): Json {
    const shifted = shiftTimestamps(fixture(name), now() - Date.parse(PLATFORM_FIXTURE_NOW)) as Json;
    return scenario === "empty" ? emptied(name, shifted) : shifted;
  }

  function json(response: ServerResponse, status: number, payload: unknown): true {
    const body = JSON.stringify(payload);
    response.writeHead(status, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
      "Content-Length": Buffer.byteLength(body),
    });
    response.end(body);
    return true;
  }

  function organizations(): Json {
    const value = read("organizations.json");
    if (role === "staff") {
      return {
        ...value,
        can_support: false,
        can_grant: false,
        organizations: value.organizations.map((organization: Json) => ({ ...organization, can_support: false })),
      };
    }
    return value;
  }

  function record(id: string): Json | undefined {
    return (read("organization-records.json") as Record<string, Json>)[id];
  }

  function denied(response: ServerResponse): true | null {
    if (role === "signed-out") return json(response, 401, { code: "unauthenticated", message: "Sign in again." });
    if (role === "forbidden") return json(response, 403, { code: "forbidden", message: "Detent staff only." });
    return null;
  }

  return {
    actions: () => actions,
    async handle({ response, url, method, readBody }) {
      const path = url.pathname;

      if (path === "/__mock/platform") {
        const nextScenario = url.searchParams.get("scenario");
        const nextRole = url.searchParams.get("role");
        if (nextScenario !== null && SCENARIOS.includes(nextScenario as PlatformScenario)) scenario = nextScenario as PlatformScenario;
        if (nextRole !== null && ROLES.includes(nextRole as PlatformRole)) role = nextRole as PlatformRole;
        return json(response, 200, { scenario, role, actions });
      }

      if (path === "/support/start" && method === "POST") {
        actions.push({ path, body: {} });
        response.writeHead(303, { Location: "/platform/support" });
        response.end();
        return true;
      }

      if (!path.startsWith("/api/cloud/platform/")) return false;
      const refused = denied(response);
      if (refused !== null) return refused;

      const rest = path.slice("/api/cloud/platform/".length).split("/").map(decodeURIComponent);
      const live = rest[0] === "organizations" && rest.length === 1;
      const liveReads = new Set(["allowlist", "health"]);
      const entitlements = rest[0] === "organizations" && rest[2] === "entitlements";

      if (method === "POST") {
        const body = await readBody();
        actions.push({ path, body });
        if (entitlements) {
          if (role !== "admin") return json(response, 403, { code: "forbidden", message: "Entitlement administrators only." });
          return json(response, 200, { action: String(body.action ?? "grant"), grant_id: "comp_mock" });
        }
        if (scenario === "error") return json(response, 503, { code: "tenant_unavailable", message: "The organization's Hub did not answer." });
        return json(response, 200, { ok: true, message: "Done. The mock recorded the action and changed nothing." });
      }

      if (scenario === "error" && !live) {
        return json(response, 503, { code: "tenant_unavailable", message: "Detent could not reach the tenant Hubs. Try again shortly." });
      }
      if (live) return json(response, 200, organizations());
      if (rest.length === 1 && liveReads.has(rest[0] ?? "")) return json(response, 200, read(`${rest[0]}.json`));
      if (entitlements) {
        if (role !== "admin") return json(response, 403, { code: "forbidden", message: "Entitlement administrators only." });
        return json(response, 200, { ...read("entitlements.json"), organization_id: rest[1] });
      }

      if (rest[0] === "organizations" && rest[1] !== undefined) {
        const found = record(rest[1]);
        if (found === undefined) return json(response, 404, { code: "not_found", message: "No such organization." });
        const runners = read("runners.json");
        switch (rest[2]) {
          case undefined:
            return json(response, 200, found.detail);
          case "members":
          case "projects":
          case "usage":
          case "billing":
          case "provisioning":
            return json(response, 200, found[rest[2]]);
          case "runners":
            return json(response, 200, {
              ...runners,
              runners: runners.runners.filter((runner: Json) => runner.organization.id === rest[1]),
              unreachable: [],
            });
          default:
            return json(response, 404, { code: "not_found", message: "No such endpoint." });
        }
      }

      const files: Record<string, string> = {
        overview: "overview.json",
        runners: "runners.json",
        users: "users.json",
        plans: "plans.json",
        billing: "billing.json",
        tenants: "tenants.json",
        audit: "audit.json",
        "support-sessions": "support-sessions.json",
        settings: "settings.json",
      };
      const file = rest.length === 1 ? files[rest[0] ?? ""] : undefined;
      if (file === undefined) return json(response, 404, { code: "not_found", message: "No such endpoint." });
      const value = read(file);
      if (file === "audit.json") {
        const q = (url.searchParams.get("q") ?? "").toLowerCase();
        const kind = url.searchParams.get("actor_kind");
        const organization = url.searchParams.get("organization");
        value.entries = value.entries.filter(
          (entry: Json) =>
            (kind === null || entry.actor_kind === kind) &&
            (organization === null || entry.organization?.id === organization) &&
            (q === "" || JSON.stringify(entry).toLowerCase().includes(q)),
        );
      }
      return json(response, 200, value);
    },
  };
}
