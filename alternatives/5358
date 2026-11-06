import { basePath } from "../runtime/basePath.ts";
// The account, settings, usage, project-settings and wizard routes.
//
// `router.tsx` owns the route tree; this file owns the routes it adds for the
// hosted account and settings screens, so the two can be worked on
// independently. It exports a builder rather than a tree: TanStack routes are
// bound to their parent at construction, and the parent is the router's own
// root.
//
// Every path here is what the hub serves the application shell for:
// `/login`, `/settings/*`, `/usage`, `/projects/*`, and the three
// paths that used to be screens of their own and are now redirects into a
// settings section: `/organization`, `/fleet` and
// `/projects/:project/settings`. They stay as routes rather than being dropped
// because they are what links, bookmarks and the end-to-end specs already
// point at.
import { createRoute, lazyRouteComponent, redirect, type AnyRoute } from "@tanstack/react-router";
import React from "react";

import { LoginRoute } from "./account/Login.tsx";
import { writeLastProject } from "./client.ts";
import { DEFAULT_SECTION } from "./settings/sections.tsx";
import { usePageTitle } from "./pageTitle.ts";

const SettingsScreen = lazyRouteComponent(() => import("./routes.account.screens.tsx"), "SettingsScreen");
const ProjectSettingsScreen = lazyRouteComponent(() => import("./routes.account.screens.tsx"), "ProjectSettingsScreen");
const SetupScreen = lazyRouteComponent(() => import("./routes.account.screens.tsx"), "SetupScreen");
const UsageRoute = lazyRouteComponent(() => import("./usage/UsagePage.tsx"), "UsageRoute");
const ThreadsRoute = lazyRouteComponent(() => import("./threads/ThreadsPage.tsx"), "ThreadsRoute");
const ActivityRoute = lazyRouteComponent(() => import("./activity/ActivityPage.tsx"), "ActivityRoute");
const DiagnosticsRoute = lazyRouteComponent(() => import("./diagnostics/DiagnosticsPage.tsx"), "DiagnosticsRoute");
const ReportsRoute = lazyRouteComponent(() => import("./reports/ReportsPage.tsx"), "ReportsRoute");
const REPORT_TAB_PATHS = new Set(["flow", "quality", "cost", "failures"]);

/**
 * The navigation the screens take. It is passed in rather than imported so the
 * components stay renderable in a test with no router, and so a destination
 * that is not a route yet (the Work board, which another route file owns) is a
 * plain string rather than a typed reference this file cannot make.
 */
/**
 * Splits `/path?key=value` into the router's path and search. The router
 * matches `to` against route paths only, so a query left inside `to` matches
 * no route and the navigation silently does nothing.
 */
export function navigationTarget(to: string): { readonly to: string; readonly search?: Record<string, string> } {
  const at = to.indexOf("?");
  if (at < 0) return { to };
  return { to: to.slice(0, at), search: Object.fromEntries(new URLSearchParams(to.slice(at + 1))) };
}

function LoginScreen(): React.ReactElement {
  usePageTitle("Sign in");
  return <LoginRoute />;
}

/** The paths this file registers, for the router and for a test. */
export const ACCOUNT_ROUTE_PATHS = [
  "/login",
  "/organization",
  "/settings",
  "/settings/$section",
  "/settings/organization/$organizationPage",
  "/settings/projects/$project",
  "/settings/projects/$project/$tab",
  "/settings/runners/$runner",
  "/usage",
  "/reports",
  "/reports/$tab",
  "/diagnostics",
  "/projects/$project/settings",
  "/projects/$project/setup",
  "/fleet",
] as const;

/**
 * Builds the account routes under `rootRoute`. `router.tsx` spreads the result
 * into `rootRoute.addChildren([...])`.
 */
