import React from "react";

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
  observed,
  canManage,
  onApprove,
  onApprovePasted,
  approving,
  error,
}: {
  readonly policy: PolicyApproval | null;
  /** A descriptor a runner resolved and could not run, if it differs from `policy`. */
  readonly observed: ObservedPolicy | null;
  readonly canManage: boolean;
  /** Approves `observed`. */
  readonly onApprove: () => void;
  readonly onApprovePasted: (text: string) => void;
  readonly approving: boolean;
  readonly error: string | null;
}): React.ReactElement {
  const [pasting, setPasting] = React.useState(false);
  const [pasted, setPasted] = React.useState("");
  const description =
    observed !== null
      ? `A runner resolved a different policy from the repository's detent.yaml and WORKFLOW.md and is waiting for it to be approved. Nothing runs on this project until it is.`
      : policy === null
        ? "No policy is approved. Start a runner for this project and it reports the policy it resolved here, or paste the output of the inspect command."
        : "The resolved policy descriptor a human approved. When the repository's detent.yaml or WORKFLOW.md changes, the runner reports the new policy here for approval.";
  return (
    <SettingsRow
      title="Repository policy"
      description={description}
      status={
        <>
          {policy === null ? null : (
            <span className="block">
              Approved <span className="font-mono">{policy.policy.policy_id}</span> by {policy.approved_by} on{" "}
              {policy.approved_at}
            </span>
          )}
          {observed === null ? null : (
            <span className="block text-warning-foreground">
              Runner <span className="font-mono">{observed.runner_id}</span> reports{" "}
              <span className="font-mono">{observed.policy.policy_id}</span> (source revision{" "}
              <span className="font-mono">{observed.policy.source_revision.slice(0, 12)}</span>) at{" "}
              {observed.observed_at}
            </span>
          )}
        </>
      }
      control={
        canManage ? (
          <div className="flex flex-col items-end gap-1.5">
            {observed === null ? null : (
              <Button size="sm" disabled={approving} onClick={onApprove}>
                {approving ? "Approving…" : "Approve reported policy"}
              </Button>
            )}
            <Button size="sm" variant="ghost-muted" onClick={() => setPasting((open) => !open)}>
              {pasting ? "Cancel pasting" : "Paste a descriptor"}
            </Button>
            <ControlError message={error} />
          </div>
        ) : (
          <span className="text-sm text-muted-foreground">
            {observed !== null ? "Waiting for approval" : policy === null ? "Not approved" : "Approved"}
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
  observedPolicy,
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
  readonly onApprovePastedPolicy: (text: string) => void;
  readonly observedPolicy: ObservedPolicy | null;
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

      {policy === null || observedPolicy !== null ? (
        <SettingsWarning
          action={
            canManage && observedPolicy !== null ? (
              <Button size="sm" variant="warning-outline" onClick={onApprovePolicy} disabled={approving}>
                {approving ? "Approving…" : "Approve reported policy"}
              </Button>
            ) : null
          }
        >
          {observedPolicy !== null ? (
            <>
              <b className="font-semibold">A runner is waiting for a new policy.</b> The repository&apos;s
              detent.yaml or WORKFLOW.md changed, and nothing runs until the policy it resolved is approved.
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
          onApprovePasted={onApprovePastedPolicy}
          observed={observedPolicy}
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

  // What a runner resolved and could not run: the Hub records it so the owner
  // approves exactly that descriptor instead of pasting it.
  const setup = useResource(() => api.onboarding(projectId), [api, projectId]);
  const observed = setup.value?.observed_policy ?? null;

  // Approves what the runner reported, or a descriptor pasted from the inspect
  // command. Both go through the onboarding route, the one the hosted Hub
  // serves to owners and admins.
  const approve = useMutation(async (pasted: string | null) => {
    const descriptor: { readonly policy_id: string } | undefined =
      pasted === null ? observed?.policy : parsePolicyDescriptor(pasted);
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
        expectedPolicyId: policy.value?.policy.policy_id ?? "",
        policy: descriptor,
        onboarding: true,
      });
      policy.set(approved);
      return approved;
    } finally {
      await setup.refresh();
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
      onApprovePolicy={() => void approve.call(null)}
      onApprovePastedPolicy={(text) => void approve.call(text)}
      observedPolicy={observed}
      saving={save.pending}
      approving={approve.pending}
      saveError={saveMessage(save.error)}
      approveError={approve.error?.message ?? null}
      header={header}
      onOpenFleet={() => onNavigate?.("/settings/runners")}
    />
  );
}
