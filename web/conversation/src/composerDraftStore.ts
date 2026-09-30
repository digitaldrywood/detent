import { create } from "zustand";

import type { ScopedThreadRef } from "./environment/scoped.ts";
import type { EnvironmentId } from "./contracts/index.ts";
import type { ChatFileAttachment, ProjectId, ThreadId } from "./contracts/ui.ts";

export type DraftId = string & { readonly DraftId?: unique symbol };
export const DraftId = { make: (value: string): DraftId => value };

export interface ComposerThreadDraftState {
  readonly prompt: string;
  readonly images: readonly unknown[];
  readonly files: readonly unknown[];
  readonly persistedAttachments: readonly unknown[];
  readonly terminalContexts: readonly unknown[];
  readonly elementContexts: readonly unknown[];
  readonly previewAnnotations: readonly unknown[];
  readonly reviewComments: readonly unknown[];
}

export interface ComposerFileAttachment extends ChatFileAttachment {
  readonly file: File | null;
  readonly uploadedAttachmentId?: string;
  readonly uploadEnvironmentId?: EnvironmentId;
}

export interface DraftSessionState {
  readonly threadId: ThreadId;
  readonly environmentId: EnvironmentId;
  readonly projectId: ProjectId;
  readonly createdAt: string;
  /** Set once the draft became a server thread. */
  readonly promotedTo?: ScopedThreadRef | null;
}

export function composerDraftHasUserContent(
  draft: ComposerThreadDraftState | undefined,
): boolean {
  if (!draft) return false;
  return (
    draft.prompt.trim().length > 0 ||
    draft.images.length > 0 ||
    draft.files.length > 0 ||
    draft.persistedAttachments.length > 0 ||
    draft.terminalContexts.length > 0 ||
    draft.elementContexts.length > 0 ||
    draft.previewAnnotations.length > 0 ||
    draft.reviewComments.length > 0
  );
}

export interface ComposerDraftStore {
  readonly draftThreadsByThreadKey: Readonly<Record<string, DraftSessionState>>;
  readonly draftsByThreadKey: Readonly<Record<string, ComposerThreadDraftState>>;
  readonly getDraftSession: (draftId: DraftId) => DraftSessionState | null;
  readonly getComposerDraft: (
    target: DraftId | ScopedThreadRef,
  ) => ComposerThreadDraftState | null;
  readonly clearDraftThread: (draftId: DraftId) => void;
  readonly clearComposerContent: (target: DraftId | ScopedThreadRef) => void;
}

export const useComposerDraftStore = create<ComposerDraftStore>()(() => ({
  draftThreadsByThreadKey: {},
  draftsByThreadKey: {},
  getDraftSession: () => null,
  getComposerDraft: () => null,
  clearDraftThread: () => {},
  clearComposerContent: () => {},
}));

export function useThreadHasUnsentDraft(_threadRef: ScopedThreadRef): boolean {
  return false;
}
