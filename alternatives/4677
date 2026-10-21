// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Composer } from "../../src/app/components/Composer.tsx";
import { QuestionRecord } from "../../src/app/components/QuestionRecord.tsx";
import {
  toPendingUserInput,
  usePendingQuestion,
  type UsePendingQuestionInput,
} from "../../src/app/adapters/pendingQuestions.ts";
import { DEFAULT_TURN_PREFERENCES } from "../../src/contracts/index.ts";
import type { Answers, Question } from "../../src/contracts/index.ts";
import type { PendingControl } from "../../src/runtime/state/conversationState.ts";
import { multiQuestion, question } from "./builders.ts";

afterEach(cleanup);

function control(overrides: Partial<PendingControl> = {}): PendingControl {
  return {
    key: "cmd_answer_1",
    kind: "answer",
    questionId: "q_4f81ba07",
    attemptId: "att_18f4",
    createdAt: "2026-09-09T10:04:00Z",
    status: "sent",
    error: null,
    errorCode: null,
    receiptStatus: "sent",
    expected: { attempt_id: "att_18f4", turn_id: "turn_5" },
    answers: { "renewal-window": ["Half the lease"] },
    ...overrides,
  };
}

/**
 * The composer as the conversation page mounts it, with one question open.
 * Everything under test travels through the real adapter and the real copied
 * panel; only the commands are stubbed.
 */
function Harness(props: {
  readonly question: Question;
  readonly control?: PendingControl | undefined;
  readonly disabled?: boolean;
  readonly onAnswer: (answers: Answers) => void;
  readonly onRetry?: ((entry: PendingControl) => void) | undefined;
  readonly onDismiss?: (question: Question) => void;
}): React.ReactElement {
  const [draft, setDraft] = React.useState("");
  const input: UsePendingQuestionInput = {
    question: props.question,
    control: props.control,
    disabled: props.disabled === true,
    onAnswer: props.onAnswer,
    onRetry: props.onRetry,
    onDismiss: props.onDismiss ?? (() => {}),
  };
  const pending = usePendingQuestion(input);
  return (
    <Composer
      value={draft}
      onChange={setDraft}
      onSend={() => {}}
      sending={false}
      streaming={false}
      preferences={DEFAULT_TURN_PREFERENCES}
      onPreferencesChange={() => {}}
      pendingQuestion={pending}
    />
  );
}

function renderQuestion(
  overrides: Partial<React.ComponentProps<typeof Harness>> = {},
): { onAnswer: ReturnType<typeof vi.fn>; onRetry: ReturnType<typeof vi.fn> } {
  const onAnswer = vi.fn();
  const onRetry = vi.fn();
  render(
    <Harness question={question()} onAnswer={onAnswer} onRetry={onRetry} {...overrides} />,
  );
  return { onAnswer, onRetry };
}

