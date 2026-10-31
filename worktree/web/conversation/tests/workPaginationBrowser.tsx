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
import { SidebarSearchProvider } from "../src/app/adapters/sidebarData.tsx";
import { Input } from "../src/components/ui/input.tsx";
import { SidebarProvider } from "../src/components/ui/sidebar.tsx";
import { workPaginationFixture } from "./workPaginationFixture.ts";
import { IssueCard } from "../src/app/work/components/IssueCard.tsx";
import { toAttemptView, toWorkItemView } from "../src/app/work/lib/fromWire.ts";
import type { NativeAttempt, NativeIssue } from "../src/contracts/work.ts";
import itemFixture from "../src/contracts/fixtures/work-item.json";
import attemptsFixture from "../src/contracts/fixtures/work-attempt-list.json";

function CardFixture() {
  const [title, setTitle] = React.useState("fix(migrations): canonical activity count helper uses legacy attendance code");
  const [updated, setUpdated] = React.useState(0);
  const [opened, setOpened] = React.useState(false);
  const [moved, setMoved] = React.useState("");
  const [imported, setImported] = React.useState(false);
  const now = Date.parse("2026-09-09T12:00:00Z");
  const item = {
    ...toWorkItemView(itemFixture as unknown as NativeIssue, "detent"),
    number: 13, identifier: "#13", title, priority: "High", state: "Human Review", terminal: false, blockedBy: [],
    updatedAt: "2026-09-09T10:00:00Z", lastActivityAt: new Date(now + updated * 1000).toISOString(),
    attempt: { ...toAttemptView((attemptsFixture.items as unknown as NativeAttempt[]).slice(0, 1))!,
      attemptNumber: imported ? 444444 : 4, status: "failed", running: false },
    sourceProvider: imported ? "github" : null,
  };
  return <div className="flex min-h-screen flex-col items-start gap-4 bg-background p-6 text-foreground">
    <div style={{ width: 320 }} data-testid="card-fixture">
      <IssueCard item={item} showProject={false} now={now} moves={["Todo", "In Progress"]}
        onMove={(_, state) => setMoved(state)} onOpen={() => setOpened(true)} />
    </div>
    <button onClick={() => setTitle("migration".repeat(32))}>Use unbroken title</button>
    <button onClick={() => setUpdated((value) => value + 1)}>Refresh card</button>
    <button onClick={() => setImported(true)}>Use imported metadata</button>
    <output aria-label="Card action">{opened ? "Opened" : moved}</output>
  </div>;
}

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
  if (globalThis.location.search.includes("card")) {
    createRoot(document.getElementById("app")!).render(<CardFixture />);
    return;
  }
  const fixture = workPaginationFixture();
  await fixture.control(globalThis.location.search.includes("backlog") ? { backlogOverflow: true } : { open79: true });
  globalThis.fetch = fixture.fetch as typeof globalThis.fetch;
  Reflect.set(globalThis, "EventSource", undefined);
  const root = createRootRoute({ component: () => <SidebarProvider className="h-full min-h-0 flex-1 flex-col"><SidebarSearchProvider work><aside className="dc-side"><Input nativeInput type="search" aria-label="Search threads" /></aside><Outlet /></SidebarSearchProvider></SidebarProvider> });
  const work = createRoute({ getParentRoute: () => root, path: "/work", component: () => <WorkBoard projectId={null} /> });
  const router = createRouter({ routeTree: root.addChildren([work]), history: createMemoryHistory({ initialEntries: ["/work"] }) });
  Reflect.set(globalThis, "workFixture", { ...fixture, router });
  createRoot(document.getElementById("app")!).render(
    <ClientContext.Provider value={fixture.client}><RouterProvider router={router} /></ClientContext.Provider>,
  );
}

void mount();
