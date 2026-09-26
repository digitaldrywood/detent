// Per-conversation drafts and outbox metadata.
//
// Written for this repository. Stored in `localStorage` under a key scoped by
// `account:project:conversation` so two projects, two conversations, or two
// accounts on the same browser never see each other's text. The store records
// which account wrote it: bootstrapping as a different actor clears everything
// (decisions.md §3 rule 8, "cached client state is cleared on logout or
// account change").
//
// Only two things are kept: the unsent draft text, and the outbox entry for a
// command whose outcome is unknown. The outbox is metadata, not a retry queue
// — nothing here is ever resent without the user asking.

export const DRAFT_STORAGE_PREFIX = "detent.conversation.draft";
export const OUTBOX_STORAGE_PREFIX = "detent.conversation.outbox";
export const ACTOR_STORAGE_KEY = "detent.conversation.actor";
/**
 * The last project the reader chose. It is private cached state like a draft —
 * it names a project this account can reach — so it is cleared with the rest
 * on an account change (decisions.md §3 rule 8).
 */
export const LAST_PROJECT_STORAGE_KEY = "detent.conversation.lastProject";

/** Per-entry cap. A draft longer than this is a paste accident, not prose. */
export const MAX_DRAFT_LENGTH = 32_000;
/** Total cap across every stored draft and outbox entry, in characters. */
export const MAX_STORED_CHARACTERS = 256_000;

export interface DraftScope {
  readonly accountKey: string;
  readonly projectId: string;
  /** `new` for the not-yet-created conversation on `/chat`. */
  readonly conversationId: string;
}

export interface OutboxEntry {
  readonly key: string;
  readonly kind: string;
  readonly text: string;
  readonly createdAt: string;
  readonly status: "unknown" | "failed";
  readonly error: string | null;
}

export interface DraftStorage {
  getItem: (key: string) => string | null;
  setItem: (key: string, value: string) => void;
  removeItem: (key: string) => void;
  key: (index: number) => string | null;
  readonly length: number;
}

function safeStorage(): DraftStorage | null {
  try {
    const candidate = globalThis.localStorage;
    if (candidate === undefined || candidate === null) return null;
    return candidate;
  } catch {
    // Storage can throw on access alone in a partitioned or blocked context.
    return null;
  }
}

export function scopeKey(prefix: string, scope: DraftScope): string {
  return `${prefix}:${scope.accountKey}:${scope.projectId}:${scope.conversationId}`;
}

export class DraftStore {
  constructor(private readonly storage: DraftStorage | null = safeStorage()) {}

  /**
   * Drops everything belonging to a different account. Called once with the
   * bootstrap actor key, before any draft is read.
   */
  reconcileAccount(accountKey: string): void {
    const storage = this.storage;
    if (storage === null) return;
    let previous: string | null = null;
    try {
      previous = storage.getItem(ACTOR_STORAGE_KEY);
    } catch {
      return;
    }
    if (previous === accountKey) return;
    this.clearAll();
    try {
      storage.setItem(ACTOR_STORAGE_KEY, accountKey);
    } catch {
      // A full or blocked store simply means drafts do not persist.
    }
  }

  clearAll(): void {
    const storage = this.storage;
    if (storage === null) return;
    const doomed: string[] = [];
    try {
      for (let index = 0; index < storage.length; index += 1) {
        const key = storage.key(index);
        if (key === null) continue;
        if (key.startsWith(DRAFT_STORAGE_PREFIX) || key.startsWith(OUTBOX_STORAGE_PREFIX)) {
          doomed.push(key);
        }
      }
      for (const key of doomed) storage.removeItem(key);
      storage.removeItem(ACTOR_STORAGE_KEY);
      storage.removeItem(LAST_PROJECT_STORAGE_KEY);
    } catch {
      // Nothing to do: a store that cannot be read cannot leak either.
    }
  }

  readDraft(scope: DraftScope): string {
    return this.read(scopeKey(DRAFT_STORAGE_PREFIX, scope)) ?? "";
  }

  writeDraft(scope: DraftScope, text: string): void {
    const key = scopeKey(DRAFT_STORAGE_PREFIX, scope);
    if (text.length === 0) {
      this.remove(key);
      return;
    }
    this.write(key, text.slice(0, MAX_DRAFT_LENGTH));
  }

  readOutbox(scope: DraftScope): OutboxEntry | null {
    const raw = this.read(scopeKey(OUTBOX_STORAGE_PREFIX, scope));
    if (raw === null) return null;
    try {
      const parsed = JSON.parse(raw) as OutboxEntry;
      return typeof parsed?.key === "string" ? parsed : null;
    } catch {
      return null;
    }
  }

  writeOutbox(scope: DraftScope, entry: OutboxEntry | null): void {
    const key = scopeKey(OUTBOX_STORAGE_PREFIX, scope);
    if (entry === null) {
      this.remove(key);
      return;
    }
    this.write(key, JSON.stringify(entry));
  }

  /** Total stored characters, used to enforce the bound. */
  storedSize(): number {
    const storage = this.storage;
    if (storage === null) return 0;
    let total = 0;
    try {
      for (let index = 0; index < storage.length; index += 1) {
        const key = storage.key(index);
        if (key === null) continue;
        if (!key.startsWith(DRAFT_STORAGE_PREFIX) && !key.startsWith(OUTBOX_STORAGE_PREFIX)) {
          continue;
        }
        total += (storage.getItem(key) ?? "").length + key.length;
      }
    } catch {
      return 0;
    }
    return total;
  }

  private read(key: string): string | null {
    try {
      return this.storage?.getItem(key) ?? null;
    } catch {
      return null;
    }
  }

  private write(key: string, value: string): void {
    const storage = this.storage;
    if (storage === null) return;
    try {
      const existing = (storage.getItem(key) ?? "").length;
      if (this.storedSize() - existing + value.length > MAX_STORED_CHARACTERS) {
        this.evictOldestDrafts(key);
      }
      storage.setItem(key, value);
    } catch {
      // Quota exceeded: drop the oldest drafts and give up quietly rather
      // than failing a send because a draft could not be cached.
      this.evictOldestDrafts(key);
      try {
        storage.setItem(key, value);
      } catch {
        /* the draft simply does not persist */
      }
    }
  }

  private remove(key: string): void {
    try {
      this.storage?.removeItem(key);
    } catch {
      /* nothing to remove */
    }
  }

  /** Drops draft keys other than `keep` until the store is under the bound. */
  private evictOldestDrafts(keep: string): void {
    const storage = this.storage;
    if (storage === null) return;
    try {
      const keys: string[] = [];
      for (let index = 0; index < storage.length; index += 1) {
        const key = storage.key(index);
        if (key === null || key === keep) continue;
        if (key.startsWith(DRAFT_STORAGE_PREFIX)) keys.push(key);
      }
      for (const key of keys) {
        if (this.storedSize() <= MAX_STORED_CHARACTERS / 2) break;
        storage.removeItem(key);
      }
    } catch {
      /* nothing to evict */
    }
  }
}
