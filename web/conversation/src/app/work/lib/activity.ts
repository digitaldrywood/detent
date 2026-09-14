// The issue page's activity feed, merged client-side.
//
// Decisions §19.2: the feed is the work item's history, its attempts, the
// linked conversation's events and the comments, in one time-ordered list.
// The hub serves no merged projection — history is one endpoint, attempts
// another, comments a third, and the conversation is a different resource
// entirely — so this module is where the four become one, and it is pure so
// the ordering, the sentences and the fold can be tested without a clock, a
// DOM or a request.
//
// Two rules run through it:
//
//  - **Every row says what happened, not what the system recorded.** A row
//    whose sentence would only repeat an event type is low value and folds.
//  - **Nothing is invented.** Where the hub serves no actor name, the row
//    names the principal it does serve rather than a display name that does
//    not exist.
import type { Message, Question } from "../../../contracts/conversation.ts";
import type {
  CollaborationEvent,
  NativeActor,
  NativeAttempt,
  NativeComment,
} from "../../../contracts/work.ts";

/** Which glyph a row wears. Mapped to an icon by the component. */
export type ActivityIcon =
  | "created"
  | "moved"
  | "edited"
  | "labelled"
  | "related"
  | "runner"
  | "diff"
  | "comment"
  | "question"
  | "steering"
  | "attachment"
  | "stopped"
  | "failed";

export type ActivityKind = "event" | "comment" | "live";

export interface ActivityComment {
  readonly id: string;
  readonly body: string;
  readonly actor: string;
  readonly at: string;
}

export interface ActivityRow {
  /** Stable across re-reads: the source record's own id. */
  readonly key: string;
  readonly kind: ActivityKind;
  readonly icon: ActivityIcon;
  /** Epoch milliseconds. Rows the hub timestamped badly sort last, not first. */
  readonly at: number;
  readonly actor: string;
  /** One sentence, already written. The component adds the actor and the time. */
  readonly sentence: string;
  /** True for a row that folds into "Show N events…" when it has neighbours. */
  readonly lowValue: boolean;
  readonly comment?: ActivityComment;
}

/** A run of low-value rows, or one row that stands on its own. */
export type ActivityGroup =
  | { readonly kind: "row"; readonly key: string; readonly row: ActivityRow }
  | { readonly kind: "fold"; readonly key: string; readonly rows: readonly ActivityRow[] };

function stamp(at: string | null | undefined): number {
  if (at === undefined || at === null || at === "") return Number.MAX_SAFE_INTEGER;
  const value = Date.parse(at);
  return Number.isNaN(value) ? Number.MAX_SAFE_INTEGER : value;
}

/**
 * How a principal is named in a sentence.
 *
 * The hub serves a principal id and an actor kind and nothing else — no
 * display name, no avatar — so this is the whole of what can honestly be
 * said. The reader's own principal becomes "You" because that is the one
 * name this client does know.
 */
export function actorLabel(
  actor: NativeActor | undefined,
  viewerPrincipalId: string | null,
  runnerName: string | null,
): string {
  if (actor === undefined) return "Someone";
  if (viewerPrincipalId !== null && actor.principal_id === viewerPrincipalId) return "You";
  if (actor.kind === "runner") return runnerName ?? "The runner";
  const id = actor.principal_id;
  return id.length > 20 ? `${id.slice(0, 18)}…` : id;
}

/** The runner an attempt ran on, as a sentence names it. */
export function runnerLabel(attempt: NativeAttempt | undefined): string | null {
  if (attempt === undefined) return null;
  return attempt.runner_id ?? attempt.machine_id ?? null;
}

interface HistorySentence {
  readonly icon: ActivityIcon;
  readonly sentence: string;
  readonly lowValue: boolean;
}

/**
 * One history event as a sentence.
 *
 * `type` is an open string on the wire (the hub grows the log), so an
 * unrecognised type still produces a row — named by its own type — rather
 * than disappearing. Those rows are low value by definition: the client
 * cannot say what they mean.
 */
