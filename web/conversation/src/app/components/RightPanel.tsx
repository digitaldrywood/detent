import React from "react";

import { PanelLayoutControls, RightPanelMaximizeControl } from "../../components/chat/PanelLayoutControls.tsx";
import { RightPanelSheet } from "../../components/RightPanelSheet.tsx";
import { RightPanelTabs } from "../../components/RightPanelTabs.tsx";
import { RIGHT_PANEL_INLINE_LAYOUT_MEDIA_QUERY } from "../../rightPanelLayout.ts";
import { useMediaQuery } from "../../hooks/useMediaQuery.ts";
import { detentKeybindings, resolveShortcutCommand, shortcutLabelForCommand } from "../adapters/keybindings.ts";
import { useRightPanel, type RightPanelSurface } from "../adapters/rightPanel.ts";
import { usePanelAnimationSettings } from "../../panelAnimations.ts";
import { HUB_ENVIRONMENT_ID } from "../../contracts/index.ts";
import type { CollaborationEvent, NativeAttempt } from "../../contracts/work.ts";
import type { IssueSurfaces } from "../adapters/surfaces.ts";
import { cn } from "../../lib/utils.ts";
import { DiffSurface } from "./surfaces/DiffSurface.tsx";
import { FileSurface, FilesSurface } from "./surfaces/FilesSurface.tsx";
import { OutputSurface } from "./surfaces/OutputSurface.tsx";
import { TerminalSurface } from "./surfaces/TerminalSurface.tsx";
import { PullRequestSurface } from "./surfaces/PullRequestSurface.tsx";
import { useTheme } from "../../hooks/useTheme.ts";
import { registerRightPanelActions } from "../../rightPanelStore.ts";
import { createWorkspaceRelay, type WorkspaceRelay } from "../adapters/workspaceRelay.ts";
import { useActionRuns, type ActionRunSnapshot, type StartRunInput } from "../adapters/actionRuns.ts";
import { isWorkspaceLive, useWorkspace, workspaceSessionFacts } from "../adapters/workspaces.ts";
import { useTerminals } from "../adapters/terminalStream.ts";
import { terminalBlockedReason, type TerminalPolicy } from "../adapters/terminalPolicy.ts";

const DIFF_SURFACE: RightPanelSurface = { id: "diff", kind: "diff" };
const FILES_SURFACE: RightPanelSurface = { id: "files", kind: "files" };
const OUTPUT_SURFACE: RightPanelSurface = { id: "output", kind: "output" };

const TERMINAL_SURFACE_ID = "terminal:workspace";
const TERMINAL_SURFACE: RightPanelSurface = {
  id: TERMINAL_SURFACE_ID,
  kind: "terminal",
  resourceId: TERMINAL_SURFACE_ID,

  terminalIds: [],
  activeTerminalId: "",
};

/**
 * What the runner has to report before it may claim this workspace
 * (decisions.md §18.1, §18.12).
 *
 * Both capabilities, always, and one workspace serves both surfaces. §18.12
 * fixes `requires: ["files", "exec"]` as what "a client asks for when running
 * an action is the reason it wants a worktree", and asking for both up front
 * is what keeps a reader who opened Files and then ran an action from needing
 * a second worktree — which would cost a second slot against the
 * organization's and the person's caps for one issue. `terminal` is still not
 * here: it would additionally need the grant's `runners` flag and an
 * owner-only isolation choice (§18.3), and this client has no terminal view.
 */
const WORKSPACE_REQUIRES: readonly string[] = ["files", "exec"];

/**
 * What the runner has to report when a terminal is the reason for the worktree
 * (decisions.md §18.1, §18.3).
 *
 * It is a second list rather than a widening of the first, and that is the
 * whole point of `requires`. A workspace asking for `terminal` needs the
 * grant's `runners` flag at creation and is refused outright where
 * `workspaces.terminal.enabled` is off (§18.3), so a panel that asked for it
 * unconditionally would refuse every reader who only wanted to look at files.
 * `files` rides along because the client reuses one workspace for every
 * surface and `Satisfies` is exact: a terminal-only workspace could not serve
 * the Files tab beside it.
 */
const TERMINAL_WORKSPACE_REQUIRES: readonly string[] = ["files", "exec", "terminal"];

