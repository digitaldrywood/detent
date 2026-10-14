import React from "react";

import { HUB_ENVIRONMENT_ID } from "../contracts/index.ts";
import type { EnvironmentId, ScopedThreadRef } from "../contracts/index.ts";
import type { EnvironmentProject, EnvironmentThreadShell } from "../app/adapters/models.ts";
import { toEnvironmentProject } from "../app/adapters/shell.ts";
import { toEnvironmentThreadShell } from "../app/adapters/sidebarThreads.ts";
import { readSidebarData, useSidebarData } from "../app/adapters/sidebarData.tsx";
import { useSettledOverrideStore } from "../app/adapters/settledOverrides.ts";
import { DETENT_SERVER_CONFIG, environmentServerConfigsAtom, type ServerConfig } from "./server.ts";
import { useAtomValue } from "@effect/atom-react";

const NO_PROJECTS: readonly EnvironmentProject[] = [];
const NO_THREADS: readonly EnvironmentThreadShell[] = [];

export function useProjects(): readonly EnvironmentProject[] {
  const data = useSidebarData();
  const projects = data?.projects;
  return React.useMemo(
    () => (projects === undefined ? NO_PROJECTS : projects.map(toEnvironmentProject)),
    [projects],
  );
}

export function useServerConfigs(): ReadonlyMap<EnvironmentId, ServerConfig> {
  return useAtomValue(environmentServerConfigsAtom);
}

export function useThreadShells(): readonly EnvironmentThreadShell[] {
  const data = useSidebarData();
  const conversations = data?.conversations;
  const serverResults = data?.serverResults;
  const attention = data?.attention;
  // The reader's own Settle and Un-settle. Subscribed here, not read at call
  // time, so clicking the row's settle button moves it to the shelf on the
  // same commit (`app/adapters/settledOverrides.ts`).
  const settledAtById = useSettledOverrideStore((store) => store.settledAtById);
  const unsettledAtById = useSettledOverrideStore((store) => store.unsettledAtById);
  return React.useMemo(() => {
    if (conversations === undefined) return NO_THREADS;
    const seen = new Set(conversations.map((conversation) => conversation.id));
    const merged = [
      ...conversations,
      ...(serverResults ?? []).filter((conversation) => !seen.has(conversation.id)),
    ];
    const settledOverrides = { settledAtById, unsettledAtById };
    return merged.map((conversation) =>
      toEnvironmentThreadShell(conversation, { attention, settledOverrides }),
    );
  }, [attention, conversations, serverResults, settledAtById, unsettledAtById]);
}

export function useAllEnvironmentProjectSnapshotsReady(): boolean {
  return useSidebarData() !== null;
}

export function useAllEnvironmentShellsBootstrapped(): boolean {
  return useSidebarData() !== null;
}

/** Read outside React, for the delete path's post-hoc existence check. */
export function readThreadShell(ref: ScopedThreadRef): EnvironmentThreadShell | null {
  const data = readSidebarData();
  if (data === null || ref.environmentId !== HUB_ENVIRONMENT_ID) return null;
  const conversation = data.conversations.find(
    (candidate) => candidate.id === (ref.threadId as string),
  );
  return conversation === undefined
    ? null
    : toEnvironmentThreadShell(conversation, {
        attention: data.attention,
        settledOverrides: useSettledOverrideStore.getState(),
      });
}

export function readThreadShells(): readonly EnvironmentThreadShell[] {
  const data = readSidebarData();
  if (data === null) return NO_THREADS;
  const settledOverrides = useSettledOverrideStore.getState();
  return data.conversations.map((conversation) =>
    toEnvironmentThreadShell(conversation, { attention: data.attention, settledOverrides }),
  );
}

export function readEnvironmentSupportsSettlement(environmentId: EnvironmentId): boolean {
  return (
    environmentId === HUB_ENVIRONMENT_ID &&
    DETENT_SERVER_CONFIG.environment.capabilities.threadSettlement
  );
}
