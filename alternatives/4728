import React from "react";
import * as Cause from "effect/Cause";
import { AsyncResult } from "effect/unstable/reactivity";
import * as Effect from "effect/Effect";

import type { AtomCommandResult } from "../runtime/state/runtime.ts";
import type { ScopedThreadRef } from "../environment/scoped.ts";
import { useSettledOverrideStore } from "../app/adapters/settledOverrides.ts";
import { useSidebarData, readSidebarData } from "../app/adapters/sidebarData.tsx";
import { ClientContext } from "../app/client.ts";
import { HUB_ENVIRONMENT_ID } from "../contracts/index.ts";
import { settlePromise } from "../runtime/state/runtime.ts";

type Result = AtomCommandResult<unknown, Error>;

function succeeded(): Result {
  return AsyncResult.success<unknown, Error>(undefined);
}

function unsupported(action: string): Result {
  return AsyncResult.failure<unknown, Error>(
    Cause.fail(new Error(`Detent Cloud does not support ${action} yet.`)),
  );
}

export function useThreadActions(): {
  readonly settleThread: (ref: ScopedThreadRef) => Promise<Result>;
  readonly unsettleThread: (ref: ScopedThreadRef) => Promise<Result>;
  readonly snoozeThread: (ref: ScopedThreadRef, snoozedUntil: string) => Promise<Result>;
  readonly unsnoozeThread: (ref: ScopedThreadRef) => Promise<Result>;
  readonly pinThread: (
    ref: ScopedThreadRef,
    options?: { readonly orderKey?: string },
  ) => Promise<Result>;
  readonly unpinThread: (ref: ScopedThreadRef) => Promise<Result>;
  readonly confirmAndUnpinThread: (ref: ScopedThreadRef) => Promise<Result>;
  readonly reorderPinnedThread: (ref: ScopedThreadRef, orderKey: string) => Promise<Result>;
  readonly reorderActiveThread: (ref: ScopedThreadRef, orderKey: string) => Promise<Result>;
  readonly archiveThread: (
    ref: ScopedThreadRef,
    options?: { readonly onArchived?: () => void },
  ) => Promise<Result>;
  readonly deleteThread: (
    ref: ScopedThreadRef,
    options?: { readonly deletedThreadKeys?: ReadonlySet<string> },
  ) => Promise<Result>;
} {
  const settle = useSettledOverrideStore((store) => store.settle);
  const unsettle = useSettledOverrideStore((store) => store.unsettle);
  const client = React.useContext(ClientContext);
  const sidebar = useSidebarData();
  const removeThread = React.useCallback(
    async (ref: ScopedThreadRef, action: "archive" | "delete", onArchived?: () => void): Promise<Result> =>
      settlePromise(async () => {
        const data = readSidebarData() ?? sidebar;
        const conversation = [...(data?.conversations ?? []), ...(data?.serverResults ?? [])]
          .find((candidate) => candidate.id === ref.threadId);
        if (client === null || ref.environmentId !== HUB_ENVIRONMENT_ID || conversation === undefined) {
          throw new Error("That conversation is not available.");
        }
        const input = { projectId: conversation.project_id, conversationId: conversation.id };
        const result = await Effect.runPromise(action === "archive"
          ? client.effects.archiveConversation(input) : client.effects.deleteConversation(input));
        if (result._tag === "Failure") throw result.failure;
        onArchived?.();
        const current = readSidebarData() ?? sidebar;
        if (current?.activeConversationId === conversation.id) {
          current.navigation?.onNavigate(`/chat/p/${conversation.project_id}`);
        }
      }),
    [client, sidebar],
  );

  const settleThread = React.useCallback(
    async (ref: ScopedThreadRef): Promise<Result> => {
      settle(ref.threadId as string, new Date().toISOString());
      return succeeded();
    },
    [settle],
  );

  const unsettleThread = React.useCallback(
    async (ref: ScopedThreadRef): Promise<Result> => {
      unsettle(ref.threadId as string, new Date().toISOString());
      return succeeded();
    },
    [unsettle],
  );

  return React.useMemo(
    () => ({
      settleThread,
      unsettleThread,
      snoozeThread: async () => unsupported("snoozing a conversation"),
      unsnoozeThread: async () => unsupported("snoozing a conversation"),
      pinThread: async () => unsupported("pinning a conversation"),
      unpinThread: async () => unsupported("pinning a conversation"),
      confirmAndUnpinThread: async () => unsupported("pinning a conversation"),
      reorderPinnedThread: async () => unsupported("reordering conversations"),
      reorderActiveThread: async () => unsupported("reordering conversations"),
      archiveThread: (ref, options) => removeThread(ref, "archive", options?.onArchived),
      deleteThread: (ref) => removeThread(ref, "delete"),
    }),
    [settleThread, unsettleThread, removeThread],
  );
}
