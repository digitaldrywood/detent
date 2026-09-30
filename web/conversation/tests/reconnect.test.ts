// Reconnect behaviour: the stream resumes from the cursor the client tracked,
// and replayed frames never duplicate what is already in the transcript.
import * as Option from "effect/Option";
import { afterEach, describe, expect, it } from "vitest";

import type { Result } from "effect/Result";

import { makeHarness, PROJECT, type Harness } from "./harness.ts";
import type { CreateConversationResponse } from "../src/contracts/index.ts";

let harness: Harness | undefined;

afterEach(async () => {
  await harness?.dispose();
  harness = undefined;
});

describe("reconnect", () => {
  it("resumes from the cursor without duplicating messages", async () => {
    const h = (harness = await makeHarness());
    const created = (await h.run(
      h.client.effects.createConversation({
        projectId: PROJECT,
        key: "cmd_reconnect_create",
        firstMessage: { key: "cmd_reconnect_1", text: "Hold this line" },
      }),
    )) as Result<typeof CreateConversationResponse.Type, unknown>;
    if (created._tag !== "Success") throw new Error("Creating the conversation failed.");
    const conversationId = created.success.conversation.id;

    const atom = h.client.conversations.stateAtom({
      environmentId: "hub",
      projectId: PROJECT,
      conversationId,
    });
    h.mount(atom);
    const before = await h.waitFor(
      atom,
      (value) =>
        value.status === "live" &&
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.messages.some((message) => message.role === "assistant"),
        }),
      "the first reply",
    );
    const beforeIds = Option.getOrThrow(before.data).messages.map((message) => message.id);

    // Kill the open stream. The client reconnects and resubscribes with its
    // own cursor, so the hub replays only what came after it.
    await h.control("drop-open-streams");

    await h.run(
      h.client.effects.sendMessage({
        projectId: PROJECT,
        conversationId,
        key: "cmd_reconnect_2",
        text: "Still here?",
      }),
    );

    const after = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.messages.some((message) => message.text === "Still here?"),
        }),
      "the message sent after the drop",
    );
    const detail = Option.getOrThrow(after.data);
    const ids = detail.messages.map((message) => message.id);
    expect(new Set(ids).size).toBe(ids.length);
    for (const id of beforeIds) expect(ids).toContain(id);
    expect(detail.messages.filter((message) => message.text === "Still here?")).toHaveLength(1);
  });
});
