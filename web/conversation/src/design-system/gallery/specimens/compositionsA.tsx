// Workspace and conversation compositions: sidebar pieces, page chrome, the
// chat header and git group, the message list, and the composer's parts.
//
// Each renders the real component with synthetic data and the providers its
// own tests or its app caller supply (`tests/components/headerGitGroup.test.tsx`,
// `chatheader.test.tsx`, `app/components/ChatWorkspace.tsx`,
// `app/components/Composer.tsx`, `app/components/Timeline.tsx`).
import { LegendListRef } from "@legendapp/list/react";
import {
  AtSignIcon,
  CircleAlertIcon,
  FolderIcon,
  InfoIcon,
  PaperclipIcon,
  PlugZapIcon,
  SparklesIcon,
  TriangleAlertIcon,
} from "lucide-react";
import React from "react";

import { conversationActions, ConversationActionsProvider } from "../../../app/adapters/conversationActions.tsx";
import { DEFAULT_HEADER_ACTIONS, HeaderActionsProvider, type HeaderActions } from "../../../app/adapters/headerActions.tsx";
import type { HeaderGit } from "../../../app/adapters/headerGit.ts";
import { detentKeybindings } from "../../../app/adapters/keybindings.ts";
import { OPEN_IN_EDITORS } from "../../../app/adapters/openIn.ts";
import { toVcsStatus, type GitActionPolicy } from "../../../app/adapters/gitStatus.ts";
import { toPendingUserInput } from "../../../app/adapters/pendingQuestions.ts";
import { toEnvironmentProject } from "../../../app/adapters/shell.ts";
import { useTheme } from "../../../app/adapters/theme.ts";
import { timelineSources } from "../../../app/adapters/timelineEntries.ts";
import { ChatHeader } from "../../../components/chat/ChatHeader.tsx";
import { ComposerBanner, type ComposerBannerVariant } from "../../../components/chat/ComposerBanner.tsx";
import { ComposerBannerStack, type ComposerBannerStackItem } from "../../../components/chat/ComposerBannerStack.tsx";
import {
  ComposerControl,
  ComposerControlChevron,
  ComposerControlIcon,
  ComposerControlSeparator,
  ComposerSelectControl,
} from "../../../components/chat/ComposerControl.tsx";
import { ComposerPendingUserInputPanel } from "../../../components/chat/ComposerPendingUserInputPanel.tsx";
import { ComposerPrimaryActions } from "../../../components/chat/ComposerPrimaryActions.tsx";
import { ComposerSurface } from "../../../components/chat/ComposerSurface.tsx";
import {
  CHAT_FILE_TAG_CHIP_CLASS_NAME,
  FILE_TAG_CHIP_CLASS_NAME,
  FileTagChipContent,
} from "../../../components/chat/FileTagChip.tsx";
import { MessagesTimeline } from "../../../components/chat/MessagesTimeline.tsx";
import { ProposedPlanCard } from "../../../components/chat/ProposedPlanCard.tsx";
import { DetentWordmark } from "../../../components/DetentWordmark.tsx";
import GitActionsControl from "../../../components/GitActionsControl.tsx";
import { ProjectFavicon } from "../../../components/ProjectFavicon.tsx";
import { resolveThreadStatusPill } from "../../../components/Sidebar.logic.ts";
import ThreadSidebar from "../../../components/Sidebar.tsx";
import { SidebarChromeFooter, SidebarChromeHeader } from "../../../components/sidebar/SidebarChrome.tsx";
import { WorkspacePicker } from "../../../components/sidebar/SidebarWorkspacePicker.tsx";
import {
  ChangeRequestStatusIcon,
  ThreadStatusLabel,
  ThreadWorktreeIndicator,
} from "../../../components/ThreadStatusIndicators.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { SelectItem, SelectPopup, Select, SelectValue } from "../../../components/ui/select.tsx";
import { Sidebar, SidebarContent, SidebarMenu, SidebarProvider } from "../../../components/ui/sidebar.tsx";
import { TooltipProvider } from "../../../components/ui/tooltip.tsx";
import {
  WorkspaceBreadcrumb,
  WorkspaceBreadcrumbItem,
  WorkspaceBreadcrumbSeparator,
} from "../../../components/WorkspaceBreadcrumb.tsx";
import { WorkspacePageContainer } from "../../../components/WorkspacePageContainer.tsx";
import { WorkspacePageHeader } from "../../../components/WorkspacePageHeader.tsx";
import { SidebarDataProvider, publishSidebarData, type SidebarShellData } from "../../../app/adapters/sidebarData.tsx";
import { HUB_ENVIRONMENT_ID } from "../../../contracts/index.ts";
import { scopedThreadKey, scopeThreadRef } from "../../../environment/scoped.ts";
import { deriveTimelineEntries } from "../../../session-logic.ts";
import { useUiStateStore } from "../../../uiStateStore.ts";

import {
  ASSISTANT_MARKDOWN,
  assistantMessage,
  conversation,
  detail,
  minutesAgo,
  question,
  statusStep,
  userMessage,
} from "../fixtures";
import { Cell, LONG_LABEL, Row, type GalleryDoc } from "../specimen";

const noop = (): void => undefined;

// --- Shared synthetic data --------------------------------------------------

const PROJECTS = [
  { id: "proj_detent", name: "detent", can_write: true },
  { id: "proj_cloud", name: "detent-cloud", can_write: true },
];

