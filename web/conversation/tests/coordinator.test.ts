// The runner-dispatched coordinator (decisions.md §1 "Where model turns
// execute" and §9).
//
// The client half of that decision is small and easy to get wrong: an unlinked
// chat is no longer answered in place, so its first message is `queued` rather
// than `delivered`, its execution reports `waiting_for_runner` until a runner
// claims the coordinator work item, and `cancel` is an interrupt aimed at that
// runner. These drive the whole ladder against the mock hub in its `runner`
// mode; no DOM is involved.
import * as Option from "effect/Option";
import { afterEach, describe, expect, it } from "vitest";

import type { Result } from "effect/Result";

import { makeHarness, PROJECT, type Harness } from "./harness.ts";
import type { CreateConversationResponse, ExecutionStatus } from "../src/contracts/index.ts";
import { projectCoordinatorAvailable } from "../src/contracts/index.ts";
import type { ConversationDetail } from "../src/runtime/state/conversationState.ts";

let harness: Harness | undefined;

afterEach(async () => {
  await harness?.dispose();
  harness = undefined;
});

function atomFor(h: Harness, conversationId: string) {
  return h.client.conversations.stateAtom({
    environmentId: "hub",
    projectId: PROJECT,
    conversationId,
  });
}

function detailOf(h: Harness, conversationId: string): ConversationDetail {
  const state = h.read(atomFor(h, conversationId));
  if (state === undefined) throw new Error("The conversation is not open.");
  return Option.getOrThrow(state.data);
}

/** Opens an unlinked chat whose first message is queued for a runner. */
async function openQueuedChat(text = "Why does the lease lapse?") {
  const h = (harness = await makeHarness({ coordinator: "runner" }));
  const created = (await h.run(
    h.client.effects.createConversation({
      projectId: PROJECT,
      key: "cmd_coordinator_create",
      firstMessage: { key: "cmd_coordinator_seed", text },
    }),
  )) as Result<typeof CreateConversationResponse.Type, unknown>;
  if (created._tag !== "Success") throw new Error("Creating the conversation failed.");
  const conversationId = created.success.conversation.id;
  const atom = atomFor(h, conversationId);
  h.mount(atom);
  await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");
  return { h, conversationId, atom, receipt: created.success.receipt };
}

function awaitStatus(h: Harness, conversationId: string, status: ExecutionStatus) {
  return h.waitFor(
    atomFor(h, conversationId),
    (value) =>
      Option.match(value.data, {
        onNone: () => false,
        onSome: (detail) => detail.conversation.execution.status === status,
      }),
    `execution ${status}`,
  );
}

