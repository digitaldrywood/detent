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
import { PlatformConsole } from "./PlatformConsole.tsx";
import { usePageTitle } from "../pageTitle.ts";

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
] as const;

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
  createRoute({
    getParentRoute: () => rootRoute,
    path: "/platform",
    component: function Platform() {
      usePageTitle("Platform");
      return <PlatformConsole />;
    },
  }),
]);

export function makeEntryRouter(history?: RouterHistory) {
  return createRouter({ routeTree, basepath: "/", ...(history === undefined ? {} : { history }) });
}

/** True when the shell was served by the shared entry rather than an organization. */
export function isEntrySurface(source: Pick<Document, "querySelector"> | undefined = globalThis.document): boolean {
  return source?.querySelector('meta[name="detent-surface"]')?.getAttribute("content") === "entry";
}
