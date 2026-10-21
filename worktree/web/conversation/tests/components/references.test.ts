// Cross-references in message text (decisions.md §13.7, §14). The hub resolves
// them; the client's only job is to link what was resolved and to leave alone
// what was not.
import { describe, expect, it } from "vitest";

import { linkReferences, referenceSegments } from "../../src/app/lib/references.ts";

const issue = { kind: "issue", id: "wi_1", label: "#123", url: "/work/i/wi_1" } as const;
const crossProject = {
  kind: "issue",
  id: "wi_9",
  label: "acme/web#7",
  url: "/work/i/wi_9",
} as const;

describe("linkReferences", () => {
  it("leaves the text alone when the message carries no references", () => {
    expect(linkReferences("See #123 for the lease.", undefined)).toBe("See #123 for the lease.");
    expect(linkReferences("See #123 for the lease.", [])).toBe("See #123 for the lease.");
  });

  it("links a reference the hub resolved", () => {
    expect(linkReferences("See #123 for the lease.", [issue])).toBe(
      "See [#123](/work/i/wi_1) for the lease.",
    );
  });

  it("links the longest label first so a cross-project one stays whole", () => {
    expect(linkReferences("acme/web#7 supersedes #123.", [issue, crossProject])).toBe(
      "[acme/web#7](/work/i/wi_9) supersedes [#123](/work/i/wi_1).",
    );
  });

  it("never rewrites inside a fenced block or an inline code span", () => {
    const source = "Run `git log #123`\n\n```\ngrep #123 .\n```\n\nThen see #123.";
    expect(linkReferences(source, [issue])).toBe(
      "Run `git log #123`\n\n```\ngrep #123 .\n```\n\nThen see [#123](/work/i/wi_1).",
    );
  });

  it("does not link a longer identifier that merely contains the label", () => {
    expect(linkReferences("#1234 is not #123.", [issue])).toBe(
      "#1234 is not [#123](/work/i/wi_1).",
    );
  });
});

describe("referenceSegments", () => {
  it("returns one literal run when nothing was resolved", () => {
    expect(referenceSegments("See #123.", undefined)).toEqual([
      { kind: "text", text: "See #123." },
    ]);
  });

  it("splits the text around each resolved reference", () => {
    expect(referenceSegments("See #123 now", [issue])).toEqual([
      { kind: "text", text: "See " },
      { kind: "link", text: "#123", url: "/work/i/wi_1" },
      { kind: "text", text: " now" },
    ]);
  });
});
