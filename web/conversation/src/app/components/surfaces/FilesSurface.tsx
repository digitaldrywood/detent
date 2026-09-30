// The right panel's Files surface (decisions.md §18.4).
//
// Upstream: `components/files/FilePreviewPanel.tsx`, the read-only half of it.
// The layout is theirs — breadcrumbs and toggles in a 40px sub-header, the
// code pane, and the explorer as an aside on the right that takes the whole
// width while nothing is open — and so are the parts: Pierre's `File` in a
// `Virtualizer` for the code, with the same options the diff view passes;
// `FileBreadcrumbs.tsx` above it; `FileBrowserPanel.tsx` (Pierre's tree)
// beside it. Editing, media players, "open in" and the browser preview are
// theirs too and are left out: §18.4 puts writes out of scope for this slice
// and the runner serves files, not a dev server.
//
// What §18.4 adds to their surface is the workspace in front of it (§18.1):
// what a reader sees while it comes up, what they are told when it does not,
// a denied entry that is never read, and a read the runner refused.
import { File, Virtualizer } from "@pierre/diffs/react";
import { FolderTree, WrapText } from "lucide-react";
import React from "react";

import { DiffPanelLoadingState, DiffPanelShell } from "../../../components/DiffPanelShell.tsx";
import { DiffWorkerPoolProvider } from "../../../components/DiffWorkerPoolProvider.tsx";
import { PierreEntryIcon } from "../../../components/chat/PierreEntryIcon.tsx";
import { RenderErrorBoundary } from "../../../components/RenderErrorBoundary.tsx";
import { Button } from "../../../components/ui/button.tsx";
import { ScrollArea } from "../../../components/ui/scroll-area.tsx";
import { Toggle } from "../../../components/ui/toggle.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../../components/ui/tooltip.tsx";
import { cn } from "../../../lib/utils.ts";
import { DIFF_SURFACE_THEME_UNSAFE_CSS, resolveDiffThemeName } from "../../../lib/diffRendering.ts";
import { PREFERRED_HIGHLIGHTER } from "../../../lib/syntaxHighlighting.ts";
import {
  isWorkspaceImagePreviewPath,
  mediaKindFromPath,
} from "../../../runtime/support/filePreview.ts";
import type { WorkspaceReason, WorkspaceState } from "../../../contracts/work.ts";
import { getClientSettings } from "../../adapters/settings.ts";
import {
  decodeContent,
  RelayError,
  type ContentPayload,
  type FileEntry,
  type ListOptions,
  type ListedPayload,
} from "../../adapters/workspaceRelay.ts";
import {
  describeWorkspaceStatus,
  WORKSPACE_CONNECTING_STATUS,
  type WorkspaceSessionFacts,
} from "../../lib/workspaceStatus.ts";
import { FileBreadcrumbs } from "./FileBreadcrumbs.tsx";
import { FileBrowserPanel } from "./FileBrowserPanel.tsx";
import { WorkspaceStatusView } from "./WorkspaceStatusView.tsx";

/** Only the two calls this surface makes, so a test can stand in for them. */
export interface FilesClient {
  list(path: string, options?: ListOptions): Promise<ListedPayload>;
  read(path: string, options?: { offset?: number; length?: number }): Promise<ContentPayload>;
}

export interface FilesSurfaceProps {
  /** Null before the hub has answered at all. */
  readonly state: WorkspaceState | null;
  readonly reason: WorkspaceReason | null;
  /** A request that failed outright, rather than a workspace that failed. */
  readonly error: string | null;
  readonly loading: boolean;
  /** What the session is, for the sentences that name a runner or a time. */
  readonly session?: WorkspaceSessionFacts | null;
  /** Null until the workspace is live; the surface shows its state until then. */
  readonly files: FilesClient | null;
  readonly onRetry: () => void;
  /** Opens one file as its own panel tab, which is what the `file` surface is. */
  readonly onOpenFile?: ((path: string) => void) | undefined;
  /** The project crumb's label. */
  readonly projectName?: string | undefined;
  readonly theme?: "light" | "dark";
}

const DEFAULT_PROJECT_NAME = "Workspace";

// --- The empty and loading states -------------------------------------------

function EmptyState({
  children,
  testId,
}: {
  children: React.ReactNode;
  testId: string;
}): React.ReactElement {
  return (
    <div
      className="flex items-center justify-center px-3 py-6 text-center text-muted-foreground/70 text-xs"
      data-testid={testId}
    >
      <p>{children}</p>
    </div>
  );
}

// --- The file view ----------------------------------------------------------

