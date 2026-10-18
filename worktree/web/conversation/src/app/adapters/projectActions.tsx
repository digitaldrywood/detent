import React from "react";

import type { Action } from "../../contracts/work.ts";
import type { ProjectScript } from "../../contracts/ui.ts";
import type {
  NewProjectScriptInput,
  ProjectScriptActionResult,
} from "../../components/projectScriptEditor.tsx";
import {
  buildProjectScript,
  commandForProjectScript,
  nextProjectScriptId,
} from "../../projectScripts.ts";
import { settlePromise } from "../../runtime/state/runtime.ts";
import { newWorkKey, type WorkHttp } from "../work/lib/workHttp.ts";
import { useWorkHttp } from "../work/lib/useWork.ts";
import { registerProjectActionKeybindings, type ProjectActionBinding } from "./keybindings.ts";

// --- The project's real actions ---------------------------------------------

export interface MappedProjectAction {
  readonly action: Action;
  readonly script: ProjectScript;
  /** `script.<slug>.run`, or null for a slug that cannot carry a command. */
  readonly command: string | null;
}

export function mapProjectActions(actions: readonly Action[]): readonly MappedProjectAction[] {
  const taken: string[] = [];
  return actions.map((action) => {
    const slug = nextProjectScriptId(action.name, taken);
    taken.push(slug);
    const previewUrl = action.preview_url ?? "";
    return {
      action,
      script: buildProjectScript(slug, {
        name: action.name,
        command: action.command,
        icon: action.icon,
        runOnWorktreeCreate: action.run_on_worktree_creation,
        previewUrl: previewUrl.length > 0 ? previewUrl : null,
        autoOpenPreview: action.open_preview,
      }),
      command: commandForProjectScript(slug),
    };
  });
}

/** Which of a project's actions to run when a worktree is created, in order. */
export function worktreeCreationActions(
  mapped: readonly MappedProjectAction[],
): readonly MappedProjectAction[] {
  return mapped.filter((entry) => entry.action.run_on_worktree_creation);
}

export interface ProjectActionsHandle {
  /** What the copied control lists, in authoring order. */
  readonly scripts: readonly ProjectScript[];
  readonly mapped: readonly MappedProjectAction[];
  readonly loading: boolean;
  /** A failed read, as a sentence. A failed write is reported through its result. */
  readonly error: string | null;
  readonly reload: () => void;
  readonly add: (input: NewProjectScriptInput) => Promise<ProjectScriptActionResult>;
  readonly update: (
    scriptId: string,
    input: NewProjectScriptInput,
  ) => Promise<ProjectScriptActionResult>;
  readonly remove: (scriptId: string) => Promise<ProjectScriptActionResult>;
  /** The action a `script.<id>.run` command names, for the keybinding dispatch. */
  readonly bySlug: (scriptId: string) => MappedProjectAction | null;
}

const NO_SCRIPTS: readonly ProjectScript[] = [];
const NO_MAPPED: readonly MappedProjectAction[] = [];

const IDLE: ProjectActionsHandle = {
  scripts: NO_SCRIPTS,
  mapped: NO_MAPPED,
  loading: false,
  error: null,
  reload: () => {},
  add: async () => rejectWithoutProject(),
  update: async () => rejectWithoutProject(),
  remove: async () => rejectWithoutProject(),
  bySlug: () => null,
};

function rejectWithoutProject(): Promise<ProjectScriptActionResult> {
  // A route with no project cannot store an action. The dialog shows the
  // message rather than closing on a write that never happened.
  return settlePromise(() => {
    throw new Error("This conversation has no project to store an action on.");
  });
}

/**
 * The project's actions, read once per project and kept fresh by the write
 * that changed them.
 *
 * There is no event subscription here yet, and that is a deliberate gap rather
 * than an oversight: §18.12 puts `action_run.<status>` on the project stream
 * for *runs*, and says nothing about an action's own create, edit or delete.
 * So a colleague's newly authored action appears on the next load of the
 * route.
 */
