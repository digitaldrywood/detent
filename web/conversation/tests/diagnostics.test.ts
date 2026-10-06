import { describe, expect, it } from "vitest";
import {
  decodeDiagnostics,
  decodeHealthFindings,
  findingsFromRead,
  findingDestination,
  orderedFindings,
} from "../src/contracts/diagnostics.ts";
import fixture from "../src/contracts/fixtures/diagnostics.json";
import healthFixture from "../src/contracts/fixtures/health-findings.json";

describe("diagnostics read", () => {
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
