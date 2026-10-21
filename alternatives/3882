import { SlugSettings } from "./Organization.tsx";
import { ModelSelectionSettings } from "./ModelSelectionSettings.tsx";
import React from "react";

import { SpritesCard } from "./SpritesCard.tsx";
import { WorkflowRevisions } from "./WorkflowRevisions.tsx";

import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Alert, AlertTitle } from "../../components/ui/alert.tsx";
import { AlertDialog, AlertDialogTrigger, AlertDialogPopup, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter, AlertDialogClose } from "../../components/ui/alert-dialog.tsx";
import { useRefreshBootstrap } from "../client.ts";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { hubPath } from "../../runtime/basePath.ts";
import { Textarea } from "../../components/ui/textarea.tsx";
import type { ObservedPolicy, PolicyApproval, ProjectIntegration, Onboarding } from "../../contracts/account.ts";
import {
  SettingsRow,
  SettingsSection,
  SettingsWarning,
  SettingsUnavailableGroup,
} from "../settings/settingsLayout.tsx";
import { RUNNER_HELP } from "../fleet/runnerHelp.ts";
import type { ProjectSettingsTab } from "../settings/projectSettings.ts";
import { useProjectSettingsDirty } from "../settings/projectSettingsDirty.ts";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect, PathValue, StatusDot, ToggleControl } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { useMutation, useResource } from "./useResource.ts";

/** The editable half of the integration: what a save sends. */
export interface IntegrationDraft {
  readonly duplicateChallenge: boolean;
  readonly importAllowed: boolean;
  readonly projection: string;
  readonly repositoryEnabled: boolean;
  readonly archiveCompletedAfterDays: number | null;
  readonly archiveCancelledAfterDays: number | null;
}

export function draftOf(integration: ProjectIntegration): IntegrationDraft {
  return {
    duplicateChallenge: integration.duplicate_challenge ?? true,
    importAllowed: integration.manual_import_enabled ?? integration.intake === "manual",
    projection: integration.projection,
    repositoryEnabled: integration.repository_enabled,
    archiveCompletedAfterDays: integration.archive_completed_after_days,
    archiveCancelledAfterDays: integration.archive_cancelled_after_days,
  };
}

function hasAutomaticIntake(integration: ProjectIntegration): boolean {
  return integration.profile === "native" && Boolean(integration.repository || integration.checkout_repository);
}

export function draftChanged(a: IntegrationDraft, b: IntegrationDraft): boolean {
  return (
    a.duplicateChallenge !== b.duplicateChallenge ||
    a.importAllowed !== b.importAllowed ||
    a.projection !== b.projection ||
    a.repositoryEnabled !== b.repositoryEnabled ||
    a.archiveCompletedAfterDays !== b.archiveCompletedAfterDays ||
    a.archiveCancelledAfterDays !== b.archiveCancelledAfterDays
  );
}

/**
 * What a failed save says. A revision conflict is the one worth its own
 * sentence: nothing the reader typed was wrong, and the fix is to look again.
 */
export const OBSERVED_POLICY_REFRESH_MS = 30_000;

export function conflictingPolicies(observed: readonly ObservedPolicy[]): boolean {
  return observed.some((entry) => entry.conflict || entry.previously_approved) || new Set(observed.map((entry) => entry.policy.policy_id)).size > 1;
}

export function noApprovedPolicy(error: AccountError): boolean {
  return error.status === 404 || error.code === "policy_mismatch";
}

export function saveMessage(error: AccountError | null): string | null {
  if (error === null) return null;
  if (error.isConflict) {
    return "Somebody else changed these settings while this page was open. The values below have been re-read; check them and save again.";
  }
  if (error.status === 403 || error.status === 404) {
    return "Changing this project's integration needs owner or admin access.";
  }
  return error.message;
}

const ARCHIVE_OPTIONS = [7, 14, 30, 60, 90].map((days) => ({ value: String(days), label: `After ${days} days` })).concat({ value: "never", label: "Never" });

