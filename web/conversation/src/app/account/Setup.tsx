// `/projects/:project/setup` — the first-run wizard.
//
// The artifact's screen 5: a 640px card over a dimmed shell, the Detent Cloud
// logo, a stepper, option rows with checkboxes, and a footer that moves
// forward. What it drives is exactly what `static/js/hosted-setup.js` drove,
// step for step and payload for payload — the policy approval, the progress
// save, the runner enrollment and its routing, the artifact binding, the
// repository bind, the GitHub integration and the first issue.
//
// Two things about that page are kept deliberately:
//
//  - The four steps are the hub's, not the wizard's. `onboarding.Evaluate`
//    decides whether each one is `ready` or `action_required` from the stored
//    state, so the stepper reports readiness rather than remembering which
//    button the reader last pressed. Re-opening the wizard on another machine
//    shows the same truth.
//  - Idempotency keys live in `sessionStorage`, keyed by request path and body
//    fingerprint, and are retired only once the hub has answered. A save whose
//    outcome the browser never learned is retried under the same key, so it
//    cannot land twice (see `idempotency.ts`).
//
// The artifact draws three steps; the hub has four, and the fourth (artifact
// history) is a real decision with a real endpoint. The stepper follows the
// hub.
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import type {
  Onboarding,
  OnboardingStep,
  RunnerEnrollment,
} from "../../contracts/account.ts";
import { ONBOARDING_STEPS } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect, StatusDot } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { DetentCloudLogo } from "./Login.tsx";
import { retireKey, setupKey } from "./idempotency.ts";
import { useMutation, useResource } from "./useResource.ts";

/** The step the wizard opens on: the first one that still needs something. */
export function firstUnreadyStep(steps: readonly OnboardingStep[]): number {
  const index = steps.findIndex((step) => step.state !== "ready");
  return index === -1 ? Math.max(steps.length - 1, 0) : index;
}

/** The steps in `Evaluate`'s order, filling in any the hub did not send. */
export function orderedSteps(onboarding: Onboarding | undefined): readonly OnboardingStep[] {
  const sent = onboarding?.steps ?? [];
  return ONBOARDING_STEPS.map(
    (name) =>
      sent.find((step) => step.name === name) ?? {
        name,
        state: "action_required" as const,
        detail: "",
      },
  );
}

