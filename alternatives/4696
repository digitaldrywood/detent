// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ExecutionStrip } from "../../src/app/components/ExecutionStrip.tsx";
import type { ExecutionStatus } from "../../src/contracts/index.ts";
import type { PendingControl } from "../../src/runtime/state/conversationState.ts";
import { execution } from "./builders.ts";

afterEach(cleanup);

function renderStrip(overrides: Partial<React.ComponentProps<typeof ExecutionStrip>> = {}) {
  const onInterrupt = vi.fn();
  const onContinue = vi.fn();
  const onRetryControl = vi.fn();
  const onDiscardControl = vi.fn();
  const onDismissStale = vi.fn();
  const utils = render(
    <ExecutionStrip
      execution={execution()}
      controls={[]}
      onInterrupt={onInterrupt}
      onContinue={onContinue}
      onRetryControl={onRetryControl}
      onDiscardControl={onDiscardControl}
      stale={false}
      onDismissStale={onDismissStale}
      {...overrides}
    />,
  );
  return { ...utils, onInterrupt, onContinue, onRetryControl, onDiscardControl, onDismissStale };
}

// The strip is one line, so it prints a sentence only where the label does not
// already say it (Michael's review, September 11). Every status still carries
// its own label and tone, which is what A.11 is about.
const LABELS: ReadonlyArray<readonly [ExecutionStatus, string]> = [
  ["idle", "Idle"],
  ["waiting_for_runner", "Waiting for a runner"],
  ["starting", "Starting"],
  ["running", "Running"],
  ["waiting_input", "Waiting for you"],
  ["interrupting", "Interrupting"],
  ["completed", "Completed"],
  ["interrupted", "Interrupted"],
  ["failed", "Failed"],
  ["unknown", "Unknown"],
];

const SENTENCES: ReadonlyArray<readonly [ExecutionStatus, string]> = [
  ["waiting_for_runner", "The issue is queued. Nothing is running yet."],
  ["waiting_input", "The runner stopped to ask you something."],
  ["unknown", "We cannot read the runner's state. Refresh to check."],
];

/** Statuses whose sentence only restated the label, and so is now dropped. */
const RESTATEMENTS: ReadonlyArray<readonly [ExecutionStatus, string]> = [
  ["idle", "No runner is working on this issue."],
  ["starting", "A runner took the issue and is starting its attempt."],
  ["running", "A runner is working on this issue now."],
  ["interrupting", "Your interrupt is on its way to the runner."],
  ["completed", "The last attempt finished."],
  ["interrupted", "The last attempt stopped when it was interrupted."],
  ["failed", "The last attempt failed."],
];

