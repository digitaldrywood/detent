import { describe, expect, it } from "vitest";

import type { NativeLabel } from "../../src/contracts/work.ts";
import {
  assigneeSections,
  labelSections,
  managedLabels,
  matchesQuery,
  prioritySections,
  relatedSections,
  statusSections,
  userLabels,
  type PickerSection,
} from "../../src/app/work/lib/issuePickers.ts";

const STATES = [
  { name: "Todo", category: "unstarted" },
  { name: "In Progress", category: "started" },
  { name: "In Review", category: "started" },
  { name: "Done", category: "completed" },
];

const CATALOGUE: readonly NativeLabel[] = [
  { name: "bug", color: "#c05b6b", count: 9 },
  { name: "chore", color: "#3f9e6f", count: 4 },
  { name: "goal:reliability", color: "#6e79d6", count: 2 },
  { name: "docs", color: "#4d94bb", count: 1 },
  { name: "effort:medium", color: "#000000", count: 7 },
];

/** Every row of every section, flattened, which is what a reader sees. */
function rows<A>(sections: readonly PickerSection<A>[]) {
  return sections.flatMap((section) => section.options);
}

function keys<A>(sections: readonly PickerSection<A>[]) {
  return rows(sections).map((option) => option.key);
}

describe("matchesQuery", () => {
  it("folds case, trims and takes any of the values", () => {
    for (const test of [
      { name: "empty matches", query: "  ", values: ["Todo"], want: true },
      { name: "case folded", query: "TODO", values: ["Todo"], want: true },
      { name: "substring", query: "prog", values: ["In Progress"], want: true },
      { name: "second value", query: "done", values: ["wi_1", "Done"], want: true },
      { name: "no match", query: "zzz", values: ["Todo"], want: false },
      { name: "undefined value", query: "x", values: [undefined], want: false },
    ]) {
      expect(matchesQuery(test.query, ...test.values), test.name).toBe(test.want);
    }
  });
});

describe("the status picker", () => {
  it("lists every workflow state in order, with the current one checked", () => {
    const sections = statusSections({
      states: STATES,
      current: "Todo",
      moves: ["In Progress", "Done"],
      query: "",
    });
    expect(keys(sections)).toEqual(["Todo", "In Progress", "In Review", "Done"]);
    expect(rows(sections).filter((option) => option.selected === true).map((o) => o.key)).toEqual([
      "Todo",
    ]);
  });

  it("numbers the rows by the workflow's order", () => {
    const sections = statusSections({ states: STATES, current: "Todo", moves: [], query: "" });
    expect(rows(sections).map((option) => option.digit)).toEqual(["1", "2", "3", "4"]);
  });

  it("keeps an unreachable state on the list with its reason", () => {
    const sections = statusSections({
      states: STATES,
      current: "Todo",
      moves: ["In Progress"],
      query: "",
    });
    const done = rows(sections).find((option) => option.key === "Done");
    expect(done?.disabled).toBe(true);
    expect(done?.detail).toBe("Not allowed from Todo");
    const allowed = rows(sections).find((option) => option.key === "In Progress");
    expect(allowed?.disabled).toBe(false);
    expect(allowed?.detail).toBeUndefined();
  });

  it("says the current state cannot be moved to, so taking it is a no-op", () => {
    const sections = statusSections({
      states: STATES,
      current: "Todo",
      moves: ["Done"],
      query: "",
    });
    expect(rows(sections).find((option) => option.key === "Todo")?.action).toEqual({
      state: "Todo",
      moves: false,
    });
    expect(rows(sections).find((option) => option.key === "Done")?.action).toEqual({
      state: "Done",
      moves: true,
    });
  });

  it("filters on what the reader typed", () => {
    const sections = statusSections({
      states: STATES,
      current: "Todo",
      moves: [],
      query: "in ",
    });
    expect(keys(sections)).toEqual(["In Progress", "In Review"]);
  });

  it("stops numbering after nine states, rather than offering a two-key digit", () => {
    const many = Array.from({ length: 11 }, (_, index) => ({
      name: `State ${index}`,
      category: "unstarted",
    }));
    const sections = statusSections({ states: many, current: "State 0", moves: [], query: "" });
    expect(rows(sections).at(8)?.digit).toBe("9");
    expect(rows(sections).at(9)?.digit).toBeUndefined();
  });
});

