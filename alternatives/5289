import { describe, expect, it } from "vitest";

import {
  editorOpenUrl,
  isLocalRunner,
  openInDisabledReason,
  OPEN_IN_EDITORS,
  OPEN_IN_SENTENCES,
  remoteSentence,
  type OpenInLocation,
} from "../src/app/adapters/openIn.ts";

describe("isLocalRunner", () => {
  const table: ReadonlyArray<{
    name: string;
    page: string;
    runner: string | null;
    expected: boolean;
  }> = [
    { name: "the same bare name", page: "mac-studio", runner: "mac-studio", expected: true },
    {
      name: "an mDNS page host against a bare runner name",
      page: "mac-studio.local",
      runner: "mac-studio",
      expected: true,
    },
    {
      name: "a trailing dot, which is a legal absolute name",
      page: "mac-studio.local.",
      runner: "mac-studio",
      expected: true,
    },
    { name: "case", page: "MAC-Studio.local", runner: "mac-studio", expected: true },
    {
      name: "two different machines that share a domain",
      page: "mac-mini.local",
      runner: "mac-studio.local",
      expected: false,
    },
    // The conservative case, and the reason it is conservative is in the
    // function's own comment: a loopback page host proves nothing about where
    // the worktree is, and a path handed over wrongly opens the wrong tree.
    { name: "a loopback page host", page: "localhost", runner: "localhost", expected: false },
    { name: "a loopback literal", page: "127.0.0.1", runner: "mac-studio", expected: false },
    { name: "an IPv6 loopback", page: "[::1]", runner: "mac-studio", expected: false },
    { name: "an unknown runner hostname", page: "mac-studio", runner: null, expected: false },
    { name: "an empty runner hostname", page: "mac-studio", runner: "   ", expected: false },
    { name: "an empty page host", page: "", runner: "mac-studio", expected: false },
    {
      name: "a hosted page against any runner",
      page: "cloud.detent.dev",
      runner: "mac-studio",
      expected: false,
    },
  ];

  for (const row of table) {
    it(`answers ${row.name}`, () => {
      expect(isLocalRunner(row.page, row.runner)).toBe(row.expected);
    });
  }
});

describe("editorOpenUrl", () => {
  it("builds the scheme each editor registers", () => {
    expect(editorOpenUrl("cursor", "/Users/mv/code/widgets")).toBe(
      "cursor://file/Users/mv/code/widgets",
    );
    expect(editorOpenUrl("vscode", "/Users/mv/code/widgets")).toBe(
      "vscode://file/Users/mv/code/widgets",
    );
  });

  it("has none for the file manager, which is why its row is disabled", () => {
    expect(editorOpenUrl("file-manager", "/Users/mv/code/widgets")).toBeNull();
  });

  it("encodes a path with spaces per segment, leaving the separators alone", () => {
    expect(editorOpenUrl("cursor", "/Users/mv/My Code/acme widgets")).toBe(
      "cursor://file/Users/mv/My%20Code/acme%20widgets",
    );
  });

  it("encodes a non-ASCII path", () => {
    expect(editorOpenUrl("vscode", "/Users/mv/kód/größe")).toBe(
      "vscode://file/Users/mv/k%C3%B3d/gr%C3%B6%C3%9Fe",
    );
  });

  it("has no URL for an empty path", () => {
    expect(editorOpenUrl("cursor", "")).toBeNull();
    expect(editorOpenUrl("cursor", "   ")).toBeNull();
  });

  it("offers exactly the three rows a URL scheme can reach", () => {
    expect(OPEN_IN_EDITORS).toEqual(["cursor", "vscode", "file-manager"]);
  });
});

function location(overrides: Partial<OpenInLocation> = {}): OpenInLocation {
  return {
    worktreePath: "/Users/mv/code/widgets",
    hostname: "mac-studio",
    local: true,
    linked: true,
    ...overrides,
  };
}

describe("openInDisabledReason", () => {
  const table: ReadonlyArray<{
    name: string;
    editor: "cursor" | "vscode" | "file-manager";
    location: OpenInLocation;
    expected: string | null;
  }> = [
    { name: "a local Cursor", editor: "cursor", location: location(), expected: null },
    { name: "a local VS Code", editor: "vscode", location: location(), expected: null },
    {
      name: "the file manager, even locally",
      editor: "file-manager",
      location: location(),
      expected: OPEN_IN_SENTENCES.fileManager,
    },
    {
      name: "a remote runner, by name",
      editor: "cursor",
      location: location({ local: false }),
      expected: "This worktree is on mac-studio.",
    },
    {
      name: "a remote runner that reported no name",
      editor: "vscode",
      location: location({ local: false, hostname: null }),
      expected: "This worktree is on the runner's machine.",
    },
    {
      name: "no linked issue, in the git group's own words",
      editor: "cursor",
      location: location({ linked: false, worktreePath: null, hostname: null, local: false }),
      expected: OPEN_IN_SENTENCES.noIssue,
    },
    {
      name: "a workspace that has not reported a path yet",
      editor: "cursor",
      location: location({ worktreePath: null }),
      expected: OPEN_IN_SENTENCES.noWorktreePath,
    },
  ];

  for (const row of table) {
    it(`answers ${row.name}`, () => {
      expect(openInDisabledReason(row.editor, row.location)).toBe(row.expected);
    });
  }

  it("uses the same sentence the git group uses for the same fact", () => {
    expect(OPEN_IN_SENTENCES.noIssue).toBe("Link an issue to get a worktree.");
  });

  it("names the machine, or says which machine it is when it has no name", () => {
    expect(remoteSentence("mac-studio.local")).toBe("This worktree is on mac-studio.local.");
    expect(remoteSentence("")).toBe("This worktree is on the runner's machine.");
  });
});
