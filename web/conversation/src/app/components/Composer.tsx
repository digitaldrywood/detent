// Composes the conversation editor, attachments, pending questions, and turn controls.
// Commands and delivery state come from the hub adapters.
import { PaperclipIcon, PlugZapIcon, XIcon } from "lucide-react";
import React from "react";

import type { PreferenceChoices, TurnPreferences } from "../../contracts/index.ts";
import { cn } from "../../lib/utils.ts";
import { Button } from "../../components/ui/button.tsx";
import { RefreshIcon } from "../../components/ui/refresh-icon.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { ComposerBanner } from "../../components/chat/ComposerBanner.tsx";
import {
  ComposerBannerStack,
  type ComposerBannerStackContent,
  type ComposerBannerStackItem,
} from "../../components/chat/ComposerBannerStack.tsx";
import { ComposerPendingUserInputPanel } from "../../components/chat/ComposerPendingUserInputPanel.tsx";
import { ComposerPrimaryActions } from "../../components/chat/ComposerPrimaryActions.tsx";
import { ComposerSurface } from "../../components/chat/ComposerSurface.tsx";
import { ProviderStatusBanner } from "../../components/chat/ProviderStatusBanner.tsx";
import { PierreEntryIcon } from "../../components/chat/PierreEntryIcon.tsx";
import { useTheme } from "../adapters/theme.ts";
import {
  isInsideCollapsedComposerControls,
  isInsideRestingComposerControlScope,
} from "../../components/chat/composerEventScope.ts";
import {
  createComposerScrollGestureState,
  recordComposerScrollGestureEvent,
  resetComposerScrollGesture,
  shouldCollapseComposerForScrollKey,
} from "../../components/chat/composerScrollGesture.ts";
import {
  buildComposerPromptHistoryEntries,
  stepComposerPromptHistory,
  type ComposerPromptHistoryMessage,
  type ComposerPromptHistoryPosition,
} from "../../components/chat/composerPromptHistory.ts";
import {
  shouldUseCompactComposerFooter,
  shouldUseCompactComposerPrimaryActions,
} from "../../components/composerFooterLayout.ts";
import { formatAttachmentSize } from "../../runtime/state/attachments.ts";
import type { ComposerAttachments } from "../adapters/attachments.ts";
import type { PendingQuestionState } from "../adapters/pendingQuestions.ts";
import {
  builtinSlashCommands,
  filterSlashCommands,
  resolveSlashCommands,
  stepSlashHighlight,
  type SlashCommand,
} from "../adapters/slashCommands.ts";
import {
  ComposerPreferenceControls,
  type PreferenceField,
  type PreferenceOpenRequest,
} from "./ComposerPreferenceControls.tsx";
import {
  ComposerPromptEditor,
  type ComposerPromptEditorHandle,
  type SlashMenuKey,
} from "./ComposerPromptEditor.tsx";
import { SlashCommandMenu, slashOptionId } from "./SlashCommandMenu.tsx";

export type ComposerBannerEntry = ComposerBannerStackItem | ComposerBannerStackContent;

export interface ComposerProps {
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly onSend: () => void;
  readonly onStop?: () => void;
  /** True while a send is in flight: the draft stays, the send is disabled. */
  readonly sending: boolean;
  /** True while a turn is producing output: send becomes stop. */
  readonly streaming: boolean;
  readonly placeholder?: string;
  /** Stated reason the send is unavailable, e.g. a lost connection. */
  readonly blockedReason?: string | null;
  /**
   * True where there is nothing to draft: a viewer who cannot write, or a
   * settled chat. A blocked reason on its own leaves the editor usable,
   * because a lost connection is temporary and the draft is worth keeping; a
   * reader who may never send is not offered the keystrokes at all
   * (decisions.md §10.11, §10.12).
   */
  readonly disabled?: boolean;
  readonly autoFocus?: boolean;
  readonly label?: string;
  /** The turn preferences the three footer pickers show (§13.14, §14). */
  readonly preferences: TurnPreferences;
  /** The options the bootstrap publishes for them. */
  readonly preferenceChoices?: PreferenceChoices | undefined;
  readonly onPreferencesChange: (preferences: TurnPreferences) => void;

