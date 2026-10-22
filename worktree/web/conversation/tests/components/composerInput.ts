import { act } from "@testing-library/react";
import {
  $createParagraphNode,
  $createTextNode,
  $getRoot,
  $getSelection,
  $isRangeSelection,
  type LexicalEditor,
} from "lexical";

/**
 * jsdom implements `Range` but not its geometry, and Lexical measures the
 * caret to decide whether to scroll it into view. A browser has both; this is
 * a test-environment gap, so it is filled here rather than guarded in the
 * component.
 */
const EMPTY_RECT = {
  bottom: 0,
  height: 0,
  left: 0,
  right: 0,
  toJSON: () => ({}),
  top: 0,
  width: 0,
  x: 0,
  y: 0,
} as DOMRect;

if (typeof Range !== "undefined" && typeof Range.prototype.getBoundingClientRect !== "function") {
  Range.prototype.getBoundingClientRect = () => EMPTY_RECT;
  Range.prototype.getClientRects = () => ({
    item: () => null,
    length: 0,
    [Symbol.iterator]: function* () {},
  }) as unknown as DOMRectList;
}

type LexicalRoot = HTMLElement & { __lexicalEditor?: LexicalEditor | null };

function editorOf(node: HTMLElement): LexicalEditor {
  const editor = (node as LexicalRoot).__lexicalEditor;
  if (editor == null) throw new Error("The element is not a mounted Lexical editor.");
  return editor;
}

/** The text the composer currently holds. */
export function composerText(node: HTMLElement): string {
  return editorOf(node)
    .getEditorState()
    .read(() => $getRoot().getTextContent());
}

/**
 * Where the composer's own caret is, or null when it has not claimed one.
 *
 * This is the thing a mounting composer must not have: Lexical reconciles the
 * selection in its editor state into the document as soon as the root element
 * attaches, and a DOM selection inside a `contenteditable` moves the browser's
 * focus into it.
 */
export function composerCaret(
  node: HTMLElement,
): { readonly offset: number; readonly collapsed: boolean } | null {
  return editorOf(node)
    .getEditorState()
    .read(() => {
      const selection = $getSelection();
      if (!$isRangeSelection(selection)) return null;
      return { offset: selection.anchor.offset, collapsed: selection.isCollapsed() };
    });
}

/**
 * Puts the caret in the composer, the way a click or a Tab does.
 *
 * A freshly mounted composer holds no selection, and must not: a selection in
 * the initial editor state is reconciled into the document the moment Lexical
 * attaches the root element, which moves the browser's focus into it and takes
 * the caret away from whatever the reader is already typing into
 * (`ComposerPromptEditor.tsx`, `$writeText`). Lexical's own key handling needs
 * a selection, so a test that presses a key has to focus first — which is what
 * a reader does too. `editor.focus()` selects the end, where a click at the
 * end of the prompt would land.
 */
export async function focusComposer(node: HTMLElement): Promise<void> {
  const editor = editorOf(node);
  await act(async () => {
    editor.focus();
  });
}

/** Inserts `text` at the caret, the way typing or a paste does. */
export async function typeInComposer(node: HTMLElement, text: string): Promise<void> {
  const editor = editorOf(node);
  await act(async () => {
    editor.update(
      () => {
        const selection = $getSelection();
        const range = $isRangeSelection(selection) ? selection : $getRoot().selectEnd();
        range.insertText(text);
      },
      // Discrete: a freshly mounted editor still has its own initialisation
      // queued, and an ordinary update would be batched behind it and land
      // after the assertion that follows.
      { discrete: true },
    );
  });
}

/** Replaces the whole prompt, the way select-all-and-type does. */
export async function setComposerText(node: HTMLElement, text: string): Promise<void> {
  const editor = editorOf(node);
  await act(async () => {
    editor.update(
      () => {
        const root = $getRoot();
        root.clear();
        const paragraph = $createParagraphNode();
        if (text.length > 0) paragraph.append($createTextNode(text));
        root.append(paragraph);
        paragraph.selectEnd();
      },
      { discrete: true },
    );
  });
}
