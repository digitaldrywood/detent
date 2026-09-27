import { LexicalComposer } from "@lexical/react/LexicalComposer";
import { useLexicalComposerContext } from "@lexical/react/LexicalComposerContext";
import { ContentEditable } from "@lexical/react/LexicalContentEditable";
import { LexicalErrorBoundary } from "@lexical/react/LexicalErrorBoundary";
import { HistoryPlugin } from "@lexical/react/LexicalHistoryPlugin";
import { PlainTextPlugin } from "@lexical/react/LexicalPlainTextPlugin";
import {
  $createParagraphNode,
  $createTextNode,
  $getRoot,
  $getSelection,
  $isRangeSelection,
  COMMAND_PRIORITY_CRITICAL,
  KEY_ARROW_DOWN_COMMAND,
  KEY_ARROW_UP_COMMAND,
  KEY_ENTER_COMMAND,
  KEY_ESCAPE_COMMAND,
  KEY_TAB_COMMAND,
  type LexicalEditor,
} from "lexical";
import React from "react";

import { cn } from "../../lib/utils.ts";
import { readSlashQuery } from "../adapters/slashCommands.ts";

export interface ComposerPromptEditorHandle {
  focus: () => void;
  insertText: (text: string) => void;
}

/** Which key the slash menu was offered, in the menu's own vocabulary. */
export type SlashMenuKey = "up" | "down" | "enter" | "tab" | "escape";

/**
 * The composer's side of the slash menu.
 *
 * The editor reports the token and offers the keys; it decides nothing. What
 * the menu holds, which row is highlighted and whether a press means anything
 * are all the composer's, which is where the commands are.
 */
export interface ComposerSlashBridge {
  /** The `/…` token the prompt now opens with, or null when there is none. */
  readonly onQueryChange: (query: string | null) => void;
  /** Returns true when the menu consumed the press, which suppresses the editor's own. */
  readonly onKey: (key: SlashMenuKey) => boolean;
}

export interface ComposerPromptEditorProps {
  readonly editorRef?: React.Ref<ComposerPromptEditorHandle>;
  readonly value: string;
  readonly onChange: (value: string) => void;
  /** Enter with no modifier, outside an IME composition. */
  readonly onSubmit: () => void;

  readonly onHistoryStep?: (direction: "backward" | "forward") => boolean;
  /** The slash menu, when this composer offers one. */
  readonly slash?: ComposerSlashBridge;
  /** The listbox the slash menu draws, named on the editor while it is open. */
  readonly slashListId?: string | null;
  /** The row the slash menu has highlighted, named on the editor while it is open. */
  readonly slashActiveId?: string | null;
  readonly placeholder: string;
  readonly disabled?: boolean;
  readonly autoFocus?: boolean;
  readonly ariaLabel: string;
  readonly ariaDescribedBy?: string;
  readonly id?: string;
  readonly containerClassName?: string;
  readonly className?: string;
}

/**
 * Replaces the editor's whole content with `text`. Runs inside a Lexical
 * update, so it is also the initial-state initialiser.
 *
 * `select` is the whole of the caret question, and it is never true for the
 * initial state. A selection in the state is a claim on the caret: when
 * Lexical attaches the root element it reconciles that selection into the
 * document with `setBaseAndExtent`, and putting the DOM selection inside a
 * `contenteditable` *moves the browser's focus into it* — no `focus()` call
 * anywhere. A composer that mounts late therefore used to take the caret away
 * from whatever the reader was already typing into, and the keystrokes
 * dispatched before the focus came back landed in the wrong box. The issue
 * page mounts two composers — the conversation panel's Message box and the
 * comment box under the issue — and the second one attaches after the first,
 * which is exactly that.
 *
 * A composer that wants the caret asks for it through `autoFocus`, which is
 * `AutoFocusPlugin` calling `editor.focus()`; with no selection in the state
 * Lexical's own `focus()` selects the end, which is where the caret was
 * before. An external write made while the reader is *in* the editor keeps the
 * caret at the end the way it always did.
 */
function $writeText(text: string, select: boolean): void {
  const root = $getRoot();
  root.clear();
  const paragraph = $createParagraphNode();
  if (text.length > 0) paragraph.append($createTextNode(text));
  root.append(paragraph);
  if (select) paragraph.selectEnd();
}

/** True while the caret is inside this editor's own root element. */
function hasEditorFocus(editor: LexicalEditor): boolean {
  const root = editor.getRootElement();
  if (root === null) return false;
  const active = root.ownerDocument.activeElement;
  return active !== null && root.contains(active);
}

/**
 * Binds the editor to a controlled `value`.
 *
 * Reporting is a plain update listener rather than `OnChangePlugin`: that
 * plugin drops any update whose previous state was empty, and the first thing
 * typed into a freshly mounted composer can coalesce with Lexical's own
 * initialisation and be dropped with it. Deduplicating on the text itself
 * reports every real change exactly once and nothing else — including the
 * writes this plugin makes, whose text already equals `value`.
 */