  readonly contextStrip?: React.ReactNode;
  /** The editor handle, so a test or the shell can drive the prompt. */
  readonly editorRef?: React.Ref<ComposerPromptEditorHandle>;

  readonly attachments?: ComposerAttachments;

  readonly banners?: readonly ComposerBannerEntry[];

  readonly pendingQuestion?: PendingQuestionState | null;

  readonly history?: readonly ComposerPromptHistoryMessage[];

  readonly footerControls?: "pickers" | "none";

  readonly attachControl?: "attach" | "none";
  /**
   * What sits at the head of the footer's left row, before the pickers. The
   * issue page's mode toggle lives here rather than in a strip of its own.
   */
  readonly footerLeading?: React.ReactNode;
  /**
   * The surface's own slash commands, appended to the ones the composer
   * provides for itself. A name that matches a built-in replaces it.
   */
  readonly slashCommands?: readonly SlashCommand[];
  /**
   * The element that describes the composer, when the surface renders one.
   * No surface does today — the shortcut hint line that used to sit under the
   * card is gone — and an `aria-describedby` pointing at nothing is worse than
   * none at all, so the default is none.
   */
  readonly describedBy?: string | null;

  readonly scrollRef?: React.RefObject<HTMLElement | null>;
  /**
   * The prefix for this composer's own element ids. Two composers can share a
   * page — the issue page docks one for comments while the right panel holds
   * the conversation's — and an `id` is a document-wide name, so the editor's
   * id is per composer rather than per file.
   */
  readonly domId?: string;
}

const COLLAPSE_THRESHOLD_PX = 24;
const GESTURE_RESET_MS = 120;

const NO_STAGED: readonly [] = [];
const NO_BANNERS: readonly ComposerBannerEntry[] = [];
const NO_HISTORY: readonly ComposerPromptHistoryMessage[] = [];
const NO_SLASH_COMMANDS: readonly SlashCommand[] = [];

