// The board's formatters. Every one takes `now`, so none of them needs a fake
// clock and none of them can disagree with a sibling rendered in the same tick.
import { describe, expect, it } from "vitest";

import {
  ageLabel,
  dayLabel,
  elapsedLabel,
  issueNumber,
  moneyLabel,
  projectHue,
  tokenLabel,
} from "../../src/app/work/lib/format.ts";

const NOW = Date.parse("2026-09-09T12:00:00Z");
const ago = (ms: number) => new Date(NOW - ms).toISOString();

describe("ageLabel", () => {
  it("coarsens as the age grows", () => {
    expect(ageLabel(ago(12_000), NOW)).toBe("12s");
    expect(ageLabel(ago(3 * 60_000), NOW)).toBe("3m");
    expect(ageLabel(ago(4 * 3_600_000), NOW)).toBe("4h");
    expect(ageLabel(ago(2 * 86_400_000), NOW)).toBe("2d");
    expect(ageLabel(ago(42 * 86_400_000), NOW)).toBe("6w");
  });

  it("is empty rather than wrong for a missing or unparsable stamp", () => {
    expect(ageLabel(null, NOW)).toBe("");
    expect(ageLabel(undefined, NOW)).toBe("");
    expect(ageLabel("not a date", NOW)).toBe("");
  });

  it("never renders a negative age from a clock that is behind", () => {
    expect(ageLabel(new Date(NOW + 60_000).toISOString(), NOW)).toBe("0s");
  });
});

describe("elapsedLabel", () => {
  it("keeps seconds under an hour and drops them above it", () => {
    expect(elapsedLabel(ago(45_000), NOW)).toBe("45s");
    expect(elapsedLabel(ago(72_000), NOW)).toBe("1m 12s");
    expect(elapsedLabel(ago(2 * 3_600_000 + 4 * 60_000), NOW)).toBe("2h 04m");
  });

  it("is empty for an attempt that never started", () => {
    expect(elapsedLabel(null, NOW)).toBe("");
  });
});

describe("the remaining formatters", () => {
  it("abbreviates tokens and leaves an absent count blank", () => {
    expect(tokenLabel(1_200_000)).toBe("1.2M tok");
    expect(tokenLabel(312_000)).toBe("312k tok");
    expect(tokenLabel(84)).toBe("84 tok");
    expect(tokenLabel(0)).toBe("");
    expect(tokenLabel(null)).toBe("");
  });

  it("renders money only when there is a figure", () => {
    expect(moneyLabel(28.77)).toBe("$28.77");
    expect(moneyLabel(0)).toBe("$0.00");
    expect(moneyLabel(null)).toBe("");
  });

  it("renders a day for the opened pill", () => {
    expect(dayLabel("2026-09-07T18:04:00Z")).not.toBe("");
    expect(dayLabel(null)).toBe("");
  });

  it("gives a project the same hue every time", () => {
    expect(projectHue("proj_alpha")).toBe(projectHue("proj_alpha"));
    expect(projectHue("proj_alpha")).not.toBe(projectHue("proj_beta"));
    expect(projectHue("proj_alpha")).toBeGreaterThanOrEqual(0);
    expect(projectHue("proj_alpha")).toBeLessThan(360);
  });

  it("takes the number off the end of an identifier", () => {
    expect(issueNumber("parable#3363")).toBe("#3363");
    expect(issueNumber("Browser collaboration#2")).toBe("#2");
    expect(issueNumber("anything", 41)).toBe("#41");
    expect(issueNumber("no-number-here")).toBe("no-number-here");
    expect(issueNumber(null)).toBe("");
  });
});
