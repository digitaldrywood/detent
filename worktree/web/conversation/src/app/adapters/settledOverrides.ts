import { create } from "zustand";
import { persist } from "zustand/middleware";

const STORAGE_KEY = "detent:conversation:settled-overrides";

export interface SettledOverrideState {
  /** Conversation id → the ISO time the reader settled or un-settled it. */
  readonly settledAtById: Readonly<Record<string, string>>;
  readonly unsettledAtById: Readonly<Record<string, string>>;
  readonly settle: (conversationId: string, at: string) => void;
  readonly unsettle: (conversationId: string, at: string) => void;
}

export const useSettledOverrideStore = create<SettledOverrideState>()(
  persist(
    (set) => ({
      settledAtById: {},
      unsettledAtById: {},
      settle: (conversationId, at) =>
        set((state) => {
          const unsettledAtById = { ...state.unsettledAtById };
          delete unsettledAtById[conversationId];
          return {
            settledAtById: { ...state.settledAtById, [conversationId]: at },
            unsettledAtById,
          };
        }),
      unsettle: (conversationId, at) =>
        set((state) => {
          const settledAtById = { ...state.settledAtById };
          delete settledAtById[conversationId];
          return {
            settledAtById,
            unsettledAtById: { ...state.unsettledAtById, [conversationId]: at },
          };
        }),
    }),
    { name: STORAGE_KEY },
  ),
);

export interface SettledOverrides {
  readonly settledAtById: Readonly<Record<string, string>>;
  readonly unsettledAtById: Readonly<Record<string, string>>;
}

/**
 * Which shelf a conversation sits on, once the reader's override is taken
 * into account.
 *
 * An override only counts while it is newer than the conversation's own last
 * activity: a settled conversation that receives a message is active again,
 * which is §14's "unsettles on new activity", and a conversation the hub has
 * settled is settled whatever the reader last clicked.
 */
export function resolveSettledOverride(
  conversation: { readonly id: string; readonly status: string; readonly updated_at: string },
  overrides: SettledOverrides,
): "settled" | "active" | null {
  const activityMs = Date.parse(conversation.updated_at);
  const settledAt = overrides.settledAtById[conversation.id];
  if (settledAt !== undefined && !(Date.parse(settledAt) < activityMs)) return "settled";
  const unsettledAt = overrides.unsettledAtById[conversation.id];
  if (unsettledAt !== undefined && !(Date.parse(unsettledAt) < activityMs)) return "active";
  return conversation.status === "settled" ? "settled" : null;
}
