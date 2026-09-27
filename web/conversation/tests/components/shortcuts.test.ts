// The shell's two keyboard shortcuts (design inventory B.13 rule 4, B.14).
//
// The rule worth a test is the one that goes wrong silently: a single-key
// shortcut that fires while the reader is typing eats the keystroke, and the
// composer is the one place in this client a reader spends their time.
import { describe, expect, it } from "vitest";

import {
  isEditableTarget,
  NEW_CHAT_KEYSHORTCUTS,
  SEARCH_KEYSHORTCUTS,
  shortcutFor,
  type ShortcutEvent,
} from "../../src/app/lib/shortcuts.ts";

function event(overrides: Partial<ShortcutEvent> & { key: string }): ShortcutEvent {
  return {
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
    defaultPrevented: false,
    target: null,
    ...overrides,
  };
}

/** A stand-in for a DOM node, matched by shape rather than by realm. */
function element(tagName: string, attributes: Record<string, string> = {}) {
  return {
    tagName,
    isContentEditable: false,
    getAttribute: (name: string) => attributes[name] ?? null,
  };
}

describe("isEditableTarget", () => {
  it("names the places a reader types", () => {
    expect(isEditableTarget(element("TEXTAREA"))).toBe(true);
    expect(isEditableTarget(element("INPUT"))).toBe(true);
    expect(isEditableTarget(element("SELECT"))).toBe(true);
    expect(isEditableTarget({ ...element("DIV"), isContentEditable: true })).toBe(true);
    expect(isEditableTarget(element("DIV", { role: "textbox" }))).toBe(true);
    expect(isEditableTarget(element("DIV", { role: "searchbox" }))).toBe(true);
  });

  it("leaves ordinary controls and nothing alone", () => {
    expect(isEditableTarget(element("BUTTON"))).toBe(false);
    expect(isEditableTarget(element("A", { href: "/chat" }))).toBe(false);
    expect(isEditableTarget(null)).toBe(false);
    expect(isEditableTarget(undefined)).toBe(false);
  });
});

describe("shortcutFor", () => {
  const cases: ReadonlyArray<{
    readonly name: string;
    readonly event: ShortcutEvent;
    readonly expected: ReturnType<typeof shortcutFor>;
  }> = [
    {
      name: "slash outside a field focuses search",
      event: event({ key: "/", target: element("BODY") }),
      expected: "focus-search",
    },
    {
      name: "slash with no target at all still focuses search",
      event: event({ key: "/" }),
      expected: "focus-search",
    },
    {
      name: "slash inside the composer is a slash",
      event: event({ key: "/", target: element("TEXTAREA") }),
      expected: null,
    },
    {
      name: "slash inside the search box is a slash",
      event: event({ key: "/", target: element("INPUT") }),
      expected: null,
    },
    {
      name: "slash inside a contenteditable region is a slash",
      event: event({ key: "/", target: { ...element("DIV"), isContentEditable: true } }),
      expected: null,
    },
    {
      name: "Meta+/ is not the search shortcut",
      event: event({ key: "/", metaKey: true, target: element("BODY") }),
      expected: null,
    },
    {
      name: "Meta+Shift+N starts a new chat",
      event: event({ key: "N", metaKey: true, shiftKey: true, target: element("BODY") }),
      expected: "new-chat",
    },
    {
      name: "Control+Shift+N starts a new chat",
      event: event({ key: "N", ctrlKey: true, shiftKey: true, target: element("BODY") }),
      expected: "new-chat",
    },
    {
      name: "a layout that reports the unshifted key still starts a new chat",
      event: event({ key: "n", ctrlKey: true, shiftKey: true, target: element("BODY") }),
      expected: "new-chat",
    },
    {
      name: "the new-chat shortcut fires from inside the composer",
      event: event({ key: "N", metaKey: true, shiftKey: true, target: element("TEXTAREA") }),
      expected: "new-chat",
    },
    {
      name: "Meta+N without Shift is the browser's, not ours",
      event: event({ key: "N", metaKey: true, target: element("BODY") }),
      expected: null,
    },
    {
      name: "Alt+Meta+Shift+N is a different gesture",
      event: event({
        key: "N",
        metaKey: true,
        shiftKey: true,
        altKey: true,
        target: element("BODY"),
      }),
      expected: null,
    },
    {
      name: "an event a control already handled is left alone",
      event: event({ key: "/", defaultPrevented: true, target: element("BODY") }),
      expected: null,
    },
    {
      name: "an unrelated key is not a shortcut",
      event: event({ key: "n", target: element("BODY") }),
      expected: null,
    },
  ];

  for (const testCase of cases) {
    it(testCase.name, () => {
      expect(shortcutFor(testCase.event)).toBe(testCase.expected);
    });
  }

  it("advertises both shortcuts in aria-keyshortcuts syntax", () => {
    expect(SEARCH_KEYSHORTCUTS).toBe("/");
    // A space-separated list of "+"-joined key names, so a screen reader can
    // announce the one that applies to the platform it is on.
    expect(NEW_CHAT_KEYSHORTCUTS.split(" ")).toEqual(["Meta+Shift+N", "Control+Shift+N"]);
  });
});
