import React from "react";

import type { Answers, Question, UserInputQuestion } from "../../contracts/index.ts";
import type { PendingUserInput } from "../../session-logic.ts";
import {
  buildPendingUserInputAnswers,
  derivePendingUserInputProgress,
  setPendingUserInputCustomAnswer,
  togglePendingUserInputOptionSelection,
  type PendingUserInputDraftAnswer,
} from "../../pendingUserInput.ts";
import { type PendingControl, questionLocked } from "../../runtime/state/conversationState.ts";

const EMPTY_QUESTIONS: readonly UserInputQuestion[] = [];

export function toUserInputQuestion(prompt: Question["questions"][number]): UserInputQuestion {
  return {
    id: prompt.id,
    header: prompt.header,
    question: prompt.question,
    options: prompt.options.map((option) => ({
      label: option.label,
      description: option.description,
      value: option.label,
    })),
    allowCustomAnswer: prompt.free_text === true,
    multiSelect: prompt.multiple === true,
  };
}

export function toPendingUserInput(question: Question): PendingUserInput {
  return {
    requestId: question.id,
    createdAt: question.created_at,
    questions: question.questions.map(toUserInputQuestion),
    // A Detent question is always dismissible from this client's side: not
    // answering it is a real choice, and the runner keeps waiting either way.
    dismissible: true,
  };
}

export function toDetentAnswers(answers: Record<string, string | string[]>): Answers {
  const detent: Record<string, string[]> = {};
  for (const [promptId, value] of Object.entries(answers)) {
    detent[promptId] = Array.isArray(value) ? [...value] : [value];
  }
  return detent;
}

/** What the composer needs to draw and drive one pending question. */
export interface PendingQuestionState {
  readonly pending: PendingUserInput;
  readonly respondingRequestIds: string[];
  readonly answers: Record<string, PendingUserInputDraftAnswer>;
  readonly questionIndex: number;
  readonly onToggleOption: (questionId: string, optionValue: string) => void;

  readonly customAnswer: string;
  readonly onCustomAnswerChange: (text: string) => void;

  readonly placeholder: string;
  /** False when the active question takes options only; the editor is then idle. */
  readonly acceptsCustomAnswer: boolean;
  readonly onAdvance: () => void;
  readonly onPrevious: () => void;
  readonly onDismiss: () => void;

  readonly action: {
    readonly questionIndex: number;
    readonly isLastQuestion: boolean;
    readonly canAdvance: boolean;
    readonly isResponding: boolean;
    readonly isComplete: boolean;
  };
  /** Detent's receipt for the answer command, or null while nothing is in flight. */
  readonly receipt: string | null;
  /** Set once the question can no longer be answered; the panel is then hidden. */
  readonly lockedSentence: string | null;
  readonly retry: (() => void) | null;
}

/** The one sentence the composer shows about an answer's delivery (§5, §10.3). */
export function receiptSentence(
  question: Question,
  control: PendingControl | undefined,
): string | null {
  if (control === undefined) {
    return question.status === "answered" ? "Already answered elsewhere." : null;
  }
  switch (control.status) {
    case "sending":
      return "Sending your answer.";
    case "queued":
      return "Your answer is queued for the runner.";
    case "sent":
      return "Answer delivered to the runner.";
    case "unknown":
      return "We could not confirm the runner received your answer. Retry sends the same answer once.";
    case "rejected":
      return control.errorCode === "question_already_answered" || question.status === "answered"
        ? "Already answered elsewhere."
        : (control.error ?? "The hub refused this answer.");
  }
}

export interface UsePendingQuestionInput {
  /** Null where no turn is waiting: the hook then reports null too. */
  readonly question: Question | null;
  readonly control: PendingControl | undefined;
  readonly disabled: boolean;
  readonly onAnswer: (answers: Answers) => void;
  readonly onRetry: ((entry: PendingControl) => void) | undefined;
  readonly onDismiss: (question: Question) => void;
}

export function usePendingQuestion(input: UsePendingQuestionInput): PendingQuestionState | null {
  const { question, control } = input;
  const pending = React.useMemo(
    () => (question === null ? null : toPendingUserInput(question)),
    [question],
  );
  const [answers, setAnswers] = React.useState<Record<string, PendingUserInputDraftAnswer>>({});
  const [questionIndex, setQuestionIndex] = React.useState(0);
  const questionId = question?.id ?? null;

  // A new question starts its own drafts: the panel is keyed by request id
  // upstream, and the index must not survive the change either.
  React.useEffect(() => {
    setAnswers({});
    setQuestionIndex(0);
  }, [questionId]);

  const questions = pending?.questions ?? EMPTY_QUESTIONS;
  const progress = derivePendingUserInputProgress(questions, answers, questionIndex);
  const locked = question === null ? false : questionLocked(question, control);
  const inFlight = control?.status === "sending" || control?.status === "queued";
  const responding = inFlight || input.disabled;

  const onToggleOption = React.useCallback(
    (promptId: string, optionValue: string) => {
      const target = questions.find((candidate) => candidate.id === promptId);
      if (target === undefined) return;
      setAnswers((previous) => ({
        ...previous,
        [promptId]: togglePendingUserInputOptionSelection(target, previous[promptId], optionValue),
      }));
    },
    [questions],
  );

  const onCustomAnswerChange = React.useCallback(
    (text: string) => {
      const active = progress.activeQuestion;
      if (active === null) return;
      setAnswers((previous) => ({
        ...previous,
        [active.id]: setPendingUserInputCustomAnswer(previous[active.id], text),
      }));
    },
    [progress.activeQuestion],
  );

  const onAdvance = React.useCallback(() => {
    if (!progress.canAdvance || responding || locked) return;
    if (progress.isLastQuestion) {
      const built = buildPendingUserInputAnswers(questions, answers);
      if (built !== null) input.onAnswer(toDetentAnswers(built));
      return;
    }
    setQuestionIndex(progress.questionIndex + 1);
  }, [
    answers,
    input,
    locked,
    progress.canAdvance,
    progress.isLastQuestion,
    progress.questionIndex,
    questions,
    responding,
  ]);

  const onPrevious = React.useCallback(() => {
    setQuestionIndex((index) => Math.max(0, index - 1));
  }, []);

  const onDismiss = React.useCallback(() => {
    if (question !== null) input.onDismiss(question);
  }, [input, question]);

  if (question === null || pending === null) return null;

  return {
    pending,
    respondingRequestIds: responding ? [question.id] : [],
    answers,
    questionIndex: progress.questionIndex,
    onToggleOption,
    customAnswer: progress.customAnswer,
    onCustomAnswerChange,
    acceptsCustomAnswer: progress.activeQuestion?.allowCustomAnswer !== false,
    placeholder:
      progress.activeQuestion?.allowCustomAnswer === false
        ? "Choose an option above"
        : "Type your own answer, or leave this blank to use the selected option",
    onAdvance,
    onPrevious,
    onDismiss,
    action: {
      questionIndex: progress.questionIndex,
      isLastQuestion: progress.isLastQuestion,
      canAdvance: progress.canAdvance,
      isResponding: responding,
      isComplete: progress.isComplete,
    },
    receipt: receiptSentence(question, control),
    lockedSentence: locked
      ? question.status === "expired"
        ? "This question expired. The runner stopped waiting for an answer."
        : "This question is closed. It cannot be answered again."
      : null,
    retry:
      control?.status === "unknown" && input.onRetry !== undefined
        ? () => input.onRetry?.(control)
        : null,
  };
}
