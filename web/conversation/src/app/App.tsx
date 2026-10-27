import { SavedViewsProvider, useSavedViews } from "./work/lib/savedViews.tsx";
import { conversationBus } from "../runtime/state/conversationList.ts";
// Application shell.
import { useAtomSet, useAtomValue } from "@effect/atom-react";
import * as Option from "effect/Option";
import { AsyncResult } from "effect/unstable/reactivity";
import React from "react";
import { Outlet, useNavigate, useRouter, useRouterState } from "@tanstack/react-router";

import type { Conversation } from "../contracts/index.ts";
import { HUB_ENVIRONMENT_ID } from "../contracts/index.ts";
import type { ConversationClient } from "../runtime/bootstrap.ts";
import { readLastProject, useClient, writeLastProject } from "./client.ts";
import { HealthFindingsProvider } from "./diagnostics/healthFindings.tsx";
import { useSidebarFindings } from "./adapters/sidebarFindings.ts";
import { useSidebarHumanActions } from "./adapters/sidebarHumanActions.ts";
import { ProjectSetupProvider } from "./account/projectSetup.tsx";
import { SidebarDataProvider } from "./adapters/sidebarData.tsx";
import { sidebarProjectScopeKey } from "./adapters/shell.ts";
import { activateSidebarProjectScope, setSidebarProjectWorkingSetKeys, setSidebarProjectScopeKey, useUiStateStore } from "../uiStateStore.ts";
import { publishPaletteShell } from "./adapters/paletteContext.ts";
import { CommandPalette } from "./components/CommandPalette.tsx";
import { NewProjectProvider } from "./projects/NewProject.tsx";
import { NewIssueProvider, useNewIssue } from "./work/NewIssue.tsx";
import { AnchoredToastProvider, ToastProvider } from "../components/ui/toast.tsx";
import {
  focusSidebarSearch,
  SidebarSearchProvider,
  publishSidebarData,
  useSidebarSearchQuery,
  type SidebarShellData,
} from "./adapters/sidebarData.tsx";
import { SidebarInset, useSidebar } from "../components/ui/sidebar.tsx";
import { OrganizationMembersProvider } from "./account/OrganizationMembers.tsx";
import { AppSidebarLayout } from "../components/AppSidebarLayout.tsx";
import { ReporterShell } from "./reporter/ReporterShell.tsx";
import {
  chipStamp,
  LIVE_TOOLTIP,
  staleClientTooltip,
} from "./lib/connectionTooltips.ts";
import { dialogOpen, shortcutFor } from "./lib/shortcuts.ts";
import { conversationDestination } from "./lib/conversationDestination.ts";
import { shouldSearchServer } from "./lib/sidebarLogic.ts";
import { routerBasePath, withoutBasePath } from "../runtime/basePath.ts";

export interface ConnectionChip {
  readonly tone: "dc-ok" | "dc-warn" | "dc-err" | "";
  readonly label: string;
  readonly detail: string | null;
  /** Offered when the client is not live: a manual reconnect and re-read. */
  readonly action?: { readonly label: string; readonly onClick: () => void } | null;

  readonly tooltip: string;
}

export interface ShellState {
  readonly projectId: string;
  readonly newChatSubjectId: string | null;
  readonly setProjectId: (id: string) => void;
  readonly conversations: readonly Conversation[];
  readonly connection: ConnectionChip;
  readonly refreshList: () => void;
}

const ShellContext = React.createContext<ShellState | null>(null);

/** The shell's state, for the route surfaces it renders through its outlet. */
export function useOptionalShell(): ShellState | null {
  return React.useContext(ShellContext);
}

export function useShell(): ShellState {
  const shell = useOptionalShell();
  if (shell === null) throw new Error("The shell is not mounted.");
  return shell;
}

/**
 * The transport chip. Every degraded state names when
 * the data on screen was current and offers the one action that fixes it, so
 * a reader is never left guessing whether what they see is live (U05).
 */
