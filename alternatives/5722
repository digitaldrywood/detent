import type { WorkHttp } from "../work/lib/workHttp.ts";
import { subscribeProjectQuery } from "../work/lib/projectEvents.ts";

import type { AccountApi } from "../account/api.ts";
import type { FleetRunner } from "../../contracts/account.ts";
import type { ActivityReport } from "../../contracts/activity.ts";
import { getQueryData, prefetchQuery, scopeOf, setQueryData } from "../lib/queryCache.ts";

export interface ActivityState {
  report: ActivityReport | null;
  runners: readonly FleetRunner[];
  pending: boolean;
  error: string | null;
}

interface ActivityData {
  readonly report: ActivityReport;
  readonly runners: readonly FleetRunner[];
}

/** The cache key for one Activity scope: the selected project and the projects it spans. */
export function activityKey(project: string, projects: readonly string[]): string {
  return JSON.stringify([project, projects.toSorted()]);
}

function cacheKey(api: AccountApi, key: string) {
  return ["activity", scopeOf(api), key] as const;
}

function readActivity(api: AccountApi, project: string) {
  return async (signal: AbortSignal): Promise<ActivityData> => {
    const controller = new AbortController();
    const cancel = () => controller.abort();
    signal.addEventListener("abort", cancel);
    let expiry: ReturnType<typeof setTimeout> | undefined;
    let rejectAbort: () => void = () => {};
    const aborted = new Promise<never>((_resolve, reject) => {
      rejectAbort = () => reject(new Error("Activity read cancelled"));
      controller.signal.addEventListener("abort", rejectAbort, { once: true });
    });
    if (signal.aborted) controller.abort();
    const deadline = new Promise<never>((_resolve, reject) => {
      expiry = setTimeout(() => {
        controller.abort();
        reject(new Error("Activity read timed out"));
      }, 30_000);
    });
    try {
      const [report, fleet] = await Promise.race([
        Promise.allSettled([
          api.activity(project ? { project_id: project } : {}, controller.signal),
          api.fleet(controller.signal),
        ]),
        deadline,
        aborted,
      ]);
      if (report.status === "rejected" || fleet.status === "rejected") throw new Error("Activity read failed");
      return { report: report.value, runners: fleet.value.runners };
    } catch (cause) {
      controller.abort();
      throw cause;
    } finally {
      if (expiry !== undefined) clearTimeout(expiry);
      signal.removeEventListener("abort", cancel);
      controller.signal.removeEventListener("abort", rejectAbort);
    }
  };
}

/** Starts the Activity read for a reader about to open the page. */
export function prefetchActivity(api: AccountApi, projects: readonly string[]): void {
  prefetchQuery(cacheKey(api, activityKey("", projects)), readActivity(api, ""), 10_000);
}

export function observeActivity(
  api: AccountApi,
  key: string,
  project: string,
  update: (state: ActivityState) => void,
  http: WorkHttp,
  projects: readonly string[],
  live: (value: boolean) => void,
) {
  const entry = cacheKey(api, key);
  let current = true;
  let scheduled = false;
  const connected = new Set<string>();
  const cached = getQueryData<ActivityData>(entry);
  let state: ActivityState = cached === undefined
    ? { report: null, runners: [], pending: true, error: null }
    : { report: cached.report, runners: cached.runners, pending: false, error: null };
  update(state);
  const refresh = () => {
    if (scheduled || !current) return;
    scheduled = true;
    queueMicrotask(() => { scheduled = false; if (current) void load(); });
  };
  const queries = projects.map((id) => subscribeProjectQuery(http, id, {
    topics: ["activity", "work_item", "board", "lane"], apply: refresh, refresh,
    connection: {
      open: () => { connected.add(id); live(connected.size === projects.length); },
      error: () => { connected.delete(id); live(false); },
    },
  }));
  let generation = 0;
  let controller: AbortController | undefined;
  const load = async () => {
    const version = ++generation;
    controller?.abort();
    controller = new AbortController();
    const snapshots = queries.map((query) => query.beginSnapshot());
    const data = await readActivity(api, project)(controller.signal).catch(() => undefined);
    if (!current || version !== generation || snapshots.some((snapshot) => !snapshot.current())) return;
    state = data === undefined
      ? { ...state, pending: false, error: "Activity is temporarily unavailable." }
      : { report: data.report, runners: data.runners, pending: false, error: null };
    if (data !== undefined) setQueryData(entry, data);
    update(state);
    snapshots.forEach((snapshot, index) => snapshot.complete(data?.report.change_sequences?.[projects[index]!]));
  };
  void load();
  return { refresh, close: () => { current = false; generation++; controller?.abort(); queries.forEach((query) => query.close()); } };
}
