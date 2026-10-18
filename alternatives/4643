import { describe, expect, it } from "vitest";

import type { PolicyDescriptor } from "../../src/contracts/account.ts";
import approval from "../../src/contracts/fixtures/account-policy.json";
import { workflowChanges } from "../../src/app/account/workflowDiff.ts";

const definition: PolicyDescriptor = {
  ...approval.policy,
  workflow: { source: "detent.yaml", states: [
    { name: "Todo", dispatchable: true, terminal: false, transitions: ["Review", "Done"] },
    { name: "Review", dispatchable: false, terminal: false, transitions: ["Done"] },
    { name: "Done", dispatchable: false, terminal: true, transitions: [] },
  ] },
  configuration: { prompt: "Do the work", behavior: {
    Agent: { DispatchPriorityByState: ["Todo", "Review"], PrioritizeUnblockers: false },
    Budget: { Enabled: true, PerIssueMaxUSD: 25 },
    Gate: { Validator: { Enabled: true } },
    AllowedTransitions: { Todo: ["Review", "Done"] },
  } },
};

describe("stored workflow comparisons", () => {
  it.each([
    {
      name: "nested scheduling changes retain exact old and new values",
      next: { ...definition, configuration: { ...definition.configuration!, behavior: {
        Agent: { DispatchPriorityByState: ["Review", "Todo"], PrioritizeUnblockers: true },
        Budget: { Enabled: true, PerIssueMaxUSD: 50 },
        Gate: { Validator: { Enabled: true } },
      } } },
      expected: [
        { field: "Agent · Dispatch priority by state", before: '["Todo","Review"]', after: '["Review","Todo"]' },
        { field: "Agent · Prioritize unblockers", before: "false", after: "true" },
        { field: "Budget · Per issue max usd", before: "25", after: "50" },
      ],
    },
    {
      name: "transition target order and prompt edits do not change scheduling",
      next: { ...definition, workflow: { ...definition.workflow!, states: [
        { ...definition.workflow!.states[0]!, transitions: ["Done", "Review", "Done"] },
        ...definition.workflow!.states.slice(1),
      ] }, configuration: { ...definition.configuration!, prompt: "Different instructions" } },
      expected: [],
    },
    {
      name: "operator-only roles stay distinct from nondispatchable roles",
      next: { ...definition, workflow: { ...definition.workflow!, states: [
        definition.workflow!.states[0]!, { ...definition.workflow!.states[1]!, operator_only: true }, definition.workflow!.states[2]!,
      ] } },
      expected: [{ field: "Lane: Review", before: "Nondispatchable", after: "Nondispatchable · Operator only" }],
    },
    {
      name: "removed scheduling values are explicit",
      next: { ...definition, configuration: { ...definition.configuration!, behavior: {
        Agent: { DispatchPriorityByState: ["Todo", "Review"], PrioritizeUnblockers: false },
        Budget: { Enabled: true }, Gate: { Validator: { Enabled: true } },
      } } },
      expected: [{ field: "Budget · Per issue max usd", before: "25", after: "Not set" }],
    },
  ])("$name", ({ next, expected }) => {
    expect(workflowChanges(definition, next)).toEqual(expected);
  });

  it("keeps definition digests out of the legacy scheduling comparison", () => {
    const before = { ...definition, configuration: undefined };
    expect(workflowChanges(before, { ...before, gates: { ...before.gates, plan_stop_digest: "another-digest" } })).toEqual([]);
  });
});
