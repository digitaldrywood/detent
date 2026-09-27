import {
  BoxIcon,
  GaugeIcon,
  GitPullRequestIcon,
  HistoryIcon,
  LinkIcon,
  MessageSquareIcon,
  PlusIcon,
  UserPlusIcon,
  XIcon,
} from "lucide-react";
import React from "react";

import { resolvePullRequestState } from "../../../components/pullRequest/pullRequestPresentation.tsx";
import {
  useKeybindingActions,
  type KeybindingActions,
} from "../../adapters/keybindingActions.ts";
import { DEFAULT_BINDINGS, type KeybindingCommand } from "../../adapters/keybindings.ts";
import { cn } from "../../../lib/utils.ts";
import type { NativeLabel } from "../../../contracts/work.ts";
import type { WorkItemView } from "../lib/model.ts";
import {
  assigneeSections,
  labelSections,
  managedLabels,
  prioritySections,
  relatedSections,
  statusSections,
  userLabels,
  type MemberOption,
  type PickerOption,
  type RelatedCandidate,
  type StateOption,
} from "../lib/issuePickers.ts";
import {
  AssigneeAvatar,
  LabelDot,
  NoAssigneeGlyph,
  PriorityGlyph,
  StateGlyph,
} from "./issueGlyphs.tsx";
import { PropertyPicker, type PickerRow } from "./IssuePickers.tsx";

export { PriorityGlyph, StateGlyph } from "./issueGlyphs.tsx";
export { userLabels } from "../lib/issuePickers.ts";
export type { MemberOption, RelatedCandidate, StateOption } from "../lib/issuePickers.ts";

/** Which picker is open. One at a time, as Linear has it. */
export type IssuePickerName = "state" | "priority" | "assignee" | "labels" | "related";

/** A sibling issue the Related group lists, and what can be done with it. */
export interface RelatedEntry {
  readonly key: string;
  readonly kind: "issue" | "chat";
  readonly label: string;
  readonly detail?: string;
  /** The lane category of the related issue, for its ring. */
  readonly category?: string;
  readonly onOpen: () => void;
  /** Null where the relation is not one this page can take off. */
  readonly onRemove?: (() => void) | null;
}

export interface IssuePropertiesProps {
  readonly item: WorkItemView;
  /** Every state in the workflow's own order, for the status picker. */
  readonly states: readonly StateOption[];
  /** The states the workflow allows from here. */
  readonly moves: readonly string[];
  readonly laneCategory: string;
  readonly onMove: (state: string) => void;
  /** `null` is "No priority", which removes the priority from the issue. */
  readonly onPriority: (name: string | null) => void;
  /** The whole user-label set the issue should carry after the edit. */
  readonly onLabels: (labels: readonly string[]) => void;
  /** The project's label catalogue, or an empty list while it loads. */
  readonly labelCatalogue: readonly NativeLabel[];
  readonly onAssignees: (assignees: readonly string[]) => void;
  /** The organization's members. Empty where the hub did not answer. */
  readonly members: readonly MemberOption[];
  /** The signed-in reader, for Linear's "self-assign" row. */
  readonly viewer: MemberOption | null;
  /** Opens the invitation flow, or null when this reader cannot invite. */
  readonly onInvite: (() => void) | null;
  /** Why inviting is unavailable. Shown on the row rather than hiding it. */
  readonly inviteReason: string;
  /** False for a read-only reader: every control goes rather than greys out. */
  readonly canWrite: boolean;
  readonly saving: boolean;
  /** The runner holding the issue right now, or null. */
  readonly runner: string | null;
  /** `low · Codex gpt-6-astra`, from the attempt and the effort label. */
  readonly effort: string;
  readonly attempts: string;
  /** Sibling issues from the history relations, and the originating chat. */
  readonly related: readonly RelatedEntry[];
  /** The project's other work items, for "Add related…". */
  readonly relatedCandidates: readonly RelatedCandidate[];
  readonly onAddRelated: (workItemId: string) => void;
  readonly onOpenPullRequest: (() => void) | null;
  /**
   * True where the list is already named by whatever is showing it — the
   * disclosure at the top of the issue column while the panel is open. The
   * first heading stays in the accessibility tree either way.
   */
  readonly hideTitle?: boolean;
}

const PICKER_KEYS: readonly (readonly [KeybindingCommand, IssuePickerName])[] = [
  ["issue.status", "state"],
  ["issue.priority", "priority"],
  ["issue.assignee", "assignee"],
  ["issue.labels", "labels"],
  ["issue.related", "related"],
];

