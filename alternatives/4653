import { describe, expect, it } from "vitest";
import * as Schema from "effect/Schema";
import fixture from "../src/contracts/fixtures/reports.json";
import { ReportsReport } from "../src/contracts/reports.ts";
import { makeAccountApi, reportsQuery } from "../src/app/account/api.ts";
import { plainSearchOptions } from "../src/app/lib/searchParams.ts";
import { chipSummary, filterOptions } from "../src/app/reports/filters.tsx";
import { costRows, stageStack } from "../src/app/reports/CostTab.tsx";
import { failureRows } from "../src/app/reports/FailuresTab.tsx";
import {
  countDelta,
  csvCell,
  durationDelta,
  parseReportsSearch,
  pointsDelta,
  reportBuckets,
  reportsQueryFilters,
  reportsSearchParams,
} from "../src/app/reports/model.ts";
import { reworkRows } from "../src/app/reports/QualityTab.tsx";
import { reworkByGroup, reworkGroupOf } from "../src/app/reports/rework.ts";
import { windowLabel } from "../src/app/reports/ReportsPage.tsx";

const decode = (value: unknown) => Schema.decodeUnknownSync(ReportsReport)(value);

describe("Reports read", () => {
  it("decodes the current read and the fields added for the tabs", () => {
    const report = decode(fixture);
    expect(report.series).toHaveLength(7);
    expect(report.rework_classes[0]?.kind).toBe("issue");
    expect(report.spend[0]?.shipped).toBe(true);
    const { series: _series, cancelled: _cancelled, ...older } = fixture;
    expect(decode(older).series).toBeUndefined();
  });

  it("keeps tab, range and view options in the query string, dropping defaults", () => {
    for (const [search, params] of [
      [{}, {}],
      [{ range: "30d", clock: "created" }, { range: "30d", clock: "created" }],
      [{ range: "48h", group: "nope", compare: "off" }, { compare: "off" }],
      [{ kind: "issue", issues: "not-shipped", group: "model" }, { kind: "issue", issues: "not-shipped", group: "model" }],
      [{ from: "2026-10-01", to: "2026-10-05", range: "30d" }, { from: "2026-10-01", to: "2026-10-05" }],
      [{ from: "2026-07-01", to: "2026-10-05", range: "30d" }, { range: "30d" }],
      [{ from: "2026-10-05", to: "2026-10-01" }, {}],
      [{ from: "2026-10-01" }, {}],
      [{ project: "all", labels: ["ui", "cloud", "ui"], priorities: ["high", "Nope"], filed_by: "p_1", runners: "r_1", models: "m", stages: "code" }, { project: "all", labels: ["cloud", "ui"], priorities: ["High"], filed_by: ["p_1"], runners: ["r_1"], models: ["m"], stages: ["code"] }],
      [{ labels: "a,b" }, { labels: ["a,b"] }],
      [{ project: "prj_1", types: "bug" }, { project: "prj_1", types: ["bug"] }],
      [{ project: "all", projects: ["prj_2", "prj_1", "prj_2"] }, { project: "all", projects: ["prj_1", "prj_2"] }],
    ] as const) {
      expect(reportsSearchParams(parseReportsSearch(search))).toEqual(params);
    }
  });

  it("states deltas with direction and whether the change is good", () => {
    expect(countDelta(276, 220, true, " vs prior 7d")).toEqual({ text: "56 vs prior 7d", direction: "up", tone: "good" });
    expect(countDelta(5, 7, false)).toEqual({ text: "2", direction: "down", tone: "good" });
    expect(countDelta(5, undefined, true)).toBeNull();
    expect(durationDelta(9 * 3600, 10 * 3600)).toEqual({ text: "1h 0m faster", direction: "down", tone: "good" });
    expect(durationDelta(9 * 3600, 0)).toBeNull();
    expect(pointsDelta(61.2, 65.3)).toEqual({ text: "4 pts", direction: "down", tone: "bad" });
    expect(pointsDelta(61.2, undefined)).toBeNull();
  });

  it("labels the window by calendar day, ending a midnight window on the day before", () => {
    const at = (from: string, to: string) => windowLabel(decode({ ...fixture, analytics: { ...fixture.analytics, window: { from, to } } }));
    expect(at("2026-10-07T00:00:00Z", "2026-10-13T10:00:00Z")).toBe("Oct 7 – Oct 13 · 6 days + today · hourly data through Oct 13, 9 AM UTC");
    expect(at("2026-10-06T00:00:00Z", "2026-10-13T00:00:00Z")).toBe("Oct 6 – Oct 12 · 7 days · hourly data through Oct 12, 11 PM UTC");
    expect(at("2026-10-12T10:00:00Z", "2026-10-13T10:00:00Z")).toBe("Oct 12 – Oct 13 · hourly data through Oct 13, 9 AM UTC");
  });

  it("writes RFC 4180 cells and neutralises formula prefixes", () => {
    expect(["plain", "a,b", 'say "hi"', "=SUM(A1)", "-1", "@x", 12].map(csvCell)).toEqual([
      "plain",
      '"a,b"',
      '"say ""hi"""',
      "'=SUM(A1)",
      "'-1",
      "'@x",
      "12",
    ]);
  });

  it("rolls hourly throughput up to UTC calendar days when the Hub sends no series", () => {
    const report = decode({
      ...fixture,
      series: undefined,
      analytics: { ...fixture.analytics, window: { from: "2026-10-06T10:00:00Z", to: "2026-10-13T10:00:00Z" } },
      throughput: [
        { from: "2026-10-06T11:00:00Z", to: "2026-10-06T12:00:00Z", done: 2, failed: 1, slots: null },
        { from: "2026-10-06T15:00:00Z", to: "2026-10-06T16:00:00Z", done: 3, failed: 0, slots: null },
        { from: "2026-10-08T05:00:00Z", to: "2026-10-08T06:00:00Z", done: 1, failed: 4, slots: null },
      ],
    });
    expect(reportBuckets(report).map(({ from, done, failed }) => ({ from, done, failed }))).toEqual([
      { from: "2026-10-06T10:00:00.000Z", done: 5, failed: 1 },
      { from: "2026-10-08T00:00:00.000Z", done: 1, failed: 4 },
    ]);
  });

  it("folds rework classes into chart groups, unnamed infrastructure classes into the blue group", () => {
    expect(reworkGroupOf("Landing refused: protected base", "infrastructure").id).toBe("refused");
    expect(reworkGroupOf("Instance lock held", "infrastructure").id).toBe("refused");
    expect(reworkGroupOf("Something new", "issue").id).toBe("other");
    expect(
      reworkByGroup(
        { "Gate failed before landing": 4, "Conflict on landing": 2, "Base moved after validation": 1, "Not classified": 3 },
        { "Not classified": "unknown" },
      ),
    ).toEqual({ review: 0, gate: 4, conflict: 2, refused: 1, other: 3 });
  });

  it("orders rework classes by count with Not classified last", () => {
    const rows = reworkRows(decode(fixture));
    expect(rows[0]?.class).toBe("Gate failed before landing");
    expect(rows.at(-1)?.class).toBe("Not classified");
  });

  it("stacks attributed stage spend, other spend and not-shipped spend so they sum to the total", () => {
    expect(stageStack({ stage_cost: { code: 60, rework: 20, plan: 10, validator: 10, other: 3 }, not_shipped_cost: 25 })).toEqual({
      groups: [60, 20, 0, 20],
      other: 3,
      notShipped: 25,
    });
    const report = decode(fixture);
    const models = costRows(report.stages, "model");
    expect(models.map((row) => [row.name, row.sessions])).toEqual([
      ["gpt-6.1-sol", 987],
      ["gpt-6-astra", 418],
    ]);
  });

  it("filters recurring failures by kind, infrastructure first", () => {
    const report = decode(fixture);
    expect(failureRows(report, "all")[0]?.kind).toBe("infrastructure");
    expect(failureRows(report, "issue").every((row) => row.kind === "issue")).toBe(true);
    expect(failureRows(report, "all").at(-1)?.kind).toBe("unknown");
  });

  it("requests the selected project and range through the account API", async () => {
    let requested = "";
    const api = makeAccountApi({
      origin: "https://reports.test",
      apiBase: "/organizations/org/api",
      csrfToken: "csrf",
      fetch: (async (url: string | URL | Request) => {
        requested = String(url);
        return new Response(JSON.stringify(fixture), {
          headers: { "Content-Type": "application/json" },
        });
      }) as typeof fetch,
    });
    await api.reports("project / one", "90d");
    expect(requested).toContain("reports?project=project%20%2F%20one&range=90d");
  });

  it("round-trips repeated filter parameters through the router, keeping commas inside a value", () => {
    const search = parseReportsSearch(plainSearchOptions.parseSearch("labels=a%2Cb&labels=cloud&priorities=High&range=30d"));
    expect(search.filters.labels).toEqual(["a,b", "cloud"]);
    expect(plainSearchOptions.stringifySearch(reportsSearchParams(search))).toBe("?range=30d&labels=a%2Cb&labels=cloud&priorities=High");
  });

  it("sends a custom range and each filter value as its own parameter", () => {
    const filters = reportsQueryFilters(
      parseReportsSearch({ labels: ["cloud", "a&b"], priorities: ["Urgent", "Low"], types: "bug", filed_by: "p 1", runners: "r", models: "m", stages: "code" }).filters,
    );
    expect(reportsQuery("all", { from: "2026-10-01", to: "2026-10-05" }, filters)).toBe(
      "project=all&from=2026-10-01&to=2026-10-05&labels=a%26b&labels=cloud&priorities=3&priorities=0&types=bug&filed_by=p%201&runners=r&models=m&stages=code",
    );
    expect(reportsQuery("prj", "7d", parseReportsSearch({}).filters)).toBe("project=prj&range=7d");
  });

  it("offers each chip's facets, keeps a selected value the facets dropped and names a set chip by its value", () => {
    const facets = { labels: ["cloud"], filed_by: [{ id: "p_1", name: "cory@example.com" }, { id: "p_2", name: "" }], runners: [], models: [], stages: ["code"] };
    expect(filterOptions("labels", facets, ["gone"])).toEqual([
      { value: "cloud", label: "cloud" },
      { value: "gone", label: "gone" },
    ]);
    expect(filterOptions("filedBy", facets, []).map((option) => option.label)).toEqual(["cory@example.com", "p_2"]);
    expect(filterOptions("stages", facets, []).map((option) => option.label)).toEqual(["Code"]);
    expect(filterOptions("priorities", undefined, []).map((option) => option.value)).toEqual(["Urgent", "High", "Normal", "Low"]);
    const people = filterOptions("filedBy", facets, ["p_1"]);
    expect(chipSummary("Filed by", people, [])).toBe("Filed by");
    expect(chipSummary("Filed by", people, ["p_1"])).toBe("Filed by: cory@example.com");
    expect(chipSummary("Label", people, ["a", "b"])).toBe("Label: 2 selected");
  });
});