export function historySentence(event: CollaborationEvent): HistorySentence {
  const data = event.data;
  switch (event.type) {
    case "issue.created":
      return { icon: "created", sentence: "created the issue", lowValue: false };
    case "issue.edited":
      return {
        icon: "edited",
        sentence:
          data.fields === undefined || data.fields.length === 0
            ? "edited the issue"
            : `edited ${[...data.fields].join(", ")}`,
        lowValue: true,
      };
    case "issue.cutover":
      return { icon: "edited", sentence: "moved the issue onto the native tracker", lowValue: true };
    case "workflow.transitioned":
      return {
        icon: "moved",
        sentence:
          data.from_state === undefined
            ? `moved the issue to ${data.to_state ?? "another state"}`
            : `moved it from ${data.from_state} to ${data.to_state ?? "another state"}`,
        lowValue: false,
      };
    case "dependency.changed":
      return {
        icon: "related",
        sentence:
          data.operation === "remove"
            ? `removed the dependency on ${data.related_work_item_id ?? "another issue"}`
            : `made this depend on ${data.related_work_item_id ?? "another issue"}`,
        lowValue: false,
      };
    case "comment.created":
      return { icon: "comment", sentence: "commented", lowValue: true };
    case "comment.edited":
      return { icon: "comment", sentence: "edited a comment", lowValue: true };
    case "comment.imported":
    case "github.imported":
      return { icon: "comment", sentence: "imported this from GitHub", lowValue: true };
    case "change.created":
      return { icon: "diff", sentence: "opened a change request", lowValue: false };
    case "change.version_published":
      return { icon: "diff", sentence: "published a new round of the change", lowValue: false };
    case "run.started":
      return { icon: "runner", sentence: "started an attempt", lowValue: true };
    case "run.checkpointed":
      return { icon: "runner", sentence: "checkpointed the attempt", lowValue: true };
    case "run.finished":
      return {
        icon: "runner",
        sentence: `finished the attempt${
          data.run?.outcome === undefined ? "" : ` · ${data.run.outcome}`
        }`,
        lowValue: true,
      };
    default:
      return { icon: "edited", sentence: `recorded ${event.type}`, lowValue: true };
  }
}

/** The sentence an attempt's own record carries, once it has ended. */
function attemptSentence(attempt: NativeAttempt, ordinal: number): HistorySentence | null {
  switch (attempt.status) {
    case "running":
      // A running attempt is the live row's business, not a history row's.
      return null;
    case "succeeded":
      return { icon: "runner", sentence: `finished attempt ${ordinal}`, lowValue: false };
    case "failed":
      return {
        icon: "failed",
        sentence: `attempt ${ordinal} failed${
          attempt.outcome === undefined ? "" : ` · ${attempt.outcome}`
        }`,
        lowValue: false,
      };
    case "cancelled":
      return { icon: "stopped", sentence: `attempt ${ordinal} was cancelled`, lowValue: false };
    case "interrupted":
      return { icon: "stopped", sentence: `attempt ${ordinal} was interrupted`, lowValue: false };
  }
}

export interface ConversationActivity {
  readonly questions: readonly Question[];
  readonly messages: readonly Message[];
}

export interface ActivityInput {
  readonly history: readonly CollaborationEvent[];
  readonly attempts: readonly NativeAttempt[];
  readonly comments: readonly NativeComment[];
  /** The linked conversation's own record, or null when there is none. */
  readonly conversation: ConversationActivity | null;
  /** The signed-in principal, so their own rows read "You". */
  readonly viewerPrincipalId: string | null;
}

/**
 * Every source, in one list, oldest first.
 *
 * A history `run.started` and the attempt row it produced are the same fact
 * from two endpoints, so the attempt list wins for anything that has ended —
 * it carries the status and the outcome — and the history's own run rows stay
 * low value behind the fold rather than being dropped, because the fold is
 * where "the same thing, in the log's words" belongs.
 */