/**
 * The letters the column claims, read out of the chord table rather than
 * written down again — a rebound picker moves its claim with it.
 */
const LETTER_SHORTCUTS = PICKER_KEYS.map(([command]) => DEFAULT_BINDINGS[command].key).join(" ");

/** One fact: its glyph, then its words, on a 28px line. */
const ROW_CLASS = "flex h-7 min-w-0 items-center gap-2 text-[13px] text-foreground";
/** A fact that opens something: Linear's pill-on-hover, without a border. */
const ROW_ACTION_CLASS = cn(
  ROW_CLASS,
  "-mx-2 w-[calc(100%+1rem)] cursor-pointer rounded-md px-2 text-left outline-none ring-ring hover:bg-accent focus-visible:ring-2 disabled:cursor-default disabled:hover:bg-transparent",
);
const ICON_CLASS = "size-3.5 shrink-0 text-muted-foreground";

function Group({
  title,
  hideTitle = false,
  children,
  ...rest
}: {
  readonly title: string;
  readonly hideTitle?: boolean;
  readonly children: React.ReactNode;
} & React.HTMLAttributes<HTMLElement>): React.ReactElement {
  return (
    <section className="flex flex-col" {...rest}>
      <h3 className={cn("mb-1.5 text-[13px] text-muted-foreground", hideTitle && "sr-only")}>{title}</h3>
      <div className="flex flex-col gap-0.5">{children}</div>
    </section>
  );
}

