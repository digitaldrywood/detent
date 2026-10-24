// Breadcrumb arithmetic for the Files surface (decisions.md §18.4).
//
// Upstream: `components/files/filePath.ts`. `fileBreadcrumbs` and
// `fileBreadcrumbParent` are theirs with the host-path branch removed: a
// hosted worktree has no absolute paths, every path the runner serves is
// worktree-relative (§18.4's containment rule). `fileBreadcrumbChildren` takes
// one directory's listing rather than the whole project index, because that
// is what the runner answers with.
import type { FileEntry } from "../../adapters/workspaceRelay.ts";

export interface FileBreadcrumb {
  readonly label: string;
  readonly path: string;
  readonly kind: "project" | "directory" | "file";
}

export interface FileBreadcrumbChild {
  readonly label: string;
  readonly path: string;
  readonly kind: "directory" | "file";
  readonly denied: boolean;
}

/** Crumbs for a worktree-relative path start at the project. */
export function fileBreadcrumbs(projectName: string, relativePath: string): FileBreadcrumb[] {
  const parts = relativePath.split("/").filter(Boolean);
  return [
    { label: projectName, path: "", kind: "project" },
    ...parts.map((part, index) => ({
      label: part,
      path: parts.slice(0, index + 1).join("/"),
      kind: index === parts.length - 1 ? ("file" as const) : ("directory" as const),
    })),
  ];
}

export function fileBreadcrumbChildren(
  entries: readonly FileEntry[],
  directoryPath: string,
): FileBreadcrumbChild[] {
  let collator: Intl.Collator | undefined;
  const prefix = directoryPath ? `${directoryPath}/` : "";
  return entries
    .map((entry) => ({
      label: entry.name,
      path: `${prefix}${entry.name}`,
      kind: entry.kind === "dir" ? ("directory" as const) : ("file" as const),
      denied: entry.denied,
    }))
    .toSorted((left, right) => {
      if (left.kind !== right.kind) return left.kind === "directory" ? -1 : 1;
      collator ??= new Intl.Collator(undefined, {
        numeric: true,
        sensitivity: "base",
      });
      return collator.compare(left.label, right.label);
    });
}

export function fileBreadcrumbParent(directoryPath: string): string | null {
  if (!directoryPath) return null;
  const separatorIndex = directoryPath.lastIndexOf("/");
  return separatorIndex === -1 ? "" : directoryPath.slice(0, separatorIndex);
}
