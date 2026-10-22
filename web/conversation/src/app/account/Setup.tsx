import React from "react";
import { CheckIcon, CircleIcon, TriangleAlertIcon } from "lucide-react";
import { Spinner } from "../../components/ui/spinner.tsx";
import { formatRelativeTime } from "../fleet/format.ts";
import { GitHubIssueIntake } from "./IssueIntake.tsx";

import { Avatar } from "../../components/ui/avatar.tsx";
import { useSidebar } from "../../components/ui/sidebar.tsx";
import { useSidebarData } from "../adapters/sidebarData.tsx";
import { useClient } from "../client.ts";
import { formatPageTitle } from "../pageTitle.ts";
import { ProjectSetupContext, ProjectSetupProvider } from "./projectSetup.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import type { OnboardingStep } from "../../contracts/account.ts";
import { cn } from "../../lib/utils.ts";
import { AccountError } from "./api.ts";
import { ContextHelp } from "../components/ContextHelp.tsx";
import { EnrollRunnerDialog } from "../fleet/EnrollRunner.tsx";
import { ControlError, StatusDot } from "./controls.tsx";
import { useAccountApi } from "./context.ts";
import { DetentCloudLogo } from "./Login.tsx";
import { retireKey, setupKey } from "./idempotency.ts";
import { useMutation, useResource } from "./useResource.ts";

import { firstUnreadyStep, orderedSteps } from "./setupSteps.ts";
export { firstUnreadyStep, orderedSteps } from "./setupSteps.ts";