export function ProjectDeletionSettings({ projectName, pending, error, onDelete, onDismiss }: {
  readonly projectName: string;
  readonly pending: boolean;
  readonly error: string | null;
  readonly onDelete: () => void;
  readonly onDismiss: () => void;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const [confirmation, setConfirmation] = React.useState("");
  const confirmationId = React.useId();
  const canDelete = !pending && confirmation.trim() === projectName;
  return (
    <SettingsSection title="Delete project">
      <SettingsRow title="Delete project" description="Remove this project from Detent Cloud. Active work must finish or be cancelled first."
        control={
          <AlertDialog open={open} onOpenChange={(next) => {
            if (pending) return;
            setOpen(next);
            setConfirmation("");
            onDismiss();
          }}>
            <AlertDialogTrigger render={<Button size="sm" variant="destructive-outline" />}>Delete project</AlertDialogTrigger>
            <AlertDialogPopup>
              <form onSubmit={(event) => {
                event.preventDefault();
                if (canDelete) onDelete();
              }}>
                <AlertDialogHeader>
                  <AlertDialogTitle>Delete {projectName}?</AlertDialogTitle>
                  <Alert variant="error">
                    <AlertTitle>This action cannot be undone.</AlertTitle>
                  </Alert>
                  <AlertDialogDescription render={<div />} className="text-left">
                    <ul className="list-disc space-y-2 pl-4">
                      <li>This removes {projectName} from Cloud and deletes its secrets and access grants.</li>
                      <li>Issues, runs, Changes, conversations and stored artifacts are retained for audit but become inaccessible. Its name remains reserved, and retained storage still counts toward usage.</li>
                      <li>The GitHub repository, its issues and PRs, other projects and shared runners are preserved.</li>
                      <li>First, finish or cancel active work, close workspaces, finish imports and GitHub writes, and remove provisioned Sprite pool members.</li>
                    </ul>
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <div className="space-y-3 px-6 pb-6">
                  <Label htmlFor={confirmationId} className="inline-block leading-normal">
                    To confirm, type <span className="font-mono">{projectName}</span> below
                  </Label>
                  <Input id={confirmationId} value={confirmation} onChange={(event) => setConfirmation(event.currentTarget.value)} disabled={pending} autoComplete="off" />
                  <ControlError message={error} />
                </div>
                <AlertDialogFooter>
                  <AlertDialogClose disabled={pending} render={<Button variant="outline" />}>Cancel</AlertDialogClose>
                  <Button type="submit" variant="destructive" disabled={!canDelete}>{pending ? "Deleting…" : "Delete project"}</Button>
                </AlertDialogFooter>
              </form>
            </AlertDialogPopup>
          </AlertDialog>
        }
      />
    </SettingsSection>
  );
}

export function WorkflowSettings({
  integration,
  canManage,
  saving,
  error,
  onSave,
  revisions,
}: {
  readonly integration: ProjectIntegration;
  readonly canManage: boolean;
  readonly saving: boolean;
  readonly error: string | null;
  readonly onSave: (markdown: string) => void;
  readonly revisions?: React.ReactNode;
}): React.ReactElement {
  const states = integration.states ?? [];
  const stored = integration.workflow_markdown ?? `---
tracker:
  kind: hub_native
  lanes: ${JSON.stringify(states.map((state) => ({ name: state.name, role: state.terminal ? "terminal" : state.dispatchable ? "active" : "holding" })))}
server:
  kanban:
    allowed_transitions: ${JSON.stringify(Object.fromEntries(states.map((state) => [state.name, state.transitions])))}
---
Complete the assigned work.
`;
  const [draft, setDraft] = React.useState(stored);
  React.useEffect(() => {
    setDraft(stored);
  }, [stored]);
  useProjectSettingsDirty(draft !== stored);
  const native = integration.profile === "native";
  const repositoryControlled = integration.authority?.workflow === "repository";
  return (
    <SettingsSection title="Workflow">
      <SettingsRow
        title="Project states"
        description={repositoryControlled
          ? <span className="[overflow-wrap:anywhere]">Controlled by {integration.checkout_repository || integration.repository || "the repository"}: {integration.workflow_source} at revision {integration.workflow_source_revision}. Edit the repository definition; runners report the updated configuration.</span>
          : native ? "Author YAML frontmatter and agent instructions in Markdown using the repository workflow schema. Review states and allowed transitions before saving." : "This workflow is owned by the source tracker."}
        status={
          <ul className="text-sm [overflow-wrap:anywhere]">
            {(integration.states ?? []).map((state, index) => (
              <li key={state.name}>
                {state.name}{index === 0 ? " · Initial" : ""} · {state.terminal ? "Terminal" : state.dispatchable ? "Dispatchable" : "Nondispatchable"}{state.operator_only ? " · Operator only" : ""}
              </li>
            ))}
          </ul>
        }
      >
        {native && canManage && !repositoryControlled ? (
          <div className="flex flex-col gap-2 pb-3">
            <Textarea aria-label="Workflow definition" rows={12} className="font-mono text-xs" value={draft} disabled={saving} onChange={(event) => setDraft(event.currentTarget.value)} />
            <ControlError message={error} />
            <div>
              <Button size="sm" disabled={saving || draft === stored} onClick={() => {
                onSave(draft);
              }}>{saving ? "Saving…" : "Save workflow"}</Button>
            </div>
          </div>
        ) : null}
      </SettingsRow>
      {revisions}
    </SettingsSection>
  );
}

export function PolicyRow({
  policy,
  observed,
}: {
  readonly policy: PolicyApproval | null;
  readonly observed: readonly ObservedPolicy[];
}): React.ReactElement {
  return (
    <SettingsRow
      title="Repository configuration"
      description="The runner reads detent.yaml and WORKFLOW.md from the repository."
      status={policy === null && observed.length === 0 ? "Waiting for runner configuration" : "Configuration reported"}
    />
  );
}

export function ProjectSettingsView({
  integration,
  policy,
  canManage,
  draft,
  onDraftChange,
  onSave,
  observedPolicies,
  saving,
  saveError,
  onOpenFleet,
  onOpenSetup,
  intakeHref,
  sprites,
  workflow,
  deletion,
  tab,
  identity,
  models,
}: {
  readonly integration: ProjectIntegration;
  readonly policy: PolicyApproval | null;
  readonly canManage: boolean;
  readonly draft: IntegrationDraft;
  readonly onDraftChange: (draft: IntegrationDraft) => void;
  readonly onSave: () => void;
  readonly observedPolicies: readonly ObservedPolicy[];
  readonly saving: boolean;
  readonly saveError: string | null;
  readonly onOpenFleet: () => void;
  readonly onOpenSetup?: () => void;
  readonly intakeHref?: string;
  readonly sprites?: React.ReactNode;
  readonly workflow?: React.ReactNode;
  readonly deletion?: React.ReactNode;
  readonly tab: ProjectSettingsTab;
  readonly identity?: React.ReactNode;
  readonly models?: React.ReactNode;
}): React.ReactElement {
  const unbound = (integration.repository ?? "").length === 0;
  const repository = integration.checkout_repository || integration.repository || "";
  const transportAvailable = integration.github_transport_available !== false;
  const automaticIntake = hasAutomaticIntake(integration);
  const installed = integration.github_app_installed;
  const waitingForApp = automaticIntake && installed === false;
  const unavailable = repository === "" || !transportAvailable;
  const waitingForRepository = unbound && repository !== "";
  const dirty = draftChanged(draft, draftOf(integration));

  return (
    <div className="flex min-w-0 flex-col gap-8">
      {(tab === "general" || tab === "repository") && canManage ? (
        <div className="flex justify-end">
          <Button size="sm" disabled={!dirty || saving} onClick={onSave}>
            {saving ? "Saving…" : "Save changes"}
          </Button>
        </div>
      ) : null}
      {saveError === null ? null : (
        <SettingsWarning><b className="font-semibold">This change was not saved.</b> {saveError}</SettingsWarning>
      )}
      {tab === "general" ? identity : null}
      {tab === "repository" ? (
      <SettingsSection title="Repository">
        <SettingsRow
          title="Repository"
          description="The GitHub repository this project's work lands in."
          control={
            repository === "" ? (
              canManage && onOpenSetup ? (
                <Button size="sm" variant="outline" onClick={onOpenSetup}>Associate runner checkout</Button>
              ) : (
                <span className="text-sm text-muted-foreground">Not attached</span>
              )
            ) : (
              <PathValue value={repository} />
            )
          }
        />
        <SettingsUnavailableGroup message={!transportAvailable
          ? "Hub GitHub integration is unavailable; PR landing uses the runner’s repository configuration."
          : repository === "" ? "Associate a runner checkout to configure repository integration." : undefined}>
          {integration.profile === "native" && installed !== undefined && !unavailable ? (
            <SettingsRow
              title="GitHub App"
              status={<span className="inline-flex items-center gap-2 [overflow-wrap:anywhere]">
                <StatusDot tone={installed ? "ok" : "warn"} />
                {installed ? `Installed on ${repository}.` : "Not installed. New issues will not reach this project until it is."}
              </span>}
              control={integration.github_app_install_url ? <a className="text-sm text-primary underline underline-offset-2" href={integration.github_app_install_url} target="_blank" rel="noopener noreferrer">
                {installed ? "Manage installation" : "Install the Detent Cloud GitHub App"}
              </a> : null}
            />
          ) : null}
          <SettingsRow
            title="New issues"
            help={{ label: "New issues", text: "Detent receives GitHub's issue events through the App. There is nothing to turn on here; install or remove the App to change it." }}
            status={<span className="inline-flex items-center gap-2 [overflow-wrap:anywhere]">
              <StatusDot tone={automaticIntake && installed === true && !unavailable ? "ok" : "idle"} />
              {integration.profile === "github_compatible" ? "GitHub owns these issues until cutover."
                : unavailable ? "New GitHub issues are not reaching this project."
                : waitingForApp ? "Waiting for the GitHub App."
                : installed === true ? `Every issue opened on ${repository} enters Triage.` : "GitHub App installation status is unavailable."}
            </span>}
            control={!unavailable ? <span className="text-sm text-muted-foreground">{automaticIntake ? "Automatic" : "Manual"}</span> : null}
          />
          <SettingsRow
            title="Existing issues"
            help={{ label: "Existing issues", text: "Let members import older GitHub issues and their discussion into Triage. Turning this off prevents manual imports and keeps previously imported work." }}
            description={<>Let members import older GitHub issues and their discussion into Triage. Import from {intakeHref ? <a className="text-primary underline underline-offset-2" href={intakeHref}>GitHub issue import in project setup</a> : "GitHub issue import in project setup"}.</>}
            control={!unavailable ? <ToggleControl label="Existing issues" checked={draft.importAllowed} disabled={!canManage || saving}
              onCheckedChange={(importAllowed) => onDraftChange({ ...draft, importAllowed })} /> : null}
          />
          <SettingsRow
            title="Pull requests"
            help={{ label: "Pull requests", text: "Enabling this permits repository and pull request operations, including reading, creating and merging pull requests subject to GitHub permissions and branch protections. Disabling it stops these operations but keeps the immutable repository binding. Existing issues and work summaries are separate settings." }}
            status={<span className="inline-flex items-center gap-2 [overflow-wrap:anywhere]">
              <StatusDot tone={integration.repository_enabled && !unavailable && !waitingForApp && !waitingForRepository ? "ok" : "idle"} />
              {!transportAvailable ? "Work lands through the runner’s repository configuration."
                : !integration.repository_enabled ? "Off. Work lands by git push; no pull requests are created."
                : waitingForRepository || repository === "" ? "Waiting for repository binding."
                : waitingForApp ? "Waiting for the GitHub App." : "Detent opens, updates and merges pull requests on this repository."}
            </span>}
            control={!unavailable ? <ToggleControl label="Pull requests" checked={draft.repositoryEnabled} disabled={!canManage || unbound || saving}
              onCheckedChange={(repositoryEnabled) => onDraftChange({ ...draft, repositoryEnabled })} /> : null}
          />
          <SettingsRow
            title="Work summaries"
            help={{ label: "Work summaries", text: "A summary of each work item is posted to its linked GitHub issue. Turning this off stops new summary writes and leaves existing GitHub content in place. Summary requires native authority; it does not transfer field ownership to GitHub." }}
            status={<span className="inline-flex items-center gap-2 [overflow-wrap:anywhere]">
              <StatusDot tone={integration.profile === "native" && integration.projection === "summary" && !unavailable && !waitingForApp && !waitingForRepository ? "ok" : "idle"} />
              {integration.profile === "github_compatible" ? "Summary projection requires Detent authority."
                : integration.projection !== "summary" ? "Off. Nothing is written back to GitHub issues."
                : unavailable ? "Work summaries are not reaching GitHub."
                : waitingForRepository ? "Waiting for repository binding."
                : waitingForApp ? "Waiting for the GitHub App." : "A summary of each work item is posted to its linked GitHub issue."}
            </span>}
            control={!unavailable ? <ToggleControl label="Work summaries" checked={draft.projection === "summary"} disabled={!canManage || unbound || saving || integration.profile === "github_compatible"}
              onCheckedChange={(summary) => onDraftChange({ ...draft, projection: summary ? "summary" : "disabled" })} /> : null}
          />
          <SettingsRow
            title="Authority"
            help={{
              label: "Authority",
              text: "The native profile gives Detent ownership of issue title, body, discussion, dependencies, authors, workflow, labels, assignees and priority; the github_compatible profile gives GitHub ownership of those fields. Detent always owns scheduling, progress and native approval. Source timestamps retain their source, repository policy comes from the trusted repository revision, and GitHub controls merge protections. Intake and projection do not change these owners.",
            }}
            description="Who manages this project’s issue fields."
            status={
              <details className="text-sm">
                <summary className="cursor-pointer">Field ownership</summary>
                <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
                  {Object.entries(integration.authority ?? {}).map(([field, owner]) => (
                    <React.Fragment key={field}>
                      <dt className="break-words">{field.replaceAll("_", " ")}</dt>
                      <dd className="break-words">{owner}</dd>
                    </React.Fragment>
                  ))}
                </dl>
              </details>
            }
            control={<span className="text-sm text-muted-foreground">{integration.profile === "native" ? "Detent" : "GitHub compatible"}</span>}
          />
        </SettingsUnavailableGroup>
      </SettingsSection>

      ) : null}

      {tab === "general" && integration.profile === "native" ? <SettingsSection title="Issue filing">
        <SettingsRow
          title="Check for duplicate issues"
          description="Ask filers to review likely matches before creating a new issue."
          control={<ToggleControl
            label="Check for duplicate issues"
            checked={draft.duplicateChallenge}
            disabled={!canManage || saving}
            onCheckedChange={(checked) => onDraftChange({ ...draft, duplicateChallenge: checked })}
          />}
        />
      </SettingsSection> : null}

      {tab === "general" && integration.profile === "native" ? <SettingsSection title="Archiving">
        <SettingsRow
          title="Archive completed issues"
          description="Done issues leave the board, lists and counts after this long. They stay in search and can be restored."
          control={<NativeSelect
            aria-label="Archive completed issues"
            value={draft.archiveCompletedAfterDays === null ? "never" : String(draft.archiveCompletedAfterDays)}
            options={ARCHIVE_OPTIONS}
            disabled={!canManage || saving}
            onValueChange={(value) => onDraftChange({ ...draft, archiveCompletedAfterDays: value === "never" ? null : Number(value) })}
          />}
        />
        <SettingsRow
          title="Archive cancelled issues"
          description="Cancelled issues follow the same rule on a shorter clock."
          control={<NativeSelect
            aria-label="Archive cancelled issues"
            value={draft.archiveCancelledAfterDays === null ? "never" : String(draft.archiveCancelledAfterDays)}
            options={ARCHIVE_OPTIONS}
            disabled={!canManage || saving}
            onValueChange={(value) => onDraftChange({ ...draft, archiveCancelledAfterDays: value === "never" ? null : Number(value) })}
          />}
        />
      </SettingsSection> : null}

      {tab === "workflow" ? <>
        <SettingsSection title="Execution"><PolicyRow policy={policy} observed={observedPolicies} /></SettingsSection>
        {workflow}
      </> : null}
      {tab === "models" ? models : null}
      {tab === "runners" ? <SettingsSection title="Execution">
        <SettingsRow
          title="Runner routing"
          help={{ label: "Runner routing", text: RUNNER_HELP.routing }}
          description="Choose which runners handle this project."
          control={
            <Button size="sm" variant="outline" onClick={onOpenFleet}>
              Open fleet
            </Button>
          }
        />
        {sprites}
      </SettingsSection> : null}
      {tab === "danger" ? deletion : null}
    </div>
  );
}

export function ProjectSettingsRoute({ projectId, tab, onboarding, onNavigate }: {
  readonly projectId: string;
  readonly tab: ProjectSettingsTab;
  readonly onboarding?: Onboarding;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const refreshBootstrap = useRefreshBootstrap();
  const project = bootstrap?.projects.find((candidate) => candidate.id === projectId) ?? null;
  const canManage = bootstrap?.actor.can_manage ?? false;
  const remove = useMutation(async () => {
    if (project === null) return;
    await api.deleteProject({ projectId, name: project.name, key: newKey() });
    try {
      await refreshBootstrap();
    } catch {
      globalThis.location.assign(hubPath("/work"));
      return;
    }
    if (onNavigate) onNavigate("/work");
    else globalThis.location.assign(hubPath("/work"));
  });

  const integration = useResource<ProjectIntegration>(
    () => api.integration(projectId),
    [api, projectId],
  );
  const policy = useResource<PolicyApproval | null>(
    () =>
      api.policy(projectId).catch((cause: unknown) => {
        if (cause instanceof AccountError && noApprovedPolicy(cause)) return null;
        throw cause;
      }),
    [api, projectId],
  );

  const [draft, setDraft] = React.useState<IntegrationDraft | null>(null);
  React.useEffect(() => {
    if (integration.value !== undefined) setDraft(draftOf(integration.value));
  }, [integration.value]);

  const save = useMutation(async (next: IntegrationDraft) => {
    const current = integration.value;
    if (current === undefined) return null;
    try {
      const saved = await api.saveIntegration({
        projectId,
        key: newKey(),
        revision: current.revision,
        intake: next.importAllowed ? "manual" : "disabled",
        projection: next.projection,
        repositoryEnabled: next.repositoryEnabled,
        duplicateChallenge: next.duplicateChallenge,
        archiveCompletedAfterDays: next.archiveCompletedAfterDays,
        archiveCancelledAfterDays: next.archiveCancelledAfterDays,
      });
      integration.set({ ...current, ...saved });
      setDraft(draftOf(saved));
      return saved;
    } catch (cause) {
      // The conflict's own body carries nothing to recover from, so the screen
      // re-reads and shows the reader what is actually stored.
      if (cause instanceof AccountError && cause.isConflict) await integration.refresh();
      throw cause;
    }
  });

  const saveWorkflow = useMutation(async (markdown: string) => {
    const current = integration.value;
    if (current === undefined) return null;
    try {
      const saved = await api.saveIntegration({
        projectId,
        key: newKey(),
        revision: current.revision,
        intake: draftOf(current).importAllowed ? "manual" : "disabled",
        projection: current.projection,
        repositoryEnabled: current.repository_enabled,
        workflowMarkdown: markdown,
      });
      integration.set({ ...current, ...saved });
      globalThis.location.reload();
      return saved;
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict) await integration.refresh();
      throw cause;
    }
  });

  const observed = onboarding?.observed_policies ?? [];
  const loadOlder = useMutation(async () => {
    const current = policy.value;
    if (!current?.history_next) return null;
    const page = await api.policy(projectId, current.history_next);
    if (page.policy.policy_id !== current.policy.policy_id) {
      await policy.refresh();
      return null;
    }
    policy.set({ ...page, history: [...(current.history ?? []), ...(page.history ?? [])] });
    return page;
  });
  // A runner reports a changed policy whenever the repository changes, so the
  // page looks again while it is open instead of only on the next visit.
  React.useEffect(() => {
    const timer = globalThis.setInterval(() => {
      void policy.refresh();
    }, OBSERVED_POLICY_REFRESH_MS);
    return () => globalThis.clearInterval(timer);
  }, [policy.refresh]);

  useProjectSettingsDirty(draft !== null && integration.value !== undefined && draftChanged(draft, draftOf(integration.value)));

  if (integration.error?.isAccessError) {
    return <SettingsSection title="Not available"><SettingsRow title="You do not have access to this project" description="Ask an owner or admin for a grant on it, or pick a different project." /></SettingsSection>;
  }
  if (integration.value === undefined || draft === null) {
    return integration.error === null ? (
      <div role="status" aria-label="Loading project settings" className="space-y-3">
        <Skeleton className="h-5 w-32" /><Skeleton className="h-24 w-full" /><Skeleton className="h-24 w-full" />
      </div>
    ) : <p role="status" className="text-sm text-muted-foreground">{integration.error.message}</p>;
  }

  return (
    <ProjectSettingsView
      tab={tab}
      integration={integration.value}
      policy={policy.value ?? null}
      canManage={canManage}
      draft={draft}
      onDraftChange={setDraft}
      onSave={() => void save.call(draft)}
      observedPolicies={observed}
      saving={save.pending}
      saveError={saveMessage(save.error)}
      deletion={canManage && project?.can_write === true ? <ProjectDeletionSettings key={projectId} projectName={project.name} pending={remove.pending} error={remove.error?.message ?? null} onDelete={() => void remove.call()} onDismiss={remove.clearError} /> : null}
      onOpenFleet={() => onNavigate?.("/settings/runners")}
      intakeHref={`/projects/${projectId}/setup`}
      onOpenSetup={onNavigate ? () => onNavigate(`/projects/${projectId}/setup`) : undefined}
      sprites={<><SpritesCard key={projectId} projectId={projectId} canManage={canManage} /></>}
      identity={<SlugSettings projectId={projectId} canManage={canManage && project?.can_write === true} />}
      models={<ModelSelectionSettings projectId={projectId} canManage={canManage && project?.can_write === true} />}
      workflow={
        <>
          <WorkflowSettings
            integration={integration.value}
            canManage={canManage && project?.can_write === true}
            saving={saveWorkflow.pending}
            error={saveMessage(saveWorkflow.error)}
            onSave={(markdown) => void saveWorkflow.call(markdown)}
            revisions={
              <WorkflowRevisions
                policy={policy.value ?? onboarding?.policy ?? null}
                observed={observed}
                repository={integration.value.checkout_repository || integration.value.repository || ""}
                error={loadOlder.error?.message ?? policy.error?.message ?? null}
                onLoadOlder={() => void loadOlder.call()}
                loadingOlder={loadOlder.pending}
              />
            }
          />
        </>
      }
    />
  );
}
