import * as Effect from "effect/Effect";
import React from "react";

import {
  ATTACHMENT_MAX_BYTES,
  ATTACHMENT_MAX_PER_MESSAGE,
  attachmentTypeAccepted,
} from "../../contracts/index.ts";
import { toastManager } from "../../components/ui/toast.tsx";
import { fileAttachmentTooLargeMessage } from "../../runtime/state/attachments.ts";
import type { ConversationClient } from "../../runtime/bootstrap.ts";

/** One file staged in the composer, before or after its upload landed. */
export interface StagedAttachment {
  /** Stable while the chip is on screen; the hub's id arrives later. */
  readonly localId: string;
  readonly name: string;
  readonly mime: string;
  readonly size: number;

  readonly previewUrl: string | null;

  readonly status: "staged" | "uploading" | "ready" | "failed";
  /** The hub's attachment id, once `201` named it. */
  readonly id: string | null;
  readonly error: string | null;
}

export interface ComposerAttachments {
  readonly staged: readonly StagedAttachment[];

  readonly addFiles: (files: readonly File[]) => void;
  readonly remove: (localId: string) => void;
  readonly retry: (localId: string) => void;
  /** Drops every chip. Called once the hub has accepted the message. */
  readonly clear: () => void;
  /** The ids the `message` command carries. */
  readonly ids: readonly string[];
  /**
   * Uploads everything still staged against a conversation that now exists
   * and returns every id in chip order, or null when any upload failed (the
   * chips then show the failure and the draft stays).
   */
  readonly uploadAll: (conversationId: string) => Promise<readonly string[] | null>;
  /** True while any upload is in flight: the send waits for it. */
  readonly uploading: boolean;
  /** True when at least one upload failed: the send says so rather than silently dropping it. */
  readonly failed: boolean;
}

/** The empty controller, for a surface with no conversation to upload against. */
export const NO_ATTACHMENTS: ComposerAttachments = {
  staged: [],
  addFiles: () => {},
  remove: () => {},
  retry: () => {},
  clear: () => {},
  ids: [],
  uploadAll: async () => [],
  uploading: false,
  failed: false,
};

function makeLocalId(): string {
  const uuid = globalThis.crypto?.randomUUID?.();
  return uuid ?? `att_${Date.now()}_${Math.random().toString(16).slice(2)}`;
}

function previewFor(file: File): string | null {
  if (!file.type.toLowerCase().startsWith("image/")) return null;
  const create = globalThis.URL?.createObjectURL;
  if (typeof create !== "function") return null;
  try {
    return create.call(globalThis.URL, file);
  } catch {
    return null;
  }
}

function revoke(url: string | null): void {
  if (url === null) return;
  try {
    globalThis.URL?.revokeObjectURL?.(url);
  } catch {
    // A browser that refuses to revoke is not a reason to lose a chip.
  }
}

interface Pending {
  readonly entry: StagedAttachment;
  readonly file: File;
}

/**
 * The composer's attachment state for one conversation. `conversationId` is
 * null on a surface with no conversation yet (the new-chat hero), and the
 * controller is then inert: §17.1 stages a file against a conversation, so
 * there is nothing to upload to until the first send has made one.
 */
