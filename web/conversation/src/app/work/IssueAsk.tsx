import { useAtomSet } from "@effect/atom-react";
import * as Effect from "effect/Effect";
import React from "react";
import { useNavigate } from "@tanstack/react-router";

import { Button } from "../../components/ui/button.tsx";
import type {
  Conversation,
  ConversationListResponse,
} from "../../contracts/conversation.ts";
import { DEFAULT_TURN_PREFERENCES } from "../../contracts/index.ts";
import { ComposerContextAttachment } from "../components/ComposerContextAttachment.tsx";
import { Composer } from "../components/Composer.tsx";
import { ConversationView } from "../App.tsx";
import { newCommandKey, useClient } from "../client.ts";
import { useWorkspacePanel } from "../components/ChatWorkspace.tsx";

import { chatSlashCommands, ISSUE_QUESTIONS } from "../adapters/chatPrompts.ts";

export { ISSUE_QUESTIONS };

export interface IssueAsk {
  readonly threads: readonly Conversation[];
  readonly selected: string | null;
  readonly select: (id: string | null) => void;
  readonly loading: boolean;
  readonly pending: boolean;
  readonly error: string | null;
  readonly start: (question: string, attachIssue?: boolean) => Promise<boolean>;
}

export function useIssueAsk(
  projectId: string | null,
  workItemId: string,
): IssueAsk {
  const client = useClient();
  const navigate = useNavigate();
  const create = useAtomSet(client.createConversation, { mode: "promise" });
  const refreshList = useAtomSet(client.refreshList, { mode: "promise" });
  const [threads, setThreads] = React.useState<readonly Conversation[]>([]);
  const [selected, select] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [pending, setPending] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const intent = React.useRef<{
    question: string;
    attachIssue: boolean;
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
    async (question: string, attachIssue = true): Promise<boolean> => {
      if (
        projectId === null ||
        starting.current ||
        question.trim().length === 0
      )
        return false;
      starting.current = true;
      setPending(true);
      if (intent.current?.question !== question || intent.current?.attachIssue !== attachIssue)
        intent.current = {
          question,
          attachIssue,
          key: newCommandKey(),
          messageKey: newCommandKey(),
        };
      try {
        const result = await create({
          projectId,
          ...(attachIssue ? { subjectWorkItemId: workItemId } : {}),
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
        if (!attachIssue) {
          intent.current = null;
          await navigate({ to: "/chat/c/$conversationId", params: { conversationId: conversation.id } });
          return true;
        }
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
    [create, navigate, projectId, refreshList, workItemId],
  );
  return { threads, selected, select, loading, pending, error, start };
}

export function IssueAskPanel({
  ask,
  projectId,
  identifier,
  title,
  canWrite,
}: {
  readonly ask: IssueAsk;
  readonly projectId: string;
  readonly identifier: string;
  readonly title: string;
  readonly canWrite: boolean;
}): React.ReactElement {
  const navigate = useNavigate();
  const panel = useWorkspacePanel();
  const [question, setQuestion] = React.useState("");
  const [attached, setAttached] = React.useState(true);
  const composer = {
    label: "Ask message",
    placeholder: `Ask about ${identifier}…`,
    promptClassName: "min-h-[3lh] max-h-[12lh]",
    promptContext: (
      ask.selected === null && !attached ? null : <ComposerContextAttachment
        label={title ? `${identifier} ${title}` : identifier}
        title={title}
        onRemove={ask.selected === null ? () => setAttached(false) : undefined}
        disabled={ask.pending}
      />
    ),
    sendLabel: "Ask",
    focusRequest: panel?.askFocusRequest ?? 0,
    footerControls: "none" as const,
    attachControl: "none" as const,
    disabled: !canWrite,
  };
  return (
    <section
      data-testid="issue-ask-panel"
      aria-label="Ask about this issue"
      className="flex min-h-0 flex-1 flex-col [&_a[href*='#issue-']]:rounded-md [&_a[href*='#issue-']]:bg-muted [&_a[href*='#issue-']]:px-1.5 [&_a[href*='#issue-']]:py-0.5"
    >
      <div className="space-y-3 border-b border-border p-4">
        <div className="flex flex-wrap items-center gap-2">
          {ask.threads.length === 0 ? null : (
            <select
              aria-label="Issue chats"
              value={ask.selected ?? ""}
              onChange={(event) => ask.select(event.target.value || null)}
              className="min-w-0 flex-1 rounded-md border border-border bg-background px-2 py-1 text-sm"
            >
              <option value="">Earlier questions</option>
              {ask.threads.map((thread) => (
                <option key={thread.id} value={thread.id}>
                  {thread.title}
                </option>
              ))}
            </select>
          )}
          <Button size="xs" variant="ghost" onClick={() => {
            setAttached(true);
            ask.select(null);
            panel?.openAsk();
          }}>
            New question
          </Button>
          {ask.selected === null ? null : (
            <Button
              size="xs"
              variant="outline"
              onClick={() => void navigate({
                to: "/chat/c/$conversationId",
                params: { conversationId: ask.selected! },
              })}
            >
              Open in Chat
            </Button>
          )}
        </div>
        {ask.error === null ? null : (
          <p role="alert" className="text-sm text-destructive">{ask.error}</p>
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
          issueAskComposer={composer}
        />
      ) : (
        <div className="flex min-h-0 flex-1 flex-col">
          <div className="flex min-h-0 flex-1 flex-col items-start gap-1 overflow-y-auto p-4">
            {ISSUE_QUESTIONS.map(({ label, prompt }) => (
              <button
                key={label}
                type="button"
                className="w-full rounded-md px-2 py-2 text-left text-sm font-normal text-muted-foreground hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
                disabled={!canWrite || ask.pending}
                onClick={() => void ask.start(prompt, attached)}
              >
                {label}
              </button>
            ))}
          </div>
          <Composer
            {...composer}
            value={question}
            onChange={setQuestion}
            onSend={() => void ask.start(question, attached).then((sent) => {
              if (sent) setQuestion("");
            })}
            sending={ask.pending}
            streaming={false}
            autoFocus
            preferences={DEFAULT_TURN_PREFERENCES}
            onPreferencesChange={() => {}}
            slashCommands={chatSlashCommands({
              issue: attached,
              coordinator: canWrite,
              insert: setQuestion,
              send: (prompt) => void ask.start(prompt, attached).then((sent) => { if (sent) setQuestion(""); }),
            })}
            domId="dc-issue-ask-composer"
          />
        </div>
      )}
      <p data-testid="issue-ask-context" className="px-4 pb-3 text-xs text-muted-foreground">
        {ask.selected !== null || attached
          ? "Luna reads this issue's body, comments, history, runs and pull request on every turn. Only you see this chat."
          : "Only you see this project-wide chat."}
      </p>
    </section>
  );
}