describe("the priority picker", () => {
  it("offers No priority first, then Detent's four names, numbered from zero", () => {
    const sections = prioritySections({ current: null, query: "" });
    expect(keys(sections)).toEqual(["none", "Urgent", "High", "Normal", "Low"]);
    expect(rows(sections).map((option) => option.label)).toEqual([
      "No priority",
      "Urgent",
      "High",
      "Normal",
      "Low",
    ]);
    expect(rows(sections).map((option) => option.digit)).toEqual(["0", "1", "2", "3", "4"]);
  });

  it("checks the level the issue has, and No priority when it has none", () => {
    for (const test of [
      { name: "a level", current: "High", want: "High" },
      { name: "none", current: null, want: "none" },
    ]) {
      const sections = prioritySections({ current: test.current, query: "" });
      const checked = rows(sections).filter((option) => option.selected === true);
      expect(checked.map((option) => option.key), test.name).toEqual([test.want]);
    }
  });

  it("asks for a removal rather than a level when No priority is taken", () => {
    const sections = prioritySections({ current: "Urgent", query: "" });
    expect(rows(sections).find((option) => option.key === "none")?.action).toEqual({ name: null });
    expect(rows(sections).find((option) => option.key === "Low")?.action).toEqual({ name: "Low" });
  });

  it("filters on what the reader typed", () => {
    expect(keys(prioritySections({ current: null, query: "no pri" }))).toEqual(["none"]);
    expect(keys(prioritySections({ current: null, query: "urg" }))).toEqual(["Urgent"]);
  });
});

describe("the assignee picker", () => {
  const members = [
    { id: "owner@example.test", label: "owner@example.test" },
    { id: "viewer@example.test", label: "viewer@example.test" },
  ];
  const viewer = { id: "owner@example.test", label: "owner@example.test" };
  const base = {
    members,
    viewer,
    canInvite: true,
    inviteReason: "Only an owner or an administrator can invite somebody.",
    query: "",
  };

  it("puts No assignee and the reader first, then the team, then the invitation", () => {
    const sections = assigneeSections({ ...base, assignees: [] });
    expect(sections.map((section) => section.label)).toEqual(["", "Team members", "New user"]);
    expect(sections[0]!.options.map((option) => option.key)).toEqual([
      "none",
      "owner@example.test",
    ]);
    // The reader is in the first group, so they are not repeated in the team.
    expect(sections[1]!.options.map((option) => option.key)).toEqual(["viewer@example.test"]);
    expect(sections[2]!.options.map((option) => option.key)).toEqual(["invite"]);
  });

  it("numbers No assignee 0 and the reader 1, as Linear does", () => {
    const sections = assigneeSections({ ...base, assignees: [] });
    expect(sections[0]!.options.map((option) => option.digit)).toEqual(["0", "1"]);
  });

  it("checks No assignee on an unassigned issue and the member on an assigned one", () => {
    expect(
      rows(assigneeSections({ ...base, assignees: [] }))
        .filter((option) => option.selected === true)
        .map((option) => option.key),
    ).toEqual(["none"]);
    expect(
      rows(assigneeSections({ ...base, assignees: ["viewer@example.test"] }))
        .filter((option) => option.selected === true)
        .map((option) => option.key),
    ).toEqual(["viewer@example.test"]);
  });

  it("assigns one member, and unassigns when the assigned one is taken again", () => {
    const unassigned = assigneeSections({ ...base, assignees: [] });
    expect(
      rows(unassigned).find((option) => option.key === "viewer@example.test")?.action,
    ).toEqual({ kind: "assign", assignees: ["viewer@example.test"] });

    const assigned = assigneeSections({ ...base, assignees: ["viewer@example.test"] });
    expect(rows(assigned).find((option) => option.key === "viewer@example.test")?.action).toEqual({
      kind: "assign",
      assignees: [],
    });
  });

  it("lists the invitation with its reason rather than hiding it", () => {
    const allowed = rows(assigneeSections({ ...base, assignees: [] })).find(
      (option) => option.key === "invite",
    );
    expect(allowed?.disabled).toBe(false);
    expect(allowed?.detail).toBeUndefined();

    const refused = rows(assigneeSections({ ...base, assignees: [], canInvite: false })).find(
      (option) => option.key === "invite",
    );
    expect(refused?.disabled).toBe(true);
    expect(refused?.detail).toBe("Only an owner or an administrator can invite somebody.");
  });

  it("still offers the invitation when the search matches no member", () => {
    const sections = assigneeSections({ ...base, assignees: [], query: "nobody" });
    expect(sections[0]!.options).toEqual([]);
    expect(sections[1]!.options).toEqual([]);
    expect(sections[2]!.options.map((option) => option.key)).toEqual(["invite"]);
  });

  it("offers only No assignee when the hub served no members", () => {
    const sections = assigneeSections({ ...base, assignees: [], members: [], viewer: null });
    expect(keys(sections)).toEqual(["none", "invite"]);
  });
});