const FILE_SURFACE_UNSAFE_CSS = `
  ${DIFF_SURFACE_THEME_UNSAFE_CSS}

  diffs-container {
    --diffs-bg: var(--code-background, var(--background)) !important;
    --diffs-light-bg: var(--code-background, var(--background)) !important;
    --diffs-dark-bg: var(--code-background, var(--background)) !important;
    background-color: var(--code-background, var(--background)) !important;
    color: var(--code-foreground, var(--foreground)) !important;
  }
`;

function PlainFile({ code }: { code: string }): React.ReactElement {
  return (
    <pre
      className="min-w-0 overflow-auto p-3 font-mono text-[11px] leading-relaxed"
      data-testid="files-view-plain"
    >
      {code}
    </pre>
  );
}

function HighlightedFile({
  path,
  code,
  theme,
  wordWrap,
}: {
  path: string;
  code: string;
  theme: "light" | "dark";
  wordWrap: boolean;
}): React.ReactElement {
  return (
    <DiffWorkerPoolProvider>
      <Virtualizer
        key={`${path}:${theme}:${code.length}`}
        className="file-preview-virtualizer min-h-0 flex-1 overflow-auto"
        config={{
          overscrollSize: 600,
          intersectionObserverMargin: 1200,
        }}
      >
        <File
          file={{
            name: path,
            contents: code,
            cacheKey: `files:${path}:${code.length}`,
          }}
          options={{
            disableFileHeader: true,
            overflow: wordWrap ? "wrap" : "scroll",
            theme: resolveDiffThemeName(theme),
            preferredHighlighter: PREFERRED_HIGHLIGHTER,
            themeType: theme,
            unsafeCSS: FILE_SURFACE_UNSAFE_CSS,
          }}
          className="min-h-full"
        />
      </Virtualizer>
    </DiffWorkerPoolProvider>
  );
}

interface Opened {
  readonly path: string;
  readonly loading: boolean;
  readonly content: ContentPayload | null;
  readonly error: RelayError | null;
  /** A denylisted entry is never read at all (§18.4), only reported. */
  readonly denied: boolean;
}

function FileView({
  opened,
  theme,
  wordWrap,
}: {
  opened: Opened;
  theme: "light" | "dark";
  wordWrap: boolean;
}): React.ReactElement {
  if (opened.denied) {

    return (
      <EmptyState testId="files-view-denied">
        This path is on the workspace&rsquo;s denylist and is not readable.
      </EmptyState>
    );
  }
  if (opened.loading) return <DiffPanelLoadingState label={`Reading ${opened.path}`} />;
  if (opened.error !== null) {
    return (
      <EmptyState
        testId={opened.error.code === "too_large" ? "files-view-too-large" : "files-view-error"}
      >
        {opened.error.message}
      </EmptyState>
    );
  }
  const content = opened.content;
  if (content === null) {
    return <EmptyState testId="files-view-empty">Select a file to read it.</EmptyState>;
  }
  // An image is shown rather than decoded: §18.2 base64s binary payloads, and
  // `filePreview.ts` is the vendored classifier for which paths those are.
  if (isWorkspaceImagePreviewPath(opened.path) || mediaKindFromPath(opened.path) === "image") {
    const mime = content.mime.length > 0 ? content.mime : "application/octet-stream";
    const source =
      content.encoding === "base64"
        ? `data:${mime};base64,${content.data}`
        : `data:${mime};utf8,${encodeURIComponent(content.data)}`;
    return (
      <div className="flex min-h-0 flex-1 items-center justify-center p-3" data-testid="files-view-image">
        <img alt={opened.path} className="max-h-full max-w-full object-contain" src={source} />
      </div>
    );
  }
  const code = decodeContent(content);
  return (
    <div className="flex min-h-0 flex-1 flex-col" data-testid="files-view">
      {content.truncated ? (
        <div className="shrink-0 border-b border-warning/20 bg-warning-surface px-3 py-1.5 text-[11px] text-warning-foreground">
          Preview limited to the first {content.size.toLocaleString()} bytes of this file.
        </div>
      ) : null}
      <RenderErrorBoundary fallback={<PlainFile code={code} />} resetKeys={[opened.path]}>
        <HighlightedFile path={opened.path} code={code} theme={theme} wordWrap={wordWrap} />
      </RenderErrorBoundary>
    </div>
  );
}

// --- Reading a file ---------------------------------------------------------

/**
 * The open file and its read. `open` is the one way in: from the tree, a
 * breadcrumb, or the `file` surface's own path. A denied entry is recorded
 * without a read.
 */
