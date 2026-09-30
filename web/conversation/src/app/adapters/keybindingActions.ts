import React from "react";

import {
  detentKeybindings,
  isUnmodifiedChord,
  resolveShortcutCommand,
  type KeybindingCommand,
} from "./keybindings.ts";
import { projectScriptIdFromCommand } from "../../projectScripts.ts";

/**
 * One bound command.
 *
 * Returning `false` declines the keystroke: the handler is present but cannot
 * act right now, so the event is left alone and the browser keeps it. That is
 * the difference between a command that is disabled at this moment and one
 * that is bound to nothing — a caller whose availability changes between
 * renders says so here rather than adding and removing the binding, which
 * would mean a hook whose shape depends on state.
 */
export type KeybindingAction = () => void | boolean;

/**
 * The actions the shell can offer. Every one is optional: a surface that is
 * not on screen, or a capability a runner has not reported, simply passes
 * nothing and its chord stays inert.
 */
export interface KeybindingActions {
  readonly "chat.new"?: KeybindingAction;
  readonly "chat.newLocal"?: KeybindingAction;
  readonly "rightPanel.toggle"?: KeybindingAction;
  readonly "rightPanel.close"?: KeybindingAction;
  readonly "rightPanel.toggleMaximized"?: KeybindingAction;
  readonly "diff.toggle"?: KeybindingAction;
  /** The Terminal surface (decisions.md §18.3). */
  readonly "terminal.toggle"?: KeybindingAction;
  readonly "terminal.new"?: KeybindingAction;
  readonly "terminal.close"?: KeybindingAction;
  readonly "thread.next"?: KeybindingAction;
  readonly "thread.previous"?: KeybindingAction;
  readonly "thread.stop"?: KeybindingAction;
  readonly "thread.copyReference"?: KeybindingAction;
  readonly "commandPalette.toggle"?: KeybindingAction;
  readonly "issue.status"?: KeybindingAction;
  readonly "issue.priority"?: KeybindingAction;
  readonly "issue.assignee"?: KeybindingAction;
  readonly "issue.labels"?: KeybindingAction;
  readonly "issue.related"?: KeybindingAction;
  /**
   * One project action's chord (decisions.md §18.12).
   *
   * Not a member of the record above, and deliberately not an index signature
   * on it either. The commands are `script.<id>.run`, one per action the
   * project has authored, and the set is neither closed nor known at build
   * time — so there is no key to write. The two ways to admit them are this
   * member, which takes the id the listener parsed out of the command, or an
   * index signature on `KeybindingActions` itself.
   *
   * This is cleaner than widening the record, for three reasons. The record's
   * exhaustiveness is the point of it: `isBound` and every call site are
   * typed against a closed set, and an index signature would make a typo in a
   * static command name — `"diff.tooggle"` — a legal key that silently binds
   * nothing. The dispatcher would also have to keep the parse anyway, because
   * a caller cannot enumerate handlers for actions it has not loaded yet.
   * And the shape is honest about what it is: one handler that takes an
   * argument, rather than an unbounded family of nullary ones.
   *
   * `void | boolean` for the same reason the rest do: `false` declines the
   * keystroke, which is what a panel with no workspace to run in owes the
   * browser.
   */
  readonly runProjectAction?: (actionId: string) => void | boolean;
}

function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  // The attribute as well as the property: jsdom does not implement
  // `isContentEditable`, and the composer's prompt is a Lexical
  // `contenteditable` whose keystrokes can arrive at a node inside it.
  if (target.isContentEditable) return true;
  if (target.closest('[contenteditable=""], [contenteditable="true"]') !== null) return true;
  const tag = target.tagName;
  if (tag === "TEXTAREA" || tag === "SELECT") return true;
  if (tag !== "INPUT") return false;
  // A checkbox or a button-shaped input is not something a letter types into.
  const type = (target as HTMLInputElement).type;
  return !["checkbox", "radio", "button", "submit", "reset", "range", "color", "file"].includes(type);
}

/**
 * Binds the table to the window for as long as the component is mounted.
 *
 * `actions` is read through a ref so a handler that changes on every render —
 * which most of them do, being closures over the open conversation — does not
 * tear the listener down and put it back on each one.
 */
export function useKeybindingActions(actions: KeybindingActions): void {
  const current = React.useRef(actions);
  current.current = actions;

  React.useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.defaultPrevented) return;
      if (
        event.target instanceof HTMLElement &&
        event.target.closest("[data-keybinding-capture]")
      ) {
        return;
      }
      const command = resolveShortcutCommand(event, detentKeybindings);
      if (command === null) return;
      if (isUnmodifiedChord(command) && isEditableTarget(event.target)) return;
      // A project action's chord (decisions.md §18.12). The command names the
      // script slug, so the handler is given that rather than asked to parse
      // it back out — see `runProjectAction` for why it is a member of its own
      // rather than an index signature on the record.
      const scriptId = projectScriptIdFromCommand(command);
      if (scriptId !== null) {
        const run = current.current.runProjectAction;
        if (run === undefined) return;
        if (run(scriptId) === false) return;
        event.preventDefault();
        event.stopPropagation();
        return;
      }
      const action = staticAction(current.current, command);
      if (action === undefined) return;
      // Claim the event only once the handler has acted. A handler that
      // declines leaves the keystroke to the browser, which is what an
      // unavailable command owes the reader.
      if (action() === false) return;
      event.preventDefault();
      event.stopPropagation();
    };
    window.addEventListener("keydown", onKeyDown, true);
    return () => window.removeEventListener("keydown", onKeyDown, true);
  }, []);
}

/**
 * The nullary handler for a static command.
 *
 * `runProjectAction` is the one member of the record that takes an argument,
 * and it is never reached by name — the listener routes `script.*.run` to it
 * before it gets here — so it is excluded from the lookup rather than
 * type-asserted away.
 */
type StaticKeybindingActions = Omit<KeybindingActions, "runProjectAction">;

function staticAction(
  actions: KeybindingActions,
  command: string,
): KeybindingAction | undefined {
  return (actions as StaticKeybindingActions)[command as keyof StaticKeybindingActions];
}

/** Whether a command has a handler in this set. The settings list does not use this; the tests do. */
export function isBound(
  actions: KeybindingActions,
  // `| string` because a project action's command is not in the closed union
  // and never can be: it is `script.<slug>.run`, minted per project
  // (decisions.md §18.12). `shortcutLabelForCommand` is widened for the same
  // reason.
  command: KeybindingCommand | string,
): boolean {
  if (projectScriptIdFromCommand(command) !== null) {
    return actions.runProjectAction !== undefined;
  }
  return staticAction(actions, command) !== undefined;
}
