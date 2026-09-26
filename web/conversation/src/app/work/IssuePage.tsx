import { useAtomSet, useAtomValue } from "@effect/atom-react";
import * as Option from "effect/Option";
import { AsyncResult } from "effect/unstable/reactivity";
import { ChevronDownIcon, PlusIcon } from "lucide-react";
import React from "react";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";

import { Button } from "../../components/ui/button.tsx";
import {
  Collapsible,
  CollapsiblePanel,
  CollapsibleTrigger,
} from "../../components/ui/collapsible.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { toastManager } from "../../components/ui/toast.tsx";
import type {
  Conversation,
  Execution,
  PreferenceChoices,
  TurnPreferences,
} from "../../contracts/index.ts";
import {
  DEFAULT_TURN_PREFERENCES,
  HUB_ENVIRONMENT_ID,
  turnPreferences,
} from "../../contracts/index.ts";
import type {
  ChangeDetail,
  CollaborationEvent,
  NativeAttempt,
  NativeComment,
  NativeIssue,
  NativeLabel,
  NativeProject,
} from "../../contracts/work.ts";
import { priorityValue } from "../../contracts/work.ts";
import type { ConversationDetail } from "../../runtime/state/conversationState.ts";
import { isStreaming } from "../../runtime/state/conversationState.ts";
import { ConversationView, useShell } from "../App.tsx";
import { newCommandKey, useClient } from "../client.ts";
import { ChatWorkspace, useWorkspacePanel } from "../components/ChatWorkspace.tsx";
import { Markdown } from "../components/Markdown.tsx";
import { canInterrupt, executionCopy, expectedOwner, isActive } from "../lib/execution.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { ActivityFeed, LiveRow } from "./components/ActivityFeed.tsx";
import { IssueComposer } from "./components/IssueComposer.tsx";
import {
  IssueProperties,
  type MemberOption,
  type RelatedCandidate,
  type RelatedEntry,
} from "./components/IssueProperties.tsx";
import { IssueResources, type ResourceRow } from "./components/IssueResources.tsx";
import { mergeActivity, runnerLabel, type ActivityRow } from "./lib/activity.ts";
import { toChangeView, toWorkItemView, transitionsFrom } from "./lib/fromWire.ts";
import { elapsedLabel, issueNumber } from "./lib/format.ts";
import type { WorkItemView } from "./lib/model.ts";
import { useNow, useWorkHttp } from "./lib/useWork.ts";
import { newWorkKey, WorkApiError, type WorkHttp } from "./lib/workHttp.ts";

interface IssueData {
  readonly issue: NativeIssue;
  readonly project: NativeProject;
  readonly attempts: readonly NativeAttempt[];
  readonly history: readonly CollaborationEvent[];
  readonly comments: readonly NativeComment[];
  readonly change: ChangeDetail | null;
}

/**
 * Reads one issue and everything the page shows about it.
 *
 * Six requests, because the hub has no issue-detail projection: the item, its
 * project (for the workflow), its attempts, its history, its comments, and —
 * only when it has one — its change. They are issued together rather than in
 * sequence, so the page paints once.
 */