export function IssueProperties(props: IssuePropertiesProps): React.ReactElement {
  const { item } = props;
  const change = item.change;
  const pullRequest =
    change === null || change.number === null
      ? null
      : resolvePullRequestState({
          state: change.state === "merged" ? "merged" : change.state === "closed" ? "closed" : "open",
          isDraft: change.state === "draft",
        });

  // One picker at a time, opened by a row or by its letter.
  const [openPicker, setOpenPicker] = React.useState<IssuePickerName | null>(null);
  const [query, setQuery] = React.useState("");
  const open = React.useCallback(
    (picker: IssuePickerName) => (next: boolean) => {
      setQuery("");
      setOpenPicker(next ? picker : null);
    },
    [],
  );

  const canWrite = props.canWrite;
  const fromKeyboard = React.useMemo(() => {
    const actions: Record<string, () => boolean> = {};
    for (const [command, picker] of PICKER_KEYS) {
      actions[command] = () => {
        // A reader who cannot write has facts and no controls, so the letter
        // is declined and the browser keeps the keystroke.
        if (!canWrite) return false;
        setQuery("");
        setOpenPicker((current) => (current === picker ? null : picker));
        return true;
      };
    }
    return actions as KeybindingActions;
  }, [canWrite]);
  useKeybindingActions(fromKeyboard);

  const labels = userLabels(item.labels);
  const keep = managedLabels(item.labels);
  const writeLabels = (next: readonly string[]) => props.onLabels([...keep, ...next]);

  return (
    <div
      className="flex flex-col gap-7"
      data-testid="issue-properties"
      data-letter-shortcuts={props.canWrite ? LETTER_SHORTCUTS : undefined}
    >
      <Group title="Properties" hideTitle={props.hideTitle === true}>
        {props.canWrite ? (
          <StatePicker
            {...props}
            open={openPicker === "state"}
            onOpenChange={open("state")}
            query={query}
            onQuery={setQuery}
          />
        ) : (
          <div className={ROW_CLASS}>
            <StateGlyph category={props.laneCategory} />
            <span className="truncate" data-testid="issue-state-pill">
              {item.state}
            </span>
          </div>
        )}

        {props.canWrite ? (
          <PriorityPicker
            {...props}
            open={openPicker === "priority"}
            onOpenChange={open("priority")}
            query={query}
            onQuery={setQuery}
          />
        ) : (
          <div className={ROW_CLASS}>
            <PriorityGlyph priority={item.priority} />
            <span className="truncate" data-testid="issue-priority">
              {item.priority ?? "No priority"}
            </span>
          </div>
        )}

        {props.canWrite ? (
          <AssigneePicker
            {...props}
            open={openPicker === "assignee"}
            onOpenChange={open("assignee")}
            query={query}
            onQuery={setQuery}
          />
        ) : (
          <div className={ROW_CLASS} data-testid="issue-assignee">
            <AssigneeRowContent runner={props.runner} assignees={item.assignees} />
          </div>
        )}

        <div className={ROW_CLASS} data-testid="issue-effort">
          <GaugeIcon className={ICON_CLASS} />
          <span className="truncate">{props.effort}</span>
        </div>

        <div className={ROW_CLASS} data-testid="issue-attempts">
          <HistoryIcon className={ICON_CLASS} />
          <span className="truncate">{props.attempts}</span>
        </div>
      </Group>

      <Group title="Labels">
        <div className="flex flex-wrap items-center gap-1.5 py-0.5" data-testid="issue-labels">
          {labels.map((label) => {
            const colour = props.labelCatalogue.find((entry) => entry.name === label)?.color;
            return props.canWrite ? (
              <button
                key={label}
                type="button"
                data-testid={`label-${label}`}
                disabled={props.saving}
                title={`Remove ${label}`}
                onClick={() => writeLabels(labels.filter((candidate) => candidate !== label))}
                className="group/label inline-flex h-6 cursor-pointer items-center gap-1.5 rounded-full border border-border px-2.5 text-xs outline-none ring-ring hover:bg-accent focus-visible:ring-2 disabled:cursor-default"
              >
                <LabelDot color={colour ?? "var(--color-primary)"} />
                {label}
                <XIcon className="-mr-0.5 size-3 text-muted-foreground opacity-0 transition-opacity group-hover/label:opacity-100 group-focus-visible/label:opacity-100" />
              </button>
            ) : (
              <span
                key={label}
                data-testid={`label-${label}`}
                className="inline-flex h-6 items-center gap-1.5 rounded-full border border-border px-2.5 text-xs"
              >
                <LabelDot color={colour ?? "var(--color-primary)"} />
                {label}
              </span>
            );
          })}
          {props.canWrite ? (
            <LabelPicker
              {...props}
              onWriteLabels={writeLabels}
              open={openPicker === "labels"}
              onOpenChange={open("labels")}
              query={query}
              onQuery={setQuery}
            />
          ) : labels.length === 0 ? (
            <span className="text-[13px] text-muted-foreground">None</span>
          ) : null}
        </div>
      </Group>

      <Group title="Project">
        <div className={ROW_CLASS}>
          <BoxIcon className={ICON_CLASS} />
          <span className="truncate">{item.projectName}</span>
        </div>
      </Group>

      <Group title="Pull request">
        {pullRequest === null || change === null ? (
          <div className={ROW_CLASS} data-testid="issue-pull-request">
            <GitPullRequestIcon className={ICON_CLASS} />
            <span className="text-muted-foreground">
              {change === null ? "None yet" : "Not mirrored to a host"}
            </span>
          </div>
        ) : (
          <button
            type="button"
            data-testid="issue-pull-request"
            disabled={props.onOpenPullRequest === null}
            onClick={() => props.onOpenPullRequest?.()}
            className={cn(ROW_ACTION_CLASS, "text-left")}
          >
            <pullRequest.Icon className={cn("size-3.5 shrink-0", pullRequest.toneClassName)} />
            <span className="truncate">
              #{change.number} · {pullRequest.label}
            </span>
          </button>
        )}
      </Group>

      <Group title="Related" data-testid="issue-related">
        {props.related.length === 0 ? (
          <div className={ROW_CLASS}>
            <LinkIcon className={ICON_CLASS} />
            <span className="text-muted-foreground">Nothing related</span>
          </div>
        ) : (
          props.related.map((entry) => (
            <div key={entry.key} className="group/related flex min-w-0 items-center">
              <button
                type="button"
                data-testid="issue-related-row"
                onClick={entry.onOpen}
                className={cn(ROW_ACTION_CLASS, "flex-1")}
              >
                {entry.kind === "chat" ? (
                  <MessageSquareIcon className={ICON_CLASS} />
                ) : (
                  <StateGlyph category={entry.category ?? "unstarted"} />
                )}
                <span className="min-w-0 truncate">{entry.label}</span>
              </button>
              {props.canWrite && entry.onRemove != null ? (
                <button
                  type="button"
                  data-testid="remove-related"
                  disabled={props.saving}
                  aria-label={`Remove the relation to ${entry.label}`}
                  onClick={entry.onRemove}
                  className="ml-1 inline-flex size-5 shrink-0 cursor-pointer items-center justify-center rounded-sm text-muted-foreground opacity-0 outline-none ring-ring transition-opacity hover:bg-accent focus-visible:opacity-100 focus-visible:ring-2 group-hover/related:opacity-100 disabled:cursor-default"
                >
                  <XIcon className="size-3" />
                </button>
              ) : null}
            </div>
          ))
        )}
        {props.canWrite ? (
          <RelatedPicker
            {...props}
            open={openPicker === "related"}
            onOpenChange={open("related")}
            query={query}
            onQuery={setQuery}
          />
        ) : null}
      </Group>
    </div>
  );
}

