// Conversation layout with the header, transcript, composer, and workspace panels.
import { PaperclipIcon } from "lucide-react";
import React from "react";
import { useNavigate } from "@tanstack/react-router";

import { ChatHeader } from "../../components/chat/ChatHeader.tsx";
import { makeWorkspaceFileDropHandlers } from "../../components/chat/workspaceFileDrop.ts";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";
import type { BootstrapProject } from "../../contracts/conversation.ts";
import type { CollaborationEvent, NativeAttempt } from "../../contracts/work.ts";
import type { ProjectScript, ThreadId } from "../../contracts/ui.ts";
import { cn } from "../../lib/utils.ts";
import { detentKeybindings } from "../adapters/keybindings.ts";
import {
  HeaderActionsProvider,
  type HeaderActions,
  type HeaderOpenTarget,
} from "../adapters/headerActions.tsx";
import {
  ConversationActionsProvider,
  conversationActions,
  type ConversationAction,
} from "../adapters/conversationActions.tsx";
import { useProjectActions } from "../adapters/projectActions.tsx";
import { useHeaderGit } from "../adapters/headerGit.ts";
import type { GitStatusPullRequest } from "../adapters/gitStatus.ts";
import { OPEN_IN_EDITORS, useOpenInLocation } from "../adapters/openIn.ts";
import { useIssuePullRequestView } from "../adapters/issuePullRequest.ts";
import { useKeybindingActions } from "../adapters/keybindingActions.ts";
import { publishPaletteContext } from "../adapters/paletteContext.ts";
import { toEnvironmentProject } from "../adapters/shell.ts";
import { useIssueSurfaces } from "../adapters/surfaces.ts";
import { toastManager } from "../../components/ui/toast.tsx";
import { type ConversationPanel, useRightPanelWorkspace } from "./RightPanel.tsx";

export interface WorkspacePanel {
  readonly open: boolean;
  readonly maximized: boolean;
  readonly openConversation: () => void;
  readonly conversationAvailable: boolean;
  readonly openPullRequest: () => void;
  readonly pullRequestAvailable: boolean;
  readonly toggle: () => void;
}

const WorkspacePanelContext = React.createContext<WorkspacePanel | null>(null);

export function useWorkspacePanel(): WorkspacePanel | null {
  return React.useContext(WorkspacePanelContext);
}

const NO_ATTEMPTS: readonly NativeAttempt[] = [];
const NO_HISTORY: readonly CollaborationEvent[] = [];

export interface ChatWorkspaceProps {
  readonly project: BootstrapProject | null;
  readonly projectName: string;
  /**
   * The reader's membership role, for the Terminal card's reason (§18.3): a
   * viewer never gets a terminal however generous the project grant is. It is
   * `/app/bootstrap`'s `actor.role`, passed through rather than read here
   * because this frame takes its facts from its route.
   */
  readonly actorRole?: string;
  /** The conversation on screen, or null on a route that has none yet. */
  readonly conversationId: string | null;
  readonly title: string;
  /** A draft has no server thread, so the title carries no action menu. */
  readonly isServerThread: boolean;
  readonly workItemId: string | null;
  readonly issueIdentifier: string | null;
  /** Opens the handoff form. Null when linking is not available here. */
  readonly onCreateIssue: (() => void) | null;
  readonly onNewThreadInProject: () => void;
  /** The issue page passes its own reads rather than repeating them. */
  readonly attempts?: readonly NativeAttempt[];
  readonly history?: readonly CollaborationEvent[];

  readonly srHeading?: boolean;
  /**
   * Takes the files dropped anywhere on the chat surface (decisions.md §17.1).
   * Absent on a route with no composer to receive them, and the surface is
   * then not a dropzone at all rather than one that swallows a drop.
   */
  readonly onDropFiles?: (files: File[]) => void;
  /**
   * The linked conversation, as the right panel's `conversation` surface
   * (decisions.md §19.3). The route builds the view; the frame only decides
   * where it is drawn.
   */
  readonly conversationPanel?: ConversationPanel | null;
  readonly children: React.ReactNode;
}

