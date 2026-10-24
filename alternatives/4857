// Runner controls and questions (U07) against the mock hub's scripted runner.
//
// Everything here is the contract in decisions.md §5: one winner per question,
// controls that name the attempt they were aimed at, `stale_execution` when
// that attempt is gone, and an unknown outcome that stays unknown until the
// user retries the same key.
import * as Option from "effect/Option";
import { afterEach, describe, expect, it } from "vitest";

import type { Result } from "effect/Result";

import { makeHarness, PROJECT, type Harness } from "./harness.ts";
import type { CreateConversationResponse } from "../src/contracts/index.ts";
import {
  latestControl,
  questionLocked,
  type ConversationDetail,
} from "../src/runtime/state/conversationState.ts";

let harness: Harness | undefined;
let second: Harness | undefined;

afterEach(async () => {
  await second?.dispose();
  second = undefined;
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

/** A linked conversation with a running attempt and an open assistant turn. */
async function openRunning() {
  const h = (harness = await makeHarness());
  const created = (await h.run(
    h.client.effects.createConversation({
      projectId: PROJECT,
      key: "cmd_runner_create",
      firstMessage: { key: "cmd_runner_seed", text: "Fix the lease renewal" },
    }),
  )) as Result<typeof CreateConversationResponse.Type, unknown>;
  if (created._tag !== "Success") throw new Error("Creating the conversation failed.");
  const conversationId = created.success.conversation.id;
  const atom = atomFor(h, conversationId);
  h.mount(atom);
  await h.waitFor(atom, (value) => Option.isSome(value.data), "the snapshot");

  const linked = await h.run(
    h.client.effects.link({
      projectId: PROJECT,
      conversationId,
      key: "cmd_runner_link",
      shareHistory: true,
      issue: { title: "Lease renewal", description: "Move it behind the handoff." },
    }),
  );
  if (linked._tag !== "Success") throw new Error("Linking failed.");
  await h.waitFor(
    atom,
    (value) =>
      Option.match(value.data, {
        onNone: () => false,
        onSome: (detail) => detail.conversation.execution.status === "waiting_for_runner",
      }),
    "waiting for a runner",
  );

  await h.control(`runner/${conversationId}/start`);
  const running = await h.waitFor(
    atom,
    (value) =>
      Option.match(value.data, {
        onNone: () => false,
        onSome: (detail) => detail.conversation.execution.status === "running",
      }),
    "the running attempt",
  );
  return {
    h,
    conversationId,
    atom,
    attemptId: Option.getOrThrow(running.data).conversation.execution.attempt_id,
  };
}

describe("runner controls", () => {
  it("carries the attempt it was shown and settles the interrupt receipt", async () => {
    const { h, conversationId, atom, attemptId } = await openRunning();

    await h.run(
      h.client.effects.sendInterrupt({
        projectId: PROJECT,
        conversationId,
        key: "cmd_interrupt_1",
        expected: { attempt_id: attemptId, turn_id: null },
      }),
    );

    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.conversation.execution.status === "interrupted",
        }),
      "the interrupted attempt",
    );
    const detail = Option.getOrThrow(state.data);
    const control = latestControl(detail, "interrupt");
    expect(control?.status).toBe("sent");
    expect(control?.attemptId).toBe(attemptId);
    expect(detail.staleExecution).toBe(false);
  });

  it("records a continue as intent and reports waiting for a runner", async () => {
    const { h, conversationId, atom, attemptId } = await openRunning();
    await h.control(`runner/${conversationId}/complete`);
    await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.conversation.execution.status === "completed",
        }),
      "the completed attempt",
    );

    await h.run(
      h.client.effects.sendContinue({
        projectId: PROJECT,
        conversationId,
        key: "cmd_continue_1",
        expected: { attempt_id: attemptId, turn_id: null },
      }),
    );

    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.conversation.execution.status === "waiting_for_runner",
        }),
      "the queued continue",
    );
    expect(latestControl(Option.getOrThrow(state.data), "continue")?.status).toBe("sent");
  });

  it("refuses a control aimed at an attempt that was replaced", async () => {
    const { h, conversationId, atom, attemptId } = await openRunning();
    await h.control(`runner/${conversationId}/complete`);
    await h.control(`runner/${conversationId}/start`);
    const restarted = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.conversation.execution.attempt_id !== attemptId &&
            detail.conversation.execution.status === "running",
        }),
      "the second attempt",
    );
    const current = Option.getOrThrow(restarted.data).conversation.execution.attempt_id;

    await h.run(
      h.client.effects.sendInterrupt({
        projectId: PROJECT,
        conversationId,
        key: "cmd_interrupt_stale",
        expected: { attempt_id: attemptId, turn_id: null },
      }),
    );

    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.staleExecution,
        }),
      "the stale notice",
    );
    const detail = Option.getOrThrow(state.data);
    const control = latestControl(detail, "interrupt");
    expect(control?.status).toBe("rejected");
    expect(control?.errorCode).toBe("stale_execution");
    // The client re-read the state; it did not re-aim at the new attempt.
    expect(detail.conversation.execution.attempt_id).toBe(current);
    expect(detail.conversation.execution.status).toBe("running");
  });

  it("restores the accumulated assistant text from a refreshed snapshot", async () => {
    const { h, conversationId, atom } = await openRunning();
    const streamed = detailOf(h, conversationId)
      .messages.filter((message) => message.role === "assistant")
      .at(-1);
    expect(streamed?.delivery).toBe("responding");

    await h.run(h.client.effects.refreshConversation({ projectId: PROJECT, conversationId }));
    const state = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.messages.some(
              (message) => message.role === "assistant" && message.text.includes("renewal"),
            ),
        }),
      "the restored partial reply",
    );
    // What the runner had written so far survives the refresh, and the
    // buffered deltas that produced it are not replayed on top of it.
    const detail = Option.getOrThrow(state.data);
    const assistant = detail.messages.filter((message) => message.role === "assistant").at(-1);
    expect(assistant?.text.startsWith("Reading the lease renewal path.")).toBe(true);
    expect(Object.keys(detail.deltas)).toHaveLength(0);
  });
});

