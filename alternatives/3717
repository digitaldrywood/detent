import type { ResolvedKeybindingsConfig, ThreadJumpKeybindingCommand } from "../../contracts/ui.ts";
import { THREAD_JUMP_KEYBINDING_COMMANDS } from "../../contracts/ui.ts";
import { isMacPlatform } from "../../lib/utils.ts";

export type KeybindingCommand =
  | "commandPalette.toggle"
  | "sidebar.toggle"
  | "rightPanel.toggle"
  | "rightPanel.close"
  | "rightPanel.toggleMaximized"
  | "diff.toggle"
  | "terminal.toggle"
  | "terminal.new"
  | "terminal.close"
  | "chat.new"
  | "chat.newLocal"
  | "thread.previous"
  | "thread.next"
  | "thread.stop"
  | "thread.copyReference"
  | "issue.status"
  | "issue.priority"
  | "issue.assignee"
  | "issue.labels"
  | "issue.related"
  | "editor.openFavorite"
  | ThreadJumpKeybindingCommand;

export interface KeybindingShortcut {
  readonly key: string;
  readonly modKey: boolean;
  readonly metaKey: boolean;
  readonly ctrlKey: boolean;
  readonly shiftKey: boolean;
  readonly altKey: boolean;
}

function chord(key: string, options: Partial<KeybindingShortcut> = {}): KeybindingShortcut {
  return {
    key,
    modKey: options.modKey ?? true,
    metaKey: options.metaKey ?? false,
    ctrlKey: options.ctrlKey ?? false,
    shiftKey: options.shiftKey ?? false,
    altKey: options.altKey ?? false,
  };
}

export const DEFAULT_BINDINGS: Readonly<Record<KeybindingCommand, KeybindingShortcut>> = {

  "commandPalette.toggle": chord("k"),
  "sidebar.toggle": chord("b"),
  "rightPanel.toggle": chord("b", { altKey: true }),
  "diff.toggle": chord("d"),
  "thread.copyReference": chord("c", { shiftKey: true }),
  "chat.newLocal": chord("n", { shiftKey: true }),

  "editor.openFavorite": chord("o"),

  // Deviations, with the browser reason for each.
  //
  // `rightPanel.close` is `mod+w` upstream, which closes the tab. `mod+alt+w`
  // is the nearest free chord and keeps the panel family on the same modifier
  // as `rightPanel.toggle`.
  "rightPanel.close": chord("w", { altKey: true }),

  "chat.new": chord("o", { shiftKey: true }),
  // Upstream has no default for either of these two; both are Detent's, chosen
  // to sit beside the command they extend.
  "rightPanel.toggleMaximized": chord("b", { altKey: true, shiftKey: true }),

  "terminal.toggle": chord("j"),

  "terminal.new": chord("t", { altKey: true }),
  "terminal.close": chord("w", { shiftKey: true, altKey: true }),
  // Upstream lists `thread.stop` as a command but ships no default chord for
  // it. Stopping a turn is the one destructive thing a reader does from the
  // keyboard, so it takes a deliberate chord rather than a bare letter.
  "thread.stop": chord(".", { shiftKey: true }),

  "thread.previous": chord("[", { shiftKey: true }),
  "thread.next": chord("]", { shiftKey: true }),

  "issue.status": chord("s", { modKey: false }),
  "issue.priority": chord("p", { modKey: false }),
  "issue.assignee": chord("a", { modKey: false }),
  "issue.labels": chord("l", { modKey: false }),
  "issue.related": chord("r", { modKey: false }),
  ...(Object.fromEntries(
    THREAD_JUMP_KEYBINDING_COMMANDS.map((command, index) => [command, chord(`${index + 1}`)]),
  ) as Record<ThreadJumpKeybindingCommand, KeybindingShortcut>),
};

