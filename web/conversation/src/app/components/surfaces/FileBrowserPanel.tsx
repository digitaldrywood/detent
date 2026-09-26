import { FileTree, useFileTree, useFileTreeSearch, useFileTreeSelector } from "@pierre/trees/react";
import type {
  FileTree as FileTreeModel,
  FileTreeDirectoryHandle,
  FileTreeItemHandle,
} from "@pierre/trees";
import { ChevronsDownUpIcon, ChevronsUpDownIcon } from "lucide-react";
import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import { Input } from "../../../components/ui/input.tsx";
import { RefreshIcon } from "../../../components/ui/refresh-icon.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../../components/ui/tooltip.tsx";
import { CONVERSATION_FILE_ICONS } from "../../../pierre-icons.ts";
import { PIERRE_TREE_UNSAFE_CSS, pierreTreeStyle } from "../../../pierre-tree-theme.ts";
import type { FileEntry } from "../../adapters/workspaceRelay.ts";
import {
  ancestorDirectoriesOf,
  directoriesNeedingListing,
  directoryPathOf,
  isDirectoryTreePath,
  soleSubdirectoryOf,
  treePathsForListing,
} from "./lazyFileTree.ts";

/** Only the call the tree makes, so a test can stand in for it. */
export interface TreeFilesClient {
  list(path: string): Promise<{ readonly entries: readonly FileEntry[] }>;
}

export interface FileBrowserPanelProps {
  readonly files: TreeFilesClient;
  readonly projectName: string;
  /** File open in the code pane; revealed and selected in the tree. */
  readonly selectedPath: string | null;
  readonly onOpenFile: (entry: FileEntry, path: string) => void;
  readonly theme: "light" | "dark";
  /** Handed the tree's model once, for a test to drive it. */
  readonly onModel?: ((model: FileTreeModel) => void) | undefined;
}

/**
 * Rows Pierre draws in jsdom, where every box measures zero and the
 * virtualizer would otherwise draw none. Harmless in a browser: the real
 * viewport takes over on the first measure.
 */
const INITIAL_VISIBLE_ROW_COUNT = 40;

/** The handle's union narrows on `isDirectory()`, which TypeScript cannot see. */
function directoryHandle(item: FileTreeItemHandle | null): FileTreeDirectoryHandle | null {
  return item !== null && item.isDirectory() ? (item as FileTreeDirectoryHandle) : null;
}

function RefreshFilesButton(props: { isPending: boolean; onRefresh: () => void }) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            aria-label="Refresh workspace files"
            onClick={props.onRefresh}
          />
        }
      >
        <RefreshIcon refreshing={props.isPending} className="size-3.5" />
      </TooltipTrigger>
      <TooltipPopup>{props.isPending ? "Refreshing…" : "Refresh files"}</TooltipPopup>
    </Tooltip>
  );
}

function FileSearchField(props: {
  ariaLabel: string;
  onClose: () => void;
  onValueChange: (value: string) => void;
  value: string;
}) {
  return (
    <Input
      type="search"
      name="workspace-files-search"
      size="compact"
      unstyled
      className="h-7 min-w-0 flex-1 bg-transparent"
      value={props.value}
      aria-label={props.ariaLabel}
      placeholder="Search files"
      spellCheck={false}
      onChange={(event) => props.onValueChange(event.target.value)}
      onKeyDown={(event) => {
        if (event.key !== "Escape") return;
        props.onClose();
        event.currentTarget.blur();
      }}
    />
  );
}

/**
 * What the feed knows: which directories have been listed, and every entry by
 * its tree path so a selection can be answered with the entry itself (the
 * tree carries paths, and §18.4's `denied` flag lives on the entry).
 */
interface Feed {
  readonly listed: ReadonlySet<string>;
  readonly pending: ReadonlySet<string>;
  readonly entries: ReadonlyMap<string, FileEntry>;
  readonly errors: ReadonlyMap<string, string>;
}

/** One answered `list` frame: the directory, its rows, and their entries. */
interface Listing {
  readonly directory: string;
  readonly paths: readonly string[];
  readonly entries: readonly FileEntry[];
}

const EMPTY_FEED: Feed = {
  listed: new Set(),
  pending: new Set(),
  entries: new Map(),
  errors: new Map(),
};

