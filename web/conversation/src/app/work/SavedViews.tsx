import { useClient } from "../client.ts";
import { useShell } from "../App.tsx";
import {
  AlertDialog,
  AlertDialogPopup,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
} from "../../components/ui/alert-dialog.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import React from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import {
  BookmarkIcon,
  MoreHorizontalIcon,
  PlusIcon,
  StarIcon,
} from "lucide-react";
import { Button } from "../../components/ui/button.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import {
  Dialog,
  DialogPopup,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogPanel,
  DialogFooter,
} from "../../components/ui/dialog.tsx";
import {
  Menu,
  MenuTrigger,
  MenuPopup,
  MenuItem,
  MenuSeparator,
} from "../../components/ui/menu.tsx";
import {
  useSavedViews,
  savedViewPath,
  type SavedView,
} from "./lib/savedViews.tsx";
import {
  isDefaultViewState,
  serializeViewState,
  effectiveSort,
  SORT_LABELS,
  COMPLETED_WINDOW_LABELS,
  FILTER_KEYS,
  FILTER_LABELS,
  type WorkViewState,
} from "./lib/viewState.ts";
import { WorkBoard } from "./WorkBoard.tsx";
import { usePageTitle } from "../pageTitle.ts";

export function ViewSummary({
  state,
}: {
  state: WorkViewState;
}): React.ReactElement {
  const details = [
    state.view === "board" ? "Board" : "List",
    `${state.tab} tab`,
    SORT_LABELS[effectiveSort(state)],
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
      <DialogPopup className="sm:max-w-md" showCloseButton={!busy}>
        <DialogHeader>
          <DialogTitle>{rename ? "Rename view" : "Save as view"}</DialogTitle>
          <DialogDescription>
            {rename
              ? "Choose a name for this view."
              : "Keep this Work setup so you can return to it anytime."}
          </DialogDescription>
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
              <div className="space-y-2 rounded-lg border border-border bg-muted/40 p-3">
                <p className="text-xs font-medium">What is kept</p>
                <ViewSummary state={state} />
                <p className="text-xs text-muted-foreground">
                  {projectId === null ? "All projects" : "Current project"}
                </p>
              </div>
            )}
            <label className="flex items-center justify-between gap-4 text-sm">
              Add to Favorites
              <Switch checked={favorite} onCheckedChange={setFavorite} />
            </label>
            <label className="flex items-center justify-between gap-4 text-sm">
              Share with organization
              <Switch checked={shared} onCheckedChange={setShared} />
            </label>
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
            variant="outline"
            disabled={busy}
            onClick={onClose}
          >
            Cancel
          </Button>
          <Button type="submit" form={formId} disabled={busy || !name.trim()}>
            {busy ? "Saving…" : rename ? "Save" : "Create view"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

export function ViewActions({
  view,
  onDeleted,
}: {
  view: SavedView;
  onDeleted?: () => void;
}): React.ReactElement {
  const saved = useSavedViews();
  const navigate = useNavigate();
  const [dialog, setDialog] = React.useState<
    "rename" | "duplicate" | "delete" | null
  >(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const owner = view.can_edit;
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
          render={
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`Actions for ${view.name}`}
              disabled={busy}
            />
          }
        >
          <MoreHorizontalIcon />
        </MenuTrigger>
        <MenuPopup align="end">
          {owner ? (
            <MenuItem onClick={() => setDialog("rename")}>Rename</MenuItem>
          ) : null}
          <MenuItem onClick={() => setDialog("duplicate")}>Duplicate</MenuItem>
          <MenuItem
            onClick={() => {
              void run(async () => saved?.favorite(view, !view.favorite));
            }}
          >
            {view.favorite ? "Remove from Favorites" : "Add to Favorites"}
          </MenuItem>
          {owner ? (
            <MenuItem
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
              {view.scope === "organization"
                ? "Make personal"
                : "Share with organization"}
            </MenuItem>
          ) : null}
          {owner ? (
            <>
              <MenuSeparator />
              <MenuItem
                variant="destructive"
                onClick={() => setDialog("delete")}
              >
                Delete view
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
              <AlertDialogTitle>Delete view?</AlertDialogTitle>
              <AlertDialogDescription>
                “{view.name}” will be removed for everyone who can see it.
                Issues are kept.
              </AlertDialogDescription>
              {error ? (
                <p role="alert" className="text-sm text-destructive">
                  {error}
                </p>
              ) : null}
            </AlertDialogHeader>
            <AlertDialogFooter>
              <Button
                variant="outline"
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

export function SavedViewControls({
  state,
  projectId,
  active,
  onChange,
}: {
  state: WorkViewState;
  projectId: string | null;
  active?: SavedView;
  onChange: (state: WorkViewState) => void;
}): React.ReactElement | null {
  const saved = useSavedViews();
  const navigate = useNavigate();
  const [dialog, setDialog] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  if (!saved || saved.loading || saved.error) return null;
  const query = serializeViewState(state);
  const edited =
    active !== undefined && query !== serializeViewState(active.state);
  const canCreate =
    !isDefaultViewState(state) &&
    !saved.views.some(
      (view) =>
        view.project_id === projectId &&
        serializeViewState(view.state) === query,
    );
  const save = async () => {
    if (!active || busy) return;
    setBusy(true);
    setError(null);
    try {
      await saved.save({ ...active, state }, active.id);
    } catch {
      setError("The view could not be saved. Try again.");
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      {active ? (
        <div className="flex max-w-full flex-wrap items-center gap-2">
          {edited ? (
            <>
              <Badge variant="secondary">Edited</Badge>
              <Button
                variant="ghost"
                size="xs"
                onClick={() => onChange(active.state)}
              >
                Reset
              </Button>
              <Button variant="ghost" size="xs" onClick={() => setDialog(true)}>
                Save as new
              </Button>
              {active.can_edit ? (
                <Button
                  size="xs"
                  disabled={busy}
                  onClick={() => {
                    void save();
                  }}
                >
                  {busy ? "Saving…" : "Save"}
                </Button>
              ) : null}
            </>
          ) : null}
          <ViewActions
            view={active}
            onDeleted={() => {
              void navigate({ to: "/work" });
            }}
          />
        </div>
      ) : canCreate ? (
        <Button variant="ghost" size="xs" onClick={() => setDialog(true)}>
          <PlusIcon />
          Save as view
        </Button>
      ) : null}
      {error ? (
        <span role="alert" className="text-xs text-destructive">
          {error}
        </span>
      ) : null}
      {dialog ? (
        <SaveViewDialog
          state={state}
          projectId={projectId}
          onClose={() => setDialog(false)}
          onSaved={(view) => {
            void navigate({ to: savedViewPath(view) });
          }}
        />
      ) : null}
    </>
  );
}

export function ViewsPage(): React.ReactElement {
  usePageTitle("Views");
  const shell = useShell();
  const client = useClient();
  const saved = useSavedViews();
  const navigate = useNavigate();
  return (
    <>
      <WorkTopBar context="Work" title="Views" connection={shell.connection} />
      <div className="min-h-0 flex-1 overflow-auto">
        <WorkspacePageContainer width="wide">
          <p className="text-sm text-muted-foreground">
            Saved Work setups for you and your organization.
          </p>
          {saved?.loading ? (
            <p className="py-8 text-sm text-muted-foreground" role="status">
              Loading views…
            </p>
          ) : saved?.error ? (
            <div role="alert" className="py-8">
              <p>{saved.error}</p>
              <Button variant="outline" onClick={saved.reload}>
                Try again
              </Button>
            </div>
          ) : (
            <div className="mt-6 divide-y divide-border">
              {saved?.views.length === 0 ? (
                <p className="py-8 text-sm text-muted-foreground">
                  No saved views yet. Adjust your Work filters or display, then
                  choose Save as view.
                </p>
              ) : (
                saved?.views
                  .toSorted((a, b) => a.name.localeCompare(b.name))
                  .map((view) => (
                    <div key={view.id} className="flex items-start gap-3 py-4">
                      <BookmarkIcon className="mt-1 size-4 shrink-0 text-muted-foreground" />
                      <div className="min-w-0 flex-1">
                        <Button
                          variant="ghost"
                          className="max-w-full justify-start whitespace-normal text-left"
                          onClick={() => {
                            void navigate({ to: savedViewPath(view) });
                          }}
                        >
                          {view.name}
                          {view.favorite ? (
                            <StarIcon className="size-3 shrink-0 text-muted-foreground" />
                          ) : null}
                        </Button>
                        <p className="mt-1 text-xs text-muted-foreground">
                          {view.scope === "organization"
                            ? "Organization"
                            : "Personal"}{" "}
                          ·{" "}
                          {view.project_id === null
                            ? "All projects"
                            : client.bootstrap.projects.find((project) => project.id === view.project_id)?.name ?? "Project"}
                        </p>
                        <div className="mt-2">
                          <ViewSummary state={view.state} />
                        </div>
                      </div>
                      <ViewActions view={view} />
                    </div>
                  ))
              )}
            </div>
          )}
        </WorkspacePageContainer>
      </div>
    </>
  );
}

export function SavedViewPage(): React.ReactElement {
  const { viewId } = useParams({ from: "/work/views/$viewId" });
  const saved = useSavedViews();
  const view = saved?.views.find((view) => view.id === viewId);
  if (saved?.loading)
    return (
      <p role="status" className="p-5 text-sm text-muted-foreground">
        Loading view…
      </p>
    );
  if (!view)
    return (
      <div className="p-5">
        <p role="alert">{saved?.error ?? "This view is not available."}</p>
        <Button variant="outline" onClick={saved?.reload}>
          Try again
        </Button>
      </div>
    );
  return (
    <WorkBoard key={view.id} projectId={view.project_id} savedView={view} />
  );
}
