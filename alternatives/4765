// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { formatBusy, UsageView } from "../../src/app/usage/UsagePage.tsx";
import { toMergedUsage, type MergedUsage } from "../../src/app/usage/adapter.ts";
import { decodeUsageReport } from "../../src/contracts/usage.ts";
import emptyFixture from "../../src/contracts/fixtures/usage-empty.json";
import hourlyFixture from "../../src/contracts/fixtures/usage-24h.json";
import dailyFixture from "../../src/contracts/fixtures/usage-30d.json";

afterEach(cleanup);

// Base UI dispatches a synthetic click through `PointerEvent`, which jsdom
// does not implement. The event's own behaviour is not what these tests are
// about, so the constructor is filled in with the one jsdom does have.
if (typeof globalThis.PointerEvent === "undefined") {
  globalThis.PointerEvent = globalThis.MouseEvent as unknown as typeof PointerEvent;
}

const DAILY = toMergedUsage(decodeUsageReport(dailyFixture));
const HOURLY = toMergedUsage(decodeUsageReport(hourlyFixture));
const EMPTY = toMergedUsage(decodeUsageReport(emptyFixture));

function renderUsage(
  overrides: Partial<React.ComponentProps<typeof UsageView>> = {},
): { onPreferencesChange: ReturnType<typeof vi.fn>; onRefresh: ReturnType<typeof vi.fn> } {
  const onPreferencesChange = vi.fn();
  const onRefresh = vi.fn(async () => undefined);
  render(
    <UsageView
      merged={DAILY}
      isPending={false}
      errorMessage={null}
      scope="Mock organization"
      preferences={{ metric: "cost", windowDays: 30 }}
      onPreferencesChange={onPreferencesChange}
      onRefresh={onRefresh}
      {...overrides}
    />,
  );
  return { onPreferencesChange, onRefresh };
}

/** The wide layout's control; the compact one repeats it below `xl`. */
function control(name: string): HTMLElement {
  return screen.getAllByRole("group", { name })[0]!;
}

describe("the usage header", () => {
  it("names the page once, at level one, and says whose usage it is", () => {
    renderUsage();
    const heading = screen.getAllByRole("heading", { level: 1 });
    expect(heading).toHaveLength(1);
    expect(heading[0]?.textContent).toBe("Usage");
    expect(screen.getByLabelText("Usage breadcrumb").textContent).toContain("Mock organization");
  });

  it("offers all four tabs, including the two Detent added", () => {
    renderUsage();
    const labels = within(control("Usage metric"))
      .getAllByRole("button")
      .map((button) => button.textContent);
    expect(labels).toEqual(["Cost", "Tokens", "Limits", "Runners"]);
  });

  it("offers the four windows and reports the one that was picked", () => {
    const { onPreferencesChange } = renderUsage();
    const period = control("Usage period");
    expect(within(period).getAllByRole("button").map((button) => button.textContent)).toEqual([
      "Past 24h",
      "7 days",
      "30 days",
      "90 days",
    ]);
    fireEvent.click(within(period).getByRole("button", { name: "7 days" }));
    expect(onPreferencesChange).toHaveBeenCalledWith({ metric: "cost", windowDays: 7 });
  });

  it("keeps the period control in place but disabled on the Limits tab", () => {
    renderUsage({ preferences: { metric: "limits", windowDays: 30 } });
    const period = control("Usage period");
    expect(within(period).getByRole("button", { name: "30 days" }).hasAttribute("disabled")).toBe(
      true,
    );
  });

  it("refreshes the window on demand", () => {
    const { onRefresh } = renderUsage();

    fireEvent.click(screen.getAllByRole("button", { name: "Refresh usage" })[0]!);
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });
});