export function mergeActivity(input: ActivityInput): readonly ActivityRow[] {
  const rows: ActivityRow[] = [];
  const runner = runnerLabel(input.attempts.at(-1) ?? undefined);
  // A comment the reader can see as a card does not also need a line saying a
  // comment happened. The log's row is dropped rather than folded: the card is
  // directly below it and the fold would be a disclosure over a duplicate.
  const drawnComments = new Set(input.comments.map((comment) => comment.comment_id));

  for (const event of input.history) {
    if (event.data.comment_id !== undefined && drawnComments.has(event.data.comment_id)) continue;
    const written = historySentence(event);
    rows.push({
      key: `history:${event.event_id}`,
      kind: "event",
      icon: written.icon,
      at: stamp(event.recorded_at),
      actor: actorLabel(event.actor, input.viewerPrincipalId, runner),
      sentence: written.sentence,
      lowValue: written.lowValue,
    });
  }

  input.attempts.forEach((attempt, index) => {
    const claimed = runnerLabel(attempt) ?? "A runner";
    rows.push({
      key: `attempt:${attempt.attempt_id}:claimed`,
      kind: "event",
      icon: "runner",
      at: stamp(attempt.started_at),
      actor: claimed,
      sentence: `claimed the issue and started attempt ${index + 1}${
        attempt.identity === undefined
          ? ""
          : ` · ${attempt.identity.backend} ${attempt.identity.model}`
      }`,
      lowValue: false,
    });
    const ended = attemptSentence(attempt, index + 1);
    if (ended === null) return;
    rows.push({
      key: `attempt:${attempt.attempt_id}:ended`,
      kind: "event",
      icon: ended.icon,
      at: stamp(attempt.updated_at),
      actor: claimed,
      sentence: ended.sentence,
      lowValue: ended.lowValue,
    });
  });

  for (const comment of input.comments) {
    rows.push({
      key: `comment:${comment.comment_id}`,
      kind: "comment",
      icon: "comment",
      at: stamp(comment.created_at),
      actor: actorLabel(comment.actor, input.viewerPrincipalId, runner),
      sentence: "commented",
      lowValue: false,
      comment: {
        id: comment.comment_id,
        body: comment.body,
        actor: actorLabel(comment.actor, input.viewerPrincipalId, runner),
        at: comment.created_at,
      },
    });
  }

  const conversation = input.conversation;
  if (conversation !== null) {
    for (const question of conversation.questions) {
      const prompt = question.questions[0]?.question ?? "a question";
      rows.push({
        key: `question:${question.id}`,
        kind: "event",
        icon: "question",
        at: stamp(question.created_at),
        actor: runner ?? "The runner",
        sentence: `asked: ${prompt}`,
        lowValue: false,
      });
      if (question.status !== "answered") continue;
      rows.push({
        key: `question:${question.id}:answered`,
        kind: "event",
        icon: "question",
        at: stamp(question.updated_at),
        actor:
          question.answered_by === null
            ? "Someone"
            : actorLabel(
                { kind: "human", principal_id: question.answered_by },
                input.viewerPrincipalId,
                runner,
              ),
        sentence: "answered the question",
        lowValue: false,
      });
    }

    for (const message of conversation.messages) {
      // Steering is a user message aimed at a turn that was already open. A
      // first message is the chat, not a steering event, and the transcript in
      // the panel is where the whole conversation is read.
      if (message.role === "user" && message.attempt_id !== null) {
        rows.push({
          key: `steering:${message.id}`,
          kind: "event",
          icon: "steering",
          at: stamp(message.created_at),
          actor: actorLabel(
            { kind: "human", principal_id: message.actor.principal_id },
            input.viewerPrincipalId,
            runner,
          ),
          sentence: `sent the runner: ${message.text.trim().slice(0, 120)}`,
          lowValue: false,
        });
      }
      if (message.kind === "interrupt") {
        rows.push({
          key: `interrupt:${message.id}`,
          kind: "event",
          icon: "stopped",
          at: stamp(message.created_at),
          actor: actorLabel(
            { kind: "human", principal_id: message.actor.principal_id },
            input.viewerPrincipalId,
            runner,
          ),
          sentence: "interrupted the runner",
          lowValue: false,
        });
      }
      const attachments = message.attachments ?? [];
      if (attachments.length > 0) {
        rows.push({
          key: `attachments:${message.id}`,
          kind: "event",
          icon: "attachment",
          at: stamp(message.created_at),
          actor: actorLabel(
            { kind: "human", principal_id: message.actor.principal_id },
            input.viewerPrincipalId,
            runner,
          ),
          sentence:
            attachments.length === 1
              ? `attached ${attachments[0]?.name ?? "a file"}`
              : `attached ${attachments.length} files`,
          lowValue: true,
        });
      }
    }
  }

  // Oldest first, and total: two rows recorded in the same millisecond keep a
  // stable order rather than swapping on every re-render.
  return rows.toSorted((left, right) => left.at - right.at || left.key.localeCompare(right.key));
}

/**
 * Folds runs of low-value rows, as Linear does.
 *
 * A single low-value row is not worth a disclosure — pressing "Show 1 event…"
 * to read one line is worse than the line — so a run folds only from two.
 */
export function foldActivity(rows: readonly ActivityRow[]): readonly ActivityGroup[] {
  const groups: ActivityGroup[] = [];
  let run: ActivityRow[] = [];
  const flush = () => {
    if (run.length === 0) return;
    if (run.length === 1) {
      const only = run[0] as ActivityRow;
      groups.push({ kind: "row", key: only.key, row: only });
    } else {
      groups.push({ kind: "fold", key: `fold:${run[0]?.key ?? groups.length}`, rows: run });
    }
    run = [];
  };
  for (const row of rows) {
    if (row.lowValue && row.kind === "event") {
      run.push(row);
      continue;
    }
    flush();
    groups.push({ kind: "row", key: row.key, row });
  }
  flush();
  return groups;
}

/** `Show 3 events…`, naming what is behind the disclosure. */
export function foldLabel(rows: readonly ActivityRow[]): string {
  return rows.length === 1 ? "Show 1 event…" : `Show ${rows.length} events…`;
}

/** `4:42 PM`, the clock the feed stamps a row with. */
export function timeLabel(at: number): string {
  if (!Number.isFinite(at) || at === Number.MAX_SAFE_INTEGER) return "";
  return new Date(at).toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}
