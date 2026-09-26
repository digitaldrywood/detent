import React from "react";

import { Button } from "../../components/ui/button.tsx";
import type { PolicyApproval, ProjectIntegration } from "../../contracts/account.ts";
import { INTAKE_CHOICES, PROJECTION_CHOICES } from "../../contracts/account.ts";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
  SettingsWarning,
} from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect, PathValue, ToggleControl } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
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

export function PolicyRow({
  policy,
  canManage,
  onApprove,
  approving,
  error,
}: {
  readonly policy: PolicyApproval | null;
  readonly canManage: boolean;
  readonly onApprove: () => void;
  readonly approving: boolean;
  readonly error: string | null;
}): React.ReactElement {
  return (
    <SettingsRow
      title="Approved policy"
      description={
        policy === null
          ? "No policy is approved. Nothing runs on this project until somebody inspects the resolved descriptor and approves it."
          : "The resolved policy descriptor a human approved. Approving again re-reads the host's detent.yaml and WORKFLOW.md."
      }
      status={
        policy === null ? null : (
          <>
            <span className="font-mono">{policy.policy.policy_id}</span> · approved by{" "}
            {policy.approved_by} on {policy.approved_at}
          </>
        )
      }
      control={
        canManage ? (
          <div className="flex flex-col items-end gap-1.5">
            <Button size="sm" variant={policy === null ? "default" : "outline"} disabled={approving} onClick={onApprove}>
              {approving ? "Approving…" : policy === null ? "Approve policy" : "Re-approve"}
            </Button>
            <ControlError message={error} />
          </div>
        ) : (
          <span className="text-sm text-muted-foreground">
            {policy === null ? "Not approved" : "Approved"}
          </span>
        )
      }
    />
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
  saving,
  approving,
  saveError,
  approveError,
  onOpenFleet,
  header,
}: {
  readonly projectName: string;
  readonly integration: ProjectIntegration;
  readonly policy: PolicyApproval | null;
  readonly canManage: boolean;
  readonly draft: IntegrationDraft;
  readonly onDraftChange: (draft: IntegrationDraft) => void;
  readonly onSave: () => void;
  readonly onApprovePolicy: () => void;
  readonly saving: boolean;
  readonly approving: boolean;
  readonly saveError: string | null;
  readonly approveError: string | null;
  readonly onOpenFleet: () => void;
  /**
   * Rendered above the screen, inside its own scroll container. The settings
   * page puts its project picker here (§17.3): two `SettingsPageContainer`s
   * side by side would be two scrolling columns rather than one page.
   */
  readonly header?: React.ReactNode;
}): React.ReactElement {
  const unbound = (integration.repository ?? "").length === 0;
  const dirty = draftChanged(draft, draftOf(integration));

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

      {policy === null ? (
        <SettingsWarning
          action={
            canManage ? (
              <Button size="sm" variant="warning-outline" onClick={onApprovePolicy} disabled={approving}>
                {approving ? "Approving…" : "Approve policy"}
              </Button>
            ) : null
          }
        >
          <b className="font-semibold">No policy is approved for this project.</b> Detent will not
          dispatch work until somebody inspects the resolved descriptor on the execution host and
          approves it.
        </SettingsWarning>
      ) : null}

      <SettingsSection title="Repository">
        <SettingsRow
          title="Repository"
          description="The GitHub repository this project's work lands in. A binding is immutable once it exists."
          control={
            unbound ? (
              <span className="text-sm text-muted-foreground">Not attached</span>
            ) : (
              <PathValue value={integration.repository ?? ""} />
            )
          }
        />
        <SettingsRow
          title="Repository and pull request integration"
          description="Let Detent read and write this repository's pull requests. Attach a repository first."
          control={
            <ToggleControl
              label="Repository and pull request integration"
              checked={draft.repositoryEnabled}
              disabled={!canManage || unbound}
              onCheckedChange={(repositoryEnabled) => onDraftChange({ ...draft, repositoryEnabled })}
            />
          }
        />
      </SettingsSection>

      <SettingsSection title="Issue flow">
        <SettingsRow
          title="Intake"
          description="Whether issues opened on GitHub are pulled into this project's board."
          control={
            <NativeSelect
              aria-label="Intake"
              value={draft.intake}
              options={INTAKE_OPTIONS}
              disabled={!canManage || unbound}
              onValueChange={(intake) => onDraftChange({ ...draft, intake })}
            />
          }
        />
        <SettingsRow
          title="Projection"
          description="Whether Detent writes a summary of each work item back to the GitHub issue. Summary projection requires native authority."
          control={
            <NativeSelect
              aria-label="Projection"
              value={draft.projection}
              options={PROJECTION_OPTIONS}
              disabled={!canManage || unbound || integration.profile === "github_compatible"}
              onValueChange={(projection) => onDraftChange({ ...draft, projection })}
            />
          }
        />
        <SettingsRow
          title="Authority"
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

      <SettingsSection title="Execution">
        <PolicyRow
          policy={policy}
          canManage={canManage}
          onApprove={onApprovePolicy}
          approving={approving}
          error={approveError}
        />
        <SettingsRow
          title="Runner routing"
          description="Which hosts may take this project's work, and the tags that select them. Routing lives with the fleet, because a runner serves more than one project."
          control={
            <Button size="sm" variant="outline" onClick={onOpenFleet}>
              Open the fleet
            </Button>
          }
        />
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
      integration.set(saved);
      setDraft(draftOf(saved));
      return saved;
    } catch (cause) {
      // The conflict's own body carries nothing to recover from, so the screen
      // re-reads and shows the reader what is actually stored.
      if (cause instanceof AccountError && cause.isConflict) await integration.refresh();
      throw cause;
    }
  });

  const approve = useMutation(async () => {
    const descriptor = policy.value?.policy;
    if (descriptor === undefined) {
      throw new AccountError({
        status: 422,
        code: "invalid_request",
        message:
          "There is no resolved policy to approve yet. Run the first-run setup for this project, which reads the descriptor from the execution host.",
      });
    }
    const approved = await api.approvePolicy({
      projectId,
      expectedPolicyId: descriptor.policy_id,
      policy: descriptor,
    });
    policy.set(approved);
    return approved;
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
      onApprovePolicy={() => void approve.call()}
      saving={save.pending}
      approving={approve.pending}
      saveError={saveMessage(save.error)}
      approveError={approve.error?.message ?? null}
      header={header}
      onOpenFleet={() => onNavigate?.("/settings/runners")}
    />
  );
}