export const UNBOUND_COMMANDS: Readonly<Record<string, string>> = {

  "terminal.split": "Detent's Terminal surface shows one shell at a time and has no split.",
  "terminal.splitVertical": "Detent's Terminal surface shows one shell at a time and has no split.",
  "preview.toggle": "The Browser surface is snapshot-first and not live yet.",
  "preview.refresh": "The Browser surface is snapshot-first and not live yet.",
  "preview.focusUrl": "The Browser surface is snapshot-first and not live yet.",
  "preview.zoomIn": "The Browser surface is snapshot-first and not live yet.",
  "preview.zoomOut": "The Browser surface is snapshot-first and not live yet.",
  "preview.resetZoom": "The Browser surface is snapshot-first and not live yet.",
  "filePicker.toggle": "Browsing the checkout needs a runner that reports the files capability.",
  "projectSearch.toggle":
    "Searching a project's contents needs a runner that reports the files capability.",
  "modelPicker.toggle":
    "The model, effort and access pickers are on the composer and open by pointer only for now.",
  "thread.settle":
    "A conversation settles on its own after the project's settle window, and unsettles on new activity.",
  "themeEditor.toggle": "Detent follows the operating system's appearance and has no theme editor.",
  "composer.stash": "The composer keeps one draft per conversation and has no stash.",
  "thread.pin": "The hub stores no pinned state for a conversation.",
};

/**
 * Upstream's whole command list, in upstream's order, so the settings page can
 * show every one of them. Transcribed from
 * `packages/contracts/src/keybindings.ts` (`STATIC_KEYBINDING_COMMANDS`).
 */
export const ALL_KEYBINDING_COMMANDS: readonly string[] = [
  "sidebar.toggle",
  "terminal.toggle",
  "terminal.split",
  "terminal.splitVertical",
  "terminal.new",
  "terminal.close",
  "rightPanel.toggle",
  "rightPanel.toggleMaximized",
  "rightPanel.close",
  "diff.toggle",
  "preview.toggle",
  "preview.refresh",
  "preview.focusUrl",
  "preview.zoomIn",
  "preview.zoomOut",
  "preview.resetZoom",
  "commandPalette.toggle",
  "filePicker.toggle",
  "projectSearch.toggle",
  "themeEditor.toggle",
  "composer.stash",
  "chat.new",
  "chat.newLocal",
  "editor.openFavorite",
  "modelPicker.toggle",
  "thread.stop",
  "thread.previous",
  "thread.next",
  "thread.copyReference",
  "thread.settle",
  "thread.pin",
  ...THREAD_JUMP_KEYBINDING_COMMANDS,
  // Detent's own, after upstream's list so the transcription above stays a
  // transcription: the issue page's property pickers.
  "issue.status",
  "issue.priority",
  "issue.assignee",
  "issue.labels",
  "issue.related",
];

/**
 * Whether a command's chord is a bare key with no modifier.
 *
 * Only the issue page's pickers are, and the dispatcher uses this to decide
 * whether a keystroke belongs to the shortcut or to whatever has focus.
 */
export function isUnmodifiedChord(command: string): boolean {
  const shortcut = DEFAULT_BINDINGS[command as KeybindingCommand] ?? projectActionBindings.get(command);
  if (shortcut === undefined) return false;

  return !shortcut.modKey && !shortcut.metaKey && !shortcut.ctrlKey && !shortcut.altKey;
}

/** One row of the Keybindings settings section. */
export interface KeybindingCatalogueRow {
  readonly command: string;
  readonly label: string;
  /** The chord, or null when the command is listed but not bound. */
  readonly chord: string | null;
  /** Why it is not bound. Null when it is. */
  readonly reason: string | null;
}

/** Every command, labelled, with its chord or its reason. */
export function keybindingCatalogue(platform?: string): readonly KeybindingCatalogueRow[] {
  const staticRows = ALL_KEYBINDING_COMMANDS.map((command) => {
    const shortcut = DEFAULT_BINDINGS[command as KeybindingCommand];
    return {
      command,
      label: commandLabel(command),
      chord: shortcut === undefined ? null : formatShortcutLabel(shortcut, platform),
      reason:
        shortcut === undefined
          ? (UNBOUND_COMMANDS[command] ?? "This command has no target in Detent Cloud yet.")
          : null,
    };
  });

  const actionRows = [...projectActionBindings.entries()].map(([command, shortcut]) => ({
    command,
    label: commandLabel(command),
    chord: formatShortcutLabel(shortcut, platform),
    reason: null,
  }));
  return [...staticRows, ...actionRows];
}

