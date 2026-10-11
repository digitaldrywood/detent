import { CheckIcon, RocketIcon } from "lucide-react";
import { useNavigate } from "@tanstack/react-router";
import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "../../../components/ui/empty.tsx";
import { cn } from "../../../lib/utils.ts";
import type { OnboardingStepState } from "../../../contracts/account.ts";
import { useAccountApi, useAccountBootstrap } from "../../account/context.ts";
import { useResource } from "../../account/useResource.ts";
import { EnrollRunnerDialog } from "../../fleet/EnrollRunner.tsx";
import { PROJECT_CREATION_UNAVAILABLE, useNewProject } from "../../projects/NewProject.tsx";
import { NewIssueDialog } from "../NewIssue.tsx";

export { firstIssueState } from "../NewIssue.tsx";

export type FirstRunStepId = "project" | "runner" | "setup" | "issue";

export interface FirstRunFacts {
  readonly projects: number;
  readonly runners: number | "loading" | "unavailable";
  readonly runnerState: OnboardingStepState | "loading" | "unavailable";
  readonly issues: number;
  /**
   * The Hub's unready onboarding steps for the project this board is on, the
   * same count the setup wizard and Settings → Projects show.
   */
  readonly setupStepsLeft: number | "loading" | "unavailable";
  readonly canManageProjects: boolean;
  readonly canEnrollRunners: boolean;
  readonly canWriteIssues: boolean;
}

export interface FirstRunStep {
  readonly id: FirstRunStepId;
  readonly title: string;
  readonly description: string;
  readonly actionLabel: string;
  readonly done: boolean;
  readonly blockedReason: string | null;
  readonly note: string | null;
}

export const NEEDS_PROJECT = "Create a project first";
export const RUNNER_ENROLLMENT_UNAVAILABLE = "An organization owner or admin enrolls runners";
export const RUNNER_ACCESS_NEEDED =
  "Enrolling a runner needs runner access on every project. Grant it to yourself in Settings → Organization";
export const RUNNERS_LOADING = "Checking this organization's runners";
export const RUNNERS_UNAVAILABLE = "The runner list could not be read. Reload to try again";
export const ISSUE_CREATION_UNAVAILABLE = "You have read-only access to this project";
export const SETUP_LOADING = "Checking this project's setup";
export const SETUP_UNAVAILABLE = "The project's setup could not be read. Reload to try again";
export const SETUP_NEEDS_ADMIN = "An organization owner or admin finishes project setup";

export function setupStepsLeftLabel(count: number): string {
  return `${count} setup step${count === 1 ? "" : "s"} left`;
}

export function firstRunSteps(facts: FirstRunFacts): readonly FirstRunStep[] {
  const hasProject = facts.projects > 0;
  const configureRunner = hasProject && typeof facts.runners === "number" && facts.runners > 0;
  const needsRunnerAccess = hasProject && !facts.canEnrollRunners && facts.canManageProjects;
  return [
    {
      id: "project",
      title: "Create a project",
      description: "A project holds the board, the workflow and the runners that work on it.",
      actionLabel: "New project",
      done: hasProject,
      blockedReason: facts.canManageProjects ? null : PROJECT_CREATION_UNAVAILABLE,
      note: null,
    },
    {
      id: "runner",
      title: "Enroll a runner",
      description:
        "Configure this project on a runner to take issue runs on your own machine, with your own provider login.",
      actionLabel: needsRunnerAccess ? "Grant runner access" : configureRunner ? "Configure runner" : "Enroll a runner",
      done: hasProject && facts.runnerState === "ready",
      blockedReason: !hasProject
        ? NEEDS_PROJECT
        : facts.runnerState === "loading"
          ? SETUP_LOADING
          : facts.runnerState === "unavailable"
            ? SETUP_UNAVAILABLE
            : facts.runners === "loading"
              ? RUNNERS_LOADING
              : facts.runners === "unavailable"
                ? RUNNERS_UNAVAILABLE
                : facts.canEnrollRunners || needsRunnerAccess
                  ? null
                  : RUNNER_ENROLLMENT_UNAVAILABLE,
      note: needsRunnerAccess && typeof facts.runners === "number" ? RUNNER_ACCESS_NEEDED : null,
    },
    {
      id: "setup",
      title: "Finish project setup",
      description:
        "Approve the repository policy, check the runner host, and choose where artifact history lives.",
      actionLabel: "Finish setup",
      done: facts.setupStepsLeft === 0,
      blockedReason: !hasProject
        ? NEEDS_PROJECT
        : facts.setupStepsLeft === "loading"
          ? SETUP_LOADING
          : facts.setupStepsLeft === "unavailable"
            ? SETUP_UNAVAILABLE
            : facts.setupStepsLeft > 0 && !facts.canManageProjects
              ? SETUP_NEEDS_ADMIN
              : null,
      note:
        hasProject && typeof facts.setupStepsLeft === "number" && facts.setupStepsLeft > 0
          ? setupStepsLeftLabel(facts.setupStepsLeft)
          : null,
    },
    {
      id: "issue",
      title: "Create your first issue",
      description: "Describe the first piece of work and it lands on this board.",
      actionLabel: "New issue",
      done: facts.issues > 0,
      blockedReason: !hasProject
        ? NEEDS_PROJECT
        : facts.canWriteIssues
          ? null
          : ISSUE_CREATION_UNAVAILABLE,
      note: null,
    },
  ];
}

