// The board's view state, and where it lives.
//
// The URL is the source of truth: a filtered board is a thing people paste to
// each other, and a filter that only exists in component state cannot be
// linked, reloaded or navigated back to. `localStorage` is a second-class
// memory used for exactly one thing — the view you last had *in this project*
// when you arrive with no query string at all — so a shared link always wins
// over a remembered preference.
//
// Everything here is pure apart from the two storage helpers, which swallow a
// blocked store: a private window costs the reader a remembered filter, not
// the board.

export const WORK_VIEWS = ["board", "list"] as const;
export type WorkViewMode = (typeof WORK_VIEWS)[number];

export const WORK_SORTS = ["priority", "updated", "created", "title"] as const;
export type WorkSort = (typeof WORK_SORTS)[number];

export const SORT_LABELS: Readonly<Record<WorkSort, string>> = {
  priority: "Priority",
  updated: "Recently updated",
  created: "Newest",
  title: "Title",
};

export interface WorkViewState {
  readonly view: WorkViewMode;
  readonly q: string;
  readonly state: readonly string[];
  readonly label: readonly string[];
  readonly assignee: readonly string[];
  readonly priority: readonly string[];
  readonly sort: WorkSort;
  /**
   * The lanes the reader chose to show. `null` means "every lane", which is
   * not the same as an empty list ("none") and is why this is nullable rather
   * than defaulted to the full set: the full set is not known until the
   * project's workflow has loaded, and a default written before then would
   * freeze whatever lanes happened to exist that day.
   */
  readonly lanes: readonly string[] | null;
}

export const DEFAULT_VIEW_STATE: WorkViewState = {
  view: "board",
  q: "",
  state: [],
  label: [],
  assignee: [],
  priority: [],
  sort: "priority",
  lanes: null,
};

/** The multi-valued filters, in the order the Filters menu renders them. */
export const FILTER_KEYS = ["state", "label", "assignee", "priority"] as const;
export type FilterKey = (typeof FILTER_KEYS)[number];

export const FILTER_LABELS: Readonly<Record<FilterKey, string>> = {
  state: "State",
  label: "Label",
  assignee: "Assignee",
  priority: "Priority",
};

function readList(raw: string | null): readonly string[] {
  if (raw === null) return [];
  const items = raw
    .split(",")
    .map((value) => value.trim())
    .filter((value) => value.length > 0);
  // Deduplicated and ordered, so two URLs that mean the same thing serialize
  // the same way and the "is this the default?" check below is an equality.
  return [...new Set(items)].toSorted();
}

function writeList(values: readonly string[]): string {
  return [...new Set(values)].toSorted().join(",");
}

/**
 * Reads the view out of a query string. Anything unrecognised falls back to
 * the default rather than failing: a stale link from an older build has to
 * still open the board.
 */
export function parseViewState(search: string | URLSearchParams): WorkViewState {
  const params = typeof search === "string" ? new URLSearchParams(search) : search;
  const view = params.get("view");
  const sort = params.get("sort");
  const lanes = params.get("lanes");
  return {
    view: WORK_VIEWS.includes(view as WorkViewMode) ? (view as WorkViewMode) : DEFAULT_VIEW_STATE.view,
    q: params.get("q") ?? "",
    state: readList(params.get("state")),
    label: readList(params.get("label")),
    assignee: readList(params.get("assignee")),
    priority: readList(params.get("priority")),
    sort: WORK_SORTS.includes(sort as WorkSort) ? (sort as WorkSort) : DEFAULT_VIEW_STATE.sort,
    // `lanes=` with an empty value is "no lanes", which is a legitimate (if
    // odd) thing to link to; a missing `lanes` is "every lane".
    lanes: lanes === null ? null : readList(lanes),
  };
}

/**
 * The query string for a view. Defaults are omitted, so an untouched board has
 * a clean URL and the Reset control has something honest to compare against.
 */
export function serializeViewState(state: WorkViewState): string {
  const params = new URLSearchParams();
  if (state.view !== DEFAULT_VIEW_STATE.view) params.set("view", state.view);
  if (state.q.trim().length > 0) params.set("q", state.q.trim());
  for (const key of FILTER_KEYS) {
    const values = state[key];
    if (values.length > 0) params.set(key, writeList(values));
  }
  if (state.sort !== DEFAULT_VIEW_STATE.sort) params.set("sort", state.sort);
  if (state.lanes !== null) params.set("lanes", writeList(state.lanes));
  return params.toString();
}

/** True when nothing is filtered, searched, sorted or hidden. */
export function isDefaultViewState(state: WorkViewState): boolean {
  return serializeViewState(state) === "";
}

/** How many filter values are active, for the Filters button's count badge. */
export function activeFilterCount(state: WorkViewState): number {
  return FILTER_KEYS.reduce((total, key) => total + state[key].length, 0);
}

/** Adds or removes one value from one multi-valued filter. */
export function toggleFilter(
  state: WorkViewState,
  key: FilterKey,
  value: string,
): WorkViewState {
  const current = state[key];
  const next = current.includes(value)
    ? current.filter((candidate) => candidate !== value)
    : [...current, value].toSorted();
  return { ...state, [key]: next };
}

/** Shows or hides one lane. `null` (every lane) expands first, then removes. */
export function toggleLane(
  state: WorkViewState,
  lane: string,
  allLanes: readonly string[],
): WorkViewState {
  const current = state.lanes ?? allLanes;
  const next = current.includes(lane)
    ? current.filter((candidate) => candidate !== lane)
    : [...current, lane];
  // Back to every lane is expressed as `null`, not as the full list: the full
  // list would pin today's workflow into tomorrow's URL.
  const ordered = allLanes.filter((candidate) => next.includes(candidate));
  return { ...state, lanes: ordered.length === allLanes.length ? null : ordered };
}

/** Whether a lane is visible under this view. */
export function laneVisible(state: WorkViewState, lane: string): boolean {
  return state.lanes === null || state.lanes.includes(lane);
}

const STORAGE_PREFIX = "detent.work.view:";

/** The remembered view for a project, or `null` when there is none. */
export function readStoredViewState(projectId: string): WorkViewState | null {
  try {
    const raw = globalThis.localStorage?.getItem(`${STORAGE_PREFIX}${projectId}`);
    if (raw === null || raw === undefined || raw === "") return null;
    return parseViewState(raw);
  } catch {
    return null;
  }
}

/** Remembers the view for a project. A default view clears the memory. */
export function writeStoredViewState(projectId: string, state: WorkViewState): void {
  try {
    const key = `${STORAGE_PREFIX}${projectId}`;
    const serialized = serializeViewState(state);
    if (serialized === "") globalThis.localStorage?.removeItem(key);
    else globalThis.localStorage?.setItem(key, serialized);
  } catch {
    // A blocked store only costs the remembered view.
  }
}

/** The scope key a view is remembered under. `""` is the all-projects board. */
export function viewScopeKey(projectId: string | null): string {
  return projectId ?? "";
}
