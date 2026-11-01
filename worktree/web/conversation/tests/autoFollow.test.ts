// The transcript signature the auto-follow effect watches.
import { describe, expect, it } from "vitest";

import { transcriptSignature } from "../src/app/lib/autoFollow.ts";
import type { ConversationDetail } from "../src/runtime/state/conversationState.ts";

function detail(overrides: Partial<ConversationDetail>): ConversationDetail {
  return {
    conversation: {} as ConversationDetail["conversation"],
    messages: [],
    deltas: {},
    questions: [],
    receipts: {},
    pending: [],
    controls: [],
    staleExecution: false,
    page: { hasMore: false, loadingOlder: false, oldestSeq: null },
    ...overrides,
  } as ConversationDetail;
}

const message = (id: string, text: string) =>
  ({ id, text, role: "assistant" }) as ConversationDetail["messages"][number];

describe("transcriptSignature", () => {
  // Counting open delta buffers pinned this at one for a whole streaming
  // reply, so the view stopped following exactly while the text grew.
  it("changes as a streamed reply grows", () => {
    const first = transcriptSignature(
      detail({ messages: [message("msg_1", "")], deltas: { msg_1: { parts: { 1: "half" } } } }),
    );
    const second = transcriptSignature(
      detail({
        messages: [message("msg_1", "")],
        deltas: { msg_1: { parts: { 1: "half", 2: " and the rest" } } },
      }),
    );
    expect(second).toBeGreaterThan(first);
  });

  it("changes when the final text replaces the deltas", () => {
    const streaming = transcriptSignature(
      detail({ messages: [message("msg_1", "")], deltas: { msg_1: { parts: { 1: "done" } } } }),
    );
    const settled = transcriptSignature(detail({ messages: [message("msg_1", "done!")] }));
    expect(settled).not.toBe(streaming);
  });

  it("counts a new message", () => {
    const one = transcriptSignature(detail({ messages: [message("msg_1", "a")] }));
    const two = transcriptSignature(
      detail({ messages: [message("msg_1", "a"), message("msg_2", "b")] }),
    );
    expect(two).toBeGreaterThan(one);
  });
});