export function useComposerAttachments(
  client: ConversationClient,
  input: { readonly projectId: string; readonly conversationId: string | null },
): ComposerAttachments {
  const [staged, setStaged] = React.useState<readonly StagedAttachment[]>([]);
  const files = React.useRef(new Map<string, File>());
  const { projectId, conversationId } = input;

  // A conversation change abandons whatever was staged for the previous one:
  // an attachment belongs to the conversation it was uploaded against.
  React.useEffect(() => {
    return () => {
      for (const entry of staged) revoke(entry.previewUrl);
      files.current.clear();
    };
    // Only the conversation identity resets the stage.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conversationId]);

  // One upload against one conversation, resolving to the hub's id or null.
  const uploadTo = React.useCallback(
    async (target: string, localId: string, file: File): Promise<string | null> => {
      setStaged((current) =>
        current.map((entry) =>
          entry.localId === localId ? { ...entry, status: "uploading" as const, error: null } : entry,
        ),
      );
      const result = await Effect.runPromise(
        Effect.result(
          client.http.uploadAttachment({ projectId, conversationId: target, key: localId, file }),
        ),
      );
      setStaged((current) =>
        current.map((entry) =>
          entry.localId === localId
            ? result._tag === "Success"
              ? { ...entry, status: "ready" as const, id: result.success.id, error: null }
              : {
                  ...entry,
                  status: "failed" as const,
                  error: result.failure.message.length > 0
                    ? result.failure.message
                    : "The upload failed.",
                }
            : entry,
        ),
      );
      return result._tag === "Success" ? result.success.id : null;
    },
    [client, projectId],
  );

  const upload = React.useCallback(
    (localId: string, file: File) => {
      if (conversationId === null) return;
      void uploadTo(conversationId, localId, file);
    },
    [conversationId, uploadTo],
  );

  const addFiles = React.useCallback(
    (incoming: readonly File[]) => {
      if (incoming.length === 0) return;

      const accepted: Pending[] = [];
      let reserved = staged.length;
      let refusal: string | null = null;
      for (const file of incoming) {
        if (reserved >= ATTACHMENT_MAX_PER_MESSAGE) {
          refusal = `You can attach up to ${ATTACHMENT_MAX_PER_MESSAGE} files per message.`;
          continue;
        }
        if (file.size <= 0) {
          refusal = `'${file.name}' is empty or could not be read.`;
          continue;
        }
        if (file.size > ATTACHMENT_MAX_BYTES) {
          refusal = fileAttachmentTooLargeMessage(file.name, ATTACHMENT_MAX_BYTES);
          continue;
        }
        if (!attachmentTypeAccepted(file.type, file.name)) {
          refusal = `'${file.name}' is not a supported attachment type. Attach an image or a text file.`;
          continue;
        }
        const localId = makeLocalId();
        accepted.push({
          file,
          entry: {
            localId,
            name: file.name.length > 0 ? file.name : "file",
            mime: file.type,
            size: file.size,
            previewUrl: previewFor(file),
            // Before the conversation exists the file waits in the draft.
            status: conversationId === null ? "staged" : "uploading",
            id: null,
            error: null,
          },
        });
        reserved += 1;
      }
      if (refusal !== null) toastManager.add({ type: "error", title: refusal });
      if (accepted.length === 0) return;
      for (const pending of accepted) files.current.set(pending.entry.localId, pending.file);
      setStaged((current) => [...current, ...accepted.map((pending) => pending.entry)]);
      if (conversationId !== null) {
        for (const pending of accepted) upload(pending.entry.localId, pending.file);
      }
    },
    [conversationId, staged.length, upload],
  );

  const remove = React.useCallback(
    (localId: string) => {
      const entry = staged.find((candidate) => candidate.localId === localId);
      revoke(entry?.previewUrl ?? null);
      files.current.delete(localId);
      setStaged((current) => current.filter((candidate) => candidate.localId !== localId));
      // An upload that already landed is released on the hub too, so a file the
      // reader changed their mind about does not outlive the draft (§17.1).
      if (entry?.id != null && conversationId !== null) {
        // Nothing is reported if it fails: the chip is gone and the hub
        // expires an unsent attachment anyway.
        void Effect.runPromise(
          Effect.result(
            client.http.deleteAttachment({
              projectId,
              conversationId,
              attachmentId: entry.id,
            }),
          ),
        );
      }
    },
    [client, conversationId, projectId, staged],
  );

  const retry = React.useCallback(
    (localId: string) => {
      const file = files.current.get(localId);
      if (file === undefined) return;
      if (conversationId === null) {
        setStaged((current) =>
          current.map((entry) =>
            entry.localId === localId ? { ...entry, status: "staged" as const, error: null } : entry,
          ),
        );
        return;
      }
      upload(localId, file);
    },
    [conversationId, upload],
  );

  const clear = React.useCallback(() => {
    setStaged((current) => {
      for (const entry of current) revoke(entry.previewUrl);
      return [];
    });
    files.current.clear();
  }, []);

  const uploadAll = React.useCallback(
    async (target: string): Promise<readonly string[] | null> => {
      const ids: string[] = [];
      let failed = false;
      for (const entry of staged) {
        if (entry.status === "ready" && entry.id !== null) {
          ids.push(entry.id);
          continue;
        }
        const file = files.current.get(entry.localId);
        if (file === undefined) {
          failed = true;
          continue;
        }
        const id = await uploadTo(target, entry.localId, file);
        if (id === null) failed = true;
        else ids.push(id);
      }
      return failed ? null : ids;
    },
    [staged, uploadTo],
  );

  const ids = React.useMemo(
    () =>
      staged
        .filter((entry) => entry.status === "ready" && entry.id !== null)
        .map((entry) => entry.id as string),
    [staged],
  );

  return {
    staged,
    addFiles,
    remove,
    retry,
    clear,
    ids,
    uploadAll,
    uploading: staged.some((entry) => entry.status === "uploading"),
    failed: staged.some((entry) => entry.status === "failed"),
  };
}
