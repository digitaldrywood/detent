import { useAtomSet } from "@effect/atom-react";
import * as Effect from "effect/Effect";
import React from "react";
import { useNavigate } from "@tanstack/react-router";

import { Button } from "../../components/ui/button.tsx";
import type {
  Conversation,
  ConversationListResponse,
} from "../../contracts/conversation.ts";
import { ConversationView } from "../App.tsx";
import { newCommandKey, useClient } from "../client.ts";
import { useWorkspacePanel } from "../components/ChatWorkspace.tsx";

export const ISSUE_QUESTIONS = [
  { label: "Why is this Blocked?", prompt: "Why is this Blocked?" },
  { label: "Summarize the history", prompt: "Summarize the history" },
  {
    label: "What is left before it can start?",
    prompt: "What is left before it can start?",
  },
  {
    label: "Split into smaller issues",
    prompt:
      "Use the split-issue skill to break this issue into smaller issues that can each land on their own. Wire up the dependencies so independent pieces can run in parallel, and show me the whole split as one proposal so I can confirm it once.",
  },
] as const;

export interface IssueAsk {
  readonly threads: readonly Conversation[];
  readonly selected: string | null;
  readonly select: (id: string | null) => void;
  readonly loading: boolean;
  readonly pending: boolean;
  readonly error: string | null;
  readonly start: (question: string) => Promise<boolean>;
}

