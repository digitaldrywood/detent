import { ModelSelectionSettings } from "./ModelSelectionSettings.tsx";
import React from "react";

import { SpritesCard } from "./SpritesCard.tsx";
import { SpritePoolCard } from "./SpritePoolCard.tsx";
import { WorkflowRevisions } from "./WorkflowRevisions.tsx";

import { Button } from "../../components/ui/button.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import type { ObservedPolicy, PolicyApproval, ProjectIntegration } from "../../contracts/account.ts";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
  SettingsWarning,
  SettingsUnavailableGroup,
} from "../settings/settingsLayout.tsx";
import { RUNNER_HELP } from "../fleet/runnerHelp.ts";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect, PathValue, StatusDot, ToggleControl } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { parsePolicyDescriptor, POLICY_INSPECT_COMMAND } from "./Setup.tsx";
import { useMutation, useResource } from "./useResource.ts";

/** The editable half of the integration: what a save sends. */
export interface IntegrationDraft {
  readonly importAllowed: boolean;
  readonly projection: string;
  readonly repositoryEnabled: boolean;
  readonly archiveCompletedAfterDays: number | null;
  readonly archiveCancelledAfterDays: number | null;
}

export function draftOf(integration: ProjectIntegration): IntegrationDraft {
  return {
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
/**
 * Whether a failed `GET .../policy` means "nothing is approved" rather than
 * "something went wrong". `404` is a project with no policy row; `409
 * policy_mismatch` is a stored approval that no longer describes what the host
 * resolves. Neither is an error the reader can act on except by approving.
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
  const native = integration.profile === "native";
  const repositoryControlled = integration.authority?.workflow === "repository";
  return (
    <SettingsSection title="Workflow">
      <SettingsRow
        title="Project states"
        description={repositoryControlled
          ? <span className="[overflow-wrap:anywhere]">Controlled by {integration.checkout_repository || integration.repository || "the repository"}: {integration.workflow_source} at revision {integration.workflow_source_revision}. Edit the repository definition, then approve its new policy.</span>
          : native ? "Author YAML frontmatter and agent instructions in Markdown using the repository workflow schema. Review states and allowed transitions before saving; execution changes require policy approval." : "This workflow is owned by the source tracker."}
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
  canManage,
  onApprove,
  onApprovePasted,
  approving,
  error,
  workflowRevisionsShown = false,
}: {
  readonly policy: PolicyApproval | null;
  /** Descriptors runners resolved and could not run, newest first. */
  readonly observed: readonly ObservedPolicy[];
  readonly canManage: boolean;
  readonly onApprove: (policyId: string) => void;
  readonly onApprovePasted: (text: string) => void;
  readonly approving: boolean;
  readonly error: string | null;
  readonly workflowRevisionsShown?: boolean;
}): React.ReactElement {
  const [pasting, setPasting] = React.useState(false);
  const [pasted, setPasted] = React.useState("");
  const conflict = conflictingPolicies(observed);
  const shared = policy?.policy.configuration !== undefined;
  const description =
    conflict
      ? shared
        ? "Runners report conflicting configurations. Load the approved shared configuration on these runners; approving their old reports will not converge the project."
        : "Runners report conflicting configurations. Choose one shared project configuration with the intended planning, validation and automatic promotion settings, then approve its inspected descriptor once."
      : observed.length > 0
      ? canManage
        ? "Review and approve the reported policy to resume work."
        : "An owner or admin must approve the reported policy to resume work."
      : policy === null
        ? canManage
          ? "Start a runner or paste a policy to approve it."
          : "An owner or admin must approve a policy before work can run."
        : shared
          ? "Authorized runners consume this approved project configuration."
          : "Approved for execution.";
  return (
    <SettingsRow
      title={shared ? "Shared project configuration" : "Repository policy"}
      help={{
        label: shared ? "Shared project configuration" : "Repository policy",
        text: shared || observed.some((entry) => entry.policy.configuration)
          ? "Cloud stores the exact approved descriptor. Runners with a supplied definition must load matching shared files from their configured source, retaining detent.local.yaml and WORKFLOW.local.md. Preview selected_policy with local_project_configuration, then use apply_local_project_policy after current work settles. A configured workflow_ref requires committed shared files; source_revision is an authored digest, not a Git ref. Credentials, paths, capacity, isolation setup and runner routing stay on each host. Execution requires an exact match to approval."
          : "The runner reports the policy descriptor it resolves from the trusted repository revision, including detent.yaml and WORKFLOW.md. Repository changes or a runner upgrade can change that descriptor and make the prior approval stale. Execution is blocked when the runner’s resolved policy does not match an approved descriptor; an owner or admin must approve the current policy.",
      }}
      description={description}
      status={
        <>
          <span className={observed.length > 0 || policy === null ? "text-warning-foreground" : undefined}>
            {observed.length > 0 ? "Needs approval" : policy === null ? "Not approved" : "Approved"}
          </span>
          {policy === null ? null : (
            <details className="mt-1 text-sm">
              <summary className="cursor-pointer">Approval details</summary>
              <p className="mt-2 break-words">Approved by {policy.approved_by} on {policy.approved_at}</p>
              <pre className="mt-2 whitespace-pre-wrap break-all font-mono text-sm">{JSON.stringify(policy.policy, null, 2)}</pre>
            </details>
          )}
          {observed.filter((entry) => !workflowRevisionsShown || !entry.policy.workflow || entry.previously_approved).map((entry) => (
            <div key={entry.policy.policy_id} className="mt-2 flex min-w-0 flex-wrap items-start gap-2">
              <details className="min-w-0 flex-1 text-sm">
                <summary className="cursor-pointer">Review policy from {entry.runner_ids?.join(", ") ?? entry.runner_id}</summary>
                <p className="mt-2">Reported at {entry.observed_at}</p>
                <pre className="mt-2 whitespace-pre-wrap break-all font-mono text-sm">{JSON.stringify(entry.policy, null, 2)}</pre>
              </details>
              {entry.previously_approved ? <span>Previously approved configuration</span> : null}
              {canManage && !conflict ? (
                <Button
                  size="xs"
                  disabled={approving}
                  aria-label={`Approve updated policy ${entry.policy.policy_id}`}
                  onClick={() => onApprove(entry.policy.policy_id)}
                >
                  {approving ? "Approving…" : "Approve"}
                </Button>
              ) : null}
            </div>
          ))}
        </>
      }
      control={
        canManage ? (
          <div className="flex flex-col items-end gap-1.5">
            <Button size="sm" variant="ghost-muted" onClick={() => setPasting((open) => !open)}>
              {pasting ? "Cancel" : "Paste policy"}
            </Button>
            <ControlError message={error} />
          </div>
        ) : (
          <ControlError message={error} />
        )
      }
    >
      {canManage && pasting ? (
        <div className="flex flex-col gap-2 pb-3">
          <p className="text-[13px] text-muted-foreground">
            Run <code className="font-mono text-xs">{POLICY_INSPECT_COMMAND}</code> on the runner host and paste its
            output.
          </p>
          <Textarea
            aria-label="Policy descriptor"
            className="font-mono text-xs"
            rows={6}
            value={pasted}
            onChange={(event) => setPasted(event.currentTarget.value)}
          />
          <div>
            <Button
              size="sm"
              variant="outline"
              disabled={approving || pasted.trim() === ""}
              onClick={() => onApprovePasted(pasted)}
            >
              Approve policy
            </Button>
          </div>
        </div>
      ) : null}
    </SettingsRow>
  );
}

