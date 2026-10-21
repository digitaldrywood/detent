import React from "react";
import { Badge } from "../../../components/ui/badge.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { Radio, RadioGroup } from "../../../components/ui/radio-group.tsx";
import { Textarea } from "../../../components/ui/textarea.tsx";
import type { DecisionAnswer, DecisionRequest } from "../../../contracts/decision.ts";

export function DecisionCard({ request, humanAction, parkedBy, canAnswer, onAnswer }: {
  readonly request?: DecisionRequest;
  readonly humanAction?: string;
  readonly parkedBy?: string;
  readonly canAnswer: boolean;
  readonly onAnswer: (answer: DecisionAnswer, state: "Todo" | "Rework") => Promise<void>;
}) {
  const questionId = React.useId();
  const [selection, setSelection] = React.useState(() => String(request?.options.findIndex((option) => option.recommended) ?? -1));
  const [custom, setCustom] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  if (request === undefined && !humanAction) return null;
  const option = request?.options[Number(selection)];
  const customSelected = selection === "custom";
  const state = customSelected ? request?.custom_return_state : option?.return_state;
  const disabled = !canAnswer || saving;

  async function answer(event: React.FormEvent) {
    event.preventDefault();
    if (request === undefined || state === undefined || disabled || customSelected && custom.trim() === "") return;
    setSaving(true);
    setError(null);
    try {
      await onAnswer({ attempt_id: request.attempt_id, ...(customSelected ? { custom: custom.trim() } : { option: Number(selection) }) }, state);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not record the answer. Try again.");
    } finally {
      setSaving(false);
    }
  }

  return (
    <section id="decision" aria-labelledby={questionId} className="rounded-xl border border-border bg-card p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="flex items-center gap-1.5 text-3xs font-medium uppercase tracking-wider text-warning-foreground">
          <span className="size-2 rounded-full bg-warning" aria-hidden="true" />Needs your decision
        </p>
        {parkedBy ? <p className="text-xs text-muted-foreground">{parkedBy}</p> : null}
      </div>
      {request === undefined ? <p id={questionId} className="mt-3 whitespace-pre-wrap text-sm">{humanAction}</p> : (
        <form onSubmit={(event) => void answer(event)}>
          <p className="mt-3 text-sm text-muted-foreground">{request.summary}</p>
          <h2 id={questionId} className="mt-3 text-base font-medium">{request.question}</h2>
          <RadioGroup aria-labelledby={questionId} value={selection} onValueChange={(value) => setSelection(String(value))} disabled={disabled} className="mt-3 gap-0 divide-y divide-border">
            {request.options.map((choice, index) => (
              <label key={index} className="flex cursor-pointer items-start gap-2.5 py-2.5">
                <Radio value={String(index)} />
                <span className="min-w-0 flex-1">
                  <span className="flex flex-wrap items-center gap-1.5 text-base font-medium sm:text-sm">
                    {choice.label}{choice.recommended ? <Badge variant="info" size="sm">Recommended</Badge> : null}
                  </span>
                  <span className="mt-1 block text-xs text-muted-foreground">{choice.consequence}</span>
                </span>
              </label>
            ))}
            {request.custom_allowed ? (
              <div className="py-2.5">
                <label className="flex cursor-pointer items-start gap-2.5">
                  <Radio value="custom" />
                  <span className="text-base font-medium sm:text-sm">Something else</span>
                </label>
                <div className="ml-6.5 mt-1 space-y-2">
                  <p className="text-xs text-muted-foreground">Describe the path you want; it is posted as the answer.</p>
                  <Textarea aria-label="Your answer" size="sm" placeholder="Describe the path you want…" value={custom} disabled={disabled} maxLength={8192} onFocus={() => setSelection("custom")} onChange={(event) => { setSelection("custom"); setCustom(event.target.value); }} />
                </div>
              </div>
            ) : null}
          </RadioGroup>
          {error ? <p role="alert" className="mt-2 text-sm text-destructive-foreground">{error}</p> : null}
          <div className="mt-0 flex items-center justify-between gap-3 border-t border-border pt-3">
            <p className="text-xs text-muted-foreground">Answering posts a comment with your choice and moves this issue to {state ?? request.custom_return_state}.</p>
            <Button type="submit" size="sm" disabled={disabled || state === undefined || customSelected && custom.trim() === ""}>{saving ? "Answering…" : "Answer"}</Button>
          </div>
        </form>
      )}
    </section>
  );
}
