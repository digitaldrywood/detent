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
import { GitHubIssueIntake } from "./IssueIntake.tsx";

import { Button } from "../../components/ui/button.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import type { Onboarding, OnboardingStep, ProjectIntegration } from "../../contracts/account.ts";
import { ONBOARDING_STEPS } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { AccountError } from "./api.ts";
import { ContextHelp } from "../components/ContextHelp.tsx";
import { EnrollRunnerDialog } from "../fleet/EnrollRunner.tsx";
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
          // `min-w-0` lets the tabs shrink to the card: without it each keeps
          // its full label's width and the row runs past the card's edge.
          <li key={step.name} className={cn("min-w-0", active ? "flex-[2_1_0%]" : "flex-1")}>
            <button
              type="button"
              aria-current={active ? "step" : undefined}
              onClick={() => onSelect(index)}
              className={cn(
                "flex h-10 w-full min-w-0 items-center justify-center gap-2.5 rounded-lg px-2 text-left text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring sm:justify-start sm:px-3",
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
              <span className={cn("min-w-0 flex-1 truncate", active ? null : "max-sm:sr-only")}>{step.name}</span>
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
/** The command that prints the descriptor a human approves. */
export const POLICY_INSPECT_COMMAND = "detent hub policy inspect --config <global.yaml> --project <project>";

/**
 * The descriptor pasted from `detent hub policy inspect`. The hub validates
 * it and binds the approval to its identity; this only refuses what is not
 * a descriptor at all, so the reader learns that before the request.
 */
export function parsePolicyDescriptor(text: string): { readonly policy_id: string } {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    throw new AccountError({ status: 422, code: "invalid_request", message: "The pasted descriptor is not JSON. Paste the whole output of the inspect command." });
  }
  if (value === null || typeof value !== "object" || Array.isArray(value) || typeof (value as { policy_id?: unknown }).policy_id !== "string") {
    throw new AccountError({ status: 422, code: "invalid_request", message: "The pasted JSON is not a policy descriptor: it has no policy_id." });
  }
  return value as { readonly policy_id: string };
}

export function OptionRow({
  checked,
  onCheckedChange,
  label,
  name,
  detail,
  right,
  help,
  disabled = false,
}: {
  readonly checked?: boolean;
  readonly onCheckedChange?: (checked: boolean) => void;
  readonly label: string;
  readonly name: React.ReactNode;
  readonly detail?: React.ReactNode;
  readonly right?: React.ReactNode;
  readonly help?: string;
  readonly disabled?: boolean;
}): React.ReactElement {
  const selectable = checked !== undefined && onCheckedChange !== undefined;
  const id = React.useId();
  const body = (
    <>
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
    </>
  );
  if (!selectable) {
    return (
      <div className="mb-2.5 flex items-center gap-3 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
        {body}
        {help === undefined ? null : <ContextHelp label={label}>{help}</ContextHelp>}
      </div>
    );
  }
  // Keep the choice label and help button as siblings so reading help cannot
  // change the selection.
  return (
    <div
      data-checked={checked ? "" : undefined}
      className={cn(
        "mb-2.5 flex cursor-pointer items-center gap-3 rounded-xl border px-4 py-3.5 transition-colors",
        checked ? "border-primary bg-primary/8" : "border-border/60 bg-muted hover:border-border",
        disabled && "cursor-not-allowed opacity-64",
      )}
    >
      <label htmlFor={id} className="flex min-w-0 flex-1 cursor-pointer items-center gap-3">
        <Checkbox
          id={id}
          aria-label={label}
          checked={checked}
          disabled={disabled}
          onCheckedChange={(next) => onCheckedChange(next === true)}
        />
        {body}
      </label>
      {help === undefined ? null : <ContextHelp label={label}>{help}</ContextHelp>}
    </div>
  );
}

export interface Choice {
  readonly value: string;
  readonly name: string;
  readonly help?: string;
  readonly detail?: React.ReactNode;
}

/**
 * One of several options, as cards. Native radios give the group its keyboard
 * behaviour (arrow keys move the selection) and its accessible role; the card
 * around each shows which one is chosen.
 */
export function ChoiceGroup({
  name,
  legend,
  value,
  onValueChange,
  choices,
}: {
  readonly name: string;
  readonly legend: string;
  readonly value: string;
  readonly onValueChange: (value: string) => void;
  readonly choices: readonly Choice[];
}): React.ReactElement {
  return (
    <fieldset className="mb-2.5 flex min-w-0 flex-col gap-2.5">
      <legend className="sr-only">{legend}</legend>
      {choices.map((choice) => {
        const selected = choice.value === value;
        const id = `${name}-${choice.value}`;
        return (
          <div
            key={choice.value}
            data-checked={selected ? "" : undefined}
            className={cn(
              "flex cursor-pointer items-center gap-3 rounded-xl border px-4 py-3.5 transition-colors has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-ring",
              selected ? "border-primary bg-primary/8" : "border-border/60 bg-muted hover:border-border",
            )}
          >
            <label htmlFor={id} className="flex min-w-0 flex-1 cursor-pointer items-center gap-3">
              <input
                id={id}
                type="radio"
                name={name}
                value={choice.value}
                aria-labelledby={`${id}-name`}
                aria-describedby={choice.detail === undefined ? undefined : `${id}-detail`}
                checked={selected}
                onChange={() => onValueChange(choice.value)}
                className="size-4 shrink-0 accent-primary outline-none"
              />
              <span className="min-w-0 flex-1">
                <span id={`${id}-name`} className="block text-[14.5px] text-foreground">
                  {choice.name}
                </span>
                {choice.detail === undefined ? null : (
                  <span id={`${id}-detail`} className="mt-px block font-mono text-[12.5px] text-muted-foreground">
                    {choice.detail}
                  </span>
                )}
              </span>
            </label>
            {choice.help === undefined ? null : <ContextHelp label={choice.name}>{choice.help}</ContextHelp>}
          </div>
        );
      })}
    </fieldset>
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
    // Not `place-items-center`: centring a card taller or wider than the
    // viewport pushes its start past the scroll origin, where it cannot be
    // scrolled to. `my-auto` centres it only while it fits.
    <div className="flex min-h-0 flex-1 flex-col items-center overflow-y-auto px-4 py-6 sm:p-10">
      <div className="my-auto w-full min-w-0 max-w-[640px] rounded-2xl border border-border bg-card shadow-[0_30px_80px_rgb(0_0_0/45%)]">
        <div className="border-b border-border/60 px-5 pt-[26px] pb-[18px] sm:px-7">
          <DetentCloudLogo />
          <Stepper steps={steps} activeIndex={activeIndex} onSelect={onSelectStep} />
        </div>
        <div className="px-5 pt-6 pb-[26px] sm:px-7">
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
  const integration = useResource<ProjectIntegration>(() => api.integration(projectId), [api, projectId]);

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
  const [artifacts, setArtifacts] = React.useState("");
  React.useEffect(() => {
    if (progress === undefined) return;
    setRepository(progress.repository);
    setArtifacts(progress.artifacts);
  }, [progress]);

  const [repositoryName, setRepositoryName] = React.useState("");
  const [pastedPolicy, setPastedPolicy] = React.useState("");
  const policyReports = onboarding.value?.observed_policies ?? [];
  const policyConflict = policyReports.some((entry) => entry.conflict || entry.previously_approved) || new Set(policyReports.map((entry) => entry.policy.policy_id)).size > 1;
  const [enrolling, setEnrolling] = React.useState(false);
  const fleet = useResource(() => enrolling ? api.fleet() : Promise.resolve(undefined), [api, enrolling]);
  const [serviceId, setServiceId] = React.useState("");
  const [serviceOrigin, setServiceOrigin] = React.useState("");
  const [servicePublisher, setServicePublisher] = React.useState("");
  const [issueTitle, setIssueTitle] = React.useState("");
  const [issueBody, setIssueBody] = React.useState("");
  const [issueState, setIssueState] = React.useState("");
  React.useEffect(() => {
    const first = project?.profile === "native" ? project.states[0] : project?.states.find((state) => state.dispatchable) ?? project?.states[0];
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
    const current = onboarding.value?.policy?.policy;
    const descriptor = pastedPolicy.trim().length > 0 ? parsePolicyDescriptor(pastedPolicy) : policyConflict ? undefined : policyReports[0]?.policy ?? current;
    if (descriptor === undefined) {
      throw new AccountError({
        status: 422,
        code: "invalid_request",
        message: `Paste the output of \`${POLICY_INSPECT_COMMAND}\` from the execution host, then approve it.`,
      });
    }
    const approved = await api.approvePolicy({
      projectId,
      expectedPolicyId: current?.policy_id ?? "",
      policy: descriptor,
      onboarding: true,
    });
    setPastedPolicy("");
    await onboarding.refresh();
    return approved;
  });

  const bindRepository = useMutation(async (name: string) => {
    const current = onboarding.value;
    if (current === undefined) return null;
    if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(name)) {
      throw new AccountError({ status: 422, code: "invalid_request", message: "Enter the repository as owner/name, then retry." });
    }
    const currentIntegration = await api.integration(projectId);
    const body = { expected_revision: currentIntegration.revision, repository: name, source: "runner_checkout" };
    const bound = await withSetupKey(`${base}/onboarding/repository`, body, (key) =>
      api.bindRepository({
        projectId,
        repository: name,
        revision: currentIntegration.revision,
        key,
      }),
    );
    integration.set(bound);
    await Promise.all([onboarding.refresh(), integration.refresh()]);
    return bound;
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
  const associatedRepository = integration.value?.checkout_repository || integration.value?.repository;
  const githubApp = integration.value;
  const showGitHubApp = githubApp?.profile === "native" && associatedRepository && githubApp.github_transport_available === true && githubApp.github_app_slug && githubApp.github_app_install_url;

  const bodies: Record<string, React.ReactNode> = {
    "Repository configuration": (
      <>
        <ChoiceGroup
          name="setup-repository"
          legend="Repository configuration"
          value={repository}
          onValueChange={setRepository}
          choices={[
            {
              value: "existing",
              name: "Use an existing repository",
              help: "Choose this if the execution host already has detent.yaml and WORKFLOW.md. This saves your setup choice; the runner reads those files on the host. Associate separately verifies the runner checkout.",
              detail: "Detent reads detent.yaml and WORKFLOW.md from it",
            },
            {
              value: "generate",
              name: "Generate the configuration",
              help: "Choose this when you need starting detent.yaml and WORKFLOW.md files. This screen records that choice; create the files on the execution host before inspecting and approving the policy. Associate does not generate files.",
              detail: "Create starting detent.yaml and WORKFLOW.md on the host",
            },
          ]}
        />
        <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5 sm:flex-row sm:items-end">
          <div className="flex flex-1 flex-col gap-1.5">
            <div className="flex items-center gap-1">
              <Label htmlFor="setup-repository">Associate the runner checkout</Label>
              <ContextHelp label="Associate the runner checkout">
                Associate verifies that an enrolled runner’s checkout has a Git origin matching owner/name, including private repositories. Clone the repository on the runner and start it first; GitHub API integration is optional. This does not write configuration files or approve policy.
              </ContextHelp>
            </div>
            <Input
              id="setup-repository"
              placeholder="owner/name"
              value={repositoryName}
              onChange={(event) => setRepositoryName(event.currentTarget.value)}
              disabled={Boolean(integration.value?.checkout_repository)}
            />
            <p className="text-[13px] text-muted-foreground">
              Clone this repository on an enrolled runner, including private repositories, and start the runner. Its Git origin must match owner/name. GitHub API integration is optional.
            </p>
            {integration.value?.checkout_repository ? (
              <p className="text-[13px] text-success-foreground">Verified runner checkout: <code className="font-mono">{integration.value.checkout_repository}</code></p>
            ) : null}
          </div>
          <Button
            variant="outline"
            disabled={bindRepository.pending || repositoryName.trim().length === 0 || Boolean(integration.value?.checkout_repository)}
            onClick={() => void bindRepository.call(repositoryName.trim())}
          >
            {bindRepository.pending ? "Associating…" : "Associate"}
          </Button>
        </div>
        <ControlError message={bindRepository.error?.message ?? null} />
        <ControlError message={integration.error?.message ?? null} />
        {showGitHubApp ? (
          <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
            <p className="text-[13px] text-muted-foreground">
              New GitHub issues enter Triage automatically once the Detent Cloud GitHub App is installed on this repository or the whole organization. {" "}
              <a className="text-primary underline underline-offset-2" href={githubApp.github_app_install_url} target="_blank" rel="noopener noreferrer">Install the Detent Cloud GitHub App</a>.
            </p>
            {githubApp.github_app_installed === undefined ? null : (
              <p className="text-[13px] text-muted-foreground">
                Detent Cloud is {githubApp.github_app_installed ? "installed" : "not installed"} on {associatedRepository}
              </p>
            )}
          </div>
        ) : null}
        {onboarding.value.observed_policies?.[0] ? (
          <details className="mb-2.5 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
            <summary className="cursor-pointer text-sm">{policyConflict ? "Conflicting runner configurations: inspect and approve one shared definition" : "Review runner-reported policy"}</summary>
            <pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-all font-mono text-xs">{JSON.stringify(onboarding.value.observed_policies[0].policy, null, 2)}</pre>
          </details>
        ) : null}
        <div className="mb-2.5 flex flex-col gap-1.5 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
          <div className="flex items-center gap-1">
            <Label htmlFor="setup-policy-descriptor">Resolved policy descriptor</Label>
            <ContextHelp label="Resolved policy descriptor">
              The CLI resolves detent.yaml and WORKFLOW.md on the execution host into this descriptor. Hub validates it when you approve; runners report the policy they actually resolve. A missing approval or policy mismatch prevents claims.
            </ContextHelp>
          </div>
          <Textarea
            id="setup-policy-descriptor"
            rows={3}
            className="font-mono text-xs"
            placeholder='{"schema":1,"policy_id":"policy_…"}'
            value={pastedPolicy}
            onChange={(event) => setPastedPolicy(event.currentTarget.value)}
          />
          <p className="text-[13px] text-muted-foreground">
            The runner reports its resolved policy above. If needed, run <code className="font-mono">{POLICY_INSPECT_COMMAND}</code> on the execution host and paste its output. Approving records the descriptor you reviewed.
          </p>
        </div>
        <OptionRow
          label="Approve the resolved policy"
          name="Approve resolved policy"
          help="An owner or admin approves this exact resolved descriptor. Hub records its policy identity; runners must resolve a matching policy to claim work. Approval does not run doctor or sign in to a provider."
          detail={<span className="break-all">{onboarding.value.observed_policies?.[0]?.policy.policy_id ?? onboarding.value.policy?.policy.policy_id ?? "Waiting for the runner to report its policy"}</span>}
          right={
            <Button size="sm" disabled={approve.pending || (policyConflict && pastedPolicy.trim().length === 0)} onClick={() => void approve.call()}>
              {approve.pending ? "Approving…" : "Approve"}
            </Button>
          }
        />
        <ControlError message={approve.error?.message ?? null} />
      </>
    ),
    "Local validation": (
      <>
        {(onboarding.value.runners ?? []).length === 0 ? (
          <p className="mb-3 text-sm text-muted-foreground">Enroll a runner to observe checks on its execution host.</p>
        ) : null}
        {(onboarding.value.runners ?? []).map((entry) => {
          const checks = entry.local_checks;
          const offline = entry.runner.health !== "online";
          return (
            <div key={entry.runner.runner_id} className="mb-3">
              <p className="mb-2 text-sm font-medium">{entry.runner.display_name}{offline ? " · Offline" : ""}</p>
              {([
                ["Project checkout and configuration", "The runner checks its project checkout and configuration on the execution host. Prepare the files there, restart the runner, then refresh these observations.", checks?.checkout, "Prepare the checkout with WORKFLOW.md and detent.yaml at the path printed by registration."],
                ["detent doctor", "The runner runs detent doctor on the execution host and reports a sanitized result. Hub stores this observation, not a browser confirmation. Run doctor locally for details, restart the runner after fixes, then refresh.", checks?.doctor, "Run detent doctor --config <global.yaml> --project <project> on this host for details and fixes."],
                ["Provider sign-in", "The runner checks sign-in to the configured provider on its execution host. Credentials and account details stay local. Sign in there, restart the runner, then refresh these observations.", checks?.provider, `Sign in to ${(checks?.provider_kinds ?? []).join(" and ") || "the selected provider"} on this host, then restart the runner.`],
              ] as const).map(([name, help, state, hint]) => (
                <OptionRow key={name} label={name} name={name} help={help}
                  detail={state === "passed" ? "Observed on the execution host" : hint}
                  right={<><StatusDot tone={state === "passed" && !offline ? "ok" : "warn"} />{state === "passed" ? "Passed" : state === "failed" ? "Failed" : state === "warning" ? "Warnings" : "Waiting"}</>} />
              ))}
              {checks?.observed_at ? <p className="text-xs text-muted-foreground">Reported {new Date(checks.observed_at).toLocaleString()}. {offline ? "Reconnect the host to use these results." : "Credentials and command output stay local."}</p> : null}
            </div>
          );
        })}
        <Button variant="outline" onClick={() => void onboarding.refresh()}>Refresh runner checks</Button>
      </>
    ),
    "Execution runner": (
      <>
        <div className="mb-2 flex items-center gap-1 text-sm">
          Runner eligibility
          <ContextHelp label="Runner eligibility">
            Hub evaluates administrator-approved project access, claim permission, enabled state,
            policy selectors, runner-reported heartbeat and capacity, shared host capacity, and
            reported provider capacity. Any listed exclusion prevents new claims; runners also
            verify that their resolved policy matches approval.
          </ContextHelp>
        </div>
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
                <ContextHelp label="Route here">Add this project to the runner’s routing scope, preserving its other projects. This can resolve missing project access; it does not fix a stale heartbeat, full capacity, disabled runner, or policy mismatch.</ContextHelp>
              </>
            }
          />
        ))}
        <ControlError message={route.error?.message ?? null} />
        <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
          <div className="text-[14.5px]">Enroll a host</div>
          <p className="text-[13px] text-muted-foreground">
            Name the host and copy one command to run on it. The command creates the host's
            identity there, connects it to this project and writes its configuration.
          </p>
          <div>
            <Button variant="outline" onClick={() => setEnrolling(true)}>
              Enroll a runner
            </Button>
          </div>
          <EnrollRunnerDialog
            open={enrolling}
            onOpenChange={setEnrolling}
            fleet={fleet}
            projectIds={[projectId]}
            onEnrolled={() => void onboarding.refresh()}
            onConnected={() => void onboarding.refresh()}
          />
        </div>
        {(integration.value?.checkout_repository || integration.value?.repository) ? <GitHubIssueIntake projectId={projectId} repository={integration.value.checkout_repository || integration.value.repository || ""} runners={(onboarding.value?.runners ?? []).map(entry => ({ id: entry.runner.runner_id, name: entry.runner.display_name }))} /> : null}
      </>
    ),
    "Artifact history": (
      <>
        <ChoiceGroup
          name="setup-artifacts"
          legend="Artifact history"
          value={artifacts}
          onValueChange={setArtifacts}
          choices={[
            {
              value: "local",
              name: "Local history",
              help: "Artifacts remain on the execution host that produced them. Choose this to start without a separate artifact service; access depends on that host being available.",
              detail: "Artifacts stay on the machine that produced them",
            },
            {
              value: "customer",
              name: "Customer service",
              help: "Use an S3-compatible artifact service and independent gateway that you operate. Bind its details below. Hub records the binding but does not test storage or guarantee offline access.",
              detail: "An S3-compatible service and an independent gateway you run",
            },
          ]}
        />
        {artifacts === "customer" ? (
          <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
            <div className="flex flex-col gap-1.5">
              <div className="flex items-center gap-1">
                <Label htmlFor="setup-service-id">Service id</Label>
                <ContextHelp label="Service id">
                  The identifier for the artifact service you operate. Binding it associates that service with this project; it does not provision or verify storage.
                </ContextHelp>
              </div>
              <Input
                id="setup-service-id"
                value={serviceId}
                onChange={(event) => setServiceId(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <div className="flex items-center gap-1">
                <Label htmlFor="setup-service-origin">Gateway origin</Label>
                <ContextHelp label="Gateway origin">
                  The HTTP(S) origin of your independent artifact gateway, for example https://artifacts.example.com. Use the gateway address, not the S3 bucket URL.
                </ContextHelp>
              </div>
              <Input
                id="setup-service-origin"
                placeholder="https://artifacts.example.com"
                value={serviceOrigin}
                onChange={(event) => setServiceOrigin(event.currentTarget.value)}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <div className="flex items-center gap-1">
                <Label htmlFor="setup-service-token">Publisher token id</Label>
                <ContextHelp label="Publisher token id">
                  The id of an existing, unrevoked Hub API token with a grant for this project. Hub checks that grant when binding. Enter its id, not the secret token; your gateway and publisher still need their credentials configured.
                </ContextHelp>
              </div>
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
      .call({ repository, doctor: false, provider: false, artifacts })
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
