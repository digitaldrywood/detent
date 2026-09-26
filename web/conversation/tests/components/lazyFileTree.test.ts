// The arithmetic between a `listed` frame and Pierre's tree (decisions.md
// §18.4): which paths a listing contributes, which directories an expansion
// obliges the feed to list next, and which chains draw flattened.
import { describe, expect, it } from "vitest";

import type { FileEntry } from "../../src/app/adapters/workspaceRelay.ts";
import {
  ancestorDirectoriesOf,
  directoriesNeedingListing,
  directoryPathOf,
  soleSubdirectoryOf,
  treePathFor,
  treePathsForListing,
} from "../../src/app/components/surfaces/lazyFileTree.ts";
import {
  fileBreadcrumbChildren,
  fileBreadcrumbParent,
  fileBreadcrumbs,
} from "../../src/app/components/surfaces/filePath.ts";

function entry(name: string, kind: FileEntry["kind"] = "file"): FileEntry {
  return { name, kind, size: 0, modified_at: "2026-09-12T00:00:00Z", ignored: false, denied: false };
}

describe("tree paths", () => {
  it("marks a directory with a trailing slash, as the treePath does", () => {
    expect(treePathFor("", entry("src", "dir"))).toBe("src/");
    expect(treePathFor("", entry("README.md"))).toBe("README.md");
    expect(treePathFor("apps/api", entry("main.go"))).toBe("apps/api/main.go");
    expect(treePathFor("apps", entry("api", "dir"))).toBe("apps/api/");
  });

  it("lists a symlink as a file, which reading answers with forbidden", () => {
    expect(treePathFor("", entry("link", "symlink"))).toBe("link");
  });

  it("strips the slash back off for the runner", () => {
    expect(directoryPathOf("apps/api/")).toBe("apps/api");
    expect(directoryPathOf("README.md")).toBe("README.md");
  });

  it("contributes one path per entry in listing order", () => {
    expect(treePathsForListing("apps", [entry("api", "dir"), entry("go.mod")])).toEqual([
      "apps/api/",
      "apps/go.mod",
    ]);
  });
});

describe("what to list next", () => {
  it("names expanded directories the feed has not listed, in tree order", () => {
    expect(directoriesNeedingListing(["apps/", "apps/api/", "docs/"], new Set(["apps"]))).toEqual([
      "apps/api",
      "docs",
    ]);
  });

  it("names nothing when everything expanded is listed", () => {
    expect(directoriesNeedingListing(["apps/"], new Set(["", "apps"]))).toEqual([]);
  });

  it("names a lone subdirectory so a flattened chain settles in one paint", () => {
    expect(soleSubdirectoryOf(".agents", [entry("skills", "dir")])).toBe(".agents/skills");
    expect(soleSubdirectoryOf(".agents", [entry("skills", "dir"), entry("README.md")])).toBeNull();
    expect(soleSubdirectoryOf(".agents", [entry("only.md")])).toBeNull();
    expect(soleSubdirectoryOf(".agents", [])).toBeNull();
  });

  it("walks a file's ancestors shallowest first", () => {
    expect(ancestorDirectoriesOf("apps/api/cmd/main.go")).toEqual(["apps", "apps/api", "apps/api/cmd"]);
    expect(ancestorDirectoriesOf("README.md")).toEqual([]);
  });
});

describe("breadcrumbs", () => {
  it("start at the project and end at the file", () => {
    expect(fileBreadcrumbs("parable", ".claude/hooks/post-tool-use-lint.sh")).toEqual([
      { label: "parable", path: "", kind: "project" },
      { label: ".claude", path: ".claude", kind: "directory" },
      { label: "hooks", path: ".claude/hooks", kind: "directory" },
      { label: "post-tool-use-lint.sh", path: ".claude/hooks/post-tool-use-lint.sh", kind: "file" },
    ]);
  });

  it("list a directory's entries directories first, then by name with numbers in order", () => {
    const children = fileBreadcrumbChildren(
      [entry("z.md"), entry("file10.md"), entry("file2.md"), entry("src", "dir"), entry("Aa.md")],
      "docs",
    );
    expect(children.map((child) => child.path)).toEqual([
      "docs/src",
      "docs/Aa.md",
      "docs/file2.md",
      "docs/file10.md",
      "docs/z.md",
    ]);
    expect(children[0]?.kind).toBe("directory");
  });

  it("carry the denied flag so a menu never opens a denylisted entry as readable", () => {
    const [env] = fileBreadcrumbChildren([{ ...entry(".env"), denied: true }], "");
    expect(env?.denied).toBe(true);
    expect(env?.path).toBe(".env");
  });

  it("know the parent of a directory, and that the root has none", () => {
    expect(fileBreadcrumbParent("apps/api")).toBe("apps");
    expect(fileBreadcrumbParent("apps")).toBe("");
    expect(fileBreadcrumbParent("")).toBeNull();
  });
});