// --- The open project's actions ---------------------------------------------
//
// `DEFAULT_BINDINGS` is a frozen const, and the chords a project's actions
// carry are not in it: they arrive per project, from the hub, and change while
// the tab is open (decisions.md §18.12). So they live in a registry beside it,
// and the three functions the copied components call consult that registry
// after the static table.
//
// **The collision rule is that the static table wins.** A reader who binds an
// action to `mod+k` keeps the command palette, and the action simply does not
// fire from the keyboard; the shortcut still shows on its menu row, because
// the row reports what the action *is bound to*, and the Keybindings settings
// page lists both so the clash is visible rather than mysterious. The
// alternative — letting a project's action shadow the palette, the sidebar or
// the panel for everyone in the project — would let one author take a chord
// away from every colleague, and there is no way for them to get it back.
//
// A module-level registry rather than React state, for the same reason
// `rightPanelStore.ts` has one: exactly one project is open at a time, the
// keystroke listener reads it from outside React (capture-phase `keydown`),
// and the copied `ProjectScriptsControl` asks for a label during render.
const projectActionBindings = new Map<string, KeybindingShortcut>();

/** One registered action: the command it answers to and the chord it carries. */
export interface ProjectActionBinding {
  /** `script.<id>.run`, from `commandForProjectScript`. */
  readonly command: string;
  /** The stored chord string, e.g. `mod+shift+t`. */
  readonly chord: string;
}

/**
 * Replaces the registry with this project's actions.
 *
 * A replace rather than an add: a deleted action must stop resolving, and a
 * renamed one changes command, so reconciling entry by entry would leave the
 * old command behind. Returns the commands that were actually registered,
 * which is what a caller needs to know — a chord the hub stored that this
 * build cannot parse registers nothing rather than binding something else.
 */
export function registerProjectActionKeybindings(
  bindings: readonly ProjectActionBinding[],
): readonly string[] {
  projectActionBindings.clear();
  const registered: string[] = [];
  for (const binding of bindings) {
    const shortcut = parseChord(binding.chord);
    if (shortcut === null) continue;
    projectActionBindings.set(binding.command, shortcut);
    registered.push(binding.command);
  }
  return registered;
}

/** The chord a registered action carries, as the dialog prefills its field. */
export function registeredProjectActionChord(command: string): string | null {
  const shortcut = projectActionBindings.get(command);
  return shortcut === undefined ? null : shortcutKey(shortcut);
}

export function parseChord(chord: string): KeybindingShortcut | null {
  const trimmed = chord.trim().toLowerCase();
  if (trimmed.length === 0) return null;
  // The split is on the *last* separator rather than on every one, because the
  // key may itself be `+` (`mod++`) and a plain split would then leave an
  // empty token that reads as a modifier nobody named.
  const separator = trimmed.lastIndexOf("+");
  const key =
    separator === -1
      ? trimmed
      : separator === trimmed.length - 1
        ? "+"
        : trimmed.slice(separator + 1);
  const modifierPart =
    separator === -1
      ? ""
      : separator === trimmed.length - 1
        ? trimmed.slice(0, Math.max(separator - 1, 0))
        : trimmed.slice(0, separator);
  const tokens = modifierPart.length === 0 ? [] : modifierPart.split("+");
  const shortcut = {
    key: key === "space" ? " " : key === "esc" ? "escape" : key,
    modKey: false as boolean,
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
  };
  for (const token of tokens) {
    if (token === "mod") shortcut.modKey = true;
    else if (token === "meta" || token === "cmd") shortcut.metaKey = true;
    else if (token === "ctrl" || token === "control") shortcut.ctrlKey = true;
    else if (token === "shift") shortcut.shiftKey = true;
    else if (token === "alt" || token === "option") shortcut.altKey = true;
    // Anything else means the chord names two keys, which no chord does.
    else return null;
  }

  if (
    !shortcut.modKey &&
    !shortcut.metaKey &&
    !shortcut.ctrlKey &&
    !shortcut.altKey &&
    !shortcut.shiftKey
  ) {
    return null;
  }
  return shortcut;
}

export function commandLabel(command: string): string {
  const raw = String(command);
  if (raw.startsWith("script.") && raw.endsWith(".run")) {
    return `Run Script: ${titleCaseCommandSegment(raw.slice("script.".length, -".run".length))}`;
  }
  return raw.split(".").map(titleCaseCommandSegment).join(": ");
}