function AssigneeRowContent({
  runner,
  assignees,
}: {
  readonly runner: string | null;
  readonly assignees: readonly string[];
}): React.ReactElement {
  if (runner !== null) {
    return (
      <>
        <AssigneeAvatar name={runner} />
        <span className="truncate">
          {runner} <span className="text-success">· working</span>
        </span>
      </>
    );
  }
  if (assignees.length > 0) {
    return (
      <>
        <AssigneeAvatar name={assignees[0]!} />
        <span className="truncate">{assignees.join(", ")}</span>
      </>
    );
  }
  return (
    <>
      <NoAssigneeGlyph />
      <span className="text-muted-foreground">Assign</span>
    </>
  );
}

type PickerProps = IssuePropertiesProps & {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly query: string;
  readonly onQuery: (query: string) => void;
};

/** Turns one built option into a row, with its glyph and its handler. */
function toRow<A>(
  option: PickerOption<A>,
  prefix: string,
  glyph: React.ReactNode,
  run: (action: A) => void,
): PickerRow {
  return {
    key: option.key,
    label: option.label,
    glyph,
    ...(option.detail === undefined ? {} : { detail: option.detail }),
    ...(option.digit === undefined ? {} : { digit: option.digit }),
    ...(option.selected === undefined ? {} : { selected: option.selected }),
    ...(option.disabled === undefined ? {} : { disabled: option.disabled }),
    testId: `${prefix}-${option.key}`,
    onSelect: () => run(option.action),
  };
}

function categoryOf(states: readonly StateOption[], name: string): string {
  return states.find((state) => state.name === name)?.category ?? "unstarted";
}

function StatePicker(props: PickerProps): React.ReactElement {
  const groups = statusSections({
    states: props.states,
    current: props.item.state,
    moves: props.moves,
    query: props.query,
  }).map((section) => ({
    label: section.label,
    rows: section.options.map((option) =>
      toRow(
        option,
        "state-menu",
        <StateGlyph category={categoryOf(props.states, option.key)} />,
        (action) => {
          props.onOpenChange(false);
          if (action.moves) props.onMove(action.state);
        },
      ),
    ),
  }));
  return (
    <PropertyPicker
      open={props.open}
      onOpenChange={props.onOpenChange}
      triggerTestId="state-menu"
      triggerClassName={ROW_ACTION_CLASS}
      triggerDisabled={props.saving || props.states.length === 0}
      trigger={
        <>
          <StateGlyph category={props.laneCategory} />
          <span className="truncate" data-testid="issue-state-pill">
            {props.item.state}
          </span>
        </>
      }
      placeholder="Change status…"
      hint="S"
      label="Change status"
      testId="state-picker"
      query={props.query}
      onQuery={props.onQuery}
      groups={groups}
      empty="No status matches."
    />
  );
}

function PriorityPicker(props: PickerProps): React.ReactElement {
  const groups = prioritySections({ current: props.item.priority, query: props.query }).map(
    (section) => ({
      label: section.label,
      rows: section.options.map((option) =>
        toRow(option, "priority-menu", <PriorityGlyph priority={option.action.name} />, (action) => {
          props.onOpenChange(false);
          props.onPriority(action.name);
        }),
      ),
    }),
  );
  return (
    <PropertyPicker
      open={props.open}
      onOpenChange={props.onOpenChange}
      triggerTestId="priority-menu"
      triggerClassName={ROW_ACTION_CLASS}
      triggerDisabled={props.saving}
      trigger={
        <>
          <PriorityGlyph priority={props.item.priority} />
          <span className="truncate" data-testid="issue-priority">
            {props.item.priority ?? "No priority"}
          </span>
        </>
      }
      placeholder="Change priority to…"
      hint="P"
      label="Change priority"
      testId="priority-picker"
      query={props.query}
      onQuery={props.onQuery}
      groups={groups}
      empty="No priority matches."
      width="w-[240px]"
    />
  );
}

