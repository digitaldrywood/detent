import React from "react";
import * as Cause from "effect/Cause";
import { AsyncResult } from "effect/unstable/reactivity";

import { useSidebarData } from "../app/adapters/sidebarData.tsx";
import type { PreviewCommand } from "./preview.ts";
import type { ShellCommand } from "./shell.ts";
import type { ThreadCommand } from "./threads.ts";

/**
 * The commands Detent names but cannot run.
 *
 * Each stands for something that happens on a machine this client cannot
 * reach: an editor on the reader's desktop, a browser view the desktop app
 * embeds, a file in the runner's checkout (decisions.md §18). Naming them
 * keeps the copied `ChatMarkdown.tsx` and `chat/ProposedPlanCard.tsx`
 * byte-identical; refusing them here is what makes their own "that failed"
 * branch — a toast, or a disabled menu item — the thing the reader sees,
 * which is what section 16 asks for.
 */
export type UnavailableCommand = ShellCommand | PreviewCommand | "project.writeFile";

export type DetentCommand = ThreadCommand | UnavailableCommand;

export interface UpdateMetadataInput {
  readonly environmentId: string;
  readonly input: {
    readonly threadId: string;
    readonly title?: string;

    readonly regenerateTitle?: boolean;
  };
}

/**
 * One settled command result, in the shape every copied caller reads.
 *
 * The value is generic and defaults to `never` because Detent runs exactly one
 * of these commands and it returns nothing. The callers that *would* read a
 * value — `chat/ProposedPlanCard.tsx` wanting the path it saved to, the
 * browser preview wanting its session snapshot — only reach it inside a
 * `result._tag === "Success"` branch that Detent never enters, and `never`
 * lets those branches type-check against whatever shape upstream expects
 * instead of forcing an edit to a copied file.
 */
export type DetentCommandResult<Value> =
  | AsyncResult.Success<Value, Error>
  | AsyncResult.Failure<Value, Error>;

/**
 * The default `Value`: an object whose every field is `never`.
 *
 * It behaves as the empty payload it is — no success branch ever produces one
 * — while still letting a copied caller read whatever field upstream reads off
 * it (`result.value.relativePath`, `result.value.tabId`) and get a `never`
 * that satisfies the type it is used at. A bare `never` would refuse the
 * property access outright and force an edit to the copied file.
 */
export type EmptyCommandValue = { readonly [key: string]: never };

/**
 * What a command is handed. `input` is `unknown` on purpose: each copied
 * caller passes its own payload — a rename, a preview open, a file write — and
 * a parameter that accepts all of them is what makes this one hook assignable
 * to each of their function types without widening any of them.
 */
export interface DetentCommandInput {
  readonly environmentId: string;
  readonly input: unknown;
}

function failed<Value>(message: string): DetentCommandResult<Value> {
  return AsyncResult.failure<Value, Error>(Cause.fail(new Error(message)));
}

/** The rename payload, once it has been checked rather than assumed. */
function renameInput(
  input: unknown,
): { readonly threadId: string; readonly title?: string } | null {
  if (typeof input !== "object" || input === null) return null;
  const candidate = input as { threadId?: unknown; title?: unknown };
  if (typeof candidate.threadId !== "string") return null;
  return {
    threadId: candidate.threadId,
    ...(typeof candidate.title === "string" ? { title: candidate.title } : {}),
  };
}

/** What the reader is told when a command names a machine Detent cannot reach. */
const UNAVAILABLE_REASON: Readonly<Record<UnavailableCommand, string>> = {
  "shell.openInEditor":
    "Opening a file in an editor needs the desktop app; this conversation runs in the browser.",
  "preview.open": "The Browser surface is not available for this runner yet.",
  "project.writeFile":
    "Saving into the checkout needs a runner that reports the files capability.",
};

export interface DetentCommandRunner {
  <Value = EmptyCommandValue>(input: DetentCommandInput): Promise<DetentCommandResult<Value>>;
}

export function useAtomCommand(
  command: DetentCommand,
  _options?: { readonly reportFailure?: boolean } | string,
): DetentCommandRunner {
  const onRename = useSidebarData()?.onRename;
  return React.useCallback(
    async <Value = EmptyCommandValue>(
      input: DetentCommandInput,
    ): Promise<DetentCommandResult<Value>> => {
      const unavailable = UNAVAILABLE_REASON[command as UnavailableCommand];
      if (unavailable !== undefined) {
        return failed<Value>(unavailable);
      }
      if (command !== "conversation.updateMetadata") {
        return failed<Value>(`Unknown command ${command}.`);
      }
      const rename = renameInput(input.input);
      if (rename === null) {
        return failed<Value>("This command was sent without a conversation.");
      }
      if (rename.title === undefined) {
        return failed<Value>("Detent Cloud does not regenerate titles yet.");
      }
      if (onRename === undefined) {
        return failed<Value>("This conversation is not open.");
      }
      try {
        await onRename(rename.threadId, rename.title);
        // The rename is the one command that succeeds, and it yields nothing.
        // `Value` is the caller's expected payload — `never` wherever nobody
        // reads one — so the empty success is stated once, here, rather than
        // by widening every copied caller's type.
        return AsyncResult.success<Value, Error>(undefined as Value);
      } catch (error) {
        return failed<Value>(error instanceof Error ? error.message : String(error));
      }
    },
    [command, onRename],
  );
}
