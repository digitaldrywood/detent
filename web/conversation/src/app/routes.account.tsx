// The account, settings, usage, project-settings and wizard routes.
//
// `router.tsx` owns the route tree; this file owns the routes it adds for the
// screens of decisions.md §12 and §17, so the two can be worked on
// independently. It exports a builder rather than a tree: TanStack routes are
// bound to their parent at construction, and the parent is the router's own
// root.
//
// Every path here is what the hub serves the application shell for (§12,
// "Serving"): `/login`, `/settings/*`, `/usage`, `/projects/*`, and the three
// paths that used to be screens of their own and are now redirects into a
// settings section (§17.3): `/organization`, `/fleet` and
// `/projects/:project/settings`. They stay as routes rather than being dropped
// because they are what links, bookmarks and the end-to-end specs already
// point at.
import { createRoute, redirect, type AnyRoute } from "@tanstack/react-router";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import React from "react";

import { LoginRoute } from "./account/Login.tsx";
import { writeLastProject } from "./client.ts";
import { SetupRoute } from "./account/Setup.tsx";
import { SettingsRoute } from "./settings/Settings.tsx";
import { DEFAULT_SECTION, isSettingsSectionId } from "./settings/sections.tsx";
import { UsageRoute } from "./usage/UsagePage.tsx";
import { DiagnosticsRoute } from "./diagnostics/DiagnosticsPage.tsx";
import { ReportsRoute } from "./reports/ReportsPage.tsx";
import { usePageTitle } from "./pageTitle.ts";

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

function useGo(): (to: string) => void {
  const navigate = useNavigate();
  return React.useCallback(
    (to: string) => {
      void navigate(navigationTarget(to) as never);
    },
    [navigate],
  );
}

function SettingsScreen(): React.ReactElement {
  const { section } = useParams({ strict: false }) as { section?: string };
  const search = useSearch({ strict: false }) as { project?: string };
  const id = section !== undefined && isSettingsSectionId(section) ? section : DEFAULT_SECTION;
  return <SettingsRoute section={id} project={search.project ?? null} onNavigate={useGo()} />;
}

function LoginScreen(): React.ReactElement {
  usePageTitle("Sign in");
  return <LoginRoute />;
}

function SetupScreen(): React.ReactElement {
  const { project } = useParams({ strict: false }) as { project?: string };
  usePageTitle("Project setup");
  return <SetupRoute projectId={project ?? ""} onNavigate={useGo()} />;
}

/** The paths this file registers, for the router and for a test. */
export const ACCOUNT_ROUTE_PATHS = [
  "/login",
  "/organization",
  "/settings",
  "/settings/$section",
  "/usage",
  "/reports",
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
    createRoute({ getParentRoute, path: "/settings/$section", component: SettingsScreen }),
    createRoute({ getParentRoute, path: "/usage", component: UsageRoute }),
    createRoute({ getParentRoute, path: "/reports", component: ReportsRoute }),
    createRoute({ getParentRoute, path: "/diagnostics", component: DiagnosticsRoute }),
    createRoute({
      getParentRoute,
      path: "/projects/$project/settings",
      beforeLoad: ({ params }) => {
        const { project } = params as { project: string };
        throw redirect({
          to: "/settings/$section",
          params: { section: "integrations" },
          search: { project },
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
