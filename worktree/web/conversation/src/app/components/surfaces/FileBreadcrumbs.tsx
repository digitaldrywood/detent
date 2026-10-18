import { ArrowLeftIcon, ChevronRightIcon } from "lucide-react";
import React from "react";

import { PierreEntryIcon } from "../../../components/chat/PierreEntryIcon.tsx";
import {
  Menu,
  MenuGroup,
  MenuItem,
  MenuPopup,
  MenuSeparator,
  MenuTrigger,
} from "../../../components/ui/menu.tsx";
import { RefreshIcon } from "../../../components/ui/refresh-icon.tsx";
import { Spinner } from "../../../components/ui/spinner.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../../components/ui/tooltip.tsx";
import { cn } from "../../../lib/utils.ts";
import type { FileEntry } from "../../adapters/workspaceRelay.ts";
import type { TreeFilesClient } from "./FileBrowserPanel.tsx";
import {
  type FileBreadcrumb,
  fileBreadcrumbChildren,
  fileBreadcrumbParent,
  fileBreadcrumbs,
} from "./filePath.ts";

export interface FileBreadcrumbsProps {
  readonly files: TreeFilesClient;
  readonly projectName: string;
  readonly relativePath: string;
  readonly theme: "light" | "dark";
  readonly onOpenFile: (entry: FileEntry, path: string) => void;
}

function pathLabel(path: string, projectName: string): string {
  return path.slice(path.lastIndexOf("/") + 1) || projectName;
}

function BreadcrumbLabel(props: {
  readonly current?: boolean;
  readonly label: string;
  readonly pathLabel: string;
}) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <span
            className={cn(
              "block max-w-40 truncate rounded-sm px-0.5",
              props.current ? "font-medium text-foreground" : "text-muted-foreground",
            )}
          />
        }
      >
        {props.label}
      </TooltipTrigger>
      <TooltipPopup side="top" className="max-w-80">
        {props.pathLabel}
      </TooltipPopup>
    </Tooltip>
  );
}

interface Listing {
  readonly entries: readonly FileEntry[] | null;
  readonly loading: boolean;
  readonly error: string | null;
}

/** One directory's listing for a menu, fetched when the menu asks for it. */
function useDirectoryListing(files: TreeFilesClient, directoryPath: string) {
  const [listing, setListing] = React.useState<Listing>({
    entries: null,
    loading: true,
    error: null,
  });
  const [nonce, setNonce] = React.useState(0);
  React.useEffect(() => {
    let cancelled = false;
    setListing({ entries: null, loading: true, error: null });
    void files
      .list(directoryPath)
      .then((listed) => {
        if (cancelled) return;
        setListing({ entries: listed.entries, loading: false, error: null });
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setListing({
          entries: null,
          loading: false,
          error: cause instanceof Error ? cause.message : String(cause),
        });
      });
    return () => {
      cancelled = true;
    };
  }, [directoryPath, files, nonce]);
  const refresh = React.useCallback(() => setNonce((value) => value + 1), []);
  return { ...listing, refresh };
}