function useIssue(
  http: WorkHttp,
  projectId: string | null,
  workItemId: string,
): {
  data: IssueData | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
  /** Applies an optimistic or answered issue without a round trip. */
  apply: React.Dispatch<React.SetStateAction<IssueData | null>>;
} {
  const [data, setData] = React.useState<IssueData | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [nonce, setNonce] = React.useState(0);

  React.useEffect(() => {
    if (projectId === null) return;
    let cancelled = false;
    setLoading(true);
    void (async () => {
      try {
        const [issue, project, attempts, history, comments, changes] = await Promise.all([
          http.getWorkItem(projectId, workItemId),
          http.getProject(projectId),
          http.listAttempts(projectId, workItemId, 20).then((page) => page.items),
          http.listHistory({ projectId, itemId: workItemId, limit: 100 }).then((page) => page.items),
          http
            .listComments({ projectId, itemId: workItemId, limit: 100 })
            .then((page) => page.items)
            .catch(() => []),
          http.listChanges(projectId, workItemId).catch(() => []),
        ]);
        const latest = changes.at(-1);
        const change =
          latest === undefined
            ? null
            : await http.getChange(projectId, workItemId, latest.change_id).catch(() => null);
        if (cancelled) return;
        setData({ issue, project, attempts, history, comments, change });
        setError(null);
      } catch (cause) {
        if (cancelled) return;
        setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId, nonce]);

  return { data, error, loading, reload: () => setNonce((value) => value + 1), apply: setData };
}

/**
 * What the property pickers offer: the project's label catalogue, the other
 * work items a relation can point at, and the organization's members.
 *
 * Read once per project rather than per picker, because all three are project
 * facts rather than issue facts and a picker that fetched on open would make
 * the first keystroke wait for a round trip. Each one fails softly: a hub
 * that does not serve members answers the assignee picker with the reader
 * alone rather than taking the page down, which is the same rule the board
 * follows for a filter it cannot populate.
 */
function useIssueOptions(
  http: WorkHttp,
  members: () => Promise<readonly MemberOption[]>,
  projectId: string | null,
  nonce: number,
): {
  labels: readonly NativeLabel[];
  candidates: readonly RelatedCandidate[];
  members: readonly MemberOption[];
} {
  const [labels, setLabels] = React.useState<readonly NativeLabel[]>([]);
  const [candidates, setCandidates] = React.useState<readonly RelatedCandidate[]>([]);
  const [people, setPeople] = React.useState<readonly MemberOption[]>([]);

  React.useEffect(() => {
    if (projectId === null) return;
    let cancelled = false;
    void (async () => {
      const [catalogue, page, roster] = await Promise.all([
        http.listLabels(projectId).then((list) => list.items).catch(() => []),
        http.listWorkItems({ projectId, limit: 100 }).then((result) => result.items).catch(() => []),
        members().catch(() => []),
      ]);
      if (cancelled) return;
      setLabels(catalogue);
      setPeople(roster);
      setCandidates(
        page.map((issue) => ({
          id: issue.work_item_id,
          label: issue.title,
          detail: issue.state,
          category: issue.terminal ? "completed" : "unstarted",
        })),
      );
    })();
    return () => {
      cancelled = true;
    };
  }, [http, members, projectId, nonce]);

  return { labels, candidates, members: people };
}

/** Everything the page needs from the linked conversation, or nothing. */
interface ConversationBridge {
  readonly id: string;
  readonly projectId: string;
  readonly detail: ConversationDetail | undefined;
  readonly execution: Execution | null;
  readonly streaming: boolean;
  readonly preferences: TurnPreferences;
  readonly setPreferences: (preferences: TurnPreferences) => void;
  readonly sendToRunner: (text: string) => Promise<void>;
  readonly interrupt: (() => void) | null;
  /** The conversation view, for the right panel's `conversation` surface. */
  readonly view: React.ReactNode;
}

export function IssuePage(): React.ReactElement {
  const { workItemId } = useParams({ from: "/work/i/$workItemId" });
  const shell = useShell();
  // The conversation list is this client's only index from work item to
  // project, and to the conversation the issue is linked to.
  const linked = shell.conversations.find(
    (conversation) => conversation.work_item_id === workItemId,
  );
  if (linked === undefined) {
    return <IssueSurface workItemId={workItemId} projectHint={null} conversation={null} />;
  }
  return <LinkedIssue key={linked.id} workItemId={workItemId} conversation={linked} />;
}

/** The issue page with a conversation behind it. Subscribes to its atoms. */
function LinkedIssue({
  workItemId,
  conversation,
}: {
  readonly workItemId: string;
  readonly conversation: Conversation;
}): React.ReactElement {
  const client = useClient();
  const projectId = conversation.project_id;
  const conversationId = conversation.id;
  const state = useAtomValue(
    client.conversations.stateAtom({
      environmentId: HUB_ENVIRONMENT_ID,
      projectId,
      conversationId,
    }),
  );
  const sendMessage = useAtomSet(client.sendMessage, { mode: "promise" });
  const sendInterrupt = useAtomSet(client.sendInterrupt, { mode: "promise" });
  const setPreferences = useAtomSet(client.setPreferences, { mode: "promise" });

  const value = Option.getOrUndefined(AsyncResult.value(state));
  const detail = value === undefined ? undefined : Option.getOrUndefined(value.data);
  const execution = detail?.conversation.execution ?? null;
  const commandKey = React.useRef(newCommandKey());

  const bridge = React.useMemo<ConversationBridge>(
    () => ({
      id: conversationId,
      projectId,
      detail,
      execution,
      streaming: detail !== undefined && isStreaming(detail),
      preferences:
        detail === undefined ? DEFAULT_TURN_PREFERENCES : turnPreferences(detail.conversation),
      setPreferences: (preferences) => {
        void setPreferences({ projectId, conversationId, preferences });
      },
      sendToRunner: async (text) => {
        await sendMessage({
          projectId,
          conversationId,
          key: commandKey.current,
          text,
          expected: {
            attempt_id: execution?.attempt_id ?? null,
            turn_id: execution?.turn_id ?? null,
          },
        });
        commandKey.current = newCommandKey();
      },
      interrupt:
        execution === null || !canInterrupt(execution)
          ? null
          : () => {
              void sendInterrupt({
                projectId,
                conversationId,
                key: newCommandKey(),
                expected: expectedOwner(execution),
              });
            },
      // The conversation view this client already has, with its own top bar
      // suppressed: the panel has a tab bar of its own and two bands stacked
      // inside a 540px column is one band too many.
      view: <ConversationView conversationId={conversationId} projectId={projectId} header={false} />,
    }),
    [conversationId, detail, execution, projectId, sendInterrupt, sendMessage, setPreferences],
  );

  return (
    <IssueSurface workItemId={workItemId} projectHint={projectId} conversation={bridge} />
  );
}

/** Opens the panel when the route asked for it (`?panel=conversation`). */
function PanelIntent({ wanted }: { readonly wanted: boolean }): null {
  const panel = useWorkspacePanel();
  const opened = React.useRef(false);
  const available = panel?.conversationAvailable === true;
  const open = panel?.openConversation;
  React.useEffect(() => {
    if (!wanted || opened.current || !available || open === undefined) return;
    opened.current = true;
    open();
  }, [available, open, wanted]);
  return null;
}

const BODY_CLAMP = 900;

function IssueSurface({
  workItemId,
  projectHint,
  conversation,
}: {
  readonly workItemId: string;
  readonly projectHint: string | null;
  readonly conversation: ConversationBridge | null;
}): React.ReactElement {
  const shell = useShell();
  const navigate = useNavigate();
  const client = useClient();
  const http = useWorkHttp();
  const now = useNow();
  const search = useSearch({ strict: false }) as { panel?: string };

  const projectId = projectHint ?? shell.projectId ?? null;
  const { data, error, loading, reload, apply } = useIssue(http, projectId, workItemId);
  const [saving, setSaving] = React.useState(false);
  const [posting, setPosting] = React.useState(false);

  // The pickers' options. `nonce` re-reads them after a write that can change
  // them: attaching a new label puts it in the catalogue, and a relation
  // changes what "Add related…" should no longer offer.
  const accountApi = useAccountApi();
  const account = useAccountBootstrap();
  const [optionsNonce, setOptionsNonce] = React.useState(0);
  const readMembers = React.useCallback(
    async (): Promise<readonly MemberOption[]> =>
      (await accountApi.members()).members
        .filter((member) => member.status === "active" && member.email.trim().length > 0)
        .map((member) => ({ id: member.email, label: member.email })),
    [accountApi],
  );
  const options = useIssueOptions(http, readMembers, projectId, optionsNonce);
  const viewer = React.useMemo<MemberOption | null>(() => {
    const email = account?.actor.email ?? "";
    return email.trim().length === 0 ? null : { id: email, label: email };
  }, [account?.actor.email]);

  const project =
    projectId === null
      ? null
      : (client.bootstrap.projects.find((candidate) => candidate.id === projectId) ?? null);
  const canWrite = project?.can_write !== false;

  const item = React.useMemo<WorkItemView | null>(() => {
    if (data === null) return null;
    const change = data.change === null ? null : toChangeView(data.change.change, data.change);
    return toWorkItemView(data.issue, data.project.name, {
      attempts: data.attempts,
      change,
      conversationId: conversation?.id ?? null,
    });
  }, [conversation?.id, data]);

  const moves = React.useMemo(
    () => (data === null ? [] : transitionsFrom(data.project, data.issue.state)),
    [data],
  );

  /**
   * One mutation path, so every conflict is handled the same way. `preview`
   * is the optimistic shape: it lands on the page at once, the hub's answer
   * replaces it, and a failure puts the previous page back.
   */
  const mutate = React.useCallback(
    async (
      run: (revision: string) => Promise<NativeIssue>,
      what: string,
      preview?: (issue: NativeIssue) => NativeIssue,
    ) => {
      if (data === null) return;
      const before = data;
      if (preview !== undefined) apply({ ...data, issue: preview(data.issue) });
      setSaving(true);
      try {
        const updated = await run(data.issue.revision);
        apply((current) => (current === null ? current : { ...current, issue: updated }));
        reload();
      } catch (cause) {
        apply(before);
        const conflict = cause instanceof WorkApiError && cause.conflict;
        toastManager.add({
          type: conflict ? "warning" : "error",
          title: conflict ? "This issue changed while you were reading it" : `Could not ${what}`,
          description: conflict
            ? "Your change was not applied. The issue has been reloaded with what the hub has now."
            : cause instanceof Error
              ? cause.message
              : String(cause),
        });
        if (conflict) reload();
      } finally {
        setSaving(false);
      }
    },
    [data, reload, apply],
  );

  const postComment = React.useCallback(
    async (body: string) => {
      if (projectId === null) return;
      setPosting(true);
      try {
        await http.createComment({
          projectId,
          itemId: workItemId,
          key: newWorkKey("comment"),
          body,
        });
        reload();
      } catch (cause) {
        toastManager.add({
          type: "error",
          title: "Could not post the comment",
          description: cause instanceof Error ? cause.message : String(cause),
        });
      } finally {
        setPosting(false);
      }
    },
    [http, projectId, reload, workItemId],
  );

  const openConversationPage = React.useCallback(() => {
    if (conversation === null) return;
    void navigate({
      to: "/chat/c/$conversationId",
      params: { conversationId: conversation.id },
    });
  }, [conversation, navigate]);

  const conversationPanel = React.useMemo(
    () =>
      conversation === null
        ? null
        : { conversationId: conversation.id, content: conversation.view },
    [conversation],
  );

  const frame = (body: React.ReactNode, title: string) => (
    <ChatWorkspace
      project={project}
      projectName={item?.projectName ?? project?.name ?? "Work"}
      conversationId={conversation?.id ?? null}
      title={title}
      srHeading={false}
      isServerThread={false}
      workItemId={workItemId}
      issueIdentifier={item === null ? workItemId : issueNumber(item.identifier, item.number)}
      onCreateIssue={null}
      onNewThreadInProject={() => void navigate({ to: "/chat" })}
      attempts={data?.attempts ?? []}
      history={data?.history ?? []}
      conversationPanel={conversationPanel}
    >
      <PanelIntent wanted={search.panel === "conversation"} />
      {body}
    </ChatWorkspace>
  );

  if (projectId === null) {
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center p-8 text-muted-foreground text-sm">
        No project is selected for this issue.
      </div>
    );
  }

  if (item === null || data === null) {
    return frame(
      <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground text-sm">
        <h1 className="dc-sr-only">{workItemId}</h1>
        <p data-testid="issue-page-status">{error ?? (loading ? "Opening issue…" : "Opening issue…")}</p>
        {error === null ? null : (
          <Button size="sm" variant="outline" onClick={reload}>
            Try again
          </Button>
        )}
      </div>,
      workItemId,
    );
  }

  return frame(
    <IssueBody
      item={item}
      data={data}
      moves={moves}
      now={now}
      canWrite={canWrite}
      saving={saving}
      posting={posting}
      conversation={conversation}
      viewerPrincipalId={client.bootstrap.actor.principal_id}
      preferenceChoices={client.bootstrap.preferences}
      onMove={(state) =>
        void mutate(
          (revision) =>
            http.transition({
              projectId,
              itemId: workItemId,
              key: newWorkKey("move"),
              expectedRevision: revision,
              state,
            }),
          "move this issue",
          (issue) => ({ ...issue, state }),
        )
      }
      // `null` is "No priority". The patch says `"none"` rather than leaving
      // the member out, which is how the hub tells a removal from an omission
      // (`tracker.PriorityPatch`).
      onPriority={(name) =>
        void mutate(
          (revision) =>
            http.patchWorkItem({
              projectId,
              itemId: workItemId,
              key: newWorkKey("priority"),
              expectedRevision: revision,
              priority: name === null ? "none" : (priorityValue(name) ?? 2),
            }),
          "change the priority",
          // The optimistic shape. A removal takes the member off rather than
          // setting it to undefined, because the wire shape has no null
          // priority: `omitempty` means absent, and an issue that came back
          // from the hub would not carry the key either.
          (issue) => {
            const { priority: _cleared, ...withoutPriority } = issue;
            return name === null
              ? withoutPriority
              : { ...withoutPriority, priority: priorityValue(name) ?? 2 };
          },
        )
      }
      onLabels={(labels) => {
        void mutate(
          (revision) =>
            http.patchWorkItem({
              projectId,
              itemId: workItemId,
              key: newWorkKey("labels"),
              expectedRevision: revision,
              labels,
            }),
          "change the labels",
          (issue) => ({ ...issue, labels }),
        ).then(() => setOptionsNonce((value) => value + 1));
      }}
      onAssignees={(assignees) =>
        void mutate(
          (revision) =>
            http.patchWorkItem({
              projectId,
              itemId: workItemId,
              key: newWorkKey("assignees"),
              expectedRevision: revision,
              assignees,
            }),
          "change the assignee",
          (issue) => ({ ...issue, assignees }),
        )
      }
      onRelation={(relatedWorkItemId, operation) => {
        void mutate(
          (revision) =>
            http.setDependency({
              projectId,
              itemId: workItemId,
              key: newWorkKey("relation"),
              expectedRevision: revision,
              relatedWorkItemId,
              operation,
            }),
          operation === "add" ? "add the related issue" : "remove the relation",
        ).then(() => setOptionsNonce((value) => value + 1));
      }}
      labelCatalogue={options.labels}
      relatedCandidates={options.candidates}
      members={options.members}
      viewer={viewer}
      // The invitation flow is the organization settings section, which only
      // an owner or an administrator may open (§16: the row is listed either
      // way, with the reason when it cannot be taken).
      onInvite={
        account?.actor.can_manage === true
          ? () => void navigate({ to: "/settings/$section", params: { section: "organization" } })
          : null
      }
      inviteReason={
        account === null
          ? "This hub does not serve the organization's members."
          : "Only an owner or an administrator can invite somebody."
      }
      onComment={postComment}
      onOpenIssue={(id) => void navigate({ to: "/work/i/$workItemId", params: { workItemId: id } })}
      onOpenConversationPage={openConversationPage}
    />,
    `${issueNumber(item.identifier, item.number)} ${item.title}`,
  );
}

interface IssueBodyProps {
  readonly item: WorkItemView;
  readonly data: IssueData;
  readonly moves: readonly string[];
  readonly now: number;
  readonly canWrite: boolean;
  readonly saving: boolean;
  readonly posting: boolean;
  readonly conversation: ConversationBridge | null;
  readonly viewerPrincipalId: string;
  readonly preferenceChoices: PreferenceChoices | undefined;
  readonly onMove: (state: string) => void;
  readonly onPriority: (name: string | null) => void;
  readonly onLabels: (labels: readonly string[]) => void;
  readonly onAssignees: (assignees: readonly string[]) => void;
  readonly onRelation: (relatedWorkItemId: string, operation: "add" | "remove") => void;
  readonly labelCatalogue: readonly NativeLabel[];
  readonly relatedCandidates: readonly RelatedCandidate[];
  readonly members: readonly MemberOption[];
  readonly viewer: MemberOption | null;
  readonly onInvite: (() => void) | null;
  readonly inviteReason: string;
  readonly onComment: (body: string) => Promise<void>;
  readonly onOpenIssue: (workItemId: string) => void;
  readonly onOpenConversationPage: () => void;
}

/**
 * Everything below the header band.
 *
 * A component of its own because it reads the workspace panel, which is only
 * published inside `ChatWorkspace`.
 */
function IssueBody(props: IssueBodyProps): React.ReactElement {
  const panel = useWorkspacePanel();
  const panelOpen = panel?.open === true;
  // The composer's `/shortcuts`. The keybindings are a settings section rather
  // than a dialog here (`app/settings/Settings.tsx`), so opening them is a
  // navigation.
  const navigate = useNavigate();
  const { data, item, conversation } = props;
  const [expanded, setExpanded] = React.useState(false);

  const running = data.attempts.at(-1)?.status === "running" ? data.attempts.at(-1) : undefined;
  const runner = runnerLabel(running);
  const lane = data.project.states.find((state) => state.name === data.issue.state);
  const laneCategory =
    lane === undefined ? "" : lane.terminal ? "completed" : lane.dispatchable ? "unstarted" : "started";

  const rows: readonly ActivityRow[] = React.useMemo(
    () =>
      mergeActivity({
        history: data.history,
        attempts: data.attempts,
        comments: data.comments,
        conversation:
          conversation?.detail === undefined
            ? null
            : {
                questions: conversation.detail.questions,
                messages: conversation.detail.messages,
              },
        viewerPrincipalId: props.viewerPrincipalId,
      }),
    [conversation?.detail, data.attempts, data.comments, data.history, props.viewerPrincipalId],
  );

  const execution = conversation?.execution ?? null;
  const lastAssistant = conversation?.detail?.messages
    .filter((message) => message.role === "assistant" && message.text.trim().length > 0)
    .at(-1);
  const liveSentence =
    lastAssistant !== undefined
      ? lastAssistant.text.trim().slice(0, 200)
      : execution === null
        ? "No runner has worked on this issue yet."
        : executionCopy(execution).sentence;
  const attemptCount = data.attempts.length;
  const liveDetail = [
    runner,
    running?.identity === undefined
      ? null
      : `${running.identity.backend} ${running.identity.model}`,
    attemptCount === 0 ? null : `attempt ${attemptCount}`,
  ]
    .filter((part): part is string => part !== null && part !== "")
    .join(" · ");

  const live =
    conversation === null || panel === null ? null : (
      <LiveRow
        running={running !== undefined}
        sentence={liveSentence}
        detail={liveDetail}
        elapsed={running === undefined ? "" : elapsedLabel(running.started_at, props.now)}
        onInterrupt={props.canWrite ? (conversation.interrupt ?? null) : null}
        onOpenConversation={panel.openConversation}
      />
    );
  // The live row belongs where the work is happening: at the newest attempt
  // while one runs, and at the end of the feed otherwise.
  const liveAt =
    running === undefined ? Number.MAX_SAFE_INTEGER : Date.parse(running.started_at);

  const resources: ResourceRow[] = [];
  // The attempt diff is absent rather than empty. A change version's code is
  // one opaque artifact and the hub serves no file list, no hunks and no
  // per-file counts, so there is no `1 file · +1 −1` to put on a row; the
  // Diff surface in the panel says the same in its own words. It appears here
  // the moment `IssueSurfaces.changedFiles` has something in it.
  if (item.change !== null && panel !== null && panel.pullRequestAvailable) {
    resources.push({
      key: "pull-request",
      icon: "pull-request",
      label:
        item.change.number === null
          ? item.change.title
          : `Pull request #${item.change.number}`,
      detail: item.change.review === "" ? "" : item.change.review,
      onOpen: panel.openPullRequest,
    });
  }
  if (conversation !== null && panel !== null) {
    const turns = conversation.detail?.messages.length ?? 0;
    resources.push({
      key: "conversation",
      icon: "conversation",
      label: runner === null ? "Conversation" : `Conversation with ${runner}`,
      detail: `${turns} ${turns === 1 ? "message" : "messages"}${
        execution !== null && isActive(execution.status) ? " · live" : ""
      }`,
      onOpen: panel.openConversation,
    });
  }

  /**
   * The Related group.
   *
   * One relation kind reaches this page: the dependency. `blockers` is the
   * hydrated set the reader is allowed to see, and the history carries the
   * same relations as `dependency.changed` events — which is where a relation
   * this reader's grant hides the other end of still shows up. The history is
   * read newest-operation-wins, so a relation that was added and then removed
   * does not linger on the list; before, every `related_work_item_id` the
   * history had ever mentioned stayed on it for ever.
   *
   * Removing one is the same endpoint that made it: `POST .../dependencies`
   * with `operation: "remove"` (`changeNativeDependency`). The chat row has
   * no relation behind it — the conversation is linked, not depended on — so
   * it carries no ×.
   */
  const related = React.useMemo(() => {
    const entries: RelatedEntry[] = [];
    const seen = new Set<string>();
    const push = (id: string, label: string, detail: string, category: string) => {
      if (seen.has(id)) return;
      seen.add(id);
      entries.push({
        key: `related:${id}`,
        kind: "issue" as const,
        label,
        detail,
        category,
        onOpen: () => props.onOpenIssue(id),
        onRemove: () => props.onRelation(id, "remove"),
      });
    };
    for (const blocker of data.issue.blockers) {
      push(
        blocker.work_item_id,
        `${blocker.work_item_id} · ${blocker.state}`,
        blocker.state,
        blocker.terminal ? "completed" : "unstarted",
      );
    }
    const operations = new Map<string, string>();
    for (const event of data.history) {
      const id = event.data.related_work_item_id;
      if (id === undefined) continue;
      operations.set(id, event.data.operation ?? "add");
    }
    for (const [id, operation] of operations) {
      if (operation === "remove") continue;
      push(id, id, "", "unstarted");
    }
    if (conversation?.detail !== undefined) {
      entries.push({
        key: "chat",
        kind: "chat" as const,
        label: `Chat: ${conversation.detail.conversation.title}`,
        onOpen: props.onOpenConversationPage,
        onRemove: null,
      });
    }
    return entries;
  }, [conversation?.detail, data.history, data.issue.blockers, props]);

  const states = React.useMemo(
    () =>
      data.project.states.map((state) => ({
        name: state.name,
        category: state.terminal ? "completed" : state.dispatchable ? "unstarted" : "started",
      })),
    [data.project.states],
  );

  // S, P, A, L and R open the pickers, the way Linear's do. They are bound by
  // the properties column itself (`IssueProperties.tsx`), which is mounted
  // exactly when those pickers are reachable — with the right panel open the
  // column yields its width and its letters together.

  const effort = [item.effort, data.attempts.at(-1)?.identity?.model]
    .filter((part): part is string => part !== null && part !== undefined && part !== "")
    .join(" · ");
  const lastAttempt = data.attempts.at(-1);
  const attempts =
    lastAttempt === undefined
      ? "No attempts yet"
      : `${attemptCount} · attempt ${attemptCount} ${lastAttempt.status}`;

  const properties = (
    <IssueProperties
      hideTitle={panelOpen}
      item={item}
      states={states}
      moves={props.moves}
      laneCategory={laneCategory}
      onMove={props.onMove}
      onPriority={props.onPriority}
      onLabels={props.onLabels}
      labelCatalogue={props.labelCatalogue}
      onAssignees={props.onAssignees}
      members={props.members}
      viewer={props.viewer}
      onInvite={props.onInvite}
      inviteReason={props.inviteReason}
      canWrite={props.canWrite}
      saving={props.saving}
      runner={runner}
      effort={effort === "" ? "No effort set" : effort}
      attempts={attempts}
      related={related}
      relatedCandidates={props.relatedCandidates}
      onAddRelated={(id) => props.onRelation(id, "add")}
      onOpenPullRequest={
        panel !== null && panel.pullRequestAvailable ? panel.openPullRequest : null
      }
    />
  );

  const truncated = item.body.length > BODY_CLAMP && !expanded;

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-y-auto">
        <div
          className={`mx-auto flex w-full flex-col gap-5 px-5 pt-7 pb-10 sm:px-10 ${
            panelOpen ? "max-w-[640px]" : "max-w-[760px]"
          }`}
        >
          <header>
            <p className="font-mono text-muted-foreground text-xs" data-testid="issue-identifier">
              {item.projectName} {issueNumber(item.identifier, item.number)}
            </p>
            <h1 className="mt-1.5 text-balance font-semibold text-2xl leading-tight tracking-[-0.01em]">
              {item.title}
            </h1>
          </header>

          {/* With the panel open the sidebar yields its width, so the facts it
              carries stay reachable here (decisions.md §19.3). */}
          {panelOpen ? (
            <Collapsible className="rounded-[var(--radius)] border border-border bg-card">
              <CollapsibleTrigger
                data-testid="properties-disclosure"
                className="flex w-full items-center gap-1.5 rounded-[var(--radius)] px-3.5 py-2.5 text-left text-[13px] text-muted-foreground outline-none ring-ring hover:text-foreground focus-visible:ring-2"
              >
                <ChevronDownIcon className="size-3" />
                Properties
              </CollapsibleTrigger>
              <CollapsiblePanel>
                <div className="px-4 pt-1 pb-4">{properties}</div>
              </CollapsiblePanel>
            </Collapsible>
          ) : null}

          <div
            className="rounded-[var(--radius)] border border-border bg-card px-4 py-3.5"
            data-testid="issue-body"
          >
            <div className={truncated ? "relative max-h-64 overflow-hidden" : undefined}>
              <Markdown source={truncated ? item.body.slice(0, BODY_CLAMP) : item.body} />
              {truncated ? (
                <span className="pointer-events-none absolute inset-x-0 bottom-0 h-10 bg-gradient-to-t from-card to-transparent" />
              ) : null}
            </div>
            {/* The mockup's own line under the description. It is only drawn
                where it is true: an issue created some other way did not come
                from a chat and does not claim to have. */}
            {conversation === null ? null : (
              <p className="mt-2.5 text-muted-foreground text-xs">
                Created from a chat · history shared with the project
              </p>
            )}
            {item.body.length > BODY_CLAMP ? (
              <button
                type="button"
                data-testid="show-full-issue"
                onClick={() => setExpanded((value) => !value)}
                className="mt-1.5 block w-full cursor-pointer rounded-sm text-right text-muted-foreground text-sm outline-none ring-ring hover:text-foreground focus-visible:ring-2"
              >
                {expanded ? "Show less" : "Show full issue"}
              </button>
            ) : null}
          </div>

          {/* §19.6: sub-issue creation is not in this slice, and §16 says a
              feature stays present with its reason rather than disappearing. */}
          <Tooltip>
            <TooltipTrigger render={<span className="inline-flex w-fit" tabIndex={0} role="note" />}>
              <Button
                size="xs"
                variant="ghost"
                disabled
                aria-disabled="true"
                data-testid="add-sub-issues"
                className="text-muted-foreground"
              >
                <PlusIcon className="size-3" />
                Add sub-issues
              </Button>
            </TooltipTrigger>
            <TooltipPopup side="top">
              Sub-issues are not in this milestone: the hub has no parent-child relation yet.
            </TooltipPopup>
          </Tooltip>

          <IssueResources rows={resources} />

          <ActivityFeed
            rows={rows}
            live={live}
            liveAt={liveAt}
            onReply={props.canWrite ? (body) => void props.onComment(body) : null}
            posting={props.posting}
          />

          {/* Comments only (§19.5, Michael's review of September 12). The
              runner is steered from the conversation surface the Activity
              feed's live row opens, not from a second mode on this card. */}
          <IssueComposer
            canWrite={props.canWrite}
            onComment={props.onComment}
            onOpenShortcuts={() =>
              void navigate({ to: "/settings/$section", params: { section: "keybindings" } })
            }
          />
        </div>
      </div>

      {panelOpen ? null : (
        <aside
          className="hidden w-[300px] shrink-0 overflow-y-auto px-6 py-8 lg:block"
          aria-label="Properties"
        >
          {properties}
        </aside>
      )}
    </div>
  );
}