export function FirstRunChecklist({
  steps,
  onAction,
}: {
  readonly steps: readonly FirstRunStep[];
  readonly onAction: (id: FirstRunStepId) => void;
}): React.ReactElement {
  const done = steps.filter((step) => step.done).length;
  return (
    <Empty className="flex-none gap-5 px-4 py-10 md:py-14" data-testid="first-run">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <RocketIcon />
        </EmptyMedia>
        <EmptyTitle className="text-lg" role="heading" aria-level={2}>
          Set up your organization
        </EmptyTitle>
        <EmptyDescription>
          These steps put the first issue on this board and a runner on it.
        </EmptyDescription>
      </EmptyHeader>
      <div className="flex w-full max-w-xl flex-col gap-2">
        <div className="flex items-center justify-between text-muted-foreground text-xs">
          <span id="first-run-progress-label">Setup progress</span>
          <span className="tabular-nums" data-testid="first-run-progress">
            {done} of {steps.length} done
          </span>
        </div>
        <div
          role="progressbar"
          aria-labelledby="first-run-progress-label"
          aria-valuemin={0}
          aria-valuemax={steps.length}
          aria-valuenow={done}
          aria-valuetext={`${done} of ${steps.length} done`}
          className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
        >
          <div
            className="h-full rounded-full bg-primary transition-[width]"
            style={{ width: `${(done / Math.max(steps.length, 1)) * 100}%` }}
          />
        </div>
        <ol className="mt-2 flex flex-col text-pretty rounded-xl border border-border/60 bg-card/40 text-left shadow-xs/5 [&>*+*]:border-border/50 [&>*+*]:border-t">
          {steps.map((step, index) => (
            <li
              key={step.id}
              data-testid={`first-run-step-${step.id}`}
              data-done={step.done ? "true" : "false"}
              className="flex flex-col gap-3 px-4 py-3.5 sm:flex-row sm:items-center sm:gap-4"
            >
              <div className="flex min-w-0 flex-1 items-start gap-3">
                <span
                  aria-hidden
                  className={cn(
                    "mt-0.5 grid size-[22px] shrink-0 place-items-center rounded-full border text-xs",
                    step.done
                      ? "border-success/60 bg-success/15 text-success-foreground"
                      : "border-border text-muted-foreground",
                  )}
                >
                  {step.done ? <CheckIcon className="size-3" /> : index + 1}
                </span>
                <div className="min-w-0 flex-1 space-y-0.5">
                  <h3 className="font-medium text-foreground text-sm">
                    {step.title}
                    <span className="sr-only">{step.done ? ", done" : ", not done"}</span>
                  </h3>
                  <p className="text-[13px] text-muted-foreground leading-[1.45]">
                    {step.description}
                  </p>
                  {!step.done && (step.blockedReason ?? step.note) !== null ? (
                    <p
                      className="text-muted-foreground/80 text-xs"
                      id={`first-run-${step.id}-note`}
                    >
                      {step.blockedReason ?? step.note}.
                    </p>
                  ) : null}
                </div>
              </div>
              <div className="flex shrink-0 ps-[34px] sm:ps-0">
                {step.done ? (
                  <span className="font-medium text-success-foreground text-xs">Done</span>
                ) : (
                  <Button
                    size="sm"
                    variant={step.blockedReason === null ? "default" : "outline"}
                    disabled={step.blockedReason !== null}
                    aria-describedby={
                      (step.blockedReason ?? step.note) === null
                        ? undefined
                        : `first-run-${step.id}-note`
                    }
                    onClick={() => onAction(step.id)}
                  >
                    {step.actionLabel}
                  </Button>
                )}
              </div>
            </li>
          ))}
        </ol>
      </div>
    </Empty>
  );
}

