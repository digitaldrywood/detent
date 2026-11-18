import React from "react";

import { cn } from "../../lib/utils.ts";

import type { Question } from "../../contracts/index.ts";

export interface QuestionRecordProps {
  readonly question: Question;
  /** The sentence saying why it is closed; from `adapters/pendingQuestions.ts`. */
  readonly sentence: string;
}

export function QuestionRecord(props: QuestionRecordProps): React.ReactElement {
  const { question } = props;
  return (
    <div
      className="mx-auto w-full max-w-3xl rounded-xl border border-info/32 bg-info/8 p-3 dark:bg-info/16"
      role="group"
      aria-label="Answered question"
      data-testid="question-card"
      data-locked="true"
    >
      <h2 className="font-medium text-info-foreground text-sm">Runner needed your input</h2>
      {question.owner.attempt_id === null ? null : (
        <p className="mt-0.5 text-muted-foreground text-xs" data-testid="question-attempt">
          attempt {question.owner.attempt_id}
        </p>
      )}
      {question.questions.map((prompt) => {
        const stored = question.answers[prompt.id] ?? [];
        return (
          <div key={prompt.id} className="mt-3 min-w-0">
            <p className="text-foreground/85 text-sm">{prompt.question}</p>
            {prompt.options.length === 0 ? null : (
              <div className="mt-2 flex flex-col gap-0.5">
                {prompt.options.map((option) => {
                  const chosen = stored.includes(option.label);
                  return (
                    <div
                      key={option.label}
                      className={cn(
                        "flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left",
                        chosen
                          ? "bg-muted/55 text-foreground"
                          : "bg-transparent text-foreground/45",
                      )}
                    >
                      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                        <span className="font-medium text-sm">{option.label}</span>
                        {option.description.length === 0 ? null : (
                          <span className="text-[11px] text-secondary-label">
                            {option.description}
                          </span>
                        )}
                      </span>
                    </div>
                  );
                })}
              </div>
            )}
            {stored.length === 0 ? null : (
              <p className="mt-2 text-muted-foreground text-xs" data-testid="stored-answer">
                Answered: {stored.join(", ")}
              </p>
            )}
          </div>
        );
      })}
      <p className="mt-3 text-muted-foreground text-xs" data-testid="question-locked">
        {props.sentence}
      </p>
    </div>
  );
}