describe("ExecutionStrip", () => {
  it.each(LABELS)("names %s", (status, label) => {
    renderStrip({ execution: execution({ status, error: null }) });
    expect(screen.getByTestId("execution-copy").textContent).toContain(label);
  });

  it.each(SENTENCES)("explains %s in plain language", (status, sentence) => {
    renderStrip({ execution: execution({ status, error: null }) });
    expect(screen.getByTestId("execution-copy").textContent).toContain(sentence);
  });

  it.each(RESTATEMENTS)("says %s once rather than twice", (status, sentence) => {
    renderStrip({ execution: execution({ status, error: null }) });
    expect(screen.getByTestId("execution-copy").textContent).not.toContain(sentence);
  });

  it("never gives a queued issue the treatment of a working runner", () => {
    const { unmount } = renderStrip({ execution: execution({ status: "waiting_for_runner" }) });
    const queued = screen.getByTestId("execution-copy").textContent;
    unmount();
    renderStrip({ execution: execution({ status: "running" }) });
    expect(screen.getByTestId("execution-copy").textContent).not.toBe(queued);
  });

  it("gives a failed attempt the reason and nothing else", () => {
    renderStrip({
      execution: execution({ status: "failed", error: "The migration did not apply." }),
    });
    const copy = screen.getByTestId("execution-copy").textContent ?? "";
    expect(copy).toContain("The migration did not apply.");
    expect(copy).not.toContain("The last attempt failed.");
  });

  // The whole point of the change: label, attempt id, controls, one line.
  it("keeps the status line to one line beside the attempt id", () => {
    renderStrip({ execution: execution({ status: "completed" }) });
    const copy = screen.getByTestId("execution-copy");
    expect(copy.textContent).toBe("Completed");
    expect(copy.className).toContain("truncate");
    expect(screen.getByTestId("execution-attempt").className).toContain("truncate");
  });

  it("shows the attempt the controls will carry", () => {
    renderStrip();
    expect(screen.getByTestId("execution-attempt").textContent).toBe("attempt att_18f4");
  });

  it("interrupts a running attempt and refuses to interrupt an idle one", () => {
    const { onInterrupt, unmount } = renderStrip();
    fireEvent.click(screen.getByRole("button", { name: "Interrupt" }));
    expect(onInterrupt).toHaveBeenCalledTimes(1);
    unmount();

    renderStrip({ execution: execution({ status: "idle", attempt_id: null }) });
    expect(
      screen.getByRole("button", { name: "Interrupt" }).getAttribute("aria-disabled"),
    ).toBe("true");
  });

  it("offers continue only once the attempt has ended", () => {
    const running = renderStrip();
    expect(screen.getByRole("button", { name: "Continue" }).getAttribute("aria-disabled")).toBe(
      "true",
    );
    running.unmount();

    const ended = renderStrip({ execution: execution({ status: "completed" }) });
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    expect(ended.onContinue).toHaveBeenCalledTimes(1);
    expect(running.onContinue).not.toHaveBeenCalled();
  });

  it("hides a control the bound backend cannot honour", () => {
    renderStrip({
      execution: execution({
        capabilities: { steer: false, interrupt: false, answer: true, continue: true },
      }),
    });
    expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
    expect(screen.getByRole("button", { name: "Continue" })).toBeTruthy();
  });

  it("says the runner changed rather than re-aiming the control", () => {
    const { onDismissStale } = renderStrip({ stale: true });
    expect(screen.getByTestId("stale-execution").textContent).toContain(
      "The runner changed; review the new state.",
    );
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(onDismissStale).toHaveBeenCalledTimes(1);
  });

  it("keeps an unconfirmed control retryable under the same key", () => {
    const entry: PendingControl = {
      key: "cmd_interrupt_1",
      kind: "interrupt",
      questionId: null,
      attemptId: "att_18f4",
      createdAt: "2026-09-09T10:04:00Z",
      status: "unknown",
      error: null,
      errorCode: null,
      receiptStatus: null,
      expected: { attempt_id: "att_18f4", turn_id: "turn_5" },
      answers: null,
    };
    const { onRetryControl, onDiscardControl } = renderStrip({ controls: [entry] });
    expect(screen.getByTestId("control-receipt").textContent).toContain(
      "We could not confirm the runner received this.",
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry same command" }));
    expect(onRetryControl).toHaveBeenCalledWith(expect.objectContaining({ key: "cmd_interrupt_1" }));
    fireEvent.click(screen.getByRole("button", { name: "Discard" }));
    expect(onDiscardControl).toHaveBeenCalledWith(
      expect.objectContaining({ key: "cmd_interrupt_1" }),
    );
  });

  it("disables the controls for a reader without write access", () => {
    renderStrip({ blockedReason: "Read-only project" });
    expect(
      screen.getByRole("button", { name: "Interrupt" }).getAttribute("aria-disabled"),
    ).toBe("true");
  });
});

// An unlinked chat is answered by a coordinator turn on a customer runner
// (decisions.md §1, §9). The strip is the same component with the chat
// vocabulary: it never says "issue", it offers Stop rather than the issue
// controls, and it offers no model, provider, cost or key choice at all.
describe("ExecutionStrip on an unlinked chat", () => {
  const chat = (status: ExecutionStatus, overrides = {}) =>
    renderStrip({
      surface: "chat",
      projectName: "alpha",
      execution: execution({ status, error: null, ...overrides }),
    });

  it("names the project it is waiting for a runner in", () => {
    chat("waiting_for_runner");
    expect(screen.getByTestId("execution-copy").textContent).toContain(
      "Waiting for a runner to pick up this chat in alpha.",
    );
  });

  it("still says it is waiting when the project name is unknown", () => {
    renderStrip({
      surface: "chat",
      execution: execution({ status: "waiting_for_runner", error: null }),
    });
    expect(screen.getByTestId("execution-copy").textContent).toContain(
      "Waiting for a runner to pick up this chat.",
    );
  });

  it.each<[ExecutionStatus, string]>([
    ["starting", "Starting"],
    ["running", "Running"],
    ["interrupting", "Stopping"],
    ["completed", "Completed"],
    ["interrupted", "Stopped"],
    ["unknown", "We cannot read the runner's state. Refresh to check."],
  ])("explains %s without calling the chat an issue", (status, sentence) => {
    chat(status);
    const copy = screen.getByTestId("execution-copy").textContent ?? "";
    expect(copy).toContain(sentence);
    expect(copy).not.toContain("issue");
  });

  it("repeats the reason the hub gave for a failure", () => {
    chat("failed", { error: "No runner claimed the coordinator item." });
    const copy = screen.getByTestId("execution-copy").textContent ?? "";
    expect(copy).toContain("Failed");
    expect(copy).toContain("No runner claimed the coordinator item.");
    expect(copy).not.toContain("The runner could not answer this chat.");
  });

  it("never gives a queued chat the treatment of an answering runner", () => {
    const { unmount } = chat("waiting_for_runner");
    const queued = screen.getByTestId("execution-copy").textContent;
    unmount();
    chat("running");
    expect(screen.getByTestId("execution-copy").textContent).not.toBe(queued);
  });

  it("offers Stop while a runner is answering and nothing else", () => {
    const onStop = vi.fn();
    renderStrip({
      surface: "chat",
      projectName: "alpha",
      onStop,
      execution: execution({ status: "running", error: null }),
    });
    expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Stop" }));
    expect(onStop).toHaveBeenCalledTimes(1);
  });

  it("offers no stop once the turn is over or already stopping", () => {
    for (const status of ["waiting_for_runner", "interrupting", "completed"] as const) {
      const { unmount } = chat(status);
      expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
      unmount();
    }
  });

  it("withholds Stop from a reader without write access", () => {
    renderStrip({
      surface: "chat",
      blockedReason: "Read-only project",
      execution: execution({ status: "running", error: null }),
    });
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  });

  it("offers no model, provider, cost or key choice", () => {
    chat("running");
    // Every turn runs on the customer's runner with that runner's own login,
    // so there is nothing here for the reader to pick (decisions.md §1).
    expect(screen.queryByRole("combobox")).toBeNull();
    const copy = screen.getByTestId("execution-strip").textContent ?? "";
    for (const word of ["model", "provider", "cost", "key", "token"]) {
      expect(copy.toLowerCase()).not.toContain(word);
    }
  });

  // §10.4: transcript recovery is visible.
  it("says when the runner continued from a transcript", () => {
    renderStrip({ execution: execution({ status: "running", resume: "transcript" }) });
    expect(screen.getByTestId("execution-resume").textContent).toBe(
      "This runner continued from a transcript of recent messages; the provider's earlier context was not available.",
    );
  });

  it("says nothing when the runner resumed its provider thread", () => {
    renderStrip({ execution: execution({ status: "running", resume: "thread" }) });
    expect(screen.queryByTestId("execution-resume")).toBeNull();
  });

  // §10.11: a viewer who cannot write gets no controls at all, not disabled
  // ones.
  it("renders no controls for a read-only viewer", () => {
    renderStrip({
      execution: execution({ status: "running", capabilities: {
        steer: true, interrupt: true, answer: true, continue: true,
      } }),
      readOnly: true,
    });
    expect(screen.queryByRole("button", { name: "Interrupt" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull();
    expect(screen.getByTestId("execution-copy").textContent).toContain("Running");
  });

  it("renders no Stop on a read-only chat", () => {
    renderStrip({
      execution: execution({ status: "running" }),
      surface: "chat",
      onStop: vi.fn(),
      readOnly: true,
    });
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull();
  });
});