function titleCaseCommandSegment(segment: string): string {
  const words: Array<string> = [];
  for (const part of segment.replace(/([a-z0-9])([A-Z])/g, "$1 $2").split(/[-_\s]+/)) {
    if (part.length > 0) {
      words.push(part.slice(0, 1).toUpperCase() + part.slice(1));
    }
  }
  return words.join(" ");
}

export const detentKeybindings: ResolvedKeybindingsConfig = {
  bindings: Object.fromEntries(
    Object.entries(DEFAULT_BINDINGS).map(([command, shortcut]) => [command, [shortcutKey(shortcut)]]),
  ),
};

function shortcutKey(shortcut: KeybindingShortcut): string {
  const parts: string[] = [];
  if (shortcut.modKey) parts.push("mod");
  if (shortcut.ctrlKey) parts.push("ctrl");
  if (shortcut.altKey) parts.push("alt");
  if (shortcut.shiftKey) parts.push("shift");
  if (shortcut.metaKey) parts.push("meta");
  parts.push(shortcut.key);
  return parts.join("+");
}

export function formatShortcutKeyLabel(key: string): string {
  if (key === " ") return "Space";
  if (key.length === 1) return key.toUpperCase();
  if (key === "escape") return "Esc";
  if (key === "arrowup") return "Up";
  if (key === "arrowdown") return "Down";
  if (key === "arrowleft") return "Left";
  if (key === "arrowright") return "Right";
  return key.slice(0, 1).toUpperCase() + key.slice(1);
}

export function formatShortcutLabel(
  shortcut: KeybindingShortcut,
  platform = globalThis.navigator?.platform ?? "",
): string {
  const keyLabel = formatShortcutKeyLabel(shortcut.key);
  const useMetaForMod = isMacPlatform(platform);
  const showMeta = shortcut.metaKey || (shortcut.modKey && useMetaForMod);
  const showCtrl = shortcut.ctrlKey || (shortcut.modKey && !useMetaForMod);
  const showAlt = shortcut.altKey;
  const showShift = shortcut.shiftKey;

  if (useMetaForMod) {
    return `${showCtrl ? "⌃" : ""}${showAlt ? "⌥" : ""}${showShift ? "⇧" : ""}${showMeta ? "⌘" : ""}${keyLabel}`;
  }

  const parts: string[] = [];
  if (showCtrl) parts.push("Ctrl");
  if (showAlt) parts.push("Alt");
  if (showShift) parts.push("Shift");
  if (showMeta) parts.push("Meta");
  parts.push(keyLabel);
  return parts.join("+");
}

export function shortcutLabelForCommand(
  _keybindings: ResolvedKeybindingsConfig,
  command: KeybindingCommand | string | null,
  platform?: string,
): string | null {
  if (command === null) return null;
  // The static table first, then the open project's actions — the same order
  // `resolveShortcutCommand` matches in, so a label cannot advertise a chord
  // the dispatcher would send somewhere else.
  const shortcut =
    DEFAULT_BINDINGS[command as KeybindingCommand] ?? projectActionBindings.get(command);
  if (shortcut === undefined) return null;
  return formatShortcutLabel(shortcut, platform ?? globalThis.navigator?.platform ?? "");
}

const EVENT_CODE_SHORTCUT_KEYS: Readonly<Record<string, string>> = {
  Backquote: "`",
  Backslash: "\\",
  BracketLeft: "[",
  BracketRight: "]",
  Comma: ",",
  Digit0: "0",
  Digit1: "1",
  Digit2: "2",
  Digit3: "3",
  Digit4: "4",
  Digit5: "5",
  Digit6: "6",
  Digit7: "7",
  Digit8: "8",
  Digit9: "9",
  Equal: "=",
  Minus: "-",
  Period: ".",
  Quote: "'",
  Semicolon: ";",
  Slash: "/",
};

function normalizeEventKey(key: string): string {
  const normalized = key.toLowerCase();
  if (normalized === "esc") return "escape";
  return normalized;
}

export function shortcutKeyFromEvent(event: Pick<ShortcutEventLike, "key" | "code">): string {
  const layoutKey = normalizeEventKey(event.key);
  if (/^[a-z]$/.test(layoutKey)) return layoutKey;
  const physicalKey = event.code ? EVENT_CODE_SHORTCUT_KEYS[event.code] : undefined;
  return physicalKey ?? layoutKey;
}