export function FirstRunPanel({
  projectId,
  issues,
  onIssueCreated,
}: {
  readonly projectId: string | null;
  readonly issues: number;
  readonly onIssueCreated: () => void;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const newProject = useNewProject();
  const navigate = useNavigate();
  const fleet = useResource(() => api.fleet(), [api]);
  const [enrolling, setEnrolling] = React.useState(false);
  const [creatingIssue, setCreatingIssue] = React.useState(false);

  const projects = bootstrap?.projects ?? [];
  const target =
    projectId === null
      ? (projects.find((project) => project.can_write) ?? projects[0])
      : projects.find((project) => project.id === projectId);
  const canEnrollRunners = bootstrap?.actor.can_manage_runners ?? false;
  const targetId = target?.id ?? null;
  // Tagged with the project it was read for: while the board moves to another
  // project, or if that read fails, the previous project's count must not be
  // shown as this one's.
  const setup = useResource(
    async () => (targetId === null ? null : { projectId: targetId, onboarding: await api.onboarding(targetId) }),
    [api, targetId],
  );
  const onboarding = setup.value?.projectId === targetId ? setup.value.onboarding : undefined;

  const steps = firstRunSteps({
    projects: projects.length,
    runners:
      fleet.value !== undefined
        ? fleet.value.runners.length
        : fleet.error !== null
          ? "unavailable"
          : "loading",
    runnerState:
      onboarding !== undefined
        ? onboarding.steps.find((step) => step.name === "Execution runner")?.state ?? "unavailable"
        : setup.error !== null && !setup.loading
          ? "unavailable"
          : "loading",
    issues,
    setupStepsLeft:
      onboarding !== undefined
        ? onboarding.steps.filter((step) => step.state !== "ready").length
        : setup.error !== null && !setup.loading
          ? "unavailable"
          : "loading",
    canManageProjects: newProject.canCreate,
    canEnrollRunners,
    canWriteIssues: target?.can_write ?? false,
  });

  const refreshFleet = fleet.refresh;
  const refreshSetup = setup.refresh;
  const onEnrollOpenChange = React.useCallback(
    (open: boolean) => {
      setEnrolling(open);
      if (!open) {
        void refreshFleet();
        void refreshSetup();
      }
    },
    [refreshFleet, refreshSetup],
  );

  return (
    <>
      <FirstRunChecklist
        steps={steps}
        onAction={(id) => {
          if (id === "project") newProject.openNewProject();
          if (id === "runner" && canEnrollRunners) {
            if (fleet.value !== undefined && fleet.value.runners.length > 0 && targetId !== null) {
              void navigate({ to: "/projects/$project/setup", params: { project: targetId } } as never);
            } else {
              setEnrolling(true);
            }
          }
          if (id === "runner" && !canEnrollRunners) {
            void navigate({ to: "/settings/$section", params: { section: "organization" } });
          }
          if (id === "setup" && targetId !== null) {
            void navigate({ to: "/projects/$project/setup", params: { project: targetId } } as never);
          }
          if (id === "issue") setCreatingIssue(true);
        }}
      />
      {canEnrollRunners && projects.length > 0 ? (
        <EnrollRunnerDialog
          open={enrolling}
          onOpenChange={onEnrollOpenChange}
          fleet={fleet}
          onEnrolled={() => void refreshFleet()}
          onConnected={() => void refreshSetup()}
        />
      ) : null}
      {target === undefined ? null : (
        <NewIssueDialog
          key={target.id}
          open={creatingIssue}
          onOpenChange={setCreatingIssue}
          projects={[target]}
          projectId={target.id}
          description={`The first card on ${target.name}'s board. A runner picks it up from its lane.`}
          onCreated={() => onIssueCreated()}
        />
      )}
    </>
  );
}
