import type { PolicyDescriptor, WorkflowState } from "../../contracts/account.ts";

export interface WorkflowChange {
  readonly field: string;
  readonly before: string;
  readonly after: string;
}

function role(state: WorkflowState): string {
  return `${state.terminal ? "Terminal" : state.dispatchable ? "Dispatchable" : "Nondispatchable"}${state.operator_only ? " · Operator only" : ""}`;
}

function label(key: string): string {
  const words = key.replace(/([a-z0-9])([A-Z])/g, "$1 $2").replaceAll("_", " ").toLowerCase();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

function flatten(value: unknown, path: string, result: Map<string, string>): void {
  if (value !== null && typeof value === "object" && !Array.isArray(value)) {
    for (const [key, child] of Object.entries(value)) flatten(child, `${path} · ${label(key)}`, result);
  } else {
    result.set(path, Array.isArray(value) ? JSON.stringify(value) : value === null ? "None" : String(value));
  }
}

const SCHEDULING_FIELDS = [
  "Review", "Gate", "Plan", "Budget", "Dependencies", "Recovery", "BacklogAdmission",
  "DependencyAutoUnblock", "BlockedRecovery", "BlockerAutoPromote", "Operator", "PriorityMap",
] as const;
const AGENT_SCHEDULING_FIELDS = [
  "DispatchPriorityByState", "DispatchPriorityByLabel", "PrioritizeUnblockers", "MergeFastPath",
  "AutoPromote", "Budget", "StopRun", "MaxRetryBackoffMS", "OverloadRetryDelayMS",
  "LifetimeSessionLimit", "LifetimeTokenLimit", "LifetimeLimitOverrideLabel", "FailureBreaker",
] as const;

function scheduling(definition: PolicyDescriptor | undefined): Map<string, string> {
  const result = new Map<string, string>();
  if (!definition) return result;
  const behavior = definition.configuration?.behavior;
  if (behavior !== null && typeof behavior === "object" && !Array.isArray(behavior)) {
    const values = behavior as Record<string, unknown>;
    for (const field of SCHEDULING_FIELDS) {
      if (field in values) flatten(values[field], label(field), result);
    }
    const agent = values.Agent;
    if (agent !== null && typeof agent === "object" && !Array.isArray(agent)) {
      const fields = agent as Record<string, unknown>;
      for (const field of AGENT_SCHEDULING_FIELDS) {
        if (field in fields) flatten(fields[field], `Agent · ${label(field)}`, result);
      }
    }
  } else {
    const { plan_stop_digest: _, ...gates } = definition.gates;
    flatten(gates, "Execution", result);
  }
  flatten(definition.requirements, "Runner requirements", result);
  return result;
}

export function workflowChanges(before: PolicyDescriptor | undefined, after: PolicyDescriptor): WorkflowChange[] {
  const result: WorkflowChange[] = [];
  const add = (field: string, previous: string, next: string) => {
    if (previous !== next) result.push({ field, before: previous, after: next });
  };
  const previousStates = before?.workflow?.states ?? [];
  const nextStates = after.workflow?.states ?? [];
  const previous = new Map(previousStates.map((state) => [state.name, state]));
  const next = new Map(nextStates.map((state) => [state.name, state]));
  for (const name of new Set([...previous.keys(), ...next.keys()])) {
    const oldState = previous.get(name);
    const newState = next.get(name);
    add(`Lane: ${name}`, oldState ? role(oldState) : "Not present", newState ? role(newState) : "Removed");
  }
  add("Lane order", previousStates.map((state) => state.name).join(" → ") || "None", nextStates.map((state) => state.name).join(" → ") || "None");
  for (const name of new Set([...previous.keys(), ...next.keys()])) {
    const targets = (state: WorkflowState | undefined) => [...new Set(state?.transitions ?? [])].sort().join(", ") || "None";
    add(`Transitions from ${name}`, targets(previous.get(name)), targets(next.get(name)));
  }
  const oldSettings = scheduling(before);
  const newSettings = scheduling(after);
  for (const field of [...new Set([...oldSettings.keys(), ...newSettings.keys()])].sort()) {
    add(field, oldSettings.get(field) ?? "Not set", newSettings.get(field) ?? "Not set");
  }
  return result;
}
