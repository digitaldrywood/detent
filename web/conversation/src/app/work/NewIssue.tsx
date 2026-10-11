// Creating an issue from anywhere in the client.
//
// One dialog serves every entry point: the first-run checklist, the board
// header, each lane's add button, the command palette and the `c` shortcut.
// The provider owns the open dialog so an entry point only says where the
// issue should go (a project, and a lane when it was started from one), and a
// board that is on screen learns about the new issue through `created`.
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
import { AttachmentEditor } from "./components/AttachmentEditor.tsx";
import { toastManager } from "../../components/ui/toast.tsx";
import type { AccountProject } from "../../contracts/account.ts";
import { ControlError, NativeSelect } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { newKey } from "../account/idempotency.ts";
import { useMutation } from "../account/useResource.ts";
import { useSidebarData } from "../adapters/sidebarData.tsx";

export const ISSUE_CREATION_NEEDS_WRITE = "You need write access to a project to create issues";

/** The lane a new issue starts in when nobody chose one. */
export function firstIssueState(project: AccountProject | undefined): string {
  if (project?.profile === "native") return project.states[0]?.name ?? "";
  return (
    project?.states.find((state) => state.dispatchable === true && state.terminal !== true)
      ?.name ??
    project?.states[0]?.name ??
    ""
  );
}

/** The requested lane when the project has it, otherwise the project's first working lane. */
export function newIssueState(project: AccountProject | undefined, requested: string | null): string {
  if (requested !== null && project?.states.some((state) => state.name === requested) === true) {
    return requested;
  }
  return firstIssueState(project);
}

/**
 * The projects a new issue may go to: the writable ones, and when it was
 * started from a lane, only those whose workflow has that lane, so "New issue
 * in Review" never lands in another lane.
 */
export function newIssueProjects(
  projects: readonly AccountProject[],
  state: string | null,
): readonly AccountProject[] {
  return projects.filter(
    (project) =>
      project.can_write && (state === null || project.states.some((entry) => entry.name === state)),
  );
}

/**
 * The project a new issue goes to: the requested one when the reader may
 * write to it, otherwise the first writable project.
 */
export function newIssueProject(
  projects: readonly AccountProject[],
  requested: string | null,
): AccountProject | undefined {
  const writable = projects.filter((project) => project.can_write);
  return writable.find((project) => project.id === requested) ?? writable[0];
}

