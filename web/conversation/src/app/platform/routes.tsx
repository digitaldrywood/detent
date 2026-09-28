import { createRoute, useParams, type AnyRoute } from "@tanstack/react-router";
import React from "react";

import { PlatformLayout } from "./PlatformLayout.tsx";
import { AuditLogPage } from "./pages/Audit.tsx";
import { BillingPage } from "./pages/Billing.tsx";
import { OrganizationDetailPage, organizationTab } from "./pages/OrganizationDetail.tsx";
import { OrganizationsPage } from "./pages/Organizations.tsx";
import { OverviewPage } from "./pages/Overview.tsx";
import { PlansPage } from "./pages/Plans.tsx";
import { ProvisioningPage } from "./pages/Provisioning.tsx";
import { RunnersPage } from "./pages/Runners.tsx";
import { PlatformSettingsPage } from "./pages/Settings.tsx";
import { SupportSessionsPage } from "./pages/Support.tsx";
import { UserDetailPage, UsersPage } from "./pages/Users.tsx";

export const PLATFORM_ROUTE_PATHS = [
  "/platform",
  "/platform/organizations",
  "/platform/organizations/$organization",
  "/platform/organizations/$organization/$tab",
  "/platform/users",
  "/platform/users/$subject",
  "/platform/runners",
  "/platform/provisioning",
  "/platform/plans",
  "/platform/billing",
  "/platform/support",
  "/platform/audit",
  "/platform/settings",
] as const;

function OrganizationRoute(): React.ReactElement {
  const { organization, tab } = useParams({ strict: false }) as { organization?: string; tab?: string };
  return <OrganizationDetailPage key={organization} organization={organization ?? ""} tab={organizationTab(tab)} />;
}

function UserRoute(): React.ReactElement {
  const { subject } = useParams({ strict: false }) as { subject?: string };
  return <UserDetailPage subject={subject ?? ""} />;
}

export function platformRoute(getParentRoute: () => AnyRoute) {
  const layout = createRoute({ getParentRoute, path: "/platform", component: () => <PlatformLayout /> });
  const child = (path: string, component: () => React.ReactElement) =>
    createRoute({ getParentRoute: () => layout, path, component });
  return layout.addChildren([
    child("/", OverviewPage),
    child("/organizations", OrganizationsPage),
    child("/organizations/$organization", OrganizationRoute),
    child("/organizations/$organization/$tab", OrganizationRoute),
    child("/users", UsersPage),
    child("/users/$subject", UserRoute),
    child("/runners", RunnersPage),
    child("/provisioning", ProvisioningPage),
    child("/plans", PlansPage),
    child("/billing", BillingPage),
    child("/support", SupportSessionsPage),
    child("/audit", AuditLogPage),
    child("/settings", PlatformSettingsPage),
  ]);
}
