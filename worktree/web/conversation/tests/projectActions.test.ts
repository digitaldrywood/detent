import { describe, expect, it } from "vitest";

import { mapProjectActions, worktreeCreationActions } from "../src/app/adapters/projectActions.tsx";
import type { Action } from "../src/contracts/work.ts";

function action(overrides: Partial<Action> = {}): Action {
  return {
    id: "action_0123456789abcdef0123456789abcdef",
    name: "Run tests",
    command: "bun test",
    icon: "test",
    open_preview: false,
    run_on_worktree_creation: false,
    created_by: "person_1",
    revision: 1,
    created_at: "2026-09-12T10:00:00Z",
    updated_at: "2026-09-12T10:00:00Z",
    ...overrides,
  } as Action;
}

describe("mapProjectActions", () => {
  it("mints a command-safe slug from the name, not from the hub id", () => {
    const [entry] = mapProjectActions([action()]);
    expect(entry?.script.id).toBe("run-tests");
    expect(entry?.command).toBe("script.run-tests.run");
    expect(entry?.script).toEqual({
      id: "run-tests",
      name: "Run tests",
      command: "bun test",
      icon: "test",
      runOnWorktreeCreate: false,
    });
    // The hub id is still what a write and a run are addressed by.
    expect(entry?.action.id).toBe("action_0123456789abcdef0123456789abcdef");
  });

  it("dedupes two actions with the same name, in authoring order", () => {
    const mapped = mapProjectActions([
      action({ id: "action_a", name: "Test" }),
      action({ id: "action_b", name: "Test" }),
    ]);
    expect(mapped.map((entry) => entry.script.id)).toEqual(["test", "test-2"]);
    // Deterministic: the same input maps the same way every time, which is
    // what a registered chord depends on.
    expect(mapProjectActions([
      action({ id: "action_a", name: "Test" }),
      action({ id: "action_b", name: "Test" }),
    ]).map((entry) => entry.script.id)).toEqual(["test", "test-2"]);
  });

  it("carries the preview URL through, and omits it when there is none", () => {
    const [withUrl] = mapProjectActions([
      action({ preview_url: "http://localhost:5173", open_preview: true }),
    ]);
    expect(withUrl?.script.previewUrl).toBe("http://localhost:5173");
    expect(withUrl?.script.autoOpenPreview).toBe(true);
    const [without] = mapProjectActions([action({ preview_url: "" })]);
    expect(without?.script.previewUrl).toBeUndefined();
  });

  it("names the run-on-worktree-creation set in authoring order", () => {
    const mapped = mapProjectActions([
      action({ id: "action_i", name: "Install", run_on_worktree_creation: true }),
      action({ id: "action_t", name: "Test" }),
      action({ id: "action_b", name: "Build", run_on_worktree_creation: true }),
    ]);
    expect(worktreeCreationActions(mapped).map((entry) => entry.action.name)).toEqual([
      "Install",
      "Build",
    ]);
  });

  // A name that slugifies to nothing still gets a usable command: upstream's
  // `normalizeScriptId` falls back to `script`.
  it("gives an unslugifiable name a usable command", () => {
    const [entry] = mapProjectActions([action({ name: "!!!" })]);
    expect(entry?.script.id).toBe("script");
    expect(entry?.command).toBe("script.script.run");
  });
});
