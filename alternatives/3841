import * as Schema from "effect/Schema";

import { UpdatesReport } from "../../contracts/account.ts";
import { initialUpdateCheckState, type UpdateCheckState } from "../lib/detentUpdates.ts";
import { hubPath } from "../../runtime/basePath.ts";

export const UPDATE_POLL_INTERVAL_MS = 4 * 60 * 1000;

/** How long a check that found nothing keeps saying so. */
export const UPDATE_UP_TO_DATE_MS = 5000;

let state: UpdateCheckState = initialUpdateCheckState;
const listeners = new Set<() => void>();
let upToDateTimer: ReturnType<typeof setTimeout> | undefined;
let inFlight: Promise<void> | undefined;

const decodeReport = Schema.decodeUnknownSync(UpdatesReport);

function publish(next: UpdateCheckState): void {
  state = next;
  for (const listener of listeners) listener();
}

export function readUpdateCheckState(): UpdateCheckState {
  return state;
}

/**
 * Asks the hub what is behind.
 *
 * A hub that has not been taught `/app/updates` yet answers 404. That is not a
 * failed check, it is a hub with nothing to report, so the pill goes back to
 * idle rather than showing an error nobody can act on.
 */
async function readReport(fetchImpl: typeof globalThis.fetch): Promise<UpdatesReport | null> {
  const response = await fetchImpl(hubPath("/app/updates"), {
    credentials: "same-origin",
    headers: { Accept: "application/json" },
  });
  if (response.status === 404) return null;
  if (!response.ok) {
    throw new Error(`The hub could not be asked what is behind (${response.status}).`);
  }
  return decodeReport(await response.json());
}

/**
 * One check. Concurrent callers — the interval, a focus, a click — share the
 * one request rather than racing, which is what keeps a press during an
 * automatic check from asking twice.
 */
export function checkForUpdates(
  fetchImpl: typeof globalThis.fetch = globalThis.fetch,
): Promise<void> {
  if (inFlight !== undefined) return inFlight;
  clearTimeout(upToDateTimer);
  publish({ ...state, status: "checking", message: null, upToDate: false });
  inFlight = readReport(fetchImpl)
    .then((report) => {
      if (report === null) {
        publish({ ...state, status: "idle", message: null, upToDate: false });
        return;
      }
      if (report.behind_count > 0) {
        publish({ ...state, status: "behind", report, message: null, upToDate: false });
        return;
      }
      publish({ ...state, status: "idle", report, message: null, upToDate: true });
      scheduleUpToDateExpiry();
    })
    .catch((error: unknown) => {
      publish({
        ...state,
        status: "error",
        upToDate: false,
        message: error instanceof Error ? error.message : "The update check failed.",
      });
    })
    .finally(() => {
      inFlight = undefined;
    });
  return inFlight;
}

function scheduleUpToDateExpiry(): void {
  clearTimeout(upToDateTimer);
  upToDateTimer = setTimeout(() => {
    if (state.upToDate) publish({ ...state, upToDate: false });
  }, UPDATE_UP_TO_DATE_MS);
}

export function startUpdateWatch(
  fetchImpl: typeof globalThis.fetch = globalThis.fetch,
): () => void {
  const check = () => void checkForUpdates(fetchImpl);
  const onVisible = () => {
    if (globalThis.document?.visibilityState === "visible") check();
  };
  const interval = setInterval(check, UPDATE_POLL_INTERVAL_MS);
  globalThis.addEventListener?.("focus", check);
  globalThis.document?.addEventListener("visibilitychange", onVisible);
  check();
  return () => {
    clearInterval(interval);
    globalThis.removeEventListener?.("focus", check);
    globalThis.document?.removeEventListener("visibilitychange", onVisible);
  };
}

export function subscribeToUpdateCheck(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** Test seam: drops everything this module remembers between cases. */
export function resetUpdateCheckState(): void {
  clearTimeout(upToDateTimer);
  upToDateTimer = undefined;
  inFlight = undefined;
  state = initialUpdateCheckState;
  listeners.clear();
}
