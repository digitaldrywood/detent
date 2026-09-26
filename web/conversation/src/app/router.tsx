// Routes.
//
// The hub serves the application shell for every non-API path
// (decisions.md §12, "Serving"), so the router's basepath is `/` and every
// path below is absolute. Chat lives under `/chat`, Work under `/work`, and
// `routes.account.tsx` adds the account, settings, project and fleet screens.
import {
  createRootRoute,
  createRoute,
  createRouter,
  redirect,
  useParams,
  type AnyRoute,
  type RouterHistory,
} from "@tanstack/react-router";

import { ConversationRoute, NewChat, ProjectNewChat, Shell } from "./App.tsx";
import { accountRoutes } from "./routes.account.tsx";
import { SupportRoute } from "./account/Support.tsx";
import { ChangesPage } from "./work/ChangesPage.tsx";
import { IssuePage } from "./work/IssuePage.tsx";
import { WorkBoard } from "./work/WorkBoard.tsx";

const rootRoute = createRootRoute({ component: Shell });

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
  supportRoute,
  supportOrganizationRoute,
  ...accountRoutes(rootRoute),
] as unknown as AnyRoute[]);

/** `history` is for tests, which have no browser location to navigate. */
export function makeRouter(history?: RouterHistory) {
  return createRouter({ routeTree, basepath: "/", ...(history === undefined ? {} : { history }) });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof makeRouter>;
  }
}