const RUNNING = conversation({
  id: "conv_dsa_running",
  project_id: "proj_detent",
  title: "Lock renewal waits on a healthy handoff",
  last_message_at: minutesAgo(2),
});

const CONVERSATIONS = [
  RUNNING,
  conversation({
    id: "conv_dsa_needs_you",
    project_id: "proj_detent",
    title: "Which renewal window should the lock use?",
    execution: { ...RUNNING.execution, status: "waiting_input" },
    last_message_at: minutesAgo(6),
  }),
  conversation({
    id: "conv_dsa_unread",
    project_id: "proj_detent",
    title: "Port the design-system gallery",
    work_item_id: null,
    work_item: null,
    execution: { ...RUNNING.execution, status: "idle", updated_at: minutesAgo(9) },
    last_message_at: minutesAgo(9),
  }),
  conversation({
    id: "conv_dsa_active",
    project_id: "proj_detent",
    title: "Usage table rounding",
    work_item_id: null,
    work_item: null,
    execution: { ...RUNNING.execution, status: "idle", updated_at: minutesAgo(40) },
    last_message_at: minutesAgo(40),
  }),
  conversation({
    id: "conv_dsa_long",
    project_id: "proj_cloud",
    title: LONG_LABEL,
    work_item_id: null,
    work_item: null,
    execution: { ...RUNNING.execution, status: "idle", updated_at: minutesAgo(300) },
    last_message_at: minutesAgo(300),
  }),
];

function sidebarData(conversations = CONVERSATIONS): SidebarShellData {
  return {
    organizationName: "Threefold",
    projects: PROJECTS,
    activeProjectId: null,
    onProjectChange: noop,
    conversations,
    serverResults: [],
    activeConversationId: "conv_dsa_active",
    onSelect: noop,
    onNewChat: noop,
    onRename: async () => undefined,
    attention: new Set(["conv_dsa_needs_you"]),
    navigation: { activePath: "/chat", onNavigate: noop },
  };
}

/** Seeds the "last visited" stamp an hour back so an idle thread reads as unseen. */
function seedUnread(id: string): void {
  const key = scopedThreadKey(scopeThreadRef(HUB_ENVIRONMENT_ID, id as never));
  useUiStateStore.setState((state) => ({
    threadLastVisitedAtById: { ...state.threadLastVisitedAtById, [key]: minutesAgo(60) },
  }));
}

/** The sidebar frame the app puts these pieces in (`AppSidebarLayout`), minus the offcanvas. */
function SidebarFrame({ children }: { readonly children: React.ReactNode }) {
  return (
    <SidebarProvider className="h-full! min-h-0!">
      <Sidebar
        collapsible="none"
        data-app-sidebar=""
        className="border-r border-sidebar-border bg-sidebar text-sidebar-foreground"
      >
        {children}
      </Sidebar>
    </SidebarProvider>
  );
}

function headerGit(policy: Partial<GitActionPolicy> = {}, status: "dirty" | "clean" | "none" = "dirty"): HeaderGit {
  return {
    policy: {
      workItemId: "wi_3363",
      canWrite: true,
      hasConnector: true,
      workspaceState: "ready",
      workspaceReadOnly: false,
      workspaceError: null,
      gitCapable: true,
      busy: false,
      ...policy,
    },
    status:
      status === "none"
        ? null
        : toVcsStatus({
            status: {
              branch: "detent/detent_142",
              detached: false,
              remote: "origin",
              upstream: true,
              ahead: status === "dirty" ? 1 : 0,
              behind: 0,
              dirty_file_count: status === "dirty" ? 2 : 0,
              head_sha: "a".repeat(40),
            },
            pullRequest: null,
            connector: { baseUrl: "https://github.com" },
          }),
    statusError: null,
    busy: policy.busy ?? false,
    worktreePath: "/Users/runner/code/detent",
    machineHostname: "mac-studio",
    run: noop,
    refresh: noop,
  };
}

function withHeader(actions: Partial<HeaderActions>, node: React.ReactNode) {
  return <HeaderActionsProvider value={{ ...DEFAULT_HEADER_ACTIONS, ...actions }}>{node}</HeaderActionsProvider>;
}

// --- Workspace --------------------------------------------------------------

function ThreadSidebarSpecimen({ conversations }: { readonly conversations: typeof CONVERSATIONS }) {
  const data = React.useMemo(() => sidebarData(conversations), [conversations]);
  React.useState(() => {
    seedUnread("conv_dsa_unread");
    publishSidebarData(data);
    return null;
  });
  return (
    <SidebarDataProvider value={data}>
      <SidebarFrame>
        <ThreadSidebar />
      </SidebarFrame>
    </SidebarDataProvider>
  );
}

const threadSidebar: GalleryDoc = {
  meta: { name: "Thread sidebar", kind: "composition", group: "Workspace", source: "src/components/Sidebar.tsx" },
  specimens: [
    {
      id: "rows",
      title: "Threads: running, needs you, unread, active, long title",
      note: "The thread list on its own, inside a non-collapsible sidebar; App sidebar layout shows it in the offcanvas shell.",
      height: 780,
      render: () => <ThreadSidebarSpecimen conversations={CONVERSATIONS} />,
    },
    {
      id: "empty",
      title: "No threads yet",
      height: 420,
      render: () => <ThreadSidebarSpecimen conversations={[]} />,
    },
  ],
};