function useOpenedFile(files: FilesClient | null): {
  readonly opened: Opened | null;
  readonly open: (path: string, denied: boolean) => void;
  readonly close: () => void;
} {
  const [opened, setOpened] = React.useState<Opened | null>(null);
  const requestRef = React.useRef(0);

  const open = React.useCallback(
    (path: string, denied: boolean) => {
      const request = (requestRef.current += 1);
      if (denied) {
        setOpened({ path, loading: false, content: null, error: null, denied: true });
        return;
      }
      if (files === null) {
        setOpened({ path, loading: true, content: null, error: null, denied: false });
        return;
      }
      setOpened({ path, loading: true, content: null, error: null, denied: false });
      void files
        .read(path)
        .then((content) => {
          if (request !== requestRef.current) return;
          setOpened({ path, loading: false, content, error: null, denied: false });
        })
        .catch((cause: unknown) => {
          if (request !== requestRef.current) return;
          setOpened({
            path,
            loading: false,
            content: null,
            error:
              cause instanceof RelayError
                ? cause
                : new RelayError("invalid_frame", cause instanceof Error ? cause.message : String(cause)),
            denied: false,
          });
        });
    },
    [files],
  );

  const close = React.useCallback(() => {
    requestRef.current += 1;
    setOpened(null);
  }, []);

  return { opened, open, close };
}

// --- The view ---------------------------------------------------------------

function WorkspaceFilesView({
  files,
  projectName,
  theme,
  opened,
  onOpen,
  onOpenFile,
  explorerToggle = true,
}: {
  readonly files: FilesClient;
  readonly projectName: string;
  readonly theme: "light" | "dark";
  readonly opened: Opened | null;
  readonly onOpen: (entry: FileEntry, path: string) => void;
  readonly onOpenFile?: ((path: string) => void) | undefined;
  /** Whether the explorer can be hidden; a surface that is only a tree cannot. */
  readonly explorerToggle?: boolean;
}): React.ReactElement {
  const [wordWrap, setWordWrap] = React.useState(() => getClientSettings().wordWrap);
  const [explorerOpen, setExplorerOpen] = React.useState(true);
  const relativePath = opened?.path ?? null;
  const showExplorer = relativePath === null || explorerOpen;

  const select = React.useCallback(
    (entry: FileEntry, path: string) => {
      onOpen(entry, path);
      if (!entry.denied) onOpenFile?.(path);
    },
    [onOpen, onOpenFile],
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background" data-testid="files-surface">
      {relativePath !== null ? (
        <div
          className="flex h-10 min-h-10 shrink-0 items-center gap-2 border-b border-border/60 bg-background px-3"
          data-surface-subheader
        >
          <ScrollArea hideScrollbars scrollFade className="min-w-0 flex-1 rounded-none" data-file-breadcrumbs>
            <div className="flex h-full w-max min-w-full items-center text-xs">
              <FileBreadcrumbs
                files={files}
                projectName={projectName}
                relativePath={relativePath}
                theme={theme}
                onOpenFile={select}
              />
            </div>
          </ScrollArea>
          <Tooltip>
            <TooltipTrigger
              render={
                <Toggle
                  className="shrink-0"
                  pressed={wordWrap}
                  onPressedChange={setWordWrap}
                  aria-label={wordWrap ? "Disable word wrap" : "Enable word wrap"}
                  variant="ghost"
                  size="sm"
                >
                  <WrapText className="size-3.5" />
                </Toggle>
              }
            />
            <TooltipPopup>{wordWrap ? "Disable word wrap" : "Enable word wrap"}</TooltipPopup>
          </Tooltip>
          {explorerToggle ? (
            <Tooltip>
              <TooltipTrigger
                render={
                  <Toggle
                    className="shrink-0"
                    pressed={explorerOpen}
                    onPressedChange={setExplorerOpen}
                    aria-label={explorerOpen ? "Hide file explorer" : "Show file explorer"}
                    variant="ghost"
                    size="sm"
                  >
                    <FolderTree className="size-3.5" />
                  </Toggle>
                }
              />
              <TooltipPopup>{explorerOpen ? "Hide file explorer" : "Show file explorer"}</TooltipPopup>
            </Tooltip>
          ) : null}
        </div>
      ) : null}
      <div className="flex min-h-0 flex-1 overflow-hidden">
        <div
          className={cn(
            "min-w-0 flex-1 flex-col overflow-hidden",
            relativePath !== null ? "flex" : "hidden",
          )}
        >
          {opened !== null ? <FileView opened={opened} theme={theme} wordWrap={wordWrap} /> : null}
        </div>
        {showExplorer ? (
          <aside
            className={cn(
              "flex min-h-0 shrink-0 bg-background",
              relativePath !== null
                ? "w-[min(22rem,46%)] min-w-64 border-l border-border/60"
                : "min-w-0 flex-1",
            )}
          >
            <FileBrowserPanel
              files={files}
              projectName={projectName}
              selectedPath={relativePath}
              onOpenFile={select}
              theme={theme}
            />
          </aside>
        ) : null}
      </div>
    </div>
  );
}