function ValuePlugin({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}): null {
  const [editor] = useLexicalComposerContext();
  const last = React.useRef(value);

  // Layout effects, the way Lexical's own `OnChangePlugin` registers: a
  // passive effect leaves a window between mount and registration in which the
  // first keystroke goes unreported.
  React.useLayoutEffect(() => {
    if (last.current === value) return;
    last.current = value;
    const current = editor.getEditorState().read(() => $getRoot().getTextContent());
    if (current === value) return;
    // The caret follows the write only where the caret already is. Moving it
    // into an editor nobody is in is how a composer steals focus.
    const select = hasEditorFocus(editor);
    editor.update(() => $writeText(value, select), { tag: "detent-external-write" });
  }, [editor, value]);

  React.useLayoutEffect(
    () =>
      editor.registerUpdateListener(({ editorState }) => {
        const text = editorState.read(() => $getRoot().getTextContent());
        if (text === last.current) return;
        last.current = text;
        onChange(text);
      }),
    [editor, onChange],
  );

  return null;
}

function EditablePlugin({ editable }: { editable: boolean }): null {
  const [editor] = useLexicalComposerContext();
  React.useEffect(() => {
    editor.setEditable(editable);
  }, [editable, editor]);
  return null;
}

function AutoFocusPlugin({ autoFocus }: { autoFocus: boolean }): null {
  const [editor] = useLexicalComposerContext();
  React.useEffect(() => {
    if (autoFocus) editor.focus();
  }, [autoFocus, editor]);
  return null;
}

function HandlePlugin({ handle }: { handle: React.Ref<ComposerPromptEditorHandle> }): null {
  const [editor] = useLexicalComposerContext();
  React.useImperativeHandle(
    handle,
    () => ({
      focus: () => editor.focus(),
      insertText: (text: string) =>
        editor.update(() => {
          const selection = $getSelection();
          const range = $isRangeSelection(selection) ? selection : $getRoot().selectEnd();
          range.insertText(text);
        }),
    }),
    [editor],
  );
  return null;
}

/**
 * Enter sends; every modified Enter inserts a line break instead. Registered at
 * critical priority so it runs before Lexical's own newline handling, and
 * returning `true` is what stops that handling from also firing.
 */
function SubmitPlugin({
  onSubmit,
  enabled,
}: {
  onSubmit: () => void;
  enabled: boolean;
}): null {
  const [editor] = useLexicalComposerContext();
  React.useEffect(
    () =>
      editor.registerCommand(
        KEY_ENTER_COMMAND,
        (event) => {
          if (event === null) return false;
          // An IME candidate window commits with Enter. `isComposing` is the
          // only reliable signal for it, and it must suppress the send and the
          // newline alike.
          if (event.isComposing) return true;
          if (event.shiftKey || event.altKey || event.ctrlKey || event.metaKey) return false;
          event.preventDefault();
          if (enabled) onSubmit();
          return true;
        },
        COMMAND_PRIORITY_CRITICAL,
      ),
    [editor, enabled, onSubmit],
  );
  return null;
}

/**
 * Reports the `/…` token and hands the slash menu its four keys.
 *
 * Registered ahead of `SubmitPlugin` and `PromptHistoryPlugin` — both at the
 * same critical priority, and Lexical runs same-priority listeners in the order
 * they were registered, which is the order these plugins are mounted in. That
 * ordering is the whole keyboard contract: with the menu open Enter runs the
 * highlighted command and Up and Down walk the list, and with it closed every
 * one of those presses falls through untouched to the handlers that own them,
 * so Enter still sends and Up still recalls the previous prompt.
 *
 * The bridge is read through a ref so the registration happens once: a bridge
 * rebuilt on every keystroke would re-register these listeners behind the two
 * plugins below and hand Enter back to the send.
 */
function SlashPlugin({ bridge }: { bridge: ComposerSlashBridge }): null {
  const [editor] = useLexicalComposerContext();
  const current = React.useRef(bridge);
  current.current = bridge;

  React.useLayoutEffect(
    () =>
      editor.registerUpdateListener(({ editorState }) => {
        const text = editorState.read(() => $getRoot().getTextContent());
        current.current.onQueryChange(readSlashQuery(text));
      }),
    [editor],
  );

  React.useEffect(() => {
    const handle = (key: SlashMenuKey) => (event: KeyboardEvent | null) => {
      // A modified press is never a menu press: Shift+Enter is a line break,
      // Shift+Tab is the previous tab stop, and an IME candidate window owns
      // Enter outright.
      if (event !== null && (event.shiftKey || event.altKey || event.ctrlKey || event.metaKey)) {
        return false;
      }
      if (event?.isComposing === true) return false;
      if (!current.current.onKey(key)) return false;
      event?.preventDefault();
      return true;
    };
    const offs = [
      editor.registerCommand(KEY_ARROW_UP_COMMAND, handle("up"), COMMAND_PRIORITY_CRITICAL),
      editor.registerCommand(KEY_ARROW_DOWN_COMMAND, handle("down"), COMMAND_PRIORITY_CRITICAL),
      editor.registerCommand(KEY_ENTER_COMMAND, handle("enter"), COMMAND_PRIORITY_CRITICAL),
      editor.registerCommand(KEY_TAB_COMMAND, handle("tab"), COMMAND_PRIORITY_CRITICAL),
      editor.registerCommand(KEY_ESCAPE_COMMAND, handle("escape"), COMMAND_PRIORITY_CRITICAL),
    ];
    return () => {
      for (const off of offs) off();
    };
  }, [editor]);

  return null;
}