export function ProjectSettingsView({
  projectName,
  integration,
  policy,
  canManage,
  draft,
  onDraftChange,
  onSave,
  onApprovePolicy,
  onApprovePastedPolicy,
  observedPolicies,
  saving,
  approving,
  saveError,
  approveError,
  onOpenFleet,
  onOpenSetup,
  intakeHref,
  header,
  sprites,
  workflow,
}: {
  readonly projectName: string;
  readonly integration: ProjectIntegration;
  readonly policy: PolicyApproval | null;
  readonly canManage: boolean;
  readonly draft: IntegrationDraft;
  readonly onDraftChange: (draft: IntegrationDraft) => void;
  readonly onSave: () => void;
  readonly onApprovePolicy: (policyId: string) => void;
  readonly onApprovePastedPolicy: (text: string) => void;
  readonly observedPolicies: readonly ObservedPolicy[];
  readonly saving: boolean;
  readonly approving: boolean;
  readonly saveError: string | null;
  readonly approveError: string | null;
  readonly onOpenFleet: () => void;
  readonly onOpenSetup?: () => void;
  readonly intakeHref?: string;
  /**
   * Rendered above the screen, inside its own scroll container. The settings
   * page puts its project picker here (§17.3): two `SettingsPageContainer`s
   * side by side would be two scrolling columns rather than one page.
   */
  readonly header?: React.ReactNode;
  readonly sprites?: React.ReactNode;
  readonly workflow?: React.ReactNode;
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
  const conflict = conflictingPolicies(observedPolicies);

  return (
    <SettingsPageContainer>
      {header}
      <div className="flex flex-wrap items-center justify-between gap-3 px-3 sm:px-4">
        <h2 className="text-xl font-semibold tracking-[-0.01em]">{projectName} settings</h2>
        {canManage ? (
          <Button size="sm" disabled={!dirty || saving} onClick={onSave}>
            {saving ? "Saving…" : "Save changes"}
          </Button>
        ) : null}
      </div>

      {saveError === null ? null : (
        <SettingsWarning>
          <b className="font-semibold">This change was not saved.</b> {saveError}
        </SettingsWarning>
      )}

      {policy === null || observedPolicies.length > 0 ? (
        <SettingsWarning
          action={
            canManage && observedPolicies.length === 1 && !conflict ? (
              <Button
                size="sm"
                variant="warning-outline"
                onClick={() => onApprovePolicy(observedPolicies[0]!.policy.policy_id)}
                disabled={approving}
              >
                {approving ? "Approving…" : "Approve updated policy"}
              </Button>
            ) : null
          }
        >
          {conflict ? (
            <>
              <b className="font-semibold">Runners have conflicting project configurations.</b>{" "}
              {policy?.policy.configuration ? "Load the approved shared configuration on the reported runners. Previously reported policies do not need another approval." : "Inspect and approve one shared configuration with the intended planning, validation and automatic promotion settings to converge the runners."}
            </>
          ) : observedPolicies.length > 0 ? (
            <>
              <b className="font-semibold">A runner is waiting for a new policy.</b> Approve the updated policy to resume work.
            </>
          ) : (
            <>
              <b className="font-semibold">No policy is approved for this project.</b> Work is blocked until an owner or admin approves a policy.
            </>
          )}
        </SettingsWarning>
      ) : null}

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
          ? "Hub GitHub integration is unavailable; PR landing uses the runner’s approved repository policy."
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
            description={<>Let members import older GitHub issues and their discussion into Triage. Import from {intakeHref ? <a className="text-primary underline underline-offset-2" href={intakeHref}>the project's Intake page</a> : "the project's Intake page"}.</>}
            control={!unavailable ? <ToggleControl label="Existing issues" checked={draft.importAllowed} disabled={!canManage || saving}
              onCheckedChange={(importAllowed) => onDraftChange({ ...draft, importAllowed })} /> : null}
          />
          <SettingsRow
            title="Pull requests"
            help={{ label: "Pull requests", text: "Enabling this permits repository and pull request operations, including reading, creating and merging pull requests subject to GitHub permissions and branch protections. Disabling it stops these operations but keeps the immutable repository binding. Existing issues and work summaries are separate settings." }}
            status={<span className="inline-flex items-center gap-2 [overflow-wrap:anywhere]">
              <StatusDot tone={integration.repository_enabled && !unavailable && !waitingForApp && !waitingForRepository ? "ok" : "idle"} />
              {!transportAvailable ? "Work lands through the runner’s approved repository policy."
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

      {integration.profile === "native" ? <SettingsSection title="Archiving">
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

      {workflow}

      <SettingsSection title="Execution">
        <PolicyRow
          workflowRevisionsShown={workflow !== undefined}
          policy={policy}
          canManage={canManage}
          onApprove={onApprovePolicy}
          onApprovePasted={onApprovePastedPolicy}
          observed={observedPolicies}
          approving={approving}
          error={approveError}
        />
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
      </SettingsSection>
    </SettingsPageContainer>
  );
}

export function ProjectSettingsRoute({
  projectId,
  onNavigate,
  header,
}: {
  readonly projectId: string;
  readonly onNavigate?: (to: string) => void;
  /** See `ProjectSettingsView`'s own `header`. */
  readonly header?: React.ReactNode;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const project = bootstrap?.projects.find((candidate) => candidate.id === projectId) ?? null;
  const canManage = bootstrap?.actor.can_manage ?? false;

  const integration = useResource<ProjectIntegration>(
    () => api.integration(projectId),
    [api, projectId],
  );
  // A project with nothing approved is an answer rather than a failure, and
  // the hub gives it two shapes: `404` where the project has no policy row at
  // all, and `409 policy_mismatch` where the stored approval no longer matches
  // the resolved descriptor (`policyMismatch` in `internal/hubserver/policy.go`
  // — the hosted preview answers exactly this for a fresh project). Both mean
  // "nothing is approved": the screen says so and offers the approval.
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

  // What a runner resolved and could not run: the Hub records it so the owner
  // approves exactly that descriptor instead of pasting it.
  const setup = useResource(() => api.onboarding(projectId), [api, projectId]);
  const observed = setup.value?.observed_policies ?? [];
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
      void setup.refresh();
      void policy.refresh();
    }, OBSERVED_POLICY_REFRESH_MS);
    return () => globalThis.clearInterval(timer);
  }, [setup.refresh, policy.refresh]);

  // Approves what the runner reported, or a descriptor pasted from the inspect
  // command. Both go through the onboarding route, the one the hosted Hub
  // serves to owners and admins.
  const approve = useMutation(async (choice: { readonly reported: string } | { readonly pasted: string }) => {
    const descriptor: { readonly policy_id: string } | undefined =
      "pasted" in choice
        ? parsePolicyDescriptor(choice.pasted)
        : observed.find((entry) => entry.policy.policy_id === choice.reported)?.policy;
    if (descriptor === undefined) {
      throw new AccountError({
        status: 422,
        code: "invalid_request",
        message: "No runner has reported a policy for this project yet.",
      });
    }
    try {
      const approved = await api.approvePolicy({
        projectId,
        // The onboarding response that listed the reported policies also
        // carries the approval they were compared with, so the two never
        // disagree, even while the separate policy read is still loading.
        expectedPolicyId: (setup.value?.policy ?? policy.value)?.policy.policy_id ?? "",
        policy: descriptor,
        onboarding: true,
      });
      policy.set(approved);
      await integration.refresh();
      globalThis.location.reload();
      return approved;
    } finally {
      // A conflict means somebody else approved meanwhile: re-read both, so a
      // retry names the approval that is actually current.
      await Promise.all([setup.refresh(), policy.refresh()]);
    }
  });

  if (integration.error !== null && integration.error.isAccessError) {
    return (
      <SettingsPageContainer>
        {header}
        <h2 className="px-3 text-xl font-semibold sm:px-4">Project settings</h2>
        <SettingsSection title="Not available">
          <SettingsRow
            title="You do not have access to this project"
            description="Ask an owner or admin for a grant on it, or pick a different project."
          />
        </SettingsSection>
      </SettingsPageContainer>
    );
  }

  if (integration.value === undefined || draft === null) {
    return (
      <SettingsPageContainer>
        {header}
        <h2 className="px-3 text-xl font-semibold sm:px-4">Project settings</h2>
        <p role="status" className="px-3 text-sm text-muted-foreground sm:px-4">
          {integration.error === null
            ? "Loading the project settings."
            : integration.error.message}
        </p>
      </SettingsPageContainer>
    );
  }

  return (
    <ProjectSettingsView
      projectName={project?.name ?? projectId}
      integration={integration.value}
      policy={policy.value ?? null}
      canManage={canManage}
      draft={draft}
      onDraftChange={setDraft}
      onSave={() => void save.call(draft)}
      onApprovePolicy={(policyId) => void approve.call({ reported: policyId })}
      onApprovePastedPolicy={(text) => void approve.call({ pasted: text })}
      observedPolicies={observed}
      saving={save.pending}
      approving={approve.pending}
      saveError={saveMessage(save.error)}
      approveError={approve.error?.message ?? null}
      header={header}
      onOpenFleet={() => onNavigate?.("/settings/runners")}
      intakeHref={`/projects/${projectId}/setup`}
      onOpenSetup={onNavigate ? () => onNavigate(`/projects/${projectId}/setup`) : undefined}
      sprites={<><SpritesCard key={projectId} projectId={projectId} canManage={canManage} /><SpritePoolCard key={`pool-${projectId}`} projectId={projectId} canManage={canManage} /></>}
      workflow={
        <>
          <ModelSelectionSettings projectId={projectId} canManage={canManage && project?.can_write === true} />
          <WorkflowSettings
            integration={integration.value}
            canManage={canManage && project?.can_write === true}
            saving={saveWorkflow.pending}
            error={saveMessage(saveWorkflow.error)}
            onSave={(markdown) => void saveWorkflow.call(markdown)}
            revisions={
              <WorkflowRevisions
                policy={policy.value ?? setup.value?.policy ?? null}
                observed={observed}
                repository={integration.value.checkout_repository || integration.value.repository || ""}
                canApprove={canManage && !conflictingPolicies(observed)}
                approving={approve.pending}
                onApprove={(policyId) => void approve.call({ reported: policyId })}
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
