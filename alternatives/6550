import { describe, expect, it } from "vitest";

import { mergeConversation } from "../src/runtime/state/conversationList.ts";
import { conversation } from "./components/builders.ts";

describe("conversation list stream updates", () => {
  it.each([
    { name: "user chat", origin: "user" as const, linked: false, listed: true },
    { name: "linked user chat", origin: "user" as const, linked: true, listed: true },
    { name: "worker chat", origin: "worker" as const, linked: true, listed: false },
    { name: "legacy user chat", origin: undefined, linked: true, listed: true },
  ])("keeps $name visibility after an issue stream update", ({ origin, linked, listed }) => {
    const manual = conversation({ id: "conv_manual", origin: "user" });
    const incoming = conversation({
      id: "conv_incoming",
      origin,
      work_item_id: linked ? "wi_issue" : null,
      revision: 2,
    });
    const merged = mergeConversation([manual], incoming);
    expect(merged.map((chat) => chat.id)).toContain(manual.id);
    expect(merged.some((chat) => chat.id === incoming.id)).toBe(listed);
  });

  it("removes an archived row without restoring it from later stream updates", () => {
    const archived = conversation({ id: "conv_archived", revision: 1 });
    const other = conversation({ id: "conv_other" });
    const merged = mergeConversation([archived, other], { ...archived, archived: true, revision: 2 });
    expect(merged.map((chat) => chat.id)).toEqual([other.id]);
    expect(mergeConversation(merged, { ...archived, archived: true, revision: 3 })).toEqual(merged);
  });

  it("removes a cached worker row when its conversation stream updates", () => {
    const manual = conversation({ id: "conv_manual", origin: "user" });
    const worker = conversation({ id: "conv_worker", origin: "worker", revision: 1 });
    const merged = mergeConversation([worker, manual], { ...worker, revision: 2 });
    expect(merged.map((chat) => chat.id)).toEqual([manual.id]);
  });
});