export function useProjectActions(projectId: string | null): ProjectActionsHandle {
  const http = useWorkHttp();
  const [actions, setActions] = React.useState<readonly Action[] | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [nonce, setNonce] = React.useState(0);
  const reload = React.useCallback(() => setNonce((value) => value + 1), []);

  React.useEffect(() => {
    if (projectId === null) {
      setActions(null);
      setError(null);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    void http
      .listActions(projectId)
      .then((page) => {
        if (cancelled) return;
        setActions(page.items);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        setActions(null);
        setError(cause instanceof Error ? cause.message : String(cause));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [http, projectId, nonce]);

  const mapped = React.useMemo(
    () => (actions === null ? NO_MAPPED : mapProjectActions(actions)),
    [actions],
  );

  // The chords, into the registry the keystroke listener and the menu labels
  // read (`adapters/keybindings.ts`). A replace on every change, because a
  // deleted action has to stop resolving; unmounting clears the registry so a
  // route with no project holds no chords open.
  React.useEffect(() => {
    const bindings: ProjectActionBinding[] = [];
    for (const entry of mapped) {
      const chord = entry.action.keybinding ?? "";
      if (entry.command === null || chord.length === 0) continue;
      bindings.push({ command: entry.command, chord });
    }
    registerProjectActionKeybindings(bindings);
    return () => {
      registerProjectActionKeybindings([]);
    };
  }, [mapped]);

  const bySlug = React.useCallback(
    (scriptId: string) => mapped.find((entry) => entry.script.id === scriptId) ?? null,
    [mapped],
  );

  return React.useMemo<ProjectActionsHandle>(() => {
    if (projectId === null) return IDLE;
    return {
      scripts: mapped.map((entry) => entry.script),
      mapped,
      loading,
      error,
      reload,
      add: (input) =>
        settlePromise(async () => {
          await writeAction(http, projectId, null, input);
          reload();
        }),
      update: (scriptId, input) =>
        settlePromise(async () => {
          const existing = mapped.find((entry) => entry.script.id === scriptId);
          if (existing === undefined) {
            throw new Error("That action is no longer in this project.");
          }
          await writeAction(http, projectId, existing.action, input);
          reload();
        }),
      remove: (scriptId) =>
        settlePromise(async () => {
          const existing = mapped.find((entry) => entry.script.id === scriptId);
          if (existing === undefined) {
            throw new Error("That action is no longer in this project.");
          }
          await http.deleteAction(projectId, existing.action.id);
          reload();
        }),
      bySlug,
    };
  }, [bySlug, error, http, loading, mapped, projectId, reload]);
}

/**
 * One create or one edit.
 *
 * The whole record goes on a patch rather than a diff of it. §18.12 requires
 * `expected_revision`, so the write is already conflict-checked, and sending
 * every field is what makes clearing a chord or a preview URL expressible —
 * an omitted field means "leave alone", which is not what an emptied input
 * says.
 */
async function writeAction(
  http: WorkHttp,
  projectId: string,
  existing: Action | null,
  input: NewProjectScriptInput,
): Promise<void> {
  const fields = {
    name: input.name,
    command: input.command,
    keybinding: input.keybinding,
    icon: input.icon,
    previewUrl: input.previewUrl,
    // §18.7: the switch is disabled, so this is always the stored value and
    // never something the reader just turned on. It is still sent, because the
    // hub stores and echoes it and an action authored today should not have to
    // be re-authored when the Browser surface lands.
    openPreview: input.autoOpenPreview,
    runOnWorktreeCreation: input.runOnWorktreeCreate,
  };
  if (existing === null) {
    await http.createAction({ projectId, key: newWorkKey("action"), ...fields });
    return;
  }
  await http.updateAction({
    projectId,
    actionId: existing.id,
    key: newWorkKey("action"),
    expectedRevision: existing.revision,
    ...fields,
  });
}
