// Searches Detent conversations and destinations and dispatches available commands.
import { useAtomValue } from "@effect/atom-react";
import { useNavigate, useRouterState } from "@tanstack/react-router";
import {
  FileSearchIcon,
  FolderIcon,
  FolderPlusIcon,
  GitPullRequestIcon,
  LayoutListIcon,
  LinkIcon,
  MessageSquareIcon,
  PaletteIcon,
  PanelLeftIcon,
  PanelRightIcon,
  PlusIcon,
  SettingsIcon,
  SplitIcon,
  SquarePenIcon,
  TextSearchIcon,
  ChartColumnIcon,
  ArrowLeftIcon,
} from "lucide-react";
import React, {
  useCallback,
  useDeferredValue,
  useEffect,
  useMemo,
  useReducer,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";

import {
  ADDON_ICON_CLASS,
  ITEM_ICON_CLASS,
  RECENT_THREAD_LIMIT,
  buildProjectActionItems,
  buildRootGroups,
  buildThreadActionItems,
  enumerateCommandPaletteItems,
  filterCommandPaletteGroups,
  getCommandPaletteInputPlaceholder,
  getCommandPaletteMode,
  reduceCommandPaletteUiState,
  type CommandPaletteActionItem,
  type CommandPaletteProject,
  type CommandPaletteSubmenuItem,
  type CommandPaletteView,
  type SearchOverlayMode,
} from "../../components/CommandPalette.logic.ts";
import { CommandPaletteContent } from "../../components/CommandPaletteContent.tsx";
import { CommandPaletteResults } from "../../components/CommandPaletteResults.tsx";
import { ThreadCommandSubtitle } from "../../components/ThreadCommandSubtitle.tsx";
import { CommandDialog, CommandDialogPopup } from "../../components/ui/command.tsx";
import { stackedThreadToast, toastManager } from "../../components/ui/toast.tsx";
import { onOpenCommandPalette } from "../../commandPaletteBus.ts";
import {
  SETTINGS_SECTION_LABELS,
  searchSettings,
} from "../../components/settings/settingsSearch.ts";
import { useAvailableSettingsSearchItems } from "../../components/settings/useAvailableSettingsSearchItems.ts";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";
import type { Conversation } from "../../contracts/conversation.ts";
import { resolveShortcutCommand } from "../../keybindings.ts";
import { cn } from "../../lib/utils.ts";
import { primaryServerKeybindingsAtom } from "../../state/server.ts";
import type { SidebarThreadSummary } from "../../types.ts";
import { usePaletteContext, usePaletteShell } from "../adapters/paletteContext.ts";
import { conversationDestination } from "../lib/conversationDestination.ts";
import { useSidebarData } from "../adapters/sidebarData.tsx";
import { toEnvironmentProject } from "../adapters/shell.ts";
import { toEnvironmentThreadShell } from "../adapters/sidebarThreads.ts";
import { ProjectGlyph } from "./ProjectGlyph.tsx";
import { useWorkIssueItems } from "../work/lib/usePaletteIssues.ts";

const OVERLAY_MODE_BY_COMMAND = {
  "commandPalette.toggle": "command",
} as const satisfies Partial<Record<string, SearchOverlayMode>>;

function overlayModeForCommand(command: string | null): SearchOverlayMode | null {
  if (command === null) return null;
  return command in OVERLAY_MODE_BY_COMMAND
    ? OVERLAY_MODE_BY_COMMAND[command as keyof typeof OVERLAY_MODE_BY_COMMAND]
    : null;
}

export function CommandPalette({ children }: { children: ReactNode }) {
  const [state, dispatch] = useReducer(reduceCommandPaletteUiState, {
    open: false,
    mode: "command",
    openIntent: null,
  });
  const setOpen = useCallback((open: boolean) => dispatch({ _tag: "SetOpen", open }), []);
  const toggleMode = useCallback(
    (mode: SearchOverlayMode) => dispatch({ _tag: "ToggleMode", mode }),
    [],
  );
  const openNewThreadIn = useCallback(() => dispatch({ _tag: "OpenNewThreadIn" }), []);
  const clearOpenIntent = useCallback(() => dispatch({ _tag: "ClearOpenIntent" }), []);
  const keybindings = useAtomValue(primaryServerKeybindingsAtom);

  const openerRef = React.useRef<HTMLElement | null>(null);
  const rememberOpener = useCallback(() => {
    const active = document.activeElement;
    openerRef.current = active instanceof HTMLElement ? active : null;
  }, []);

  useEffect(() => {
    const onKeyDown = (event: globalThis.KeyboardEvent) => {
      if (event.defaultPrevented) return;
      const command = resolveShortcutCommand(event, keybindings);
      const mode = overlayModeForCommand(command);
      if (mode === null) {
        return;
      }
      event.preventDefault();
      event.stopPropagation();
      rememberOpener();
      toggleMode(mode);
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [keybindings, rememberOpener, toggleMode]);

  useEffect(
    () =>
      onOpenCommandPalette((detail) => {
        rememberOpener();
        if (detail.open === "new-thread-in") {
          openNewThreadIn();
        } else {
          setOpen(true);
        }
      }),
    [openNewThreadIn, rememberOpener, setOpen],
  );

  return (
    <CommandDialog open={state.open} onOpenChange={(open) => setOpen(open)}>
      {/* Block background focus calls for the entire time the palette is open. */}
      <div className="contents" inert={state.open}>
        {children}
      </div>
      <CommandPaletteDialog
        mode={state.mode}
        openIntent={state.openIntent}
        setOpen={setOpen}
        clearOpenIntent={clearOpenIntent}
        openerRef={openerRef}
      />
    </CommandDialog>
  );
}

function CommandPaletteDialog(props: {
  readonly mode: SearchOverlayMode;
  readonly openIntent: { readonly kind: "add-project" | "new-thread-in" } | null;
  readonly setOpen: (open: boolean) => void;
  readonly clearOpenIntent: () => void;
  readonly openerRef: React.RefObject<HTMLElement | null>;
}) {
  return (
    <CommandDialogPopup
      aria-label="Command palette"
      finalFocus={() => {
        const opener = props.openerRef.current;
        if (opener !== null && opener.isConnected) {
          opener.focus();
          return false;
        }
        return true;
      }}
      className={cn("overflow-hidden p-0")}
      data-command-palette="true"
      data-palette-mode={props.mode}
      data-testid="command-palette"
      onBackdropPointerDown={() => {
        props.setOpen(false);
      }}
    >
      <OpenCommandPaletteDialog
        openIntent={props.openIntent}
        setOpen={props.setOpen}
        clearOpenIntent={props.clearOpenIntent}
      />
    </CommandDialogPopup>
  );
}

function toPaletteProject(project: {
  readonly id: string;
  readonly name: string;
}): CommandPaletteProject {
  return {
    ...toEnvironmentProject({ id: project.id, name: project.name, can_write: true }),
    displayName: project.name,
  };
}

function toPaletteThread(conversation: Conversation): SidebarThreadSummary {
  return toEnvironmentThreadShell(conversation);
}

function OpenCommandPaletteDialog(props: {
  readonly openIntent: { readonly kind: "add-project" | "new-thread-in" } | null;
  readonly setOpen: (open: boolean) => void;
  readonly clearOpenIntent: () => void;
}) {
  const { clearOpenIntent, openIntent, setOpen } = props;
  const navigate = useNavigate();
  const pathname = useRouterState({ select: (routerState) => routerState.location.pathname });
  const [query, setQuery] = useState("");
  const deferredQuery = useDeferredValue(query);
  const isActionsOnly = deferredQuery.startsWith(">");
  const [highlightedItemValue, setHighlightedItemValue] = useState<string | null>(null);
  const [viewStack, setViewStack] = useState<CommandPaletteView[]>([]);
  const currentView = viewStack.at(-1) ?? null;

  const sidebar = useSidebarData();
  const conversationContext = usePaletteContext();
  const shell = usePaletteShell();
  const keybindings = useAtomValue(primaryServerKeybindingsAtom);
  const availableSettingsSearchItems = useAvailableSettingsSearchItems();

  const projects = sidebar?.projects ?? [];
  const conversations = sidebar?.conversations ?? [];
  const activeConversationId = sidebar?.activeConversationId ?? null;
  const activeProjectId = sidebar?.activeProjectId ?? null;
  const activeProjectTitle =
    projects.find((project) => project.id === activeProjectId)?.name ?? projects[0]?.name ?? null;

  // Only while the palette is open, and only the active project's board: the
  // issues group exists to reach an issue by name, not to mirror the board.
  const issues = useWorkIssueItems(activeProjectId);

  const pickerProjects = useMemo(() => projects.map(toPaletteProject), [projects]);
  const projectTitleById = useMemo(
    () => new Map(projects.map((project) => [project.id, project.name] as const)),
    [projects],
  );
  const threads = useMemo(() => conversations.map(toPaletteThread), [conversations]);

  const navigateTo = useCallback(
    async (to: string) => {
      await navigate({ to });
    },
    [navigate],
  );

  const openProject = useCallback(
    async (project: CommandPaletteProject) => {
      await navigate({ to: "/chat/p/$projectId", params: { projectId: String(project.id) } });
    },
    [navigate],
  );

  const projectSearchItems = useMemo(
    () =>
      buildProjectActionItems({
        projects: pickerProjects,
        valuePrefix: "project",
        icon: (project) => (
          <ProjectGlyph
            className={ITEM_ICON_CLASS}
            projectId={String(project.id)}
            projectName={project.title}
          />
        ),
        renderDescription: () => "Open this project's chats",
        runProject: openProject,
      }),
    [openProject, pickerProjects],
  );

  const projectThreadItems = useMemo(
    () =>
      enumerateCommandPaletteItems(
        buildProjectActionItems({
          projects: pickerProjects,
          valuePrefix: "new-thread-in",
          icon: (project) => (
            <ProjectGlyph
              className={ITEM_ICON_CLASS}
              projectId={String(project.id)}
              projectName={project.title}
            />
          ),
          renderDescription: () => "Start a new chat here",
          runProject: openProject,
        }),
      ),
    [openProject, pickerProjects],
  );

  const allThreadItems = useMemo(
    () =>
      buildThreadActionItems({
        threads,
        ...(activeConversationId ? { activeThreadId: activeConversationId } : {}),
        projectTitleById,
        sortOrder: "updated_at",
        icon: <MessageSquareIcon className={ITEM_ICON_CLASS} />,
        renderDescription: (thread, { projectTitle }) => (
          <ThreadCommandSubtitle
            project={{
              environmentId: HUB_ENVIRONMENT_ID,
              workspaceRoot: String(thread.projectId),
              title: projectTitle ?? "",
              faviconPath: null,
              projectIcon: null,
            }}
            projectTitle={projectTitle ?? null}
            branch={thread.branch}
            worktreePath={thread.worktreePath}
            isCurrent={thread.id === activeConversationId}
          />
        ),
        // A linked conversation's destination is its issue page with the chat
        // open in the panel (decisions.md §19.4), the same rule the sidebar's
        // rows follow.
        runThread: async (thread) => {
          const conversation = conversations.find(
            (candidate) => candidate.id === String(thread.id),
          );
          await navigate(
            conversationDestination(
              conversation ?? { id: String(thread.id), work_item_id: null },
            ),
          );
        },
      }),
    [activeConversationId, conversations, navigate, projectTitleById, threads],
  );
  const recentThreadItems = allThreadItems.slice(0, RECENT_THREAD_LIMIT);

  const pushPaletteView = useCallback((view: CommandPaletteView): void => {
    setViewStack((previousViews) => [
      ...previousViews,
      {
        addonIcon: view.addonIcon,
        groups: view.groups,
        ...(view.initialQuery ? { initialQuery: view.initialQuery } : {}),
      },
    ]);
    setHighlightedItemValue(null);
    setQuery(view.initialQuery ?? "");
  }, []);

  function pushView(item: CommandPaletteSubmenuItem): void {
    pushPaletteView({
      addonIcon: item.addonIcon,
      groups: item.groups,
      ...(item.initialQuery ? { initialQuery: item.initialQuery } : {}),
    });
  }

  const popView = useCallback(() => {
    setViewStack((previousViews) => previousViews.slice(0, -1));
    setHighlightedItemValue(null);
    setQuery("");
  }, []);

  // `openCommandPalette({ open: "new-thread-in" })` lands straight in the
  // project picker, as it does upstream.
  React.useLayoutEffect(() => {
    if (openIntent?.kind !== "new-thread-in" || projectThreadItems.length === 0) {
      return;
    }
    clearOpenIntent();
    setViewStack([]);
    setQuery("");
    pushPaletteView({
      addonIcon: <SquarePenIcon className={ADDON_ICON_CLASS} />,
      groups: [{ value: "projects", label: "Projects", items: projectThreadItems }],
    });
  }, [clearOpenIntent, openIntent, projectThreadItems, pushPaletteView]);

  const actionItems: Array<CommandPaletteActionItem | CommandPaletteSubmenuItem> = [];

  if (projects.length > 0) {
    if (activeProjectTitle) {
      actionItems.push({
        kind: "action",
        value: "action:new-thread",
        searchTerms: ["new chat", "chat", "create", "draft", "new thread"],
        title: (
          <>
            New chat in <span className="font-semibold">{activeProjectTitle}</span>
          </>
        ),
        icon: <SquarePenIcon className={ITEM_ICON_CLASS} />,
        run: async () => {
          await navigateTo("/chat");
        },
      });
    }

    actionItems.push({
      kind: "submenu",
      value: "action:new-thread-in",
      searchTerms: ["new chat", "project", "pick", "choose", "select", "new thread"],
      title: "New chat in...",
      icon: <SquarePenIcon className={ITEM_ICON_CLASS} />,
      addonIcon: <SquarePenIcon className={ADDON_ICON_CLASS} />,
      groups: [{ value: "projects", label: "Projects", items: projectThreadItems }],
    });
  }

  if (conversationContext?.createIssue) {
    const createIssue = conversationContext.createIssue;
    actionItems.push({
      kind: "action",
      value: "action:create-linked-issue",
      searchTerms: ["create", "linked", "issue", "handoff", "link", "work item"],
      title: "Create linked issue",
      ...(conversationContext.conversationTitle
        ? { description: conversationContext.conversationTitle }
        : {}),
      icon: <PlusIcon className={ITEM_ICON_CLASS} />,
      run: async () => {
        createIssue();
      },
    });
  }

  if (conversationContext?.openIssue) {
    const openIssue = conversationContext.openIssue;
    actionItems.push({
      kind: "action",
      value: "action:open-linked-issue",
      searchTerms: ["open", "issue", "work", conversationContext.issueIdentifier ?? ""],
      title: `Open issue ${conversationContext.issueIdentifier ?? ""}`.trim(),
      icon: <LayoutListIcon className={ITEM_ICON_CLASS} />,
      run: async () => {
        openIssue();
      },
    });
  }

  if (conversationContext) {
    const copyLink = conversationContext.copyLink;
    actionItems.push({
      kind: "action",
      value: "action:copy-thread-reference",
      searchTerms: ["copy", "link", "url", "reference", "share"],
      title: "Copy link",
      ...(conversationContext.conversationTitle
        ? { description: conversationContext.conversationTitle }
        : {}),
      icon: <LinkIcon className={ITEM_ICON_CLASS} />,
      run: async () => {
        copyLink();
      },
    });
  }

  if (conversationContext?.openDiff) {
    const openDiff = conversationContext.openDiff;
    actionItems.push({
      kind: "action",
      value: "action:open-diff",
      searchTerms: ["diff", "changes", "review", "right panel", "surface"],
      title: "Open Diff panel",
      icon: <SplitIcon className={ITEM_ICON_CLASS} />,
      run: async () => {
        openDiff();
      },
    });
  }

  actionItems.push({
    kind: "action",
    value: "action:open-file-picker",
    searchTerms: ["go to file", "open file", "file picker", "find file", "quick open"],
    title: "Go to file",
    description: "A hosted project has no checkout to open a file from",
    disabled: true,
    icon: <FileSearchIcon className={ITEM_ICON_CLASS} />,
    keepOpen: true,
    run: async () => undefined,
  });

  actionItems.push({
    kind: "action",
    value: "action:search-project-contents",
    searchTerms: ["search project", "find in files", "grep", "content search", "text search"],
    title: "Search project contents",
    description: "A hosted project has no checkout to search",
    disabled: true,
    icon: <TextSearchIcon className={ITEM_ICON_CLASS} />,
    keepOpen: true,
    run: async () => undefined,
  });

  actionItems.push({
    kind: "action",
    value: "action:add-project",
    searchTerms: [
      "add project",
      "folder",
      "directory",
      "browse",
      "clone",
      "remote",
      "repository",
      "repo",
      "git",
    ],
    title: "Add project",
    description: "Projects are created in the hub, not from this client",
    disabled: true,
    icon: <FolderPlusIcon className={ITEM_ICON_CLASS} />,
    keepOpen: true,
    run: async () => undefined,
  });

  actionItems.push({
    kind: "action",
    value: "action:theme-editor",
    searchTerms: ["theme", "appearance", "colors", "palette", "customize"],
    title: "Toggle theme editor",
    description: "The appearance follows the operating system",
    disabled: true,
    icon: <PaletteIcon className={ITEM_ICON_CLASS} />,
    run: async () => undefined,
  });

  if (shell) {
    const toggleSidebar = shell.toggleSidebar;
    actionItems.push({
      kind: "action",
      value: "action:toggle-sidebar",
      searchTerms: ["sidebar", "toggle", "collapse", "expand", "panel"],
      title: "Toggle sidebar",
      icon: <PanelLeftIcon className={ITEM_ICON_CLASS} />,
      shortcutCommand: "sidebar.toggle",
      run: async () => {
        toggleSidebar();
      },
    });
  }

  if (conversationContext) {
    const toggleRightPanel = conversationContext.toggleRightPanel;
    actionItems.push({
      kind: "action",
      value: "action:toggle-right-panel",
      searchTerms: ["right panel", "toggle", "panel", "surfaces", "collapse", "expand"],
      title: "Toggle right panel",
      icon: <PanelRightIcon className={ITEM_ICON_CLASS} />,
      shortcutCommand: "rightPanel.toggle",
      run: async () => {
        toggleRightPanel();
      },
    });
  }

  actionItems.push({
    kind: "action",
    value: "action:go-to-work",
    searchTerms: ["work", "board", "issues", "backlog", "lanes"],
    title: "Go to Work",
    icon: <LayoutListIcon className={ITEM_ICON_CLASS} />,
    run: async () => {
      await navigateTo("/work");
    },
  });

  actionItems.push({
    kind: "action",
    value: "action:go-to-chat",
    searchTerms: ["chat", "conversations", "new"],
    title: "Go to Chat",
    icon: <MessageSquareIcon className={ITEM_ICON_CLASS} />,
    run: async () => {
      await navigateTo("/chat");
    },
  });

  actionItems.push({
    kind: "action",
    value: "action:pull-requests",
    searchTerms: ["pull requests", "changes", "review", "prs"],
    title: "Pull requests",
    icon: <GitPullRequestIcon className={ITEM_ICON_CLASS} />,
    run: async () => {
      await navigateTo("/work/changes");
    },
  });

  actionItems.push({
    kind: "action",
    value: "action:usage",
    searchTerms: ["usage", "spend", "cost", "tokens", "limits"],
    title: "Usage",
    icon: <ChartColumnIcon className={ITEM_ICON_CLASS} />,
    run: async () => {
      await navigateTo("/usage");
    },
  });

  actionItems.push({
    kind: "action",
    value: "action:settings",
    searchTerms: ["settings", "preferences", "configuration", "keybindings"],
    title: "Open settings",
    icon: <SettingsIcon className={ITEM_ICON_CLASS} />,
    run: async () => {
      await navigateTo("/settings");
    },
  });

  if (activeProjectId !== null) {
    actionItems.push({
      kind: "action",
      value: "action:project-settings",
      searchTerms: ["project", "settings", "name", "workspace", "policy", "routing"],
      title: "Project settings",
      ...(activeProjectTitle ? { description: activeProjectTitle } : {}),
      icon: <FolderIcon className={ITEM_ICON_CLASS} />,
      run: async () => {
        await navigateTo("/settings/projects");
      },
    });
  }

  const rootGroups = buildRootGroups({ actionItems, recentThreadItems });

  const settingsSearchItems: CommandPaletteActionItem[] = searchSettings(
    deferredQuery,
    availableSettingsSearchItems,
  ).map((item) => ({
    kind: "action",
    value: `setting:${item.id}`,
    searchTerms: [item.title, SETTINGS_SECTION_LABELS[item.to] ?? "", item.keywords ?? ""],
    title: item.title,
    description: `Settings · ${SETTINGS_SECTION_LABELS[item.to] ?? ""}`,
    icon: <SettingsIcon className={ITEM_ICON_CLASS} />,
    run: async () => {
      await navigate({
        to: item.to,
        hash: item.targetId ?? item.id,
        replace: pathname === item.to,
        hashScrollIntoView: false,
      });
    },
  }));

  const issueItems: CommandPaletteActionItem[] = useMemo(
    () =>
      issues.map((issue) => ({
        kind: "action" as const,
        value: `issue:${issue.id}`,
        searchTerms: [issue.identifier, issue.title, issue.lane],
        title: issue.title,
        titleLeadingContent: (
          <span className="shrink-0 text-muted-foreground/70 tabular-nums">{issue.identifier}</span>
        ),
        description: issue.lane,
        icon: <LayoutListIcon className={ITEM_ICON_CLASS} />,
        run: async () => {
          await navigate({ to: "/work/i/$workItemId", params: { workItemId: issue.id } });
        },
      })),
    [issues, navigate],
  );

  const searching = deferredQuery.trim().length > 0 && !isActionsOnly;
  const activeGroups =
    currentView?.groups ??
    (searching && issueItems.length > 0
      ? [...rootGroups, { value: "issues", label: "Issues", items: issueItems }]
      : rootGroups);

  const filteredGroups = filterCommandPaletteGroups({
    activeGroups,
    query: deferredQuery,
    isInSubmenu: currentView !== null,
    projectSearchItems,
    settingsSearchItems,
    threadSearchItems: allThreadItems,
  });

  const isSubmenu = currentView !== null;
  const mode = getCommandPaletteMode({ currentView, isBrowsing: false });
  const inputPlaceholder = getCommandPaletteInputPlaceholder(mode);
  const displayedGroups = filteredGroups;

  function executeItem(item: CommandPaletteActionItem | CommandPaletteSubmenuItem): void {
    if (item.disabled) {
      return;
    }

    if (item.kind === "submenu") {
      pushView(item);
      return;
    }

    if (!item.keepOpen) {
      setOpen(false);
    }

    void item.run().catch((error: unknown) => {
      toastManager.add(
        stackedThreadToast({
          type: "error",
          title: "Unable to run command",
          description: error instanceof Error ? error.message : "An unexpected error occurred.",
        }),
      );
    });
  }

  function handleKeyDown(event: KeyboardEvent<HTMLInputElement>): void {
    if (event.key === "Backspace" && query === "" && isSubmenu) {
      event.preventDefault();
      popView();
    }
  }

  return (
    <CommandPaletteContent
      key={`${viewStack.length}`}
      aria-label="Command palette"
      inputProps={{
        placeholder: inputPlaceholder,
        wrapperClassName: isSubmenu
          ? "[&_[data-slot=autocomplete-start-addon]]:pointer-events-auto"
          : undefined,
        ...(isSubmenu
          ? {
              startAddon: (
                <button
                  type="button"
                  className="flex cursor-pointer items-center"
                  aria-label="Back"
                  onClick={popView}
                >
                  <ArrowLeftIcon />
                </button>
              ),
            }
          : {}),
        onKeyDown: handleKeyDown,
      }}
      mode="none"
      onItemHighlighted={(value) => {
        setHighlightedItemValue(typeof value === "string" ? value : null);
      }}
      onValueChange={(value) => {
        setQuery(typeof value === "string" ? value : "");
      }}
      panelClassName="max-h-[min(28rem,70vh)]"
      showBackHint={isSubmenu}
      value={query}
    >
      <CommandPaletteResults
        groups={displayedGroups}
        highlightedItemValue={highlightedItemValue}
        isActionsOnly={isActionsOnly}
        keybindings={keybindings}
        onExecuteItem={executeItem}
      />
    </CommandPaletteContent>
  );
}
