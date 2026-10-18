import React from "react";

import { Button } from "../../components/ui/button.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../components/ui/dialog.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Switch } from "../../components/ui/switch.tsx";
import { hubPath } from "../../runtime/basePath.ts";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { newKey } from "../account/idempotency.ts";
import { useMutation } from "../account/useResource.ts";

export const PROJECT_CREATION_UNAVAILABLE = "An organization owner or admin creates projects";

export interface NewProjectAction {
  readonly openNewProject: () => void;
  readonly canCreate: boolean;
  readonly unavailableReason: string | null;
}

const UNAVAILABLE: NewProjectAction = {
  openNewProject: () => undefined,
  canCreate: false,
  unavailableReason: PROJECT_CREATION_UNAVAILABLE,
};

const NewProjectContext = React.createContext<NewProjectAction | null>(null);

export function useNewProject(): NewProjectAction {
  return React.use(NewProjectContext) ?? UNAVAILABLE;
}

export function projectBoardPath(projectId: string): string {
  return `/work/p/${encodeURIComponent(projectId)}`;
}

/**
 * The created project's id, from the `201` the hub answers with: the native
 * project record names it `project_id`, a project summary names it `id`.
 */
export function createdProjectId(created: unknown): string | null {
  if (typeof created !== "object" || created === null) return null;
  const record = created as { readonly project_id?: unknown; readonly id?: unknown };
  for (const id of [record.project_id, record.id]) {
    if (typeof id === "string" && id.length > 0) return id;
  }
  return null;
}

/**
 * A full load rather than a router navigation: the bootstrap carries the
 * project list the sidebar, the palette and the runner enrollment read, and
 * only a fresh load brings the new project into it.
 */
function openCreatedProject(projectId: string | null): void {
  globalThis.location?.assign(hubPath(projectId === null ? "/work" : projectBoardPath(projectId)));
}

export function NewProjectProvider({
  children,
  onCreated = openCreatedProject,
}: {
  readonly children: React.ReactNode;
  readonly onCreated?: (projectId: string | null) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const canCreate = useAccountBootstrap()?.actor.can_manage ?? false;
  const [open, setOpen] = React.useState(false);
  const key = React.useRef(newKey());
  const create = useMutation(async (input: { name: string; grantAccess: boolean }) => {
    const created = await api.createProject({ ...input, key: key.current });
    key.current = newKey();
    setOpen(false);
    onCreated(createdProjectId(created));
    return created;
  });
  const clearError = create.clearError;

  const openNewProject = React.useCallback(() => {
    if (!canCreate) return;
    clearError();
    setOpen(true);
  }, [canCreate, clearError]);

  const value = React.useMemo<NewProjectAction>(
    () => ({
      openNewProject,
      canCreate,
      unavailableReason: canCreate ? null : PROJECT_CREATION_UNAVAILABLE,
    }),
    [canCreate, openNewProject],
  );

  return (
    <NewProjectContext.Provider value={value}>
      {children}
      {canCreate ? (
        <NewProjectDialog
          open={open}
          onOpenChange={setOpen}
          onCreate={(input) => void create.call(input)}
          pending={create.pending}
          error={create.error?.message ?? null}
        />
      ) : null}
    </NewProjectContext.Provider>
  );
}

export function NewProjectDialog({
  open,
  onOpenChange,
  onCreate,
  pending,
  error,
}: {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly onCreate: (input: { name: string; grantAccess: boolean }) => void;
  readonly pending: boolean;
  readonly error: string | null;
}): React.ReactElement {
  const [name, setName] = React.useState("");
  const [grantAccess, setGrantAccess] = React.useState(true);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup>
        <DialogHeader>
          <DialogTitle>New project</DialogTitle>
          <DialogDescription>
            A project owns its own board, policy, runners and repository binding.
          </DialogDescription>
        </DialogHeader>
        <DialogPanel>
          <form
            id="new-project-form"
            className="flex flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault();
              const value = name.trim();
              if (value.length > 0) onCreate({ name: value, grantAccess });
            }}
          >
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="new-project-name">Name</Label>
              <Input
                id="new-project-name"
                value={name}
                placeholder="example-app"
                onChange={(event) => setName(event.currentTarget.value)}
              />
            </div>
            <label className="flex items-center gap-2.5 text-sm">
              <Switch
                aria-label="Grant yourself write access"
                checked={grantAccess}
                onCheckedChange={setGrantAccess}
              />
              Grant yourself write access
            </label>
            <ControlError message={error} />
          </form>
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline">Cancel</Button>} />
          <Button
            type="submit"
            form="new-project-form"
            disabled={pending || name.trim().length === 0}
          >
            {pending ? "Creating…" : "Create project"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}
