// Handoff (U06) against the mock hub. No DOM: this is the runtime contract.
//
// The three rules under test are decisions.md §3: the link key is an
// idempotency key, a second link is refused with the conversation to navigate
// to instead, and a link without an explicit share confirmation is refused.
import * as Option from "effect/Option";
import { afterEach, describe, expect, it } from "vitest";

import type { Result } from "effect/Result";

import { makeHarness, PROJECT, type Harness } from "./harness.ts";
import {
  type CreateConversationResponse,
  readAlreadyLinked,
  readIssueResult,
} from "../src/contracts/index.ts";

let harness: Harness | undefined;

afterEach(async () => {
  await harness?.dispose();
  harness = undefined;
});

async function openLinked(text = "Why does the lock lapse?") {
  const h = (harness = await makeHarness());
  const created = (await h.run(
    h.client.effects.createConversation({
      projectId: PROJECT,
      key: "cmd_link_create",
      firstMessage: { key: "cmd_link_seed", text },
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
  await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");
  return { h, conversationId, atom };
}

const ISSUE = {
  title: "Checkout lock renewal waits on a healthy handoff",
  description: "Move the renewal behind the handoff acknowledgement.",
};

describe("handoff", () => {
  it("links once and returns the stored result when the key is reused", async () => {
    const { h, conversationId, atom } = await openLinked();

    const first = await h.run(
      h.client.effects.link({
        projectId: PROJECT,
        conversationId,
        key: "cmd_link_1",
        shareHistory: true,
        issue: ISSUE,
      }),
    );
    if (first._tag !== "Success") throw new Error("Linking failed.");
    expect(first.success.conversation.work_item_id).not.toBeNull();
    expect(first.success.conversation.visibility).toBe("shared");
    expect(first.success.scheduling.runner_bound).toBe(false);

    // The same intent, retried: the key is the idempotency key, so the hub
    // returns the same issue rather than creating a second one.
    const again = await h.run(
      h.client.effects.link({
        projectId: PROJECT,
        conversationId,
        key: "cmd_link_1",
        shareHistory: true,
        issue: ISSUE,
      }),
    );
    if (again._tag !== "Success") throw new Error("The retry failed.");
    expect(again.success.issue.id).toBe(first.success.issue.id);

    // The route and the history did not change; the resource did.
    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.conversation.work_item_id !== null,
        }),
      "the linked conversation",
    );
    const detail = Option.getOrThrow(state.data);
    expect(detail.conversation.visibility).toBe("shared");
    expect(detail.messages.some((message) => message.text === "Why does the lock lapse?")).toBe(
      true,
    );
    const card = detail.messages
      .map((message) => readIssueResult(message.data))
      .find((issue) => issue !== undefined);
    expect(card?.identifier).toBe(first.success.issue.identifier);
    expect(card?.runner_bound).toBe(false);
  });

  it("reports the existing conversation instead of linking twice", async () => {
    const { h, conversationId } = await openLinked();
    await h.run(
      h.client.effects.link({
        projectId: PROJECT,
        conversationId,
        key: "cmd_link_1",
        shareHistory: true,
        issue: ISSUE,
      }),
    );

    const second = await h.run(
      h.client.effects.link({
        projectId: PROJECT,
        conversationId,
        key: "cmd_link_2",
        shareHistory: true,
        issue: ISSUE,
      }),
    );
    if (second._tag === "Success") throw new Error("The second link should have been refused.");
    const failure = second.failure;
    if (failure._tag !== "ApiRequestError") throw new Error("Expected an API failure.");
    expect(failure.code).toBe("conversation_already_linked");
    // The client can navigate rather than duplicating (decisions.md §3.4).
    expect(readAlreadyLinked(failure.details)?.existing_conversation_id).toBe(conversationId);
  });

  it("refuses to link without an explicit share confirmation", async () => {
    const { h, conversationId } = await openLinked();
    const result = await h.run(
      h.client.effects.link({
        projectId: PROJECT,
        conversationId,
        key: "cmd_link_unconfirmed",
        shareHistory: false,
        issue: ISSUE,
      }),
    );
    if (result._tag === "Success") throw new Error("An unconfirmed link should be refused.");
    if (result.failure._tag !== "ApiRequestError") throw new Error("Expected an API failure.");
    expect(result.failure.code).toBe("share_history_required");
  });

  it("proposes an issue from the coordinator when the user asks for one", async () => {
    const { h, conversationId, atom } = await openLinked("Please create an issue for this");
    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.messages.some(
              (message) => (message.data as { proposal?: unknown }).proposal !== undefined,
            ),
        }),
      "the proposal message",
    );
    const detail = Option.getOrThrow(state.data);
    const proposal = detail.messages
      .map((message) => (message.data as { proposal?: { project_id: string } }).proposal)
      .find((value) => value !== undefined);
    expect(proposal?.project_id).toBe(PROJECT);
    expect(conversationId).toMatch(/^conv_/);
  });
});
