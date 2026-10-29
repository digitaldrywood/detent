// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { activateSidebarProjectScope, parsePersistedState, persistState, PERSISTED_STATE_KEY, setSidebarProjectScopeKey, setSidebarProjectWorkingSetKeys } from "../../src/uiStateStore.ts";

afterEach(() => localStorage.clear());

describe("project working set persistence", () => {
  it.each([
    { legacy: "project-one", keys: ["project-one"], focus: "project-one" },
    { legacy: null, keys: [], focus: null },
  ])("migrates legacy scope $legacy and persists the new shape", ({ legacy, keys, focus }) => {
    const state = parsePersistedState({ sidebarProjectScopeKey: legacy });
    expect(state.sidebarProjectWorkingSetKeys).toEqual(keys);
    expect(state.sidebarProjectScopeKey).toBe(focus);
    persistState(state);
    const stored = JSON.parse(localStorage.getItem(PERSISTED_STATE_KEY)!);
    expect(stored.sidebarProjectWorkingSetKeys).toEqual(keys);
    expect(parsePersistedState(stored).sidebarProjectScopeKey).toBe(focus);
  });

  it("keeps focus in the working set and clears a removed focus", () => {
    let state = parsePersistedState({ sidebarProjectWorkingSetKeys: ["one", "two"], sidebarProjectScopeKey: "one" });
    state = setSidebarProjectScopeKey(state, null);
    expect(state.sidebarProjectWorkingSetKeys).toEqual(["one", "two"]);
    state = setSidebarProjectScopeKey(state, "two");
    state = setSidebarProjectWorkingSetKeys(state, ["one", "one"]);
    expect(state.sidebarProjectScopeKey).toBeNull();
    expect(state.sidebarProjectWorkingSetKeys).toEqual(["one"]);
    state = setSidebarProjectScopeKey(state, "three");
    expect(state.sidebarProjectWorkingSetKeys).toEqual(["one", "three"]);
    state = setSidebarProjectWorkingSetKeys(state, []);
    expect(state.sidebarProjectScopeKey).toBeNull();
  });

  it("isolates users and restores each user's working set and focus after reload", () => {
    let state = activateSidebarProjectScope(parsePersistedState({ sidebarProjectScopeKey: "one" }), "user-one");
    state = setSidebarProjectWorkingSetKeys(state, ["one", "two"]);
    state = setSidebarProjectScopeKey(state, "two");
    state = activateSidebarProjectScope(state, "user-two");
    expect(state.sidebarProjectWorkingSetKeys).toEqual([]);
    expect(state.sidebarProjectScopeKey).toBeNull();
    state = setSidebarProjectWorkingSetKeys(state, ["three"]);
    state = setSidebarProjectScopeKey(state, "three");
    persistState(state);
    state = parsePersistedState(JSON.parse(localStorage.getItem(PERSISTED_STATE_KEY)!));
    state = activateSidebarProjectScope(state, "user-one");
    expect(state.sidebarProjectWorkingSetKeys).toEqual(["one", "two"]);
    expect(state.sidebarProjectScopeKey).toBe("two");
    state = activateSidebarProjectScope(state, "user-two");
    expect(state.sidebarProjectWorkingSetKeys).toEqual(["three"]);
    expect(state.sidebarProjectScopeKey).toBe("three");
  });
});
