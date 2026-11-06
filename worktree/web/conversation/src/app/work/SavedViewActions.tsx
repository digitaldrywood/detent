import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { RefreshCwIcon, CopyIcon, MoreHorizontalIcon, PencilIcon, StarIcon, TrashIcon, UsersIcon } from "lucide-react";
import { useClient } from "../client.ts";
import { useIsMobile } from "../../hooks/useMediaQuery.ts";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import {
  AlertDialog, AlertDialogPopup, AlertDialogHeader, AlertDialogTitle,
  AlertDialogDescription, AlertDialogFooter,
} from "../../components/ui/alert-dialog.tsx";
import {
  Dialog, DialogPopup, DialogHeader, DialogTitle, DialogDescription,
  DialogPanel, DialogFooter,
} from "../../components/ui/dialog.tsx";
import { Menu, MenuTrigger, MenuPopup, MenuItem, MenuSeparator } from "../../components/ui/menu.tsx";
import { Pill } from "./components/Pill.tsx";
import { ProjectGlyph } from "../components/ProjectGlyph.tsx";
import type { Lane } from "./lib/model.ts";
import { cn } from "../../lib/utils.ts";
import { useSavedViews, savedViewPath, type SavedView } from "./lib/savedViews.tsx";
import {
  effectiveSort, laneVisible, SORT_LABELS, COMPLETED_WINDOW_LABELS, FILTER_KEYS,
  FILTER_LABELS, type WorkViewState,
} from "./lib/viewState.ts";

export function ViewSummary({
  state,
  pills = false,
}: {
  state: WorkViewState;
  pills?: boolean;
}): React.ReactElement {
  const details = [
    state.view === "board" ? "Board" : "List",
    `${state.tab} tab`,
    `Sort: ${SORT_LABELS[effectiveSort(state)]}`,
    `Completed: ${COMPLETED_WINDOW_LABELS[state.completedWindow]}`,
    `Lanes: ${state.lanes === null ? "All" : state.lanes.join(", ") || "none"}`,
    ...(state.collapsed.length
      ? [`Collapsed: ${state.collapsed.join(", ")}`]
      : []),
    ...FILTER_KEYS.filter((key) => state[key].length).map(
      (key) => `${FILTER_LABELS[key]}: ${state[key].join(", ")}`,
    ),
    ...(state.q ? [`Search: ${state.q}`] : []),
    ...(state.createdBy === "me" ? ["Filed by me"] : []),
    ...(state.archived ? ["Including archived"] : []),
  ];
  if (pills) return (
    <div className="flex flex-wrap gap-1.5">
      {details.map((detail) => <Pill key={detail} tone="mute" className="max-w-full"><span className="truncate">{detail}</span></Pill>)}
    </div>
  );
  return (
    <p className="text-sm text-muted-foreground break-words">
      {details.join(" · ")}
    </p>
  );
}

