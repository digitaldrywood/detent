// The Change Request diff is drawn by @pierre/diffs, which lays each hunk's
// gutter and code columns out with inline `style="grid-row: span N"`
// attributes. The Hub serves the client under a Content-Security-Policy; if
// that policy drops style attributes, the columns collapse onto one grid row
// and the reviewer sees only the "N unmodified lines" bar (#3209). These
// cases render real patches the way the diff surface does and check that the
// served policy allows every inline style the rendered markup needs.
import { readFileSync } from "node:fs";
import { preloadDiffHTML } from "@pierre/diffs/ssr";
import { describe, expect, it } from "vitest";

import partialAttemptDiff from "../src/contracts/fixtures/work-attempt-diff-partial.json";
import { getRenderablePatch } from "../src/lib/diffRendering.ts";

function servedPolicy(): Map<string, string[]> {
  const source = readFileSync(
    new URL("../../../internal/cloudentry/service.go", import.meta.url),
    "utf8",
  );
  const match = /contentSecurity\s*=\s*"([^"]+)"/.exec(source);
  if (match === null)
    throw new Error(
      "contentSecurity not found in internal/cloudentry/service.go",
    );
  const directives = new Map<string, string[]>();
  for (const directive of match[1]!.split(";")) {
    const [name, ...values] = directive.trim().split(/\s+/);
    if (name) directives.set(name, values);
  }
  return directives;
}

function allowsStyleAttributes(policy: Map<string, string[]>): boolean {
  const sources =
    policy.get("style-src-attr") ??
    policy.get("style-src") ??
    policy.get("default-src") ??
    [];
  return sources.includes("'unsafe-inline'");
}

async function renderPatch(patch: string): Promise<string> {
  const renderable = getRenderablePatch(patch, "diff-content-security");
  if (renderable?.kind !== "files") throw new Error("patch did not parse");
  return preloadDiffHTML({
    fileDiff: renderable.files[0]!,
    options: { diffStyle: "unified", collapsed: false },
  });
}

const midFileAddition = [
  "diff --git a/internal/app/server.go b/internal/app/server.go",
  "index 1111111..2222222 100644",
  "--- a/internal/app/server.go",
  "+++ b/internal/app/server.go",
  "@@ -120,6 +120,7 @@ func routes() {",
  ' \te.GET("/health", health)',
  ' \te.GET("/ready", ready)',
  ' \te.GET("/version", version)',
  '+\te.GET("/status", status)',
  ' \te.GET("/metrics", metrics)',
  ' \te.GET("/debug", debug)',
  ' \te.GET("/docs", docs)',
  "",
].join("\n");

describe("the diff surface under the served Content-Security-Policy", () => {
  const cases = [
    {
      name: "the production payload: one added line deep in footer.templ",
      patch: partialAttemptDiff.files[0]!.patch,
      added: "https://hub.detent.build",
    },
    {
      name: "a one-line addition in the middle of a file",
      patch: midFileAddition,
      added: "/status",
    },
  ];

  for (const tc of cases) {
    it(`draws the changed line of ${tc.name} and lays it out with style attributes the policy allows`, async () => {
      const html = await renderPatch(tc.patch);
      expect(html).toContain(tc.added);
      expect(html).toMatch(/style="grid-row: span \d+"/);
      expect(allowsStyleAttributes(servedPolicy())).toBe(true);
    });
  }

  it("keeps scripts and style elements strict", () => {
    const policy = servedPolicy();
    expect(policy.get("script-src")).not.toContain("'unsafe-inline'");
    expect(policy.get("style-src")).toEqual(["'self'"]);
  });
});
