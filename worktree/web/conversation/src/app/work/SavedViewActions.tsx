import React from "react";
import { useNavigate } from "@tanstack/react-router";
import { CopyIcon, MoreHorizontalIcon, PencilIcon, StarIcon, TrashIcon, UsersIcon } from "lucide-react";
import { useClient } from "../client.ts";
import { useIsMobile } from "../../hooks/useMediaQuery.ts";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
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
import { cn } from "../../lib/utils.ts";
import { useSavedViews, savedViewPath, type SavedView } from "./lib/savedViews.tsx";
import {
  effectiveSort, SORT_LABELS, COMPLETED_WINDOW_LABELS, FILTER_KEYS,
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
    ...(state.lanes === null
      ? []
      : [`Lanes: ${state.lanes.join(", ") || "none"}`]),
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
  source,
  rename = false,
  onClose,
  onSaved,
}: {
  state: WorkViewState;
  projectId: string | null;
  source?: SavedView;
  rename?: boolean;
  onClose: () => void;
  onSaved: (view: SavedView) => void;
}): React.ReactElement {
  const saved = useSavedViews();
  const client = useClient();
  const formId = React.useId();
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
      <DialogPopup className="sm:max-w-lg" showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{rename ? "Rename view" : "Save view"}</DialogTitle>
          {rename ? null : <DialogDescription>
            Keeps the current layout, filters, sort and lanes under a name. The board and list stay as they are.
          </DialogDescription>}
        </DialogHeader>
        <DialogPanel>
          <form
            id={formId}
            className="space-y-5"
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
              <div className="space-y-2">
                <p className="text-sm font-medium">This view keeps</p>
                <ViewSummary state={state} pills />
                <p className="text-xs text-muted-foreground">
                  {projectId === null ? "All projects" : "Current project"}
                </p>
              </div>
            )}
            {rename ? null : <>
              <div className="flex items-start justify-between gap-4 text-sm">
                <div className="space-y-1">
                  <label htmlFor={`${formId}-favorite`} className="font-medium">Add to Favorites</label>
                  <p id={`${formId}-favorite-help`} className="text-muted-foreground">Shows the view in the sidebar under Favorites.</p>
                </div>
                <Switch id={`${formId}-favorite`} aria-describedby={`${formId}-favorite-help`} checked={favorite} onCheckedChange={setFavorite} />
              </div>
              <div className="flex items-start justify-between gap-4 text-sm">
                <div className="space-y-1">
                  <label htmlFor={`${formId}-shared`} className="font-medium">Share with organization</label>
                  <p id={`${formId}-shared-help`} className="text-muted-foreground">Everyone in {client.bootstrap.organization?.name ?? "the organization"} can open it. Only you can change or delete it.</p>
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
  active = false,
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
          className={sidebar ? cn("absolute top-1 right-1 inline-flex size-6 shrink-0 cursor-pointer items-center justify-center rounded-sm text-sidebar-muted-foreground outline-none ring-ring hover:bg-sidebar-accent hover:text-sidebar-foreground focus-visible:ring-2 data-[popup-open]:opacity-100 sm:group-hover/menu-item:opacity-100 sm:group-focus-within/menu-item:opacity-100", !active && "sm:opacity-0") : undefined}
          render={sidebar ? undefined : <Button variant="ghost" size="icon-sm" />}
        >
          <MoreHorizontalIcon className={sidebar ? "size-3.5" : undefined} />
        </MenuTrigger>
        <MenuPopup align="end" className={isMobile ? "w-60" : "w-52"}>
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
            {view.favorite ? "Remove from Favorites" : "Add to Favorites"}
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
