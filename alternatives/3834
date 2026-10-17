import type { AccountApi } from "../account/api.ts";
import type { FleetRunner } from "../../contracts/account.ts";
import type { ActivityReport } from "../../contracts/activity.ts";
import { fetchQuery, getQueryData, prefetchQuery, scopeOf } from "../lib/queryCache.ts";

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

interface ActivityResource {
  inFlight: Promise<void> | null;
  startedAt: number;
}

const resources = new WeakMap<AccountApi, ActivityResource>();

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
      ]);
      if (report.status === "rejected" || fleet.status === "rejected") throw new Error("Activity read failed");
      return { report: report.value, runners: fleet.value.runners };
    } catch (cause) {
      controller.abort();
      throw cause;
    } finally {
      if (expiry !== undefined) clearTimeout(expiry);
      signal.removeEventListener("abort", cancel);
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
) {
  let resource = resources.get(api);
  if (!resource) {
    resource = { inFlight: null, startedAt: -Infinity };
    resources.set(api, resource);
  }
  const shared = resource;
  const entry = cacheKey(api, key);
  let current = true;
  let stale = true;
  let waiting = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const cached = getQueryData<ActivityData>(entry);
  let state: ActivityState = cached === undefined
    ? { report: null, runners: [], pending: true, error: null }
    : { report: cached.report, runners: cached.runners, pending: false, error: null };
  update(state);

  const schedule = () => {
    if (!current || !stale || timer !== undefined || waiting || document.visibilityState !== "visible") return;
    if (shared.inFlight) {
      waiting = true;
      void shared.inFlight.then(() => {
        waiting = false;
        schedule();
      });
      return;
    }
    const delay = Math.max(0, shared.startedAt + 2000 - performance.now());
    if (delay > 0) {
      timer = setTimeout(() => {
        timer = undefined;
        schedule();
      }, delay);
      return;
    }
    stale = false;
    shared.startedAt = performance.now();
    shared.inFlight = fetchQuery(entry, readActivity(api, project), { force: true, join: true }).then((data) => {
      state = data === undefined
        ? { ...state, pending: false, error: "Activity is temporarily unavailable." }
        : { report: data.report, runners: data.runners, pending: false, error: null };
      if (current) update(state);
    }).then(() => {
      shared.inFlight = null;
      schedule();
    });
  };
  schedule();
  return {
    refresh: () => { stale = true; schedule(); },
    close: () => {
      current = false;
      if (timer !== undefined) clearTimeout(timer);
    },
  };
}
