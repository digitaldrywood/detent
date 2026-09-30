// @vitest-environment jsdom
import { render } from "@testing-library/react";
import type React from "react";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { vi } from "vitest";

import ThreadSidebar from "../../src/components/Sidebar.tsx";
import { SidebarProvider } from "../../src/components/ui/sidebar.tsx";
import {
  publishSidebarData,
  SidebarDataProvider,
  type SidebarShellData,
} from "../../src/app/adapters/sidebarData.tsx";
import { useSettledOverrideStore } from "../../src/app/adapters/settledOverrides.ts";
import { ToastProvider } from "../../src/components/ui/toast.tsx";
import type { Conversation } from "../../src/contracts/index.ts";

export const PROJECTS = [
  { id: "proj_alpha", name: "alpha", can_write: true },
  { id: "proj_beta", name: "beta", can_write: true },
] as const;

export interface SidebarHarness {
  readonly onSelect: ReturnType<typeof vi.fn>;
  readonly onNewChat: ReturnType<typeof vi.fn>;
  readonly onNavigate: ReturnType<typeof vi.fn>;
  readonly navigated: string[];
}

/** Every shelf and override the sidebar persists, back to its first-run state. */
export function resetSidebarState(): void {
  window.localStorage.clear();
  useSettledOverrideStore.setState({ settledAtById: {}, unsettledAtById: {} });
}

export async function renderSidebar(
  overrides: Partial<SidebarShellData> = {},
  options: { readonly path?: string } = {},
): Promise<SidebarHarness> {
  const onSelect = vi.fn();
  const onNewChat = vi.fn();
  const onNavigate = vi.fn();
  const navigated: string[] = [];

  const data: SidebarShellData = {
    organizationName: "Threefold",
    projects: [...PROJECTS],
    activeProjectId: "proj_alpha",
    onProjectChange: vi.fn(),
    conversations: [],
    serverResults: [],
    activeConversationId: null,
    onSelect,
    onNewChat,
    onRename: vi.fn(async () => undefined),
    attention: new Set<string>(),
    navigation: { activePath: options.path ?? "/chat", onNavigate },
    ...overrides,
  };
  publishSidebarData(data);

  const Mounted = (): React.ReactElement => (
    <ToastProvider>
      <SidebarDataProvider value={data}>
        <SidebarProvider>
          <ThreadSidebar />
        </SidebarProvider>
      </SidebarDataProvider>
    </ToastProvider>
  );
  const root = createRootRoute();
  // Every route the copied sidebar navigates to renders the sidebar, so a
  // click resolves rather than throwing and the sidebar stays mounted across
  // it: the conversation route, the new-chat surface, the project scope, and
  // the footer's three destinations.
  const children = [
    "/chat",
    "/chat/c/$conversationId",
    "/chat/p/$projectId",
    "/settings",
    "/usage",
    "/work",
    "/work/changes",
  ].map((path) =>
    createRoute({ getParentRoute: () => root, path, component: Mounted }),
  );
  const router = createRouter({
    routeTree: root.addChildren(children),
    history: createMemoryHistory({ initialEntries: [options.path ?? "/chat"] }),
  });
  router.subscribe("onResolved", (event) => {
    navigated.push(event.toLocation.pathname);
  });
  // The router resolves its first match asynchronously; without this the
  // provider's first paint is the pending frame and nothing is in the DOM.
  await (router as unknown as { load: () => Promise<void> }).load();
  render(<RouterProvider router={router as never} />);
  return { onSelect, onNewChat, onNavigate, navigated };
}

/** The row for a conversation, card or slim, as the copied sidebar renders it. */
export function rowFor(
  container: HTMLElement,
  title: string,
): HTMLElement | null {
  for (const row of container.querySelectorAll<HTMLElement>(
    "[data-testid='sidebar-row-card'], [data-testid='sidebar-row-slim']",
  )) {
    if (row.textContent?.includes(title)) return row;
  }
  return null;
}

export type { Conversation };
