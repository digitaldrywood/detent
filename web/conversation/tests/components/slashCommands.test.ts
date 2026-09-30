// The composer's slash-command registry and its matching.
//
// Everything here is pure, which is the point of the split: the menu's
// decisions — is this token a command, which rows does it match, which row does
// a press move to — are checkable without a DOM, and the drawing is left to the
// browser spec (`tests/visual/conversation.spec.js`).
import { describe, expect, it, vi } from "vitest";

import {
  builtinSlashCommands,
  filterSlashCommands,
  readSlashQuery,
  resolveSlashCommands,
  stepSlashHighlight,
  type ComposerSlashContext,
  type SlashCommand,
} from "../../src/app/adapters/slashCommands.ts";

function context(overrides: Partial<ComposerSlashContext> = {}): ComposerSlashContext {
  return {
    openPreference: () => {},
    attach: () => {},
    clear: () => {},
    stop: null,
    ...overrides,
  };
}

function names(commands: readonly SlashCommand[]): readonly string[] {
  return commands.map((command) => command.name);
}

describe("readSlashQuery", () => {
  it("opens on a slash at the start of an empty prompt", () => {
    expect(readSlashQuery("/")).toBe("");
  });

  it("reads the name being typed after it", () => {
    expect(readSlashQuery("/mod")).toBe("mod");
    expect(readSlashQuery("/skill:review-pr")).toBe("skill:review-pr");
  });

  it("allows the leading whitespace a half-started draft carries", () => {
    expect(readSlashQuery("\n  /mo")).toBe("mo");
  });

  it("is not a command in the middle of a sentence", () => {
    expect(readSlashQuery("look in src/app")).toBeNull();
    expect(readSlashQuery("ship it /model")).toBeNull();
  });

  // The token ends at the first space: `/model please` is prose about the
  // model, and the menu has to be out of the way for it.
  it("closes once the token stops being the whole prompt", () => {
    expect(readSlashQuery("/model ")).toBeNull();
    expect(readSlashQuery("/model please")).toBeNull();
  });

  it("has no token for an empty prompt", () => {
    expect(readSlashQuery("")).toBeNull();
    expect(readSlashQuery("   ")).toBeNull();
  });
});

describe("filterSlashCommands", () => {
  const commands: readonly SlashCommand[] = [
    { name: "model", description: "", run: () => {} },
    { name: "effort", description: "", run: () => {} },
    { name: "comment", description: "", run: () => {} },
  ];

  it("offers everything for a bare slash", () => {
    expect(names(filterSlashCommands(commands, ""))).toEqual(["model", "effort", "comment"]);
  });

  it("filters by name, ignoring case", () => {
    expect(names(filterSlashCommands(commands, "MOD"))).toEqual(["model"]);
  });

  // "mod" is a prefix of `/model` and appears inside nothing else here; where
  // both kinds match, the prefix is what the reader meant.
  it("ranks a prefix above a containment", () => {
    const withContainment: readonly SlashCommand[] = [
      { name: "remodel", description: "", run: () => {} },
      ...commands,
    ];
    expect(names(filterSlashCommands(withContainment, "mod"))).toEqual(["model", "remodel"]);
  });

  it("keeps the registry's order within each group", () => {
    const matches = filterSlashCommands(commands, "e");
    expect(names(matches)).toEqual(["effort", "model", "comment"]);
  });

  it("matches nothing for a name no command has", () => {
    expect(filterSlashCommands(commands, "zzz")).toHaveLength(0);
  });
});

describe("stepSlashHighlight", () => {
  it("moves down and wraps at the end", () => {
    expect(stepSlashHighlight(3, 0, "down")).toBe(1);
    expect(stepSlashHighlight(3, 2, "down")).toBe(0);
  });

  it("moves up and wraps at the start", () => {
    expect(stepSlashHighlight(3, 1, "up")).toBe(0);
    expect(stepSlashHighlight(3, 0, "up")).toBe(2);
  });

  it("comes back to the first row from an index a shrinking list left behind", () => {
    expect(stepSlashHighlight(2, 7, "down")).toBe(1);
    expect(stepSlashHighlight(2, 7, "up")).toBe(1);
  });

  it("has nothing to highlight in an empty list", () => {
    expect(stepSlashHighlight(0, 0, "down")).toBe(0);
  });
});

describe("builtinSlashCommands", () => {
  it("offers the pickers, the paperclip and the draft on a full composer", () => {
    expect(names(builtinSlashCommands(context()))).toEqual([
      "model",
      "effort",
      "access",
      "attach",
      "clear",
    ]);
  });

  // A command that would do nothing is worse than a command that is not there:
  // the reader cannot tell which they are looking at.
  it("offers no picker commands where the footer has no pickers", () => {
    const commands = builtinSlashCommands(context({ openPreference: null }));
    expect(names(commands)).toEqual(["attach", "clear"]);
  });

  it("offers no attach where the surface takes no files", () => {
    expect(names(builtinSlashCommands(context({ attach: null })))).not.toContain("attach");
  });

  it("offers stop only while a turn is running", () => {
    expect(names(builtinSlashCommands(context()))).not.toContain("stop");
    expect(names(builtinSlashCommands(context({ stop: () => {} })))).toContain("stop");
  });

  it("runs the picker the command names", () => {
    const openPreference = vi.fn();
    const commands = builtinSlashCommands(context({ openPreference }));
    commands.find((command) => command.name === "effort")?.run();
    expect(openPreference).toHaveBeenCalledWith("reasoning_effort");
  });
});

describe("resolveSlashCommands", () => {
  const builtin: readonly SlashCommand[] = [
    { name: "model", description: "built-in", run: () => {} },
    { name: "clear", description: "built-in", run: () => {} },
  ];

  it("appends the surface's own commands after the built-ins", () => {
    const resolved = resolveSlashCommands(builtin, [
      { name: "comment", description: "", run: () => {} },
    ]);
    expect(names(resolved)).toEqual(["model", "clear", "comment"]);
  });

  // Two rows of the same name is a menu that cannot say which one a press runs.
  it("replaces a built-in of the same name in place", () => {
    const resolved = resolveSlashCommands(builtin, [
      { name: "clear", description: "surface", run: () => {} },
    ]);
    expect(names(resolved)).toEqual(["model", "clear"]);
    expect(resolved[1]?.description).toBe("surface");
  });
});