describe("the cost tab", () => {
  it("draws the hero total as an API estimate over a session count", () => {
    renderUsage();
    expect(screen.getByText("$36.78")).toBeTruthy();
    expect(screen.getByText(/94 sessions · API estimate/)).toBeTruthy();
  });

  it("gives every provider with activity a row with its share and its tokens", () => {
    renderUsage();
    expect(screen.getByText("Codex")).toBeTruthy();
    expect(screen.getByText("Claude Code")).toBeTruthy();
    expect(screen.getByText(/39\.7% of cost/)).toBeTruthy();
  });

  it("labels the chart and the totals grid", () => {
    renderUsage();
    expect(screen.getByRole("heading", { name: "Daily cost" })).toBeTruthy();
    expect(screen.getByRole("img", { name: "Daily cost by provider" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Totals" })).toBeTruthy();
    expect(screen.getByText("Cache savings")).toBeTruthy();
  });
});

describe("the tokens tab", () => {
  it("switches the hero, the chart and the session line to tokens", () => {
    renderUsage({ preferences: { metric: "tokens", windowDays: 30 } });
    // The hero, and the same number again in the Totals grid.
    expect(screen.getAllByText("71.5M").length).toBeGreaterThan(0);
    expect(screen.getByText("94 sessions")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Daily processed tokens" })).toBeTruthy();
  });
});

describe("the breakdown", () => {
  it("lists one row per model, newest metric first", () => {
    renderUsage();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(DAILY.models.length);
    expect(rows[0]?.textContent).toContain("claude-opus-5");
  });

  it("swaps the table for one row per day, newest first", () => {
    renderUsage();
    fireEvent.click(within(control("Usage breakdown")).getByRole("button", { name: "Day" }));
    const header = screen.getAllByRole("row")[0]!;
    expect(within(header).getByText("Day")).toBeTruthy();
    expect(within(header).getByText("Total")).toBeTruthy();
    const first = screen.getAllByRole("row")[1]!;
    expect(first.textContent).toContain("Sep 10");
  });

  it("calls the time column Hour on a 24 hour window", () => {
    renderUsage({ preferences: { metric: "cost", windowDays: 1 }, merged: HOURLY });
    expect(
      within(control("Usage breakdown")).getByRole("button", { name: "Hour" }),
    ).toBeTruthy();
  });
});

describe("the limits tab", () => {
  it("draws the entitlement allowances with a meter each", () => {
    renderUsage({ preferences: { metric: "limits", windowDays: 30 } });
    expect(screen.getByRole("heading", { name: "Limits" })).toBeTruthy();
    const meters = screen.getAllByRole("progressbar");
    expect(meters).toHaveLength(DAILY.limits.length);
    expect(
      screen.getByRole("progressbar", { name: "API mutations used" }).getAttribute("aria-valuenow"),
    ).toBe("11840");
  });

  it("says over limit in words as well as in colour", () => {
    renderUsage({ preferences: { metric: "limits", windowDays: 30 } });
    expect(screen.getAllByText("over limit").length).toBeGreaterThan(0);
  });
});

describe("the runners tab", () => {
  it("gives one row per runner, with what it was busy doing and what it cost", () => {
    renderUsage({ preferences: { metric: "runners", windowDays: 30 } });
    expect(screen.getByRole("heading", { name: "Runners" })).toBeTruthy();
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(DAILY.runners.length);
    expect(rows[0]?.textContent).toContain("Mock MacBook Pro");
    expect(rows[0]?.textContent).toContain("of capacity");
  });

  it("reads seconds under lease as a duration", () => {
    expect(formatBusy(42)).toBe("42s");
    expect(formatBusy(600)).toBe("10m");
    expect(formatBusy(578_534)).toBe("161h");
  });
});

describe("an organization that has run nothing", () => {
  it("draws zeroes and says the window is empty rather than showing gaps", () => {
    renderUsage({ merged: EMPTY });
    expect(screen.getAllByText("$0.00").length).toBeGreaterThan(0);
    expect(screen.getByText(/0 sessions · API estimate/)).toBeTruthy();
    expect(screen.getByText("No activity in this window.")).toBeTruthy();
  });

  it("has no runner rows to draw either", () => {
    renderUsage({ merged: EMPTY, preferences: { metric: "runners", windowDays: 30 } });
    expect(screen.getByText("No activity in this window.")).toBeTruthy();
  });

  it("reports a refusal instead of a page of zeroes that were never measured", () => {
    renderUsage({ merged: EMPTY, errorMessage: "You do not have permission to read usage." });
    expect(screen.getByRole("status").textContent).toContain("You do not have permission");
    expect(screen.queryByRole("heading", { name: "Totals" })).toBeNull();
  });
});

describe("while the first report is in flight", () => {
  it("holds the loaded page's shape rather than collapsing the layout", () => {
    renderUsage({ merged: EMPTY as MergedUsage, isPending: true });
    expect(screen.getByRole("heading", { name: "Totals" })).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });
});
