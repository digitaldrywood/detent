// Draft and outbox storage: scoping, bounds, and the clear-on-account-change
// rule from decisions.md §3 rule 8.
import { describe, expect, it } from "vitest";

import {
  DraftStore,
  LAST_PROJECT_STORAGE_KEY,
  MAX_STORED_CHARACTERS,
  type DraftScope,
  type DraftStorage,
} from "../src/runtime/state/drafts.ts";

function fakeStorage(): DraftStorage {
  const map = new Map<string, string>();
  return {
    getItem: (key) => map.get(key) ?? null,
    setItem: (key, value) => {
      map.set(key, value);
    },
    removeItem: (key) => {
      map.delete(key);
    },
    key: (index) => [...map.keys()][index] ?? null,
    get length() {
      return map.size;
    },
  };
}

const scope = (conversationId: string, projectId = "proj_alpha"): DraftScope => ({
  accountKey: "org_1:tok_1",
  projectId,
  conversationId,
});

describe("drafts", () => {
  it("keeps drafts separate per conversation and per project", () => {
    const store = new DraftStore(fakeStorage());
    store.reconcileAccount("org_1:tok_1");
    store.writeDraft(scope("conv_a"), "first");
    store.writeDraft(scope("conv_b"), "second");
    store.writeDraft(scope("conv_a", "proj_beta"), "third");

    expect(store.readDraft(scope("conv_a"))).toBe("first");
    expect(store.readDraft(scope("conv_b"))).toBe("second");
    expect(store.readDraft(scope("conv_a", "proj_beta"))).toBe("third");
    expect(store.readDraft(scope("conv_c"))).toBe("");
  });

  it("clears an empty draft rather than storing it", () => {
    const store = new DraftStore(fakeStorage());
    store.writeDraft(scope("conv_a"), "text");
    store.writeDraft(scope("conv_a"), "");
    expect(store.readDraft(scope("conv_a"))).toBe("");
  });

  it("round-trips an outbox entry and clears it on request", () => {
    const store = new DraftStore(fakeStorage());
    store.writeOutbox(scope("conv_a"), {
      key: "cmd_1",
      kind: "message",
      text: "unsure",
      createdAt: "2026-09-09T10:00:00Z",
      status: "unknown",
      error: null,
    });
    expect(store.readOutbox(scope("conv_a"))?.key).toBe("cmd_1");
    store.writeOutbox(scope("conv_a"), null);
    expect(store.readOutbox(scope("conv_a"))).toBeNull();
  });

  it("drops every stored draft when a different account bootstraps", () => {
    const storage = fakeStorage();
    const store = new DraftStore(storage);
    store.reconcileAccount("org_1:tok_1");
    store.writeDraft(scope("conv_a"), "private text");
    store.writeOutbox(scope("conv_a"), {
      key: "cmd_1",
      kind: "message",
      text: "private text",
      createdAt: "2026-09-09T10:00:00Z",
      status: "unknown",
      error: null,
    });

    const second = new DraftStore(storage);
    second.reconcileAccount("org_1:tok_2");

    expect(second.readDraft(scope("conv_a"))).toBe("");
    expect(second.readOutbox(scope("conv_a"))).toBeNull();
    expect(storage.length).toBe(1); // only the new actor marker
  });

  it("stays under the size bound by evicting other drafts", () => {
    const store = new DraftStore(fakeStorage());
    store.reconcileAccount("org_1:tok_1");
    for (let index = 0; index < 12; index += 1) {
      store.writeDraft(scope(`conv_${index}`), "x".repeat(30_000));
    }
    expect(store.storedSize()).toBeLessThanOrEqual(MAX_STORED_CHARACTERS);
    expect(store.readDraft(scope("conv_11"))).toHaveLength(30_000);
  });

  it("survives a storage that throws on every access", () => {
    const hostile: DraftStorage = {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {
        throw new Error("blocked");
      },
      removeItem: () => {
        throw new Error("blocked");
      },
      key: () => {
        throw new Error("blocked");
      },
      get length(): number {
        throw new Error("blocked");
      },
    };
    const store = new DraftStore(hostile);
    expect(() => store.reconcileAccount("org_1:tok_1")).not.toThrow();
    expect(() => store.writeDraft(scope("conv_a"), "text")).not.toThrow();
    expect(store.readDraft(scope("conv_a"))).toBe("");
  });

  // The remembered project names a project the previous account could reach.
  // It is private cached state like a draft and goes with the rest.
  it("clears the remembered project when the account changes", () => {
    const storage = fakeStorage();
    const store = new DraftStore(storage);
    store.reconcileAccount("org_1:tok_1");
    storage.setItem(LAST_PROJECT_STORAGE_KEY, "proj_alpha");
    store.writeDraft(scope("conv_a"), "private text");

    new DraftStore(storage).reconcileAccount("org_2:tok_2");
    expect(storage.getItem(LAST_PROJECT_STORAGE_KEY)).toBeNull();
    expect(new DraftStore(storage).readDraft(scope("conv_a"))).toBe("");
  });
});
