import React from "react";
import * as Cause from "effect/Cause";
import { AsyncResult } from "effect/unstable/reactivity";

import type { AtomCommandResult } from "../runtime/state/runtime.ts";
import type { ScopedThreadRef } from "../environment/scoped.ts";
import { useSettledOverrideStore } from "../app/adapters/settledOverrides.ts";

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
      archiveThread: async () => unsupported("archiving a conversation"),
      deleteThread: async () => unsupported("deleting a conversation"),
    }),
    [settleThread, unsettleThread],
  );
}
