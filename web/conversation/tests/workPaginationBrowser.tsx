import { createRoot } from "react-dom/client";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";

import { ClientContext } from "../src/app/client.ts";
import { WorkBoard } from "../src/app/work/WorkBoard.tsx";
import { workPaginationFixture } from "./workPaginationFixture.ts";

async function mount() {
  const fixture = workPaginationFixture();
  await fixture.control(globalThis.location.search.includes("backlog") ? { backlogOverflow: true } : { open79: true });
  globalThis.fetch = fixture.fetch as typeof globalThis.fetch;
  Reflect.set(globalThis, "EventSource", undefined);
  const root = createRootRoute({ component: Outlet });
  const work = createRoute({ getParentRoute: () => root, path: "/work", component: () => <WorkBoard projectId={null} /> });
  const router = createRouter({ routeTree: root.addChildren([work]), history: createMemoryHistory({ initialEntries: ["/work"] }) });
  Reflect.set(globalThis, "workFixture", { ...fixture, router });
  createRoot(document.getElementById("app")!).render(
    <ClientContext.Provider value={fixture.client}><RouterProvider router={router} /></ClientContext.Provider>,
  );
}

void mount();
