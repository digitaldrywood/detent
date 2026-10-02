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

describe("shared host capacity", () => {
  const active = { ...FLEET.runners[0]!, machine_id: "machine_shared", state: "active", health: "online", claim_refusal_reason: "", provider_capacity: [], host_capacity: 8, host_used: 1, capacity_limit: 8, reported_capacity: 8 };
  const lease = active.leases[0]!;

  it("counts a shared eight-slot host once and retains each identity's work", () => {
    renderSection({ ...FLEET, runners: [
      { ...active, capacity_limit: 4, reported_capacity: 4, host_used: 3 },
      { ...active, id: "runner_sibling", display_name: "Sibling", capacity_limit: 4, reported_capacity: 4, host_used: 3, leases: [
        { ...lease, lease_id: "lease_second", work_item_id: "wi_second" },
        { ...lease, lease_id: "lease_third", work_item_id: "wi_third" },
      ] },
    ] });
    expect(screen.getByText("3 of 8 slots running work")).toBeTruthy();
    const host = screen.getByTestId("runner-capacity");
    expect(host.querySelectorAll("[data-slot-state]")).toHaveLength(8);
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(3);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(5);
    expect(host.querySelector('[title="#wi_second connections: custom alerts from segment membership"]')).toBeTruthy();
    expect(screen.getAllByTestId("host-card")).toHaveLength(2);
  });

  it.each([
    ["draining", { state: "draining" }, 0],
    ["disabled", { state: "disabled" }, 0],
    ["offline", { health: "offline" }, 0],
    ["outside hours", { health: "outside_hours" }, 0],
    ["needs attention", { health: "needs_attention" }, 0],
    ["claim refused", { claim_refusal_reason: "Too old to take work" }, 0],
    ["reported limit", { reported_capacity: 3 }, 2],
    ["routing limit", { capacity_limit: 2 }, 1],
  ])("preserves running work and limits free slots for %s", (_name, values, free) => {
    renderSection({ ...FLEET, runners: [{ ...active, ...values }] });
    const host = screen.getByTestId("runner-capacity");
    expect(screen.getByText("1 of 8 slots running work")).toBeTruthy();
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(1);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(free);
    expect(host.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(7 - free);
  });

  it("uses only an active sibling's remaining allowance while another drains", () => {
    renderSection({ ...FLEET, runners: [
      { ...active, state: "draining", host_used: 2 },
      { ...active, id: "runner_sibling", host_used: 2, capacity_limit: 2, reported_capacity: 4, leases: [{ ...lease, lease_id: "lease_second" }] },
    ] });
    const host = screen.getByTestId("runner-capacity");
    expect(host.querySelectorAll('[data-slot-state="running"]')).toHaveLength(2);
    expect(host.querySelectorAll('[data-slot-state="free"]')).toHaveLength(1);
    expect(host.querySelectorAll('[data-slot-state="unavailable"]')).toHaveLength(5);
  });

  it("keeps distinct machine identities separate despite identical names", () => {
    renderSection({ ...FLEET, runners: [active, { ...active, id: "runner_other", machine_id: "machine_other" }] });
    expect(screen.getAllByTestId("runner-capacity")).toHaveLength(2);
    expect(screen.getByText("2 of 16 slots running work")).toBeTruthy();
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