export function NewIssueDialog({
  open,
  onOpenChange,
  projects,
  projectId,
  state: requestedState = null,
  description,
  onCreated,
}: {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  /** The projects the issue may go to. A project picker shows when there is more than one. */
  readonly projects: readonly AccountProject[];
  readonly projectId: string | null;
  readonly state?: string | null;
  readonly description?: string;
  readonly onCreated: (input: { readonly projectId: string; readonly title: string }) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const [target, setTarget] = React.useState(() => newIssueProject(projects, projectId)?.id ?? "");
  const project = projects.find((candidate) => candidate.id === target);
  const [title, setTitle] = React.useState("");
	const [githubIssueUrl, setGithubIssueUrl] = React.useState("");
  const [body, setBody] = React.useState("");
  const [uploading, setUploading] = React.useState(false);
  const [state, setState] = React.useState(() => newIssueState(project, requestedState));
  const key = React.useRef(newKey());
  const create = useMutation(async () => {
    if (uploading) return;
    const created = await api.createFirstIssue({
      projectId: target,
	  githubIssueUrl: githubIssueUrl.trim() || undefined,
      title: title.trim(),
      body: body.trim(),
      state,
      key: key.current,
    });
    key.current = newKey();
    const createdTitle = title.trim() || githubIssueUrl.trim();
    setTitle("");
    setBody("");
	setGithubIssueUrl("");
    onOpenChange(false);
    onCreated({ projectId: target, title: createdTitle });
    return created;
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup>
        <DialogHeader>
          <DialogTitle>New issue</DialogTitle>
          <DialogDescription>
            {description ?? "A runner picks it up from its lane."}
          </DialogDescription>
        </DialogHeader>
        <DialogPanel>
          <form
            id="new-issue-form"
            className="flex flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault();
              if ((title.trim().length > 0 || githubIssueUrl.trim().length > 0) && project !== undefined) void create.call();
            }}
          >
            {/* The picker also shows when the issue cannot go where it was
                started, so it never lands in another project unannounced. */}
            {projects.length > 1 || (projectId !== null && projectId !== target) ? (
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="new-issue-project">Project</Label>
                <NativeSelect
                  id="new-issue-project"
                  value={target}
                  options={projects.map((entry) => ({ value: entry.id, label: entry.name }))}
                  onValueChange={(next) => {
                    setTarget(next);
                    setState(
                      newIssueState(
                        projects.find((candidate) => candidate.id === next),
                        requestedState,
                      ),
                    );
                  }}
                />
              </div>
            ) : null}
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="new-issue-github-url">GitHub issue URL (optional)</Label>
              <Input
                id="new-issue-github-url"
                value={githubIssueUrl}
                placeholder="https://github.com/owner/repo/issues/123"
                onChange={(event) => setGithubIssueUrl(event.currentTarget.value)}
              />
              <p className="text-xs text-muted-foreground">Link an issue from this project's repository. The runner imports its context before the first run.</p>
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="new-issue-title">Title</Label>
              <Input
                id="new-issue-title"
                autoFocus
                value={title}
                onChange={(event) => setTitle(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="new-issue-body">What needs doing</Label>
              <AttachmentEditor
                key={target}
                projectId={target}
                onUploadingChange={setUploading}
                disabled={create.pending}
                id="new-issue-body"
                value={body}
                onChange={setBody}
              />
            </div>
            {project !== undefined && project.states.length > 1 ? (
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="new-issue-state">Lane</Label>
                <NativeSelect
                  id="new-issue-state"
                  value={state}
                  options={project.states.map((entry) => ({
                    value: entry.name,
                    label: entry.name,
                  }))}
                  onValueChange={setState}
                />
              </div>
            ) : null}
            <ControlError message={create.error?.message ?? null} />
          </form>
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline">Cancel</Button>} />
          <Button
            type="submit"
            form="new-issue-form"
            disabled={uploading || create.pending || (title.trim().length === 0 && githubIssueUrl.trim().length === 0) || project === undefined}
          >
            {create.pending ? "Creating…" : "Create issue"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

export interface NewIssueRequest {
  /** The project to create in; the board on screen's, then the sidebar's active project, when omitted. */
  readonly projectId?: string | null;
  /** The lane to start in; the project's first working lane when omitted. */
  readonly state?: string | null;
}

export interface NewIssueAction {
  readonly openNewIssue: (request?: NewIssueRequest) => void;
  readonly canCreate: boolean;
  readonly unavailableReason: string | null;
  /** Counts issues created through the provider, so an open board can reload. */
  readonly created: number;
  /** Names the project the board on screen shows; returns the undo. */
  readonly setScope: (projectId: string | null) => () => void;
}

const UNAVAILABLE: NewIssueAction = {
  openNewIssue: () => undefined,
  canCreate: false,
  unavailableReason: ISSUE_CREATION_NEEDS_WRITE,
  created: 0,
  setScope: () => () => undefined,
};

const NewIssueContext = React.createContext<NewIssueAction | null>(null);

export function useNewIssue(): NewIssueAction {
  return React.use(NewIssueContext) ?? UNAVAILABLE;
}

/**
 * Makes `projectId` the default project for every entry point while the
 * calling board is mounted, so the palette and the shortcut create into the
 * board the reader is looking at.
 */
export function useNewIssueScope(projectId: string | null): void {
  const { setScope } = useNewIssue();
  React.useEffect(() => setScope(projectId), [setScope, projectId]);
}

interface OpenRequest {
  readonly seq: number;
  readonly projectId: string | null;
  readonly state: string | null;
}

export function NewIssueProvider({ children }: { readonly children: React.ReactNode }): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  const activeProjectId = useSidebarData()?.activeProjectId ?? null;
  const allProjects = bootstrap?.projects;
  const writable = React.useMemo(() => newIssueProjects(allProjects ?? [], null), [allProjects]);
  const canCreate = writable.length > 0;
  const [request, setRequest] = React.useState<OpenRequest | null>(null);
  const [open, setOpen] = React.useState(false);
  const [created, setCreated] = React.useState(0);
  const scope = React.useRef<{ readonly projectId: string | null } | null>(null);

  const setScope = React.useCallback((projectId: string | null) => {
    const entry = { projectId };
    scope.current = entry;
    return () => {
      if (scope.current === entry) scope.current = null;
    };
  }, []);

  const openNewIssue = React.useCallback(
    (next?: NewIssueRequest) => {
      if (!canCreate) return;
      setRequest((current) => ({
        seq: (current?.seq ?? 0) + 1,
        projectId: next?.projectId ?? scope.current?.projectId ?? activeProjectId,
        state: next?.state ?? null,
      }));
      setOpen(true);
    },
    [canCreate, activeProjectId],
  );

  const onCreated = React.useCallback((input: { readonly title: string }) => {
    setCreated((count) => count + 1);
    toastManager.add({ type: "success", title: "Issue created", description: input.title });
  }, []);

  const value = React.useMemo<NewIssueAction>(
    () => ({
      openNewIssue,
      canCreate,
      unavailableReason: canCreate ? null : ISSUE_CREATION_NEEDS_WRITE,
      created,
      setScope,
    }),
    [openNewIssue, canCreate, created, setScope],
  );

  return (
    <NewIssueContext.Provider value={value}>
      {children}
      {request === null || !canCreate ? null : (
        <NewIssueDialog
          key={request.seq}
          open={open}
          onOpenChange={setOpen}
          projects={newIssueProjects(writable, request.state)}
          projectId={request.projectId}
          state={request.state}
          onCreated={onCreated}
        />
      )}
    </NewIssueContext.Provider>
  );
}