/**
 * The linked conversation, as the panel's own surface (decisions.md §19.3).
 *
 * The content is passed in rather than imported: the conversation view is
 * mounted by the route (`src/app/work/IssuePage.tsx`), which already owns the
 * conversation's atoms, and importing it here would make this module and
 * `App.tsx` import one another.
 */
export interface ConversationPanel {
  readonly conversationId: string;
  readonly content: React.ReactNode;
}

export interface RightPanelWorkspace {
  /** The fixed titlebar cluster: maximize, then the two panel toggles. */
  readonly layoutControls: React.ReactNode;
  /** The panel itself. Rendered as the last child of the workspace row. */
  readonly panel: React.ReactNode;
  readonly open: boolean;
  readonly maximized: boolean;
  /** Opens the Diff surface, for the command palette's row. */
  readonly openDiff: () => void;
  /** False when there is no change request behind the Diff tab. */
  readonly diffAvailable: boolean;
  /** Opens the Pull request surface. A no-op with no mirrored pull request. */
  readonly openPullRequest: () => void;
  readonly pullRequestAvailable: boolean;
  /** Opens the Conversation surface. A no-op where there is no conversation. */
  readonly openConversation: () => void;
  readonly conversationAvailable: boolean;
  /** Opens the Files surface (decisions.md §18.4). */
  readonly openFiles: () => void;
  readonly filesAvailable: boolean;
  /** Opens the Output surface (decisions.md §18.12). */
  readonly openOutput: () => void;
  readonly outputAvailable: boolean;
  /** Opens the Terminal surface (decisions.md §18.3). */
  readonly openTerminal: () => void;
  /** Opens another shell on it. §18.2: its own stream, its own PTY. */
  readonly newTerminal: () => void;
  /**
   * Closes the shell in front of the reader, and answers false when there was
   * none — which is what `terminal.close` owes the browser rather than
   * swallowing the keystroke.
   */
  readonly closeTerminal: () => boolean;
  readonly terminalAvailable: boolean;
  /** Why the Terminal surface is unavailable, or null when it is not. */
  readonly terminalBlockedReason: string | null;
  /**
   * Runs one project action (decisions.md §18.12): opens the Output surface,
   * asks for a workspace if there is not one yet, records the run and follows
   * its exec stream. A no-op where there is no issue to open a worktree for.
   */
  readonly runProjectAction: (input: StartRunInput) => void;

  readonly toggleRightPanel: () => void;

  readonly closeRightPanel: () => void;
  readonly toggleMaximized: () => void;
}

