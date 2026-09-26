// The shell's keyboard shortcuts (design inventory B.13 rule 4 and B.14).
//
// Two, and only two: `/` focuses the sidebar search, and `Mod+Shift+N` starts
// a new chat in the current project context. The second one deliberately has
// no button of its own — B.13 allows a shortcut to duplicate the one create
// action, and this one fires exactly the same handler the compose pencil does.
//
// Both are decided by a pure function so the rules can be tested without a
// browser: the single case that matters is that a single-key shortcut must
// never fire while the reader is typing.

/** The part of a keyboard event these rules read. */
export interface ShortcutEvent {
  readonly key: string;
  readonly metaKey: boolean;
  readonly ctrlKey: boolean;
  readonly shiftKey: boolean;
  readonly altKey: boolean;
  readonly defaultPrevented?: boolean;
  /** Read by shape, not by class: a jsdom node and a browser node differ. */
  readonly target?: unknown;
}

export type Shortcut = "focus-search" | "new-chat";

/** The `aria-keyshortcuts` value advertising the new-chat shortcut. */
export const NEW_CHAT_KEYSHORTCUTS = "Meta+Shift+N Control+Shift+N";

/** The `aria-keyshortcuts` value advertising the search shortcut. */
export const SEARCH_KEYSHORTCUTS = "/";

/**
 * True when the event came from somewhere the reader is typing: a text field,
 * a `contenteditable` region, or a control that owns its own key handling. A
 * single-key shortcut must not steal a keystroke from any of them (B.14).
 */
export function isEditableTarget(target: unknown): boolean {
  if (target === null || target === undefined) return false;
  const element = target as {
    tagName?: unknown;
    isContentEditable?: unknown;
    getAttribute?: (name: string) => string | null;
  };
  if (element.isContentEditable === true) return true;
  const tag = typeof element.tagName === "string" ? element.tagName.toUpperCase() : "";
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
  const role = element.getAttribute?.("role") ?? null;
  return role === "textbox" || role === "searchbox" || role === "combobox";
}

/** Which shortcut this event is, if any. */
export function shortcutFor(event: ShortcutEvent): Shortcut | null {
  if (event.defaultPrevented === true) return null;
  const mod = event.metaKey || event.ctrlKey;

  // `Mod+Shift+N` fires wherever focus is: it carries a modifier, so it cannot
  // be mistaken for typing. Shift changes the reported key on most layouts,
  // so both cases are accepted.
  if (mod && event.shiftKey && !event.altKey && (event.key === "N" || event.key === "n")) {
    return "new-chat";
  }

  if (event.key === "/" && !mod && !event.altKey && !event.shiftKey) {
    return isEditableTarget(event.target) ? null : "focus-search";
  }

  return null;
}