const sidebarChrome: GalleryDoc = {
  meta: {
    name: "Sidebar chrome",
    kind: "composition",
    group: "Workspace",
    source: "src/components/sidebar/SidebarChrome.tsx",
  },
  specimens: [
    {
      id: "header-footer",
      title: "Header (wordmark, toggle) and footer utility menu",
      height: 360,
      render: () => (
        <SidebarDataProvider value={sidebarData()}>
          <SidebarFrame>
            <SidebarChromeHeader isElectron={false} />
            <SidebarContent />
            <SidebarChromeFooter />
          </SidebarFrame>
        </SidebarDataProvider>
      ),
    },
  ],
};

const WORKSPACES = [
  { id: "org_acme", name: "Acme Robotics" },
  { id: "org_field", name: "Field Operations and Long-Running Maintenance" },
  { id: "org_lab", name: "Research Lab" },
] as const;

function WorkspacePickerFrame({ children }: { readonly children: React.ReactNode }) {
  return (
    <SidebarFrame>
      <SidebarContent />
      <SidebarMenu className="p-2">{children}</SidebarMenu>
    </SidebarFrame>
  );
}

const sidebarWorkspacePicker: GalleryDoc = {
  meta: {
    name: "Sidebar workspace picker",
    kind: "composition",
    group: "Workspace",
    source: "src/components/sidebar/SidebarWorkspacePicker.tsx",
  },
  specimens: [
    {
      id: "trigger",
      title: "Trigger: the current workspace's badge; click to open the searchable list",
      height: 360,
      render: () => (
        <WorkspacePickerFrame>
          <WorkspacePicker
            current={WORKSPACES[0]}
            organizations={WORKSPACES}
            onSelect={() => undefined}
            onManage={() => undefined}
            addHref="#"
          />
        </WorkspacePickerFrame>
      ),
    },
    {
      id: "error",
      title: "A failed switch reopens the list with the error",
      height: 360,
      render: () => (
        <WorkspacePickerFrame>
          <WorkspacePicker
            current={WORKSPACES[1]}
            organizations={WORKSPACES}
            onSelect={() => undefined}
            onManage={() => undefined}
            addHref="#"
            error="You no longer have access to Research Lab."
          />
        </WorkspacePickerFrame>
      ),
    },
    {
      id: "pending",
      title: "Pending: rows are disabled while a switch is in flight",
      height: 360,
      render: () => (
        <WorkspacePickerFrame>
          <WorkspacePicker
            current={WORKSPACES[2]}
            organizations={WORKSPACES}
            onSelect={() => undefined}
            onManage={() => undefined}
            addHref="#"
            pending
          />
        </WorkspacePickerFrame>
      ),
    },
  ],
};

const detentWordmark: GalleryDoc = {
  meta: { name: "Detent wordmark", kind: "composition", group: "Workspace", source: "src/components/DetentWordmark.tsx" },
  specimens: [
    {
      id: "sizes",
      title: "Sizes and colour (inherits currentColor)",
      render: () => (
        <Row>
          {(["size-4", "size-6", "size-10", "size-16"] as const).map((size) => (
            <Cell key={size} label={size}>
              <DetentWordmark className={size} aria-label="Detent" />
            </Cell>
          ))}
          <Cell label="text-muted-foreground">
            <DetentWordmark className="size-10 text-muted-foreground" aria-label="Detent" />
          </Cell>
          <Cell label="text-primary">
            <DetentWordmark className="size-10 text-primary" aria-label="Detent" />
          </Cell>
        </Row>
      ),
    },
  ],
};

function Breadcrumb({ long = false }: { readonly long?: boolean }) {
  return (
    <WorkspaceBreadcrumb ariaLabel="Location">
      <WorkspaceBreadcrumbItem>Work</WorkspaceBreadcrumbItem>
      <WorkspaceBreadcrumbSeparator />
      <WorkspaceBreadcrumbItem>detent</WorkspaceBreadcrumbItem>
      <WorkspaceBreadcrumbSeparator />
      <WorkspaceBreadcrumbItem current>
        <span className="truncate">{long ? LONG_LABEL : "detent#142"}</span>
      </WorkspaceBreadcrumbItem>
    </WorkspaceBreadcrumb>
  );
}

const workspaceBreadcrumb: GalleryDoc = {
  meta: {
    name: "Workspace breadcrumb",
    kind: "composition",
    group: "Workspace",
    source: "src/components/WorkspaceBreadcrumb.tsx",
  },
  specimens: [
    {
      id: "trail",
      title: "Trail, and a long current item that truncates",
      render: () => (
        <div className="flex max-w-xl flex-col gap-4">
          <Breadcrumb />
          <Breadcrumb long />
        </div>
      ),
    },
  ],
};

const workspacePageHeader: GalleryDoc = {
  meta: {
    name: "Workspace page header",
    kind: "composition",
    group: "Workspace",
    source: "src/components/WorkspacePageHeader.tsx",
  },
  specimens: [
    {
      id: "bar",
      title: "Top bar with breadcrumb and actions",
      note: "Fixed top-bar height and safe-area insets; at 390px the inset tightens.",
      render: () => (
        <div className="rounded-lg border">
          <WorkspacePageHeader>
            <div className="min-w-0 flex-1">
              <Breadcrumb />
            </div>
            <Button size="sm" variant="outline">
              Share
            </Button>
            <Button size="sm">New issue</Button>
          </WorkspacePageHeader>
        </div>
      ),
    },
  ],
};

