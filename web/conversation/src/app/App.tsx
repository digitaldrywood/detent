// Application shell and route surfaces.
import { useAtomSet, useAtomValue } from "@effect/atom-react";
import * as Option from "effect/Option";
import { AsyncResult } from "effect/unstable/reactivity";
import React from "react";
import { Outlet, useNavigate, useParams, useRouterState } from "@tanstack/react-router";

import type {
  Answers,
  Conversation,
  Expected,
  IssueProposal,
  IssueResult,
  Message,
  Question,
  TurnPreferences,
} from "../contracts/index.ts";
import {
  DEFAULT_TURN_PREFERENCES,
  HUB_ENVIRONMENT_ID,
  projectCoordinatorAvailable,
  readAlreadyLinked,
  readIssueResult,
  turnPreferences,
} from "../contracts/index.ts";
import type { ConversationClient } from "../runtime/bootstrap.ts";
import type {
  ConversationDetail,
  PendingControl,
  PendingMessage,
} from "../runtime/state/conversationState.ts";
import {
  isStreaming,
  latestControl,
  unsettledControls,
} from "../runtime/state/conversationState.ts";
import { newCommandKey, readLastProject, useClient, writeLastProject } from "./client.ts";
import { Button } from "../components/ui/button.tsx";
import { Composer } from "./components/Composer.tsx";
import { ComposerContextStrip } from "./components/ComposerContextStrip.tsx";
import { useIssuePullRequest } from "./adapters/issuePullRequest.ts";
import { DraftHeroHeadline } from "./components/DraftHeroHeadline.tsx";
import {
  type HandoffFailure,
  HandoffForm,
  type HandoffValues,
} from "./components/HandoffForm.tsx";
import { QuestionRecord } from "./components/QuestionRecord.tsx";
import { Timeline } from "./components/Timeline.tsx";
import { ChatWorkspace } from "./components/ChatWorkspace.tsx";
import { SidebarDataProvider } from "./adapters/sidebarData.tsx";
import { publishPaletteShell } from "./adapters/paletteContext.ts";
import { useKeybindingActions } from "./adapters/keybindingActions.ts";
import { CommandPalette } from "./components/CommandPalette.tsx";
import { AnchoredToastProvider, ToastProvider } from "../components/ui/toast.tsx";
import { useComposerAttachments } from "./adapters/attachments.ts";
import { composerBanners } from "./adapters/composerBanners.tsx";
import { usePendingQuestion } from "./adapters/pendingQuestions.ts";
import {
  focusSidebarSearch,
  publishSidebarData,
  useSidebarSearchQuery,
  type SidebarShellData,
} from "./adapters/sidebarData.tsx";
import type { LegendListRef } from "@legendapp/list/react";
import { runNarrowComposerTransition } from "./lib/draftHeroTransition.ts";
import { SidebarInset, useSidebar } from "../components/ui/sidebar.tsx";
import { AppSidebarLayout } from "../components/AppSidebarLayout.tsx";
import {
  chipStamp,
  LIVE_TOOLTIP,
  staleClientTooltip,
} from "./lib/connectionTooltips.ts";
import { expectedOwner, isActive } from "./lib/execution.ts";
import { shortcutFor } from "./lib/shortcuts.ts";
import { conversationDestination } from "./lib/conversationDestination.ts";
import { shouldSearchServer } from "./lib/sidebarLogic.ts";

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
  readonly setProjectId: (id: string) => void;
  readonly conversations: readonly Conversation[];
  readonly connection: ConnectionChip;
  readonly refreshList: () => void;
}

const ShellContext = React.createContext<ShellState | null>(null);

/** The shell's state, for the route surfaces it renders through its outlet. */
export function useShell(): ShellState {
  const shell = React.useContext(ShellContext);
  if (shell === null) throw new Error("The shell is not mounted.");
  return shell;
}

