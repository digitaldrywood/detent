import React from "react";

import type { HandoffNext, IssueProposal } from "../../contracts/index.ts";
import { PRIORITY_NAMES } from "../../contracts/work.ts";
import { Button } from "../../components/ui/button.tsx";
import {
  Dialog,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../components/ui/dialog.tsx";
import { Label } from "../../components/ui/label.tsx";

export interface HandoffValues {
  readonly title: string;
  readonly description: string;
  readonly labels: readonly string[];
  /** The tracker's own rank, 0-3, or null to leave it to the project. */
  readonly priority: number | null;
  readonly shareHistory: boolean;
  /**
   * What happens to the issue once it exists (decisions.md §13.8, §14).
   * Always present: the form preselects the project's defaults, so the reader
   * is answering a question that has already been answered sensibly rather
   * than filling in a blank.
   */
  readonly next: HandoffNext;
}

/** The project's workflow, as much of it as this form needs. */
export interface HandoffState {
  readonly name: string;
  readonly terminal?: boolean | undefined;
  readonly dispatchable?: boolean | undefined;
}

/**
 * The next step the form opens on: the first dispatchable state and `later` —
 * creating an issue from a chat is a handoff, not a launch, so the reader has
 * to ask for the launch (§13.8). Priority is left unset, which is how the
 * request asks the hub for the project's own default rather than guessing it.
 */
export function defaultNext(states: readonly HandoffState[] | undefined): HandoffNext {
  const dispatchable = (states ?? []).find((state) => state.dispatchable === true);
  const first = dispatchable ?? (states ?? [])[0];
  return {
    ...(first === undefined ? {} : { state: first.name }),
    dispatch: "later",
  };
}

export interface HandoffFailure {
  readonly code: string;
  readonly message: string;
  /** Set for `conversation_already_linked`. */
  readonly existingConversationId?: string | null;
}

export interface HandoffFormProps {
  readonly projectId: string;
  readonly projectName: string;
  /**
   * How much history becomes readable: the conversation's `message_count`,
   * which is the whole history rather than the loaded page (decisions.md
   * §10.5). Stated, never estimated.
   */
  readonly messageCount: number;
  readonly proposal?: IssueProposal | null;
  /** Seed for the objective when there is no proposal: the opening message. */
  readonly seedTitle?: string;
  readonly seedObjective?: string;
  readonly labels?: readonly string[];
  /** The project's workflow states, for the next step's lane (§14). */
  readonly states?: readonly HandoffState[];
  readonly submitting: boolean;
  readonly failure: HandoffFailure | null;
  readonly onSubmit: (values: HandoffValues) => void;
  readonly onCancel: () => void;
  readonly onOpenExisting?: (conversationId: string) => void;
}

export const TITLE_MAX_LENGTH = 180;

const FIELD_CLASS =
  "w-full rounded-lg border border-input bg-background px-[calc(--spacing(3)-1px)] py-1.5 text-foreground text-sm shadow-xs/5 outline-none placeholder:text-placeholder focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/24 dark:bg-input/32";

export function HandoffForm(props: HandoffFormProps): React.ReactElement {
  const proposal = props.proposal ?? null;
  const [title, setTitle] = React.useState(() => proposal?.title ?? props.seedTitle ?? "");
  const [description, setDescription] = React.useState(
    () => proposal?.objective ?? props.seedObjective ?? "",
  );
  const [labels, setLabels] = React.useState<readonly string[]>([]);
  const [next, setNext] = React.useState<HandoffNext>(() => defaultNext(props.states));
  const [shareHistory, setShareHistory] = React.useState(false);
  const [localError, setLocalError] = React.useState<string | null>(null);

  const titleField = React.useRef<HTMLInputElement>(null);
  const shareField = React.useRef<HTMLInputElement>(null);

  React.useEffect(() => {
    titleField.current?.focus();
  }, []);

  // `share_history_required` is the hub saying the confirmation is missing,
  // so the confirmation is where the user is put.
  React.useEffect(() => {
    if (props.failure?.code === "share_history_required") shareField.current?.focus();
  }, [props.failure]);

  const alreadyLinked =
    props.failure?.code === "conversation_already_linked"
      ? (props.failure.existingConversationId ?? null)
      : null;

  const complete = title.trim().length > 0 && description.trim().length > 0;

  const submit = (event: React.FormEvent) => {
    event.preventDefault();
    if (!complete || props.submitting) return;
    if (!shareHistory) {
      setLocalError(
        "Linking shares this conversation's full history with the project. Confirm before linking.",
      );
      shareField.current?.focus();
      return;
    }
    setLocalError(null);
    props.onSubmit({
      title: title.trim(),
      description: description.trim(),
      labels,
      priority: next.priority ?? null,
      shareHistory: true,
      next,
    });
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onCancel();
      }}
    >
      <DialogPopup
        aria-label="Create linked issue"
        className="max-w-xl"
        showCloseButton={false}
        // The share confirmation is where the hub sends the reader back to
        // when it is missing; anywhere else the title is the first field.
        initialFocus={props.failure?.code === "share_history_required" ? shareField : titleField}
      >
        {/* The popup is a flex column and its panel scrolls; the form has to
            be that column itself, or the footer falls out of the card. */}
        <form
          className="flex min-h-0 flex-1 flex-col"
          data-testid="handoff-form"
          onSubmit={submit}
        >
          <DialogHeader>
            <DialogTitle>Create linked issue</DialogTitle>
          </DialogHeader>

          <DialogPanel className="flex min-h-0 flex-col gap-4">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="dc-handoff-project">Project</Label>
              {/* Fixed, not disabled: a disabled control reads as "not yet",
                  and this one is never going to change (decisions.md §3 rule
                  5). */}
              <input
                id="dc-handoff-project"
                type="text"
                className={FIELD_CLASS}
                value={props.projectName}
                readOnly
                aria-describedby="dc-handoff-project-note"
              />
              <p className="text-muted-foreground text-xs" id="dc-handoff-project-note">
                A conversation stays in the project it started in.
              </p>
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="dc-handoff-title">Title</Label>
              <input
                id="dc-handoff-title"
                ref={titleField}
                type="text"
                className={FIELD_CLASS}
                required
                maxLength={TITLE_MAX_LENGTH}
                value={title}
                onChange={(event) => setTitle(event.target.value)}
              />
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="dc-handoff-objective">Objective</Label>
              <textarea
                id="dc-handoff-objective"
                className={FIELD_CLASS}
                required
                rows={5}
                value={description}
                onChange={(event) => setDescription(event.target.value)}
              />
            </div>

            {props.labels === undefined || props.labels.length === 0 ? null : (
              <fieldset className="flex min-w-0 flex-col gap-1.5 border-0 p-0">
                <legend className="font-medium text-foreground text-sm">Labels (optional)</legend>
                <div className="flex flex-wrap gap-2">
                  {props.labels.map((label) => (
                    <label
                      key={label}
                      className="flex cursor-pointer items-center gap-1.5 rounded-md border border-border px-2 py-1 text-sm"
                    >
                      <input
                        type="checkbox"
                        className="size-3.5 accent-primary"
                        value={label}
                        checked={labels.includes(label)}
                        onChange={(event) =>
                          setLabels((current) =>
                            event.target.checked
                              ? [...current, label]
                              : current.filter((value) => value !== label),
                          )
                        }
                      />
                      <span>{label}</span>
                    </label>
                  ))}
                </div>
              </fieldset>
            )}

            {/* The next step (decisions.md §13.8, §14). Creating an issue from
                a chat always ends with the same three questions, and Detent
                knows sensible answers to all three, so they are preselected
                rather than asked from a blank. */}
            <fieldset
              className="flex min-w-0 flex-col gap-3 border-0 p-0"
              data-testid="handoff-next"
            >
              <legend className="font-medium text-foreground text-sm">What happens next</legend>

              {props.states === undefined || props.states.length === 0 ? null : (
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="dc-handoff-state">Lane</Label>
                  <select
                    id="dc-handoff-state"
                    className={FIELD_CLASS}
                    value={next.state}
                    onChange={(event) =>
                      setNext((current) => ({ ...current, state: event.target.value }))
                    }
                  >
                    {props.states.map((state) => (
                      <option key={state.name} value={state.name}>
                        {state.name}
                      </option>
                    ))}
                  </select>
                </div>
              )}

              {/* The hub takes the native rank, 0-3 (§14), so the options are
                  the tracker's own ranks under its own names. */}
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="dc-handoff-priority">Priority</Label>
                <select
                  id="dc-handoff-priority"
                  className={FIELD_CLASS}
                  value={next.priority === undefined ? "" : String(next.priority)}
                  onChange={(event) =>
                    setNext((current) => {
                      const { priority: _priority, ...rest } = current;
                      return event.target.value === ""
                        ? rest
                        : { ...rest, priority: Number(event.target.value) };
                    })
                  }
                >
                  <option value="">Leave to the project default</option>
                  {PRIORITY_NAMES.map((name, rank) => (
                    <option key={name} value={String(rank)}>
                      {name}
                    </option>
                  ))}
                </select>
              </div>

              <div className="flex flex-col gap-1.5">
                <Label htmlFor="dc-handoff-dispatch">Dispatch</Label>
                <select
                  id="dc-handoff-dispatch"
                  className={FIELD_CLASS}
                  value={next.dispatch}
                  onChange={(event) =>
                    setNext((current) => ({
                      ...current,
                      dispatch: event.target.value === "now" ? "now" : "later",
                    }))
                  }
                >
                  <option value="later">Later — leave it for someone to pick up</option>
                  <option value="now">Now — put it in front of the next free runner</option>
                </select>
              </div>
            </fieldset>

            <div
              className="flex flex-col gap-1.5 rounded-lg border border-warning/32 bg-warning-surface p-3"
              data-testid="handoff-audience"
            >
              <h3 className="font-medium text-sm">Who will be able to read this</h3>
              <p className="text-sm" data-testid="handoff-message-count">
                All {props.messageCount} messages in this chat become readable by everyone who can
                read project {props.projectName}.
              </p>
              <p className="text-muted-foreground text-xs">Sharing cannot be undone.</p>
              <label className="mt-1 flex cursor-pointer items-start gap-2 text-sm">
                <input
                  ref={shareField}
                  type="checkbox"
                  className="mt-0.5 size-3.5 accent-primary"
                  checked={shareHistory}
                  onChange={(event) => {
                    setShareHistory(event.target.checked);
                    if (event.target.checked) setLocalError(null);
                  }}
                />
                <span>Share this conversation&apos;s history with the project</span>
              </label>
            </div>

            {localError === null ? null : (
              <div
                className="rounded-md bg-error-surface px-3 py-2 text-error-foreground text-sm"
                role="alert"
                data-testid="handoff-local-error"
              >
                {localError}
              </div>
            )}

            {props.failure === null ? null : (
              <div
                className="flex flex-col gap-2 rounded-md bg-error-surface px-3 py-2 text-error-foreground text-sm"
                role="alert"
                data-testid="handoff-failure"
              >
                <span>{props.failure.message}</span>
                {alreadyLinked === null ? null : (
                  <Button
                    variant="outline"
                    size="sm"
                    className="self-start"
                    onClick={() => props.onOpenExisting?.(alreadyLinked)}
                  >
                    Open the linked conversation
                  </Button>
                )}
              </div>
            )}
          </DialogPanel>

          <DialogFooter>
            <Button variant="outline" size="sm" onClick={props.onCancel}>
              Cancel
            </Button>
            <Button
              render={<button type="submit" />}
              size="sm"
              disabled={!complete || props.submitting}
              aria-disabled={!complete || props.submitting}
            >
              {props.submitting ? "Creating the issue" : "Create linked issue"}
            </Button>
          </DialogFooter>
        </form>
      </DialogPopup>
    </Dialog>
  );
}