export function Stepper({
  steps,
  activeIndex,
  onSelect,
  summaries = {},
}: {
  readonly steps: readonly OnboardingStep[];
  readonly activeIndex: number;
  readonly onSelect: (index: number) => void;
  readonly summaries?: Readonly<Record<string, string>>;
}): React.ReactElement {
  return (
    <ol
      aria-label="Setup steps"
      className="mt-[18px] grid grid-cols-2 gap-1.5 sm:grid-cols-4 rounded-[10px] border border-border/60 bg-muted p-1"
    >
      {steps.map((step, index) => {
        const active = index === activeIndex;
        return (
          <li key={step.name} className="min-w-0">
            <button
              type="button"
              aria-current={active ? "step" : undefined}
              aria-label={`${step.name} ${step.state === "ready" ? "ready" : "action required"}`}
              aria-description={summaries[step.name]}
              onClick={() => onSelect(index)}
              className={cn(
                "flex h-full min-h-14 w-full min-w-0 items-start gap-1.5 rounded-lg px-2 py-2 text-left text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring sm:flex-col",
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
              <span className="min-w-0 flex-1 break-words sm:w-full">
                <span className="block font-medium">{step.name}</span>
                {summaries[step.name] ? <span className="mt-0.5 block text-xs text-muted-foreground">{summaries[step.name]}</span> : null}
              </span>
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
  readonly name: React.ReactNode;
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
            {choice.help === undefined ? null : <ContextHelp label={typeof choice.name === "string" ? choice.name : legend}>{choice.help}</ContextHelp>}
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
  projectName,
  organizationName,
  summaries,
}: {
  readonly steps: readonly OnboardingStep[];
  readonly activeIndex: number;
  readonly onSelectStep: (index: number) => void;
  readonly title: string;
  readonly lede: string;
  readonly children: React.ReactNode;
  readonly footer: React.ReactNode;
  readonly projectName?: string;
  readonly organizationName?: string;
  readonly summaries?: Readonly<Record<string, string>>;
}): React.ReactElement {
  return (
    // Not `place-items-center`: centring a card taller or wider than the
    // viewport pushes its start past the scroll origin, where it cannot be
    // scrolled to. `my-auto` centres it only while it fits.
    <div className="flex min-h-0 flex-1 flex-col items-center overflow-y-auto px-4 py-6 sm:p-10">
      <div className="my-auto w-full min-w-0 max-w-[640px] rounded-2xl border border-border bg-card shadow-[0_30px_80px_rgb(0_0_0/45%)]">
        <div className="border-b border-border/60 px-5 pt-[26px] pb-[18px] sm:px-7">
          <DetentCloudLogo />
          {projectName ? (
            <section aria-label="Project setup context" className="mt-4 flex flex-wrap items-center gap-3 rounded-xl border border-border/60 bg-muted px-3 py-3">
              <Avatar name={projectName} size="md" className="rounded-lg bg-background" />
              <div className="min-w-0 flex-1 break-words">
                <p className="text-xs text-muted-foreground">Setting up a project in {organizationName}</p>
                <p className="text-sm font-semibold">{projectName}</p>
              </div>
              <SwitchProject />
            </section>
          ) : null}
          {steps.length > 0 ? <Stepper steps={steps} activeIndex={activeIndex} onSelect={onSelectStep} summaries={summaries} /> : null}
        </div>
        <div className="px-5 pt-6 pb-[26px] sm:px-7">
          <h1 className="mb-2 text-2xl font-semibold tracking-[-0.02em] text-balance">{title}</h1>
          <p className="mb-[18px] text-sm text-muted-foreground">{lede}</p>
          {children}
          <div className="mt-[18px] flex flex-wrap justify-end gap-2">{footer}</div>
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

function SwitchProject(): React.ReactElement | null {
  const sidebar = useSidebarData();
  return sidebar?.projectSwitcher ? <SwitchProjectButton open={sidebar.projectSwitcher.setOpen} /> : null;
}

function SwitchProjectButton({ open }: { readonly open: (open: boolean) => void }): React.ReactElement {
  const { isMobile, setOpenMobile, setOpen } = useSidebar();
  return <Button variant="link" size="sm" onClick={() => {
    if (isMobile) setOpenMobile(true);
    else setOpen(true);
    open(true);
  }} className="text-xs text-muted-foreground underline">Switch project</Button>;
}

export function SetupRoute(props: {
  readonly projectId: string;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const setup = React.useContext(ProjectSetupContext);
  return setup?.projectId === props.projectId
    ? <SetupContent {...props} />
    : <ProjectSetupProvider key={props.projectId} projectId={props.projectId}><SetupContent {...props} /></ProjectSetupProvider>;
}

function SetupContent({ projectId, onNavigate }: {
  readonly projectId: string;
  readonly onNavigate?: (to: string) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const client = useClient();
  const { onboarding, integration } = React.useContext(ProjectSetupContext)!;
  const projectName = client.bootstrap.projects.find((project) => project.id === projectId)?.name;
  const organizationName = client.bootstrap.organization.name;
  React.useEffect(() => {
    document.title = formatPageTitle(projectName ? `${projectName} setup` : "Project setup");
  }, [projectName]);

  const steps = orderedSteps(onboarding.value, integration.value);
  const runners = React.useMemo(() => [...(onboarding.value?.runners ?? [])].sort((a, b) => Number((b.runner.connection_health ?? b.runner.health) === "online") - Number((a.runner.connection_health ?? a.runner.health) === "online")), [onboarding.value?.runners]);
  const [selectedRunnerId, setSelectedRunnerId] = React.useState("");
  const runner = runners.find((entry) => entry.runner.runner_id === selectedRunnerId)
    ?? runners.find((entry) => entry.runner.state === "active" && (entry.runner.connection_health ?? entry.runner.health) === "online" && !(entry.exclusions ?? []).some((exclusion) => !["project_access_denied", "host_capacity", "runner_capacity"].includes(exclusion.code)))
    ?? runners.find((entry) => entry.runner.state === "active");
  const readySummaries: Record<string, string> = {
    Repository: integration.value?.checkout_repository || integration.value?.repository || "Repository connected",
    Runner: runner?.runner.display_name ?? "Runner connected",
    Checkout: onboarding.value?.runners?.some((entry) => entry.local_checks?.doctor === "warning" && entry.local_checks.checkout === "passed" && entry.local_checks.provider === "passed") ? "Checks passed with warnings" : "All checks passed",
    "Run files": onboarding.value?.progress.artifacts === "customer" ? "Your bucket and gateway" : `Kept on ${runner?.runner.display_name ?? "your runner"}`,
  };
  const prompts: Record<string, string> = {
    Runner: "Choose a computer",
    Repository: "Pick a repository",
    Checkout: "Check your runner",
    "Run files": "Choose storage",
  };
  const [activeIndex, setActiveIndex] = React.useState(0);
  const summaries = Object.fromEntries(steps.map((step, index) => [step.name,
    step.state === "ready" ? readySummaries[step.name] ?? "Ready" : index === activeIndex ? prompts[step.name] ?? "Complete this step" : "—",
  ]));
  const [importing, setImporting] = React.useState(false);
  const [showReady, setShowReady] = React.useState(false);
  const [settled, setSettled] = React.useState(false);
  React.useEffect(() => {
    if (onboarding.value === undefined || integration.value === undefined || settled) return;
    setActiveIndex(firstUnreadyStep(orderedSteps(onboarding.value, integration.value)));
    setShowReady(onboarding.value.ready && orderedSteps(onboarding.value, integration.value).every((step) => step.state === "ready"));
    setSettled(true);
  }, [onboarding.value, integration.value, settled]);

  // The editable progress, seeded from the hub and written back whole: the
  // endpoint replaces the object, so sending half of it would clear the rest.
  const progress = onboarding.value?.progress;
  const savedRepository = progress?.repository;
  const savedArtifacts = progress?.artifacts;
  const [repository, setRepository] = React.useState("");
  const [artifacts, setArtifacts] = React.useState("local");
  React.useEffect(() => {
    if (savedRepository === undefined || savedArtifacts === undefined) return;
    setRepository(savedRepository);
    setArtifacts(savedArtifacts || "local");
  }, [projectId, savedRepository, savedArtifacts]);

  const [repositoryName, setRepositoryName] = React.useState("");
  const [enrollingChoice, setEnrolling] = React.useState<boolean | null>(null);
  const enrolling = enrollingChoice ?? (onboarding.value !== undefined && runners.length === 0);
  const enrollmentBaseline = React.useRef<Set<string> | null>(null);
  React.useEffect(() => {
    if (!enrolling) { enrollmentBaseline.current = null; return; }
    if (onboarding.value === undefined) return;
    if (enrollmentBaseline.current === null) {
      enrollmentBaseline.current = new Set(runners.map((entry) => entry.runner.runner_id));
      setEnrolling(true);
      return;
    }
    const connected = runners.find((entry) => !enrollmentBaseline.current!.has(entry.runner.runner_id)
      && entry.runner.state === "active" && (entry.runner.connection_health ?? entry.runner.health) === "online"
      && !(entry.exclusions ?? []).some((exclusion) => exclusion.code === "project_access_denied"));
    if (connected) {
      setSelectedRunnerId(connected.runner.runner_id);
      setEnrolling(false);
      setActiveIndex(1);
    }
  }, [enrolling, onboarding.value, runners]);
  const fleet = useResource(() => enrolling ? api.fleet() : Promise.resolve(undefined), [api, enrolling]);
  const [serviceId, setServiceId] = React.useState("");
  const [serviceOrigin, setServiceOrigin] = React.useState("");
  const [servicePublisher, setServicePublisher] = React.useState("");

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
      const updated = await api.onboarding(projectId);
      onboarding.set(updated);
      return updated;
    },
  );

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
    const found = await api.runnerRouting(runner).catch((cause: unknown) => {
      if (cause instanceof AccountError && cause.status === 404) {
        throw new AccountError({
          status: 404,
          code: "not_found",
          message: "Runner access changed. Reload this project.",
        });
      }
      throw cause;
    });
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
      projectIds: found["scope"] === "organization" ? [] : projectIds.includes(projectId) ? projectIds : [...projectIds, projectId],
      scope: found["scope"] === "organization" ? "organization" : "projects",
      projectRanks: found["project_ranks"] as Record<string, number> | undefined,
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

  if (onboarding.error !== null && onboarding.error.isAccessError) {
    return (
      <WizardCard
        projectName={projectName}
        organizationName={organizationName}
        summaries={summaries}
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
  const needsAction = step?.state === "action_required";
  const next = () => setActiveIndex((index) => Math.min(index + 1, steps.length - 1));
  const back = () => setActiveIndex((index) => Math.max(index - 1, 0));
  const checkoutRepository = integration.value?.checkout_repository;
  const associatedRepository = checkoutRepository || integration.value?.repository;
  const checkoutVerified = integration.value?.checkout_verified === true;
  const preparingRunner = runner;
  const configurationMissing = preparingRunner?.local_checks?.configuration === "missing";
  const runnerName = preparingRunner?.runner.display_name ?? "Your runner";
  const requestChecks = () => void saveProgress.call({ repository: progress?.repository ?? "", doctor: progress?.doctor ?? false, provider: progress?.provider ?? false, artifacts });
  const githubApp = integration.value;
  const showGitHubApp = githubApp?.profile === "native" && associatedRepository && githubApp.github_transport_available === true && githubApp.github_app_slug && githubApp.github_app_install_url;

  const bodies: Record<string, React.ReactNode> = {
    "Repository": (
      <>
        <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5 sm:flex-row sm:items-end">
          <div className="flex flex-1 flex-col gap-1.5">
            <div className="flex items-center gap-1">
              <Label htmlFor="setup-repository">Which repository should Detent work on?</Label>
              <ContextHelp label="Associate the runner checkout">
                This connects your project to a repository your runner can download using its Git sign-in. Enter owner/name and click Associate; private repositories work too, without a GitHub integration.
              </ContextHelp>
            </div>
            <Input
              id="setup-repository"
              placeholder="owner/name"
              value={checkoutRepository || repositoryName}
              onChange={(event) => setRepositoryName(event.currentTarget.value)}
              disabled={Boolean(integration.value?.checkout_repository)}
            />
            <p className="text-[13px] text-muted-foreground">
              Enter your repository's owner/name; your runner downloads it using its Git sign-in. Private repositories work too, without a GitHub integration.
            </p>
            {integration.value?.checkout_repository ? (
              <p role="status" className={`text-[13px] ${checkoutVerified ? "text-success-foreground" : "text-muted-foreground"}`}>
                {checkoutVerified ? "Verified runner checkout: " : `Waiting for ${preparingRunner?.runner.display_name || "an enrolled runner"} to clone or verify checkout: `}
                <code className="font-mono">{integration.value.checkout_repository}</code>
              </p>
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
      </>
    ),
    "Checkout": (
      <>
        {!preparingRunner ? <p className="mb-3 text-sm text-muted-foreground">Enroll a runner first to check that its computer is ready.</p> : (() => {
          const checks = preparingRunner.local_checks;
          const offline = (preparingRunner.runner.connection_health ?? preparingRunner.runner.health) !== "online";
          const configuration = checks?.configuration ?? (checks?.checkout === "passed" ? "passed" : "pending");
          const rows = [
            { name: `Cloned ${associatedRepository ?? "repository"}`, state: checks?.checkout, at: checks?.checkout_at, detail: `Using ${runnerName}’s Git sign-in`, hint: checks?.checkout_fix || "Check the runner’s Git sign-in and repository access, then retry." },
            { name: configurationMissing ? "detent.yaml and WORKFLOW.md not found" : "Found detent.yaml and WORKFLOW.md", state: configuration, at: checks?.configuration_at, detail: configurationMissing ? "Looked in the repository root" : "Project settings loaded from the repository", hint: "Check detent.yaml and WORKFLOW.md in the repository, then retry." },
            { name: "detent doctor", state: checks?.doctor, at: checks?.doctor_at, detail: configurationMissing ? "Waiting for settings" : `Checking build tools on ${runnerName}`, hint: "Run detent doctor --config <global.yaml> --project <project> on your runner’s computer to see what needs fixing." },
            { name: "AI coding tool sign-in", state: checks?.provider, at: checks?.provider_at, detail: configurationMissing ? "Waiting for settings" : `Sign-in stays on ${runnerName}`, hint: `Sign in to ${(checks?.provider_kinds ?? []).join(" and ") || "the AI coding tool chosen for this project"} on your runner’s computer, then retry.` },
          ];
          return <>
            <ul aria-label="Runner checks" className="divide-y divide-border/60 overflow-hidden rounded-xl border border-border/60 bg-muted" aria-live="polite">
              {rows.map((row) => {
                const state = row.state ?? "pending";
                const status = state === "passed" ? "Passed" : state === "running" ? "Running" : state === "failed" ? "Failed" : state === "missing" ? "Missing" : state === "warning" ? "Warnings" : "Waiting";
                const observed = row.at || checks?.observed_at;
                return <li key={row.name} className="flex items-start gap-3 px-3.5 py-3">
                  <span className="mt-0.5 shrink-0" aria-hidden="true">
                    {state === "passed" ? <CheckIcon className="size-5 rounded-full bg-success/15 p-0.5 text-success-foreground" /> : state === "running" ? <Spinner size="lg" /> : ["failed", "missing", "warning"].includes(state) ? <TriangleAlertIcon className="size-5 text-warning-foreground" /> : <CircleIcon className="size-5 text-muted-foreground" />}
                  </span>
                  <div className="min-w-0 flex-1 text-sm">
                    <p className="break-words">{row.name}</p>
                    <p className="break-words text-xs text-muted-foreground">{state === "failed" ? row.hint : row.detail}</p>
                    {state === "failed" && row.name.startsWith("Cloned") && checks?.checkout_message ? <p className="text-xs text-muted-foreground">{checks.checkout_message}</p> : null}
                    {state === "failed" ? <Button variant="link" size="sm" disabled={saveProgress.pending} onClick={requestChecks} aria-label={`Retry ${row.name}`}>Retry</Button> : null}
                  </div>
                  <div className="shrink-0 text-right text-xs text-muted-foreground">
                    <span>{status}</span>
                    {observed && !["pending", "running"].includes(state) ? <time className="mt-0.5 block" dateTime={observed}>{formatRelativeTime(observed)}</time> : null}
                  </div>
                </li>;
              })}
            </ul>
            {offline ? <p role="status" className="mt-2 text-sm text-muted-foreground">Reconnect {runnerName} to continue checking.</p> : null}
            {configurationMissing ? <section className="mt-3">
              <ChoiceGroup name="setup-repository" legend="Configuration files" value={repository || "generate"} onValueChange={setRepository} choices={[
                { value: "generate", name: `Create starter files on ${runnerName}`, detail: `Written into ${runnerName}’s checkout for you to review and commit` },
                { value: "existing", name: "I’ll add them myself", detail: `${runnerName} picks them up on its next check` },
              ]} />
              {progress?.repository ? <p role="status" className="text-sm text-muted-foreground">{progress.repository === "generate" ? `Waiting for ${runnerName} to create starter files.` : `Add and commit the files. ${runnerName} will check again automatically.`}</p> : null}
            </section> : <div className="mt-2"><Button variant="link" size="sm" disabled={saveProgress.pending} onClick={requestChecks}>Refresh runner checks</Button></div>}
          </>;
        })()}
      </>
    ),
    "Runner": enrolling ? (
      <EnrollRunnerDialog inline open onOpenChange={setEnrolling} fleet={fleet} projectIds={[projectId]}
        onEnrolled={() => void onboarding.refresh()} onConnected={() => void onboarding.refresh()} />
    ) : (
      <>
        <ChoiceGroup name="setup-runner" legend="Computer for this project" value={runner?.runner.runner_id ?? ""} onValueChange={setSelectedRunnerId}
          choices={runners.map((entry) => {
            const online = (entry.runner.connection_health ?? entry.runner.health) === "online";
            const count = entry.other_project_count ?? entry.runner.project_ids?.filter((id) => id !== projectId).length ?? 0;
            return {
              value: entry.runner.runner_id,
              name: <span className="flex flex-wrap items-center justify-between gap-2"><span>{entry.runner.display_name}</span><span className="flex items-center gap-1.5 text-xs text-muted-foreground"><StatusDot tone={online ? "ok" : "idle"} />{online ? "Online" : "Offline"}</span></span>,
              detail: <>{entry.runner.hostname ?? entry.runner.runner_id} · works on {count} other {count === 1 ? "project" : "projects"}{online ? "" : " · starts work when it reconnects"}</>,
            };
          })} />
        <OptionRow label="Use a different computer" name="Use a different computer" detail="Enroll a new runner with one command"
          right={<Button variant="outline" onClick={() => setEnrolling(true)}>Enroll a runner</Button>} />
        <ControlError message={route.error?.message ?? null} />
      </>
    ),
    "Run files": (
      <>
        <ChoiceGroup
          name="setup-artifacts"
          legend="Run files"
          value={artifacts}
          onValueChange={setArtifacts}
          choices={[
            {
              value: "local",
              name: `Keep them on ${runner?.runner.display_name ?? "your runner"}`,
              help: "Keep logs, screenshots and other run files on your runner, with no separate storage account or fee. You can view them while that computer is available; losing it also loses those files.",
              detail: "Keep run files on your runner, with no separate storage fee",
            },
            {
              value: "customer",
              name: "Your bucket and gateway",
              help: "Keep run files in storage you run and pay for: an S3-compatible bucket and a gateway, the service that lets you view those files. Enter the details below; connecting it does not check that storage works or ensure access when your runner is offline.",
              detail: "Keep run files in storage you manage and pay for",
            },
          ]}
        />
        {artifacts === "customer" ? (
          <div className="mb-2.5 flex flex-col gap-2 rounded-xl border border-border/60 bg-muted px-4 py-3.5">
            <div className="flex flex-col gap-1.5">
              <div className="flex items-center gap-1">
                <Label htmlFor="setup-service-id">Service id</Label>
                <ContextHelp label="Service id">
                  Enter the ID from the service you set up to store run files. This connects it to the project; it does not create storage or check that it works.
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
                  Enter the web address of your gateway, the service that lets you view stored run files, for example https://artifacts.example.com. Use that address, not the S3 bucket URL.
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
                  Enter the ID of an active Detent API token that has access to this project, not the secret token itself. Set up the sign-in details for your storage service separately.
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
                variant={needsAction ? "default" : "outline"}
                disabled={bindArtifacts.pending || serviceId.trim().length === 0}
                onClick={() => void bindArtifacts.call()}
              >
                {bindArtifacts.pending ? "Binding…" : "Bind the service"}
              </Button>
            </div>
            <ControlError message={bindArtifacts.error?.message ?? null} />
            <p className="text-[13px] text-muted-foreground">
              Connecting storage saves where your run files will go. Check that your storage service
              works; connecting it does not ensure access when your runner is offline.
            </p>
          </div>
        ) : null}
      </>
    ),
  };

  const saveAndContinue = () => {
    void saveProgress.call({ repository, doctor: progress?.doctor ?? false, provider: progress?.provider ?? false, artifacts }).then((saved) => {
      if (saved === null) return;
      if (activeIndex < steps.length - 1) next();
      else if (onboarding.value?.ready && steps.every((candidate) => candidate.state === "ready")) setShowReady(true);
      else onNavigate?.("/work");
    });
  };
  const finishSetup = async () => {
    const saved = await saveProgress.call({ repository, doctor: progress?.doctor ?? false, provider: progress?.provider ?? false, artifacts });
    if (saved === null) return;
    if (saved.ready) setShowReady(true);
    else onNavigate?.("/work");
  };
  const titles: Record<string, string> = {
    Runner: enrolling ? `Connect a computer to ${projectName ?? "this project"}` : `Which computer should work on ${projectName ?? "this project"}?`,
    Repository: "Repository",
    Checkout: configurationMissing ? `${associatedRepository ?? "This repository"} has no Detent settings yet` : `${runnerName} is preparing ${associatedRepository ?? "the repository"}`,
    "Run files": "Where should run files go?",
  };
  const selectStep = (index: number) => { setShowReady(false); setActiveIndex(index); };
  const ready = showReady && onboarding.value.ready && steps.every((candidate) => candidate.state === "ready");
  return (
    <WizardCard projectName={projectName} organizationName={organizationName} summaries={summaries}
      steps={ready ? [] : steps} activeIndex={activeIndex} onSelectStep={selectStep}
      title={ready ? `${projectName ?? "This project"} is ready for work` : titles[step?.name ?? ""] ?? "Set up this project"}
      lede={ready ? `New issues in this project go to ${runner?.runner.display_name ?? "your runner"}. You can change any of this in project settings.`
        : step?.name === "Runner" && enrolling ? "Name it, then run the command in a terminal on that computer."
        : step?.name === "Checkout" ? configurationMissing ? `${runnerName} cloned it but found no project configuration. Pick one and the checks continue on their own.` : "The runner clones the repository and checks itself. This page updates as each check finishes; sign-in details and command output stay on that computer."
        : step?.name === "Run files" ? `Logs, screenshots and other files from each run of ${projectName ?? "this project"}.` : step?.detail ?? ""}
      footer={ready ? <Button onClick={() => onNavigate?.("/work")}>Open Work</Button> : <>
        {step?.name === "Runner" ? <>
          {enrolling ? <Button variant="outline" onClick={() => setEnrolling(false)}>Use an existing runner</Button> : null}
          <Button variant="outline" onClick={next}>Skip for now</Button>
          {!enrolling ? <Button disabled={!runner || route.pending} onClick={() => {
            if (runner) { setSelectedRunnerId(runner.runner.runner_id); void route.call(runner.runner.runner_id).then((routed) => { if (routed !== null) next(); }); }
          }}>{route.pending ? "Routing…" : `Use ${runner?.runner.display_name ?? "runner"}`}</Button> : null}
        </> : <>
          <Button variant="outline" onClick={back}>Back</Button>
          {step?.name === "Checkout" ? <>
            {configurationMissing ? <Button disabled={saveProgress.pending} onClick={() => void saveProgress.call({ repository: repository || "generate", doctor: progress?.doctor ?? false, provider: progress?.provider ?? false, artifacts })}>{saveProgress.pending ? "Saving…" : repository === "existing" ? "Wait for files" : "Create starter files"}</Button> : <>
              <Button variant="outline" className="order-last w-full sm:order-none sm:w-auto" onClick={next}>Continue in background</Button>
              <Button disabled={needsAction} onClick={next}>Continue</Button>
            </>}
          </> : step?.name === "Run files" ? <Button disabled={saveProgress.pending || artifacts === "customer" && (onboarding.value.artifact_services ?? []).length === 0} onClick={() => void finishSetup()}>{saveProgress.pending ? "Saving…" : "Finish setup"}</Button>
            : <Button variant={needsAction ? "outline" : "default"} disabled={saveProgress.pending} onClick={saveAndContinue}>{saveProgress.pending ? "Saving…" : needsAction ? "Skip for now" : "Continue"}</Button>}
        </>}
      </>}
    >
      {ready ? <>
        <dl className="divide-y divide-border/60 rounded-xl border border-border/60 bg-muted">
          {steps.map((candidate, index) => <div key={candidate.name} className="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
            <dt className="w-20 text-muted-foreground">{candidate.name}</dt><dd className="min-w-0 flex-1 break-words">{readySummaries[candidate.name]}</dd>
            <Button variant="link" size="sm" aria-label={`${candidate.name === "Checkout" ? "View" : "Change"} ${candidate.name}`} onClick={() => selectStep(index)}>{candidate.name === "Checkout" ? "View" : "Change"}</Button>
          </div>)}
        </dl>
        {associatedRepository ? <div className="mt-3">
          <OptionRow label="Bring in GitHub issues" name="Bring in GitHub issues" detail={`Optional: import open issues from ${associatedRepository}`}
            right={<Button variant="outline" onClick={() => setImporting((open) => !open)}>{importing ? "Hide import" : "Import issues"}</Button>} />
          {importing ? <GitHubIssueIntake projectId={projectId} repository={associatedRepository} runners={runners.map((entry) => ({ id: entry.runner.runner_id, name: entry.runner.display_name }))} /> : null}
        </div> : null}
      </> : <>{bodies[step?.name ?? ""] ?? null}</>}
      <ControlError message={saveProgress.error?.message ?? null} />
    </WizardCard>
  );
}
