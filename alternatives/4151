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
    const minimum = state.report?.minimum_runner_version ?? state.report?.current ?? "";
    return `${count} ${count === 1 ? "runner" : "runners"} too old to take work, needs ${minimum}`;
  }
  if (state.status === "error") return "Couldn't check for updates · Retry";
  return state.upToDate ? "All runners up to date" : "Check for updates";
}
