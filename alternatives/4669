import React from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { BookmarkIcon, PlusIcon, StarIcon } from "lucide-react";
import { useClient } from "../client.ts";
import { useShell } from "../App.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";
import { WorkTopBar } from "./components/WorkTopBar.tsx";
import { Button } from "../../components/ui/button.tsx";
import { useSavedViews, savedViewPath, type SavedView } from "./lib/savedViews.tsx";
import { serializeViewState, type WorkViewState } from "./lib/viewState.ts";
import { SaveViewDialog, ViewActions, ViewSummary } from "./SavedViewActions.tsx";
import { WorkBoard } from "./WorkBoard.tsx";
import { useUiStateStore, setSidebarProjectWorkingSetKeys, setSidebarProjectScopeKey } from "../../uiStateStore.ts";
import { sidebarProjectScopeKey } from "../adapters/shell.ts";
import { usePageTitle } from "../pageTitle.ts";
import type { Lane } from "./lib/model.ts";

export { SaveViewDialog, ViewActions, ViewSummary } from "./SavedViewActions.tsx";

export function SavedViewControls({
  state,
  projectId,
  lanes,
  active,
  onChange,
}: {
  state: WorkViewState;
  projectId: string | null;
  lanes?: readonly Lane[];
  active?: SavedView;
  onChange: (state: WorkViewState) => void;
}): React.ReactElement | null {
  const saved = useSavedViews();
  const client = useClient();
  const navigate = useNavigate();
  const [dialog, setDialog] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  if (!saved || saved.loading || saved.error) return null;
  const projectIds = saved.currentSetup?.project_ids ?? (projectId === null ? [] : [projectId]);
  const query = serializeViewState(state);
  const edited =
    active !== undefined && (query !== serializeViewState(active.state) || active.project_id !== projectId ||
      JSON.stringify([...active.project_ids].sort()) !== JSON.stringify([...projectIds].sort()));
  const canCreate =
    !saved.views.some(
      (view) =>
        view.project_id === projectId &&
        JSON.stringify([...view.project_ids].sort()) === JSON.stringify([...projectIds].sort()) &&
        serializeViewState(view.state) === query,
    );
  const save = async () => {
    if (!active || busy) return;
    setBusy(true);
    setError(null);
    try {
      await saved.save({ ...active, project_id: projectId, project_ids: projectIds, state }, active.id);
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
                onClick={() => {
                  const projects = client.bootstrap.projects;
                  const keys = projects.filter((project) => active.project_ids.includes(project.id)).map(sidebarProjectScopeKey);
                  const focus = projects.find((project) => project.id === active.project_id);
                  useUiStateStore.setState((current) => setSidebarProjectScopeKey(setSidebarProjectWorkingSetKeys(current, keys), focus ? sidebarProjectScopeKey(focus) : null));
                  onChange(active.state);
                }}
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
          projectIds={projectIds}
          lanes={lanes}
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
                          {view.project_ids.length === 0 ? "All projects" : view.project_ids.map((id) => client.bootstrap.projects.find((project) => project.id === id)?.name ?? "Project").join(", ")}
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
  const shell = useShell();
  const view = saved?.views.find((view) => view.id === viewId);
  const scope = view ? JSON.stringify([view.id, view.project_ids, view.project_id]) : null;
  const [restored, setRestored] = React.useState<string | null>(null);
  React.useLayoutEffect(() => setRestored(scope), [scope]);
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
  if (restored !== scope) return <p role="status" className="p-5 text-sm text-muted-foreground">Loading view…</p>;
  return (
    <WorkBoard key={view.id} projectId={shell.projectId || null} savedView={view} />
  );
}
