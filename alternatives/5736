import type {
  EnvironmentId,
  OrchestrationProjectShell,
  OrchestrationThreadShell,
} from "../../contracts/index.ts";

export interface EnvironmentProject extends OrchestrationProjectShell {
  readonly environmentId: EnvironmentId;
}

export interface EnvironmentThreadShell extends OrchestrationThreadShell {
  readonly environmentId: EnvironmentId;
}

export interface EnvironmentThread extends EnvironmentThreadShell {
  readonly messages?: readonly unknown[];
  readonly activities?: readonly unknown[];
  readonly checkpoints?: readonly unknown[];
  readonly proposedPlans?: readonly unknown[];
  readonly deletedAt?: string | null;
}

export function scopeProject(
  environmentId: EnvironmentId,
  project: OrchestrationProjectShell,
): EnvironmentProject {
  return { ...project, environmentId };
}

export function scopeThreadShell(
  environmentId: EnvironmentId,
  thread: OrchestrationThreadShell,
): EnvironmentThreadShell {
  return { ...thread, environmentId };
}

export function scopeThread(
  environmentId: EnvironmentId,
  thread: EnvironmentThread,
): EnvironmentThread {
  return { ...thread, environmentId };
}