const workspacePageContainer: GalleryDoc = {
  meta: {
    name: "Workspace page container",
    kind: "composition",
    group: "Workspace",
    source: "src/components/WorkspacePageContainer.tsx",
  },
  specimens: [
    {
      id: "widths",
      title: "Widths: readable, wide, expanded",
      note: "The dashed outline is the container's own box.",
      render: () => (
        <div className="flex flex-col gap-3">
          {(["readable", "wide", "expanded"] as const).map((width) => (
            <WorkspacePageContainer key={width} width={width} className="border border-dashed border-border pb-6">
              <p className="font-mono text-muted-foreground text-xs">width=&quot;{width}&quot;</p>
            </WorkspacePageContainer>
          ))}
        </div>
      ),
    },
  ],
};

const FAVICON_PROJECTS = {
  "default (no icon)": toEnvironmentProject(PROJECTS[0] as never),
  "emoji override": { ...toEnvironmentProject(PROJECTS[0] as never), projectIcon: { kind: "emoji" as const, emoji: "🚀" } },
  "lucide override": {
    ...toEnvironmentProject(PROJECTS[1] as never),
    projectIcon: { kind: "lucide" as const, name: "rocket", color: "emerald" as const },
  },
};

const projectFavicon: GalleryDoc = {
  meta: { name: "Project favicon", kind: "composition", group: "Workspace", source: "src/components/ProjectFavicon.tsx" },
  specimens: [
    {
      id: "kinds",
      title: "Fallback, emoji and icon overrides",
      note: "Hosted projects publish no favicon file, so the fallback glyph is the common case.",
      render: () => (
        <Row>
          {Object.entries(FAVICON_PROJECTS).map(([label, project]) => (
            <Cell key={label} label={label}>
              <span className="flex items-center gap-2 text-sm">
                <ProjectFavicon project={project} />
                {project.title}
              </span>
            </Cell>
          ))}
          <Cell label="custom fallbackIcon">
            <span className="flex items-center gap-2 text-sm">
              <ProjectFavicon project={FAVICON_PROJECTS["default (no icon)"]} fallbackIcon={FolderIcon} />
              detent
            </span>
          </Cell>
        </Row>
      ),
    },
  ],
};

type StatusThread = Parameters<typeof resolveThreadStatusPill>[0]["thread"];

function statusThread(overrides: Record<string, unknown>): StatusThread {
  return {
    hasActionableProposedPlan: false,
    hasPendingApprovals: false,
    hasPendingUserInput: false,
    interactionMode: "default",
    latestTurn: null,
    session: null,
    backgroundLiveness: null,
    ...overrides,
  } as unknown as StatusThread;
}

export const STATUS_THREADS: Record<string, StatusThread> = {
  working: statusThread({ session: { status: "running" } }),
  connecting: statusThread({ session: { status: "starting" } }),
  "awaiting input": statusThread({ hasPendingUserInput: true }),
  "pending approval": statusThread({ hasPendingApprovals: true }),
  monitoring: statusThread({ backgroundLiveness: "monitoring" }),
  completed: statusThread({
    latestTurn: { completedAt: minutesAgo(5) },
    lastVisitedAt: minutesAgo(60),
  }),
};

const threadStatusIndicators: GalleryDoc = {
  meta: {
    name: "Thread status indicators",
    kind: "composition",
    group: "Workspace",
    source: "src/components/ThreadStatusIndicators.tsx",
  },
  specimens: [
    {
      id: "pills",
      title: "Status pills, full and compact",
      note: "Each pill comes from resolveThreadStatusPill; hover for the tooltip. Labels hide below md.",
      minHeight: 160,
      render: () => (
        <TooltipProvider>
          <div className="flex flex-col gap-4">
            <Row>
              {Object.entries(STATUS_THREADS).map(([label, thread]) => {
                const pill = resolveThreadStatusPill({ thread });
                return pill === null ? null : (
                  <Cell key={label} label={label}>
                    <ThreadStatusLabel status={pill} />
                  </Cell>
                );
              })}
            </Row>
            <Row>
              {Object.entries(STATUS_THREADS).map(([label, thread]) => {
                const pill = resolveThreadStatusPill({ thread });
                return pill === null ? null : (
                  <Cell key={label} label={`${label} (compact)`}>
                    <ThreadStatusLabel status={pill} compact />
                  </Cell>
                );
              })}
            </Row>
          </div>
        </TooltipProvider>
      ),
    },
    {
      id: "change-request",
      title: "Change request state icons and worktree indicator",
      minHeight: 140,
      render: () => (
        <TooltipProvider>
          <Row>
            <Cell label="open">
              <ChangeRequestStatusIcon state="open" className="size-4 text-emerald-600 dark:text-emerald-300/90" />
            </Cell>
            <Cell label="draft">
              <ChangeRequestStatusIcon state="open" isDraft className="size-4 text-muted-foreground" />
            </Cell>
            <Cell label="merged">
              <ChangeRequestStatusIcon state="merged" className="size-4 text-violet-600 dark:text-violet-300/90" />
            </Cell>
            <Cell label="closed">
              <ChangeRequestStatusIcon state="closed" className="size-4 text-red-600 dark:text-red-300/90" />
            </Cell>
            <Cell label="worktree (hover)">
              <ThreadWorktreeIndicator
                thread={{
                  id: "conv_dsa_running" as never,
                  branch: "detent/detent_142",
                  worktreePath: "/Users/runner/.detent/worktrees/detent_142",
                }}
              />
            </Cell>
          </Row>
        </TooltipProvider>
      ),
    },
  ],
};