export function SaveViewDialog({
  state,
  projectId,
  projectIds,
  lanes,
  source,
  rename = false,
  onClose,
  onSaved,
}: {
  state: WorkViewState;
  projectId: string | null;
  projectIds: readonly string[];
  lanes?: readonly Lane[];
  source?: SavedView;
  rename?: boolean;
  onClose: () => void;
  onSaved: (view: SavedView) => void;
}): React.ReactElement {
  const saved = useSavedViews();
  const client = useClient();
  const formId = React.useId();
  const projects = client.bootstrap.projects.filter((project) => projectIds.length === 0 || projectIds.includes(project.id));
  const focusedProject = projects.find((project) => project.id === projectId);
  const filters = [
    ...FILTER_KEYS.filter((key) => state[key].length).map((key) => `${FILTER_LABELS[key]}: ${state[key].join(", ")}`),
    ...(state.q ? [`Search: ${state.q}`] : []),
    ...(state.createdBy === "me" ? ["Filed by me"] : []),
    ...(state.archived ? ["Including archived"] : []),
  ];
  const boardLanes = lanes?.filter((lane) => !lane.terminal);
  const laneSummary = boardLanes === undefined
    ? state.lanes === null ? "All" : state.lanes.join(", ") || "none"
    : `${boardLanes.filter((lane) => laneVisible(state, lane)).length}/${boardLanes.length}`;
  const [name, setName] = React.useState(
    source ? (rename ? source.name : `${source.name} copy`) : "",
  );
  const [favorite, setFavorite] = React.useState(
    rename ? (source?.favorite ?? true) : true,
  );
  const [shared, setShared] = React.useState(
    rename && source?.scope === "organization",
  );
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!saved || busy || !name.trim()) return;
    setBusy(true);
    setError(null);
    try {
      const view = await saved.save(
        {
          name: name.trim(),
          project_id: projectId,
          project_ids: projectIds,
          scope: shared ? "organization" : "personal",
          favorite,
          state,
        },
        rename ? source?.id : undefined,
      );
      onSaved(view);
      onClose();
    } catch {
      setError("The view could not be saved. Try again.");
      setBusy(false);
    }
  };
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onClose();
      }}
    >
      <DialogPopup className="sm:max-w-md" showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{rename ? "Rename view" : "Save as view"}</DialogTitle>
          {rename ? null : <DialogDescription className="sr-only">
            Keep this project setup, filters and display under a name.
          </DialogDescription>}
        </DialogHeader>
        <DialogPanel>
          <form
            id={formId}
            className="space-y-4"
            onSubmit={(event) => {
              void submit(event);
            }}
          >
            <label className="grid gap-2 text-sm">
              Name
              <Input
                autoFocus
                required
                maxLength={120}
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder="e.g. Active bugs"
              />
            </label>
            {rename ? null : (
              <div className="space-y-4">
                <div className="space-y-2">
                  <p className="text-sm font-medium">Projects</p>
                  <div className="flex flex-wrap gap-1.5" aria-label="Projects">
                    {projects.map((project) => <Pill key={project.id} tone="mute"><ProjectGlyph projectId={project.id} projectName={project.name} className="size-3.5 shrink-0" />{project.name}</Pill>)}
                    {projectIds.length === 0 ? <span className="text-xs text-muted-foreground">All projects</span> : null}
                  </div>
                </div>
                <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm">
                  <dt className="text-muted-foreground">Opens on</dt>
                  <dd>{focusedProject?.name ?? `All ${projects.length} projects`}</dd>
                  <dt className="text-muted-foreground">Filters</dt>
                  <dd className="break-words">{filters.join(" · ") || "None"}</dd>
                  <dt className="text-muted-foreground">Sort</dt>
                  <dd>{SORT_LABELS[effectiveSort(state)]}</dd>
                  <dt className="text-muted-foreground">Layout</dt>
                  <dd>{state.view === "board" ? "Board" : "List"} · Lanes {laneSummary}</dd>
                  {state.collapsed.length ? <><dt className="text-muted-foreground">Collapsed</dt><dd>{state.collapsed.join(", ")}</dd></> : null}
                  {state.tab !== "active" ? <><dt className="text-muted-foreground">Tab</dt><dd>{state.tab}</dd></> : null}
                  {state.completedWindow !== "24h" || state.completedFrom || state.completedTo ? <>
                    <dt className="text-muted-foreground">Completed</dt>
                    <dd>{state.completedFrom || state.completedTo ? `${state.completedFrom || "Any time"} – ${state.completedTo || "Now"}` : COMPLETED_WINDOW_LABELS[state.completedWindow]}</dd>
                  </> : null}
                </dl>
              </div>
            )}
            {rename ? null : <>
              <div className="flex items-start justify-between gap-4 text-sm">
                <label className="flex items-center gap-2">
                  <Checkbox checked={favorite} onCheckedChange={setFavorite} />
                  Pin to Favorites
                </label>
              </div>
              <div className="flex items-start justify-between gap-4 text-sm">
                <div className="space-y-1">
                  <label htmlFor={`${formId}-shared`} className="font-medium">Share with organization</label>
                  <p id={`${formId}-shared-help`} className="sr-only">Everyone in {client.bootstrap.organization?.name ?? "the organization"} can open it. Only you can change or delete it.</p>
                </div>
                <Switch id={`${formId}-shared`} aria-describedby={`${formId}-shared-help`} checked={shared} onCheckedChange={setShared} />
              </div>
            </>}
            {error ? (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            ) : null}
          </form>
        </DialogPanel>
        <DialogFooter>
          <Button
            type="button"
            variant="ghost"
            disabled={busy}
            onClick={onClose}
          >
            Cancel
          </Button>
          <Button type="submit" form={formId} disabled={busy || !name.trim()}>
            {busy ? "Saving…" : rename ? "Rename" : "Save view"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

export function ViewActions({
  view,
  onDeleted,
  sidebar = false,
}: {
  view: SavedView;
  onDeleted?: () => void;
  sidebar?: boolean;
  active?: boolean;
}): React.ReactElement {
  const saved = useSavedViews();
  const navigate = useNavigate();
  const [dialog, setDialog] = React.useState<
    "rename" | "duplicate" | "delete" | null
  >(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const owner = view.can_edit;
  const isMobile = useIsMobile();
  const density = isMobile ? "touch" : "default";
  const run = async (operation: () => Promise<unknown>) => {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await operation();
    } catch {
      setError("The view could not be updated. Try again.");
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <Menu>
        <MenuTrigger
          aria-label={`Actions for ${view.name}`}
          disabled={busy}
          className={sidebar ? cn("absolute top-1 right-1 inline-flex size-6 shrink-0 cursor-pointer items-center justify-center rounded-sm text-sidebar-muted-foreground outline-none ring-ring hover:bg-sidebar-accent hover:text-sidebar-foreground focus-visible:ring-2 data-[popup-open]:opacity-100 sm:group-hover/menu-item:opacity-100 sm:group-focus-within/menu-item:opacity-100", "sm:opacity-0") : undefined}
          render={sidebar ? undefined : <Button variant="ghost" size="icon-sm" />}
        >
          <MoreHorizontalIcon className={sidebar ? "size-3.5" : undefined} />
        </MenuTrigger>
        <MenuPopup align="end" className={isMobile ? "w-60" : "w-64"}>
          {owner ? <MenuItem density={density} disabled={saved?.currentSetup == null} onClick={() => {
            const setup = saved?.currentSetup;
            if (setup) void run(() => saved.save({ ...view, ...setup }, view.id));
          }}><RefreshCwIcon />Update with current setup</MenuItem> : null}
          {owner ? (
            <MenuItem density={density} onClick={() => setDialog("rename")}><PencilIcon />Rename</MenuItem>
          ) : null}
          <MenuItem density={density} onClick={() => setDialog("duplicate")}><CopyIcon />Duplicate</MenuItem>
          <MenuItem
            density={density}
            onClick={() => {
              void run(async () => saved?.favorite(view, !view.favorite));
            }}
          >
            <StarIcon />
            {view.favorite ? "Unpin from Favorites" : "Add to Favorites"}
          </MenuItem>
          {owner ? (
            <MenuItem
            density={density}
              onClick={() => {
                void run(async () =>
                  saved?.save(
                    {
                      ...view,
                      scope:
                        view.scope === "organization"
                          ? "personal"
                          : "organization",
                    },
                    view.id,
                  ),
                );
              }}
            >
              <UsersIcon />
              {view.scope === "organization"
                ? "Make personal"
                : "Share with organization"}
            </MenuItem>
          ) : null}
          {owner ? (
            <>
              <MenuSeparator />
              <MenuItem
            density={density}
                variant="destructive"
                onClick={() => setDialog("delete")}
              >
                <TrashIcon />Delete view
              </MenuItem>
            </>
          ) : null}
        </MenuPopup>
      </Menu>
      {error && dialog !== "delete" ? (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      ) : null}
      {dialog === "rename" || dialog === "duplicate" ? (
        <SaveViewDialog
          state={view.state}
          projectId={view.project_id}
          projectIds={view.project_ids}
          source={view}
          rename={dialog === "rename"}
          onClose={() => setDialog(null)}
          onSaved={(next) => {
            if (dialog === "duplicate")
              void navigate({ to: savedViewPath(next) });
          }}
        />
      ) : null}
      {dialog === "delete" ? (
        <AlertDialog
          open
          onOpenChange={(open) => {
            if (!open && !busy) setDialog(null);
          }}
        >
          <AlertDialogPopup className="sm:max-w-md">
            <AlertDialogHeader>
              <AlertDialogTitle>Delete “{view.name}”?</AlertDialogTitle>
              <AlertDialogDescription>
                The view is removed for you and for anyone it is shared with. Issues are not changed.
              </AlertDialogDescription>
              {error ? (
                <p role="alert" className="text-sm text-destructive">
                  {error}
                </p>
              ) : null}
            </AlertDialogHeader>
            <AlertDialogFooter>
              <Button
                variant="ghost"
                disabled={busy}
                onClick={() => setDialog(null)}
              >
                Cancel
              </Button>
              <Button
                variant="destructive"
                disabled={busy}
                onClick={() => {
                  void run(async () => {
                    await saved?.remove(view);
                    setDialog(null);
                    onDeleted?.();
                  });
                }}
              >
                Delete view
              </Button>
            </AlertDialogFooter>
          </AlertDialogPopup>
        </AlertDialog>
      ) : null}
    </>
  );
}
