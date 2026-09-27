import type { FileEntry } from "../../adapters/workspaceRelay.ts";

export function treePathFor(parentPath: string, entry: Pick<FileEntry, "name" | "kind">): string {
  const path = parentPath === "" ? entry.name : `${parentPath}/${entry.name}`;
  return entry.kind === "dir" ? `${path}/` : path;
}

/** The directory a tree path names, without its trailing slash. */
export function directoryPathOf(treePath: string): string {
  return treePath.endsWith("/") ? treePath.slice(0, -1) : treePath;
}

export function isDirectoryTreePath(treePath: string): boolean {
  return treePath.endsWith("/");
}

/** The tree paths one listing contributes. */
export function treePathsForListing(parentPath: string, entries: readonly FileEntry[]): string[] {
  return entries.map((entry) => treePathFor(parentPath, entry));
}

/**
 * A directory whose listing is a single subdirectory draws flattened
 * (`.agents / skills`) once that subdirectory's listing is known, and as a
 * bare folder until then. Naming the chain lets the feed fetch it eagerly so
 * the row settles in one paint rather than two.
 */
export function soleSubdirectoryOf(parentPath: string, entries: readonly FileEntry[]): string | null {
  if (entries.length !== 1) return null;
  const [only] = entries;
  if (only === undefined || only.kind !== "dir") return null;
  return directoryPathOf(treePathFor(parentPath, only));
}

/** Every ancestor directory of a file path, shallowest first. */
export function ancestorDirectoriesOf(path: string): string[] {
  const segments = path.split("/").filter(Boolean);
  const ancestors: string[] = [];
  for (let index = 1; index < segments.length; index += 1) {
    ancestors.push(segments.slice(0, index).join("/"));
  }
  return ancestors;
}

/**
 * The directories the feed has to list next: expanded in the tree and not yet
 * listed. Order is the tree's own, so a reader who opened two folders sees
 * them fill top to bottom.
 */
export function directoriesNeedingListing(
  expandedTreePaths: readonly string[],
  listed: ReadonlySet<string>,
): string[] {
  const pending: string[] = [];
  for (const treePath of expandedTreePaths) {
    const directory = directoryPathOf(treePath);
    if (!listed.has(directory) && !pending.includes(directory)) pending.push(directory);
  }
  return pending;
}