const gitActionsControl: GalleryDoc = {
  meta: {
    name: "Git actions control",
    kind: "composition",
    group: "Workspace",
    source: "src/components/GitActionsControl.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Dirty worktree, clean, busy, read-only, no issue",
      note: "The quick action opens the commit dialog; the chevron opens the git menu. Disabled states carry their reason.",
      minHeight: 460,
      render: () => (
        <div className="flex flex-col gap-4">
          <Cell label="dirty (commit, push & PR)">{withHeader({ git: headerGit() }, <GitActionsControl />)}</Cell>
          <Cell label="clean">{withHeader({ git: headerGit({}, "clean") }, <GitActionsControl />)}</Cell>
          <Cell label="busy">{withHeader({ git: headerGit({ busy: true }) }, <GitActionsControl />)}</Cell>
          <Cell label="read-only while running">
            {withHeader({ git: headerGit({ workspaceReadOnly: true }) }, <GitActionsControl />)}
          </Cell>
          <Cell label="no linked issue">{withHeader({}, <GitActionsControl />)}</Cell>
        </div>
      ),
    },
  ],
};

// --- Conversation -----------------------------------------------------------

function ChatHeaderSpecimen({ linked, title }: { readonly linked: boolean; readonly title: string }) {
  const project = React.useMemo(() => toEnvironmentProject(PROJECTS[0] as never), []);
  const actions = React.useMemo(
    () => conversationActions({ canCreateIssue: !linked, issueIdentifier: linked ? "detent#142" : null }),
    [linked],
  );
  return (
    <HeaderActionsProvider value={{ ...DEFAULT_HEADER_ACTIONS, git: linked ? headerGit() : null }}>
      <ConversationActionsProvider value={{ actions, run: noop }}>
        <div className="rounded-lg border">
          <WorkspacePageHeader>
            <ChatHeader
              activeThreadEnvironmentId={HUB_ENVIRONMENT_ID as never}
              activeThreadId={"conv_dsa_running" as never}
              activeThreadTitle={title}
              isServerThread
              activeProject={project}
              openInCwd={linked ? "/Users/runner/code/detent" : null}
              activeProjectScripts={[]}
              preferredScriptId={null}
              keybindings={detentKeybindings}
              availableEditors={OPEN_IN_EDITORS}
              rightPanelOpen={false}
              gitCwd={linked ? "/Users/runner/code/detent" : null}
              onNewThreadInProject={noop}
              onRunProjectScript={noop}
              onAddProjectScript={async () => ({ ok: true }) as never}
              onUpdateProjectScript={async () => ({ ok: true }) as never}
              onDeleteProjectScript={async () => ({ ok: true }) as never}
            />
          </WorkspacePageHeader>
        </div>
      </ConversationActionsProvider>
    </HeaderActionsProvider>
  );
}

const chatHeader: GalleryDoc = {
  meta: { name: "Chat header", kind: "composition", group: "Conversation", source: "src/components/chat/ChatHeader.tsx" },
  specimens: [
    {
      id: "linked",
      title: "Linked conversation: git group and open-in",
      note: "Double-click the title to rename. The project actions menu lists the conversation's actions.",
      minHeight: 220,
      render: () => <ChatHeaderSpecimen linked title="Lock renewal waits on a healthy handoff" />,
    },
    {
      id: "unlinked",
      title: "Unlinked chat with a long title",
      minHeight: 160,
      render: () => <ChatHeaderSpecimen linked={false} title={LONG_LABEL} />,
    },
  ],
};

function MessagesTimelineSpecimen({ empty, working }: { readonly empty?: boolean; readonly working?: boolean }) {
  const { resolvedTheme } = useTheme();
  const listRef = React.useRef<LegendListRef | null>(null);
  const entries = React.useMemo(() => {
    if (empty === true) return [];
    const sources = timelineSources(
      detail({
        conversation: conversation({ id: "conv_dsa_timeline" }),
        messages: [
          userMessage({ id: "msg_dsa_u1", seq: 1, text: "Why does the lock renewal flake under load?", created_at: minutesAgo(12) }),
          statusStep("msg_dsa_s1", minutesAgo(11), "Read internal/lock/lease.go"),
          statusStep("msg_dsa_s2", minutesAgo(10), "Ran go test ./internal/lock/..."),
          assistantMessage({ id: "msg_dsa_a1", seq: 4, text: ASSISTANT_MARKDOWN, created_at: minutesAgo(8) }),
        ],
      }),
    );
    return deriveTimelineEntries(sources.messages, sources.proposedPlans, sources.workEntries);
  }, [empty]);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <MessagesTimeline
        isWorking={working === true}
        activeTurnStartedAt={working === true ? minutesAgo(1) : null}
        listRef={listRef}
        timelineEntries={entries}
        latestTurn={null}
        runningTurnId={null}
        turnDiffSummaries={[]}
        routeThreadKey={`${HUB_ENVIRONMENT_ID}:conv_dsa_timeline`}
        onOpenTurnDiff={noop}
        supportsConversationRollback={false}
        onRevertToTurnCount={noop}
        isRevertingCheckpoint={false}
        onImageExpand={noop}
        activeThreadEnvironmentId={HUB_ENVIRONMENT_ID as never}
        markdownCwd={undefined}
        resolvedTheme={resolvedTheme}
        timestampFormat="locale"
        workspaceRoot={undefined}
        anchorMessageId={null}
        onAnchorReady={noop}
        contentInsetEndAdjustment={0}
        liveFollowEnabled
        onIsAtEndChange={noop}
        onManualNavigation={noop}
      />
    </div>
  );
}

