import { dependencyLabel } from "./lib/dependencies.ts";
import { ReporterIssue } from "../reporter/ReporterIssue.tsx";
import { DependencyPanels } from "./components/DependencyDetails.tsx";
import { useAtomSet, useAtomValue } from "@effect/atom-react";
import * as Option from "effect/Option";
import { AsyncResult } from "effect/unstable/reactivity";
import { ChevronDownIcon, PlusIcon } from "lucide-react";
import React from "react";
import { useNavigate, useParams, useRouteContext, useSearch } from "@tanstack/react-router";

import { Badge } from "../../components/ui/badge.tsx";
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
  IssuePickerPage,
  NativeIssue,
  NativeLabel,
  ReportContext,
} from "../../contracts/work.ts";
import { priorityValue } from "../../contracts/work.ts";
import type { ConversationDetail } from "../../runtime/state/conversationState.ts";
import { isStreaming } from "../../runtime/state/conversationState.ts";
import { useShell } from "../App.tsx";
import { ConversationView } from "../ChatRoutes.tsx";
import { newCommandKey, useClient } from "../client.ts";
import { usePageTitle } from "../pageTitle.ts";
import { ChatWorkspace, useWorkspacePanel } from "../components/ChatWorkspace.tsx";
import { Markdown } from "../components/Markdown.tsx";
import { AttachmentEditor } from "./components/AttachmentEditor.tsx";
import { canInterrupt, executionCopy, expectedOwner, isActive } from "../lib/execution.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useActivityCitation } from "./lib/useActivityCitation.ts";
import { IssueAskPanel, useIssueAsk } from "./IssueAsk.tsx";
import { ActivityFeed, LiveRow } from "./components/ActivityFeed.tsx";
import { IssueComposer } from "./components/IssueComposer.tsx";
import {
  IssueProperties,
  type MemberOption,
  type RelatedCandidate,
  type RelatedEntry,
} from "./components/IssueProperties.tsx";
import { IssueResources, type ResourceRow } from "./components/IssueResources.tsx";
import { IssueDiagnostics, IssueDetailTabs } from "./components/IssueDiagnostics.tsx";
import { SourceRecoveryControls } from "./components/SourceRecoveryControls.tsx";
import { VerificationControls, type VerificationResult } from "./components/VerificationControls.tsx";
import { mergeActivity, runnerLabel, type ActivityRow } from "./lib/activity.ts";
import { toChangeView, toWorkItemView, transitionsFrom } from "./lib/fromWire.ts";
import { elapsedLabel, issueNumber } from "./lib/format.ts";
import type { WorkItemView } from "./lib/model.ts";
import { useRunnerNames } from "./lib/runnerNames.ts";
import { useNow, useWorkHttp } from "./lib/useWork.ts";
import { newWorkKey, WorkApiError, type WorkHttp } from "./lib/workHttp.ts";
import { invalidateQueries, scopeOf, setQueryData, useQuery } from "../lib/queryCache.ts";
import { issueKey, readIssue, readOlderIssue, selectChangeId, type IssueData } from "./lib/issueQuery.ts";

export { selectChangeId };

/** Keep every linked change visible, including one whose detail read failed. */
export function changeResourceRows(
  changes: IssueData["changes"],
  onOpen: (changeId: string) => void,
): ResourceRow[] {
  return changes.map(({ record, detail, pending, deferred }) => ({
    key: record.change_id,
    icon: "diff" as const,
    label: record.title,
    detail: detail?.summary.status.replaceAll("_", " ") ?? (pending === true || deferred === true ? "" : "unavailable"),
    onOpen: () => onOpen(record.change_id),
  }));
}

