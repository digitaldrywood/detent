import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import { Button, InlineButton } from "../../../components/ui/button.tsx";
import { Input } from "../../../components/ui/input.tsx";
import { Label } from "../../../components/ui/label.tsx";
import { Popover, PopoverPopup, PopoverTitle, PopoverTrigger } from "../../../components/ui/popover.tsx";
import type { NativeTestItem, NativeTestPlan, TestItemResult } from "../../../contracts/work.ts";
import { formatChatTimestampTooltip, formatRelativeTimeLabel } from "../../../timestampFormat.ts";

const RESULTS: readonly { readonly result: TestItemResult; readonly label: string; readonly variant: "success" | "error" | "warning" }[] = [
  { result: "pass", label: "Passed", variant: "success" },
  { result: "fail", label: "Failed", variant: "error" },
  { result: "blocked", label: "Blocked", variant: "warning" },
];

export interface TestItemRecord {
  readonly item: number;
  readonly text: string;
  readonly result: TestItemResult;
  readonly note: string;
  readonly relatedIssueNumber: number | undefined;
}

function ResultBadge({ result }: { readonly result: TestItemResult | undefined }): React.ReactElement {
  const known = RESULTS.find((entry) => entry.result === result);
  return (
    <Badge size="sm" variant={known?.variant ?? "outline"} data-testid="test-item-result">
      {known?.label ?? "Untested"}
    </Badge>
  );
}

function RecordForm({
  item,
  onRecord,
  onDone,
}: {
  readonly item: NativeTestItem;
  readonly onRecord: (record: TestItemRecord) => Promise<void>;
  readonly onDone: () => void;
}): React.ReactElement {
  const [note, setNote] = React.useState("");
  const [related, setRelated] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const relatedNumber = related.trim() === "" ? undefined : Number(related.trim());
  const relatedValid = relatedNumber === undefined || (Number.isInteger(relatedNumber) && relatedNumber > 0);
  const record = (result: TestItemResult) => {
    if (!relatedValid || saving) return;
    setSaving(true);
    setError(null);
    void onRecord({ item: item.index, text: item.text, result, note, relatedIssueNumber: result === "fail" ? relatedNumber : undefined }).then(
      onDone,
      (cause: unknown) => {
        setError(cause instanceof Error ? cause.message : String(cause));
        setSaving(false);
      },
    );
  };
  const noteId = `test-item-note-${item.index}`;
  const relatedId = `test-item-related-${item.index}`;
  return (
    <form className="flex flex-col gap-3" onSubmit={(event) => event.preventDefault()}>
      <PopoverTitle className="text-sm">Record a result for item {item.index}</PopoverTitle>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={noteId}>Note (optional)</Label>
        <Input id={noteId} size="compact" value={note} maxLength={2000} onChange={(event) => setNote(event.target.value)} />
      </div>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={relatedId}>Issue number filed for a failure (optional)</Label>
        <Input id={relatedId} size="compact" inputMode="numeric" placeholder="For example 42" value={related}
          aria-invalid={!relatedValid} onChange={(event) => setRelated(event.target.value)} />
      </div>
      {error === null ? null : <p role="alert" className="text-destructive text-sm">{error}</p>}
      <div className="flex flex-wrap gap-2">
        {RESULTS.map((entry) => (
          <Button key={entry.result} type="button" size="xs" variant={entry.result === "pass" ? "default" : "outline"}
            disabled={saving || !relatedValid} data-testid={`test-item-${entry.result}`} onClick={() => record(entry.result)}>
            {entry.result === "pass" ? "Pass" : entry.result === "fail" ? "Fail" : "Blocked"}
          </Button>
        ))}
      </div>
    </form>
  );
}

function TestItemRow({
  item,
  canRecord,
  onRecord,
  onOpenIssue,
}: {
  readonly item: NativeTestItem;
  readonly canRecord: boolean;
  readonly onRecord: (record: TestItemRecord) => Promise<void>;
  readonly onOpenIssue: (workItemId: string) => void;
}): React.ReactElement {
  const [open, setOpen] = React.useState(false);
  const summary = (
    <>
      <ResultBadge result={item.result} />
      <span className="min-w-0 flex-1 text-sm">{item.text}</span>
    </>
  );
  const tester = item.tester?.display_name || item.tester?.principal_id;
  return (
    <li className="flex flex-col gap-1 py-2" data-testid="test-item">
      {canRecord ? (
        <Popover open={open} onOpenChange={setOpen}>
          <PopoverTrigger render={<InlineButton className="w-full items-start gap-2 text-left" aria-label={`Record a result for item ${item.index}: ${item.text}`} />}>
            {summary}
          </PopoverTrigger>
          <PopoverPopup side="bottom" align="start" className="w-80 max-w-[calc(100vw-2rem)]">
            <RecordForm item={item} onRecord={onRecord} onDone={() => setOpen(false)} />
          </PopoverPopup>
        </Popover>
      ) : (
        <div className="flex items-start gap-2">{summary}</div>
      )}
      {tester === undefined || item.recorded_at === undefined ? null : (
        <p className="text-muted-foreground text-xs">
          {tester} ·{" "}
          <time dateTime={item.recorded_at} title={formatChatTimestampTooltip(item.recorded_at, "locale")}>
            {formatRelativeTimeLabel(item.recorded_at)}
          </time>
          {item.note === undefined ? null : <> · {item.note}</>}
        </p>
      )}
      {item.linked_issues.length === 0 ? null : (
        <p className="flex flex-wrap gap-2 text-xs">
          {item.linked_issues.map((link) => (
            <InlineButton key={link.work_item_id} tone="muted" onClick={() => onOpenIssue(link.work_item_id)}>
              #{link.number}
            </InlineButton>
          ))}
        </p>
      )}
    </li>
  );
}

/** Per-item results and overall coverage of an issue labeled `test-plan`. */
export function TestPlanProgress({
  plan,
  canRecord,
  onRecord,
  onOpenIssue,
}: {
  readonly plan: NativeTestPlan;
  readonly canRecord: boolean;
  readonly onRecord: (record: TestItemRecord) => Promise<void>;
  readonly onOpenIssue: (workItemId: string) => void;
}): React.ReactElement {
  return (
    <section className="rounded-[var(--radius)] border border-border bg-card px-4 py-3.5" aria-label="Test plan" data-testid="test-plan">
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="font-medium text-sm">Test plan</h2>
        <span className="text-muted-foreground text-xs tabular-nums" data-testid="test-plan-counts">
          {plan.passed} passed · {plan.failed} failed · {plan.blocked} blocked · {plan.untested} untested
        </span>
      </div>
      {plan.items.length === 0 ? (
        <p className="mt-2 text-muted-foreground text-sm">The plan has no checklist items. Add one per screen or workflow, such as - [ ] Quote builder: create, edit, send.</p>
      ) : (
        <ol className="mt-1 divide-y divide-border">
          {plan.items.map((item) => (
            <TestItemRow key={`${item.index}:${item.text}`} item={item} canRecord={canRecord} onRecord={onRecord} onOpenIssue={onOpenIssue} />
          ))}
        </ol>
      )}
    </section>
  );
}
