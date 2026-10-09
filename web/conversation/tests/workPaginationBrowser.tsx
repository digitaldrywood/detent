import React from "react";
import type { ConnectionChip } from "../src/app/App.tsx";
import { ChevronDownIcon, PlusIcon, SparklesIcon } from "lucide-react";
import { WorkTopBar } from "../src/app/work/components/WorkTopBar.tsx";
import { CompletedCounter } from "../src/app/work/components/CompletedCounter.tsx";
import { Button, SplitButton } from "../src/components/ui/button.tsx";
import { boardConnectionChip } from "../src/app/work/lib/freshness.ts";
import { createRoot } from "react-dom/client";
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";

import { ClientContext } from "../src/app/client.ts";
import { WorkBoard } from "../src/app/work/WorkBoard.tsx";
import { SidebarProvider } from "../src/components/ui/sidebar.tsx";
import { workPaginationFixture } from "./workPaginationFixture.ts";

function HeaderProbe() {
  const [state, setState] = React.useState({ label: "Live", reload: false });
  Reflect.set(globalThis, "setHeaderState", setState);
  const app: ConnectionChip = { tone: state.label === "Reconnecting" ? "dc-warn" : "dc-ok", label: state.label, detail: null, tooltip: "Fixture" };
  const chip = boardConnectionChip({ app, streaming: state.label !== "Not streaming", loading: state.label === "Loading", refreshing: state.label === "Updating", cached: state.label === "Cached", asOf: Date.UTC(2026, 9, 7, 13, 4), onReload: () => {} });
  return <WorkTopBar context="Work" title="All projects"
    completed={<CompletedCounter count={146} onCompletedWindowChange={() => {}} loading={false} />}
    connection={{ ...chip, action: state.reload ? { label: "Reload", onClick: () => {} } : null }}
    actions={<Button size="sm" className="hidden md:inline-flex" data-testid="board-new-issue"><PlusIcon />New issue</Button>}
    mobileActions={<SplitButton className="md:hidden" aria-label="Work actions"><Button><SparklesIcon />Ask</Button><Button size="icon" aria-label="More work actions"><ChevronDownIcon /></Button></SplitButton>}
  />;
}

async function mount() {
  if (globalThis.location.search.includes("header")) {
    createRoot(document.getElementById("app")!).render(<HeaderProbe />);
    return;
  }
  const fixture = workPaginationFixture();
  await fixture.control(globalThis.location.search.includes("backlog") ? { backlogOverflow: true } : { open79: true });
  globalThis.fetch = fixture.fetch as typeof globalThis.fetch;
  Reflect.set(globalThis, "EventSource", undefined);
  const root = createRootRoute({ component: () => <SidebarProvider className="h-full min-h-0 flex-1 flex-col"><Outlet /></SidebarProvider> });
  const work = createRoute({ getParentRoute: () => root, path: "/work", component: () => <WorkBoard projectId={null} /> });
  const router = createRouter({ routeTree: root.addChildren([work]), history: createMemoryHistory({ initialEntries: ["/work"] }) });
  Reflect.set(globalThis, "workFixture", { ...fixture, router });
  createRoot(document.getElementById("app")!).render(
    <ClientContext.Provider value={fixture.client}><RouterProvider router={router} /></ClientContext.Provider>,
  );
}

void mount();
