// Binds each platform fixture to its schema. The live endpoints' fixtures
// decode through the entry client's own schemas, so a fixture can never drift
// from what the production console reads.
import * as Schema from "effect/Schema";

import {
  OrganizationEntitlements,
  PlatformAllowlist,
  PlatformHealth,
  PlatformOrganizations,
} from "../app/entry/api.ts";
import {
  AuditPage,
  OrganizationRecord,
  PlatformBilling,
  PlatformOverview,
  PlatformPlans,
  PlatformRunners,
  PlatformSettings,
  PlatformTenants,
  PlatformUsers,
  SupportSessions,
} from "./platform.ts";

export const PLATFORM_FIXTURE_SCHEMAS: Readonly<Record<string, Schema.Codec<any, any, never, never>>> = {
  "allowlist.json": PlatformAllowlist,
  "audit.json": AuditPage,
  "billing.json": PlatformBilling,
  "entitlements.json": OrganizationEntitlements,
  "health.json": PlatformHealth,
  "organization-records.json": Schema.Record(Schema.String, OrganizationRecord),
  "organizations.json": PlatformOrganizations,
  "overview.json": PlatformOverview,
  "plans.json": PlatformPlans,
  "runners.json": PlatformRunners,
  "settings.json": PlatformSettings,
  "support-sessions.json": SupportSessions,
  "tenants.json": PlatformTenants,
  "users.json": PlatformUsers,
};