export function useIssueAsk(
  projectId: string | null,
  workItemId: string,
): IssueAsk {
  const client = useClient();
  const create = useAtomSet(client.createConversation, { mode: "promise" });
  const refreshList = useAtomSet(client.refreshList, { mode: "promise" });
  const [threads, setThreads] = React.useState<readonly Conversation[]>([]);
  const [selected, select] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const intent = React.useRef<{
    question: string;
    key: string;
    messageKey: string;
  } | null>(null);
  React.useEffect(() => {
    let cancelled = false;
    setThreads([]);
    select(null);
    setLoading(true);
    intent.current = null;
    if (projectId === null) return;
    void (async () => {
      try {
        const found: Conversation[] = [];
        let cursor: string | null = null;
        do {
          const page: ConversationListResponse = await Effect.runPromise(
            client.http.listProjectConversations({
              projectId,
              subjectWorkItemId: workItemId,
              cursor,
              limit: 200,
            }),
          );
          found.push(...page.conversations);
          cursor = page.next_cursor;
        } while (cursor !== null && !cancelled);
        found.sort(
          (a, b) => Date.parse(b.created_at) - Date.parse(a.created_at),
        );
        if (!cancelled) {
          setThreads(found);
          select(found[0]?.id ?? null);
          setError(null);
        }
      } catch (cause) {
        if (!cancelled)
          setError(cause instanceof Error ? cause.message : String(cause));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, projectId, workItemId]);
  const starting = React.useRef(false);
  const start = React.useCallback(
    async (question: string): Promise<boolean> => {
      if (
        projectId === null ||
        starting.current ||
        question.trim().length === 0
      )
        return false;
      starting.current = true;
      setPending(true);
      if (intent.current?.question !== question)
        intent.current = {
          question,
          key: newCommandKey(),
          messageKey: newCommandKey(),
        };
      try {
        const result = await create({
          projectId,
          subjectWorkItemId: workItemId,
          key: intent.current.key,
          firstMessage: {
            key: intent.current.messageKey,
            text: question.trim(),
          },
        });
        if (result._tag !== "Success") {
          setError(result.failure.message);
          return false;
        }
        const conversation = result.success.conversation;
        setThreads((current) => [
          conversation,
          ...current.filter((thread) => thread.id !== conversation.id),
        ]);
        select(conversation.id);
        intent.current = null;
        setError(null);
        try {
          await refreshList();
        } catch (cause) {
          setError(
            `Chat created; recent chats could not refresh: ${cause instanceof Error ? cause.message : String(cause)}`,
          );
        }
        return true;
      } catch (cause) {
        setError(cause instanceof Error ? cause.message : String(cause));
        return false;
      } finally {
        starting.current = false;
        setPending(false);
      }
    },
    [create, projectId, refreshList, workItemId],
  );
  return { threads, selected, select, loading, pending, error, start };
}

export function IssueAskEntry({
  ask,
  canWrite,
}: {
  readonly ask: IssueAsk;
  readonly canWrite: boolean;
}): React.ReactElement {
  const panel = useWorkspacePanel();
  const [question, setQuestion] = React.useState("");
  const submit = async (text: string) => {
    panel?.openAsk();
    if (await ask.start(text)) setQuestion("");
  };
  return (
    <section
      data-testid="issue-ask-inline"
      className="rounded-lg border border-border bg-muted/30 p-3"
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void submit(question);
        }}
        className="flex items-center gap-2"
      >
        <input
          aria-label="Ask about this issue"
          placeholder="Ask about this issue…"
          value={question}
          disabled={!canWrite || ask.pending || ask.loading}
          onFocus={() => panel?.openAsk()}
          onChange={(event) => setQuestion(event.target.value)}
          className="min-w-0 flex-1 bg-transparent text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring"
        />
        <Button
          size="sm"
          variant="outline"
          type="submit"
          disabled={
            !canWrite || ask.pending || ask.loading || question.trim() === ""
          }
        >
          Ask
        </Button>
      </form>
      <p className="mt-2 text-xs text-muted-foreground">
        Private · Never posted to the issue
      </p>
      <div className="mt-2 flex flex-wrap gap-2">
        {ISSUE_QUESTIONS.map(({ label, prompt }) => (
          <Button
            key={label}
            size="xs"
            variant="ghost"
            disabled={!canWrite || ask.pending || ask.loading}
            onClick={() => void submit(prompt)}
          >
            {label}
          </Button>
        ))}
      </div>
    </section>
  );
}

export function IssueAskPanel({
  ask,
  projectId,
  identifier,
  canWrite,
}: {
  readonly ask: IssueAsk;
  readonly projectId: string;
  readonly identifier: string;
  readonly canWrite: boolean;
}): React.ReactElement {
  const navigate = useNavigate();
  const [question, setQuestion] = React.useState("");
  return (
    <section
      data-testid="issue-ask-panel"
      aria-label="Ask about this issue"
      className="flex min-h-0 flex-1 flex-col [&_a[href*='#issue-']]:rounded-md [&_a[href*='#issue-']]:bg-muted [&_a[href*='#issue-']]:px-1.5 [&_a[href*='#issue-']]:py-0.5"
    >
      <div className="space-y-3 border-b border-border p-4">
        <div className="flex items-center gap-2">
          <select
            aria-label="Issue chats"
            value={ask.selected ?? ""}
            onChange={(event) => ask.select(event.target.value || null)}
            className="min-w-0 flex-1 rounded-md border border-border bg-background px-2 py-1 text-sm"
          >
            <option value="">New question</option>
            {ask.threads.map((thread) => (
              <option key={thread.id} value={thread.id}>
                {thread.title}
              </option>
            ))}
          </select>
          <Button size="xs" variant="ghost" onClick={() => ask.select(null)}>
            New chat
          </Button>
          {ask.selected === null ? null : (
            <Button
              size="xs"
              variant="outline"
              onClick={() =>
                void navigate({
                  to: "/chat/c/$conversationId",
                  params: { conversationId: ask.selected! },
                })
              }
            >
              Open in Chat
            </Button>
          )}
        </div>
        <div
          className="rounded-lg border border-border bg-muted/30 p-3"
          data-testid="issue-ask-context"
        >
          <p className="text-sm font-medium">Context loaded from this issue</p>
          <p className="mt-1 text-xs text-muted-foreground">
            Body · Comments · Lane history · Attempts · Latest workpad ·
            Dependencies · Pull request
          </p>
          <p className="mt-1 text-xs text-muted-foreground">
            Refreshed on every turn. Private · Never posted to the issue.
          </p>
        </div>
        {ask.error === null ? null : (
          <p role="alert" className="text-sm text-destructive">
            {ask.error}
          </p>
        )}
      </div>
      {ask.loading ? (
        <p role="status" className="p-4 text-sm text-muted-foreground">
          Loading private chats…
        </p>
      ) : ask.selected !== null ? (
        <ConversationView
          key={ask.selected}
          conversationId={ask.selected}
          projectId={projectId}
          header={false}
        />
      ) : (
        <div className="flex min-h-0 flex-1 flex-col gap-3 p-4">
          {ISSUE_QUESTIONS.map(({ label, prompt }) => (
            <Button
              key={label}
              variant="outline"
              disabled={!canWrite || ask.pending || ask.loading}
              onClick={() => void ask.start(prompt)}
            >
              {label}
            </Button>
          ))}
          <form
            className="mt-auto space-y-2"
            onSubmit={(event) => {
              event.preventDefault();
              void ask.start(question).then((sent) => {
                if (sent) setQuestion("");
              });
            }}
          >
            <span className="inline-block rounded-md border border-border px-2 py-1 text-xs">
              {identifier} attached
            </span>
            <textarea
              aria-label="Ask message"
              placeholder="Ask about this issue…"
              value={question}
              onChange={(event) => setQuestion(event.target.value)}
              disabled={!canWrite || ask.pending || ask.loading}
              className="min-h-24 w-full rounded-lg border border-border bg-background p-3 text-sm focus-visible:ring-2 focus-visible:ring-ring"
            />
            <Button
              type="submit"
              size="sm"
              disabled={
                !canWrite ||
                ask.pending ||
                ask.loading ||
                question.trim() === ""
              }
            >
              Ask
            </Button>
          </form>
        </div>
      )}
    </section>
  );
}
