// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import type { FleetResponse } from "../../src/contracts/account.ts";
import emptyFixture from "../../src/contracts/fixtures/account-fleet-empty.json";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";

afterEach(() => { cleanup(); window.history.replaceState(null, "", "/"); });

const FLEET = fleetFixture as unknown as FleetResponse;
const EMPTY = emptyFixture as unknown as FleetResponse;
const NOW = Date.parse("2026-09-10T12:09:31Z");

function renderSection(fleet: FleetResponse = FLEET) {
  render(<RunnersSectionView fleet={fleet} now={NOW} />);
}

describe("runner rows", () => {
  it("shows sleeping Sprites without capacity or attention warnings", () => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[1]!, health: "asleep", claim_refusal_reason: "", problems: [], sprite: { name: "build-host", status: "warm", can_wake: true, wake_failed: false } }] });
    expect(screen.getByText("Asleep, wakes on new work")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByText(/slots can't take work/)).toBeNull();
    expect(screen.getByRole("link", { name: "Needs attention 0" })).toBeTruthy();
    expect(document.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(0);
  });
  it.each([
    ["v1.2.3", "Too old to take work, needs v1.2.4"],
    ["v1.2.4", ""],
    ["v1.3.0", ""],
    ["dev", ""],
  ])("renders the Hub admission refusal for %s", (version, reason) => {
    renderSection({ ...FLEET, current: "v1.2.4", minimum_runner_version: "v1.2.4", runners: [{ ...FLEET.runners[0]!, version, claim_refusal_reason: reason }] });
    const warning = screen.queryByTestId("host-update");
    if (reason !== "") {
      expect(warning?.textContent).toContain(reason);
      expect(screen.getAllByText(reason)).toHaveLength(1);
    } else {
      expect(warning).toBeNull();
    }
  });

  it("shows contextual problems and filters through the segmented control", () => {
    const problem = { code: "tier_unavailable", message: "Sandbox tooling is unavailable", fix_hint: "Repair the sandbox tooling", first_seen: "2026-09-10T12:00:00Z" };
    renderSection({ ...FLEET, runners: FLEET.runners.map((runner, index) => index === 0 ? { ...runner, health: "needs_attention", problems: [problem] } : runner) });
    expect(screen.getByText("Needs attention")).toBeTruthy();
    expect(screen.getByText(problem.message)).toBeTruthy();
    expect(screen.getByText(problem.fix_hint)).toBeTruthy();
    fireEvent.click(screen.getByRole("link", { name: "Needs attention 1" }));
    expect(window.location.search).toBe("?health=needs_attention");
    expect(screen.getAllByTestId("host-card")).toHaveLength(1);
    fireEvent.click(screen.getByRole("link", { name: "All 2" }));
    expect(window.location.search).toBe("");
    expect(screen.getAllByTestId("host-card")).toHaveLength(FLEET.runners.length);
  });

  it("draws one row per runner with leases and host capacity", () => {
    renderSection();
    const rows = screen.getAllByTestId("host-card");
    expect(rows).toHaveLength(FLEET.runners.length);
    expect(within(rows[0]!).getByRole("heading", { name: "Michael's MacBook Pro" })).toBeTruthy();
    expect(rows[0]!.textContent).toContain("1 of 2 slots in use");
    expect(rows[1]!.textContent).toContain("0 of 2 slots in use");
    expect(rows[1]!.textContent).toContain("stale");
    expect(screen.getByText("1 of 4 slots running work")).toBeTruthy();
    expect(screen.queryAllByRole("heading", { level: 1 })).toHaveLength(0);
  });
});

describe("providers", () => {
  it("folds every runner's capacity onto one table row per provider", () => {
    renderSection();
    const providers = screen.getByRole("region", { name: "Provider accounts" });
    expect(providers.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(within(providers).getByText("codex")).toBeTruthy();
    expect(within(providers).getByText("claude")).toBeTruthy();
    expect(within(providers).getByText(/gpt-6-astra/)).toBeTruthy();
  });

  it("invites enrollment and omits capacity when the fleet is empty", () => {
    renderSection(EMPTY);
    expect(screen.getByText("No provider capacity reported")).toBeTruthy();
    expect(screen.getByText("No runners yet. Enroll a machine to start taking work.")).toBeTruthy();
    expect(screen.queryAllByTestId("host-card")).toHaveLength(0);
    expect(screen.queryByRole("heading", { name: "Capacity right now" })).toBeNull();
  });
});

describe("runner details", () => {
  it.each(["Spilled over", "Waiting for home work (5m)"])("keeps %s out of the list and in Manage", (status) => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[0]!, home_project_ids: ["prj_home"], home_status: status }] });
    const row = screen.getByTestId("host-card");
    expect(within(row).queryByText("Home projects: prj_home")).toBeNull();
    fireEvent.click(within(row).getByRole("button", { name: "Manage Michael's MacBook Pro" }));
    const details = screen.getByRole("dialog");
    expect(within(details).getByText("Home projects: prj_home")).toBeTruthy();
    expect(within(details).getByText(status)).toBeTruthy();
  });

  it("shows outside hours as a plain runner status", () => {
    renderSection({ ...FLEET, runners: [{ ...FLEET.runners[0]!, health: "outside_hours", claim_refusal_reason: "" }] });
    const row = screen.getByTestId("host-card");
    expect(within(row).getByText("Outside hours")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});
