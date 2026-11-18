import type { ProjectId, ThreadId } from "../contracts/ui.ts";

export interface ScopedProjectRef {
  readonly environmentId: string;
  readonly projectId: ProjectId;
}

export interface ScopedThreadRef {
  readonly environmentId: string;
  readonly threadId: ThreadId;
}

export function scopeProjectRef(environmentId: string, projectId: ProjectId): ScopedProjectRef {
  return { environmentId, projectId };
}

export function scopeThreadRef(environmentId: string, threadId: ThreadId): ScopedThreadRef {
  return { environmentId, threadId };
}

function scopedRefKey(ref: ScopedProjectRef | ScopedThreadRef): string {
  const localId = "projectId" in ref ? ref.projectId : ref.threadId;
  return `${ref.environmentId}:${localId}`;
}

export function scopedProjectKey(ref: ScopedProjectRef): string {
  return scopedRefKey(ref);
}

export function scopedThreadKey(ref: ScopedThreadRef): string {
  return scopedRefKey(ref);
}

function parseScopedKey(key: string): { environmentId: string; localId: string } | null {
  const separatorIndex = key.indexOf(":");
  if (separatorIndex <= 0 || separatorIndex >= key.length - 1) {
    return null;
  }
  return {
    environmentId: key.slice(0, separatorIndex),
    localId: key.slice(separatorIndex + 1),
  };
}

export function parseScopedProjectKey(key: string): ScopedProjectRef | null {
  const parsed = parseScopedKey(key);
  if (!parsed) return null;
  return { environmentId: parsed.environmentId, projectId: parsed.localId as ProjectId };
}

export function parseScopedThreadKey(key: string): ScopedThreadRef | null {
  const parsed = parseScopedKey(key);
  if (!parsed) return null;
  return { environmentId: parsed.environmentId, threadId: parsed.localId as ThreadId };
}