function AssigneePicker(props: PickerProps): React.ReactElement {
  const groups = assigneeSections({
    assignees: props.item.assignees,
    members: props.members,
    viewer: props.viewer,
    canInvite: props.onInvite !== null,
    inviteReason: props.inviteReason,
    query: props.query,
  }).map((section) => ({
    label: section.label,
    rows: section.options.map((option) =>
      toRow(
        option,
        "assignee-menu",
        option.action.kind === "invite" ? (
          <UserPlusIcon className={ICON_CLASS} />
        ) : option.key === "none" ? (
          <NoAssigneeGlyph />
        ) : (
          <AssigneeAvatar name={option.label} />
        ),
        (action) => {
          props.onOpenChange(false);
          if (action.kind === "invite") props.onInvite?.();
          else props.onAssignees(action.assignees);
        },
      ),
    ),
  }));
  return (
    <PropertyPicker
      open={props.open}
      onOpenChange={props.onOpenChange}
      triggerTestId="issue-assignee"
      triggerClassName={ROW_ACTION_CLASS}
      triggerDisabled={props.saving}
      trigger={<AssigneeRowContent runner={props.runner} assignees={props.item.assignees} />}
      placeholder="Assign to…"
      hint="A"
      label="Assign"
      testId="assignee-picker"
      query={props.query}
      onQuery={props.onQuery}
      groups={groups}
      width="w-[288px]"
    />
  );
}

function LabelPicker(
  props: PickerProps & { readonly onWriteLabels: (labels: readonly string[]) => void },
): React.ReactElement {
  const groups = labelSections({
    attached: props.item.labels,
    catalogue: props.labelCatalogue,
    query: props.query,
  }).map((section) => ({
    label: section.label,
    rows: section.options.map((option) => ({
      ...toRow(
        option,
        "label-menu",
        option.action.kind === "create" ? (
          <PlusIcon className={ICON_CLASS} />
        ) : (
          <LabelDot color={option.action.color} />
        ),
        (action) => {
          if (action.kind === "create") props.onQuery("");
          props.onWriteLabels(action.labels);
        },
      ),
      checkbox: option.action.kind === "toggle",
      testId: option.action.kind === "create" ? "create-label" : `label-menu-${option.key}`,
    })),
  }));
  return (
    <PropertyPicker
      open={props.open}
      onOpenChange={props.onOpenChange}
      triggerTestId="add-label"
      triggerClassName="inline-flex size-6 cursor-pointer items-center justify-center rounded-full text-muted-foreground outline-none ring-ring hover:bg-accent focus-visible:ring-2 disabled:cursor-default"
      triggerDisabled={props.saving}
      trigger={
        <>
          <PlusIcon className="size-3.5" />
          <span className="sr-only">Add label</span>
        </>
      }
      placeholder="Change or add labels…"
      hint="L"
      label="Change or add labels"
      testId="label-picker"
      query={props.query}
      onQuery={props.onQuery}
      groups={groups}
      empty={
        props.labelCatalogue.length === 0
          ? "This project has no labels yet. Type a name to make one."
          : "No label matches. Type a name to make one."
      }
      width="w-[264px]"
    />
  );
}

function RelatedPicker(props: PickerProps): React.ReactElement {
  const groups = relatedSections({
    candidates: props.relatedCandidates,
    relatedIds: props.related.map((entry) => entry.key.split(":").at(-1) ?? ""),
    self: props.item.id,
    query: props.query,
  }).map((section) => ({
    label: section.label,
    rows: section.options.map((option) =>
      toRow(option, "related-menu", <StateGlyph category={option.action.category} />, (action) => {
        props.onOpenChange(false);
        props.onAddRelated(action.id);
      }),
    ),
  }));
  return (
    <PropertyPicker
      open={props.open}
      onOpenChange={props.onOpenChange}
      triggerTestId="add-related"
      triggerClassName={cn(ROW_ACTION_CLASS, "text-muted-foreground")}
      triggerDisabled={props.saving}
      trigger={
        <>
          <PlusIcon className={ICON_CLASS} />
          <span className="truncate">Add related…</span>
        </>
      }
      placeholder="Search issues…"
      hint="R"
      label="Add a related issue"
      testId="related-picker"
      query={props.query}
      onQuery={props.onQuery}
      groups={groups}
      empty="No other issue in this project matches."
      width="w-[320px]"
    />
  );
}