const messagesTimeline: GalleryDoc = {
  meta: {
    name: "Messages timeline",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/MessagesTimeline.tsx",
  },
  specimens: [
    {
      id: "rows",
      title: "User row, work group, assistant markdown",
      note: "The list the conversation timeline drives; here fed the same entries directly.",
      height: 640,
      render: () => <MessagesTimelineSpecimen />,
    },
    {
      id: "working",
      title: "Working: the live turn indicator",
      height: 640,
      render: () => <MessagesTimelineSpecimen working />,
    },
    {
      id: "empty",
      title: "Empty conversation placeholder",
      height: 260,
      render: () => <MessagesTimelineSpecimen empty />,
    },
  ],
};

function SurfaceSpecimen({ contextStrip }: { readonly contextStrip: boolean }) {
  return (
    <div className="mx-auto w-full max-w-3xl">
      <ComposerSurface.Shell contextStrip={contextStrip}>
        <ComposerSurface.Host>
          <ComposerSurface.Main>
            <div className="min-h-20 px-4 py-3 text-muted-foreground text-sm">Prompt area</div>
          </ComposerSurface.Main>
        </ComposerSurface.Host>
        {contextStrip ? (
          <ComposerSurface.ContextStrip className="px-3 text-muted-foreground/70 text-xs">
            <FolderIcon className="size-3" /> detent · Issue #142
          </ComposerSurface.ContextStrip>
        ) : null}
      </ComposerSurface.Shell>
    </div>
  );
}

const composerSurface: GalleryDoc = {
  meta: {
    name: "Composer surface",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ComposerSurface.tsx",
  },
  specimens: [
    {
      id: "shell",
      title: "Shell, host and main; with the context strip",
      note: "The glass frame the composer sits in, with placeholder content in its slots.",
      render: () => (
        <div className="flex flex-col gap-6">
          <SurfaceSpecimen contextStrip={false} />
          <SurfaceSpecimen contextStrip />
        </div>
      ),
    },
  ],
};

const composerControl: GalleryDoc = {
  meta: {
    name: "Composer control",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ComposerControl.tsx",
  },
  specimens: [
    {
      id: "sizes",
      title: "Buttons and select triggers, sm and xs",
      note: "The footer picker controls. `xs` is the resting (collapsed) family.",
      minHeight: 280,
      render: () => (
        <div className="flex flex-col gap-4">
          {(["sm", "xs"] as const).map((size) => (
            <Cell key={size} label={size}>
              <div className="flex items-center gap-1">
                <ComposerControl size={size}>
                  <ComposerControlIcon icon={PaperclipIcon} size={size} />
                  Attach
                </ComposerControl>
                <ComposerControlSeparator size={size} />
                <ComposerControl size={size}>
                  <ComposerControlIcon icon={SparklesIcon} size={size} />
                  Model
                  <ComposerControlChevron size={size} />
                </ComposerControl>
                <ComposerControlSeparator size={size} />
                <Select<string> defaultValue="high">
                  <ComposerSelectControl size={size} aria-label="Effort">
                    <SelectValue />
                  </ComposerSelectControl>
                  <SelectPopup>
                    <SelectItem value="low">Low</SelectItem>
                    <SelectItem value="medium">Medium</SelectItem>
                    <SelectItem value="high">High</SelectItem>
                  </SelectPopup>
                </Select>
                <ComposerControl size={size} disabled>
                  <ComposerControlIcon icon={AtSignIcon} size={size} />
                  Disabled
                </ComposerControl>
              </div>
            </Cell>
          ))}
        </div>
      ),
    },
  ],
};

type PrimaryActionsProps = React.ComponentProps<typeof ComposerPrimaryActions>;

const PRIMARY_BASE: PrimaryActionsProps = {
  compact: false,
  pendingAction: null,
  isRunning: false,
  showPlanFollowUpPrompt: false,
  promptHasText: true,
  isSendBusy: false,
  sendDisabledReason: null,
  isConnecting: false,
  isEnvironmentUnavailable: false,
  isPreparingWorktree: false,
  hasSendableContent: true,
  showSendWhileRunning: true,
  onPreviousPendingQuestion: noop,
  onInterrupt: noop,
  onImplementPlanInNewThread: noop,
};

const PRIMARY_STATES: Record<string, Partial<PrimaryActionsProps>> = {
  "ready to send": {},
  "empty prompt": { promptHasText: false, hasSendableContent: false },
  sending: { isSendBusy: true },
  "running (stop + send)": { isRunning: true },
  "blocked with reason": { sendDisabledReason: "Waiting for the upload to finish" },
  "environment unavailable": { isEnvironmentUnavailable: true, sendDisabledReason: "Sending is unavailable" },
  compact: { compact: true },
  "pending question: next": {
    pendingAction: { questionIndex: 0, isLastQuestion: false, canAdvance: true, isResponding: false, isComplete: false },
  },
  "pending question: submit": {
    pendingAction: { questionIndex: 1, isLastQuestion: true, canAdvance: true, isResponding: false, isComplete: true },
  },
};

