import type { Message } from "../../contracts/index.ts";
import { readAttention, readIssueProposal, readIssueResult } from "../../contracts/index.ts";

/** True for a status or tool message that carries one of the cards (B.8.6). */
export function carriesCard(message: Message): boolean {
  if (message.kind !== "status" && message.kind !== "tool") return false;
  return (
    readIssueProposal(message.data) !== undefined ||
    readIssueResult(message.data) !== undefined ||
    readAttention(message.data).length > 0
  );
}

/** True for a message that folds into a work log rather than being spoken. */
export function isWorkMessage(message: Message): boolean {
  return (message.kind === "status" || message.kind === "tool") && !carriesCard(message);
}

export type TimelineEntry =
  | { readonly kind: "message"; readonly message: Message }
  | {
      readonly kind: "work";
      readonly id: string;
      readonly messages: readonly Message[];
      readonly durationMs: number | null;
    };

export function formatWorkDuration(durationMs: number): string {
  const totalSeconds = Math.max(0, Math.round(durationMs / 1_000));
  if (totalSeconds < 60) return `${totalSeconds}s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return `${minutes}m ${seconds}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

function timestamp(message: Message): number | null {
  const at = Date.parse(message.created_at);
  return Number.isNaN(at) ? null : at;
}

export function foldTimeline(messages: readonly Message[]): readonly TimelineEntry[] {
  const entries: TimelineEntry[] = [];
  let run: Message[] = [];
  let boundary: number | null = null;

  const flush = (next: Message | undefined) => {
    if (run.length === 0) return;
    const first = run[0];
    const last = run[run.length - 1];
    const start = boundary ?? (first === undefined ? null : timestamp(first));
    const end =
      (next === undefined ? null : timestamp(next)) ?? (last === undefined ? null : timestamp(last));
    entries.push({
      kind: "work",
      id: `work:${first?.id ?? entries.length}`,
      messages: run,
      durationMs: start === null || end === null ? null : Math.max(0, end - start),
    });
    run = [];
  };

  for (const message of messages) {
    if (isWorkMessage(message)) {
      run.push(message);
      continue;
    }
    flush(message);
    entries.push({ kind: "message", message });
    boundary = message.role === "user" ? timestamp(message) : null;
  }
  flush(undefined);
  return entries;
}

/** The fold row's label. A run with no readable clock still names itself. */
export function workLogLabel(entry: Extract<TimelineEntry, { kind: "work" }>): string {
  if (entry.durationMs === null) {
    return entry.messages.length === 1 ? "1 step" : `${entry.messages.length} steps`;
  }
  return `Worked for ${formatWorkDuration(entry.durationMs)}`;
}
