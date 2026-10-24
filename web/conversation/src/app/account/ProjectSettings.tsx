import { ProjectIconSettings } from "./ProjectIconSettings.tsx";
import { SlugSettings } from "./Organization.tsx";
import { ModelSelectionSettings } from "./ModelSelectionSettings.tsx";
import React from "react";
import { CheckIcon, CircleIcon, PlayIcon } from "lucide-react";

import { SpritesCard } from "./SpritesCard.tsx";
import { ProjectSpriteControls } from "./ProjectSpriteControls.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { useMediaQuery } from "../../hooks/useMediaQuery.ts";

import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Alert, AlertTitle } from "../../components/ui/alert.tsx";
import { AlertDialog, AlertDialogTrigger, AlertDialogPopup, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter, AlertDialogClose } from "../../components/ui/alert-dialog.tsx";
import { useRefreshBootstrap } from "../client.ts";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { hubPath } from "../../runtime/basePath.ts";
import { Textarea } from "../../components/ui/textarea.tsx";
import type { ObservedPolicy, PolicyApproval, ProjectIntegration, Onboarding, WorkflowState } from "../../contracts/account.ts";
import {
  SettingsRow,
  SettingsSection,
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

export function saveMessage(error: AccountError | null): string | null {
  if (error === null) return null;
  if (error.isConflict) {
    return "Not saved. Somebody else changed these settings. Values re-read; check and save again.";
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

function LaneMarker({ state, initial = false }: { readonly state: WorkflowState; readonly initial?: boolean }) {
  if (initial) return <PlayIcon aria-label="Initial" className="size-3 fill-current text-muted-foreground" />;
  if (state.terminal) return <CheckIcon aria-label="Terminal" className="size-3 text-muted-foreground" />;
  return <CircleIcon aria-label={state.dispatchable ? "Dispatchable" : "Holding"} className={state.dispatchable ? "size-2 fill-primary text-primary" : "size-2 text-muted-foreground"} />;
}

function LaneDetails({ state }: { readonly state: WorkflowState }) {
  return <div className="space-y-2 p-1 text-xs">
    <div className="flex items-center gap-1.5"><LaneMarker state={state} /><span className="font-medium">{state.name}</span><span className="text-muted-foreground">· {state.terminal ? "Terminal" : state.dispatchable ? "Dispatchable" : "Holding"}</span></div>
    <p className="text-muted-foreground">Can move to</p>
    <ul aria-label="Allowed transitions" className="flex flex-wrap gap-1">{(state.transitions ?? []).map((name) => <li key={name}><Badge variant="secondary">{name}</Badge></li>)}</ul>
  </div>;
}

export function WorkflowSettings({
  integration,
  canManage,
  saving,
  error,
  onSave,
  policy = null,
  observed = [],
}: {
  readonly policy?: PolicyApproval | null;
  readonly observed?: readonly ObservedPolicy[];
  readonly integration: ProjectIntegration;
  readonly canManage: boolean;
  readonly saving: boolean;
  readonly error: string | null;
  readonly onSave: (markdown: string) => void;
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
  const phone = useMediaQuery("max-sm");
  const [selected, setSelected] = React.useState<string | null>(null);
  const selectedState = states.find((state) => state.name === selected);
  const repository = integration.checkout_repository || integration.repository;
  const source = integration.workflow_source || "detent.yaml";
  const revision = integration.workflow_source_revision || "HEAD";
  const sourceHref = repository ? `https://github.com/${repository}/blob/${encodeURIComponent(revision)}/${source.split("/").map(encodeURIComponent).join("/")}` : undefined;
  return (
    <SettingsSection title="Lanes" headerAction={<div className="flex flex-wrap items-center justify-end gap-3 text-xs text-muted-foreground">
      <span className="inline-flex items-center gap-1"><CircleIcon aria-hidden className="size-2 fill-primary text-primary" />Dispatchable</span>
      <span className="inline-flex items-center gap-1"><CircleIcon aria-hidden className="size-2" />Holding</span>
      <span className="inline-flex items-center gap-1"><CheckIcon aria-hidden className="size-3" />Terminal</span>
    </div>}>
      <div className="space-y-3 p-3 sm:p-4">
        <div className="flex flex-wrap gap-1.5" aria-label="Workflow lanes">
          {states.map((state, index) => {
            const chip = <Button size="xs" variant="outline" aria-pressed={selected === state.name} onClick={phone ? () => setSelected(selected === state.name ? null : state.name) : undefined}><LaneMarker state={state} initial={index === 0} />{state.name}</Button>;
            return phone ? <React.Fragment key={state.name}>{chip}</React.Fragment> : <Tooltip key={state.name} open={selected === state.name} onOpenChange={(open) => setSelected(open ? state.name : null)}>
              <TooltipTrigger delay={150} closeOnClick={false} render={chip} />
              <TooltipPopup role="tooltip" side="bottom" align="start"><LaneDetails state={state} /></TooltipPopup>
            </Tooltip>;
          })}
        </div>
        {phone && selectedState ? <div role="tooltip" className="rounded-md border border-border bg-popover p-2"><LaneDetails state={selectedState} /></div> : null}
        {repositoryControlled ? <div className="flex items-center gap-2 text-xs text-muted-foreground"><StatusDot tone="ok" />Defined in {sourceHref ? <a href={sourceHref} target="_blank" rel="noreferrer" className="text-primary underline underline-offset-2">detent.yaml</a> : "detent.yaml"}</div> : null}
        {native && canManage && !repositoryControlled ? <div className="space-y-3">
          <Label htmlFor="project-workflow-definition">Workflow definition</Label>
          <Textarea id="project-workflow-definition" rows={12} className="font-mono text-xs" value={draft} disabled={saving} onChange={(event) => setDraft(event.currentTarget.value)} />
          <ControlError message={error} />
          <div className="flex flex-col gap-2 sm:flex-row">
            <Button size="sm" variant="ghost" disabled={saving || draft === stored} onClick={() => setDraft(stored)}>Cancel</Button>
            <Button size="sm" disabled={saving || draft === stored} onClick={() => onSave(draft)}>{saving ? "Saving…" : "Save workflow"}</Button>
          </div>
        </div> : null}
      </div>
      <PolicyRow policy={policy} observed={observed} />
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
      className="[&>div]:grid [&>div]:grid-cols-[minmax(0,1fr)_auto] [&>div]:items-center"
      control={<span className="inline-flex items-center gap-2 text-xs text-muted-foreground"><StatusDot tone={policy?.policy == null && observed.length === 0 ? "idle" : "ok"} />{policy?.policy == null && observed.length === 0 ? "Waiting for runner" : "Reported"}</span>}
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
  const actions = canManage && dirty ? <div className="flex items-center gap-2">
    <Button size="sm" variant="ghost" disabled={saving} onClick={() => onDraftChange(draftOf(integration))}>Discard</Button>
    <Button size="sm" disabled={saving} onClick={onSave}>{saving ? "Saving…" : "Save"}</Button>
  </div> : null;
  const indicator = (text: string, tone: "ok" | "warn" | "idle" = "idle") => <span className="inline-flex items-center gap-2 text-sm"><StatusDot tone={tone} />{text}</span>;
  const archiveControl = (label: string, value: number | null, field: "archiveCompletedAfterDays" | "archiveCancelledAfterDays") => canManage
    ? <NativeSelect aria-label={label} value={value === null ? "never" : String(value)} options={ARCHIVE_OPTIONS} disabled={saving} onValueChange={(next) => onDraftChange({ ...draft, [field]: next === "never" ? null : Number(next) })} />
    : <span className="text-sm">{value === null ? "Never" : `After ${value} days`}</span>;

  return (
    <div className="flex min-w-0 flex-col gap-8">
      {saveError === null ? null : <Alert variant="warning"><AlertTitle>{saveError}</AlertTitle></Alert>}
      {tab === "general" ? identity : null}
      {tab === "repository" ? <SettingsSection title="Repository & GitHub" variant="grouped" headerAction={actions}>
        <SettingsRow title="Repository" status={repository === "" ? indicator("Not attached") : undefined} control={repository === "" ? <div className="flex flex-wrap items-center gap-3">
          {canManage && onOpenSetup ? <Button size="sm" variant="outline" onClick={onOpenSetup}>Associate runner checkout</Button> : null}
        </div> : <PathValue value={repository} />} />
        <SettingsUnavailableGroup message={!transportAvailable ? "Hub GitHub integration is unavailable; PR landing uses the runner’s repository configuration." : repository === "" ? "Associate a runner checkout to configure repository integration." : undefined}>
          <SettingsRow title="GitHub App" status={unavailable ? undefined : indicator(installed === true ? "Installed" : installed === false ? "Not installed · new issues will not arrive" : "Installation status unavailable", installed === true ? "ok" : "warn")}
            control={!unavailable && canManage && integration.github_app_install_url ? <a className="text-sm text-primary underline underline-offset-2" href={integration.github_app_install_url} target="_blank" rel="noopener noreferrer">{installed ? "Manage installation" : "Install GitHub App"}</a> : null} />
          <SettingsRow title="New issues" status={unavailable ? undefined : indicator(waitingForApp ? "Waiting for the GitHub App" : integration.profile === "github_compatible" ? "GitHub" : installed === true ? "Enter Triage" : "Waiting for the GitHub App", automaticIntake && installed === true ? "ok" : "idle")}
            control={!unavailable ? <span className="text-sm text-muted-foreground">{automaticIntake ? "Automatic" : "Manual"}</span> : null} />
          <SettingsRow title="Existing issues" status={!unavailable && intakeHref ? <a className="text-sm text-primary underline underline-offset-2" href={intakeHref}>Import issues</a> : undefined} control={!unavailable ? <div className="flex items-center gap-3">
            <ToggleControl label="Existing issues" checked={draft.importAllowed} disabled={!canManage || saving} onCheckedChange={(importAllowed) => onDraftChange({ ...draft, importAllowed })} />
          </div> : null} />
          <SettingsRow title="Pull requests" status={unavailable ? undefined : indicator(!draft.repositoryEnabled ? "Off · lands by git push" : waitingForRepository ? "Waiting for repository binding" : waitingForApp ? "Waiting for the GitHub App" : "On", draft.repositoryEnabled && !waitingForApp && !waitingForRepository ? "ok" : "idle")}
            control={!unavailable ? <ToggleControl label="Pull requests" checked={draft.repositoryEnabled} disabled={!canManage || unbound || saving} onCheckedChange={(repositoryEnabled) => onDraftChange({ ...draft, repositoryEnabled })} /> : null} />
          <SettingsRow title="Work summaries" status={unavailable ? undefined : indicator(draft.projection === "summary" ? "On" : "Off", draft.projection === "summary" ? "ok" : "idle")} control={!unavailable ? <ToggleControl label="Work summaries" checked={draft.projection === "summary"} disabled={!canManage || unbound || saving || integration.profile === "github_compatible"} onCheckedChange={(summary) => onDraftChange({ ...draft, projection: summary ? "summary" : "disabled" })} /> : null} />
          <SettingsRow title="Authority" control={repository !== "" ? <span className="text-sm text-muted-foreground">{integration.profile === "native" ? "Detent" : "GitHub compatible"}</span> : null} />
        </SettingsUnavailableGroup>
      </SettingsSection> : null}
      {tab === "general" && integration.profile === "native" ? <SettingsSection title="Issues" variant="grouped" headerAction={actions}>
        <SettingsRow title="Archive completed issues" control={archiveControl("Archive completed issues", draft.archiveCompletedAfterDays, "archiveCompletedAfterDays")} />
        <SettingsRow title="Archive cancelled issues" control={archiveControl("Archive cancelled issues", draft.archiveCancelledAfterDays, "archiveCancelledAfterDays")} />
        <SettingsRow title="Check for duplicate issues" control={<ToggleControl label="Check for duplicate issues" checked={draft.duplicateChallenge} disabled={!canManage || saving} onCheckedChange={(duplicateChallenge) => onDraftChange({ ...draft, duplicateChallenge })} />} />
      </SettingsSection> : null}

      {tab === "workflow" ? workflow ?? <WorkflowSettings integration={integration} canManage={false} saving={false} error={null} onSave={() => undefined} policy={policy} observed={observedPolicies} /> : null}
      {tab === "models" ? models : null}
      {tab === "runners" ? <SettingsSection title="Runners & Sprites" hideTitle>
        <SettingsRow
          title="Runner routing"
          className="[&>div]:grid [&>div]:grid-cols-[minmax(0,1fr)_auto] [&>div]:items-center"
          help={{ label: "Runner routing", text: RUNNER_HELP.routing }}
          control={
            <Button size="sm" variant="outline" className="w-full sm:w-auto" onClick={onOpenFleet}>
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

function ProjectIdentitySettings({ integration, fallbackName, canManage, projectId, onSaved, onReload }: {
  readonly integration: ProjectIntegration;
  readonly fallbackName: string;
  readonly canManage: boolean;
  readonly projectId: string;
  readonly onSaved: (integration: ProjectIntegration) => void;
  readonly onReload: (integration: ProjectIntegration) => void;
}): React.ReactElement {
  const current = integration.name ?? fallbackName;
  const [name, setName] = React.useState(current);
  React.useEffect(() => setName(current), [current]);
  const api = useAccountApi();
  const refreshBootstrap = useRefreshBootstrap();
  const rename = useMutation(async (next: string) => {
    try {
      const saved = await api.renameProject({ projectId, name: next, revision: integration.revision, key: newKey() });
      onSaved(saved);
      setName(saved.name ?? next);
    } catch (cause) {
      if (cause instanceof AccountError && cause.isConflict) {
        const saved = await api.integration(projectId);
        onReload(saved);
        setName(saved.name ?? fallbackName);
      }
      throw cause;
    }
    await refreshBootstrap();
  });
  useProjectSettingsDirty(name !== current);
  return <SettingsSection title="Identity" variant="grouped">
 <ProjectIconSettings projectId={projectId} projectName={current} integration={integration} canManage={canManage} onSaved={onSaved} onReload={onReload} />
    <SettingsRow title="Project name" control={
      <div className="flex flex-col gap-2">
        {canManage ? <div className="flex min-w-0 items-center gap-2">
          <Input size="sm" aria-label="Project name" value={name} disabled={rename.pending} onChange={(event) => setName(event.currentTarget.value)} />
          <Button size="sm" disabled={rename.pending || name.trim() === "" || name.trim() === current} onClick={() => void rename.call(name.trim())}>{rename.pending ? "Saving…" : "Save name"}</Button>
        </div> : <span className="text-sm">{current}</span>}
        <ControlError message={rename.error?.isConflict ? "Changed by someone else. Reloaded; save again." : rename.error?.message ?? null} />
      </div>
    } />
    <SlugSettings projectId={projectId} canManage={canManage} embedded />
  </SettingsSection>;
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
  const canManage = (bootstrap?.actor.can_manage ?? false) && project?.can_write === true;
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
  const policy = useResource(() => api.policy(projectId), [api, projectId]);

  const [draft, setDraft] = React.useState<IntegrationDraft | null>(null);
  React.useEffect(() => {
    if (integration.value !== undefined) setDraft((current) => current ?? draftOf(integration.value!));
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
      if (cause instanceof AccountError && cause.isConflict) {
        const current = await api.integration(projectId);
        integration.set(current);
        setDraft(draftOf(current));
      }
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

  const observed = policy.value?.observed_policies ?? onboarding?.observed_policies ?? [];
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
      sprites={<><ProjectSpriteControls key={projectId} projectId={projectId} canManage={canManage && project?.can_write === true} /><SpritesCard key={`token-${projectId}`} projectId={projectId} canManage={canManage} /></>}
      identity={<ProjectIdentitySettings integration={integration.value} fallbackName={project?.name ?? ""} canManage={canManage} projectId={projectId} onSaved={integration.set} onReload={(current) => {
        integration.set(current);
        setDraft(draftOf(current));
      }} />}
      models={<ModelSelectionSettings projectId={projectId} canManage={canManage && project?.can_write === true} />}
      workflow={
        <>
          <WorkflowSettings
            policy={policy.value ?? onboarding?.policy ?? null}
            observed={observed}
            integration={integration.value}
            canManage={canManage && project?.can_write === true}
            saving={saveWorkflow.pending}
            error={saveMessage(saveWorkflow.error)}
            onSave={(markdown) => void saveWorkflow.call(markdown)}

          />
        </>
      }
    />
  );
}