export function connectionChip(
  status: string,
  asOf: number | null,
  onRetry: () => void,
): ConnectionChip {
  const stamp = chipStamp(asOf);
  switch (status) {
    case "live":
      return {
        tone: "dc-ok",
        label: "Live",
        detail: stamp.length > 0 ? `data current · ${stamp}` : "data current",
        action: null,
        tooltip: LIVE_TOOLTIP,
      };
    case "synchronizing":
      return {
        tone: "dc-warn",
        label: "Reconnecting",
        detail: stamp.length > 0 ? `data as of ${stamp}` : "reading the latest state",
        action: { label: "Retry now", onClick: onRetry },
        tooltip: staleClientTooltip(
          "Reconnecting",
          stamp,
          "waiting for the hub's event stream",
        ),
      };
    case "cached":
      return {
        tone: "dc-err",
        label: "Offline",
        detail: stamp.length > 0 ? `data as of ${stamp}` : "showing cached data",
        action: { label: "Retry now", onClick: onRetry },
        tooltip: staleClientTooltip("Offline", stamp, "showing cached data"),
      };
    default:
      return {
        tone: "",
        label: "Connecting",
        detail: null,
        action: null,
        tooltip: "Connecting: opening this client's event stream to the hub.",
      };
  }
}

function useListState(client: ConversationClient) {
  const result = useAtomValue(client.list.stateAtom(HUB_ENVIRONMENT_ID));
  return Option.getOrUndefined(AsyncResult.value(result));
}

export function Shell(): React.ReactElement {
  const client = useClient();
  const pathname = useRouterState({ select: (state) => withoutBasePath(state.location.pathname) });
  if (client.account?.actor.role === "reporter") return <ToastProvider><ReporterShell /></ToastProvider>;

  return (
    <ToastProvider>
      <AnchoredToastProvider>
        <OrganizationMembersProvider enabled={/^\/settings(\/|$)/.test(pathname)}>
          <HealthFindingsProvider projects={client.bootstrap.projects}>
            <SavedViewsProvider key={JSON.stringify([client.http.origin, client.http.apiBase, client.bootstrap.organization.id, client.bootstrap.actor?.principal_id])}>
              <ShellBody />
            </SavedViewsProvider>
          </HealthFindingsProvider>
        </OrganizationMembersProvider>
      </AnchoredToastProvider>
    </ToastProvider>
  );
}