describe("the label picker", () => {
  it("suggests the busiest labels the issue does not carry, and only with no query", () => {
    const sections = labelSections({ attached: ["bug"], catalogue: CATALOGUE, query: "" });
    expect(sections[0]!.label).toBe("Suggestions");
    expect(sections[0]!.options.map((option) => option.key)).toEqual(["chore", "goal:reliability", "docs"]);
    // A suggested label is not repeated in the catalogue group below it.
    expect(sections[1]!.options.map((option) => option.key)).toEqual(["bug"]);

    const typed = labelSections({ attached: ["bug"], catalogue: CATALOGUE, query: "o" });
    expect(typed[0]!.options).toEqual([]);
    expect(typed[1]!.options.map((option) => option.key)).toEqual(["chore", "goal:reliability", "docs"]);
  });

  it("never offers a label the hub owns", () => {
    const sections = labelSections({ attached: ["effort:medium"], catalogue: CATALOGUE, query: "" });
    expect(keys(sections)).not.toContain("effort:medium");
  });

  it("checks what the issue carries and toggles it off when taken again", () => {
    const sections = labelSections({ attached: ["bug", "effort:low"], catalogue: CATALOGUE, query: "" });
    const bug = rows(sections).find((option) => option.key === "bug");
    expect(bug?.selected).toBe(true);
    // The managed label is not in the set a toggle writes: the component adds
    // it back, so a label edit cannot drop the issue's effort.
    expect(bug?.action).toEqual({ kind: "toggle", color: "#c05b6b", labels: [] });

    const chore = rows(sections).find((option) => option.key === "chore");
    expect(chore?.selected).toBe(false);
    expect(chore?.action).toEqual({ kind: "toggle", color: "#3f9e6f", labels: ["bug", "chore"] });
  });

  it("offers to create a name the catalogue has never seen", () => {
    const sections = labelSections({ attached: ["bug"], catalogue: CATALOGUE, query: " flaky " });
    const create = rows(sections).find((option) => option.key === "create:flaky");
    expect(create?.label).toBe('Create label "flaky"');
    expect(create?.action).toEqual({
      kind: "create",
      name: "flaky",
      color: "",
      labels: ["bug", "flaky"],
    });
  });

  it("does not offer to create what already exists, whatever the case", () => {
    for (const query of ["bug", "BUG", " Bug "]) {
      const sections = labelSections({ attached: [], catalogue: CATALOGUE, query });
      expect(keys(sections).some((key) => key.startsWith("create:")), query).toBe(false);
    }
  });

  it("refuses to create a label under a prefix the hub owns", () => {
    const sections = labelSections({ attached: [], catalogue: CATALOGUE, query: "priority:high" });
    expect(keys(sections).some((key) => key.startsWith("create:"))).toBe(false);
  });

  it("offers to create the first label of a project with an empty catalogue", () => {
    const sections = labelSections({ attached: [], catalogue: [], query: "flaky" });
    expect(keys(sections)).toEqual(["create:flaky"]);
  });
});

describe("the related picker", () => {
  const candidates = [
    { id: "wi_1", label: "Lease renewal under load", detail: "Todo", category: "unstarted" },
    { id: "wi_2", label: "Retire the legacy poller", detail: "Done", category: "completed" },
    { id: "wi_3", label: "Checkout lock renewal", detail: "Todo", category: "unstarted" },
  ];

  it("offers the project's other issues", () => {
    const sections = relatedSections({ candidates, relatedIds: [], self: "wi_9", query: "" });
    expect(keys(sections)).toEqual(["wi_1", "wi_2", "wi_3"]);
    expect(rows(sections)[1]?.action).toEqual({ id: "wi_2", category: "completed" });
  });

  it("never offers this issue, nor one that is already related", () => {
    const sections = relatedSections({
      candidates,
      relatedIds: ["wi_2"],
      self: "wi_3",
      query: "",
    });
    expect(keys(sections)).toEqual(["wi_1"]);
  });

  it("searches the title, the state and the identifier", () => {
    for (const test of [
      { name: "title", query: "legacy", want: ["wi_2"] },
      { name: "state", query: "done", want: ["wi_2"] },
      { name: "identifier", query: "wi_3", want: ["wi_3"] },
      { name: "nothing", query: "zzz", want: [] },
    ]) {
      const sections = relatedSections({ candidates, relatedIds: [], self: "", query: test.query });
      expect(keys(sections), test.name).toEqual(test.want);
    }
  });

  it("stops at thirty results rather than rendering the whole project", () => {
    const many = Array.from({ length: 60 }, (_, index) => ({
      id: `wi_${index}`,
      label: `Issue ${index}`,
      detail: "Todo",
      category: "unstarted",
    }));
    const sections = relatedSections({ candidates: many, relatedIds: [], self: "", query: "" });
    expect(keys(sections)).toHaveLength(30);
  });
});

describe("splitting an issue's labels", () => {
  it("keeps what a reader may attach and sets aside what the hub owns", () => {
    for (const test of [
      { name: "a plain label", labels: ["bug"], user: ["bug"], managed: [] },
      { name: "an effort", labels: ["effort:low"], user: [], managed: ["effort:low"] },
      { name: "a priority", labels: ["priority:high"], user: [], managed: ["priority:high"] },
      {
        name: "a reserved label",
        labels: ["detent:coordinator"],
        user: [],
        managed: ["detent:coordinator"],
      },
      { name: "another namespace", labels: ["goal:x"], user: ["goal:x"], managed: [] },
      {
        name: "a mix",
        labels: ["bug", "effort:low", "epic:1"],
        user: ["bug", "epic:1"],
        managed: ["effort:low"],
      },
    ]) {
      expect(userLabels(test.labels), `${test.name}: user`).toEqual(test.user);
      expect(managedLabels(test.labels), `${test.name}: managed`).toEqual(test.managed);
    }
  });
});