describe("the runner-dispatched coordinator", () => {
  it("queues an unlinked message, waits for a runner, then runs and completes", async () => {
    const { h, conversationId, receipt } = await openQueuedChat();

    // The hub creates the coordinator work item and waits: the message is
    // accepted and held, not delivered to anything (decisions.md §9.1).
    expect(receipt?.status).toBe("queued");
    await awaitStatus(h, conversationId, "waiting_for_runner");
    expect(detailOf(h, conversationId).conversation.work_item_id).toBeNull();
    expect(
      detailOf(h, conversationId).messages.find((message) => message.role === "user")?.delivery,
    ).toBe("queued");

    // A runner claims the item and binds a coordinator attempt.
    await h.control(`runner/${conversationId}/start`);
    const running = await awaitStatus(h, conversationId, "running");
    const bound = Option.getOrThrow(running.data);
    expect(bound.conversation.execution.attempt_id).toBe("att_1");
    // The queued message is what the attempt answers, so it leaves `queued`.
    await h.waitFor(
      atomFor(h, conversationId),
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.messages.some(
              (message) => message.role === "user" && message.delivery === "sent",
            ),
        }),
      "the message the attempt picked up",
    );

    await h.control(`runner/${conversationId}/complete`);
    const completed = await awaitStatus(h, conversationId, "completed");
    const detail = Option.getOrThrow(completed.data);
    const answer = detail.messages.filter((message) => message.role === "assistant").at(-1);
    expect(answer?.text).toContain("renewal");
    // The turn ran on a runner, in the coordinator role it was dispatched for.
    expect(answer?.actor.kind).toBe("coordinator");
    expect(answer?.attempt_id).toBe("att_1");
  }, 20_000);

  it("stops a running coordinator turn with cancel, not interrupt", async () => {
    const { h, conversationId } = await openQueuedChat();
    await h.control(`runner/${conversationId}/start`);
    const running = await awaitStatus(h, conversationId, "running");
    const execution = Option.getOrThrow(running.data).conversation.execution;

    // The client-facing kind stays `cancel`; the hub delivers it to the runner
    // holding the coordinator work item as an interrupt (decisions.md §9.1).
    await h.run(
      h.client.effects.sendControl({
        projectId: PROJECT,
        conversationId,
        command: {
          key: "cmd_coordinator_cancel",
          kind: "cancel",
          expected: { attempt_id: execution.attempt_id, turn_id: execution.turn_id },
        },
      }),
    );

    const stopped = await awaitStatus(h, conversationId, "interrupted");
    const detail = Option.getOrThrow(stopped.data);
    expect(detail.receipts["cmd_coordinator_cancel"]?.status).toBe("sent");
    expect(detail.receipts["cmd_coordinator_cancel"]?.kind).toBe("cancel");
    // The open answer stopped where it was rather than being dropped.
    const answer = detail.messages.filter((message) => message.role === "assistant").at(-1);
    expect(answer?.delivery).toBe("interrupted");
    expect(detail.staleExecution).toBe(false);
  }, 20_000);

  it("passes the interrupting step through on the way to interrupted", async () => {
    const { h, conversationId } = await openQueuedChat();
    await h.control(`runner/${conversationId}/start`);
    const running = await awaitStatus(h, conversationId, "running");
    const execution = Option.getOrThrow(running.data).conversation.execution;

    const seen: ExecutionStatus[] = [];
    const unsubscribe = h.registry.subscribe(atomFor(h, conversationId), () => {
      const state = h.read(atomFor(h, conversationId));
      if (state === undefined) return;
      const status = Option.getOrUndefined(state.data)?.conversation.execution.status;
      if (status !== undefined && seen.at(-1) !== status) seen.push(status);
    });
    await h.run(
      h.client.effects.sendControl({
        projectId: PROJECT,
        conversationId,
        command: {
          key: "cmd_coordinator_cancel_ladder",
          kind: "cancel",
          expected: { attempt_id: execution.attempt_id, turn_id: execution.turn_id },
        },
      }),
    );
    await awaitStatus(h, conversationId, "interrupted");
    unsubscribe();

    // "Your stop is on its way" and "it stopped" are different sentences in the
    // strip, so the client has to be told both rather than only the outcome.
    expect(seen).toContain("interrupting");
    expect(seen.indexOf("interrupting")).toBeLessThan(seen.indexOf("interrupted"));
  }, 20_000);

  it("posts a runner proposal as a system status message the client can read", async () => {
    const { h, conversationId } = await openQueuedChat(
      "Please create an issue for the lock renewal",
    );
    await h.control(`runner/${conversationId}/start`);
    await awaitStatus(h, conversationId, "running");
    await h.control(`runner/${conversationId}/complete`);
    await awaitStatus(h, conversationId, "completed");

    // `propose_issue` posts an `item` event with `data.proposal` and creates
    // nothing (decisions.md §9.4). It arrives as `role: system, kind: status`.
    const proposal = detailOf(h, conversationId).messages.find(
      (message) => "proposal" in message.data,
    );
    expect(proposal?.role).toBe("system");
    expect(proposal?.kind).toBe("status");
    expect(detailOf(h, conversationId).conversation.work_item_id).toBeNull();
  }, 20_000);

  it("reports no coordinator for a project with neither a runner nor a backend", async () => {
    const h = (harness = await makeHarness({ coordinator: "none" }));
    expect(h.client.bootstrap.capabilities.coordinator).toBe(false);
    expect(projectCoordinatorAvailable(h.client.bootstrap, PROJECT)).toBe(false);

    // The send is still accepted: a message queues and waits, so the client has
    // a note to show, not a block to enforce (decisions.md §9.1).
    const created = (await h.run(
      h.client.effects.createConversation({
        projectId: PROJECT,
        key: "cmd_no_coordinator_create",
        firstMessage: { key: "cmd_no_coordinator", text: "Anyone home?" },
      }),
    )) as Result<typeof CreateConversationResponse.Type, unknown>;
    expect(created._tag).toBe("Success");
  }, 20_000);

  it("keeps the scripted hub coordinator as the default mode", async () => {
    const h = (harness = await makeHarness());
    expect(projectCoordinatorAvailable(h.client.bootstrap, PROJECT)).toBe(true);
    const created = (await h.run(
      h.client.effects.createConversation({
        projectId: PROJECT,
        key: "cmd_hub_mode_create",
        firstMessage: { key: "cmd_hub_mode", text: "Why does the lease lapse?" },
      }),
    )) as Result<typeof CreateConversationResponse.Type, unknown>;
    if (created._tag !== "Success") throw new Error("Creating the conversation failed.");
    expect(created.success.receipt?.status).toBe("delivered");
  }, 20_000);
});
