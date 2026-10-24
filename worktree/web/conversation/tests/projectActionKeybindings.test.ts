// Per-action keybindings (decisions.md §18.12): a chord the project stored
// resolves to that action's command, labels its menu row, appears in the
// Keybindings settings list, loses to a static binding, and stops resolving
// when the action is deleted.
//
// No DOM for the first three groups — the registry and the resolver are plain
// functions, the way `tests/workspaceRelay.test.ts` drives the relay through a
// substituted transport rather than a socket.
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  detentKeybindings,
  keybindingCatalogue,
  isUnmodifiedChord,
  parseChord,
  registerProjectActionKeybindings,
  registeredProjectActionChord,
  resolveShortcutCommand,
  shortcutLabelForCommand,
} from "../src/app/adapters/keybindings.ts";
import { isBound } from "../src/app/adapters/keybindingActions.ts";
import { keybindingValueForCommand } from "../src/lib/projectScriptKeybindings.ts";

const MAC = "MacIntel";
const LINUX = "Linux x86_64";

afterEach(() => registerProjectActionKeybindings([]));

function press(overrides: Partial<KeyboardEvent> = {}): KeyboardEvent {
  return {
    key: "t",
    code: "KeyT",
    metaKey: true,
    ctrlKey: false,
    shiftKey: true,
    altKey: false,
    ...overrides,
  } as KeyboardEvent;
}

describe("parseChord", () => {
  it("is the inverse of the chord strings this client writes", () => {
    expect(parseChord("mod+shift+t")).toEqual({
      key: "t",
      modKey: true,
      metaKey: false,
      ctrlKey: false,
      shiftKey: true,
      altKey: false,
    });

    expect(parseChord("shift+mod+t")).toEqual(parseChord("mod+shift+t"));
    // `space` and `esc` are the two tokens that are not the key's own name.
    expect(parseChord("mod+space")?.key).toBe(" ");
    expect(parseChord("mod+esc")?.key).toBe("escape");
    // The one key whose name is also the separator.
    expect(parseChord("mod++")?.key).toBe("+");
  });

  it("refuses a chord with no modifier and a chord naming two keys", () => {
    expect(parseChord("t")).toBeNull();
    expect(parseChord("")).toBeNull();
    expect(parseChord("mod+t+k")).toBeNull();
  });
});

describe("registerProjectActionKeybindings", () => {
  it("resolves a registered chord to that action's command", () => {
    expect(registerProjectActionKeybindings([{ command: "script.test.run", chord: "mod+shift+t" }]))
      .toEqual(["script.test.run"]);
    expect(resolveShortcutCommand(press(), detentKeybindings, MAC)).toBe("script.test.run");
  });

  it("labels the command, for the menu row and the settings list", () => {
    registerProjectActionKeybindings([{ command: "script.test.run", chord: "mod+shift+t" }]);
    expect(shortcutLabelForCommand(detentKeybindings, "script.test.run", MAC)).toBe("⇧⌘T");
    expect(shortcutLabelForCommand(detentKeybindings, "script.test.run", LINUX)).toBe(
      "Ctrl+Shift+T",
    );
  });

  it("prefills the editor dialog's Keybinding field from the registry", () => {
    registerProjectActionKeybindings([{ command: "script.test.run", chord: "mod+shift+t" }]);
    // The static table has no row for it, so this is the registry fallback in
    // `lib/projectScriptKeybindings.ts` — the one adapted function there.
    expect(keybindingValueForCommand(detentKeybindings, "script.test.run")).toBe("mod+shift+t");
    expect(registeredProjectActionChord("script.other.run")).toBeNull();
  });

  it("puts one row per action in the catalogue, with the Run Script label", () => {
    registerProjectActionKeybindings([
      { command: "script.run-tests.run", chord: "mod+shift+t" },
    ]);
    const row = keybindingCatalogue(MAC).find((entry) => entry.command === "script.run-tests.run");
    expect(row).toEqual({
      command: "script.run-tests.run",
      label: "Run Script: Run Tests",
      chord: "⇧⌘T",
      reason: null,
    });
  });

  it("registers nothing for a chord this build cannot parse", () => {
    expect(registerProjectActionKeybindings([{ command: "script.test.run", chord: "t" }])).toEqual(
      [],
    );
    expect(resolveShortcutCommand(press({ metaKey: false, shiftKey: false }), detentKeybindings, MAC))
      .toBeNull();
  });

  // The collision rule, chosen and written down at `projectActionBindings` in
  // the adapter: the static table wins. A project's author must not be able to
  // take the command palette away from every colleague in the project, because
  // there would be no way for them to get it back.
  it("loses to a static binding on a colliding chord", () => {
    registerProjectActionKeybindings([{ command: "script.test.run", chord: "mod+k" }]);
    const event = press({ key: "k", code: "KeyK", shiftKey: false });
    expect(resolveShortcutCommand(event, detentKeybindings, MAC)).toBe("commandPalette.toggle");
    // The action still reports the chord it is bound to: the clash is visible
    // in the settings list rather than silently rewritten.
    expect(shortcutLabelForCommand(detentKeybindings, "script.test.run", MAC)).toBe("⌘K");
  });

  it("stops resolving a deleted action's chord", () => {
    registerProjectActionKeybindings([{ command: "script.test.run", chord: "mod+shift+t" }]);
    expect(resolveShortcutCommand(press(), detentKeybindings, MAC)).toBe("script.test.run");
    registerProjectActionKeybindings([]);
    expect(resolveShortcutCommand(press(), detentKeybindings, MAC)).toBeNull();
    expect(shortcutLabelForCommand(detentKeybindings, "script.test.run", MAC)).toBeNull();
    expect(
      keybindingCatalogue(MAC).some((entry) => entry.command === "script.test.run"),
    ).toBe(false);
  });

  it("treats a shift-only action chord as one a focused field wins", () => {
    registerProjectActionKeybindings([{ command: "script.test.run", chord: "shift+t" }]);
    expect(isUnmodifiedChord("script.test.run")).toBe(true);
    expect(isUnmodifiedChord("commandPalette.toggle")).toBe(false);
  });
});

// The listener itself needs a window, so it is driven in
// `tests/components/keybindings.test.tsx` beside the rest of the dispatch
// table. What is checkable here is which commands the table claims to bind.
describe("isBound", () => {
  it("reports every script command as bound iff there is a run handler", () => {
    const runProjectAction = vi.fn();
    expect(isBound({ runProjectAction }, "script.test.run")).toBe(true);
    expect(isBound({ runProjectAction }, "script.anything-else.run")).toBe(true);
    expect(isBound({}, "script.test.run")).toBe(false);
    // Not a script command, so the record is still what answers.
    expect(isBound({ runProjectAction }, "diff.toggle")).toBe(false);
  });
});