// --- The surface ------------------------------------------------------------

/** The shell around a workspace that is not serving files yet, or any more. */
function WorkspaceStateShell({
  state,
  loading,
  children,
}: {
  state: WorkspaceState | null;
  loading: boolean;
  children: React.ReactElement;
}): React.ReactElement {
  const header = (
    <div className="flex min-w-0 flex-1 items-center gap-3">
      <span
        className="inline-flex h-6 max-w-full items-center gap-1 rounded-md bg-accent px-2 font-medium text-accent-foreground text-xs"
        data-testid="files-workspace"
      >
        <span className="truncate">Workspace · {state ?? (loading ? "opening" : "none")}</span>
      </span>
    </div>
  );
  return (
    <DiffPanelShell mode="sheet" header={header}>
      <div className="flex min-h-0 flex-1 flex-col" data-testid="files-surface">
        {children}
      </div>
    </DiffPanelShell>
  );
}

export function FilesSurface({
  state,
  reason,
  error,
  loading,
  session = null,
  files,
  onRetry,
  onOpenFile,
  projectName = DEFAULT_PROJECT_NAME,
  theme = "dark",
}: FilesSurfaceProps): React.ReactElement {
  const { opened, open } = useOpenedFile(files);
  const onOpen = React.useCallback(
    (entry: FileEntry, path: string) => open(path, entry.denied),
    [open],
  );

  if (error !== null) {
    return (
      <WorkspaceStateShell state={state} loading={loading}>
        <div className="min-h-0 flex-1 overflow-auto">
          <EmptyState testId="files-error">{error}</EmptyState>
          <div className="flex justify-center pb-4">
            <Button size="xs" variant="outline" onClick={onRetry}>
              Try again
            </Button>
          </div>
        </div>
      </WorkspaceStateShell>
    );
  }
  // Every state that is not serving files is words (§18.1); the skeleton is
  // kept for `starting` alone, which `describeWorkspaceStatus` decides.
  const status = describeWorkspaceStatus({
    state,
    reason,
    capability: "files",
    session,
    connected: files !== null,
  });
  if (status !== null || files === null) {
    return (
      <WorkspaceStateShell state={state} loading={loading}>
        <WorkspaceStatusView
          status={status ?? WORKSPACE_CONNECTING_STATUS}
          testIdPrefix="files"
          onRetry={onRetry}
        />
      </WorkspaceStateShell>
    );
  }
  return (
    <WorkspaceFilesView
      files={files}
      projectName={projectName}
      theme={theme}
      opened={opened}
      onOpen={onOpen}
      onOpenFile={onOpenFile}
    />
  );
}

export function FileSurface({
  path,
  files,
  onOpenFile,
  projectName = DEFAULT_PROJECT_NAME,
  theme = "dark",
}: {
  readonly path: string;
  readonly files: FilesClient | null;
  readonly onOpenFile?: ((path: string) => void) | undefined;
  readonly projectName?: string | undefined;
  readonly theme?: "light" | "dark";
}): React.ReactElement {
  const { opened, open } = useOpenedFile(files);
  React.useEffect(() => {
    open(path, false);
  }, [open, path]);

  const onOpen = React.useCallback(
    (entry: FileEntry, entryPath: string) => {
      // The tab is this path's; another file opens its own tab, and this
      // one's selection stays where it is.
      if (entryPath === path) open(entryPath, entry.denied);
    },
    [open, path],
  );
  const openOther = React.useCallback(
    (entryPath: string) => {
      if (entryPath !== path) onOpenFile?.(entryPath);
    },
    [onOpenFile, path],
  );

  if (files === null) {
    const header = (
      <div className="flex min-w-0 flex-1 items-center gap-2">
        <PierreEntryIcon
          pathValue={path}
          kind="file"
          theme={theme}
          className="size-3.5 text-muted-foreground/70"
        />
        <span className="min-w-0 truncate font-mono text-[11px]">{path}</span>
      </div>
    );
    return (
      <DiffPanelShell mode="sheet" header={header}>
        <div className="flex min-h-0 flex-1 flex-col" data-testid="file-surface">
          <EmptyState testId="file-surface-unavailable">
            This file needs an open workspace. Open the Files tab first.
          </EmptyState>
        </div>
      </DiffPanelShell>
    );
  }
  return (
    <div className="flex min-h-0 flex-1 flex-col" data-testid="file-surface">
      <WorkspaceFilesView
        files={files}
        projectName={projectName}
        theme={theme}
        opened={opened}
        onOpen={onOpen}
        onOpenFile={openOther}
      />
    </div>
  );
}
