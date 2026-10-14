import { isManagedLabel, PRIORITY_NAMES, type NativeLabel } from "../../../contracts/work.ts";

/** One row, before it is given a glyph and a handler. */
export interface PickerOption<A> {
  readonly key: string;
  readonly label: string;
  /** A second line: a related issue's state, or why a row cannot be taken. */
  readonly detail?: string;
  /** Linear's digit hint, which is also a shortcut inside the open picker. */
  readonly digit?: string;
  readonly selected?: boolean;
  readonly disabled?: boolean;
  /** What taking the row means. The component turns it into a call. */
  readonly action: A;
}

/** A named run of rows. `""` is Linear's first, unheaded group. */
export interface PickerSection<A> {
  readonly label: string;
  readonly options: readonly PickerOption<A>[];
}

/** Case-folded substring match over whatever a row is searchable by. */
export function matchesQuery(query: string, ...values: readonly (string | undefined)[]): boolean {
  const needle = query.trim().toLowerCase();
  if (needle.length === 0) return true;
  return values.some((value) => value !== undefined && value.toLowerCase().includes(needle));
}

/** The labels a reader may attach: everything the hub does not own itself. */
export function userLabels(labels: readonly string[]): readonly string[] {
  return labels.filter((label) => !isManagedLabel(label));
}

/** The labels the hub owns, kept on every write so an edit cannot drop them. */
export function managedLabels(labels: readonly string[]): readonly string[] {
  return labels.filter((label) => isManagedLabel(label));
}

// --- Status -----------------------------------------------------------------

export interface StateOption {
  readonly name: string;
  /** `unstarted`, `started` or `completed`; drives the ring. */
  readonly category: string;
}

export interface StatusAction {
  readonly state: string;
  /** False where taking the row would change nothing. */
  readonly moves: boolean;
}

/**
 * Every state in the workflow's own order.
 *
 * A state the workflow does not allow from here stays on the list with the
 * reason rather than disappearing (decisions.md §16) — a workflow that will
 * not take a move is a fact about the workflow, and a reader who cannot see
 * the state at all cannot learn it. Digits follow the workflow's order, so
 * the number beside a state is the number it always has.
 */
export function statusSections(input: {
  readonly states: readonly StateOption[];
  readonly current: string;
  readonly moves: readonly string[];
  readonly query: string;
}): readonly PickerSection<StatusAction>[] {
  const allowed = new Set(input.moves);
  const options = input.states.map((state, index) => {
    const current = state.name === input.current;
    const reachable = current || allowed.has(state.name);
    return {
      key: state.name,
      label: state.name,
      ...(index < 9 ? { digit: String(index + 1) } : {}),
      selected: current,
      disabled: !reachable,
      ...(reachable ? {} : { detail: `Not allowed from ${input.current}` }),
      action: { state: state.name, moves: !current },
    };
  });
  return [{ label: "", options: options.filter((option) => matchesQuery(input.query, option.label)) }];
}

// --- Priority ---------------------------------------------------------------

export interface PriorityAction {
  /** `null` is "No priority", which removes the priority from the issue. */
  readonly name: string | null;
}

/**
 * "No priority" first and digit 0, as Linear has it, then the four levels.
 *
 * The first row is the one the hub could not offer until its patch learned to
 * clear a field (`tracker.PriorityPatch`); before that the picker had to say
 * a priority could not be removed once set.
 */
export function prioritySections(input: {
  readonly current: string | null;
  readonly query: string;
}): readonly PickerSection<PriorityAction>[] {
  const names: readonly (string | null)[] = [null, ...PRIORITY_NAMES];
  const options = names.map((name, index) => ({
    key: name ?? "none",
    label: name ?? "No priority",
    digit: String(index),
    selected: input.current === name,
    action: { name },
  }));
  return [{ label: "", options: options.filter((option) => matchesQuery(input.query, option.label)) }];
}

// --- Assignee ---------------------------------------------------------------

/** An organization member the assignee picker can assign to. */
export interface MemberOption {
  /** What goes in the issue's `assignees` list. The member's email. */
  readonly id: string;
  readonly label: string;
}

export type AssigneeAction =
  | { readonly kind: "assign"; readonly assignees: readonly string[] }
  | { readonly kind: "invite" };

/**
 * "No assignee", the reader, the organization's members, and the invitation.
 *
 * Assigning is a single-assignee operation here even though the work item
 * carries a list: Linear's picker assigns one person, and a hub that models
 * several has no second control to set them. Taking the assigned member again
 * unassigns, which is how their picker toggles.
 *
 * The invitation row is listed whether or not this reader may use it, with
 * the reason when they may not (§16).
 */