function PromptHistoryPlugin({
  onHistoryStep,
}: {
  onHistoryStep: (direction: "backward" | "forward") => boolean;
}): null {
  const [editor] = useLexicalComposerContext();
  React.useEffect(() => {
    const handle = (direction: "backward" | "forward") => (event: KeyboardEvent | null) => {
      if (event === null || event.shiftKey || event.altKey || event.ctrlKey || event.metaKey) {
        return false;
      }
      const collapsed = editor.getEditorState().read(() => {
        const selection = $getSelection();
        return $isRangeSelection(selection) && selection.isCollapsed();
      });
      if (!collapsed) return false;
      if (!onHistoryStep(direction)) return false;
      event.preventDefault();
      return true;
    };
    const offUp = editor.registerCommand(
      KEY_ARROW_UP_COMMAND,
      handle("backward"),
      COMMAND_PRIORITY_CRITICAL,
    );
    const offDown = editor.registerCommand(
      KEY_ARROW_DOWN_COMMAND,
      handle("forward"),
      COMMAND_PRIORITY_CRITICAL,
    );
    return () => {
      offUp();
      offDown();
    };
  }, [editor, onHistoryStep]);
  return null;
}

export function ComposerPromptEditor(props: ComposerPromptEditorProps): React.ReactElement {
  const disabled = props.disabled === true;
  const initialConfig = React.useMemo(
    () => ({
      namespace: "detent-composer",
      editable: !disabled,
      onError: (error: Error) => {
        throw error;
      },
      editorState: () => {
        // No selection: see `$writeText`. A composer takes the caret only when
        // it is asked to, through `autoFocus`.
        $writeText(props.value, false);
      },
      theme: {},
    }),
    // The initial state is exactly that: later changes travel through
    // `ValuePlugin`, not through a re-created editor.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [],
  );

  return (
    <LexicalComposer initialConfig={initialConfig}>
      <div
        className={cn(
          "relative [font-family:var(--font-composer,var(--font-sans))] [font-size:var(--font-size-prompt,0.875rem)] [@media(max-width:39.999rem)_and_(pointer:coarse)]:[font-size:max(var(--font-size-prompt,1rem),16px)]",
          props.containerClassName,
        )}
      >
        <PlainTextPlugin
          contentEditable={
            <ContentEditable
              className={cn(
                // The wrapper owns the appearance preference; keep everything else here.
                "block max-h-50 min-h-17.5 w-full overflow-y-auto whitespace-pre-wrap wrap-break-word bg-transparent leading-relaxed text-foreground focus:outline-none",
                props.className,
              )}
              data-testid="composer-editor"
              id={props.id}
              role="textbox"
              aria-multiline="true"
              aria-label={props.ariaLabel}
              aria-placeholder={props.placeholder}
              aria-describedby={props.ariaDescribedBy}
              aria-disabled={disabled || undefined}
              // While the slash menu is open the prompt is the thing being
              // typed into and the list is the thing being chosen from, so the
              // editor is what points at it: the menu itself never takes focus.
              // `aria-expanded` is deliberately not here: it is not an
              // attribute `role="textbox"` supports, and a scan is right to
              // say so.
              aria-controls={props.slashListId ?? undefined}
              aria-activedescendant={props.slashActiveId ?? undefined}
              placeholder={<span />}
            />
          }
          placeholder={
            <div className="pointer-events-none absolute inset-0 leading-relaxed text-placeholder/75">
              {props.placeholder}
            </div>
          }
          ErrorBoundary={LexicalErrorBoundary}
        />
        <ValuePlugin value={props.value} onChange={props.onChange} />
        <EditablePlugin editable={!disabled} />
        {/* Ahead of the two below: same priority, first registered wins, and
            mount order is registration order. */}
        {props.slash === undefined ? null : <SlashPlugin bridge={props.slash} />}
        <SubmitPlugin onSubmit={props.onSubmit} enabled={!disabled} />
        {props.onHistoryStep === undefined ? null : (
          <PromptHistoryPlugin onHistoryStep={props.onHistoryStep} />
        )}
        <AutoFocusPlugin autoFocus={props.autoFocus === true && !disabled} />
        {props.editorRef === undefined ? null : <HandlePlugin handle={props.editorRef} />}
        <HistoryPlugin />
      </div>
    </LexicalComposer>
  );
}
