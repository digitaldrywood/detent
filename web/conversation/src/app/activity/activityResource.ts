import type { AccountApi } from "../account/api.ts";
import type { FleetRunner } from "../../contracts/account.ts";
import type { ActivityReport } from "../../contracts/activity.ts";

export interface ActivityState {
  report: ActivityReport | null;
  runners: readonly FleetRunner[];
  pending: boolean;
  error: string | null;
}

interface ActivityResource {
  inFlight: Promise<void> | null;
  startedAt: number;
  reports: Map<string, ActivityState>;
}

const resources = new WeakMap<AccountApi, ActivityResource>();

export function observeActivity(
  api: AccountApi,
  key: string,
  project: string,
  update: (state: ActivityState) => void,
) {
  let resource = resources.get(api);
  if (!resource) {
    resource = { inFlight: null, startedAt: -Infinity, reports: new Map() };
    resources.set(api, resource);
  }
  const shared = resource;
  let current = true;
  let stale = true;
  let waiting = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let state = shared.reports.get(key) ?? {
    report: null, runners: [], pending: true, error: null,
  };
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
    shared.inFlight = (async () => {
      try {
        const [report, fleet] = await Promise.allSettled([
          api.activity(project ? { project_id: project } : {}), api.fleet(),
        ]);
        if (report.status === "rejected" || fleet.status === "rejected") throw new Error("Activity read failed");
        state = { report: report.value, runners: fleet.value.runners, pending: false, error: null };
        shared.reports.set(key, state);
      } catch {
        state = { ...state, pending: false, error: "Activity is temporarily unavailable." };
      }
      if (current) update(state);
    })().then(() => {
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