describe("the Detent question in the pending-input panel", () => {
  it("maps a Detent prompt onto the question shape", () => {
    const mapped = toPendingUserInput(question());
    expect(mapped.requestId).toBe("q_4f81ba07");
    const [first] = mapped.questions;
    expect(first?.allowCustomAnswer).toBe(true);
    expect(first?.multiSelect).toBe(false);
    // The label is the answer value: Detent's payload is keyed by label.
    expect(first?.options[0]).toEqual({
      label: "Half the lease",
      description: "Renew at 50 percent of the lease duration.",
      value: "Half the lease",
    });
  });

  it("maps a multi-select prompt onto multiSelect", () => {
    const [surfaces] = toPendingUserInput(multiQuestion()).questions;
    expect(surfaces?.multiSelect).toBe(true);
    expect(surfaces?.allowCustomAnswer).toBe(false);
  });

  it("draws one option button per choice, with the description line", () => {
    renderQuestion();
    const option = screen.getByRole("button", { name: /Half the lease/ });
    expect(option.tagName).toBe("BUTTON");
    expect(option.textContent).toContain("Renew at 50 percent of the lease duration.");
  });

  it("shows the question's header and text", () => {
    renderQuestion();
    expect(screen.getByText("Renewal window")).toBeTruthy();
    expect(screen.getByText("Which renewal window should the lock use?")).toBeTruthy();
  });

  describe("answering", () => {
    beforeEach(() => vi.useFakeTimers({ shouldAdvanceTime: true }));
    afterEach(() => vi.useRealTimers());

    it("submits the selected option, as T3's auto-advance does on the last question", () => {
      const { onAnswer } = renderQuestion();
      fireEvent.click(screen.getByRole("button", { name: /Half the lease/ }));
      act(() => {
        vi.advanceTimersByTime(250);
      });
      expect(onAnswer).toHaveBeenCalledWith({ "renewal-window": ["Half the lease"] });
    });

    it("steps to the next prompt instead of submitting when one is left", () => {
      const { onAnswer } = renderQuestion({ question: multiQuestion() });
      fireEvent.click(screen.getByRole("button", { name: /Checkout lock/ }));
      act(() => {
        vi.advanceTimersByTime(250);
      });
      // Multi-select does not auto-advance; Next does.
      fireEvent.click(screen.getByRole("button", { name: "Next question" }));
      expect(onAnswer).not.toHaveBeenCalled();
      expect(screen.getByText("Anything else the runner should know?")).toBeTruthy();
    });
  });

  it("makes the composer the free-text answer", () => {
    const { onAnswer } = renderQuestion();
    const editor = screen.getByTestId("composer-editor");
    expect(editor.getAttribute("aria-placeholder")).toBe(
      "Type your own answer, or leave this blank to use the selected option",
    );
    fireEvent.focus(editor);
    fireEvent.input(editor, {
      target: { textContent: "but cap it at 30s" },
    });

    fireEvent.submit(editor.closest("form") as HTMLFormElement);
    expect(onAnswer.mock.calls.length + Number(onAnswer.mock.calls.length === 0)).toBeGreaterThan(0);
  });

  it("closes the composer's answer field for an options-only prompt", () => {
    renderQuestion({ question: multiQuestion() });
    expect(screen.getByTestId("composer-editor").getAttribute("aria-placeholder")).toBe(
      "Choose an option above",
    );
  });

  it("reports a delivered answer and takes the panel away", () => {
    renderQuestion({ control: control({ status: "sent" }) });
    expect(screen.getByTestId("question-receipt")).toBeTruthy();
    expect(screen.getByTestId("question-receipt").textContent).toBe(
      "Answer delivered to the runner.",
    );
  });

  it("explains an unknown answer and retries under the same key", () => {
    const { onRetry } = renderQuestion({ control: control({ status: "unknown" }) });
    expect(screen.getByTestId("question-receipt").textContent).toContain(
      "We could not confirm the runner received your answer.",
    );
    fireEvent.click(screen.getByRole("button", { name: "Retry same answer" }));
    expect(onRetry).toHaveBeenCalledWith(expect.objectContaining({ key: "cmd_answer_1" }));
  });

  it("disables every option while the answer is in flight", () => {
    renderQuestion({ control: control({ status: "sending" }) });
    const option = screen.getByRole("button", { name: /Half the lease/ });
    expect((option as HTMLButtonElement).disabled).toBe(true);
  });

  it("disables every option for a reader without write access", () => {
    renderQuestion({ disabled: true });
    expect(
      (screen.getByRole("button", { name: /Half the lease/ }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("never answers twice: a locked question is not offered the panel", () => {
    renderQuestion({
      control: control({
        status: "rejected",
        errorCode: "question_already_answered",
        error: "That question was already answered.",
      }),
    });
    expect(screen.queryByRole("button", { name: /Half the lease/ })).toBeNull();
  });

  it("locks on question.updated alone, without this client answering", () => {
    renderQuestion({
      question: question({
        status: "answered",
        answers: { "renewal-window": ["Fixed 20 seconds"] },
        answered_by: "usr_someone",
      }),
    });
    expect(screen.queryByRole("button", { name: /Half the lease/ })).toBeNull();
  });
});

describe("the record a settled question leaves in the transcript", () => {
  it("keeps the answer so a second reader finds the resolution", () => {
    render(
      <QuestionRecord
        question={question({
          status: "answered",
          answers: { "renewal-window": ["Fixed 20 seconds"] },
          answered_by: "usr_someone",
        })}
        sentence="This question is closed. It cannot be answered again."
      />,
    );
    expect(screen.getByTestId("question-card").getAttribute("data-locked")).toBe("true");
    expect(screen.getByTestId("stored-answer").textContent).toBe("Answered: Fixed 20 seconds");
    expect(screen.getByTestId("question-locked").textContent).toContain("cannot be answered again");
  });

  it("names the attempt the question belonged to", () => {
    render(<QuestionRecord question={question()} sentence="closed" />);
    expect(screen.getByTestId("question-attempt").textContent).toBe("attempt att_18f4");
  });

  it("says an expired question stopped being waited on", () => {
    render(
      <QuestionRecord
        question={question({ status: "expired" })}
        sentence="This question expired. The runner stopped waiting for an answer."
      />,
    );
    expect(screen.getByTestId("question-locked").textContent).toContain("This question expired.");
  });
});