function ShellBody(): React.ReactElement {
  const client = useClient();
  const navigate = useNavigate();
  const list = useListState(client);
  const search = useAtomSet(client.searchConversations, { mode: "promise" });
  const refreshList = useAtomSet(client.refreshList, { mode: "promise" });
  const params = useRouterState({ select: (state) => state.matches[state.matches.length - 1]?.params ?? {} }) as { conversationId?: string; projectId?: string; project?: string; workItemId?: string; viewId?: string };
  const savedView = useSavedViews()?.views.find((view) => view.id === params.viewId);
  // The Work destination and the Browse group need to know where they are.
  // `useRouterState` is the only source of the current path that stays correct
  // through a redirect; `params` alone cannot tell `/work` from `/work/changes`.
  const context = (useRouterState({ select: (state) => state.matches[state.matches.length - 1]?.context }) ?? {}) as { resolvedProjectId?: string; resolvedWorkItemId?: string };
  const routePath = useRouterState({ select: (state) => withoutBasePath(state.matches[state.matches.length - 1]?.pathname ?? state.location.pathname, routerBasePath()) });
  const activePath = context.resolvedWorkItemId ? `/work/i/${context.resolvedWorkItemId}` : context.resolvedProjectId ? `/work/p/${context.resolvedProjectId}` : routePath;

  const [newChatSubjectId, setNewChatSubjectId] = React.useState<string | null>(null);
  React.useEffect(() => {
    if (activePath !== "/chat") {
      setNewChatSubjectId(context.resolvedWorkItemId || params.workItemId || null);
    }
  }, [activePath, params.workItemId, context.resolvedWorkItemId]);

  const projects = client.bootstrap.projects;
  const findings = useSidebarFindings(projects);
  const humanActions = useSidebarHumanActions(projects);
  const [selectedProjectId, setProjectIdState] = React.useState(
    () => readLastProject() ?? projects[0]?.id ?? "",
  );
  React.useLayoutEffect(() => {
    const remembered = readLastProject();
    if (remembered) writeLastProject(remembered);
  }, []);
  const scopeAccount = JSON.stringify([client.http.origin, client.http.apiBase, client.bootstrap.organization.id, client.bootstrap.actor?.principal_id]);
  const scopeAccountKey = useUiStateStore((state) => state.sidebarProjectScopeAccountKey);
  const focusKey = useUiStateStore((state) => state.sidebarProjectScopeKey);
  React.useLayoutEffect(() => {
    useUiStateStore.setState((state) => activateSidebarProjectScope(state, scopeAccount));
  }, [scopeAccount]);
  const focusedProjectId = projects.find((project) => sidebarProjectScopeKey(project) === focusKey)?.id ?? "";
  const isWorkBoard = activePath === "/work" || activePath.startsWith("/work/p/");
  const [projectScopePath, setProjectScopePath] = React.useState<string | null>(null);
  const enteringWorkBoard = isWorkBoard && projectScopePath !== routePath;
  const workBoardPath = isWorkBoard ? routePath : null;
  const savedProjectId = savedView === undefined ? undefined : savedView.project_id ?? "";
  const requestedProject = routePath === "/chat" || (isWorkBoard && !enteringWorkBoard)
    ? focusedProjectId
    : params.project ?? params.projectId ?? context.resolvedProjectId ?? savedProjectId ?? ((activePath === "/work" || activePath === "/chat") ? focusedProjectId : selectedProjectId);
  const projectId = client.account?.projects.find((project) => project.id === requestedProject || project.slug === requestedProject)?.id ?? requestedProject;
  React.useEffect(() => {
    if (scopeAccountKey !== scopeAccount) return;
    setProjectScopePath(routePath);
    setProjectIdState(projectId);
    writeLastProject(projectId);
    const project = projects.find((candidate) => candidate.id === projectId);
    useUiStateStore.setState((state) => {
      const key = project === undefined ? null : sidebarProjectScopeKey(project);
      return setSidebarProjectScopeKey(savedView === undefined ? state : setSidebarProjectWorkingSetKeys(state, key === null ? [] : [key]), key);
    });
  }, [projectId, projects, workBoardPath, scopeAccount, scopeAccountKey, savedView]);
  // The copied `components/Sidebar.tsx` owns its search box and searches its
  // own list by title (`Sidebar.logic.ts`). The shell reads that query back
  // off the input so it can ask the hub for conversations past the first page
  // and merge them into the list the sidebar searches
  // (`state/entities.ts` `useThreadShells`).
  const query = useSidebarSearchQuery();
  const [serverResults, setServerResults] = React.useState<readonly Conversation[]>([]);
  React.useEffect(() => conversationBus.subscribe((update) => {
    if ("removed" in update || update.archived) {
      setServerResults((current) => current.filter((conversation) => conversation.id !== update.id));
    }
  }), []);
  const [asOf, setAsOf] = React.useState<number | null>(null);

  const setProjectId = React.useCallback((id: string) => {
    setProjectIdState(id);
    writeLastProject(id);
  }, []);

  // The "as of" stamp is the last moment the client knew the list was current.
  React.useEffect(() => {
    if (list?.status === "live") setAsOf(Date.now());
  }, [list?.status, list?.conversations]);

  // Navigation refreshes the list: there is no organization-wide stream.
  React.useEffect(() => {
    void refreshList();
  }, [refreshList, params.conversationId]);

  React.useEffect(() => {
    if (!shouldSearchServer(query)) {
      setServerResults([]);
      return;
    }
    let cancelled = false;
    const timer = setTimeout(() => {
      void search({ query: query.trim() }).then((result) => {
        if (cancelled) return;
        setServerResults(result._tag === "Success" ? result.success.conversations : []);
      });
    }, 200);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [query, search]);

  const startNewChat = React.useCallback(() => {
    void navigate({ to: "/chat" });
  }, [navigate]);

  const conversations = list?.conversations ?? [];
  const attention = React.useMemo(
    () =>
      new Set(
        conversations
          .filter((conversation) => conversation.execution.status === "waiting_input")
          .map((conversation) => conversation.id),
      ),
    [conversations],
  );
  const connection = connectionChip(list?.status ?? "empty", asOf, () => void refreshList());

  const shell: ShellState = {
    projectId,
    newChatSubjectId,
    setProjectId,
    conversations,
    connection,
    refreshList: () => void refreshList(),
  };

  const rename = useAtomSet(client.renameConversation, { mode: "promise" });
  const router = useRouter();
  const activeProjectId = projectId === "" ? null : projectId;
  const prefetch = React.useCallback((to: string) => {
    void import("./lib/routePrefetch.ts").then(({ prefetchRoute }) => prefetchRoute(to, { router, client, activeProjectId }));
  }, [activeProjectId, client, router]);

  const [projectSwitcherOpen, setProjectSwitcherOpen] = React.useState(false);
  const sidebarProps: SidebarShellData = {
    projectSwitcher: { open: projectSwitcherOpen, setOpen: setProjectSwitcherOpen },
    organizationName: client.bootstrap.organization.name,
    projects,
    activeProjectId: projectId === "" ? null : projectId,
    onProjectChange: (id: string) => {
      setProjectId(id);
      if (isWorkBoard) return;
      if (activePath === "/reports" || activePath.startsWith("/reports/")) {
        writeLastProject(id);
        void navigate({
          to: activePath,
          search: (previous: Record<string, unknown>) => ({ ...previous, project: id === "" ? "all" : id, projects: undefined }),
        } as never);
      } else if (activePath.startsWith("/work") && activePath !== "/work") {
        void navigate({ to: "/work", search: (previous: Record<string, unknown>) => previous } as never);
      } else {
        void navigate({ to: id === "" ? "/chat" : `/chat/p/${id}` });
      }
    },
    conversations,
    serverResults,
    activeConversationId: params.conversationId ?? null,
    // The rename both the copied sidebar and the copied header dispatch.
    // Their signature carries no project, so the shell supplies it: the
    // conversation's own where the list holds it, the open one otherwise.
    onRename: async (conversationId: string, title: string) => {
      const conversation = conversations.find((candidate) => candidate.id === conversationId);
      const project = conversation?.project_id ?? projectId;
      if (project === "") return;
      const result = await rename({ projectId: project, conversationId, title });
      if (result._tag === "Failure") {
        throw result.failure instanceof Error
          ? result.failure
          : new Error(String(result.failure));
      }
    },
    // A linked conversation's destination is its issue page with the chat
    // open beside it; an unlinked one is still a chat.
    onSelect: (conversation) => {
      setProjectId(conversation.project_id);
      void navigate(conversationDestination(conversation));
    },
    onNewChat: startNewChat,
    preferenceChoices: client.bootstrap.preferences,
    navigation: {
      activePath,
      onNavigate: (to: string) => void navigate({ to }),
      onPrefetch: prefetch,
    },
    attention,
    findings,
    humanActions,
  };

  // `state/entities.ts` reads the same value outside React, for the one path
  // the copied sidebar takes without a hook (`readThreadShell`).
  publishSidebarData(sidebarProps);

  return (
    <ShellContext.Provider value={shell}>

      <ProjectSetupProvider projectId={projectId}>
      <SidebarDataProvider key={scopeAccount} value={sidebarProps}>
        <SidebarSearchProvider work={activePath === "/work" || activePath.startsWith("/work/")}>

        <NewProjectProvider>
          <NewIssueProvider>
            <CommandPalette>
              <AppSidebarLayout>

                <ShellShortcuts onNewChat={startNewChat} />
                <SidebarInset className="dc-main min-h-0 overflow-hidden">
                  <Outlet />
                </SidebarInset>
              </AppSidebarLayout>
            </CommandPalette>
          </NewIssueProvider>
        </NewProjectProvider>
        </SidebarSearchProvider>
      </SidebarDataProvider>
      </ProjectSetupProvider>
    </ShellContext.Provider>
  );
}

function ShellShortcuts({ onNewChat }: { readonly onNewChat: () => void }): null {
  const { isMobile, openMobile, setOpenMobile, toggleSidebar } = useSidebar();
  const { openNewIssue } = useNewIssue();

  React.useEffect(() => publishPaletteShell({ toggleSidebar }), [toggleSidebar]);

  // The copied sidebar owns its search box and exposes no ref, so `/` finds it
  // the way a reader would (`adapters/sidebarData.tsx` `focusSidebarSearch`).
  const focusSearch = React.useCallback(() => {
    if (isMobile && !openMobile) {
      setOpenMobile(true);
      // The sheet mounts and takes focus itself; the box exists on the next
      // frame, and this is the frame after that.
      requestAnimationFrame(() => {
        requestAnimationFrame(() => focusSidebarSearch());
      });
      return;
    }
    focusSidebarSearch();
  }, [isMobile, openMobile, setOpenMobile]);

  React.useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const shortcut = shortcutFor(event);
      if (shortcut === null) return;
      // A single key belongs to whatever dialog is open, the palette included.
      if (shortcut === "new-issue" && dialogOpen(globalThis.document)) return;
      event.preventDefault();
      if (shortcut === "focus-search") focusSearch();
      else if (shortcut === "new-issue") openNewIssue();
      else {
        if (isMobile) setOpenMobile(false);
        onNewChat();
      }
    };
    globalThis.document?.addEventListener("keydown", onKeyDown);
    return () => globalThis.document?.removeEventListener("keydown", onKeyDown);
  }, [focusSearch, onNewChat, openNewIssue, isMobile, setOpenMobile]);

  return null;
}

/** The new-chat surface: one composer, centred, docking after the first send. */
