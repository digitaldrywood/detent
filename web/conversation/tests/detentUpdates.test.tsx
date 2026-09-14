// @vitest-environment jsdom
import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  UPDATE_POLL_INTERVAL_MS,
  UPDATE_UP_TO_DATE_MS,
  checkForUpdates,
  readUpdateCheckState,
  resetUpdateCheckState,
} from "../src/app/adapters/detentUpdates.ts";
import {
  RUNNER_SETTINGS_PATH,
  RUNNER_UPGRADE_COMMAND,
  behindCount,
  getUpdateButtonTooltip,
  initialUpdateCheckState,
  resolveUpdateButtonAction,
  runnerUpdateLabel,
  updateIconStatus,
} from "../src/app/lib/detentUpdates.ts";
import { hostIsBehind } from "../src/app/fleet/HostCard.tsx";
import { renderSidebar, resetSidebarState } from "./components/sidebarHarness.tsx";

const CLIENT = {
  version: "v1.2.4",
  build: "aaaa",
  served_at: "2026-09-12T09:00:00Z",
};

function report(behind: number) {
  return {
    current: "v1.2.4",
    source: "hub",
    runners: [
      {
        runner_id: "rnr_a",
        display_name: "Athens",
        version: behind > 0 ? "v1.2.3" : "v1.2.4",
        online: true,
        behind: behind > 0,
      },
      {
        runner_id: "rnr_b",
        display_name: "Dublin",
        version: behind > 1 ? "v1.2.3" : "v1.2.4",
        online: true,
        behind: behind > 1,
      },
    ],
    behind_count: behind,
    client: CLIENT,
  };
}