function BreadcrumbMenuContent(props: {
  readonly files: TreeFilesClient;
  readonly currentFilePath: string;
  readonly directoryPath: string;
  readonly onDirectoryChange: (path: string) => void;
  readonly onOpenChange: (open: boolean) => void;
  readonly onOpenFile: (entry: FileEntry, path: string) => void;
  readonly projectName: string;
  readonly rootPath: string;
  readonly theme: "light" | "dark";
}) {
  const listing = useDirectoryListing(props.files, props.directoryPath);
  const children = React.useMemo(
    () => fileBreadcrumbChildren(listing.entries ?? [], props.directoryPath),
    [listing.entries, props.directoryPath],
  );
  const entryByPath = React.useMemo(() => {
    const map = new Map<string, FileEntry>();
    const prefix = props.directoryPath ? `${props.directoryPath}/` : "";
    for (const entry of listing.entries ?? []) map.set(`${prefix}${entry.name}`, entry);
    return map;
  }, [listing.entries, props.directoryPath]);
  const parentPath = fileBreadcrumbParent(props.directoryPath);
  const canGoBack =
    props.directoryPath !== props.rootPath &&
    parentPath !== null &&
    (props.rootPath === "" ||
      parentPath === props.rootPath ||
      parentPath.startsWith(`${props.rootPath}/`));

  return (
    <MenuPopup
      align="start"
      side="bottom"
      className="w-max min-w-32 max-w-[min(19rem,var(--available-width))]"
      onKeyDown={(event) => {
        if (event.key !== "ArrowLeft" || !canGoBack || parentPath === null) return;
        event.preventDefault();
        event.stopPropagation();
        props.onDirectoryChange(parentPath);
      }}
    >
      {canGoBack && parentPath !== null ? (
        <>
          <MenuItem closeOnClick={false} onClick={() => props.onDirectoryChange(parentPath)}>
            <ArrowLeftIcon />
            <span className="truncate">Back to {pathLabel(parentPath, props.projectName)}</span>
          </MenuItem>
          <MenuSeparator />
        </>
      ) : null}
      <MenuGroup key={props.directoryPath}>
        {listing.loading ? (
          <MenuItem disabled>
            <Spinner />
            Loading folder…
          </MenuItem>
        ) : listing.error !== null ? (
          <MenuItem closeOnClick={false} onClick={listing.refresh}>
            <RefreshIcon refreshing={false} />
            <span className="min-w-0 flex-1 truncate">Retry loading folder</span>
          </MenuItem>
        ) : children.length === 0 ? (
          <MenuItem disabled>This folder is empty.</MenuItem>
        ) : (
          children.map((child) => {
            const isCurrentFile = child.kind === "file" && child.path === props.currentFilePath;
            return (
              <MenuItem
                key={child.path}
                closeOnClick={child.kind === "file"}
                aria-current={isCurrentFile ? "page" : undefined}
                className={cn(isCurrentFile && "bg-foreground/[0.08]")}
                onClick={() => {
                  if (child.kind === "directory") {
                    props.onDirectoryChange(child.path);
                    return;
                  }
                  const entry = entryByPath.get(child.path);
                  if (entry === undefined) return;
                  props.onOpenChange(false);
                  props.onOpenFile(entry, child.path);
                }}
              >
                <PierreEntryIcon pathValue={child.path} kind={child.kind} theme={props.theme} />
                <Tooltip>
                  <TooltipTrigger render={<span className="min-w-0 flex-1 truncate" />}>
                    {child.label}
                  </TooltipTrigger>
                  <TooltipPopup side="right" className="max-w-80">
                    {child.path}
                  </TooltipPopup>
                </Tooltip>
                {child.kind === "directory" ? <ChevronRightIcon /> : null}
              </MenuItem>
            );
          })
        )}
      </MenuGroup>
    </MenuPopup>
  );
}

function DirectoryBreadcrumb(props: FileBreadcrumbsProps & { readonly crumb: FileBreadcrumb }) {
  const [open, setOpen] = React.useState(false);
  const [directoryPath, setDirectoryPath] = React.useState(props.crumb.path);

  React.useEffect(() => {
    setOpen(false);
    setDirectoryPath(props.crumb.path);
  }, [props.crumb.path, props.relativePath]);

  const handleOpenChange = (nextOpen: boolean) => {
    setOpen(nextOpen);
    if (nextOpen) setDirectoryPath(props.crumb.path);
  };

  return (
    <Menu open={open} onOpenChange={handleOpenChange}>
      <Tooltip>
        <TooltipTrigger
          render={
            <MenuTrigger
              render={
                <button
                  type="button"
                  aria-label={`Browse ${props.crumb.label}`}
                  className="relative block max-w-40 cursor-pointer rounded-sm px-0.5 text-left text-muted-foreground outline-none pointer-coarse:after:-inset-y-3 pointer-coarse:after:absolute pointer-coarse:after:inset-x-0 hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring data-popup-open:bg-accent data-popup-open:text-foreground"
                />
              }
            />
          }
        >
          <span className="block truncate">{props.crumb.label}</span>
        </TooltipTrigger>
        <TooltipPopup side="top" className="max-w-80">
          {props.crumb.path || props.projectName}
        </TooltipPopup>
      </Tooltip>
      {open ? (
        <BreadcrumbMenuContent
          files={props.files}
          currentFilePath={props.relativePath}
          directoryPath={directoryPath}
          onDirectoryChange={setDirectoryPath}
          onOpenChange={handleOpenChange}
          onOpenFile={props.onOpenFile}
          projectName={props.projectName}
          rootPath={props.crumb.path}
          theme={props.theme}
        />
      ) : null}
    </Menu>
  );
}

export function FileBreadcrumbs(props: FileBreadcrumbsProps): React.ReactElement {
  const breadcrumbs = React.useMemo(
    () => fileBreadcrumbs(props.projectName, props.relativePath),
    [props.projectName, props.relativePath],
  );

  return (
    <>
      {breadcrumbs.map((crumb, index) => (
        <div
          key={crumb.path || "project"}
          className="flex min-w-0 shrink-0 items-center"
          data-current-file-crumb={crumb.kind === "file"}
        >
          {index > 0 ? (
            <ChevronRightIcon className="mx-1 size-3.5 shrink-0 text-muted-foreground/60" />
          ) : null}
          {crumb.kind === "file" ? (
            <span aria-current="page">
              <BreadcrumbLabel current label={crumb.label} pathLabel={crumb.path} />
            </span>
          ) : (
            <DirectoryBreadcrumb {...props} crumb={crumb} />
          )}
        </div>
      ))}
    </>
  );
}
