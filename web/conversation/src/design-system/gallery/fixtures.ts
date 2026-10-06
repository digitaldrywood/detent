// Synthetic data for composition specimens.
//
// Seeded from the shared contract fixtures (the same JSON the component tests
// build from, see `tests/components/builders.ts`) so a specimen shows a
// realistic payload rather than a hand-drawn approximation of one.
import conversationFixture from "../../contracts/fixtures/conversation.json";
import assistantFixture from "../../contracts/fixtures/message-assistant.json";
import questionFixture from "../../contracts/fixtures/question.json";
import userFixture from "../../contracts/fixtures/message-user.json";
import attemptsFixture from "../../contracts/fixtures/work-attempt-list.json";
import itemFixture from "../../contracts/fixtures/work-item.json";
import projectFixture from "../../contracts/fixtures/work-project.json";
import usageDailyFixture from "../../contracts/fixtures/usage-30d.json";
import usageEmptyFixture from "../../contracts/fixtures/usage-empty.json";

import type { Conversation, Message, Question } from "../../contracts/index.ts";
import type { NativeAttempt, NativeIssue, NativeProject } from "../../contracts/work.ts";
import { decodeUsageReport } from "../../contracts/usage.ts";
import { toMergedUsage, type MergedUsage } from "../../app/usage/adapter.ts";
import { toAttemptView, toWorkItemView } from "../../app/work/lib/fromWire.ts";
import type { Lane, WorkItemView } from "../../app/work/lib/model.ts";
import type { ConversationDetail } from "../../runtime/state/conversationState.ts";

/** A fixed clock for components that take `now` as a prop, so their ages read the same everywhere. */
export const NOW = Date.parse("2026-09-09T12:00:00Z");

/**
 * A time relative to the real clock, for components that read the clock
 * themselves (the sidebar's "2m" and "Working 4m").
 */
export function minutesAgo(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString();
}

export function conversation(overrides: Partial<Conversation> = {}): Conversation {
  return { ...(conversationFixture as Conversation), ...overrides };
}

export function userMessage(overrides: Partial<Message> = {}): Message {
  return { ...(userFixture as Message), ...overrides };
}

export function assistantMessage(overrides: Partial<Message> = {}): Message {
  return { ...(assistantFixture as Message), ...overrides };
}

/** A work-log step: the system status messages a turn folds into "Worked for…". */
export function statusStep(id: string, at: string, text: string): Message {
  return { ...assistantMessage(), id, role: "system", kind: "status", data: {}, text, created_at: at };
}

export function question(overrides: Partial<Question> = {}): Question {
  return { ...(questionFixture as Question), ...overrides };
}

export function detail(overrides: Partial<ConversationDetail> = {}): ConversationDetail {
  return {
    conversation: conversation(),
    messages: [userMessage(), assistantMessage()],
    deltas: {},
    questions: [],
    receipts: {},
    pending: [],
    controls: [],
    staleExecution: false,
    page: { hasMore: false, loadingOlder: false, oldestSeq: 7 },
    ...overrides,
  };
}

export const ASSISTANT_MARKDOWN = [
  "The lock renewal now waits on a healthy handoff. Two changes:",
  "",
  "1. `renewLease` reads the lease duration instead of a constant.",
  "2. The handoff check runs before the renewal, not after.",
  "",
  "```go",
  "func (l *Lock) renewLease(ctx context.Context) error {",
  "\tinterval := l.lease / 2",
  "\treturn l.store.Renew(ctx, l.key, interval)",
  "}",
  "```",
  "",
  "| Case | Before | After |",
  "| --- | --- | --- |",
  "| 30s lease | renew at 20s | renew at 15s |",
  "| 2m lease | renew at 20s | renew at 60s |",
  "",
  "> [!NOTE]",
  "> The integration test covers both windows.",
].join("\n");

const PROJECT = projectFixture as unknown as NativeProject;
const ATTEMPTS = (attemptsFixture as { items: unknown[] }).items as unknown as NativeAttempt[];

export function workItem(overrides: Partial<WorkItemView> = {}): WorkItemView {
  return { ...toWorkItemView(itemFixture as unknown as NativeIssue, "detent"), ...overrides };
}

export function runningAttempt(): WorkItemView["attempt"] {
  return toAttemptView(ATTEMPTS);
}

export const LANES: readonly Lane[] = PROJECT.states.map((state) => ({
  id: state.name,
  name: state.name,
  terminal: state.terminal,
  category: state.terminal ? "completed" : state.dispatchable ? "unstarted" : "started",
}));

export function usageDaily(): MergedUsage {
  return toMergedUsage(decodeUsageReport(usageDailyFixture));
}

export function usageEmpty(): MergedUsage {
  return toMergedUsage(decodeUsageReport(usageEmptyFixture));
}
