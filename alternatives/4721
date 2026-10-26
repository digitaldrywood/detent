import React from "react";

import { scopeProjectRef } from "../environment/scoped.ts";
import type { ScopedProjectRef, ScopedThreadRef } from "../environment/scoped.ts";
import { HUB_ENVIRONMENT_ID } from "../contracts/index.ts";
import type { ProjectId, ThreadId } from "../contracts/ui.ts";
import { useSidebarData } from "../app/adapters/sidebarData.tsx";

interface ThreadContextLike {
  readonly environmentId: string;
  readonly projectId: ProjectId;
}

export function useHandleNewThread(): {
  readonly activeDraftThread: ThreadContextLike | null;
  readonly activeThread: ThreadContextLike | undefined;
  readonly defaultProjectRef: ScopedProjectRef | null;
  readonly handleNewThread: (
    projectRef: ScopedProjectRef,
    options?: {
      branch?: string | null;
      worktreePath?: string | null;
      envMode?: "local" | "remote" | "worktree";
      startFromOrigin?: boolean;
    },
  ) => Promise<unknown>;
  readonly routeDraftId: string | null;
  readonly routeThreadRef: ScopedThreadRef | null;
} {
  const data = useSidebarData();
  const activeConversationId = data?.activeConversationId ?? null;
  const activeProjectId = data?.activeProjectId ?? null;
  const onNewChat = data?.onNewChat;

  const handleNewThread = React.useCallback(
    async (projectRef: ScopedProjectRef): Promise<unknown> => {
      // Shift-click reaches this path, and it is the one that means "create
      // in this project". The shell's action carries that, so the flag is the
      // signal rather than the project ref, which the route already scopes.
      void projectRef;
      onNewChat?.(true);
      return undefined;
    },
    [onNewChat],
  );

  const activeThread = React.useMemo(
    () =>
      activeConversationId === null || activeProjectId === null
        ? undefined
        : {
            environmentId: HUB_ENVIRONMENT_ID,
            projectId: activeProjectId as ProjectId,
          },
    [activeConversationId, activeProjectId],
  );

  const defaultProjectRef = React.useMemo(
    () =>
      activeProjectId === null
        ? (data?.projects[0]?.id ?? null) === null
          ? null
          : scopeProjectRef(HUB_ENVIRONMENT_ID, data!.projects[0]!.id as ProjectId)
        : scopeProjectRef(HUB_ENVIRONMENT_ID, activeProjectId as ProjectId),
    [activeProjectId, data],
  );

  const routeThreadRef = React.useMemo(
    () =>
      activeConversationId === null
        ? null
        : { environmentId: HUB_ENVIRONMENT_ID, threadId: activeConversationId as ThreadId },
    [activeConversationId],
  );

  return {
    // Detent has no draft sessions: an unsent composer lives on `/chat`, which
    // carries no project until the reader picks one in the hero headline.
    activeDraftThread: null,
    activeThread,
    defaultProjectRef,
    handleNewThread,
    routeDraftId: null,
    routeThreadRef,
  };
}
