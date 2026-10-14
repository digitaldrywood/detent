import { describe, expect, it } from "vitest";
import {
  decodeDiagnostics,
  decodeHealthFindings,
  findingsFromRead,
  findingDestination,
  orderedFindings,
} from "../src/contracts/diagnostics.ts";
import { readDiagnostics } from "../src/app/diagnostics/diagnosticsQuery.ts";
import emptyFleet from "../src/contracts/fixtures/account-fleet-empty.json";
import updatesFixture from "../src/contracts/fixtures/account-updates.json";
import fixture from "../src/contracts/fixtures/diagnostics.json";
import healthFixture from "../src/contracts/fixtures/health-findings.json";
import { makeAccountApi } from "../src/app/account/api.ts";

describe("diagnostics read", () => {
  it.each([200, 404, 503])("keeps Diagnostics available when the updates read returns %i", async (status) => {
    const requests: string[] = [];
    const api = makeAccountApi({
      origin: "", apiBase: "/api/v2/organizations/org_fixture", csrfToken: "",
      fetch: async (url) => {
        requests.push(url);
        if (url === "/app/updates") return Response.json(status === 200 ? updatesFixture : {}, { status });
        return Response.json(url.includes("/fleet") ? emptyFleet : fixture);
      },
    });
    const read = await readDiagnostics(api, "24h")();
    expect(requests).toContain("/app/updates");
    expect(read.appUpdates).toEqual(status === 200 ? updatesFixture : null);
    expect(read.report).toEqual(decodeDiagnostics(fixture));
    expect(read.runners).toEqual([]);
  });

  it.each([{}, { state: "resolved" as const, since: "2026-10-07T10:00:00Z" }])("reads granted projects, follows findings cursors, and deduplicates shared findings (%j)", async (options) => {
    const requests: string[] = [];
    const api = makeAccountApi({
      origin: "",
      apiBase: "/api/v2/organizations/org_fixture",
      csrfToken: "",
      fetch: async (url) => {
        requests.push(url);
        const query = new URL(url, "https://example.test");
        const project = query.pathname.split("/").at(-3)!;
        const issue = healthFixture.items[1]!;
        return Response.json({
          ...healthFixture,
          items: query.searchParams.has("cursor")
            ? [{ ...issue, id: `finding_${project}`, subject: { kind: "work_item", id: `wi_${project}` } }]
            : [healthFixture.items[0]],
          next_cursor: query.searchParams.has("cursor") ? undefined : "next/page",
        });
      },
    });
    const read = await api.healthFindings(["prj_first", "prj_second"], options);
    expect(read.items.map((finding) => finding.id)).toEqual(["finding_watch", "finding_prj_first", "finding_prj_second"]);
    expect(read.items.filter((finding) => finding.subject.kind === "work_item").map((finding) => finding.subject)).toEqual([
      { kind: "work_item", id: "wi_prj_first", project_id: "prj_first" },
      { kind: "work_item", id: "wi_prj_second", project_id: "prj_second" },
    ]);
    expect(findingsFromRead(read).filter((finding) => finding.subject.kind === "issue").map((finding) => finding.subject)).toEqual([
      { kind: "issue", work_item_id: "wi_prj_first", project_id: "prj_first" },
      { kind: "issue", work_item_id: "wi_prj_second", project_id: "prj_second" },
    ]);
    expect(findingsFromRead(read)[0]?.subject.project_id).toBe("prj_second");
    expect(read.last_tick_at).toBe(healthFixture.last_tick_at);
    const query = options.state === "resolved" ? "state=resolved&since=2026-10-07T10%3A00%3A00Z" : "state=open";
    expect(requests.toSorted()).toEqual([
      `/api/v2/organizations/org_fixture/projects/prj_first/health/findings?${query}`,
      `/api/v2/organizations/org_fixture/projects/prj_first/health/findings?${query}&cursor=next%2Fpage`,
      `/api/v2/organizations/org_fixture/projects/prj_second/health/findings?${query}`,
      `/api/v2/organizations/org_fixture/projects/prj_second/health/findings?${query}&cursor=next%2Fpage`,
    ]);
  });

  it.each([null, healthFixture.last_tick_at])("keeps detector tick %s distinct from an empty findings list", async (tick) => {
    const api = makeAccountApi({
      origin: "",
      apiBase: "/api/v2/organizations/org_fixture",
      csrfToken: "",
      fetch: async () => Response.json({ items: [], last_tick_at: tick }),
    });
    expect(await api.healthFindings(["prj_fixture"])).toEqual({ items: [], last_tick_at: tick });
  });

  it("orders attention before newer watch findings and links to each subject", () => {
    const findings = orderedFindings(
      findingsFromRead(decodeHealthFindings(healthFixture)),
    );
    expect(findings.map((finding) => finding.id)).toEqual([
      "finding_attention",
      "finding_watch",
      "finding_project",
    ]);
    expect(findings.map(findingDestination)).toEqual([
      "/work/i/wi_fixture?tab=diagnostics",
      "/fleet",
      "/work/p/prj_fixture",
    ]);
    expect(fixture.findings[0]!.id).toBe("finding_watch");
  });

  it("preserves unavailable reads and missing coverage instead of manufacturing zeroes", () => {
    const report = decodeDiagnostics({
      ...fixture,
      findings: null,
      detector_tick: null,
      skipped: null,
      capacity: null,
    });
    expect(report.findings).toBeNull();
    expect(report.detector_tick).toBeNull();
    expect(report.skipped).toBeNull();
    expect(report.coverage.at(-2)?.observed).toBeNull();
    expect(report.capacity).toBeNull();
  });

  it("rejects malformed detector rows and avoids inventing an incomplete subject link", () => {
    expect(() =>
      decodeDiagnostics({
        ...fixture,
        findings: [{ ...fixture.findings[0], severity: "urgent" }],
      }),
    ).toThrow();
    expect(
      findingDestination({
        ...decodeDiagnostics(fixture).findings![0]!,
        subject: { kind: "issue" },
      }),
    ).toBeNull();
  });
});
