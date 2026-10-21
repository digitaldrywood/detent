import React from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { BookmarkIcon, PlusIcon, StarIcon } from "lucide-react";
import { useClient } from "../client.ts";
import { useShell } from "../App.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import { Button } from "../../components/ui/button.tsx";
import { useSavedViews, savedViewPath, type SavedView } from "./lib/savedViews.tsx";
import { isDefaultViewState, serializeViewState, type WorkViewState } from "./lib/viewState.ts";
import { SaveViewDialog, ViewActions, ViewSummary } from "./SavedViewActions.tsx";
import { WorkBoard } from "./WorkBoard.tsx";
import { usePageTitle } from "../pageTitle.ts";

export { SaveViewDialog, ViewActions, ViewSummary } from "./SavedViewActions.tsx";

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
        <div className="flex w-full max-w-full flex-wrap items-center gap-2 sm:w-auto">
          {edited ? (
            <>
              <span className="mr-auto inline-flex items-center gap-1.5 text-xs text-muted-foreground sm:mr-0"><span aria-hidden className="size-1.5 rounded-full bg-primary" />Edited</span>
              <Button
                variant="ghost"
                size="xs"
                onClick={() => onChange(active.state)}
              >
                Reset
              </Button>
              <Button variant="outline" size="xs" onClick={() => setDialog(true)}>
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