/**
 * The transport chip (design inventory B.10). Every degraded state names when
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

  return (
    <ToastProvider>
      <AnchoredToastProvider>
        <ShellBody />
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
  const params = useParams({ strict: false }) as { conversationId?: string };
  // The Work destination and the Browse group need to know where they are.
  // `useRouterState` is the only source of the current path that stays correct
  // through a redirect; `params` alone cannot tell `/work` from `/work/changes`.
  const activePath = useRouterState({ select: (state) => state.location.pathname });

  const projects = client.bootstrap.projects;
  const [projectId, setProjectIdState] = React.useState(
    () => readLastProject() ?? projects[0]?.id ?? "",
  );
  // The copied `components/Sidebar.tsx` owns its search box and searches its
  // own list by title (`Sidebar.logic.ts`). The shell reads that query back
  // off the input so it can ask the hub for conversations past the first page
  // and merge them into the list the sidebar searches
  // (`state/entities.ts` `useThreadShells`).
  const query = useSidebarSearchQuery();
  const [serverResults, setServerResults] = React.useState<readonly Conversation[]>([]);
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
    setProjectId,
    conversations,
    connection,
    refreshList: () => void refreshList(),
  };

  const rename = useAtomSet(client.renameConversation, { mode: "promise" });

  const sidebarProps: SidebarShellData = {
    organizationName: client.bootstrap.organization.name,
    projects,
    activeProjectId: projectId === "" ? null : projectId,
    onProjectChange: (id: string) => {
      setProjectId(id);
      void navigate({ to: "/chat/p/$projectId", params: { projectId: id } });
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
    // open beside it; an unlinked one is still a chat (decisions.md §19.4).
    onSelect: (conversation) => {
      setProjectId(conversation.project_id);
      void navigate(conversationDestination(conversation));
    },
    onNewChat: startNewChat,
    preferenceChoices: client.bootstrap.preferences,
    navigation: {
      activePath,
      onNavigate: (to: string) => void navigate({ to }),
    },
    attention,
  };

  // `state/entities.ts` reads the same value outside React, for the one path
  // the copied sidebar takes without a hook (`readThreadShell`).
  publishSidebarData(sidebarProps);

  return (
    <ShellContext.Provider value={shell}>

      <SidebarDataProvider value={sidebarProps}>

        <CommandPalette>
          <AppSidebarLayout>

            <ShellShortcuts onNewChat={startNewChat} />
            <SidebarInset className="dc-main min-h-0 overflow-hidden">
              <Outlet />
            </SidebarInset>
          </AppSidebarLayout>
        </CommandPalette>
      </SidebarDataProvider>
    </ShellContext.Provider>
  );
}

function ShellShortcuts({ onNewChat }: { readonly onNewChat: () => void }): null {
  const { isMobile, openMobile, setOpenMobile, toggleSidebar } = useSidebar();

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
      event.preventDefault();
      if (shortcut === "focus-search") focusSearch();
      else {
        if (isMobile) setOpenMobile(false);
        onNewChat();
      }
    };
    globalThis.document?.addEventListener("keydown", onKeyDown);
    return () => globalThis.document?.removeEventListener("keydown", onKeyDown);
  }, [focusSearch, onNewChat, isMobile, setOpenMobile]);

  return null;
}

/** The new-chat surface: one composer, centred, docking after the first send. */
export function NewChat({ projectId }: { projectId?: string }): React.ReactElement {
  const client = useClient();
  const shell = useShell();
  const navigate = useNavigate();
  const create = useAtomSet(client.createConversation, { mode: "promise" });
  const sendFirst = useAtomSet(client.sendMessage, { mode: "promise" });
  const active = projectId ?? shell.projectId;
  const project = client.bootstrap.projects.find((candidate) => candidate.id === active);
  // No runner able to take coordinator turns is enrolled here. The message is
  // still accepted and still queues (decisions.md §9.1), so the send stays
  // available and the note says what will happen instead of what is forbidden.
  const coordinator = projectCoordinatorAvailable(client.bootstrap, active);

  const scope = { accountKey: accountKey(client), projectId: active, conversationId: "new" };
  const [draft, setDraft] = React.useState(() => client.drafts.readDraft(scope));
  const [sending, setSending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  // There is no conversation to PATCH yet, so the draft's pickers hold their
  // choice here and it travels to the conversation the first send creates.
  const [preferences, setPreferences] = React.useState<TurnPreferences>(
    DEFAULT_TURN_PREFERENCES,
  );
  const commandKey = React.useRef(newCommandKey());
  // Files dropped on the draft wait as staged chips; they upload against the
  // conversation the first send creates (decisions.md §17.1).
  const attachments = useComposerAttachments(client, { projectId: active, conversationId: null });
  // The create key outlives this component. `POST /conversations` is
  // idempotent by actor and key (decisions.md §10.2), so the key is written to
  // the outbox before the request and only cleared once the hub answers: a
  // reload mid-flight finds it and retries the same create rather than opening
  // a second conversation.
  const createKey = React.useRef<string>(
    client.drafts.readOutbox(scope)?.key ?? newCommandKey(),
  );

  React.useEffect(() => {
    setDraft(client.drafts.readDraft(scope));
    createKey.current = client.drafts.readOutbox(scope)?.key ?? newCommandKey();
    // Only the project scope changes the draft identity here.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active]);

  const onChange = (value: string) => {
    setDraft(value);
    client.drafts.writeDraft(scope, value);
  };

  const send = async () => {
    if (project?.can_write === false) {
      setError("You have read-only access to this project.");
      return;
    }
    if (draft.trim().length === 0 && attachments.staged.length === 0) return;
    setSending(true);
    setError(null);
    client.drafts.writeOutbox(scope, {
      key: createKey.current,
      kind: "create",
      text: draft,
      createdAt: new Date().toISOString(),
      status: "unknown",
      error: null,
    });
    // With files staged the conversation is created first, the files upload
    // against it, and the first message then names them; without files the
    // create carries the first message as before.
    const withFiles = attachments.staged.length > 0;
    const result = await create({
      projectId: active,
      key: createKey.current,
      ...(withFiles ? {} : { firstMessage: { key: commandKey.current, text: draft } }),
    });
    if (result._tag !== "Success") {
      setSending(false);
      // The draft, the create key and the message key all survive: retrying
      // reuses them, so a create that did land cannot produce a second
      // conversation and a message that did land cannot produce a second
      // message.
      setError(result.failure.message);
      return;
    }
    if (withFiles) {
      const createdId = result.success.conversation.id;
      const ids = await attachments.uploadAll(createdId);
      if (ids === null) {
        setSending(false);
        setError("An attachment did not upload. Retry it or remove it, then send again.");
        return;
      }
      // Delivery is reported through the message's receipt on the
      // conversation page, as every later send is.
      await sendFirst({
        projectId: active,
        conversationId: createdId,
        key: commandKey.current,
        text: draft,
        attachments: ids,
      });
      attachments.clear();
    }
    setSending(false);
    client.drafts.writeOutbox(scope, null);
    client.drafts.writeDraft(scope, "");
    commandKey.current = newCommandKey();
    createKey.current = newCommandKey();
    // The composer moves from centred to docked across this navigation. On a
    // narrow viewport that move is animated; reduced motion and browsers
    // without view transitions fall through to a plain navigation.
    await runNarrowComposerTransition(() =>
      navigate({
        to: "/chat/c/$conversationId",
        params: { conversationId: result.success.conversation.id },
      }),
    );
  };

  return (
    <ChatWorkspace
      project={project ?? null}
      projectName={project?.name ?? "Project"}
      conversationId={null}
      title="New chat"
      srHeading={false}
      isServerThread={false}
      workItemId={null}
      onDropFiles={project?.can_write === false ? undefined : attachments.addFiles}
      issueIdentifier={null}
      onCreateIssue={null}
      onNewThreadInProject={() => void navigate({ to: "/chat" })}
    >

      <div className="dc-hero flex min-h-0 flex-1 flex-col items-center justify-center overflow-y-auto px-3 py-8 sm:px-5">
        <div className="w-full max-w-3xl">
          <div className="pb-8">
            <DraftHeroHeadline
              projects={client.bootstrap.projects}
              activeProjectId={active === "" ? null : active}
              activeProjectName={project?.name ?? null}
              onProjectChange={(id) => {
                shell.setProjectId(id);
                void navigate({ to: "/chat/p/$projectId", params: { projectId: id } });
              }}
            />
          </div>
          {error === null ? null : (
            <div
              className="mx-auto mb-2 w-full max-w-3xl rounded-md bg-error-surface px-3 py-2 text-error-foreground text-sm"
              role="alert"
            >
              {error}
            </div>
          )}
          <Composer
            value={draft}
            onChange={onChange}
            onSend={() => void send()}
            sending={sending}
            streaming={false}
            autoFocus
            label="Message"
            placeholder="Ask for changes, send follow-ups, or describe the work"
            blockedReason={project?.can_write === false ? "Read-only project" : null}
            attachments={attachments}
            preferences={preferences}
            preferenceChoices={client.bootstrap.preferences}
            onPreferencesChange={setPreferences}
            // There is no conversation to hand off yet, so `/issue` and
            // `/handoff` are not on offer here; the draft's pickers, paperclip
            // and draft are, and the composer supplies those itself.
            slashCommands={[
              {
                name: "shortcuts",
                description: "Open the keyboard shortcuts",
                run: () =>
                  void navigate({ to: "/settings/$section", params: { section: "keybindings" } }),
              },
            ]}
            // No hint line under this card, so nothing for the editor to
            // point at: an `aria-describedby` aimed at an element that is not
            // rendered is worse than none.
            describedBy={null}
            contextStrip={
              <ComposerContextStrip
                projectName={project?.name ?? null}
                issueIdentifier={null}
                onCreateIssue={null}
                draft
              />
            }
          />
          {coordinator ? null : (
            <p
              className="mx-auto mt-2 w-full max-w-3xl px-1 text-center text-muted-foreground/70 text-xs"
              data-testid="coordinator-note"
              role="status"
            >
              No runner can answer chats in this project yet. Your message is saved and waits for
              the first runner that can.
            </p>
          )}
        </div>
      </div>
    </ChatWorkspace>
  );
}

function accountKey(client: ConversationClient): string {
  return `${client.bootstrap.organization.id}:${client.bootstrap.actor.principal_id}`;
}

/** The issue a linked conversation carries, as the transcript reports it. */

function ChatContextStrip(
  props: React.ComponentProps<typeof ComposerContextStrip> & {
    readonly projectId: string;
    readonly workItemId: string | null;
  },
): React.ReactElement {
  const { projectId, workItemId, ...strip } = props;
  const pullRequest = useIssuePullRequest(projectId, workItemId);
  return <ComposerContextStrip {...strip} pullRequest={pullRequest} />;
}

export function linkedIssue(detail: ConversationDetail): IssueResult | null {
  if (detail.conversation.work_item_id === null) return null;
  for (let index = detail.messages.length - 1; index >= 0; index -= 1) {
    const message = detail.messages[index];
    if (message === undefined) continue;
    const issue = readIssueResult(message.data);
    if (issue !== undefined) return issue;
  }
  return null;
}

/** The opening user message, used to seed the handoff form. */
function firstUserText(detail: ConversationDetail): string {
  return detail.messages.find((message) => message.role === "user")?.text ?? "";
}

export function ConversationRoute(): React.ReactElement {
  const { conversationId } = useParams({ from: "/chat/c/$conversationId" });
  const shell = useShell();
  const navigate = useNavigate();
  const known = shell.conversations.find((candidate) => candidate.id === conversationId);
  const projectId = known?.project_id ?? shell.projectId;
  // A linked conversation is not a page of its own any more: the issue is the
  // page and the conversation is a surface on it (decisions.md §19.4). The
  // redirect carries `?panel=conversation`, so the reader lands on the issue
  // with the chat already open beside it. An unlinked chat keeps this page.
  const linkedWorkItem = known?.work_item_id ?? null;
  React.useEffect(() => {
    if (linkedWorkItem === null) return;
    void navigate({
      to: "/work/i/$workItemId",
      params: { workItemId: linkedWorkItem },
      search: { panel: "conversation" },
      replace: true,
    });
  }, [linkedWorkItem, navigate]);
  if (linkedWorkItem !== null) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center p-8 text-muted-foreground text-sm">
        Opening the issue…
      </div>
    );
  }
  return <ConversationView key={conversationId} conversationId={conversationId} projectId={projectId} />;
}

export function ConversationView({
  conversationId,
  projectId,
  header = true,
}: {
  conversationId: string;
  projectId: string;
  /**
   * Whether this view renders the route's top bar. The Work issue page embeds
   * the same transcript under its own header (`src/app/work/IssuePage.tsx`),
   * and two 52px bands stacked on one route is one band too many; everything
   * else about the view is unchanged.
   */
  header?: boolean;
}): React.ReactElement {
  const client = useClient();
  const shell = useShell();
  const navigate = useNavigate();
  const thread = React.useRef<HTMLDivElement>(null);
  const state = useAtomValue(
    client.conversations.stateAtom({
      environmentId: HUB_ENVIRONMENT_ID,
      projectId,
      conversationId,
    }),
  );
  const sendMessage = useAtomSet(client.sendMessage, { mode: "promise" });
  const retryPending = useAtomSet(client.retryPending, { mode: "promise" });
  const retryMessage = useAtomSet(client.retryMessage, { mode: "promise" });
  const setPreferences = useAtomSet(client.setPreferences, { mode: "promise" });
  const sendControl = useAtomSet(client.sendControl, { mode: "promise" });
  const sendAnswer = useAtomSet(client.sendAnswer, { mode: "promise" });
  const sendInterrupt = useAtomSet(client.sendInterrupt, { mode: "promise" });
  const sendContinue = useAtomSet(client.sendContinue, { mode: "promise" });
  const retryControlCommand = useAtomSet(client.retryControl, { mode: "promise" });
  const discardControl = useAtomSet(client.discardControl, { mode: "promise" });
  const acknowledgeStale = useAtomSet(client.acknowledgeStale, { mode: "promise" });
  const refreshConversation = useAtomSet(client.refreshConversation, { mode: "promise" });
  const dismissPending = useAtomSet(client.dismissPending, { mode: "promise" });
  const loadOlder = useAtomSet(client.loadOlder, { mode: "promise" });
  const link = useAtomSet(client.link, { mode: "promise" });

  const value = Option.getOrUndefined(AsyncResult.value(state));
  const detail = value === undefined ? undefined : Option.getOrUndefined(value.data);

  const scope = {
    accountKey: accountKey(client),
    projectId,
    conversationId,
  };
  // The link outbox lives beside the draft, under its own conversation key, so
  // a message outbox entry and a half-finished handoff never overwrite one
  // another.
  const linkScope = { ...scope, conversationId: `${conversationId}#link` };
  const [draft, setDraft] = React.useState(() => client.drafts.readDraft(scope));
  const [sending, setSending] = React.useState(false);
  const commandKey = React.useRef(newCommandKey());

  const [handoffOpen, setHandoffOpen] = React.useState(false);
  const [handoffProposal, setHandoffProposal] = React.useState<IssueProposal | null>(null);
  const [handoffSubmitting, setHandoffSubmitting] = React.useState(false);
  const [handoffFailure, setHandoffFailure] = React.useState<HandoffFailure | null>(null);
  const handoffTrigger = React.useRef<HTMLElement | null>(null);
  const linkKey = React.useRef<string>(
    client.drafts.readOutbox(linkScope)?.key ?? newCommandKey(),
  );

  const timelineList = React.useRef<LegendListRef | null>(null);
  const [following, setFollowing] = React.useState(true);
  const jumpToLatest = React.useCallback(() => {
    timelineList.current?.scrollToEnd({ animated: true });
  }, []);
  // The files staged for the next message (decisions.md §17.1). The whole chat
  // surface drops into it and the composer's paperclip picks into it.
  const attachments = useComposerAttachments(client, { projectId, conversationId });

  const project = client.bootstrap.projects.find((candidate) => candidate.id === projectId);
  const canWrite = project?.can_write !== false;
  const streaming = detail !== undefined && isStreaming(detail);

  const openHandoff = (proposal: IssueProposal | null) => {
    handoffTrigger.current = (globalThis.document?.activeElement as HTMLElement | null) ?? null;
    setHandoffProposal(proposal);
    setHandoffFailure(null);
    setHandoffOpen(true);
  };

  const closeHandoff = React.useCallback(() => {
    setHandoffOpen(false);
    handoffTrigger.current?.focus();
  }, []);

  const send = async () => {
    const files = attachments.ids;
    // A message is text, files, or both (decisions.md §17.1): a drop with no
    // sentence is still something to send.
    if (draft.trim().length === 0 && files.length === 0) return;
    if (attachments.uploading) return;
    setSending(true);
    const text = draft;
    await sendMessage({
      projectId,
      conversationId,
      key: commandKey.current,
      text,
      ...(files.length === 0 ? {} : { attachments: files }),
      expected: {
        attempt_id: detail?.conversation.execution.attempt_id ?? null,
        turn_id: detail?.conversation.execution.turn_id ?? null,
      },
    });
    setSending(false);
    commandKey.current = newCommandKey();
    setDraft("");
    client.drafts.writeDraft(scope, "");
    attachments.clear();
  };

  // Retrying a message never sends a second one (decisions.md §10.3): where the
  // hub named the message, this is a `retry` command carrying a new key and
  // that message id; where it did not, the identical `message` command goes
  // back under its original key, `expected` included.
  const retry = (entry: PendingMessage) =>
    void retryPending({ projectId, conversationId, key: newCommandKey(), entry });

  const retryStoredMessage = (message: Message) =>
    void retryMessage({
      projectId,
      conversationId,
      key: newCommandKey(),
      messageId: message.id,
    });

  const answerQuestion = (question: Question, answers: Answers, key = newCommandKey()) => {
    const expected: Expected = {
      attempt_id: question.owner.attempt_id,
      turn_id: question.owner.turn_id,
    };
    void sendAnswer({
      projectId,
      conversationId,
      key,
      questionId: question.id,
      answers,
      expected,
    });
  };

  // The outbox entry carries its own payload, so a retry works after the view
  // was unmounted and remounted — navigating away and back used to leave this
  // button unable to rebuild the command it had to resend.
  const retryControl = (entry: PendingControl) =>
    void retryControlCommand({ projectId, conversationId, entry });

  const stopFromKeyboard = React.useRef<(() => void) | null>(null);
  useKeybindingActions(
    React.useMemo(
      () => ({
        "thread.stop": () => {
          const stopNow = stopFromKeyboard.current;
          if (stopNow === null) return false;
          stopNow();
          return true;
        },
      }),
      [],
    ),
  );

  const openQuestion =
    detail === undefined
      ? null
      : (detail.questions.find((candidate) => {
          if (candidate.status === "pending") return true;
          const own = latestControl(detail, "answer", candidate.id);
          return own?.status === "unknown" || own?.status === "rejected";
        }) ?? null);
  const openQuestionControl =
    detail === undefined || openQuestion === null
      ? undefined
      : latestControl(detail, "answer", openQuestion.id);
  const pendingQuestion = usePendingQuestion({
    question: openQuestion,
    control: openQuestionControl,
    disabled: !canWrite,
    onAnswer: (answers) => {
      if (openQuestion !== null) answerQuestion(openQuestion, answers);
    },
    onRetry: retryControl,
    onDismiss: (question) =>
      void dismissPending({ projectId, conversationId, key: `question:${question.id}` }),
  });

  const submitHandoff = async (values: HandoffValues) => {
    setHandoffSubmitting(true);
    setHandoffFailure(null);
    // The key is recorded before the request so a reload during the round trip
    // finds it and reuses it instead of creating a second issue.
    client.drafts.writeOutbox(linkScope, {
      key: linkKey.current,
      kind: "link",
      text: values.title,
      createdAt: new Date().toISOString(),
      status: "unknown",
      error: null,
    });
    const result = await link({
      projectId,
      conversationId,
      key: linkKey.current,
      shareHistory: values.shareHistory,
      issue: {
        title: values.title,
        description: values.description,
        ...(values.labels.length > 0 ? { labels: [...values.labels] } : {}),
        ...(values.priority === null ? {} : { priority: values.priority }),
        ...(values.next.state === undefined ? {} : { state: values.next.state }),
      },
      next: values.next,
    });
    setHandoffSubmitting(false);
    if (result._tag !== "Success") {
      const failure = result.failure;
      // The form keeps its values and its key: the next attempt is the same
      // intent, not a new one.
      setHandoffFailure({
        code: failure._tag === "ApiRequestError" ? failure.code : "network",
        message: failure.message,
        existingConversationId:
          failure._tag === "ApiRequestError"
            ? (readAlreadyLinked(failure.details)?.existing_conversation_id ?? null)
            : null,
      });
      return;
    }
    client.drafts.writeOutbox(linkScope, null);
    linkKey.current = newCommandKey();
    setHandoffOpen(false);
    shell.refreshList();
  };

  const openIssue = (workItemId: string) =>
    void navigate({ to: "/work/i/$workItemId", params: { workItemId } });

  const frame = (
    input: {
      readonly title: string;
      readonly isServerThread: boolean;
      readonly workItemId: string | null;
      readonly issueIdentifier: string | null;
      readonly onCreateIssue: (() => void) | null;
    },
    body: React.ReactNode,
  ): React.ReactElement =>
    header ? (
      <ChatWorkspace
        project={project ?? null}
        projectName={project?.name ?? "Project"}
        conversationId={conversationId}
        title={input.title}
        isServerThread={input.isServerThread}
        workItemId={input.workItemId}
        issueIdentifier={input.issueIdentifier}
        onCreateIssue={input.onCreateIssue}
        onNewThreadInProject={() => void navigate({ to: "/chat" })}
        onDropFiles={attachments.addFiles}
      >
        {body}
      </ChatWorkspace>
    ) : (
      <>{body}</>
    );

  if (value === undefined || (detail === undefined && value.status !== "gone")) {
    // A snapshot that failed is not a snapshot that is slow. Without this the
    // reader watched "Opening conversation…" forever with nothing to press.
    const failure = value === undefined ? undefined : Option.getOrUndefined(value.error);
    return frame(
      {
        title: failure === undefined ? "Opening conversation" : "Conversation unavailable",
        isServerThread: false,
        workItemId: null,
        issueIdentifier: null,
        onCreateIssue: null,
      },
      <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground text-sm">
        {failure === undefined ? (
          <p>Opening conversation…</p>
        ) : (
          <div className="flex flex-col items-center gap-3" role="alert" data-testid="snapshot-error">
            <p>{failure}</p>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void refreshConversation({ projectId, conversationId })}
            >
              Try again
            </Button>
          </div>
        )}
      </div>,
    );
  }

  if (detail === undefined) {
    const reason = Option.getOrUndefined(value.closedReason);
    return frame(
      {
        title: "Conversation unavailable",
        isServerThread: false,
        workItemId: null,
        issueIdentifier: null,
        onCreateIssue: null,
      },
      <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground text-sm">
        <div className="flex flex-col items-center gap-3" role="alert">
          <p>
            {reason === "access_revoked"
              ? "Your access to this conversation was removed."
              : "This conversation is not available."}
          </p>
          <a className="text-primary underline-offset-4 hover:underline" href="/chat">
            Back to your chats
          </a>
        </div>
      </div>,
    );
  }

  const execution = detail.conversation.execution;
  const linked = detail.conversation.work_item_id !== null;
  // A settled chat is not a closed one (decisions.md §13.9): it settles
  // because nothing has happened for a while and it unsettles the moment
  // something does, so the composer stays open and sending is what unsettles
  // it. The one reason to close the composer is a reader who may never send.
  const composerBlockedReason = !canWrite
    ? "You can read this chat but not send messages"
    : value.status === "cached"
      ? "Reconnecting — you can keep drafting"
      : null;
  const closed = !canWrite;
  // Stopping a linked attempt is an `interrupt` aimed at that attempt; stopping
  // an unlinked chat is `cancel`, which the hub delivers as an interrupt to the
  // runner holding the coordinator work item (decisions.md §9.1). One call
  // site, so the composer's stop and the strip's Stop can never diverge.
  const stop = () =>
    void sendControl({
      projectId,
      conversationId,
      command: {
        key: newCommandKey(),
        kind: linked ? "interrupt" : "cancel",
        expected: expectedOwner(execution),
      },
    });
  // What `thread.stop` does, now that both are known. Null while there is no
  // turn to stop or this reader may not write, so the chord is inert rather
  // than sending a control the hub would reject — the same rule the Stop
  // button itself follows.
  stopFromKeyboard.current = closed || !isActive(execution.status) ? null : stop;

  const issue = linkedIssue(detail);
  const outbox = unsettledControls(detail).filter((entry) => entry.kind !== "answer");
  const interruptEntry = latestControl(detail, "interrupt");
  const continueEntry = latestControl(detail, "continue");
  const stripControls = [interruptEntry, continueEntry].filter(
    (entry): entry is PendingControl =>
      entry !== undefined && (entry.status === "unknown" || entry.status === "rejected"),
  );

  const banners = composerBanners({
    ...(linked
      ? {
          execution: {
            execution,
            controls: stripControls,
            stale: detail.staleExecution,
            onDismissStale: () => void acknowledgeStale({ projectId, conversationId }),
            onInterrupt: () =>
              void sendInterrupt({
                projectId,
                conversationId,
                key: newCommandKey(),
                expected: expectedOwner(execution),
              }),
            onContinue: () =>
              void sendContinue({
                projectId,
                conversationId,
                key: newCommandKey(),
                expected: { attempt_id: execution.attempt_id, turn_id: null },
              }),
            onRetryControl: retryControl,
            onDiscardControl: (entry: PendingControl) =>
              void discardControl({ projectId, conversationId, key: entry.key }),
            readOnly: closed,
            blockedReason: composerBlockedReason,
          },
        }
      : execution.status === "idle"
        ? {}
        : {
            // An unlinked chat has a coordinator turn to report on from the
            // moment its work item exists. It gets the chat vocabulary and one
            // control — Stop — and never a model, provider, cost or key
            // choice: the turn runs on the customer's runner with that
            // runner's own login (decisions.md §1).
            execution: {
              execution,
              surface: "chat" as const,
              projectName: project?.name ?? null,
              controls: [],
              stale: detail.staleExecution,
              onDismissStale: () => void acknowledgeStale({ projectId, conversationId }),
              onInterrupt: stop,
              onContinue: () => {},
              onStop: stop,
              onRetryControl: retryControl,
              onDiscardControl: (entry: PendingControl) =>
                void discardControl({ projectId, conversationId, key: entry.key }),
              readOnly: closed,
              blockedReason: composerBlockedReason,
            },
          }),
  });

  const promptHistory = detail.messages.map((message) => ({
    id: message.id,
    role: message.role,
    text: message.text,
  }));

  return frame(
    {
      title: detail.conversation.title,
      isServerThread: true,
      workItemId: detail.conversation.work_item_id,
      issueIdentifier: issue?.identifier ?? detail.conversation.work_item_id,
      onCreateIssue: linked || closed ? null : () => openHandoff(null),
    },
    <>
      <div className="relative flex min-h-0 flex-1 flex-col">

        <div className="flex min-h-0 flex-1 flex-col" ref={thread}>
          <Timeline
            detail={detail}
            listRef={timelineList}
            onIsAtEndChange={setFollowing}
            onLoadOlder={() => void loadOlder({ projectId, conversationId })}
            onRetry={retry}
            {...(closed ? {} : { onRetryMessage: retryStoredMessage })}
            onDismiss={(entry) =>
              void dismissPending({ projectId, conversationId, key: entry.key })
            }
            onCreateIssue={linked || closed ? undefined : (proposal) => openHandoff(proposal)}
            onOpenIssue={openIssue}

            renderQuestion={(question) =>
              question.id === openQuestion?.id ? null : (
                <QuestionRecord
                  question={question}
                  sentence={
                    question.status === "expired"
                      ? "This question expired. The runner stopped waiting for an answer."
                      : "This question is closed. It cannot be answered again."
                  }
                />
              )
            }
          />
          <div className="mx-auto flex w-full max-w-3xl flex-col gap-1 px-3 pb-4 sm:px-4">
            {outbox.length === 0 ? null : (
              <div className="flex flex-col gap-1" aria-label="Unconfirmed commands">
                {outbox.map((entry) => (
                  <div
                    className="flex items-center gap-2 px-1 text-muted-foreground text-xs"
                    key={entry.key}
                    data-testid="outbox-entry"
                  >
                    <span className="size-1.5 shrink-0 rounded-full bg-warning" aria-hidden="true" />
                    <span>
                      <b className="font-medium text-foreground">
                        {entry.kind} {entry.status}
                      </b>{" "}
                      · attempt {entry.attemptId ?? "none"}
                    </span>
                    <span className="ml-auto" />
                    <Button
                      variant="ghost"
                      size="xs"
                      onClick={() =>
                        void discardControl({ projectId, conversationId, key: entry.key })
                      }
                    >
                      Discard
                    </Button>
                  </div>
                ))}
              </div>
            )}
            {Option.isSome(value.error) ? (
              <div
                className="rounded-md bg-warning-surface px-3 py-2 text-warning-foreground text-sm"
                role="status"
              >
                {value.error.value}
              </div>
            ) : null}
          </div>
        </div>
        {following ? null : (
          <div className="pointer-events-none absolute inset-x-0 bottom-2 z-30 flex justify-center">
            <Button
              variant="outline"
              size="sm"
              className="pointer-events-auto rounded-full shadow-lg"
              onClick={jumpToLatest}
            >
              Jump to latest
            </Button>
          </div>
        )}

        <div className="shrink-0 px-3 pb-3 sm:px-5 sm:pb-4">
          <div className="mx-auto w-full max-w-3xl">
            {handoffOpen ? (
              <HandoffForm
                projectId={projectId}
                projectName={project?.name ?? projectId}
                messageCount={detail.conversation.message_count}
                proposal={handoffProposal}
                seedTitle={detail.conversation.title}
                seedObjective={firstUserText(detail)}
                {...(project?.labels === undefined ? {} : { labels: project.labels })}
                {...(project?.states === undefined ? {} : { states: project.states })}
                submitting={handoffSubmitting}
                failure={handoffFailure}
                onSubmit={(values) => void submitHandoff(values)}
                onCancel={closeHandoff}
                onOpenExisting={(existing) => {
                  setHandoffOpen(false);
                  void navigate({
                    to: "/chat/c/$conversationId",
                    params: { conversationId: existing },
                  });
                }}
              />
            ) : null}
            <Composer
              value={draft}
              onChange={(next) => {
                setDraft(next);
                client.drafts.writeDraft(scope, next);
              }}
              onSend={() => void send()}
              {...(closed ? {} : { onStop: stop })}
              attachments={attachments}
              sending={sending}
              streaming={streaming}
              autoFocus={!closed}
              label="Message"
              blockedReason={composerBlockedReason}
              disabled={closed}
              preferences={turnPreferences(detail.conversation)}
              preferenceChoices={client.bootstrap.preferences}
              onPreferencesChange={(next) =>
                void setPreferences({ projectId, conversationId, preferences: next })
              }
              banners={banners}
              pendingQuestion={pendingQuestion}
              history={promptHistory}
              scrollRef={thread}
              // No hint line under this card, and no scope sentence in the
              // footer either: the execution strip above the card already says
              // what state the issue is in, and it is the strip that now says
              // where a send goes (Michael, September 12: "Queued for the next
              // attempt, why is this showing if we have the top banner").
              describedBy={null}
              // The chat's own slash commands. Everything else the menu offers
              // — the three pickers, the paperclip, the draft, the interrupt —
              // the composer supplies from its own handles.
              slashCommands={[
                ...(linked || closed
                  ? []
                  : [
                      {
                        name: "issue",
                        description: "Create a linked issue from this chat",
                        run: () => openHandoff(null),
                      },
                      {
                        name: "handoff",
                        description: "Hand this chat to a runner — the same form as /issue",
                        run: () => openHandoff(null),
                      },
                    ]),
                {
                  name: "shortcuts",
                  description: "Open the keyboard shortcuts",
                  run: () =>
                    void navigate({
                      to: "/settings/$section",
                      params: { section: "keybindings" },
                    }),
                },
              ]}
              contextStrip={
                <ChatContextStrip
                  projectId={projectId}
                  workItemId={detail.conversation.work_item_id}
                  projectName={project?.name ?? null}
                  issueIdentifier={
                    linked ? (issue?.identifier ?? detail.conversation.work_item_id) : null
                  }
                  issueLane={detail.conversation.work_item?.lane ?? issue?.lane ?? null}
                  onCreateIssue={linked || closed ? null : () => openHandoff(null)}
                  onOpenIssue={
                    linked && detail.conversation.work_item_id !== null
                      ? () => openIssue(detail.conversation.work_item_id as string)
                      : null
                  }
                />
              }
            />
            {value.status === "cached" ? (
              <p className="mt-1 flex justify-center">
                <Button
                  variant="ghost"
                  size="xs"
                  onClick={() => void refreshConversation({ projectId, conversationId })}
                >
                  Recover history
                </Button>
              </p>
            ) : null}
          </div>
        </div>
      </div>
    </>,
  );
}

export function ProjectNewChat(): React.ReactElement {
  const { projectId } = useParams({ from: "/chat/p/$projectId" });
  return <NewChat projectId={projectId} />;
}