const composerPrimaryActions: GalleryDoc = {
  meta: {
    name: "Composer primary actions",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ComposerPrimaryActions.tsx",
  },
  specimens: [
    {
      id: "states",
      title: "Send, sending, stop, blocked, compact, pending question",
      minHeight: 200,
      render: () => (
        <TooltipProvider>
          <Row>
            {Object.entries(PRIMARY_STATES).map(([label, overrides]) => (
              <Cell key={label} label={label}>
                <ComposerPrimaryActions {...PRIMARY_BASE} {...overrides} />
              </Cell>
            ))}
          </Row>
        </TooltipProvider>
      ),
    },
  ],
};

const BANNER_VARIANTS: Record<ComposerBannerVariant, React.ReactNode> = {
  default: <InfoIcon />,
  info: <InfoIcon />,
  success: <SparklesIcon />,
  warning: <TriangleAlertIcon />,
  error: <CircleAlertIcon />,
};

const composerBanner: GalleryDoc = {
  meta: {
    name: "Composer banner",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ComposerBanner.tsx",
  },
  specimens: [
    {
      id: "variants",
      title: "Variants (floating), with actions and dismiss",
      render: () => (
        <div className="mx-auto flex max-w-3xl flex-col gap-3">
          {(Object.keys(BANNER_VARIANTS) as ComposerBannerVariant[]).map((variant) => (
            <ComposerBanner.Root key={variant} variant={variant} placement="floating" role="status">
              <ComposerBanner.Row>
                <ComposerBanner.Icon>{BANNER_VARIANTS[variant]}</ComposerBanner.Icon>
                <ComposerBanner.Content>
                  <span className="min-w-0 font-medium">{variant} banner</span>
                </ComposerBanner.Content>
                <ComposerBanner.Actions>
                  <Button size="xs" variant="outline">
                    Action
                  </Button>
                  <ComposerBanner.Dismiss aria-label="Dismiss" />
                </ComposerBanner.Actions>
              </ComposerBanner.Row>
            </ComposerBanner.Root>
          ))}
        </div>
      ),
    },
    {
      id: "attached",
      title: "Attached to the composer surface, long text",
      render: () => (
        <div className="mx-auto w-full max-w-3xl">
          <ComposerSurface.Shell>
            <ComposerBanner.Attachment>
              <ComposerBanner.Root variant="warning" role="status">
                <ComposerBanner.Row>
                  <ComposerBanner.Icon>
                    <PlugZapIcon />
                  </ComposerBanner.Icon>
                  <ComposerBanner.Content>
                    <span className="min-w-0 font-medium">{LONG_LABEL}</span>
                  </ComposerBanner.Content>
                </ComposerBanner.Row>
              </ComposerBanner.Root>
            </ComposerBanner.Attachment>
            <ComposerSurface.Host>
              <ComposerSurface.Main>
                <div className="min-h-20 px-4 py-3 text-muted-foreground text-sm">Prompt area</div>
              </ComposerSurface.Main>
            </ComposerSurface.Host>
          </ComposerSurface.Shell>
        </div>
      ),
    },
  ],
};

function BannerStackSpecimen() {
  const [items, setItems] = React.useState<ComposerBannerStackItem[]>(() => [
    {
      id: "usage",
      variant: "warning",
      icon: <TriangleAlertIcon />,
      title: "Usage limit at 90%",
      description: "New runs pause at 100% until the window resets.",
      dismissLabel: "Dismiss usage warning",
    },
    {
      id: "runner",
      variant: "error",
      icon: <PlugZapIcon />,
      title: "Runner disconnected",
      description: "The attempt resumes when mac-studio reconnects.",
      actions: (
        <Button size="xs" variant="outline">
          Retry
        </Button>
      ),
    },
    {
      id: "info",
      variant: "info",
      icon: <InfoIcon />,
      title: "A newer client is available",
      dismissLabel: "Dismiss update notice",
    },
  ]);
  const withDismiss = items.map((item) =>
    item.dismissLabel === undefined
      ? item
      : { ...item, onDismiss: () => setItems((all) => all.filter((candidate) => candidate.id !== item.id)) },
  );
  return (
    <div className="mx-auto w-full max-w-3xl">
      <ComposerBannerStack items={withDismiss} />
    </div>
  );
}

const composerBannerStack: GalleryDoc = {
  meta: {
    name: "Composer banner stack",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ComposerBannerStack.tsx",
  },
  specimens: [
    {
      id: "stack",
      title: "Three notices: the most urgent leads, the rest peek",
      note: "Click the peek to expand the stack; dismiss removes a notice with its exit transition.",
      minHeight: 300,
      render: () => <BannerStackSpecimen />,
    },
  ],
};