export function ChatWorkspace(props: ChatWorkspaceProps): React.ReactElement {
  const navigate = useNavigate();
  const [now, setNow] = React.useState(() => Date.now());
  React.useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(timer);
  }, []);

  const projectId = props.project?.id ?? null;
  const surfaces = useIssueSurfaces(projectId, props.workItemId);
  const scopeKey = props.conversationId ?? props.workItemId ?? "new";

  const workspace = useRightPanelWorkspace({
    scopeKey,
    projectId,
    projectName: props.projectName,
    workItemId: props.workItemId,
    // The Terminal card's own gate (§18.3). They come from the same bootstrap
    // row `canWrite` above comes from, and the actor's role beside it.
    canWrite: props.project?.can_write ?? false,
    canManageRunners: props.project?.can_manage_runners ?? false,
    role: props.actorRole ?? "",
    surfaces,
    attempts: props.attempts ?? NO_ATTEMPTS,
    history: props.history ?? NO_HISTORY,
    now,
    conversation: props.conversationPanel ?? null,
    title: props.title,
  });

  const panelContext = React.useMemo<WorkspacePanel>(
    () => ({
      open: workspace.open,
      maximized: workspace.maximized,
      openConversation: workspace.openConversation,
      conversationAvailable: workspace.conversationAvailable,
      openPullRequest: workspace.openPullRequest,
      pullRequestAvailable: workspace.pullRequestAvailable,
      toggle: workspace.toggleRightPanel,
    }),
    [
      workspace.conversationAvailable,
      workspace.openPullRequest,
      workspace.pullRequestAvailable,
      workspace.maximized,
      workspace.open,
      workspace.openConversation,
      workspace.toggleRightPanel,
    ],
  );

  const openIssue = React.useCallback(() => {
    if (props.workItemId === null) return;
    void navigate({ to: "/work/i/$workItemId", params: { workItemId: props.workItemId } });
  }, [navigate, props.workItemId]);

  const copyLink = React.useCallback(() => {
    const href = globalThis.location?.href;
    if (href === undefined) return;
    void globalThis.navigator?.clipboard
      ?.writeText(href)
      .then(() => toastManager.add({ type: "success", title: "Link copied" }))
      .catch(() => toastManager.add({ type: "error", title: "Could not copy the link" }));
  }, []);

  const actions = useProjectActions(projectId);
  const conversation = React.useMemo(
    () =>
      conversationActions({
        canCreateIssue: props.onCreateIssue !== null,
        issueIdentifier: props.issueIdentifier,
      }),
    [props.onCreateIssue, props.issueIdentifier],
  );

  const onCreateIssueProp = props.onCreateIssue;
  const runConversationAction = React.useCallback(
    (action: ConversationAction) => {
      switch (action.id) {
        case "create-issue":
          onCreateIssueProp?.();
          return;
        case "open-issue":
          openIssue();
          return;
        case "copy-link":
          copyLink();
      }
    },
    [copyLink, onCreateIssueProp, openIssue],
  );

  const runProjectAction = workspace.runProjectAction;
  const runScript = React.useCallback(
    (script: ProjectScript) => {
      const entry = actions.bySlug(script.id);
      if (entry === null) return;
      runProjectAction({
        actionId: entry.action.id,
        name: entry.action.name,
        command: entry.action.command,
      });
    },
    [actions, runProjectAction],
  );

  // What the copied control's one Detent insertion reads
  // (`ProjectScriptsControl.tsx`, `adapters/conversationActions.tsx`).
  const conversationActionsValue = React.useMemo(
    () => ({ actions: conversation, run: runConversationAction }),
    [conversation, runConversationAction],
  );

  const onCreateIssue = props.onCreateIssue;
  const workItemId = props.workItemId;
  const issueIdentifier = props.issueIdentifier;
  const conversationTitle = props.title;
  const canOpenDiff = workspace.diffAvailable;
  const openDiff = workspace.openDiff;
  const toggleRightPanel = workspace.toggleRightPanel;
  React.useEffect(
    () =>
      publishPaletteContext({
        conversationTitle,
        issueIdentifier,
        createIssue: onCreateIssue,
        openIssue: workItemId === null ? null : openIssue,
        copyLink,
        openDiff: canOpenDiff ? openDiff : null,
        toggleRightPanel,
      }),
    [
      canOpenDiff,
      conversationTitle,
      copyLink,
      issueIdentifier,
      onCreateIssue,
      openDiff,
      openIssue,
      toggleRightPanel,
      workItemId,
    ],
  );

  // The keybinding table, bound to what this workspace can actually do
  // (decisions.md §18.10). A command whose surface is not here — the Files
  // picker, the Browser — is left out rather than bound to nothing, so the
  // browser keeps the keystroke and the Keybindings settings page can say why
  // it is unbound.
  //
  // The three terminal commands are bound whenever the surface is available
  // (§18.3) and decline the keystroke otherwise, which is the difference the
  // dispatcher's `false` exists for: a chord whose surface is refused for a
  // reason the reader can fix must not be swallowed by a handler that does
  // nothing, and must not disappear from the table between renders either.
  useKeybindingActions({
    "chat.new": props.onNewThreadInProject,
    "chat.newLocal": props.onNewThreadInProject,
    "rightPanel.toggle": workspace.toggleRightPanel,
    "rightPanel.close": workspace.closeRightPanel,
    "rightPanel.toggleMaximized": workspace.toggleMaximized,
    ...(canOpenDiff ? { "diff.toggle": openDiff } : {}),
    "terminal.toggle": () => {
      if (!workspace.terminalAvailable) return false;
      workspace.openTerminal();
      return true;
    },
    "terminal.new": () => {
      if (!workspace.terminalAvailable) return false;
      workspace.newTerminal();
      return true;
    },
    "terminal.close": () => {
      if (!workspace.terminalAvailable) return false;
      return workspace.closeTerminal();
    },
    "thread.copyReference": copyLink,
    // One handler for every `script.<id>.run` chord the project registered
    // (decisions.md §18.12). Declining an unknown slug leaves the keystroke to
    // the browser, which is what a chord whose action was just deleted owes
    // the reader.
    runProjectAction: (scriptId: string) => {
      const entry = actions.bySlug(scriptId);
      if (entry === null) return false;
      runProjectAction({
        actionId: entry.action.id,
        name: entry.action.name,
        command: entry.action.command,
      });
      return true;
    },

  });

  // The header's git group and Open picker (decisions.md §18.6, §18.12).
  //
  // Three facts reach them from here and nowhere else: whether the reader may
  // write to this project (`/app/bootstrap`'s project row carries `can_write`),
  // whether the project has a GitHub connector (§18.6's rows carry a nullable
  // one), and the issue's pull request. The workspace itself is the git hook's,
  // because it is the hook's first interaction that asks for one — rendering
  // this frame must spend no workspace slot (§18.1).
  const pullRequestView = useIssuePullRequestView(projectId, props.workItemId);
  const git = useHeaderGit({
    projectId,
    workItemId: props.workItemId,
    canWrite: props.project?.can_write ?? false,
    connector: pullRequestView.connector,
    pullRequest: pullRequestView.pullRequest as GitStatusPullRequest | null,
  });
  const openLocation = useOpenInLocation({
    worktreePath: git.worktreePath,
    hostname: git.machineHostname,
    linked: props.workItemId !== null,
  });

  const openPullRequestOnHost = React.useCallback(() => {
    const url = pullRequestView.pullRequest?.url ?? surfaces.pullRequest?.url ?? null;
    if (url === null) return;
    globalThis.open?.(url, "_blank", "noopener");
  }, [pullRequestView.pullRequest, surfaces.pullRequest]);

  const headerActions = React.useMemo<HeaderActions>(() => {
    const targets: HeaderOpenTarget[] = [];
    if (props.workItemId !== null) {
      targets.push({
        id: "issue",
        label: `Open ${props.issueIdentifier ?? "the issue"} in Work`,
        run: openIssue,
        external: false,
      });
    }
    const pullRequest = surfaces.pullRequest;
    if (pullRequest !== null) {
      targets.push({
        id: "pull-request",
        label: `Open pull request #${pullRequest.number}`,
        run: () => globalThis.open?.(pullRequest.url, "_blank", "noopener"),
        external: true,
      });
    }
    return {
      openTargets: targets,
      openLocation,
      // No linked issue means no worktree, so there is no group to drive: the
      // control falls back to "Link an issue to get a worktree." on every row.
      git: props.workItemId === null ? null : git,
      openPullRequest:
        pullRequestView.pullRequest === null && surfaces.pullRequest === null
          ? null
          : openPullRequestOnHost,
      projectId,
    };
  }, [
    git,
    openIssue,
    openLocation,
    openPullRequestOnHost,
    projectId,
    props.issueIdentifier,
    props.workItemId,
    pullRequestView.pullRequest,
    surfaces.pullRequest,
  ]);

  const environmentProject = React.useMemo(
    () => (props.project === null ? null : toEnvironmentProject(props.project)),
    [props.project],
  );

  const [dragging, setDragging] = React.useState(false);
  const onDropFiles = props.onDropFiles;
  React.useEffect(() => {
    if (!dragging) return;
    const clear = () => setDragging(false);
    window.addEventListener("dragend", clear);
    return () => window.removeEventListener("dragend", clear);
  }, [dragging]);
  const dropHandlers = React.useMemo(
    () =>
      onDropFiles === undefined
        ? null
        : makeWorkspaceFileDropHandlers({
            setDragActive: setDragging,
            addFiles: onDropFiles,
          }),
    [onDropFiles],
  );

  return (
    <HeaderActionsProvider value={headerActions}>

      <ConversationActionsProvider value={conversationActionsValue}>
        <div className="relative flex min-h-0 min-w-0 flex-1 overflow-hidden bg-background">
          {workspace.layoutControls}
          <div
            className={cn(
              "flex min-h-0 min-w-0 flex-col overflow-x-hidden",
              workspace.maximized ? "w-0 flex-none" : "flex-1",
            )}
            data-chat-column-maximized-away={workspace.maximized ? "true" : "false"}
          >

            {props.srHeading === false ? null : (
              <h1 className="dc-sr-only" data-testid="route-heading">
                {props.title}
              </h1>
            )}
            {/* `openInCwd` and `gitCwd` are the checkout on the reader's own
                machine upstream. A Detent worktree is the runner's, and
                §18.13's workspace resource names its absolute path — which is
                exactly what those two props mean, so they carry it. */}
            <WorkspacePageHeader data-chat-header className="relative bg-background">
              <ChatHeader
                activeThreadEnvironmentId={HUB_ENVIRONMENT_ID}
                activeThreadId={(props.conversationId ?? "draft") as ThreadId}
                activeThreadTitle={props.title}
                isServerThread={props.isServerThread}
                activeProject={environmentProject}
                openInCwd={git.worktreePath}
                activeProjectScripts={actions.scripts}
                preferredScriptId={null}
                keybindings={detentKeybindings}
                availableEditors={OPEN_IN_EDITORS}
                rightPanelOpen={workspace.open}
                gitCwd={git.worktreePath}
                onNewThreadInProject={props.onNewThreadInProject}
                onRunProjectScript={runScript}
                onAddProjectScript={actions.add}
                onUpdateProjectScript={actions.update}
                onDeleteProjectScript={actions.remove}
              />
            </WorkspacePageHeader>

            <div
              className="relative flex min-h-0 min-w-0 flex-1 flex-col"
              data-chat-workspace-drop-target={dropHandlers === null ? undefined : "true"}
              {...(dropHandlers ?? {})}
            >
              {dragging ? (
                <div
                  className="pointer-events-none absolute inset-2 z-40 flex items-center justify-center rounded-2xl border-2 border-dashed border-primary/60 bg-primary/[0.035]"
                  data-chat-workspace-drop-overlay="true"
                >
                  <div
                    role="status"
                    className="flex items-center gap-2 rounded-full border border-primary/25 bg-background/95 px-4 py-2.5 font-medium text-foreground text-sm shadow-lg"
                  >
                    <PaperclipIcon className="size-4 text-primary" aria-hidden="true" />
                    Drop files to attach
                  </div>
                </div>
              ) : null}
              <WorkspacePanelContext.Provider value={panelContext}>
                {props.children}
              </WorkspacePanelContext.Provider>
            </div>
          </div>
          {workspace.panel}
        </div>
      </ConversationActionsProvider>
    </HeaderActionsProvider>
  );
}