export function FileBrowserPanel({
  files,
  projectName,
  selectedPath,
  onOpenFile,
  theme,
  onModel,
}: FileBrowserPanelProps): React.ReactElement {
  const feedRef = React.useRef<Feed>(EMPTY_FEED);
  const [feed, setFeedState] = React.useState<Feed>(EMPTY_FEED);
  const setFeed = React.useCallback((update: (current: Feed) => Feed) => {
    feedRef.current = update(feedRef.current);
    setFeedState(feedRef.current);
  }, []);
  const [generation, setGeneration] = React.useState(0);
  const syncingSelectionRef = React.useRef(false);
  const treeSelectionPathRef = React.useRef<string | null>(null);
  const onOpenFileRef = React.useRef(onOpenFile);
  React.useEffect(() => {
    onOpenFileRef.current = onOpenFile;
  });

  const { model } = useFileTree({
    density: "compact",
    fileTreeSearchMode: "hide-non-matches",
    flattenEmptyDirectories: true,
    icons: CONVERSATION_FILE_ICONS,
    initialVisibleRowCount: INITIAL_VISIBLE_ROW_COUNT,
    onSelectionChange: (selectedPaths) => {
      // Selection changes driven by the reveal sync below are echoes of an
      // already-open file, not a request to open it again.
      if (syncingSelectionRef.current) return;
      const treePath = selectedPaths.at(-1);
      if (treePath === undefined || isDirectoryTreePath(treePath)) return;
      const entry = feedRef.current.entries.get(treePath);
      if (entry === undefined) return;
      treeSelectionPathRef.current = treePath;
      onOpenFileRef.current(entry, treePath);
    },
    paths: [],
    search: false,
    unsafeCSS: PIERRE_TREE_UNSAFE_CSS,
  });
  React.useEffect(() => {
    onModel?.(model);
  }, [model, onModel]);

  /**
   * One `list` frame and the feed bookkeeping it settles, without touching the
   * tree. What the tree is told differs between the root, which is one
   * `resetPaths`, and every other directory, which is a `batch` of adds under
   * a row that already exists — so the model operation is the caller's.
   */
  const fetchListing = React.useCallback(
    (directory: string): Promise<Listing | null> => {
      const current = feedRef.current;
      if (current.listed.has(directory) || current.pending.has(directory)) {
        return Promise.resolve(null);
      }
      setFeed((state) => ({ ...state, pending: new Set(state.pending).add(directory) }));
      return files
        .list(directory)
        .then((listed) => {
          const paths = treePathsForListing(directory, listed.entries);
          setFeed((state) => {
            const entries = new Map(state.entries);
            listed.entries.forEach((entry, index) => {
              const path = paths[index];
              if (path !== undefined) entries.set(path, entry);
            });
            const pending = new Set(state.pending);
            pending.delete(directory);
            const errors = new Map(state.errors);
            errors.delete(directory);
            return {
              listed: new Set(state.listed).add(directory),
              pending,
              entries,
              errors,
            };
          });
          return { directory, paths, entries: listed.entries };
        })
        .catch((cause: unknown) => {
          setFeed((state) => {
            const pending = new Set(state.pending);
            pending.delete(directory);
            const errors = new Map(state.errors);
            errors.set(directory, cause instanceof Error ? cause.message : String(cause));
            return { ...state, pending, errors };
          });
          return null;
        });
    },
    [files, setFeed],
  );

  // A directory the reader opened: its rows are added under the row they
  // belong to, which is below whatever the reader is looking at.
  const listDirectory = React.useCallback(
    async (directory: string): Promise<void> => {
      const listing = await fetchListing(directory);
      if (listing === null) return;
      if (listing.paths.length > 0) {
        model.batch(listing.paths.map((path) => ({ type: "add" as const, path })));
      }
      // A lone subdirectory draws flattened onto its parent once its own
      // listing is known; fetch it now so the row settles in one paint.
      const sole = soleSubdirectoryOf(directory, listing.entries);
      if (sole !== null) await listDirectory(sole);
    },
    [fetchListing, model],
  );

  const listRoot = React.useCallback(async (): Promise<void> => {
    const root = await fetchListing("");
    if (root === null) return;
    // Alphabetical, which is the order the tree draws siblings in, so the
    // listings still fill top to bottom for a reader watching them arrive.
    const directories = root.paths.filter(isDirectoryTreePath).sort((a, b) => a.localeCompare(b));
    const children = await Promise.all(
      directories.map((treePath) => fetchListing(directoryPathOf(treePath))),
    );
    model.resetPaths(
      [...root.paths, ...children.flatMap((child) => child?.paths ?? [])],
      { initialExpandedPaths: directories },
    );
    // A lone subdirectory is a level deeper than the paint above, so it is
    // added the ordinary way once the rows it hangs under exist.
    for (const child of children) {
      if (child === null) continue;
      const sole = soleSubdirectoryOf(child.directory, child.entries);
      if (sole !== null) void listDirectory(sole);
    }
  }, [fetchListing, listDirectory, model]);

  // A refresh, and the arrival of a client at all, drop every listing.
  React.useEffect(() => {
    feedRef.current = EMPTY_FEED;
    setFeedState(EMPTY_FEED);
    model.resetPaths([]);
    void listRoot();
  }, [listRoot, model, generation]);

  // Expansion is what triggers a listing: any expanded directory the feed
  // has not listed is fetched, once. The selector reads the tree's visible
  // rows, which is where the tree keeps expansion state.
  const expandedKey = useFileTreeSelector(model, (current) => {
    const rows = current.getVisibleRows(0, current.getVisibleCount());
    return rows
      .filter((row) => row.kind === "directory" && row.isExpanded)
      .map((row) => row.path)
      .join("\n");
  });
  React.useEffect(() => {
    const expanded = expandedKey === "" ? [] : expandedKey.split("\n");
    for (const directory of directoriesNeedingListing(expanded, feed.listed)) {
      if (!feed.pending.has(directory)) void listDirectory(directory);
    }
  }, [expandedKey, feed.listed, feed.pending, listDirectory]);

  // An open from outside the tree — a breadcrumb, a chat chip — is revealed:
  // every ancestor is listed and expanded, then the row is selected and
  // scrolled to. A selection that originated inside the tree is already
  // visible, and re-revealing it would close the search and clobber the
  // reader's context.
  React.useEffect(() => {
    if (selectedPath === null) return;
    if (treeSelectionPathRef.current === selectedPath) {
      treeSelectionPathRef.current = null;
      return;
    }
    let cancelled = false;
    void (async () => {
      for (const ancestor of ancestorDirectoriesOf(selectedPath)) {
        if (cancelled) return;
        if (!feedRef.current.listed.has(ancestor)) await listDirectory(ancestor);
        directoryHandle(model.getItem(`${ancestor}/`))?.expand();
      }
      if (cancelled) return;
      const item = model.getItem(selectedPath);
      if (item === null) return;
      syncingSelectionRef.current = true;
      model.closeSearch();
      for (const path of model.getSelectedPaths()) model.getItem(path)?.deselect();
      item.select();
      model.scrollToPath(selectedPath, { focus: false, offset: "center" });
      queueMicrotask(() => {
        syncingSelectionRef.current = false;
      });
    })();
    return () => {
      cancelled = true;
    };
  }, [listDirectory, model, selectedPath]);

  const search = useFileTreeSearch(model);
  const handleSearchValueChange = (value: string) => {
    if (value.trim().length === 0) {
      search.close();
      return;
    }
    if (!search.isOpen) {
      search.open(value);
      return;
    }
    search.setValue(value);
  };

  const listedDirectoryTreePaths = React.useMemo(
    () => [...feed.entries.keys()].filter(isDirectoryTreePath),
    [feed.entries],
  );
  const allDirectoriesExpanded = useFileTreeSelector(model, (current) =>
    listedDirectoryTreePaths.length > 0 &&
    listedDirectoryTreePaths.every((path) => directoryHandle(current.getItem(path))?.isExpanded() === true),
  );
  const toggleAllDirectories = () => {
    for (const path of listedDirectoryTreePaths) {
      const item = directoryHandle(model.getItem(path));
      if (item === null || item.isExpanded() === !allDirectoriesExpanded) continue;
      if (allDirectoriesExpanded) item.collapse();
      else item.expand();
    }
  };

  const rootError = feed.errors.get("") ?? null;
  const isPending = feed.pending.size > 0;

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-background" data-testid="files-tree">
      <div
        className="flex h-10 min-h-10 shrink-0 items-center gap-1 border-b border-border/60 bg-background px-2"
        data-surface-subheader
      >
        <RefreshFilesButton
          isPending={isPending}
          onRefresh={() => setGeneration((value) => value + 1)}
        />
        <FileSearchField
          ariaLabel={`Search ${projectName} files`}
          value={search.value}
          onValueChange={handleSearchValueChange}
          onClose={search.close}
        />
        {listedDirectoryTreePaths.length > 0 ? (
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  type="button"
                  size="icon-xs"
                  variant="ghost"
                  aria-label={
                    allDirectoriesExpanded ? "Collapse all folders" : "Expand all folders"
                  }
                  onClick={toggleAllDirectories}
                />
              }
            >
              {allDirectoriesExpanded ? (
                <ChevronsDownUpIcon className="size-3.5" />
              ) : (
                <ChevronsUpDownIcon className="size-3.5" />
              )}
            </TooltipTrigger>
            <TooltipPopup>
              {allDirectoriesExpanded ? "Collapse all folders" : "Expand all folders"}
            </TooltipPopup>
          </Tooltip>
        ) : null}
      </div>
      {rootError !== null && feed.listed.size === 0 ? (
        <div className="p-4 font-mono text-[11px] leading-relaxed text-error-foreground" data-testid="files-tree-error">
          {rootError}
        </div>
      ) : (
        <FileTree
          model={model}
          aria-label={`${projectName} files`}
          className="min-h-0 flex-1 overflow-hidden"
          style={pierreTreeStyle(theme)}
        />
      )}
    </div>
  );
}

/** Whether an entry the tree selected is a directory row, for callers. */
export function isDirectorySelection(treePath: string): boolean {
  return isDirectoryTreePath(treePath);
}

export { directoryPathOf };
