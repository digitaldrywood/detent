import { describe, expect, it, vi } from "vitest";

import { AccountError } from "../src/app/account/api.ts";
import {
  EMPTY_USAGE,
  fetchUsage,
  toMergedUsage,
  usagePath,
} from "../src/app/usage/adapter.ts";
import { decodeUsageReport } from "../src/contracts/usage.ts";
import empty from "../src/contracts/fixtures/usage-empty.json";
import hourly from "../src/contracts/fixtures/usage-24h.json";
import daily from "../src/contracts/fixtures/usage-30d.json";

const REPORT = decodeUsageReport(daily);
const HOURLY = decodeUsageReport(hourly);
const EMPTY = decodeUsageReport(empty);

function respond(status: number, body: unknown): typeof globalThis.fetch {
  return vi.fn(async () =>
    new Response(body === undefined ? "" : JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    }),
  ) as unknown as typeof globalThis.fetch;
}

describe("the usage query", () => {
  it("asks for the window the range control is showing", () => {
    expect(usagePath("/api/v2/organizations/org_1", "7d")).toBe(
      "/api/v2/organizations/org_1/usage?range=7d",
    );
  });

  it("narrows to one project only when there is one", () => {
    expect(usagePath("/api/v2/organizations/org_1", "30d", "proj_alpha")).toBe(
      "/api/v2/organizations/org_1/usage?range=30d&project=proj_alpha",
    );
    expect(usagePath("/api/v2/organizations/org_1", "30d", "")).toBe(
      "/api/v2/organizations/org_1/usage?range=30d",
    );
  });
});

describe("reshaping a report", () => {
  it("keeps the hero numbers the hub sent", () => {
    const merged = toMergedUsage(REPORT);
    expect(merged.costUsd).toBe(REPORT.total.cost);
    expect(merged.totalTokens).toBe(REPORT.total.tokens);
    expect(merged.sessions).toBe(REPORT.total.sessions);
    expect(merged.costQuality.cacheSavingsUsd).toBe(REPORT.totals.cache_savings);
  });

  it("gives every provider both shares, because the two tabs label different ones", () => {
    const merged = toMergedUsage(REPORT);
    expect(merged.providers).toHaveLength(REPORT.providers.length);
    const codex = merged.providers.find((provider) => provider.provider === "codex");
    expect(codex?.costShare).toBe(REPORT.providers[0]?.share);
    // The token share is the client's, because §17.5 only sends the cost one.
    expect(codex?.tokenShare).toBeCloseTo(
      (REPORT.providers[0]?.tokens ?? 0) / REPORT.total.tokens,
      10,
    );
    const shares = merged.providers.reduce((sum, provider) => sum + provider.tokenShare, 0);
    expect(shares).toBeCloseTo(1, 6);
  });

  it("splits a day's tokens across its providers by that day's own cost", () => {
    const merged = toMergedUsage(REPORT);
    const first = merged.daily[0];
    expect(first).toBeDefined();
    const source = REPORT.daily[0]!;
    expect(first!.costUsd).toBe(source.cost);
    const summed = [...first!.byProvider.values()].reduce(
      (sum, entry) => sum + entry.totalTokens,
      0,
    );
    expect(summed).toBeCloseTo(source.tokens, 4);
    expect(first!.byProvider.get("codex")?.costUsd).toBe(source.by_provider.codex);
  });

  it("reads a 24 hour window's buckets as hours, not as days", () => {
    const merged = toMergedUsage(HOURLY);
    expect(merged.daily).toHaveLength(0);
    expect(merged.hourly).toHaveLength(24);
    expect(merged.hourly[0]?.hourStart).toMatch(/T/);
  });

  it("marks an allowance the window has already spent as over limit", () => {
    const merged = toMergedUsage(REPORT);
    const mutations = merged.limits.find((limit) => limit.name === "api_mutations");
    expect(mutations?.overLimit).toBe(true);
    expect(merged.limits.find((limit) => limit.name === "members")?.overLimit).toBe(false);
    // Sorted, so the list does not reshuffle between two reads of one window.
    expect(merged.limits.map((limit) => limit.name)).toEqual(
      merged.limits.map((limit) => limit.name).toSorted(),
    );
  });

  it("carries the fleet's own rows for the Runners tab", () => {
    const merged = toMergedUsage(REPORT);
    expect(merged.runners.map((runner) => runner.id)).toEqual(
      REPORT.runners.map((runner) => runner.id),
    );
    expect(merged.runners[0]?.busySeconds).toBe(REPORT.runners[0]?.busy_seconds);
    expect(merged.runners[0]?.capacityUsed).toBe(REPORT.runners[0]?.capacity_used);
  });

  it("turns an organization that has run nothing into zeroes, not into gaps", () => {
    const merged = toMergedUsage(EMPTY);
    expect(merged.costUsd).toBe(0);
    expect(merged.providers).toEqual([]);
    expect(merged.daily).toEqual([]);
    expect(merged.models).toEqual([]);
    expect(merged.runners).toEqual([]);
    expect(merged.limits.length).toBeGreaterThan(0);
  });
});

describe("reading a window", () => {
  const options = { origin: "", apiBase: "/api/v2/organizations/org_1" };

  it("decodes the hub's report", async () => {
    const merged = await fetchUsage({ ...options, fetch: respond(200, daily) }, "30d");
    expect(merged.totalTokens).toBe(REPORT.total.tokens);
    expect(merged.currency).toBe("USD");
  });

  it("turns a refusal into the account error the page renders", async () => {
    await expect(
      fetchUsage(
        { ...options, fetch: respond(403, { code: "forbidden", message: "Not for you." }) },
        "30d",
      ),
    ).rejects.toMatchObject({ status: 403, message: "Not for you." });
  });

  it("says so when the hub does not report usage at all", async () => {
    const failure = await fetchUsage({ ...options, fetch: respond(404, undefined) }, "30d").catch(
      (cause: unknown) => cause,
    );
    expect(failure).toBeInstanceOf(AccountError);
    expect((failure as AccountError).message).toContain("does not report usage");
  });

  it("refuses a payload that is not the contract rather than drawing half of it", async () => {
    const failure = await fetchUsage(
      { ...options, fetch: respond(200, { total: { cost: 1 } }) },
      "30d",
    ).catch((cause: unknown) => cause);
    expect(failure).toBeInstanceOf(AccountError);
    expect((failure as AccountError).code).toBe("invalid");
  });

  it("reports a dead network as a network failure, not as a decode failure", async () => {
    const failing = vi.fn(async () => {
      throw new Error("offline");
    }) as unknown as typeof globalThis.fetch;
    const failure = await fetchUsage({ ...options, fetch: failing }, "7d").catch(
      (cause: unknown) => cause,
    );
    expect((failure as AccountError).code).toBe("network");
  });
});

describe("the empty value", () => {
  it("is what the page draws before the first report arrives", () => {
    expect(EMPTY_USAGE.providers).toEqual([]);
    expect(EMPTY_USAGE.costUsd).toBe(0);
    expect(EMPTY_USAGE.from).toBe("");
  });
});