describe("questions", () => {
  it("has one winner across two clients", async () => {
    const { h, conversationId, atom } = await openRunning();
    await h.control(`runner/${conversationId}/question`);
    const opened = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.questions.length > 0,
        }),
      "the opened question",
    );
    const question = Option.getOrThrow(opened.data).questions[0]!;

    // A second client on the same hub: another tab, another registry.
    const other = (second = await makeHarness({ hub: h.hub }));
    const otherAtom = atomFor(other, conversationId);
    other.mount(otherAtom);
    await other.waitFor(
      otherAtom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.questions.some((entry) => entry.id === question.id),
        }),
      "the question in the second client",
    );

    await h.run(
      h.client.effects.sendAnswer({
        projectId: PROJECT,
        conversationId,
        key: "cmd_answer_winner",
        questionId: question.id,
        answers: { "renewal-window": ["Half the lease"] },
        expected: question.owner,
      }),
    );

    const winner = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.questions.some(
              (entry) => entry.id === question.id && entry.status === "answered",
            ),
        }),
      "the answered question",
    );
    const winnerDetail = Option.getOrThrow(winner.data);
    const winnerControl = latestControl(winnerDetail, "answer", question.id);
    expect(winnerControl?.status).toBe("sent");
    expect(
      questionLocked(
        winnerDetail.questions.find((entry) => entry.id === question.id)!,
        winnerControl,
      ),
    ).toBe(true);

    // The second client learns from `question.updated` alone: it locks the
    // card without having answered anything.
    const observed = await other.waitFor(
      otherAtom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            detail.questions.some(
              (entry) => entry.id === question.id && entry.status === "answered",
            ),
        }),
      "the question.updated in the second client",
    );
    const observedQuestion = Option.getOrThrow(observed.data).questions.find(
      (entry) => entry.id === question.id,
    )!;
    expect(questionLocked(observedQuestion, undefined)).toBe(true);

    // Answering anyway loses: exactly one winner per question.
    await other.run(
      other.client.effects.sendAnswer({
        projectId: PROJECT,
        conversationId,
        key: "cmd_answer_loser",
        questionId: question.id,
        answers: { "renewal-window": ["Fixed 20 seconds"] },
        expected: question.owner,
      }),
    );
    const loser = await other.waitFor(
      otherAtom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            latestControl(detail, "answer", question.id)?.status === "rejected",
        }),
      "the refused second answer",
    );
    const loserControl = latestControl(Option.getOrThrow(loser.data), "answer", question.id);
    expect(loserControl?.errorCode).toBe("question_already_answered");
    expect(
      questionLocked(
        Option.getOrThrow(loser.data).questions.find((entry) => entry.id === question.id)!,
        loserControl,
      ),
    ).toBe(true);
  });

  it("keeps an unknown answer unknown and reuses the key on retry", async () => {
    const { h, conversationId, atom } = await openRunning();
    await h.control(`runner/${conversationId}/question`);
    const opened = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) => detail.questions.length > 0,
        }),
      "the opened question",
    );
    const question = Option.getOrThrow(opened.data).questions[0]!;

    await h.control("unknown-outcome");
    const send = () =>
      h.run(
        h.client.effects.sendAnswer({
          projectId: PROJECT,
          conversationId,
          key: "cmd_answer_unknown",
          questionId: question.id,
          answers: { "renewal-window": ["Half the lease"] },
          expected: question.owner,
        }),
      );
    await send();

    const unknown = await h.waitFor(
      atom,
      (value) =>
        Option.match(value.data, {
          onNone: () => false,
          onSome: (detail) =>
            latestControl(detail, "answer", question.id)?.status === "unknown",
        }),
      "the unknown answer",
    );
    expect(
      Option.getOrThrow(unknown.data).questions.find((entry) => entry.id === question.id)?.status,
    ).toBe("pending");

    // The retry is the same intent under the same key, so the hub replays its
    // stored receipt and the question is answered at most once.
    await send();
    const after = detailOf(h, conversationId);
    expect(after.controls.filter((entry) => entry.key === "cmd_answer_unknown")).toHaveLength(1);
    expect(latestControl(after, "answer", question.id)?.status).toBe("unknown");
  });
});
