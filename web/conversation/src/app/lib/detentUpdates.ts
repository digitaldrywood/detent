import type { DesktopUpdateStatusIconState } from "../../components/sidebar/DesktopUpdateStatusIcon.tsx";
import type { UpdatesReport } from "../../contracts/account.ts";

export type UpdateCheckStatus = "idle" | "checking" | "behind" | "error";

export interface UpdateCheckState {
  readonly status: UpdateCheckStatus;
  /** The last report the hub gave, or null before the first check landed. */
  readonly report: UpdatesReport | null;
  /** What went wrong on the last check, for the error toast's description. */
  readonly message: string | null;

  readonly upToDate: boolean;
}

export const initialUpdateCheckState: UpdateCheckState = {
  status: "idle",
  report: null,
  message: null,
  upToDate: false,
};

/** Where the update state sends a reader. There is nothing to press here. */
export const RUNNER_SETTINGS_PATH = "/settings/runners";

/** What an operator runs on a host to bring it up to the hub's build. */
export const RUNNER_UPGRADE_COMMAND = "detent update";

export type UpdateButtonAction = "show" | "check" | "none";

export function resolveUpdateButtonAction(state: UpdateCheckState): UpdateButtonAction {
  if (state.status === "checking") return "none";
  if (state.status === "behind") return "show";
  return "check";
}

export function isUpdateButtonDisabled(state: UpdateCheckState): boolean {
  return state.status === "checking";
}

export function updateIconStatus(state: UpdateCheckState): DesktopUpdateStatusIconState {
  if (state.status === "checking") return "checking";
  if (state.status === "behind") return "available";
  return "idle";
}

export function showsUpdateDetail(state: UpdateCheckState): boolean {
  return state.status === "behind";
}

/** How many runners the report says are behind. */
export function behindCount(state: UpdateCheckState): number {
  return state.status === "behind" ? (state.report?.behind_count ?? 0) : 0;
}

export function getUpdateButtonTooltip(state: UpdateCheckState): string {
  if (state.status === "checking") return "Checking for updates…";
  if (state.status === "behind") {
    const count = behindCount(state);
    return `${count} ${count === 1 ? "runner" : "runners"} behind · Update`;
  }
  if (state.status === "error") return "Couldn't check for updates · Retry";
  return state.upToDate ? "All runners up to date" : "Check for updates";
}

/**
 * What a behind row on Settings → Providers & runners says above its command.
 * A runner that has never reported a version has nothing to show an arrow
 * from, so it is named without one.
 */
export function runnerUpdateLabel(reported: string, current: string): string {
  const from = reported.trim();
  const to = current.trim();
  if (from === "") return `Update available: ${to}`;
  return `Update available: ${from} → ${to}`;
}