export function useRightPanelWorkspace(input: {
  /** One panel per conversation or issue. */
  readonly scopeKey: string;
  /** The issue the live surfaces read, and the workspace's own scope (§18.1). */
  readonly projectId?: string | null;
  readonly workItemId?: string | null;
  /** The Files surface's project crumb (§18.4). */
  readonly projectName?: string;
  /**
   * The reader's authority on this project, for the Terminal card's reason
   * (decisions.md §18.3).
   *
   * The terminal is the one surface whose gate the client has to be able to
   * state: the hub refuses it with `forbidden`, which is one code covering
   * four different facts, and "forbidden" tells a reader nothing they can act
   * on. All three come from `/app/bootstrap` — the project row's `can_write`
   * and `can_manage_runners`, and the actor's role — and all three default to
   * the refusing answer, so a caller that has not passed them shows a disabled
   * card rather than an enabled one that fails on press.
   */
  readonly canWrite?: boolean;
  readonly canManageRunners?: boolean;
  readonly role?: string;
  readonly surfaces: IssueSurfaces;
  readonly attempts: readonly NativeAttempt[];
  readonly history: readonly CollaborationEvent[];
  readonly now: number;
  readonly conversation?: ConversationPanel | null;
  /** The route's name, for the narrow sheet's own heading. */
  readonly title?: string;
}): RightPanelWorkspace {
  const panel = useRightPanel(input.scopeKey);
  const shouldUseSheet = useMediaQuery(RIGHT_PANEL_INLINE_LAYOUT_MEDIA_QUERY);
  const { active: panelAnimationsActive, durationMs: panelAnimationDurationMs } =
    usePanelAnimationSettings();

  // Always openable: Detent either has a diff to draw — a stored attempt diff
  // (§18.5) or the round's change version — or names the reason it has none,
  // and §16 keeps the surface present either way rather than removing the tab.
  const diffAvailable = true;
  const pullRequestAvailable = input.surfaces.pullRequest !== null;

  const addDiff = React.useCallback(() => panel.addSurface(DIFF_SURFACE), [panel]);
  const addFiles = React.useCallback(() => panel.addSurface(FILES_SURFACE), [panel]);
  const addOutput = React.useCallback(() => panel.addSurface(OUTPUT_SURFACE), [panel]);
  const addFile = React.useCallback(
    (relativePath: string) =>
      panel.addSurface({
        id: `file:${relativePath}`,
        kind: "file",
        relativePath,
        revealLine: null,
        revealRequestId: 0,
      }),
    [panel],
  );
  const addPullRequest = React.useCallback(() => {
    const pullRequest = input.surfaces.pullRequest;
    if (pullRequest === null) return;
    panel.addSurface({
      id: `pull-request:${pullRequest.repository}#${pullRequest.number}`,
      kind: "pull-request",
      projectId: "",
      repository: pullRequest.repository,
      number: pullRequest.number,
      url: pullRequest.url,
    });
  }, [input.surfaces.pullRequest, panel]);
  const addTerminal = React.useCallback(() => panel.addSurface(TERMINAL_SURFACE), [panel]);
  const noop = React.useCallback(() => {}, []);

  const conversation = input.conversation ?? null;
  const conversationId = conversation?.conversationId ?? null;
  const addConversation = React.useCallback(() => {
    if (conversationId === null) return;
    panel.addSurface({
      id: `conversation:${conversationId}`,
      kind: "conversation",
      conversationId,
    });
  }, [conversationId, panel]);

  const toggleMaximized = panel.toggleMaximized;
  const panelOpen = panel.isOpen;
  const surfaceCount = panel.surfaces.length;
  const panelToggle = panel.toggle;
  const toggle = React.useCallback(() => {
    if (!panelOpen && surfaceCount === 0 && conversationId !== null) {
      addConversation();
      return;
    }
    panelToggle();
  }, [addConversation, conversationId, panelOpen, panelToggle, surfaceCount]);
  React.useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented) return;
      const command = resolveShortcutCommand(event, detentKeybindings);
      if (command === "rightPanel.toggle") {
        event.preventDefault();
        event.stopPropagation();
        toggle();
        return;
      }
      if (command === "rightPanel.toggleMaximized") {
        event.preventDefault();
        event.stopPropagation();
        toggleMaximized();
      }
    };
    window.addEventListener("keydown", onKeyDown, true);
    return () => window.removeEventListener("keydown", onKeyDown, true);
  }, [toggle, toggleMaximized]);

  const activeSurface =
    panel.surfaces.find((surface) => surface.id === panel.activeSurfaceId) ?? null;

  // The Files surface, and the workspace behind it (decisions.md §18.1, §18.4).
  //
  // The workspace is requested only once a file surface is actually open. It
  // costs a runner slot and counts against the organization's and the person's
  // open-workspace limits, so a panel that is merely *able* to show files must
  // not spend one: opening the tab is what asks for a worktree.
  const projectId = input.projectId ?? null;
  const workItemId = input.workItemId ?? null;
  const filesAvailable = projectId !== null && workItemId !== null;
  const filesOpen = panel.surfaces.some(
    (surface) => surface.kind === "files" || surface.kind === "file",
  );
  // Running an action asks for a worktree too (decisions.md §18.12), and it
  // asks *before* there is a surface open on it — the reader pressed a header
  // button, not a tab. `runRequested` is that ask, and it stays true for the
  // life of the panel: a workspace released the moment a run finished would be
  // re-opened, at the cost of another slot, by the next run.
  const [runRequested, setRunRequested] = React.useState(false);
  // The Terminal tab is the third reason to want a worktree, and it is the one
  // that changes what the workspace has to be: a terminal workspace asks for
  // the terminal capability in `requires`, which the claim gate matches and
  // which §18.3 gates behind the runner grant. Opening the tab is what asks,
  // for the reason the Files tab is: a panel merely able to show a shell must
  // not spend a runner slot on one.
  const terminalOpen = panel.surfaces.some((surface) => surface.kind === "terminal");
  const workspace = useWorkspace({
    projectId,
    workItemId,
    requires: terminalOpen ? TERMINAL_WORKSPACE_REQUIRES : WORKSPACE_REQUIRES,
    enabled: filesAvailable && (filesOpen || runRequested || terminalOpen),
  });
  const { resolvedTheme } = useTheme();
  // What the surfaces' waiting sentences name: the runner that claimed it, and
  // the times the request timeout is derived from (§18.1).
  const workspaceResource = workspace.workspace;
  const workspaceSession = React.useMemo(
    () => workspaceSessionFacts(workspaceResource),
    [workspaceResource],
  );

  // One relay per live workspace. A new workspace id is a new socket; closing
  // the surface closes it, because §18.1 only resets the idle timer on
  // person-originated frames and a socket nobody is reading is not one.
  const workspaceLive = workspace.state !== null && isWorkspaceLive(workspace.state);
  const relayUrl = workspace.relayUrl;
  const mintTicket = workspace.mintTicket;
  const relay = React.useMemo<WorkspaceRelay | null>(() => {
    if (!workspaceLive || relayUrl === null || mintTicket === null) return null;
    return createWorkspaceRelay({ url: relayUrl, mintTicket });
  }, [workspaceLive, relayUrl, mintTicket]);
  React.useEffect(() => {
    if (relay === null) return;
    return () => relay.close();
  }, [relay]);

  // The action runs this panel is following (decisions.md §18.12). The hook
  // owns both halves — the exec stream for a run this tab started, and the
  // project event stream for one it did not — because a run outlives the
  // surface that watched it start.
  // The recorded runs are read back only while the Output surface is on the
  // panel: that is the one place they are drawn, and a run the hub recorded
  // with nobody watching (§18.12) is invisible without them.
  const outputOpen = panel.surfaces.some((surface) => surface.kind === "output");
  const runs = useActionRuns({
    projectId,
    workspaceId: workspace.workspace?.id ?? null,
    workspaceLive,
    relayUrl,
    mintTicket,
    showRecorded: outputOpen,
  });
  // The shells this panel is holding (decisions.md §18.3). They open when the
  // tab does and close with the workspace: a PTY whose workspace has ended is
  // a shell nobody can reach, and the runner kills it regardless.
  const terminals = useTerminals({
    workspaceId: workspace.workspace?.id ?? null,
    workspaceLive,
    relayUrl,
    mintTicket,
    enabled: terminalOpen,
  });

  const terminalPolicy = React.useMemo<TerminalPolicy>(
    () => ({
      workItemId,
      canWrite: input.canWrite ?? false,
      canManageRunners: input.canManageRunners ?? false,
      role: input.role ?? "",
      workspaceReadOnly: workspaceResource?.read_only === true,
      workspaceState: workspace.state,
      workspaceReason: workspace.reason,
      workspaceError: workspace.error,
      // Until a workspace exists there is nothing to read a capability off, and
      // §18.1's rule is that a null is not a refusal: the tab is pressable, and
      // pressing it is what opens the workspace that answers the question.
      terminalCapable: workspaceResource?.capabilities?.terminal ?? null,
    }),
    [
      input.canManageRunners,
      input.canWrite,
      input.role,
      workItemId,
      workspace.error,
      workspace.reason,
      workspace.state,
      workspaceResource,
    ],
  );
  const terminalReason = terminalBlockedReason(terminalPolicy);
  const terminalIsAvailable = terminalReason === null;
  const toggleTerminal = React.useCallback(() => {
    if (!terminalIsAvailable) return;
    if (terminalOpen) {
      panel.closeSurface(TERMINAL_SURFACE);
      return;
    }
    addTerminal();
  }, [addTerminal, panel, terminalIsAvailable, terminalOpen]);
  // `terminal.new` opens the tab first where it is not open, because a reader
  // who pressed it without a Terminal surface meant "give me a shell" rather
  // than "give me a second one".
  const openTerminals = terminals.open;
  const closeOneTerminal = terminals.closeTerminal;
  const activeTerminalId = terminals.activeId;
  const newTerminal = React.useCallback(() => {
    if (!terminalOpen) {
      addTerminal();
      return;
    }
    openTerminals();
  }, [addTerminal, openTerminals, terminalOpen]);
  const closeActiveTerminal = React.useCallback((): boolean => {
    if (!terminalOpen || activeTerminalId === null) return false;
    closeOneTerminal(activeTerminalId);
    return true;
  }, [activeTerminalId, closeOneTerminal, terminalOpen]);
  const startRun = runs.start;
  const runProjectAction = React.useCallback(
    (request: StartRunInput) => {
      if (!filesAvailable) return;
      setRunRequested(true);
      addOutput();
      startRun(request);
    },
    [addOutput, filesAvailable, startRun],
  );
  const rerun = React.useCallback(
    (run: ActionRunSnapshot) =>
      runProjectAction({ actionId: run.actionId, name: run.name, command: run.command }),
    [runProjectAction],
  );

  React.useEffect(() => {
    if (!filesAvailable) return;
    return registerRightPanelActions({
      openFile: (_ref, relativePath) => addFile(relativePath),
      openBrowser: () => {},
    });
  }, [addFile, filesAvailable]);

  const content =
    activeSurface === null ? null : activeSurface.kind === "diff" ? (
      <DiffSurface
        change={input.surfaces.change}
        source={input.surfaces.source}
        attempts={input.attempts}
        history={input.history}
        now={input.now}
        theme={resolvedTheme}
        onReload={input.surfaces.reload}
      />
    ) : activeSurface.kind === "files" ? (
      <FilesSurface
        state={workspace.state}
        reason={workspace.reason}
        error={workspace.error}
        loading={workspace.loading}
        session={workspaceSession}
        files={relay}
        onRetry={workspace.retry}
        onOpenFile={addFile}
        projectName={input.projectName}
        theme={resolvedTheme}
      />
    ) : activeSurface.kind === "file" ? (
      <FileSurface
        path={activeSurface.relativePath}
        files={relay}
        onOpenFile={addFile}
        projectName={input.projectName}
      />
    ) : activeSurface.kind === "output" ? (
      <OutputSurface
        runs={runs.runs}
        activeRunId={runs.activeRunId}
        onSelectRun={runs.select}
        state={workspace.state}
        reason={workspace.reason}
        error={workspace.error}
        loading={workspace.loading || runs.pending}
        session={workspaceSession}
        onRetry={workspace.retry}
        onRerun={rerun}
        theme={resolvedTheme}
      />
    ) : activeSurface.kind === "terminal" ? (
      <TerminalSurface
        terminals={terminals.terminals}
        activeId={terminals.activeId}
        onSelect={terminals.select}
        onNewTerminal={terminals.open}
        onCloseTerminal={terminals.closeTerminal}
        registerOutput={terminals.registerOutput}
        state={workspace.state}
        reason={workspace.reason}
        error={workspace.error}
        loading={workspace.loading}
        session={workspaceSession}
        onRetry={workspace.retry}
      />
    ) : activeSurface.kind === "pull-request" ? (
      <PullRequestSurface pullRequest={input.surfaces.pullRequest} />
    ) : activeSurface.kind === "conversation" ? (
      <div
        className="flex min-h-0 min-w-0 flex-1 flex-col"
        data-testid="conversation-surface"
        data-conversation={activeSurface.conversationId}
      >
        {conversation === null || conversation.conversationId !== activeSurface.conversationId
          ? null
          : conversation.content}
      </div>
    ) : null;

  const panelToggleControls = (
    <PanelLayoutControls
      terminalAvailable={terminalIsAvailable}
      terminalOpen={terminalOpen}
      terminalShortcutLabel={shortcutLabelForCommand(detentKeybindings, "terminal.toggle")}
      rightPanelAvailable
      rightPanelOpen={panel.isOpen}
      rightPanelShortcutLabel={shortcutLabelForCommand(detentKeybindings, "rightPanel.toggle")}
      rightPanelUnavailableLabel="Right panel is unavailable"
      liveAgentCount={input.surfaces.agents.liveCount}
      onToggleTerminal={toggleTerminal}
      onToggleRightPanel={toggle}
    />
  );

  const layoutControls = (
    <div
      className="pointer-events-none fixed top-[var(--workspace-controls-top)] right-[var(--workspace-controls-right)] z-50 mr-px flex h-[var(--workspace-topbar-height)] items-center gap-1"
      data-workspace-titlebar-controls
    >
      {!shouldUseSheet ? (
        <span
          aria-hidden={!panel.isOpen}
          className={cn(
            "flex shrink-0",
            panelAnimationsActive &&
              "motion-safe:transition-opacity motion-safe:[transition-duration:var(--panel-animation-duration)] motion-safe:ease-out",
            panel.isOpen ? "pointer-events-auto opacity-100" : "pointer-events-none opacity-0",
          )}
          inert={!panel.isOpen}
        >
          <RightPanelMaximizeControl
            maximized={panel.maximized}
            onToggle={panel.toggleMaximized}
          />
        </span>
      ) : null}
      <div className="pointer-events-auto flex h-full items-center">{panelToggleControls}</div>
    </div>
  );

  const tabProps = {
    surfaces: panel.surfaces,
    environmentId: HUB_ENVIRONMENT_ID,
    activeSurfaceId: panel.activeSurfaceId,
    pendingSurfaceIds: EMPTY_IDS,
    previewSessions: EMPTY_SESSIONS,
    desktopByTabId: EMPTY_OVERLAYS,
    terminalLabelsById: EMPTY_LABELS,
    onActivate: panel.activate,
    onCloseSurface: panel.closeSurface,
    onCloseOtherSurfaces: panel.closeOthers,
    onCloseSurfacesToRight: panel.closeToRight,
    onCloseAllSurfaces: panel.closeAll,
    onCopyFilePath: noop,
    onAddBrowser: noop,
    onAddBrowserInProfile: noop,
    onAddTerminal: addTerminal,
    terminalDisabledReason: terminalReason,
    onAddDiff: addDiff,
    onAddFiles: addFiles,
    onAddPullRequest: addPullRequest,
    onAddOutput: addOutput,
    browserAvailable: false,
    terminalAvailable: terminalIsAvailable,
    diffAvailable,
    filesAvailable,
    pullRequestAvailable,
    // The same condition as Files: the Output surface needs a worktree, and a
    // worktree belongs to an issue (§18.1).
    outputAvailable: filesAvailable,
    liveAgentCount: input.surfaces.agents.liveCount,
  } as const;

  const rendered = !shouldUseSheet ? (
    <RightPanelTabs mode="inline" open={panel.isOpen} maximized={panel.maximized} {...tabProps}>
      {content}
    </RightPanelTabs>
  ) : (
    <RightPanelSheet
      animationDurationMs={panelAnimationsActive ? panelAnimationDurationMs : 0}
      open={panel.isOpen}
      label={input.title === undefined ? "Right panel" : `Right panel: ${input.title}`}
      onClose={panel.close}
    >

      <h1 className="dc-sr-only" data-testid="panel-heading">
        {input.title ?? "Right panel"}
      </h1>
      <RightPanelTabs
        mode="sheet"
        layoutControls={
          panel.isOpen ? <div className="mr-px flex items-center">{panelToggleControls}</div> : null
        }
        {...tabProps}
      >
        {content}
      </RightPanelTabs>
    </RightPanelSheet>
  );

  return {
    layoutControls,
    panel: panel.isOpen || !shouldUseSheet ? rendered : rendered,
    open: panel.isOpen,
    maximized: panel.maximized,
    openDiff: addDiff,
    diffAvailable,
    openPullRequest: addPullRequest,
    pullRequestAvailable,
    openConversation: addConversation,
    conversationAvailable: conversationId !== null,
    openFiles: addFiles,
    filesAvailable,
    openOutput: addOutput,
    outputAvailable: filesAvailable,
    openTerminal: addTerminal,
    newTerminal,
    closeTerminal: closeActiveTerminal,
    terminalAvailable: terminalIsAvailable,
    terminalBlockedReason: terminalReason,
    runProjectAction,
    toggleRightPanel: toggle,
    closeRightPanel: panel.close,
    toggleMaximized: panel.toggleMaximized,
  };
}

const EMPTY_IDS: ReadonlySet<string> = new Set();
const EMPTY_SESSIONS = {} as const;
const EMPTY_OVERLAYS = {} as const;
const EMPTY_LABELS: ReadonlyMap<string, string> = new Map();