export function useIssue(
  http: WorkHttp,
  workItemId: string,
  requestedChange: string | null = null,
): {
  data: IssueData | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
  loadOlder: () => void;
  /** Applies an optimistic or answered issue without a round trip. */
  apply: React.Dispatch<React.SetStateAction<IssueData | null>>;
} {
  const key = issueKey(http, workItemId, requestedChange);
  const query = useQuery(key, readIssue(http, workItemId, requestedChange));
  const data = query.data ?? null;
  const current = React.useRef(data);
  current.current = data;
  const refresh = query.refresh;
  const reload = React.useCallback(() => void refresh(), [refresh]);
  const latestKey = React.useRef(key);
  latestKey.current = key;
  const apply = React.useCallback<React.Dispatch<React.SetStateAction<IssueData | null>>>((action) => {
    setQueryData<IssueData | null>(latestKey.current, (previous) =>
      typeof action === "function" ? action(previous ?? null) : action);
  }, []);

  const paging = React.useRef(false);
  const loadOlder = React.useCallback(() => {
    const base = current.current;
    if (paging.current || base === null || base.issue.work_item_id !== workItemId || !(base.attemptsCursor || base.historyCursor || base.commentsCursor)) return;
    paging.current = true;
    const pageKey = latestKey.current;
    void readOlderIssue(http, base).then((next) => {
      setQueryData<IssueData | null>(pageKey, (previous) => {
        if (previous?.issue !== base.issue || previous.history !== base.history || previous.attempts !== base.attempts || previous.comments !== base.comments) return previous ?? null;
        return { ...previous, attempts: next.attempts, history: next.history, comments: next.comments, attemptsCursor: next.attemptsCursor, historyCursor: next.historyCursor, commentsCursor: next.commentsCursor };
      });
    }).catch(() => undefined).finally(() => { paging.current = false; });
  }, [http, workItemId]);

  const projectId = data?.issue.work_item_id === workItemId ? data.issue.project_id : null;
  React.useEffect(() => {
    if (projectId === null || typeof globalThis.EventSource !== "function") return;
    const source = new globalThis.EventSource(http.eventsUrl(projectId, undefined, workItemId), { withCredentials: true });
    let previous: bigint | null = null;
    const onActivity = (event: MessageEvent<string>) => {
      if (!/^\d+$/.test(event.data)) return;
      const next = BigInt(event.data);
      const seen = previous;
      previous = next;
      const loaded = current.current?.history.at(-1)?.aggregate_sequence ?? "0";
      if (next > (seen ?? BigInt(loaded))) invalidateQueries(["issue", workItemId]);
    };
    const onError = () => {
      if (source.readyState === 2) reload();
    };
    source.addEventListener("activity", onActivity as EventListener);
    source.addEventListener("error", onError);
    return () => {
      source.removeEventListener("activity", onActivity as EventListener);
      source.removeEventListener("error", onError);
      source.close();
    };
  }, [http, projectId, workItemId, reload]);

  const error = query.error === null || query.error === undefined ? null : query.error instanceof Error ? query.error.message : String(query.error);
  return {
    data: data?.issue.work_item_id === workItemId ? data : null,
    error,
    loading: query.pending,
    reload,
    loadOlder,
    apply,
  };
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
function toCandidates(page: IssuePickerPage): readonly RelatedCandidate[] {
  return page.items.map((issue) => ({
    id: issue.work_item_id,
    label: issue.title,
    detail: issue.state,
    category: issue.terminal ? "completed" : "unstarted",
  }));
}

function useIssueOptions(
  http: WorkHttp,
  members: () => Promise<readonly MemberOption[]>,
  projectId: string | null,
  nonce: number,
): {
  labels: readonly NativeLabel[];
  candidates: readonly RelatedCandidate[];
  members: readonly MemberOption[];
  search: (query: string) => void;
} {
  const scope = scopeOf(http);
  const base = useQuery(
    projectId === null ? null : ["issue-options", projectId, scope],
    async () => {
      const [labels, candidates, roster] = await Promise.all([
        http.listLabels(projectId!).then((list) => list.items).catch(() => [] as readonly NativeLabel[]),
        http.listIssuePicker({ projectId: projectId!, limit: 100 }).then(toCandidates).catch(() => [] as readonly RelatedCandidate[]),
        members().catch(() => [] as readonly MemberOption[]),
      ]);
      return { labels, candidates, members: roster };
    },
    { staleTime: 30_000 },
  );
  const refreshOptions = base.refresh;
  React.useEffect(() => {
    if (nonce > 0) void refreshOptions();
  }, [nonce, refreshOptions]);
  const [query, setQuery] = React.useState("");
  const [debounced, setDebounced] = React.useState("");
  React.useEffect(() => {
    const timer = setTimeout(() => setDebounced(query.trim()), 200);
    return () => clearTimeout(timer);
  }, [query]);
  const searched = useQuery(
    projectId === null || debounced === "" ? null : ["issue-picker", projectId, scope, debounced, nonce],
    (signal) => http.listIssuePicker({ projectId: projectId!, q: debounced, limit: 100, signal }).then(toCandidates),
    { staleTime: 30_000, keepPreviousIn: JSON.stringify([scope, projectId]) },
  );
  const initial = base.data?.candidates ?? [];
  const candidates = React.useMemo(() => {
    if (query.trim() === "" || searched.data === undefined) return initial;
    const seen = new Set(searched.data.map((candidate) => candidate.id));
    return [...searched.data, ...initial.filter((candidate) => !seen.has(candidate.id))];
  }, [initial, query, searched.data]);
  return { labels: base.data?.labels ?? [], candidates, members: base.data?.members ?? [], search: setQuery };
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
  const account = useAccountBootstrap();
  return account?.actor.role === "reporter" ? <ReporterIssue /> : <FullIssuePage />;
}

function FullIssuePage(): React.ReactElement {
  const params = useParams({ strict: false }) as { workItemId?: string };
 const context = useRouteContext({ strict: false }) as { resolvedWorkItemId?: string };
 const workItemId = context.resolvedWorkItemId ?? params.workItemId ?? "";
  const shell = useShell();
  const http = useWorkHttp();
  const search = useSearch({ strict: false }) as { change?: string };
  const issueState = useIssue(http, workItemId, search.change ?? null);
  const projectId = issueState.data?.issue.project_id ?? null;
  const indexed = shell.conversations.find(
    (conversation) => conversation.work_item_id === workItemId && conversation.project_id === projectId,
  );
  const [resolved, setResolved] = React.useState<Conversation | undefined>();
  React.useEffect(() => {
    let cancelled = false;
    setResolved(undefined);
    if (projectId !== null && indexed === undefined) {
      void http
        .getWorkItemConversation(projectId, workItemId, issueState.data?.issue.archive_summary !== undefined)
        .then((snapshot) => {
          if (!cancelled) setResolved(snapshot.conversation);
        })
        .catch((error: unknown) => {
          if (!cancelled && !(error instanceof WorkApiError && error.code === "not_found")) {
            toastManager.add({
              title: "Worker conversation unavailable",
              description: String(error),
              type: "error",
            });
          }
        });
    }
    return () => {
      cancelled = true;
    };
  }, [http, projectId, workItemId, indexed]);
  const linked =
    indexed ??
    (resolved !== undefined &&
    resolved.work_item_id === workItemId &&
    resolved.project_id === projectId
      ? resolved
      : undefined);
  if (linked === undefined) {
    return <IssueSurface key={workItemId} workItemId={workItemId} issueState={issueState} conversation={null} />;
  }
  if (issueState.data?.issue.archive_summary !== undefined) return <ArchivedLinkedIssue key={linked.id} workItemId={workItemId} issueState={issueState} conversation={linked} />;
  return <LinkedIssue key={linked.id} workItemId={workItemId} issueState={issueState} conversation={linked} />;
}

function ArchivedLinkedIssue({ workItemId, issueState, conversation }: {
  readonly workItemId: string;
  readonly issueState: ReturnType<typeof useIssue>;
  readonly conversation: Conversation;
}): React.ReactElement {
  const client = useClient();
  const [detail, setDetail] = React.useState<ConversationDetail>();
  const send = useAtomSet(client.sendMessage, { mode: "promise" });
  const setPreferences = useAtomSet(client.setPreferences, { mode: "promise" });
  const bridge: ConversationBridge = {
    id: conversation.id, projectId: conversation.project_id, detail,
    execution: conversation.execution, streaming: false, preferences: turnPreferences(conversation),
    setPreferences: (preferences) => { void setPreferences({ projectId: conversation.project_id, conversationId: conversation.id, preferences }); },
    sendToRunner: async (text) => { await send({ projectId: conversation.project_id, conversationId: conversation.id, key: newCommandKey(), text, expected: expectedOwner(conversation.execution) }); },
    interrupt: null,
    view: <ArchivedConversation conversation={conversation} onDetail={setDetail} />,
  };
  return <IssueSurface workItemId={workItemId} issueState={issueState} conversation={bridge} />;
}

function ArchivedConversation({ conversation, onDetail }: {
  readonly conversation: Conversation;
  readonly onDetail: (detail: ConversationDetail | undefined) => void;
}): React.ReactElement {
  const client = useClient();
  const state = useAtomValue(client.conversations.stateAtom({
    environmentId: HUB_ENVIRONMENT_ID, projectId: conversation.project_id, conversationId: conversation.id,
  }));
  const value = Option.getOrUndefined(AsyncResult.value(state));
  const detail = value === undefined ? undefined : Option.getOrUndefined(value.data);
  React.useEffect(() => { onDetail(detail); }, [detail, onDetail]);
  return <ConversationView conversationId={conversation.id} projectId={conversation.project_id} header={false} />;
}

/** The issue page with a conversation behind it. Subscribes to its atoms. */
function LinkedIssue({
  workItemId,
  issueState,
  conversation,
}: {
  readonly workItemId: string;
  readonly issueState: ReturnType<typeof useIssue>;
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
    <IssueSurface workItemId={workItemId} issueState={issueState} conversation={bridge} />
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
  issueState,
  conversation,
}: {
  readonly workItemId: string;
  readonly issueState: ReturnType<typeof useIssue>;
  readonly conversation: ConversationBridge | null;
}): React.ReactElement {
  const navigate = useNavigate();
  const client = useClient();
  const http = useWorkHttp();
  const now = useNow();
  const search = useSearch({ strict: false }) as { panel?: string; change?: string };

  const { data, error, loading, reload, apply } = issueState;
  const projectId = data?.issue.project_id ?? null;
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
  const role = account?.actor.role;
  const canVerify = data !== null && data.project.profile === "native" && data.issue.verification?.state === "pending" &&
    (data.issue.actor.kind === "human" && data.issue.actor.principal_id === client.bootstrap.actor.principal_id ||
      canWrite && (role === "member" || role === "admin" || role === "owner"));
  const ask = useIssueAsk(projectId, workItemId);

  const item = React.useMemo<WorkItemView | null>(() => {
    if (data === null) return null;
    const change = data.change === null ? null : toChangeView(data.change.change, data.change);
    const view = toWorkItemView(data.issue, data.project.name, {
      attempts: data.attempts,
      change,
      conversationId: conversation?.id ?? null,
    });
    return data.attemptsCursor && view.attempt !== null ? { ...view, attempt: { ...view.attempt, attemptNumber: null } } : view;
  }, [conversation?.id, data]);
  usePageTitle(
    item === null ? "Issue" : `Issue ${issueNumber(item.identifier, item.number)}: ${item.title}`,
    item?.projectName ?? project?.name,
  );

  const moves = React.useMemo(
    () => (data === null || data.issue.archived ? [] : transitionsFrom(data.project, data.issue.state)),
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
        const activeWork = what === "archive this issue" && cause instanceof WorkApiError && cause.code === "lease_conflict";
        const conflict = cause instanceof WorkApiError && cause.conflict && !activeWork;
        toastManager.add({
          type: conflict ? "warning" : "error",
          title: conflict ? "This issue changed while you were reading it" : `Could not ${what}`,
          description: activeWork
            ? "Finish or stop active work and release its claim before archiving."
            : conflict
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
        throw cause;
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
      projectBreadcrumbAction={projectId === null ? undefined : {
        label: `Open ${item?.projectName ?? project?.name ?? "project"} in Work`,
        onClick: () => void navigate({ to: "/work/p/$projectId", params: { projectId } }),
      }}
      attempts={data?.attempts ?? []}
      attemptsPartial={Boolean(data?.attemptsCursor)}
      history={data?.history ?? []}
      archived={data?.issue.archive_summary !== undefined}
      askPanel={projectId === null ? null : <IssueAskPanel ask={ask} projectId={projectId} identifier={item === null ? workItemId : issueNumber(item.identifier, item.number)} title={item?.title ?? ""} canWrite={canWrite} />}
      conversationPanel={conversationPanel}
    >
      <PanelIntent wanted={search.panel === "conversation"} />
      {body}
    </ChatWorkspace>
  );

  if (item === null || data === null || projectId === null) {
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
      onLoadOlder={issueState.loadOlder}
      http={http}
      canManageRunners={project?.can_manage_runners === true}
      onTransferred={reload}
      onBodySave={async (body) => {
        const updated = await http.patchWorkItem({ projectId, itemId: workItemId, key: newWorkKey("body"), expectedRevision: data.issue.revision, body });
        apply((current) => current === null ? current : { ...current, issue: updated });
        reload();
      }}
      item={item}
      data={data}
      moves={moves}
      now={now}
      onOpenChange={(changeId) =>
        void navigate({
          to: "/work/i/$workItemId/changes/$changeId",
          params: { workItemId, changeId },
          ...(projectId === null ? {} : { search: { project: projectId } }),
        })
      }
      canWrite={canWrite}
      saving={saving}
      posting={posting}
      conversation={conversation}
      viewerPrincipalId={client.bootstrap.actor.principal_id}
      preferenceChoices={client.bootstrap.preferences}
      onArchive={() => void mutate(
        (revision) => http.setArchived({ projectId, itemId: workItemId,
          key: newWorkKey("archive"), expectedRevision: revision, archived: !item.archived }),
        item.archived ? "restore this issue" : "archive this issue",
      )}
      canVerify={canVerify}
      onVerify={(result, comment) =>
        void mutate(
          (revision) =>
            http.verifyFix({
              projectId,
              itemId: workItemId,
              key: newWorkKey("verify"),
              expectedRevision: revision,
              result,
              ...(comment === undefined ? {} : { comment }),
            }),
          "record the verification",
        )
      }
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
      onRelatedQuery={options.search}
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
      onChangeComment={async (comment, body, deleted) => {
        if (projectId === null || comment.revision === undefined) return;
        try {
          const updated = await http.updateComment({ projectId, itemId: workItemId, commentId: comment.id, key: newWorkKey("edit-comment"), expectedRevision: comment.revision, body, deleted });
          apply((current) => current === null ? current : { ...current, comments: current.comments.map((entry) => entry.comment_id === updated.comment_id ? updated : entry) });
          reload();
        } catch (cause) {
          if (cause instanceof WorkApiError && cause.conflict) reload();
          throw cause;
        }
      }}
      onOpenIssue={(id) => void navigate({ to: "/work/i/$workItemId", params: { workItemId: id } })}
      onOpenConversationPage={openConversationPage}
    />,
    `${issueNumber(item.identifier, item.number)} ${item.title}`,
  );
}

interface IssueBodyProps {
  readonly onLoadOlder: () => void;
  readonly http: WorkHttp;
  readonly canManageRunners: boolean;
  readonly onTransferred: () => void;
  readonly onBodySave: (body: string) => Promise<void>;
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
  readonly onArchive: () => void;
  readonly canVerify: boolean;
  readonly onVerify: (result: VerificationResult, comment?: string) => void;
  readonly onMove: (state: string) => void;
  readonly onPriority: (name: string | null) => void;
  readonly onLabels: (labels: readonly string[]) => void;
  readonly onAssignees: (assignees: readonly string[]) => void;
  readonly onRelation: (relatedWorkItemId: string, operation: "add" | "remove") => void;
  readonly labelCatalogue: readonly NativeLabel[];
  readonly relatedCandidates: readonly RelatedCandidate[];
  readonly onRelatedQuery?: (query: string) => void;
  readonly members: readonly MemberOption[];
  readonly viewer: MemberOption | null;
  readonly onInvite: (() => void) | null;
  readonly inviteReason: string;
  readonly onComment: (body: string) => Promise<void>;
  readonly onChangeComment: NonNullable<React.ComponentProps<typeof ActivityFeed>["onChangeComment"]>;
  readonly onOpenIssue: (workItemId: string) => void;
  readonly onOpenConversationPage: () => void;
  /** Opens any Change Request linked to this issue. */
  readonly onOpenChange: (changeId: string) => void;
}

const REPORT_CONTEXT_FIELDS = [
  ["url", "URL"],
  ["app_build", "App build"],
  ["browser", "Browser"],
  ["os", "OS"],
  ["viewport", "Viewport"],
  ["captured_at", "Captured"],
] as const satisfies readonly (readonly [keyof ReportContext, string])[];

function ReportContextBlock({ context }: { readonly context: ReportContext }): React.ReactElement | null {
  const rows = REPORT_CONTEXT_FIELDS.flatMap(([key, label]) => context[key] === undefined || context[key] === "" ? [] : [{ label, value: context[key] }]);
  if (rows.length === 0) return null;
  return (
    <section className="rounded-[var(--radius)] border border-border bg-card px-4 py-3" data-testid="issue-report-context" aria-labelledby="issue-report-context-title">
      <h2 id="issue-report-context-title" className="text-[13px] text-muted-foreground">Report context</h2>
      <dl className="mt-1.5 grid grid-cols-[max-content_minmax(0,1fr)] gap-x-4 gap-y-1 text-[13px]">
        {rows.map((row) => (
          <React.Fragment key={row.label}>
            <dt className="text-muted-foreground">{row.label}</dt>
            <dd className="min-w-0 break-words">{row.value}</dd>
          </React.Fragment>
        ))}
      </dl>
    </section>
  );
}

/**
 * Everything below the header band.
 *
 * A component of its own because it reads the workspace panel, which is only
 * published inside `ChatWorkspace`.
 */
function IssueBody(props: IssueBodyProps): React.ReactElement {
  const now = useNow();
  const [editingBody, setEditingBody] = React.useState(false);
  const [bodyDraft, setBodyDraft] = React.useState("");
  const [bodyUploading, setBodyUploading] = React.useState(false);
  const [bodySaving, setBodySaving] = React.useState(false);
  const [bodyError, setBodyError] = React.useState<string | null>(null);
  const panel = useWorkspacePanel();
  const panelOpen = panel?.open === true;
  // The composer's `/shortcuts`. The keybindings are a settings section rather
  // than a dialog here (`app/settings/Settings.tsx`), so opening them is a
  // navigation.
  const navigate = useNavigate();
  const { data, item, conversation } = props;
  const [expanded, setExpanded] = React.useState(false);
  const highlightedId = useActivityCitation(item.id);
  const lastScrollTop = React.useRef(0);
  const intentConsumed = React.useRef(false);
  const loadOlderAtEnd = (element: HTMLDivElement) => {
    if (intentConsumed.current || element.scrollHeight - element.clientHeight - element.scrollTop > 32) return;
    intentConsumed.current = true;
    props.onLoadOlder();
  };

  const runnerNames = useRunnerNames();
  const running = data.attempts.at(-1)?.status === "running" ? data.attempts.at(-1) : undefined;
  const runner = runnerLabel(running, runnerNames);
  const lane = data.project.states.find((state) => state.name === data.issue.state);
  const laneCategory =
    lane === undefined ? "" : lane.terminal ? "completed" : lane.dispatchable ? "unstarted" : "started";

  const rows: readonly ActivityRow[] = React.useMemo(
    () =>
      mergeActivity({
        history: data.history,
        attemptsPartial: Boolean(data.attemptsCursor),
        attempts: data.attempts,
        comments: data.comments,
        runnerNames,
        conversation:
          conversation?.detail === undefined
            ? null
            : {
                questions: conversation.detail.questions,
                messages: conversation.detail.messages,
              },
        viewerPrincipalId: props.viewerPrincipalId,
      }),
    [
      runnerNames,
      conversation?.detail,
      data.attempts,
      data.attemptsCursor,
      data.comments,
      data.history,
      props.viewerPrincipalId,
    ],
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
    attemptCount === 0 ? null : data.attemptsCursor ? `${attemptCount} loaded attempts` : `attempt ${attemptCount}`,
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
  // The Change Request page: the round under review, its diff and the
  // decisions. Every issue with a change has one, mirrored to a host or not.
  resources.push(...changeResourceRows(data.changes, props.onOpenChange));
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
    const push = (id: string, label: string, detail: string, category: string, removable = true) => {
      if (seen.has(id)) return;
      seen.add(id);
      entries.push({
        key: `related:${id}`,
        kind: "issue" as const,
        label,
        detail,
        category,
        onOpen: () => props.onOpenIssue(id),
        onRemove: removable ? () => props.onRelation(id, "remove") : null,
      });
    };
    for (const blocker of data.issue.blockers) {
      push(
        blocker.work_item_id,
        dependencyLabel(blocker),
        blocker.state,
        blocker.terminal ? "completed" : blocker.started_at || blocker.dispatchable === false ? "started" : "unstarted",
      );
    }
    for (const link of [...(data.issue.dependents ?? []), ...(data.issue.related ?? [])]) {
      push(link.work_item_id, dependencyLabel(link), link.state, link.terminal ? "completed" : link.started_at || link.dispatchable === false ? "started" : "unstarted", false);
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
  }, [conversation?.detail, data.history, data.issue.blockers, data.issue.dependents, data.issue.related, props]);

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

  const lastAttempt = data.attempts.at(-1);
  const attempts =
    lastAttempt === undefined
      ? "No attempts yet"
      : data.attemptsCursor ? `${attemptCount} loaded · latest ${lastAttempt.status}` : `${attemptCount} · attempt ${attemptCount} ${lastAttempt.status}`;

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
      attempts={attempts}
      related={related}
      relatedCandidates={props.relatedCandidates}
      onRelatedQuery={props.onRelatedQuery}
      onAddRelated={(id) => props.onRelation(id, "add")}
      onOpenPullRequest={
        panel !== null && panel.pullRequestAvailable ? panel.openPullRequest : null
      }
      onOpenChangeRequest={item.change === null ? null : () => props.onOpenChange(item.change!.id)}
    />
  );

  const truncated = item.body.length > BODY_CLAMP && !expanded;

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-y-auto" onScroll={(event) => {
        const element = event.currentTarget;
        const downward = element.scrollTop > lastScrollTop.current;
        if (element.scrollTop < lastScrollTop.current) intentConsumed.current = false;
        lastScrollTop.current = element.scrollTop;
        if (downward) loadOlderAtEnd(element);
      }} onWheel={(event) => {
        if (event.deltaY <= 0) return;
        intentConsumed.current = false;
        loadOlderAtEnd(event.currentTarget);
      }} onKeyDown={(event) => {
        if (event.repeat || !["End", "PageDown", "ArrowDown", " "].includes(event.key)) return;
        if (event.target instanceof HTMLElement && (event.target.isContentEditable || event.target.closest("input, textarea, button, select, [role='textbox']"))) return;
        intentConsumed.current = false;
        loadOlderAtEnd(event.currentTarget);
      }} onTouchStart={() => {
        intentConsumed.current = false;
      }} onTouchMove={(event) => {
        loadOlderAtEnd(event.currentTarget);
      }} onPointerDown={(event) => {
        intentConsumed.current = false;
        if (event.pointerType !== "touch") loadOlderAtEnd(event.currentTarget);
      }}>
        <div
          className={`mx-auto flex w-full flex-col gap-5 px-5 pt-7 pb-10 sm:px-10 ${
            panelOpen ? "max-w-[640px]" : "max-w-[760px]"
          }`}
        >
          <header>
            <p className="font-mono text-muted-foreground text-xs" data-testid="issue-identifier">
              {item.projectName} {issueNumber(item.identifier, item.number)}{item.archived ? " · Archived" : ""}
            </p>
            <div className="mt-1.5 flex flex-wrap items-center gap-2">
              <h1 className="text-balance font-semibold text-2xl leading-tight tracking-[-0.01em]">
                {item.title}
              </h1>
              {item.type === undefined ? null : (
                <Badge variant="secondary" size="sm">{item.type === "bug" ? "Bug" : "Change request"}</Badge>
              )}
            </div>
            {props.canVerify ? (
              <VerificationControls key={data.issue.revision} projectId={data.project.project_id} saving={props.saving} onVerify={props.onVerify} />
            ) : null}
            {props.canWrite && data.project.profile === "native" ? (
              <Button size="sm" variant="outline" className="mt-3" data-testid="issue-archive"
                disabled={props.saving || data.attempts.some((attempt) => attempt.status === "running")}
                title={data.attempts.some((attempt) => attempt.status === "running") ? "Finish or stop active work before archiving" : undefined}
                onClick={props.onArchive}>
                {item.archived ? "Restore issue" : "Archive issue"}
              </Button>
            ) : null}
          </header>

          {data.project.profile === "native" ? <SourceRecoveryControls key={data.issue.work_item_id} http={props.http} projectId={data.issue.project_id} itemId={data.issue.work_item_id} revision={data.issue.revision} canManage={props.canWrite && props.canManageRunners} onTransferred={props.onTransferred} /> : null}

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

          <DependencyPanels item={item} queued={data.project.states.some((state) => state.name === item.state && state.dispatchable && !state.terminal) && !item.attempt?.running} />
          <div
            className="rounded-[var(--radius)] border border-border bg-card px-4 py-3.5"
            data-testid="issue-body"
          >
			{data.issue.linked_source === undefined ? null : (
				<div className="mb-3 text-sm" data-testid="issue-linked-source">
					<a className="text-primary underline" href={data.issue.linked_source.url} target="_blank" rel="noopener noreferrer">GitHub source</a>
					<p className="text-xs text-muted-foreground">{data.issue.linked_source.status === "complete" ? "Source intake completed" : "Awaiting source intake before the first run"}</p>
						{data.issue.linked_source.snapshot === undefined ? null : (
						<details className="mt-2">
							<summary>Original source context</summary>
							<p className="text-xs text-muted-foreground">Observed {data.issue.linked_source.snapshot.provenance.observed_at}</p>
							<p>{data.issue.linked_source.snapshot.title}</p>
                            <Markdown projectId={data.project.project_id} source={data.issue.linked_source.snapshot.body} />
						</details>
					)}
				</div>
			)}
            {editingBody ? <form onSubmit={(event) => {
              event.preventDefault();
              if (bodyUploading || bodySaving) return;
              setBodySaving(true);
              setBodyError(null);
              void props.onBodySave(bodyDraft).then(() => setEditingBody(false), (cause: unknown) => setBodyError(cause instanceof Error ? cause.message : String(cause))).finally(() => setBodySaving(false));
            }}>
              <AttachmentEditor projectId={data.project.project_id} value={bodyDraft} onChange={setBodyDraft}
                onUploadingChange={setBodyUploading} disabled={bodySaving} aria-label="Issue body" />
              {bodyError === null ? null : <p role="alert" className="text-sm text-destructive">{bodyError}</p>}
              <div className="mt-2 flex gap-2">
                <Button type="submit" size="xs" disabled={bodyUploading || bodySaving}>Save body</Button>
                <Button type="button" size="xs" variant="ghost" disabled={bodySaving} onClick={() => setEditingBody(false)}>Cancel</Button>
              </div>
            </form> : <div className={truncated ? "relative max-h-64 overflow-hidden" : undefined}>
              <Markdown projectId={data.project.project_id} source={truncated ? item.body.slice(0, BODY_CLAMP) : item.body} />
              {truncated ? (
                <span className="pointer-events-none absolute inset-x-0 bottom-0 h-10 bg-gradient-to-t from-card to-transparent" />
              ) : null}
            </div>}
            {props.canWrite && !editingBody && data.project.profile === "native" ? <Button type="button" size="xs" variant="ghost" onClick={() => { setBodyDraft(item.body); setBodyError(null); setEditingBody(true); }}>Edit body</Button> : null}
            {conversation?.detail?.conversation.visibility !== "shared" ? null : (
              <p className="mt-2.5 text-muted-foreground text-xs">
                Conversation history shared with the project
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

          {data.issue.report_context === undefined ? null : <ReportContextBlock context={data.issue.report_context} />}

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

          <IssueDetailTabs native={data.project.profile === "native"} diagnostics={<IssueDiagnostics issue={data.issue} attempts={data.attempts} history={data.history} now={now} attemptsAvailable={data.attemptsAvailable} historyAvailable={data.historyAvailable} historyPartial={Boolean(data.historyCursor) || data.historyCompacted === true} attemptsPartial={Boolean(data.attemptsCursor)} />}>
          <ActivityFeed
            projectId={data.project.project_id}
            highlightedId={highlightedId}
            rows={rows}
            live={live}
            liveAt={liveAt}
            onReply={props.canWrite ? props.onComment : null}
            onChangeComment={props.canWrite && data.project.profile === "native" ? props.onChangeComment : null}
            posting={props.posting}
          />

          {/* Comments only (§19.5, Michael's review of September 12). The
              runner is steered from the conversation surface the Activity
              feed's live row opens, not from a second mode on this card. */}
          <IssueComposer
            projectId={data.project.project_id}
            canWrite={props.canWrite}
            onComment={props.onComment}
            onOpenShortcuts={() =>
              void navigate({ to: "/settings/$section", params: { section: "keybindings" } })
            }
          />
          </IssueDetailTabs>
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