export function assigneeSections(input: {
  readonly assignees: readonly string[];
  readonly members: readonly MemberOption[];
  readonly viewer: MemberOption | null;
  readonly canInvite: boolean;
  readonly inviteReason: string;
  readonly query: string;
}): readonly PickerSection<AssigneeAction>[] {
  const assigned = new Set(input.assignees);
  const toggle = (id: string): AssigneeAction => ({
    kind: "assign",
    assignees: assigned.has(id) ? [] : [id],
  });

  const first: PickerOption<AssigneeAction>[] = [];
  if (matchesQuery(input.query, "No assignee", "Unassigned")) {
    first.push({
      key: "none",
      label: "No assignee",
      digit: "0",
      selected: input.assignees.length === 0,
      action: { kind: "assign", assignees: [] },
    });
  }
  const viewer = input.viewer;
  if (viewer !== null && matchesQuery(input.query, viewer.label, viewer.id)) {
    first.push({
      key: viewer.id,
      label: viewer.label,
      digit: "1",
      selected: assigned.has(viewer.id),
      action: toggle(viewer.id),
    });
  }

  const team = input.members
    .filter((member) => member.id !== viewer?.id)
    .filter((member) => matchesQuery(input.query, member.label, member.id))
    .map((member) => ({
      key: member.id,
      label: member.label,
      selected: assigned.has(member.id),
      action: toggle(member.id),
    }));

  return [
    { label: "", options: first },
    { label: "Team members", options: team },
    {
      label: "New user",
      options: [
        {
          key: "invite",
          label: "Invite and assign…",
          ...(input.canInvite ? {} : { detail: input.inviteReason }),
          disabled: !input.canInvite,
          action: { kind: "invite" as const },
        },
      ],
    },
  ];
}

// --- Labels -----------------------------------------------------------------

export type LabelAction =
  | { readonly kind: "toggle"; readonly labels: readonly string[] }
  | { readonly kind: "create"; readonly name: string; readonly labels: readonly string[] };

export interface LabelOptionExtra {
  readonly color: string;
}

/** How many labels Linear's Suggestions group holds. */
export const LABEL_SUGGESTION_LIMIT = 3;

/**
 * Suggestions, then the catalogue, then the offer to make a new one.
 *
 * Suggestions are the busiest labels the issue does not already carry, and
 * only with an empty query: once the reader is typing, the one list they are
 * filtering is the whole catalogue. Typing a name the project has never used
 * offers to create it — which, with no label table behind the hub, is exactly
 * attaching the name.
 *
 * `attached` is the issue's *user* labels: a managed prefix is a field with a
 * row of its own and is never a label to attach or take off here.
 */
export function labelSections(input: {
  readonly attached: readonly string[];
  readonly catalogue: readonly NativeLabel[];
  readonly query: string;
}): readonly PickerSection<LabelAction & LabelOptionExtra>[] {
  const attached = input.attached.filter((label) => !isManagedLabel(label));
  const held = new Set(attached);
  const catalogue = input.catalogue.filter((label) => !isManagedLabel(label.name));

  const option = (label: NativeLabel) => ({
    key: label.name,
    label: label.name,
    selected: held.has(label.name),
    action: {
      kind: "toggle" as const,
      color: label.color,
      labels: held.has(label.name)
        ? attached.filter((candidate) => candidate !== label.name)
        : [...attached, label.name],
    },
  });

  const typed = input.query.trim();
  const suggestions =
    typed.length > 0
      ? []
      : catalogue.filter((label) => !held.has(label.name)).slice(0, LABEL_SUGGESTION_LIMIT);
  const suggested = new Set(suggestions.map((label) => label.name));
  const rest = catalogue
    .filter((label) => !suggested.has(label.name))
    .filter((label) => matchesQuery(input.query, label.name));

  const create =
    typed.length > 0 &&
    !isManagedLabel(typed) &&
    !catalogue.some((label) => label.name.toLowerCase() === typed.toLowerCase())
      ? [
          {
            key: `create:${typed}`,
            label: `Create label "${typed}"`,
            action: {
              kind: "create" as const,
              name: typed,
              color: "",
              labels: [...attached, typed],
            },
          },
        ]
      : [];

  return [
    { label: "Suggestions", options: suggestions.map(option) },
    { label: "Labels", options: rest.map(option) },
    { label: "", options: create },
  ];
}

// --- Related ----------------------------------------------------------------

/** A work item "Add related…" can point at. */
export interface RelatedCandidate {
  readonly id: string;
  readonly label: string;
  readonly detail: string;
  readonly category: string;
}

export interface RelatedAction {
  readonly id: string;
  readonly category: string;
}

/** How many candidates the list holds before the reader has to narrow it. */
export const RELATED_RESULT_LIMIT = 30;

/**
 * The project's other work items, minus this one and the ones already
 * related. Offering either would produce a relation the hub refuses — a
 * dependency cannot be its own cycle — or a second copy of a row that is
 * already on the column.
 */
export function relatedSections(input: {
  readonly candidates: readonly RelatedCandidate[];
  readonly relatedIds: readonly string[];
  readonly self: string;
  readonly query: string;
}): readonly PickerSection<RelatedAction>[] {
  const linked = new Set(input.relatedIds);
  const options = input.candidates
    .filter((candidate) => candidate.id !== input.self && !linked.has(candidate.id))
    .filter((candidate) => matchesQuery(input.query, candidate.label, candidate.detail, candidate.id))
    .slice(0, RELATED_RESULT_LIMIT)
    .map((candidate) => ({
      key: candidate.id,
      label: candidate.label,
      detail: candidate.detail,
      action: { id: candidate.id, category: candidate.category },
    }));
  return [{ label: "", options }];
}