export function accountRoutes(rootRoute: AnyRoute): AnyRoute[] {
  const getParentRoute = () => rootRoute;
  return [
    createRoute({ getParentRoute, path: "/login", component: LoginScreen }),
    createRoute({
      getParentRoute,
      path: "/organization",
      beforeLoad: () => {
        throw redirect({ to: "/settings/$section", params: { section: "organization" } });
      },
    }),
    createRoute({
      getParentRoute,
      path: "/fleet",
      beforeLoad: () => {
        throw redirect({ to: "/settings/$section", params: { section: "runners" } });
      },
    }),
    createRoute({
      getParentRoute,
      path: "/settings",
      beforeLoad: () => {
        throw redirect({ to: "/settings/$section", params: { section: DEFAULT_SECTION } });
      },
    }),
    createRoute({ getParentRoute, path: "/settings/$section", component: SettingsScreen, beforeLoad: ({ params }) => {
      if (params.section === "api-keys" && basePath().startsWith("/organizations/")) {
        throw redirect({ to: "/settings/$section", params: { section: "mcp" }, replace: true });
      }
    } }),
    createRoute({ getParentRoute, path: "/settings/organization/$organizationPage", component: SettingsScreen }),
    createRoute({ getParentRoute, path: "/settings/projects/$project", component: ProjectSettingsScreen }),
    createRoute({ getParentRoute, path: "/settings/projects/$project/$tab", component: ProjectSettingsScreen }),
    createRoute({ getParentRoute, path: "/settings/runners/$runner", component: SettingsScreen }),
    createRoute({ getParentRoute, path: "/usage", component: UsageRoute }),
    createRoute({ getParentRoute, path: "/reports", component: ReportsRoute }),
    createRoute({
      getParentRoute,
      path: "/reports/$tab",
      beforeLoad: ({ params, search }) => {
        const { tab } = params as { tab: string };
        if (!REPORT_TAB_PATHS.has(tab)) throw redirect({ to: "/reports", search: search as never });
      },
      component: ReportsRoute,
    }),
    createRoute({ getParentRoute, path: "/activity", component: ActivityRoute }),
    createRoute({ getParentRoute, path: "/threads", component: ThreadsRoute }),
    createRoute({ getParentRoute, path: "/threads/$thread", component: ThreadsRoute }),
    createRoute({ getParentRoute, path: "/diagnostics", component: DiagnosticsRoute }),
    createRoute({
      getParentRoute,
      path: "/projects/$project/settings",
      beforeLoad: ({ params }) => {
        const { project } = params as { project: string };
        throw redirect({
          to: "/settings/projects/$project/$tab",
          params: { project, tab: "general" },
          replace: true,
        });
      },
    }),
    createRoute({ getParentRoute, path: "/projects/$project/setup", component: SetupScreen }),
    ...legacyProjectRoutes(rootRoute),
  ] as unknown as AnyRoute[];
}

/**
 * The project, issue and Change pages the hub used to render itself. The hub
 * now serves the application shell for them, and the links and bookmarks that
 * point at them land here: each one redirects to the Work screen that replaced
 * it, and remembers the project so the issue page resolves it.
 */
export const LEGACY_PROJECT_ROUTE_PATHS = [
  "/projects/$project",
  "/projects/$project/changes",
  "/projects/$project/issues/$item",
  "/projects/$project/issues/$item/changes/$change",
] as const;

function legacyProjectRoutes(rootRoute: AnyRoute): AnyRoute[] {
  const getParentRoute = () => rootRoute;
  const board = ({ params }: { params: unknown }) => {
    const { project } = params as { project: string };
    writeLastProject(project);
    throw redirect({ to: "/work/p/$projectId", params: { projectId: project }, replace: true });
  };
  const changes = ({ params }: { params: unknown }) => {
    writeLastProject((params as { project: string }).project);
    throw redirect({ to: "/work/changes", replace: true });
  };
  const issue = ({ params }: { params: unknown }) => {
    const { project, item, change } = params as { project: string; item: string; change?: string };
    writeLastProject(project);
    throw redirect({
      to: "/work/i/$workItemId",
      params: { workItemId: item },
      ...(change === undefined ? {} : { search: { change } }),
      replace: true,
    } as never);
  };
  return [
    createRoute({ getParentRoute, path: "/projects/$project", beforeLoad: board }),
    createRoute({ getParentRoute, path: "/projects/$project/changes", beforeLoad: changes }),
    createRoute({ getParentRoute, path: "/projects/$project/issues/$item", beforeLoad: issue }),
    createRoute({ getParentRoute, path: "/projects/$project/issues/$item/changes/$change", beforeLoad: issue }),
  ] as unknown as AnyRoute[];
}

/**
 * True for the one route the hub serves without a session. The entry point
 * uses it to render the login card when the bootstrap request is refused:
 * everything else needs a session, and `/login` is where a reader without one
 * is sent.
 */
export function isLoginPath(pathname: string): boolean {
  return pathname === "/login" || pathname.startsWith("/login/");
}

export { LoginCard } from "./account/Login.tsx";
