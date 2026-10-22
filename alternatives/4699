import { scopeThreadRef } from "@t3tools/client-runtime/environment";
import type { EnvironmentId, ScopedThreadRef, ThreadId } from "@t3tools/contracts";
import { HUB_ENVIRONMENT_ID } from "./contracts/index.ts";
import type { DraftId } from "./composerDraftStore";

export type ThreadRouteTarget =
  | {
      kind: "server";
      threadRef: ScopedThreadRef;
    }
  | {
      kind: "draft";
      draftId: DraftId;
    };

type DraftThreadRouteState = {
  environmentId: EnvironmentId;
  threadId: ThreadId;
  promotedTo?: ScopedThreadRef | null;
};

export type ThreadRouteRenderState = "loading" | "ready" | "missing";

export function resolveThreadRouteRenderState(input: {
  bootstrapComplete: boolean;
  serverThreadShellExists: boolean;
  serverThreadDetailExists: boolean;
  serverThreadDetailDeleted: boolean;
  draftThreadExists: boolean;
}): ThreadRouteRenderState {
  if (!input.bootstrapComplete) {
    return "loading";
  }
  if (input.serverThreadDetailExists || input.draftThreadExists) {
    return "ready";
  }
  if (input.serverThreadDetailDeleted) {
    return "missing";
  }
  return input.serverThreadShellExists ? "loading" : "missing";
}

export function buildThreadRouteParams(ref: ScopedThreadRef): {
  conversationId: ThreadId;
} {
  return { conversationId: ref.threadId };
}

export function buildDraftThreadRouteParams(draftId: DraftId): {
  draftId: DraftId;
} {
  return { draftId };
}

export function resolveThreadRouteRef(
  params: Partial<Record<"conversationId", string | undefined>>,
): ScopedThreadRef | null {
  if (!params.conversationId) {
    return null;
  }

  return scopeThreadRef(HUB_ENVIRONMENT_ID as EnvironmentId, params.conversationId as ThreadId);
}

export function resolveThreadRouteTarget(
  params: Partial<Record<"conversationId" | "draftId", string | undefined>>,
): ThreadRouteTarget | null {
  if (params.conversationId) {
    return {
      kind: "server",
      threadRef: scopeThreadRef(
        HUB_ENVIRONMENT_ID as EnvironmentId,
        params.conversationId as ThreadId,
      ),
    };
  }

  // Detent has no draft route: `/chat` is the unsent composer and it carries
  // no id, so this branch never fires. It stays because the copied callers
  // still read its `kind` (decisions.md §16).
  if (!params.draftId) {
    return null;
  }

  return {
    kind: "draft",
    draftId: params.draftId as DraftId,
  };
}

/**
 * Resolves the thread represented by either a canonical thread route or a
 * draft route whose promotion to a server thread has been recorded.
 */
export function resolveActiveThreadRouteRef(
  target: ThreadRouteTarget | null,
  draftThread: DraftThreadRouteState | null,
): ScopedThreadRef | null {
  if (target?.kind === "server") {
    return target.threadRef;
  }
  if (target?.kind !== "draft" || !draftThread?.promotedTo) {
    return null;
  }
  return draftThread.promotedTo;
}
