import { describe, expect, it } from "vitest";
import * as Schema from "effect/Schema";
import fixture from "../src/contracts/fixtures/reports.json";
import { ReportsReport } from "../src/contracts/reports.ts";
import { makeAccountApi } from "../src/app/account/api.ts";

import { reworkByCause } from "../src/app/reports/rework.ts";

describe("Reports read", () => {
  it("decodes recorded metrics and keeps unavailable coverage nullable", () => {
    const report = Schema.decodeUnknownSync(ReportsReport)(fixture);
    expect(report.completion.system.p50_seconds).toBe(7200);
    expect(report.coverage.at(-1)?.observed).toBeNull();
    expect(report.analytics.failure_signatures.items[0]?.work_items).toEqual([
      "wi_old",
    ]);
    expect(report.stages[1]?.disabled).toBe(true);
  });
  it("groups recorded rework causes and preserves unknown reasons", () => {
    expect(
      reworkByCause([
        { from_state: "Merging", reason_detail: "merge conflict", count: 2 },
        {
          from_state: "Merging",
          reason_detail: "conflict on landing",
          count: 3,
        },
        {
          from_state: "Human Review",
          reason_detail: "changes requested",
          count: 1,
        },
        {
          from_state: "In Progress",
          reason_detail: "unknown source failure",
          count: 1,
        },
      ]).map(({ label, count }) => ({ label, count })),
    ).toEqual([
      { label: "Conflict on landing", count: 5 },
      { label: "Review transition", count: 1 },
      { label: "unknown source failure", count: 1 },
    ]);
  });
  it("requests the selected project and window through the account API", async () => {
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
    await api.reports("project / one", "48h");
    expect(requested).toContain(
      "reports?project=project%20%2F%20one&range=48h",
    );
  });
});
