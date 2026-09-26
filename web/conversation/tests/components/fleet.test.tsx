// @vitest-environment jsdom
//
// Settings → Providers & runners. What `/fleet` used to be, minus everything
// `/usage` now owns: the spend hero, the spend chart, the range control, the
// allowance totals grid and the spend-by-project breakdown moved there with
// the numbers they measure (decisions.md §17.5), and their cover moved to
// `tests/components/usage.test.tsx` and `tests/usage.test.ts`.
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { RunnersSectionView } from "../../src/app/fleet/RunnersSection.tsx";
import type { FleetResponse } from "../../src/contracts/account.ts";
import emptyFixture from "../../src/contracts/fixtures/account-fleet-empty.json";
import fleetFixture from "../../src/contracts/fixtures/account-fleet.json";

afterEach(cleanup);

const FLEET = fleetFixture as unknown as FleetResponse;
const EMPTY = emptyFixture as unknown as FleetResponse;
// A fixed clock: "last heartbeat" is relative, and a test that reads the wall
// clock would be a different test every minute.
const NOW = Date.parse("2026-09-10T12:09:31Z");

function renderSection(fleet: FleetResponse = FLEET) {
  render(<RunnersSectionView fleet={fleet} now={NOW} />);
}

describe("host cards", () => {
  it("draws one card per runner, with the host's own slot numbers", () => {
    renderSection();
    const cards = screen.getAllByTestId("host-card");
    expect(cards).toHaveLength(FLEET.runners.length);
    expect(within(cards[0]!).getByText("Michael's MacBook Pro")).toBeTruthy();
    expect(cards[0]!.textContent).toContain("1 / 2 in use");
    expect(cards[1]!.textContent).toContain("0 / 2 in use");
    // The runner's own limit is a different fact from the host's slots, and
    // the card says so when the runner reports something else.
    expect(cards[1]!.textContent).toContain("reports 0");
  });

  it("labels the slot meter and gives it the host's numbers", () => {
    renderSection();
    const first = screen.getByRole("progressbar", {
      name: "Slots in use on Michael's MacBook Pro",
    });
    expect(first.getAttribute("aria-valuenow")).toBe("1");
    expect(first.getAttribute("aria-valuemin")).toBe("0");
    expect(first.getAttribute("aria-valuemax")).toBe("2");
  });

  it("names a degraded host in words, not only in colour", () => {
    renderSection();
    const cards = screen.getAllByTestId("host-card");
    expect(cards[1]!.textContent).toContain("stale");
    expect(cards[1]!.textContent).toContain("paused");
  });

  it("adds no heading of its own, because the settings page owns the h1", () => {
    renderSection();
    expect(screen.queryAllByRole("heading", { level: 1 })).toHaveLength(0);
    expect(screen.getByRole("heading", { name: "Runners" })).toBeTruthy();
  });

  it("counts the runners and the leases they are holding", () => {
    renderSection();
    const leases = FLEET.runners.reduce((count, runner) => count + runner.leases.length, 0);
    expect(screen.getByText(new RegExp(`${FLEET.runners.length} runners · ${leases} active`))).toBeTruthy();
  });
});

describe("the providers section", () => {
  it("folds every runner's capacity onto one row per provider", () => {
    renderSection();
    const providers = document.querySelector("#settings-providers") as HTMLElement;
    expect(providers).toBeTruthy();
    const rows = providers.querySelectorAll('[data-slot="settings-row"]');
    expect(rows).toHaveLength(2);
    expect(within(providers).getByText("codex")).toBeTruthy();
    expect(within(providers).getByText("claude")).toBeTruthy();
    // Availability and models are facts about the account, not the host.
    expect(within(providers).getByText(/gpt-6-astra/)).toBeTruthy();
  });

  it("says so when no runner reports any provider capacity", () => {
    renderSection(EMPTY);
    expect(screen.getByText("No provider capacity reported")).toBeTruthy();
  });
});

describe("an organization with no runners", () => {
  it("says the fleet is empty rather than drawing an empty grid", () => {
    renderSection({ ...EMPTY, runners: [] });
    expect(screen.getByText(/No runners are enrolled/)).toBeTruthy();
    expect(screen.queryAllByTestId("host-card")).toHaveLength(0);
  });
});
