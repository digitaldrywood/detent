import { validateUserSearch } from "./userList.ts";
// The shared entry's route tree. It is separate from the organization client's
// tree: the entry serves this shell with an empty base path and a
// `detent-surface=entry` marker, and none of these screens needs the
// organization bootstrap.
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  redirect,
  useNavigate,
  useParams,
  type RouterHistory,
} from "@tanstack/react-router";
import React from "react";

import {
  CreateOrganization,
  EntrySignIn,
  OrganizationChooser,
  ProvisioningProgress,
} from "./EntryScreens.tsx";
import {
  PlatformAllowlistPage,
  PlatformConsole,
  PlatformHealthPage,
  PlatformSectionPage,
  PlatformTenantsPage,
  PlatformTenantPage,
} from "./PlatformConsole.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { validateTenantSearch } from "./tenantList.ts";
import { PlatformAuditPage } from "./PlatformAudit.tsx";
import { PlatformUserPage, PlatformUsersPage } from "./PlatformUsers.tsx";

function useGo(): (to: string) => void {
  const navigate = useNavigate();
  return React.useCallback(
    (to: string) => {
      void navigate((to.includes("?") ? { href: to } : { to }) as never);
    },
    [navigate],
  );
}

const rootRoute = createRootRoute({
  component: () => (
    <div className="flex h-full flex-col bg-background text-foreground">
      <Outlet />
    </div>
  ),
});

export const ENTRY_ROUTE_PATHS = [
  "/",
  "/organizations",
  "/organizations/new",
  "/organizations/$organization/provisioning",
  "/platform",
  "/platform/tenants",
  "/platform/tenants/$organization",
  "/platform/users",
  "/platform/users/$email",
  "/platform/staff",
  "/platform/audit",
  "/platform/health",
  "/platform/allowlist",
] as const;

const platformRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/platform",
  component: PlatformConsole,
});

const platformRoutes = platformRoute.addChildren([
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/",
    beforeLoad: () => {
      throw redirect({ to: "/platform/tenants", replace: true } as never);
    },
  }),
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/tenants",
    validateSearch: validateTenantSearch,
    beforeLoad: ({ search }) => {
      if (search.tenant) throw redirect({ to: "/platform/tenants/$organization", params: { organization: search.tenant }, search: { ...search, tenant: undefined }, replace: true } as never);
    },
    component: PlatformTenantsPage,
  }),
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/tenants/$organization",
    validateSearch: (search: Record<string, unknown>) => ({ ...validateTenantSearch(search), section: typeof search.section === "string" ? search.section : undefined }),
    component: PlatformTenantPage,
  }),
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/users",
    validateSearch: validateUserSearch,
    component: PlatformUsersPage,
  }),
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/users/$email",
    validateSearch: validateUserSearch,
    component: PlatformUserPage,
  }),
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/accounts",
    beforeLoad: () => {
      throw redirect({ to: "/platform/users", replace: true } as never);
    },
  }),
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/staff",
    component: () => <PlatformSectionPage section="staff" />,
  }),
  createRoute({
    getParentRoute: () => platformRoute,
    path: "/audit",
    component: PlatformAuditPage,
  }),
  createRoute({ getParentRoute: () => platformRoute, path: "/health", component: PlatformHealthPage }),
  createRoute({ getParentRoute: () => platformRoute, path: "/allowlist", component: PlatformAllowlistPage }),
]);

const routeTree = rootRoute.addChildren([
  createRoute({
    getParentRoute: () => rootRoute,
    path: "$",
    beforeLoad: () => {
      throw redirect({ to: "/organizations" } as never);
    },
  }),
  createRoute({
    getParentRoute: () => rootRoute,
    path: "/",
    component: function SignIn() {
      usePageTitle("Sign in");
      return <EntrySignIn />;
    },
  }),
  createRoute({
    getParentRoute: () => rootRoute,
    path: "/organizations",
    component: function Chooser() {
      usePageTitle("Organizations");
      return <OrganizationChooser onNavigate={useGo()} />;
    },
  }),
  createRoute({
    getParentRoute: () => rootRoute,
    path: "/organizations/new",
    component: function Create() {
      usePageTitle("New organization");
      return <CreateOrganization onNavigate={useGo()} />;
    },
  }),
  createRoute({
    getParentRoute: () => rootRoute,
    path: "/organizations/$organization/provisioning",
    component: function Progress() {
      const { organization } = useParams({ strict: false }) as { organization?: string };
      usePageTitle("Provisioning", organization);
      return <ProvisioningProgress organization={organization ?? ""} onNavigate={useGo()} />;
    },
  }),
  platformRoutes,
]);

export function makeEntryRouter(history?: RouterHistory) {
  return createRouter({ routeTree, basepath: "/", ...(history === undefined ? {} : { history }) });
}

export { isEntrySurface } from "./surface.ts";
