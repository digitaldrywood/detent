// @vitest-environment jsdom
// The board's URL state: parse, serialize, and the round trip between them.
import { afterEach, describe, expect, it } from "vitest";

import {
  activeFilterCount,
  DEFAULT_VIEW_STATE,
  isDefaultViewState,
  laneVisible,
  parseViewState,
  readStoredViewState,
  serializeViewState,
  toggleFilter,
  toggleLane,
  writeStoredViewState,
} from "../../src/app/work/lib/viewState.ts";

const LANES = ["Backlog", "Todo", "In progress", "Review", "Done"];

describe("the board's view state", () => {
  it("defaults an empty query string", () => {
    expect(parseViewState("")).toEqual(DEFAULT_VIEW_STATE);
    expect(isDefaultViewState(DEFAULT_VIEW_STATE)).toBe(true);
  });

  it("reads every control out of the query string", () => {
    const state = parseViewState(
      "view=list&q=lease&state=Todo,Review&label=bug&assignee=michael&priority=High&sort=updated&lanes=Todo,Done",
    );
    expect(state.view).toBe("list");
    expect(state.q).toBe("lease");
    expect(state.state).toEqual(["Review", "Todo"]);
    expect(state.label).toEqual(["bug"]);
    expect(state.assignee).toEqual(["michael"]);
    expect(state.priority).toEqual(["High"]);
    expect(state.sort).toBe("updated");
    expect(state.lanes).toEqual(["Done", "Todo"]);
  });

  it("falls back rather than failing on a value it does not know", () => {
    const state = parseViewState("view=gallery&sort=vibes");
    expect(state.view).toBe("board");
    expect(state.sort).toBe("priority");
  });

  it("omits defaults when it serializes", () => {
    expect(serializeViewState(DEFAULT_VIEW_STATE)).toBe("");
    expect(serializeViewState({ ...DEFAULT_VIEW_STATE, view: "list" })).toBe("view=list");
  });

  it("round-trips", () => {
    const query =
      "view=list&q=lease&state=Review%2CTodo&label=bug&assignee=michael&priority=High&sort=updated&lanes=Done%2CTodo";
    expect(serializeViewState(parseViewState(query))).toBe(query);
  });

  it("treats two orderings of the same filter as one URL", () => {
    expect(serializeViewState(parseViewState("state=Review,Todo"))).toBe(
      serializeViewState(parseViewState("state=Todo,Review")),
    );
  });

  it("distinguishes every lane from no lane", () => {
    expect(parseViewState("").lanes).toBeNull();
    expect(parseViewState("lanes=").lanes).toEqual([]);
    expect(laneVisible(parseViewState(""), "Todo")).toBe(true);
    expect(laneVisible(parseViewState("lanes="), "Todo")).toBe(false);
  });

  it("toggles a filter value on and off and counts what is active", () => {
    let state = toggleFilter(DEFAULT_VIEW_STATE, "label", "bug");
    state = toggleFilter(state, "state", "Todo");
    expect(activeFilterCount(state)).toBe(2);
    state = toggleFilter(state, "label", "bug");
    expect(state.label).toEqual([]);
    expect(activeFilterCount(state)).toBe(1);
  });

  it("returns to null once every lane is back on", () => {
    const hidden = toggleLane(DEFAULT_VIEW_STATE, "Done", LANES);
    expect(hidden.lanes).toEqual(["Backlog", "Todo", "In progress", "Review"]);
    expect(toggleLane(hidden, "Done", LANES).lanes).toBeNull();
  });

  it("keeps the workflow's lane order when lanes are toggled out of order", () => {
    let state = toggleLane(DEFAULT_VIEW_STATE, "Backlog", LANES);
    state = toggleLane(state, "Review", LANES);
    state = toggleLane(state, "Backlog", LANES);
    expect(state.lanes).toEqual(["Backlog", "Todo", "In progress", "Done"]);
  });
});

describe("the remembered view", () => {
  afterEach(() => globalThis.localStorage?.clear());

  it("remembers and reads back a view per project", () => {
    writeStoredViewState("proj_alpha", { ...DEFAULT_VIEW_STATE, view: "list", q: "lease" });
    expect(readStoredViewState("proj_alpha")?.view).toBe("list");
    expect(readStoredViewState("proj_alpha")?.q).toBe("lease");
    expect(readStoredViewState("proj_beta")).toBeNull();
  });

  it("forgets a view that is back to the default", () => {
    writeStoredViewState("proj_alpha", { ...DEFAULT_VIEW_STATE, view: "list" });
    writeStoredViewState("proj_alpha", DEFAULT_VIEW_STATE);
    expect(readStoredViewState("proj_alpha")).toBeNull();
  });
});
