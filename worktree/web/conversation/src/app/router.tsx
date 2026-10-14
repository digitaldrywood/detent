// Routes.
//
// The hub serves the application shell for every non-API path
// (decisions.md §12, "Serving"). The router's basepath is the hub's base path
// (`/` at the root of an origin, `/organizations/ORG` behind the shared Cloud
// entry), and every path below is absolute within it. Chat lives under
// `/chat`, Work under `/work`, and `routes.account.tsx` adds the account,
// settings, project and fleet screens.
import {
  createRootRoute,
  createRoute,
  createRouter,
  lazyRouteComponent,
  Outlet,
  redirect,
  useParams,
  useRouterState,
  type AnyRoute,
  type RouterHistory,
} from "@tanstack/react-router";
import type { ReactElement } from "react";

import { ConversationRoute, NewChat, ProjectNewChat, Shell } from "./App.tsx";
import { accountRoutes } from "./routes.account.tsx";
import { SupportRoute } from "./account/Support.tsx";
import { ChangeRequestPage } from "./work/ChangeRequestPage.tsx";
import { ChangesPage } from "./work/ChangesPage.tsx";
import { IssuePage } from "./work/IssuePage.tsx";
import { WorkBoard } from "./work/WorkBoard.tsx";
import { routerBasePath, withoutBasePath } from "../runtime/basePath.ts";
import { plainSearchOptions } from "./lib/searchParams.ts";

// --- Design system (dev builds only) ----------------------------------------
//
// `/design-system` is the component gallery (`src/design-system/gallery`). It
// exists only when `import.meta.env.DEV` is true: production builds replace
// that with `false`, so the routes, the lazy import and the gallery chunk are
// dropped from the bundle. It renders outside the Shell — no sidebar, no
// conversation list — because it is a viewer of components, not a screen of
// the app, and each specimen frame is a page of its own.

/** True for the gallery and its specimen frames. */
export function isDesignSystemPath(pathname: string): boolean {
  return pathname === "/design-system" || pathname.startsWith("/design-system/");
}

/** The gallery routes, or none outside a dev build. */
export function designSystemRoutes(parent: AnyRoute, dev: boolean): AnyRoute[] {
  if (!dev) return [];
  const gallery = lazyRouteComponent(() => import("../design-system/gallery/Gallery.tsx"), "GalleryRoute");
  const frame = lazyRouteComponent(() => import("../design-system/gallery/Gallery.tsx"), "FrameRoute");
  return [
    createRoute({ getParentRoute: () => parent, path: "/design-system", component: gallery }),
    createRoute({ getParentRoute: () => parent, path: "/design-system/$entryId", component: gallery }),
    createRoute({
      getParentRoute: () => parent,
      path: "/design-system/frame/$entryId/$specimenId",
      component: frame,
    }),
  ] as unknown as AnyRoute[];
}

/** The Shell everywhere except the dev-only gallery, which brings its own chrome. */
function DevRoot(): ReactElement {
  const pathname: string = useRouterState({ select: (state) => withoutBasePath(state.location.pathname) });
  return isDesignSystemPath(pathname) ? <Outlet /> : <Shell />;
}

const rootRoute = createRootRoute({ component: import.meta.env.DEV ? DevRoot : Shell });

/** Reads the project out of the path so the board stays a pure component. */
function WorkProjectBoard() {
  const { projectId } = useParams({ from: "/work/p/$projectId" });
  return <WorkBoard projectId={projectId} />;
}

// `/` is not a screen of its own: Work is the application's home (§12, "the
// hub redirects a successful sign-in to /work"), so the root sends the reader
// there rather than rendering a second, emptier front door.
const homeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/work" });
  },
});

// --- Chat -------------------------------------------------------------------

const newChatRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/chat",
  component: () => <NewChat />,
});

const conversationRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/chat/c/$conversationId",
  component: ConversationRoute,
});

// `/chat/issues/:workItem` was the linked conversation; the issue page is the
// destination now (decisions.md §19.4), so the old path is a redirect rather
// than a second view of the same thing. It is a route-level redirect because
// the work item id is in the path: nothing has to be read to resolve it.
const issueRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/chat/issues/$workItemId",
  beforeLoad: ({ params }) => {
    throw redirect({
      to: "/work/i/$workItemId",
      params: { workItemId: (params as { workItemId: string }).workItemId },
      replace: true,
    });
  },
});

const projectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/chat/p/$projectId",
  component: ProjectNewChat,
});

// --- Work (design inventory A.1, A.2) ---------------------------------------
//
// `/work` is the all-projects board, `/work/p/:project` one project's, and
// `/work/i/:workItem` one issue. The view, the filters, the sort and the lane
// set all live in the query string (`src/app/work/lib/viewState.ts`), not in
// the path, so a filtered board is a link and the back button undoes a filter.
//
// `/work/changes` is declared before the `$projectId` route so the static
// segment cannot be read as a project id.

const workRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work",
  component: () => <WorkBoard projectId={null} />,
});

const workChangesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work/changes",
  component: ChangesPage,
});

const workProjectRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work/p/$projectId",
  component: WorkProjectBoard,
});

const workItemRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work/i/$workItemId",
  component: IssuePage,
});

// One Change Request, under the issue it belongs to: the hub addresses a
// change through its primary work item, so the URL does too.
const workChangeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/work/i/$workItemId/changes/$changeId",
  component: ChangeRequestPage,
});

// --- Support ----------------------------------------------------------------
//
// `/support` is the one screen a Detent staff account reaches before it has
// any organization access at all, so it renders without the bootstrap; the
// entry point mounts it directly when the bootstrap is refused (§12).

const supportRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/support",
  component: SupportRoute,
});

const supportOrganizationRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/support/$organization",
  component: SupportRoute,
});

const routeTree = rootRoute.addChildren([
  homeRoute,
  newChatRoute,
  conversationRoute,
  issueRoute,
  projectRoute,
  workRoute,
  workChangesRoute,
  workProjectRoute,
  workItemRoute,
  workChangeRoute,
  supportRoute,
  supportOrganizationRoute,
  ...accountRoutes(rootRoute),
  // The literal guard (not just the parameter) is what lets the production
  // build drop `designSystemRoutes` and its dynamic import entirely.
  ...(import.meta.env.DEV ? designSystemRoutes(rootRoute as unknown as AnyRoute, true) : []),
] as unknown as AnyRoute[]);

/** `history` is for tests, which have no browser location to navigate. */
export function makeRouter(history?: RouterHistory, basepath: string = routerBasePath()) {
  return createRouter({ routeTree, basepath, ...plainSearchOptions, ...(history === undefined ? {} : { history }) });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof makeRouter>;
  }
}