/** An `/app/updates` that always answers with one report. */
function hubReporting(behind: number): typeof globalThis.fetch {
  return vi.fn(
    async () =>
      new Response(JSON.stringify(report(behind)), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
  ) as unknown as typeof globalThis.fetch;
}

function hubRefusing(status: number): typeof globalThis.fetch {
  return vi.fn(async () => new Response("", { status })) as unknown as typeof globalThis.fetch;
}

beforeEach(() => {
  resetUpdateCheckState();
  resetSidebarState();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  resetUpdateCheckState();
});

describe("what the pill says", () => {
  it("says the sentence for each state, and Michael's for the two about runners", () => {
    expect(getUpdateButtonTooltip(initialUpdateCheckState)).toBe("Check for updates");
    expect(resolveUpdateButtonAction(initialUpdateCheckState)).toBe("check");
    expect(updateIconStatus(initialUpdateCheckState)).toBe("idle");

    const checking = { ...initialUpdateCheckState, status: "checking" } as const;
    expect(getUpdateButtonTooltip(checking)).toBe("Checking for updates…");
    // A press during a check does nothing rather than asking twice.
    expect(resolveUpdateButtonAction(checking)).toBe("none");
    expect(updateIconStatus(checking)).toBe("checking");

    const upToDate = { ...initialUpdateCheckState, report: report(0), upToDate: true };
    expect(getUpdateButtonTooltip(upToDate)).toBe("All runners up to date");

    const one = { ...initialUpdateCheckState, status: "behind", report: report(1) } as const;
    expect(getUpdateButtonTooltip(one)).toBe("1 runner behind · Update");
    const two = { ...initialUpdateCheckState, status: "behind", report: report(2) } as const;
    expect(getUpdateButtonTooltip(two)).toBe("2 runners behind · Update");
    expect(behindCount(two)).toBe(2);
    expect(resolveUpdateButtonAction(two)).toBe("show");

    expect(updateIconStatus(two)).toBe("available");

    const failed = { ...initialUpdateCheckState, status: "error", message: "boom" } as const;
    expect(getUpdateButtonTooltip(failed)).toBe("Couldn't check for updates · Retry");

    expect(resolveUpdateButtonAction(failed)).toBe("check");
  });

  it("names the two builds on a behind row, and says nothing it does not know", () => {
    expect(runnerUpdateLabel("v1.2.3", "v1.2.4")).toBe("Update available: v1.2.3 → v1.2.4");
    expect(runnerUpdateLabel("", "v1.2.4")).toBe("Update available: v1.2.4");
    expect(RUNNER_UPGRADE_COMMAND).toBe("detent update");
  });

  it("only marks a host behind when both builds are real versions", () => {
    for (const [reported, current, want] of [
      ["v1.2.3", "v1.2.4", true],
      ["v1.2.4", "v1.2.4", false],
      ["", "v1.2.4", false],
      ["v1.2.3", "", false],
      ["dev", "v1.2.4", false],
      ["v1.2.3", "dev", false],
    ] as const) {
      expect(hostIsBehind(reported, current), `${reported} vs ${current}`).toBe(want);
    }
  });
});

describe("the check against the hub", () => {
  it("settles up to date, and lets the sentence expire", async () => {
    vi.useFakeTimers();
    await checkForUpdates(hubReporting(0));
    expect(readUpdateCheckState().status).toBe("idle");
    expect(readUpdateCheckState().upToDate).toBe(true);
    act(() => {
      vi.advanceTimersByTime(UPDATE_UP_TO_DATE_MS + 1);
    });
    expect(readUpdateCheckState().upToDate).toBe(false);
  });

  it("reports the runners the hub says are behind", async () => {
    await checkForUpdates(hubReporting(2));
    const state = readUpdateCheckState();
    expect(state.status).toBe("behind");
    expect(behindCount(state)).toBe(2);
    expect(state.report?.current).toBe("v1.2.4");
  });

  it("clears itself once the fleet catches up", async () => {
    await checkForUpdates(hubReporting(1));
    expect(readUpdateCheckState().status).toBe("behind");
    await checkForUpdates(hubReporting(0));
    expect(readUpdateCheckState().status).toBe("idle");
  });

  it("treats a hub without the endpoint as nothing to report", async () => {
    await checkForUpdates(hubRefusing(404));
    const state = readUpdateCheckState();
    expect(state.status).toBe("idle");
    expect(state.upToDate).toBe(false);
  });

  it("fails the check rather than the page when the hub refuses", async () => {
    await checkForUpdates(hubRefusing(503));
    expect(readUpdateCheckState().status).toBe("error");
    expect(readUpdateCheckState().message).toContain("503");
  });

  it("shares one request between concurrent callers", async () => {
    const fetchImpl = hubReporting(0);
    await Promise.all([checkForUpdates(fetchImpl), checkForUpdates(fetchImpl)]);
    expect(fetchImpl).toHaveBeenCalledTimes(1);
  });
});

/** The footer's update control, whatever it currently says. */
function updateControl(): HTMLElement {
  return screen.getByRole("button", {
    name: /Check for updates|All runners up to date|Couldn't check for updates|behind . Update|Checking for updates/,
  });
}

describe("the pill", () => {
  it("offers the check on the control itself", async () => {
    vi.stubGlobal("fetch", hubReporting(0));
    await renderSidebar();
    await waitFor(() => expect(readUpdateCheckState().status).toBe("idle"));
    // The label is the offer, or the transient answer to the check the mount
    // itself just ran.
    expect(
      screen.getByRole("button", { name: /Check for updates|All runners up to date/ }),
    ).toBeTruthy();
  });

  it("answers a press that found nothing with the toast", async () => {
    vi.stubGlobal("fetch", hubReporting(0));
    await renderSidebar();
    await waitFor(() => expect(readUpdateCheckState().status).toBe("idle"));
    fireEvent.click(updateControl());
    await waitFor(() => expect(screen.getByText("All runners up to date")).toBeTruthy());
  });

  it("shows the behind state without a press", async () => {
    vi.stubGlobal("fetch", hubReporting(2));
    await renderSidebar();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "2 runners behind · Update" })).toBeTruthy(),
    );
  });

  it("sends a press on the behind state to Providers & runners", async () => {
    vi.stubGlobal("fetch", hubReporting(1));
    const { onNavigate } = await renderSidebar();
    const button = await screen.findByRole("button", { name: "1 runner behind · Update" });
    fireEvent.click(button);
    expect(onNavigate).toHaveBeenCalledWith(RUNNER_SETTINGS_PATH);
  });

  it("raises the error toast when the check fails", async () => {
    vi.stubGlobal("fetch", hubRefusing(503));
    await renderSidebar();
    await waitFor(() => expect(readUpdateCheckState().status).toBe("error"));
    fireEvent.click(updateControl());
    await waitFor(() => expect(screen.getByText("Could not check for updates")).toBeTruthy());
  });

  it("checks again on the cadence while it is mounted", async () => {
    vi.useFakeTimers();
    const fetchImpl = hubReporting(0);
    vi.stubGlobal("fetch", fetchImpl);
    await renderSidebar();
    await act(async () => {
      await Promise.resolve();
    });
    expect(fetchImpl).toHaveBeenCalledTimes(1);
    await act(async () => {
      vi.advanceTimersByTime(UPDATE_POLL_INTERVAL_MS);
      await Promise.resolve();
    });
    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it("checks when the tab comes back", async () => {
    const fetchImpl = hubReporting(0);
    vi.stubGlobal("fetch", fetchImpl);
    await renderSidebar();
    await waitFor(() => expect(fetchImpl).toHaveBeenCalledTimes(1));
    await act(async () => {
      globalThis.dispatchEvent(new Event("focus"));
      await Promise.resolve();
    });
    await waitFor(() => expect(fetchImpl).toHaveBeenCalledTimes(2));
  });
});