export function Composer(props: ComposerProps): React.ReactElement {

  const { resolvedTheme } = useTheme();
  const disabled = props.disabled === true;
  const attachments = props.attachments;
  const staged = attachments?.staged ?? NO_STAGED;
  const attachControl = props.attachControl ?? "attach";
  const attachingBlocked = attachControl === "none" || attachments === undefined || disabled;
  const fileInput = React.useRef<HTMLInputElement>(null);
  const formRef = React.useRef<HTMLFormElement>(null);
  // A draft with a staged file is sendable: the first send uploads it.
  const hasContent = props.value.trim().length > 0 || staged.length > 0;
  const canSend =
    hasContent &&
    !props.sending &&
    props.blockedReason == null &&
    !disabled &&
    attachments?.uploading !== true;
  const label = props.label ?? "Message";
  const domId = props.domId ?? "dc-composer";
  const footerControls = props.footerControls ?? "pickers";
  const describedBy = props.describedBy ?? null;
  const images = staged.filter((entry) => entry.mime.toLowerCase().startsWith("image/"));
  const otherFiles = staged.filter((entry) => !entry.mime.toLowerCase().startsWith("image/"));
  const pending = props.pendingQuestion ?? null;

  const unavailableReason = disabled ? (props.blockedReason ?? null) : null;
  const banners = React.useMemo(() => {
    const rail = props.banners ?? NO_BANNERS;
    const reason = props.blockedReason;
    if (disabled || reason == null || reason.length === 0) return rail;
    return [
      ...rail,
      {
        id: "composer-blocked",
        variant: "warning" as const,
        priority: "urgent" as const,
        icon: <PlugZapIcon />,
        title: reason,
      },
    ];
  }, [disabled, props.banners, props.blockedReason]);

  const [footerWidth, setFooterWidth] = React.useState<number | null>(null);
  React.useEffect(() => {
    const element = formRef.current;
    if (element === null || typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry !== undefined) setFooterWidth(entry.contentRect.width);
    });
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  const compactFooter = shouldUseCompactComposerFooter(footerWidth);
  const compactActions = shouldUseCompactComposerPrimaryActions(footerWidth);

  const [scrollCollapsed, setScrollCollapsed] = React.useState(false);
  const scrollElement = props.scrollRef;
  React.useEffect(() => {
    const element = scrollElement?.current ?? null;
    if (element === null) return;

    const gesture = createComposerScrollGestureState();
    const atEnd = () =>
      element.scrollHeight - element.clientHeight - element.scrollTop <= COLLAPSE_THRESHOLD_PX;
    let reset: ReturnType<typeof setTimeout> | null = null;
    const onWheel = (event: WheelEvent) => {
      if (reset !== null) clearTimeout(reset);
      reset = setTimeout(() => resetComposerScrollGesture(gesture), GESTURE_RESET_MS);

      const deltaPx =
        Math.abs(event.deltaY) *
        (event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? element.clientHeight : 1);
      const collapse = recordComposerScrollGestureEvent(gesture, {
        now: Date.now(),
        deltaPx,
        collapseThresholdPx: COLLAPSE_THRESHOLD_PX,
        collapseEligible: element.scrollHeight > element.clientHeight,
        canScrollInGestureDirection: event.deltaY < 0 ? element.scrollTop > 0 : !atEnd(),
        scrollsTowardLogicalEnd: event.deltaY > 0,
      });
      if (collapse) setScrollCollapsed(true);
    };
    const onKey = (event: KeyboardEvent) => {
      if (
        shouldCollapseComposerForScrollKey({
          key: event.key,
          scrollTop: element.scrollTop,
          scrollHeight: element.scrollHeight,
          clientHeight: element.clientHeight,
          isAtLogicalEnd: atEnd(),
        })
      ) {
        setScrollCollapsed(true);
      }
    };
    element.addEventListener("wheel", onWheel, { passive: true });
    element.addEventListener("keydown", onKey);
    return () => {
      if (reset !== null) clearTimeout(reset);
      element.removeEventListener("wheel", onWheel);
      element.removeEventListener("keydown", onKey);
      resetComposerScrollGesture(gesture);
    };
  }, [scrollElement]);
  const controlsHidden = scrollCollapsed;

  const historyEntries = React.useMemo(
    () => buildComposerPromptHistoryEntries(props.history ?? NO_HISTORY),
    [props.history],
  );
  const historyPosition = React.useRef<ComposerPromptHistoryPosition | null>(null);
  const onChange = props.onChange;
  const value = props.value;
  const stepHistory = React.useCallback(
    (direction: "backward" | "forward"): boolean => {
      if (disabled) return false;
      const step = stepComposerPromptHistory({
        direction,
        entries: historyEntries,
        position: historyPosition.current,
        currentPrompt: value,
      });
      if (step === null) return false;
      historyPosition.current = step.position;
      onChange(step.prompt);
      return true;
    },
    [disabled, historyEntries, onChange, value],
  );

  // The slash menu.
  //
  // The token is whatever the editor reports; the highlight is this file's;
  // and `dismissed` is what Escape sets, so a closed menu stays closed for the
  // token that closed it and comes back the moment the reader types a
  // different one.
  const [slashQuery, setSlashQuery] = React.useState<string | null>(null);
  const slashQueryRef = React.useRef<string | null>(null);
  const [slashIndex, setSlashIndex] = React.useState(0);
  const [slashDismissed, setSlashDismissed] = React.useState<string | null>(null);
  const [pickerRequest, setPickerRequest] = React.useState<PreferenceOpenRequest | null>(null);

  const openPicker = React.useCallback(
    (field: PreferenceField) =>
      setPickerRequest((previous) => ({ field, nonce: (previous?.nonce ?? 0) + 1 })),
    [],
  );

  const extraCommands = props.slashCommands ?? NO_SLASH_COMMANDS;
  const onStop = props.onStop;
  const streaming = props.streaming;
  const commands = React.useMemo(
    () =>
      resolveSlashCommands(
        builtinSlashCommands({
          openPreference: footerControls === "pickers" && !disabled ? openPicker : null,
          attach: attachingBlocked ? null : () => fileInput.current?.click(),
          clear: disabled ? null : () => onChange(""),
          stop: streaming && onStop !== undefined && !disabled ? () => onStop() : null,
        }),
        extraCommands,
      ),
    [attachingBlocked, disabled, extraCommands, footerControls, onChange, onStop, openPicker, streaming],
  );

  const slashMatches = React.useMemo(
    () => (slashQuery === null ? NO_SLASH_COMMANDS : filterSlashCommands(commands, slashQuery)),
    [commands, slashQuery],
  );
  // A question owns the prompt while it is open (see the editor below), and a
  // token with nothing to run is not a menu: leaving it closed is what keeps
  // Enter on the send where the reader typed something that is not a command.
  const slashOpen =
    slashQuery !== null &&
    slashQuery !== slashDismissed &&
    slashMatches.length > 0 &&
    pending === null &&
    !disabled;

  const clearSlashToken = () => {
    onChange("");
    slashQueryRef.current = null;
    setSlashQuery(null);
    setSlashDismissed(null);
    setSlashIndex(0);
  };

  const runSlash = (command: SlashCommand | undefined) => {
    if (command === undefined) return;
    // The token goes first: a command that opens a picker or a dialog must not
    // leave `/model` sitting in the draft behind it.
    clearSlashToken();
    command.run();
  };

  // Rebuilt every render and read through a ref inside the editor, so the
  // plugin below registers once and still sees the current highlight.
  const slashBridge = {
    onQueryChange: (query: string | null) => {
      if (slashQueryRef.current === query) return;
      slashQueryRef.current = query;
      setSlashQuery(query);
      // A new token is a new question: the highlight goes back to the top and
      // an Escape that silenced the previous one does not silence this one.
      setSlashIndex(0);
      setSlashDismissed(null);
    },
    onKey: (key: SlashMenuKey): boolean => {
      if (!slashOpen) return false;
      switch (key) {
        case "escape":
          setSlashDismissed(slashQuery);
          return true;
        case "up":
        case "down":
          setSlashIndex((index) => stepSlashHighlight(slashMatches.length, index, key));
          return true;
        case "enter":
        case "tab":
          runSlash(slashMatches[slashIndex] ?? slashMatches[0]);
          return true;
      }
    },
  };

  const highlighted = slashIndex < slashMatches.length ? slashIndex : 0;
  const highlightedCommand = slashMatches[highlighted];

  const submit = () => {
    if (pending !== null) {
      pending.onAdvance();
      return;
    }
    if (canSend) props.onSend();
  };

  return (
    <ComposerSurface.Shell contextStrip={props.contextStrip !== undefined}>

      <ProviderStatusBanner status={null} onDismiss={() => {}} />
      <ComposerSurface.Host>
        <div className="relative z-10">
          <form
            ref={formRef}
            data-chat-composer-form="true"
            className="mx-auto w-full min-w-0 max-w-3xl"
            onSubmit={(event) => {
              event.preventDefault();
              submit();
            }}
            onPointerDownCapture={(event) => {
              if (isInsideRestingComposerControlScope(event.target)) return;
              if (isInsideCollapsedComposerControls(event.target)) return;
              setScrollCollapsed(false);
            }}
            onFocusCapture={(event) => {
              if (isInsideCollapsedComposerControls(event.target)) return;
              setScrollCollapsed(false);
            }}
          >

            <ComposerBanner.Dock>
              <ComposerBanner.Column>
                <ComposerBannerStack className="relative z-0" items={banners} />
                {unavailableReason === null ? null : (
                  <ComposerBanner.Attachment>
                    <ComposerBanner.Root variant="warning" role="status">
                      <ComposerBanner.Row>
                        <ComposerBanner.Icon>
                          <PlugZapIcon />
                        </ComposerBanner.Icon>
                        <ComposerBanner.Content>
                          <span className="min-w-0 font-medium">{unavailableReason}</span>
                        </ComposerBanner.Content>
                      </ComposerBanner.Row>
                    </ComposerBanner.Root>
                  </ComposerBanner.Attachment>
                )}
                {pending === null ? null : (
                  <ComposerBanner.Attachment>
                    <ComposerBanner.Root data-chat-composer-top-drawer="true" variant="info">
                      {/* Once the question has a winner the options go, because
                          there is nothing left to choose (decisions.md §5);
                          the receipt below stays, because the reader is still
                          owed the answer to "did mine land?". */}
                      {pending.lockedSentence !== null ? null : (
                        <ComposerPendingUserInputPanel
                          pendingUserInputs={[pending.pending]}
                          respondingRequestIds={pending.respondingRequestIds}
                          answers={pending.answers}
                          questionIndex={pending.questionIndex}
                          onToggleOption={pending.onToggleOption}
                          onAdvance={pending.onAdvance}
                          onDismiss={pending.onDismiss}
                        />
                      )}

                      {pending.receipt === null &&
                      pending.retry === null &&
                      pending.lockedSentence === null ? null : (
                        <ComposerBanner.Body className="pt-0 pe-1 pb-1">
                          <div className="flex min-w-0 items-center gap-2">
                            {pending.receipt === null ? null : (
                              <span
                                className="min-w-0 flex-1 text-muted-foreground text-xs"
                                role="status"
                                data-testid="question-receipt"
                              >
                                {pending.receipt}
                              </span>
                            )}
                            {pending.retry === null ? null : (
                              <Button
                                type="button"
                                size="xs"
                                variant="outline"
                                onClick={pending.retry}
                              >
                                Retry same answer
                              </Button>
                            )}
                          </div>
                          {pending.lockedSentence === null ? null : (
                            <p
                              className="mt-1 text-muted-foreground text-xs"
                              data-testid="question-locked"
                            >
                              {pending.lockedSentence}
                            </p>
                          )}
                        </ComposerBanner.Body>
                      )}
                    </ComposerBanner.Root>
                  </ComposerBanner.Attachment>
                )}
              </ComposerBanner.Column>
            </ComposerBanner.Dock>

            <div className="relative">
              <ComposerSurface.Main>
                <div
                  data-chat-composer-surface="true"
                  className="rounded-[20px] transition-[background-color] duration-200"
                >
                  {/* The slash list is part of this card, at its top, above
                      the editor — not a popup over the transcript (Michael's
                      review, September 12). It renders nothing when closed, so
                      the card is its normal height until a `/` opens it. */}
                  <SlashCommandMenu
                    open={slashOpen}
                    commands={slashMatches}
                    highlighted={highlighted}
                    domId={domId}
                    onHighlight={setSlashIndex}
                    onRun={(command) => runSlash(command)}
                  />
                  <div
                    data-chat-composer-body="true"
                    className="relative px-3 pt-3.5 pb-2 sm:px-4 sm:pt-4"
                  >

                    {images.length === 0 ? null : (
                      <div
                        className="mb-3 flex max-w-full flex-wrap gap-2"
                        data-testid="composer-attachment-images"
                      >
                        {images.map((image) => (
                          <div
                            key={image.localId}
                            data-chat-composer-expanded-image="true"
                            data-testid="composer-attachment"
                            className="group/attachment relative h-16 w-16 shrink-0 snap-start overflow-hidden rounded-lg border border-border/80 bg-background"
                          >
                            {image.previewUrl === null ? (
                              <div className="flex h-full w-full items-center justify-center px-1 text-center text-[10px] text-secondary-label">
                                {image.name}
                              </div>
                            ) : (
                              <img
                                src={image.previewUrl}
                                alt={image.name}
                                className="h-full w-full object-cover"
                              />
                            )}
                            {image.status !== "uploading" ? null : (
                              <span className="pointer-events-none absolute inset-x-0 bottom-0 bg-background/85 px-1 text-center text-[10px] text-foreground">
                                Uploading…
                              </span>
                            )}
                            {image.status !== "failed" ? null : (
                              <Tooltip>
                                <TooltipTrigger
                                  render={
                                    <Button
                                      type="button"
                                      variant="ghost"
                                      size="icon-xs"
                                      className="absolute bottom-1 left-1 bg-background/85 hover:bg-background/95"
                                      onClick={() => attachments?.retry(image.localId)}
                                      aria-label={`Retry upload for ${image.name}`}
                                    />
                                  }
                                >
                                  <RefreshIcon />
                                </TooltipTrigger>
                                <TooltipPopup
                                  side="top"
                                  className="max-w-64 whitespace-normal leading-tight"
                                >
                                  {image.error ?? "The upload failed."}
                                </TooltipPopup>
                              </Tooltip>
                            )}
                            <Button
                              type="button"
                              variant="ghost"
                              size="icon-xs"
                              className="absolute right-1 top-1 bg-background/80 hover:bg-background/90"
                              onClick={() => attachments?.remove(image.localId)}
                              aria-label={`Remove ${image.name}`}
                            >
                              <XIcon />
                            </Button>
                          </div>
                        ))}
                      </div>
                    )}

                    {otherFiles.length === 0 ? null : (
                      <div
                        className="mb-3 flex flex-col gap-1"
                        data-testid="composer-attachment-files"
                      >
                        {otherFiles.map((file) => (
                          <div
                            key={file.localId}
                            data-testid="composer-attachment"
                            className="flex min-w-0 items-center gap-2 py-1 text-foreground text-sm"
                          >
                            <PierreEntryIcon
                              pathValue={file.name}
                              kind="file"
                              theme={resolvedTheme}
                            />
                            <span className="min-w-0 flex-1 truncate">{file.name}</span>
                            <span className="shrink-0 text-secondary-label text-xs">
                              {file.status === "uploading"
                                ? "Uploading…"
                                : formatAttachmentSize(file.size)}
                            </span>
                            {file.status !== "failed" ? null : (
                              <Tooltip>
                                <TooltipTrigger
                                  render={
                                    <Button
                                      type="button"
                                      variant="ghost"
                                      size="icon-xs"
                                      onClick={() => attachments?.retry(file.localId)}
                                      aria-label={`Retry upload for ${file.name}`}
                                    />
                                  }
                                >
                                  <RefreshIcon />
                                </TooltipTrigger>
                                <TooltipPopup
                                  side="top"
                                  className="max-w-64 whitespace-normal leading-tight"
                                >
                                  {file.error ?? "The upload failed."}
                                </TooltipPopup>
                              </Tooltip>
                            )}
                            <Button
                              type="button"
                              variant="ghost"
                              size="icon-xs"
                              onClick={() => attachments?.remove(file.localId)}
                              aria-label={`Remove ${file.name}`}
                            >
                              <XIcon />
                            </Button>
                          </div>
                        ))}
                      </div>
                    )}

                    <ComposerPromptEditor
                      {...(props.editorRef === undefined ? {} : { editorRef: props.editorRef })}
                      id={`${domId}-input`}
                      value={pending === null ? props.value : pending.customAnswer}
                      onChange={pending === null ? props.onChange : pending.onCustomAnswerChange}
                      onSubmit={submit}
                      {...(pending === null ? { onHistoryStep: stepHistory } : {})}
                      placeholder={
                        pending !== null
                          ? pending.placeholder
                          : (props.placeholder ?? "Ask for changes, or send a follow-up")
                      }
                      ariaLabel={pending === null ? label : "Your answer"}
                      {...(describedBy === null ? {} : { ariaDescribedBy: describedBy })}
                      slash={slashBridge}
                      slashListId={slashOpen ? `${domId}-slash-list` : null}
                      slashActiveId={
                        slashOpen && highlightedCommand !== undefined
                          ? slashOptionId(domId, highlightedCommand.name)
                          : null
                      }
                      disabled={disabled || (pending !== null && !pending.acceptsCustomAnswer)}
                      autoFocus={props.autoFocus === true}
                    />
                  </div>

                  <div
                    data-chat-composer-footer="true"
                    className={cn(
                      "flex min-w-0 flex-nowrap items-center justify-between gap-2 overflow-visible px-3 pb-3 sm:px-4 sm:pb-4",
                      "gap-2 sm:gap-0",
                    )}
                  >

                    <div
                      data-chat-composer-controls="left"
                      data-chat-composer-footer-controls="true"
                      data-testid="composer-scope-chips"
                      aria-hidden={controlsHidden || undefined}
                      className={cn(
                        "-m-1 -ms-3.5 flex min-w-0 flex-1 items-center gap-1 overflow-x-auto p-1 ps-3.5 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden",
                        controlsHidden && "invisible",
                      )}
                    >
                      {props.footerLeading}
                      {footerControls === "none" ? null : (
                        <ComposerPreferenceControls
                          preferences={props.preferences}
                          choices={props.preferenceChoices}
                          onChange={props.onPreferencesChange}
                          disabled={disabled}
                          hidden={controlsHidden}
                          size={compactFooter ? "xs" : "sm"}
                          openRequest={pickerRequest}
                        />
                      )}
                    </div>

                    <div
                      data-chat-composer-actions="right"
                      data-chat-composer-transition-actions="true"
                      className="flex shrink-0 flex-nowrap items-center justify-end gap-2"
                    >

                      {attachControl === "none" ? null : attachingBlocked ? (
                        <Tooltip>
                          {/* The trigger wraps rather than renders the button: a
                              disabled button emits no pointer events, so the
                              tooltip that explains why would never open. */}
                          <TooltipTrigger
                            render={
                              <span
                                className="inline-flex"
                                tabIndex={0}
                                role="note"
                                aria-label="Attach files: attachments start once the chat exists"
                              />
                            }
                          >
                            <Button
                              type="button"
                              variant="ghost"
                              size="icon-sm"
                              disabled
                              aria-disabled="true"
                              onPointerDown={(event) => event.preventDefault()}
                              aria-label="Attach files"
                              data-testid="composer-attach"
                            >
                              <PaperclipIcon />
                            </Button>
                          </TooltipTrigger>
                          <TooltipPopup>
                            {disabled
                              ? "Sending is unavailable"
                              : "Attachments start once the chat exists"}
                          </TooltipPopup>
                        </Tooltip>
                      ) : (
                        <>

                          <input
                            ref={fileInput}
                            type="file"
                            multiple
                            className="hidden"
                            data-testid="composer-attach-input"
                            onChange={(event) => {
                              const picked = Array.from(event.currentTarget.files ?? []);
                              event.currentTarget.value = "";
                              attachments?.addFiles(picked);
                            }}
                          />
                          <Tooltip>
                            <TooltipTrigger
                              render={
                                <Button
                                  type="button"
                                  variant="ghost"
                                  size="icon-sm"
                                  onPointerDown={(event) => event.preventDefault()}
                                  onClick={() => fileInput.current?.click()}
                                  aria-label="Attach files"
                                  data-testid="composer-attach"
                                />
                              }
                            >
                              <PaperclipIcon />
                            </TooltipTrigger>
                            <TooltipPopup>Attach files</TooltipPopup>
                          </Tooltip>
                        </>
                      )}
                      <ComposerPrimaryActions
                        compact={compactActions}
                        pendingAction={pending === null ? null : pending.action}
                        isRunning={props.streaming && props.onStop !== undefined && !disabled}

                        showPlanFollowUpPrompt={false}
                        promptHasText={props.value.trim().length > 0}
                        isSendBusy={props.sending}
                        sendDisabledReason={
                          disabled
                            ? (props.blockedReason ?? "Sending is unavailable")
                            : attachments?.uploading === true
                              ? "Waiting for the upload to finish"
                              : (props.blockedReason ?? null)
                        }
                        // Detent reports a lost connection as a blocked reason
                        // rather than as its own spinner state.
                        isConnecting={false}
                        isEnvironmentUnavailable={disabled}
                        isPreparingWorktree={false}
                        hasSendableContent={hasContent}
                        preserveComposerFocusOnPointerDown
                        // A Detent turn can be steered while it runs, so send
                        // stays beside stop rather than replacing it.
                        showSendWhileRunning
                        onPreviousPendingQuestion={() => pending?.onPrevious()}
                        onInterrupt={() => props.onStop?.()}
                        onImplementPlanInNewThread={() => {}}
                      />
                    </div>
                  </div>
                </div>
              </ComposerSurface.Main>
            </div>
          </form>
        </div>
      </ComposerSurface.Host>
      {props.contextStrip === undefined ? null : (
        <div className="min-h-0">
          <div className="relative z-0">
            <div className="pointer-events-auto">{props.contextStrip}</div>
          </div>
        </div>
      )}
    </ComposerSurface.Shell>
  );
}