export function Stepper({
  steps,
  activeIndex,
  onSelect,
}: {
  readonly steps: readonly OnboardingStep[];
  readonly activeIndex: number;
  readonly onSelect: (index: number) => void;
}): React.ReactElement {
  return (
    <ol
      aria-label="Setup steps"
      className="mt-[18px] flex gap-1.5 rounded-[10px] border border-border/60 bg-muted p-1"
    >
      {steps.map((step, index) => {
        const active = index === activeIndex;
        return (
          <li key={step.name} className="flex-1">
            <button
              type="button"
              aria-current={active ? "step" : undefined}
              onClick={() => onSelect(index)}
              className={cn(
                "flex h-10 w-full items-center gap-2.5 rounded-lg px-3 text-left text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring",
                active
                  ? "bg-background text-foreground shadow-[0_0_0_1px_var(--contrast-border)]"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              <span
                className={cn(
                  "grid size-[22px] shrink-0 place-items-center rounded-full border text-xs",
                  step.state === "ready"
                    ? "border-success/60 bg-success/15 text-success-foreground"
                    : active
                      ? "border-primary bg-primary/15 text-primary"
                      : "border-border text-muted-foreground",
                )}
              >
                {step.state === "ready" ? "✓" : index + 1}
              </span>
              <span className="min-w-0 flex-1 truncate">{step.name}</span>
              <span className="sr-only">
                {step.state === "ready" ? "ready" : "action required"}
              </span>
            </button>
          </li>
        );
      })}
    </ol>
  );
}

/**
 * The artifact's `.opt` row: an optional checkbox, a leading icon, a name over
 * a monospace detail, and a right-hand status or action.
 */
export function OptionRow({
  checked,
  onCheckedChange,
  label,
  name,
  detail,
  right,
  disabled = false,
}: {
  readonly checked?: boolean;
  readonly onCheckedChange?: (checked: boolean) => void;
  readonly label: string;
  readonly name: React.ReactNode;
  readonly detail?: React.ReactNode;
  readonly right?: React.ReactNode;
  readonly disabled?: boolean;
}): React.ReactElement {
  const selectable = checked !== undefined && onCheckedChange !== undefined;
  return (
    <div className="mb-2.5 flex items-center gap-3 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
      {selectable ? (
        <Checkbox
          aria-label={label}
          checked={checked}
          disabled={disabled}
          onCheckedChange={(next) => onCheckedChange(next === true)}
        />
      ) : null}
      <div className="min-w-0 flex-1">
        <div className="text-[14.5px] text-foreground">{name}</div>
        {detail === undefined ? null : (
          <div className="mt-px font-mono text-[12.5px] text-muted-foreground">{detail}</div>
        )}
      </div>
      {right === undefined ? null : (
        <div className="flex shrink-0 items-center gap-1.5 text-[13px] text-muted-foreground">
          {right}
        </div>
      )}
    </div>
  );
}

export function WizardCard({
  steps,
  activeIndex,
  onSelectStep,
  title,
  lede,
  children,
  footer,
}: {
  readonly steps: readonly OnboardingStep[];
  readonly activeIndex: number;
  readonly onSelectStep: (index: number) => void;
  readonly title: string;
  readonly lede: string;
  readonly children: React.ReactNode;
  readonly footer: React.ReactNode;
}): React.ReactElement {
  return (
    <div className="grid flex-1 place-items-center overflow-y-auto p-10">
      <div className="w-full max-w-[640px] overflow-hidden rounded-2xl border border-border bg-card shadow-[0_30px_80px_rgb(0_0_0/45%)]">
        <div className="border-b border-border/60 px-7 pt-[26px] pb-[18px]">
          <DetentCloudLogo />
          <Stepper steps={steps} activeIndex={activeIndex} onSelect={onSelectStep} />
        </div>
        <div className="px-7 pt-6 pb-[26px]">
          <h1 className="mb-2 text-2xl font-semibold tracking-[-0.02em] text-balance">{title}</h1>
          <p className="mb-[18px] text-sm text-muted-foreground">{lede}</p>
          {children}
          <div className="mt-[18px] flex justify-end gap-2">{footer}</div>
        </div>
      </div>
    </div>
  );
}

/**
 * A wizard mutation. It carries the persisted key through the call and retires
 * it only on an answer, which is the whole of `hosted-setup.js`'s contract with
 * `sessionStorage`.
 */
async function withSetupKey<A>(
  path: string,
  body: Record<string, unknown>,
  run: (key: string) => Promise<A>,
): Promise<A> {
  const { key, storageKey } = await setupKey(path, body);
  const result = await run(key);
  retireKey(storageKey);
  return result;
}

export function SetupRoute({
  projectId,
  onNavigate,
}: {
  readonly projectId: string;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const project = bootstrap?.projects.find((candidate) => candidate.id === projectId) ?? null;
  const onboarding = useResource<Onboarding>(() => api.onboarding(projectId), [api, projectId]);

  const steps = orderedSteps(onboarding.value);
  const [activeIndex, setActiveIndex] = React.useState(0);
  const [settled, setSettled] = React.useState(false);
  React.useEffect(() => {
    if (onboarding.value === undefined || settled) return;
    setActiveIndex(firstUnreadyStep(orderedSteps(onboarding.value)));
    setSettled(true);
  }, [onboarding.value, settled]);

  // The editable progress, seeded from the hub and written back whole: the
  // endpoint replaces the object, so sending half of it would clear the rest.
  const progress = onboarding.value?.progress;
  const [repository, setRepository] = React.useState("");
  const [doctor, setDoctor] = React.useState(false);
  const [provider, setProvider] = React.useState(false);
  const [artifacts, setArtifacts] = React.useState("");
  React.useEffect(() => {
    if (progress === undefined) return;
    setRepository(progress.repository);
    setDoctor(progress.doctor);
    setProvider(progress.provider);
    setArtifacts(progress.artifacts);
  }, [progress]);

  const [repositoryName, setRepositoryName] = React.useState("");
  const [runnerId, setRunnerId] = React.useState("");
  const [machineId, setMachineId] = React.useState("");
  const [enrollment, setEnrollment] = React.useState<RunnerEnrollment | null>(null);
  const [serviceId, setServiceId] = React.useState("");
  const [serviceOrigin, setServiceOrigin] = React.useState("");
  const [servicePublisher, setServicePublisher] = React.useState("");
  const [issueTitle, setIssueTitle] = React.useState("");
  const [issueBody, setIssueBody] = React.useState("");
  const [issueState, setIssueState] = React.useState("");
  React.useEffect(() => {
    const first = project?.states.find((state) => state.dispatchable) ?? project?.states[0];
    if (first !== undefined && issueState === "") setIssueState(first.name);
  }, [project, issueState]);

  const base = `${api.apiBase}/projects/${encodeURIComponent(projectId)}`;

  /**
   * The progress save. Its revision comes from whatever the hub last returned,
   * because the response to a save is the stored progress with the revision
   * already incremented — re-reading the whole project just to write again
   * would be a second round trip for a number the hub already handed back.
   */
  const saveProgress = useMutation(
    async (next: {
      repository: string;
      doctor: boolean;
      provider: boolean;
      artifacts: string;
    }) => {
      const current = onboarding.value;
      if (current === undefined) return null;
      const body = {
        progress: {
          revision: current.progress.revision,
          repository: next.repository,
          doctor: next.doctor,
          provider: next.provider,
          artifacts: next.artifacts,
        },
      };
      const saved = await withSetupKey(`${base}/onboarding`, body, (key) =>
        api.saveProgress({ projectId, key, revision: current.progress.revision, ...next }),
      );
      onboarding.set({ ...current, progress: saved });
      // The steps are recomputed by the hub, so the readiness the stepper
      // shows comes from a re-read rather than from a local guess.
      await onboarding.refresh();
      return saved;
    },
  );

  const approve = useMutation(async () => {
    const descriptor = onboarding.value?.policy?.policy;
    if (descriptor === undefined) {
      throw new AccountError({
        status: 422,
        code: "invalid_request",
        message:
          "The hub has not resolved a policy for this project yet. Run `detent doctor` on the execution host so it can publish detent.yaml and WORKFLOW.md, then reload this page.",
      });
    }
    const approved = await api.approvePolicy({
      projectId,
      expectedPolicyId: descriptor.policy_id,
      policy: descriptor,
      onboarding: true,
    });
    await onboarding.refresh();
    return approved;
  });

  const bindRepository = useMutation(async (name: string) => {
    const current = onboarding.value;
    if (current === undefined) return null;
    const integration = await api.integration(projectId);
    const body = { expected_revision: integration.revision, repository: name };
    const bound = await withSetupKey(`${base}/onboarding/repository`, body, (key) =>
      api.bindRepository({
        projectId,
        repository: name,
        revision: integration.revision,
        key,
      }),
    );
    await onboarding.refresh();
    return bound;
  });

  const enroll = useMutation(async () => {
    const created = await api.enrollRunner({ projectIds: [projectId], runnerId, machineId });
    setEnrollment(created);
    await onboarding.refresh();
    return created;
  });

  /**
   * Routing is read-modify-write against the runner the reader picked. The
   * revision is re-read immediately before the write because the conflict the
   * hub returns carries nothing to recover from.
   */
  const route = useMutation(async (runner: string) => {
    // The hub answers `404` here for a reader without the runner grant as well
    // as for a missing organization (`nativeNotFound` is the permission
    // status on these routes), so both say the same thing: the access this
    // needs is not there any more.
    const fleet = (await api.runners().catch((cause: unknown) => {
      if (cause instanceof AccountError && cause.status === 404) {
        throw new AccountError({
          status: 404,
          code: "not_found",
          message: "Runner access changed. Reload this project.",
        });
      }
      throw cause;
    })) as readonly Record<string, unknown>[];
    const found = fleet.find((entry) => entry["runner_id"] === runner);
    if (found === undefined) {
      throw new AccountError({
        status: 404,
        code: "not_found",
        message: "Runner access changed. Reload this project.",
      });
    }
    const projectIds = Array.isArray(found["project_ids"])
      ? (found["project_ids"] as string[])
      : [];
    const routed = await api.setRunnerRouting({
      runner,
      revision: Number(found["revision"] ?? 0),
      displayName: String(found["display_name"] ?? ""),
      tags: Array.isArray(found["tags"]) ? (found["tags"] as string[]) : [],
      state: String(found["state"] ?? "enabled"),
      capacityLimit: Number(found["capacity_limit"] ?? 0),
      projectIds: projectIds.includes(projectId) ? projectIds : [...projectIds, projectId],
    });
    await onboarding.refresh();
    return routed;
  });

  const bindArtifacts = useMutation(async () => {
    const bound = await api.bindArtifactService({
      projectId,
      serviceId,
      origin: serviceOrigin,
      publisherTokenId: servicePublisher,
    });
    await onboarding.refresh();
    return bound;
  });

  const createIssue = useMutation(async () => {
    const body = {
      title: issueTitle,
      body: issueBody,
      state: issueState,
      labels: [],
      assignees: [],
    };
    const created = await withSetupKey(`${base}/work-items`, body, (key) =>
      api.createFirstIssue({ projectId, title: issueTitle, body: issueBody, state: issueState, key }),
    );
    onNavigate?.("/work");
    return created;
  });

  if (onboarding.error !== null && onboarding.error.isAccessError) {
    return (
      <WizardCard
        steps={steps}
        activeIndex={0}
        onSelectStep={() => undefined}
        title="Setup is not available"
        lede="Setting a project up needs owner or admin access to this organization."
        footer={
          <Button variant="outline" onClick={() => onNavigate?.("/settings")}>
            Back to settings
          </Button>
        }
      >
        <></>
      </WizardCard>
    );
  }

  if (onboarding.value === undefined) {
    return (
      <div className="grid flex-1 place-items-center p-10">
        <p role="status" className="text-sm text-muted-foreground">
          {onboarding.error === null ? "Loading the setup." : onboarding.error.message}
        </p>
      </div>
    );
  }

  const step = steps[activeIndex];
  const eligible = (onboarding.value.runners ?? []).filter(
    (entry) => (entry.exclusions ?? []).length === 0,
  );
  const blocked = (onboarding.value.runners ?? []).filter(
    (entry) => (entry.exclusions ?? []).length > 0,
  );

  const next = () => setActiveIndex((index) => Math.min(index + 1, steps.length - 1));
  const back = () => setActiveIndex((index) => Math.max(index - 1, 0));

  const bodies: Record<string, React.ReactNode> = {
    "Repository configuration": (
      <>
        <OptionRow
          label="This project already has a repository"
          checked={repository === "existing"}
          onCheckedChange={(checked) => setRepository(checked ? "existing" : "")}
          name="Use an existing repository"
          detail="Detent reads detent.yaml and WORKFLOW.md from it"
        />
        <OptionRow
          label="Generate a repository configuration"
          checked={repository === "generate"}
          onCheckedChange={(checked) => setRepository(checked ? "generate" : "")}
          name="Generate the configuration"
          detail="Detent writes a starting detent.yaml and WORKFLOW.md"
        />
        <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5 sm:flex-row sm:items-end">
          <div className="flex flex-1 flex-col gap-1.5">
            <Label htmlFor="setup-repository">Attach a GitHub repository</Label>
            <Input
              id="setup-repository"
              placeholder="owner/name"
              value={repositoryName}
              onChange={(event) => setRepositoryName(event.currentTarget.value)}
            />
          </div>
          <Button
            variant="outline"
            disabled={bindRepository.pending || repositoryName.trim().length === 0}
            onClick={() => void bindRepository.call(repositoryName.trim())}
          >
            {bindRepository.pending ? "Attaching…" : "Attach"}
          </Button>
        </div>
        <ControlError message={bindRepository.error?.message ?? null} />
        <OptionRow
          label="Approve the resolved policy"
          name="Approved policy"
          detail={onboarding.value.policy?.policy.policy_id ?? "Not approved yet"}
          right={
            <Button size="sm" disabled={approve.pending} onClick={() => void approve.call()}>
              {approve.pending ? "Approving…" : "Approve"}
            </Button>
          }
        />
        <ControlError message={approve.error?.message ?? null} />
      </>
    ),
    "Local validation": (
      <>
        <OptionRow
          label="detent doctor passes on the execution host"
          checked={doctor}
          onCheckedChange={setDoctor}
          name="`detent doctor` passes"
          detail="Run it on the machine that will execute work"
        />
        <OptionRow
          label="Signed in to the provider on the execution host"
          checked={provider}
          onCheckedChange={setProvider}
          name="Signed in to your provider"
          detail="Your ChatGPT, Claude or API credentials stay on that machine"
        />
        <p className="text-[13px] text-muted-foreground">
          Detent never holds your provider credentials. These two are what you report, and the hub
          records them as reported by you.
        </p>
      </>
    ),
    "Execution runner": (
      <>
        {eligible.map((entry) => (
          <OptionRow
            key={entry.runner.runner_id}
            label={`Runner ${entry.runner.display_name}`}
            name={entry.runner.display_name}
            detail={`${entry.runner.hostname ?? entry.runner.runner_id} · ${entry.runner.state}`}
            right={
              <>
                <StatusDot tone="ok" />
                Eligible
              </>
            }
          />
        ))}
        {blocked.map((entry) => (
          <OptionRow
            key={entry.runner.runner_id}
            label={`Runner ${entry.runner.display_name}`}
            name={entry.runner.display_name}
            detail={(entry.exclusions ?? []).map((exclusion) => exclusion.message).join(" · ")}
            right={
              <>
                <StatusDot tone="warn" />
                Not eligible
                <Button
                  size="xs"
                  variant="outline"
                  disabled={route.pending}
                  onClick={() => void route.call(entry.runner.runner_id)}
                >
                  Route here
                </Button>
              </>
            }
          />
        ))}
        <ControlError message={route.error?.message ?? null} />
        <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
          <div className="text-[14.5px]">Enroll a host</div>
          <p className="text-[13px] text-muted-foreground">
            Generate the two ids on the host itself, then paste them here. The token below is shown
            once and is only usable on the machine that generated these ids.
          </p>
          <div className="flex flex-col gap-2 sm:flex-row">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="setup-runner-id">Runner id</Label>
              <Input
                id="setup-runner-id"
                value={runnerId}
                onChange={(event) => setRunnerId(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="setup-machine-id">Machine id</Label>
              <Input
                id="setup-machine-id"
                value={machineId}
                onChange={(event) => setMachineId(event.currentTarget.value)}
              />
            </div>
          </div>
          <div>
            <Button
              variant="outline"
              disabled={
                enroll.pending || runnerId.trim().length === 0 || machineId.trim().length === 0
              }
              onClick={() => void enroll.call()}
            >
              {enroll.pending ? "Enrolling…" : "Create enrollment token"}
            </Button>
          </div>
          <ControlError message={enroll.error?.message ?? null} />
          {enrollment === null ? null : (
            <p role="status" className="rounded-lg bg-background p-3 font-mono text-xs break-all">
              One-time token: {enrollment.token} — expires {enrollment.expires_at}. Use it only on
              the host that generated these ids. If interrupted, create a fresh token for the same
              ids.
            </p>
          )}
        </div>
      </>
    ),
    "Artifact history": (
      <>
        <OptionRow
          label="Keep artifact history on the execution host"
          checked={artifacts === "local"}
          onCheckedChange={(checked) => setArtifacts(checked ? "local" : "")}
          name="Local history"
          detail="Artifacts stay on the machine that produced them"
        />
        <OptionRow
          label="Use a customer artifact service"
          checked={artifacts === "customer"}
          onCheckedChange={(checked) => setArtifacts(checked ? "customer" : "")}
          name="Customer service"
          detail="An S3-compatible service and an independent gateway you run"
        />
        {artifacts === "customer" ? (
          <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="setup-service-id">Service id</Label>
              <Input
                id="setup-service-id"
                value={serviceId}
                onChange={(event) => setServiceId(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="setup-service-origin">Gateway origin</Label>
              <Input
                id="setup-service-origin"
                placeholder="https://artifacts.example.com"
                value={serviceOrigin}
                onChange={(event) => setServiceOrigin(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="setup-service-token">Publisher token id</Label>
              <Input
                id="setup-service-token"
                value={servicePublisher}
                onChange={(event) => setServicePublisher(event.currentTarget.value)}
              />
            </div>
            <div>
              <Button
                variant="outline"
                disabled={bindArtifacts.pending || serviceId.trim().length === 0}
                onClick={() => void bindArtifacts.call()}
              >
                {bindArtifacts.pending ? "Binding…" : "Bind the service"}
              </Button>
            </div>
            <ControlError message={bindArtifacts.error?.message ?? null} />
            <p className="text-[13px] text-muted-foreground">
              A binding records where artifacts go. It does not verify the storage and does not
              promise offline access.
            </p>
          </div>
        ) : null}
        {onboarding.value.ready ? (
          <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
            <div className="text-[14.5px]">Your first issue</div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="setup-issue-title">Title</Label>
              <Input
                id="setup-issue-title"
                value={issueTitle}
                onChange={(event) => setIssueTitle(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="setup-issue-body">What needs doing</Label>
              <Textarea
                id="setup-issue-body"
                rows={3}
                value={issueBody}
                onChange={(event) => setIssueBody(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="setup-issue-state">Lane</Label>
              <NativeSelect
                id="setup-issue-state"
                value={issueState}
                options={(project?.states ?? []).map((state) => ({
                  value: state.name,
                  label: state.name,
                }))}
                onValueChange={setIssueState}
              />
            </div>
            <div>
              <Button
                disabled={createIssue.pending || issueTitle.trim().length === 0}
                onClick={() => void createIssue.call()}
              >
                {createIssue.pending ? "Creating…" : "Create the first issue"}
              </Button>
            </div>
            <ControlError message={createIssue.error?.message ?? null} />
          </div>
        ) : null}
      </>
    ),
  };

  const saveAndContinue = () => {
    void saveProgress
      .call({ repository, doctor, provider, artifacts })
      .then(() => {
        if (activeIndex < steps.length - 1) next();
      });
  };

  return (
    <WizardCard
      steps={steps}
      activeIndex={activeIndex}
      onSelectStep={setActiveIndex}
      title={step?.name ?? "Set up this project"}
      lede={step?.detail ?? ""}
      footer={
        <>
          {activeIndex > 0 ? (
            <Button variant="outline" onClick={back}>
              Back
            </Button>
          ) : null}
          {onboarding.value.ready && activeIndex === steps.length - 1 ? (
            <Button onClick={() => onNavigate?.("/work")}>Finish</Button>
          ) : (
            <Button onClick={saveAndContinue} disabled={saveProgress.pending}>
              {saveProgress.pending ? "Saving…" : "Continue"}
            </Button>
          )}
        </>
      }
    >
      <>
        {bodies[step?.name ?? ""] ?? null}
        <ControlError message={saveProgress.error?.message ?? null} />
      </>
    </WizardCard>
  );
}
