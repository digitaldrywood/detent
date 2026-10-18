import { describe, expect, it } from "vitest";

import { MAX_KEYBINDING_VALUE_LENGTH, MAX_SCRIPT_ID_LENGTH } from "../src/contracts/ui.ts";
import { shortcutLabelForCommand } from "../src/app/adapters/keybindings.ts";
import {
  buildProjectScript,
  commandForProjectScript,
  nextProjectScriptId,
  primaryProjectScript,
  projectScriptIdFromCommand,
} from "../src/projectScripts.ts";
import {
  decodeProjectScriptKeybindingRule,
  keybindingValueForCommand,
  PROJECT_SCRIPT_KEYBINDING_INVALID_MESSAGE,
} from "../src/lib/projectScriptKeybindings.ts";

const NO_BINDINGS = { bindings: {} } as const;

describe("projectScripts helpers", () => {
  it("builds scripts with preview settings", () => {
    expect(
      buildProjectScript("dev", {
        name: "Dev server",
        command: "pnpm dev",
        icon: "debug",
        runOnWorktreeCreate: false,
        previewUrl: "http://localhost:5733",
        autoOpenPreview: true,
      }),
    ).toEqual({
      id: "dev",
      name: "Dev server",
      command: "pnpm dev",
      icon: "debug",
      runOnWorktreeCreate: false,
      previewUrl: "http://localhost:5733",
      autoOpenPreview: true,
    });
  });

  it("omits preview settings when no preview URL is configured", () => {
    expect(
      buildProjectScript("test", {
        name: "Test",
        command: "pnpm test",
        icon: "test",
        runOnWorktreeCreate: false,
        previewUrl: null,
        autoOpenPreview: false,
      }),
    ).toEqual({
      id: "test",
      name: "Test",
      command: "pnpm test",
      icon: "test",
      runOnWorktreeCreate: false,
    });
  });

  it("builds and parses script run commands", () => {
    const command = commandForProjectScript("lint");
    expect(command).toBe("script.lint.run");
    expect(projectScriptIdFromCommand(command ?? "")).toBe("lint");
    expect(projectScriptIdFromCommand("terminal.toggle")).toBeNull();
  });

  it.each(["install-javascript-dependencies", "A", "a.b", "a b", "-a", "", "a".repeat(25)])(
    "omits the shortcut for legacy script ID %j without crashing script menus",
    (id) => {
      const commands = ["lint", id, "test"].map(commandForProjectScript);
      expect(commands).toEqual(["script.lint.run", null, "script.test.run"]);
      expect(commands.map((command) => shortcutLabelForCommand(NO_BINDINGS, command))).toEqual([
        null,
        null,
        null,
      ]);
    },
  );

  // A hub action id is `action_<32 hex>`, which is exactly one of the legacy
  // shapes above: an underscore and 39 characters. That is why
  // `app/adapters/projectActions.tsx` mints a slug rather than addressing an
  // action by its hub id — without it no action could ever carry a chord.
  it("refuses to mint a command for a hub action id", () => {
    expect(commandForProjectScript("action_0123456789abcdef0123456789abcdef")).toBeNull();
  });

  it("preserves the exact ID at the shortcut length limit", () => {
    const id = "a".repeat(MAX_SCRIPT_ID_LENGTH);
    expect(projectScriptIdFromCommand(commandForProjectScript(id) ?? "")).toBe(id);
  });

  it("slugifies and dedupes project script ids", () => {
    expect(nextProjectScriptId("Run Tests", [])).toBe("run-tests");
    expect(nextProjectScriptId("Run Tests", ["run-tests"])).toBe("run-tests-2");
    expect(nextProjectScriptId("!!!", [])).toBe("script");
  });

  it("resolves primary and setup scripts", () => {
    const scripts = [
      {
        id: "setup",
        name: "Setup",
        command: "bun install",
        icon: "configure" as const,
        runOnWorktreeCreate: true,
      },
      {
        id: "test",
        name: "Test",
        command: "bun test",
        icon: "test" as const,
        runOnWorktreeCreate: false,
      },
    ];

    expect(primaryProjectScript(scripts)?.id).toBe("test");
  });
});

describe("projectScriptKeybindings", () => {
  it("decodes and trims valid keybinding rules", () => {
    const rule = decodeProjectScriptKeybindingRule({
      keybinding: "  mod+k  ",
      command: commandForProjectScript("lint"),
    });

    expect(rule).toEqual({
      key: "mod+k",
      command: "script.lint.run",
    });
  });

  it("returns null when keybinding is empty", () => {
    expect(
      decodeProjectScriptKeybindingRule({
        keybinding: "   ",
        command: commandForProjectScript("lint"),
      }),
    ).toBeNull();
  });

  it("rejects invalid keybinding values", () => {
    expect(() =>
      decodeProjectScriptKeybindingRule({
        keybinding: "k".repeat(MAX_KEYBINDING_VALUE_LENGTH + 1),
        command: commandForProjectScript("lint"),
      }),
    ).toThrowError(PROJECT_SCRIPT_KEYBINDING_INVALID_MESSAGE);
  });

  it("rejects invalid commands", () => {
    expect(() =>
      decodeProjectScriptKeybindingRule({
        keybinding: "mod+k",
        command: "script.BAD.run",
      }),
    ).toThrowError(PROJECT_SCRIPT_KEYBINDING_INVALID_MESSAGE);
  });

  it("can edit or delete a legacy script without a shortcut", () => {
    const command = commandForProjectScript("install-javascript-dependencies");
    expect(keybindingValueForCommand(NO_BINDINGS, command)).toBeNull();
    expect(decodeProjectScriptKeybindingRule({ keybinding: null, command })).toBeNull();
    expect(() => decodeProjectScriptKeybindingRule({ keybinding: "mod+k", command })).toThrowError(
      PROJECT_SCRIPT_KEYBINDING_INVALID_MESSAGE,
    );
  });

  // Upstream's "reads latest matching keybinding value for a command", over
  // Detent's shape: their two rules for one command become that command's two
  // chord strings, and "last wins" is the last entry rather than a backwards
  // walk of the array.
  it("reads the last bound chord for a command", () => {
    expect(
      keybindingValueForCommand(
        { bindings: { "script.test.run": ["mod+esc", "mod+shift+k"] } },
        "script.test.run",
      ),
    ).toBe("mod+shift+k");
  });
});
