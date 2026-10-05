import { ModelSelectionSettings } from "./ModelSelectionSettings.tsx";
import React from "react";

import { SpritesCard } from "./SpritesCard.tsx";
import { SpritePoolCard } from "./SpritePoolCard.tsx";

import { Button } from "../../components/ui/button.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import type { ObservedPolicy, PolicyApproval, ProjectIntegration } from "../../contracts/account.ts";
import { INTAKE_CHOICES, PROJECTION_CHOICES } from "../../contracts/account.ts";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
  SettingsWarning,
} from "../settings/settingsLayout.tsx";
import { RUNNER_HELP } from "../fleet/runnerHelp.ts";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect, PathValue, ToggleControl } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { parsePolicyDescriptor, POLICY_INSPECT_COMMAND } from "./Setup.tsx";
import { useMutation, useResource } from "./useResource.ts";

/** The editable half of the integration: what a save sends. */
export interface IntegrationDraft {
  readonly intake: string;
  readonly projection: string;
  readonly repositoryEnabled: boolean;
}

export function draftOf(integration: ProjectIntegration): IntegrationDraft {
  return {
    intake: integration.intake,
    projection: integration.projection,
    repositoryEnabled: integration.repository_enabled,
  };
}

export function draftChanged(a: IntegrationDraft, b: IntegrationDraft): boolean {
  return (
    a.intake !== b.intake ||
    a.projection !== b.projection ||
    a.repositoryEnabled !== b.repositoryEnabled
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

const INTAKE_OPTIONS = INTAKE_CHOICES.map((value) => ({
  value,
  label: value === "manual" ? "Manual" : "Disabled",
}));
const PROJECTION_OPTIONS = PROJECTION_CHOICES.map((value) => ({
  value,
  label: value === "summary" ? "Summary" : "Disabled",
}));

export function WorkflowSettings({
  integration,
  canManage,
  saving,
  error,
  onSave,
}: {
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
}: {
  readonly policy: PolicyApproval | null;
  /** Descriptors runners resolved and could not run, newest first. */
  readonly observed: readonly ObservedPolicy[];
  readonly canManage: boolean;
  readonly onApprove: (policyId: string) => void;
  readonly onApprovePasted: (text: string) => void;
  readonly approving: boolean;
  readonly error: string | null;
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
      ? "A runner reported an updated policy that needs approval."
      : policy === null
        ? "No policy is approved. Start a runner or paste an inspected descriptor."
        : shared
          ? "Authorized runners consume this approved project configuration."
          : "The repository policy descriptor approved for execution.";
  return (
    <SettingsRow
      title={shared ? "Shared project configuration" : "Repository policy"}
      help={{
        label: shared ? "Shared project configuration" : "Repository policy",
        text: shared || observed.some((entry) => entry.policy.configuration)
          ? "Native runners consume the shared configuration stored with the exact approved descriptor. Credentials, paths, capacity, isolation setup and runner routing stay on each host. Inspect the intended project definition and approve its descriptor to change shared behavior. Execution requires an exact match to that approval."
          : "The runner reports the policy descriptor it resolves from the trusted repository revision, including detent.yaml and WORKFLOW.md. Repository changes or a runner upgrade can change that descriptor and make the prior approval stale. Execution is blocked when the runner’s resolved policy does not match an approved descriptor; an owner or admin must approve the current policy.",
      }}
      description={description}
      status={
        <>
          {policy === null ? null : (
            <span className="block break-all">
              Approved <span className="font-mono">{policy.policy.policy_id}</span> by {policy.approved_by} on{" "}
              {policy.approved_at}
              {shared ? ` · Planning ${policy.policy.gates.plan_enabled ? "on" : "off"} · Validation ${policy.policy.gates.validator ? "on" : "off"} · Automatic promotion ${policy.policy.gates.auto_promote ? "on" : "off"}` : null}
            </span>
          )}
          {observed.map((entry) => (
            <span key={entry.policy.policy_id} className="mt-1 flex min-w-0 flex-wrap items-center gap-2 break-all text-warning-foreground">
              <span>
                Runner <span className="font-mono">{entry.runner_ids?.join(", ") ?? entry.runner_id}</span> reports{" "}
                <span className="font-mono">{entry.policy.policy_id}</span> (source revision{" "}
                <span className="font-mono">{entry.policy.source_revision.slice(0, 12)}</span>) at {entry.observed_at}
                {entry.policy.configuration ? ` · Planning ${entry.policy.gates.plan_enabled ? "on" : "off"} · Validation ${entry.policy.gates.validator ? "on" : "off"} · Automatic promotion ${entry.policy.gates.auto_promote ? "on" : "off"}` : null}
              </span>
              {entry.previously_approved ? <span>Previously approved configuration</span> : null}
              {canManage && !conflict ? (
                <Button
                  size="xs"
                  disabled={approving}
                  aria-label={`Approve updated policy ${entry.policy.policy_id}`}
                  onClick={() => onApprove(entry.policy.policy_id)}
                >
                  {approving ? "Approving…" : "Approve updated policy"}
                </Button>
              ) : null}
            </span>
          ))}
        </>
      }
      control={
        canManage ? (
          <div className="flex flex-col items-end gap-1.5">
            <Button size="sm" variant="ghost-muted" onClick={() => setPasting((open) => !open)}>
              {pasting ? "Cancel pasting" : "Paste a descriptor"}
            </Button>
            <ControlError message={error} />
          </div>
        ) : (
          <span className="text-sm text-muted-foreground">
            {observed.length > 0 ? "Waiting for approval" : policy === null ? "Not approved" : "Approved"}
          </span>
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
              Approve pasted descriptor
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
              <b className="font-semibold">A runner is waiting for a new policy.</b> Repository settings or a runner
              upgrade changed the resolved policy. Approve the updated policy to resume work.
            </>
          ) : (
            <>
              <b className="font-semibold">No policy is approved for this project.</b> Detent will not
              dispatch work until a runner reports the policy it resolved and somebody approves it.
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
        <SettingsRow
          title="Repository and pull request integration"
          help={transportAvailable ? {
            label: "Repository and pull request integration",
            text: "Enabling this permits repository and pull request operations, including reading, creating and merging pull requests subject to GitHub permissions and branch protections. Disabling it stops these operations but keeps the immutable repository binding. Intake and summary projection are separate settings.",
          } : undefined}
          description={transportAvailable ? "Allow repository and pull request operations." : "Hub GitHub integration is unavailable; PR landing uses the runner’s approved repository policy."}
          control={
            transportAvailable ? (
              <ToggleControl
                label="Repository and pull request integration"
                checked={draft.repositoryEnabled}
                disabled={!canManage || unbound}
                onCheckedChange={(repositoryEnabled) => onDraftChange({ ...draft, repositoryEnabled })}
              />
            ) : null
          }
        />
      </SettingsSection>

      <SettingsSection title="Issue flow">
        {transportAvailable ? <SettingsRow
          title="Intake"
          help={{
            label: "Intake",
            text: "Manual intake imports a GitHub issue and its discussion into Detent when you request it; it does not automatically import every new issue. Disabled prevents new manual imports and keeps previously imported work. The project profile determines who owns the imported fields.",
          }}
          description="Import selected GitHub issues into this project."
          control={
            <NativeSelect
              aria-label="Intake"
              value={draft.intake}
              options={INTAKE_OPTIONS}
              disabled={!canManage || unbound}
              onValueChange={(intake) => onDraftChange({ ...draft, intake })}
            />
          }
        /> : null}
        {transportAvailable ? <SettingsRow
          title="Projection"
          help={{
            label: "Projection",
            text: "Summary sends a work-item summary from Detent to the linked GitHub issue. Disabled stops new summary writes and leaves existing GitHub content in place. Summary requires native authority; it does not transfer field ownership to GitHub.",
          }}
          description="Write work-item summaries back to GitHub."
          control={
            <NativeSelect
              aria-label="Projection"
              value={draft.projection}
              options={PROJECTION_OPTIONS}
              disabled={!canManage || unbound || integration.profile === "github_compatible"}
              onValueChange={(projection) => onDraftChange({ ...draft, projection })}
            />
          }
        /> : null}
        <SettingsRow
          title="Authority"
          help={{
            label: "Authority",
            text: "The native profile gives Detent ownership of issue title, body, discussion, dependencies, authors, workflow, labels, assignees and priority; the github_compatible profile gives GitHub ownership of those fields. Detent always owns scheduling, progress and native approval. Source timestamps retain their source, repository policy comes from the trusted repository revision, and GitHub controls merge protections. Intake and projection do not change these owners.",
          }}
          description="Which side owns each field. Set by the project's profile, not by this page."
          status={
            <span className="font-mono text-[11px]">
              {Object.entries(integration.authority ?? {})
                .map(([field, owner]) => `${field}: ${owner}`)
                .join(" · ")}
            </span>
          }
          control={<span className="text-sm capitalize text-muted-foreground">{integration.profile}</span>}
        />
      </SettingsSection>

      {workflow}

      <SettingsSection title="Execution">
        <PolicyRow
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
          description="Select authorized runners that can take this project’s work."
          control={
            <Button size="sm" variant="outline" onClick={onOpenFleet}>
              Open the fleet
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
        intake: next.intake,
        projection: next.projection,
        repositoryEnabled: next.repositoryEnabled,
      });
      integration.set({ ...saved, github_transport_available: current.github_transport_available });
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
        intake: current.intake,
        projection: current.projection,
        repositoryEnabled: current.repository_enabled,
        workflowMarkdown: markdown,
      });
      integration.set({ ...saved, github_transport_available: current.github_transport_available });
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
  // A runner reports a changed policy whenever the repository changes, so the
  // page looks again while it is open instead of only on the next visit.
  React.useEffect(() => {
    const timer = globalThis.setInterval(() => void setup.refresh(), OBSERVED_POLICY_REFRESH_MS);
    return () => globalThis.clearInterval(timer);
  }, [setup.refresh]);

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
      onOpenSetup={onNavigate ? () => onNavigate(`/projects/${projectId}/setup`) : undefined}
      sprites={<><SpritesCard key={projectId} projectId={projectId} canManage={canManage} /><SpritePoolCard key={`pool-${projectId}`} projectId={projectId} canManage={canManage} /></>}
      workflow={<><ModelSelectionSettings projectId={projectId} canManage={canManage && project?.can_write === true} /><WorkflowSettings integration={integration.value} canManage={canManage && project?.can_write === true} saving={saveWorkflow.pending} error={saveMessage(saveWorkflow.error)} onSave={(markdown) => void saveWorkflow.call(markdown)} /></>}
    />
  );
}