function PendingInputSpecimen({ responding }: { readonly responding: boolean }) {
  const pending = React.useMemo(() => toPendingUserInput(question()), []);
  const [answers, setAnswers] = React.useState<Record<string, { selectedOptionValues?: string[] }>>({});
  return (
    <div className="mx-auto w-full max-w-3xl">
      <ComposerSurface.Shell>
        <ComposerBanner.Attachment>
          <ComposerBanner.Root data-chat-composer-top-drawer="true" variant="info">
            <ComposerPendingUserInputPanel
              pendingUserInputs={[pending]}
              respondingRequestIds={responding ? [pending.requestId] : []}
              answers={answers}
              questionIndex={0}
              onToggleOption={(questionId, value) =>
                setAnswers((all) => ({ ...all, [questionId]: { selectedOptionValues: [value] } }))
              }
              onAdvance={noop}
              onDismiss={noop}
            />
          </ComposerBanner.Root>
        </ComposerBanner.Attachment>
        <ComposerSurface.Host>
          <ComposerSurface.Main>
            <div className="min-h-16 px-4 py-3 text-muted-foreground text-sm">Or type your own answer</div>
          </ComposerSurface.Main>
        </ComposerSurface.Host>
      </ComposerSurface.Shell>
    </div>
  );
}

const composerPendingUserInput: GalleryDoc = {
  meta: {
    name: "Composer pending user input",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ComposerPendingUserInputPanel.tsx",
  },
  specimens: [
    {
      id: "question",
      title: "A question with options",
      note: "Pick an option; the panel is the drawer the composer opens above itself.",
      minHeight: 320,
      render: () => <PendingInputSpecimen responding={false} />,
    },
    {
      id: "responding",
      title: "Submitting the answer",
      minHeight: 320,
      render: () => <PendingInputSpecimen responding />,
    },
  ],
};

function FileChips() {
  const { resolvedTheme } = useTheme();
  const files = [
    ["internal/lock/lease.go", "lease.go"],
    ["web/conversation/src/app/router.tsx", "router.tsx"],
    ["docs/design-system/README.md", "README.md"],
    ["web/conversation/package.json", "package.json"],
    ["src/design-system/gallery/specimens/a-very-long-file-name-that-truncates.tsx", "a-very-long-file-name-that-truncates.tsx"],
  ] as const;
  return (
    <div className="flex flex-col gap-4">
      <Cell label="composer chip">
        <span className="flex flex-wrap gap-1.5">
          {files.map(([path, label]) => (
            <span key={path} className={FILE_TAG_CHIP_CLASS_NAME}>
              <FileTagChipContent path={path} label={label} theme={resolvedTheme} />
            </span>
          ))}
        </span>
      </Cell>
      <Cell label="chat chip (selectable)">
        <span className="flex flex-wrap gap-1.5">
          {files.map(([path, label]) => (
            <span key={path} className={CHAT_FILE_TAG_CHIP_CLASS_NAME}>
              <FileTagChipContent path={path} label={label} theme={resolvedTheme} selectable />
            </span>
          ))}
        </span>
      </Cell>
    </div>
  );
}

const fileTagChip: GalleryDoc = {
  meta: { name: "File tag chip", kind: "composition", group: "Conversation", source: "src/components/chat/FileTagChip.tsx" },
  specimens: [
    {
      id: "chips",
      title: "Composer and chat chips, by file type",
      note: "The icon follows the path's file type and the frame's theme.",
      render: () => <FileChips />,
    },
  ],
};

const PLAN = [
  "# Renew the lock at half the lease",
  "",
  "1. Read the lease duration from the store instead of the 20s constant.",
  "2. Run the handoff health check before renewing, not after.",
  "3. Add an integration test for a 30s and a 2m lease.",
  "4. Update the changelog.",
  "",
  "```ts",
  "const interval = lock.leaseMs / 2;",
  "```",
  "",
  "Risks: a very short lease renews often; cap the minimum at 5s.",
].join("\n");

const proposedPlanCard: GalleryDoc = {
  meta: {
    name: "Proposed plan card",
    kind: "composition",
    group: "Conversation",
    source: "src/components/chat/ProposedPlanCard.tsx",
  },
  specimens: [
    {
      id: "plan",
      title: "Collapsed plan with its menu",
      note: "Expand to read the whole plan. Saving to the workspace is refused in the hosted client, and says so.",
      minHeight: 420,
      render: () => (
        <div className="mx-auto max-w-3xl">
          <ProposedPlanCard
            planMarkdown={PLAN}
            environmentId={HUB_ENVIRONMENT_ID as never}
            cwd={undefined}
            workspaceRoot={undefined}
          />
        </div>
      ),
    },
  ],
};

export const COMPOSITIONS_A: Readonly<Record<string, GalleryDoc>> = {
  "thread-sidebar": threadSidebar,
  "sidebar-chrome": sidebarChrome,
  "sidebar-workspace-picker": sidebarWorkspacePicker,
  "detent-wordmark": detentWordmark,
  "workspace-page-header": workspacePageHeader,
  "workspace-page-container": workspacePageContainer,
  "workspace-breadcrumb": workspaceBreadcrumb,
  "project-favicon": projectFavicon,
  "thread-status-indicators": threadStatusIndicators,
  "git-actions-control": gitActionsControl,
  "chat-header": chatHeader,
  "messages-timeline": messagesTimeline,
  "composer-surface": composerSurface,
  "composer-control": composerControl,
  "composer-primary-actions": composerPrimaryActions,
  "composer-banner": composerBanner,
  "composer-banner-stack": composerBannerStack,
  "composer-pending-user-input": composerPendingUserInput,
  "file-tag-chip": fileTagChip,
  "proposed-plan-card": proposedPlanCard,
};