interface ShortcutEventLike {
  readonly key: string;
  readonly code?: string;
  readonly metaKey: boolean;
  readonly ctrlKey: boolean;
  readonly shiftKey: boolean;
  readonly altKey: boolean;
}

function matches(event: ShortcutEventLike, shortcut: KeybindingShortcut, platform: string): boolean {
  const useMetaForMod = isMacPlatform(platform);
  const expectedMeta = shortcut.metaKey || (shortcut.modKey && useMetaForMod);
  const expectedCtrl = shortcut.ctrlKey || (shortcut.modKey && !useMetaForMod);
  if (event.metaKey !== expectedMeta) return false;
  if (event.ctrlKey !== expectedCtrl) return false;
  if (event.shiftKey !== shortcut.shiftKey) return false;
  if (event.altKey !== shortcut.altKey) return false;

  const layoutKey = event.key.toLowerCase();
  if (layoutKey === shortcut.key) return true;
  const letter = event.code?.match(/^Key([A-Z])$/)?.[1]?.toLowerCase();
  return letter !== undefined && letter === shortcut.key;
}

export interface ShortcutMatchOptions {
  readonly platform?: string;
  readonly context?: {
    readonly terminalFocus?: boolean;
    readonly terminalOpen?: boolean;
    readonly modelPickerOpen?: boolean;
  };
}

function resolvePlatform(options?: ShortcutMatchOptions | string): string {
  if (typeof options === "string") return options;
  return options?.platform ?? globalThis.navigator?.platform ?? "";
}

export function resolveShortcutCommand(
  event: ShortcutEventLike,
  _keybindings: ResolvedKeybindingsConfig,
  options?: ShortcutMatchOptions | string,
): KeybindingCommand | null {
  const platform = resolvePlatform(options);
  for (const [command, shortcut] of Object.entries(DEFAULT_BINDINGS)) {
    if (matches(event, shortcut, platform)) return command as KeybindingCommand;
  }
  // The open project's actions, after the static table: see the collision rule
  // at `projectActionBindings`. The static binding wins, so a project cannot
  // take the command palette away from a colleague.
  for (const [command, shortcut] of projectActionBindings) {
    if (matches(event, shortcut, platform)) return command as KeybindingCommand;
  }
  return null;
}

export function isOpenFavoriteEditorShortcut(
  event: ShortcutEventLike,
  keybindings: ResolvedKeybindingsConfig,
  options?: ShortcutMatchOptions | string,
): boolean {
  return resolveShortcutCommand(event, keybindings, options) === "editor.openFavorite";
}

export function threadJumpCommandForIndex(index: number): ThreadJumpKeybindingCommand | null {
  return THREAD_JUMP_KEYBINDING_COMMANDS[index] ?? null;
}

export function threadJumpIndexFromCommand(command: string): number | null {
  const index = THREAD_JUMP_KEYBINDING_COMMANDS.indexOf(command as ThreadJumpKeybindingCommand);
  return index === -1 ? null : index;
}

export function threadTraversalDirectionFromCommand(
  command: string | null,
): "previous" | "next" | null {
  if (command === "thread.previous") return "previous";
  if (command === "thread.next") return "next";
  return null;
}

export function shouldShowThreadJumpHintsForModifiers(
  modifiers: {
    readonly metaKey: boolean;
    readonly ctrlKey: boolean;
    readonly shiftKey: boolean;
    readonly altKey: boolean;
  },
  _keybindings: ResolvedKeybindingsConfig,
  options?: ShortcutMatchOptions,
): boolean {
  if (options?.context?.terminalFocus === true) return false;
  const platform = resolvePlatform(options);
  const useMetaForMod = isMacPlatform(platform);
  const jump = DEFAULT_BINDINGS["thread.jump.1"];
  const expectedMeta = jump.metaKey || (jump.modKey && useMetaForMod);
  const expectedCtrl = jump.ctrlKey || (jump.modKey && !useMetaForMod);
  return (
    modifiers.metaKey === expectedMeta &&
    modifiers.ctrlKey === expectedCtrl &&
    modifiers.shiftKey === jump.shiftKey &&
    modifiers.altKey === jump.altKey
  );
}
