import * as Schema from "effect/Schema";

const NullableNumber = Schema.NullOr(Schema.Number);
export const DiagnosticFinding = Schema.Struct({
  id: Schema.String,
  severity: Schema.Literals(["attention", "watch"]),
  class: Schema.Literals(["instance", "flow", "capacity", "cost", "human"]),
  when: Schema.String,
  summary: Schema.String,
  next_action: Schema.String,
  subject: Schema.Struct({
    kind: Schema.Literals(["issue", "fleet", "project"]),
    project_id: Schema.optional(Schema.String),
    work_item_id: Schema.optional(Schema.String),
  }),
});
export type DiagnosticFinding = typeof DiagnosticFinding.Type;

const HealthFinding = Schema.Struct({
  id: Schema.String,
  severity: DiagnosticFinding.fields.severity,
  class: DiagnosticFinding.fields.class,
  opened_at: Schema.String,
  summary: Schema.String,
  next_action: Schema.String,
  subject: Schema.Struct({
    kind: Schema.Literals(["work_item", "runner", "project", "organization"]),
    id: Schema.String,
    project_id: Schema.optional(Schema.String),
  }),
});

export const HealthFindingsRead = Schema.Struct({
  items: Schema.Array(HealthFinding),
  detector_tick: Schema.NullOr(Schema.String),
});
export type HealthFindingsRead = typeof HealthFindingsRead.Type;
export const decodeHealthFindings =
  Schema.decodeUnknownSync(HealthFindingsRead);

export function findingsFromRead(
  read: HealthFindingsRead,
): readonly DiagnosticFinding[] {
  return read.items.map((finding) => ({
    id: finding.id,
    severity: finding.severity,
    class: finding.class,
    when: finding.opened_at,
    summary: finding.summary,
    next_action: finding.next_action,
    subject:
      finding.subject.kind === "work_item"
        ? {
            kind: "issue",
            work_item_id: finding.subject.id,
            project_id: finding.subject.project_id,
          }
        : finding.subject.kind === "project"
          ? { kind: "project", project_id: finding.subject.id }
          : { kind: "fleet" },
  }));
}

export const DiagnosticsReport = Schema.Struct({
  from: Schema.String,
  to: Schema.String,
  partial: Schema.Boolean,
  findings: Schema.NullOr(Schema.Array(DiagnosticFinding)),
  detector_tick: Schema.NullOr(Schema.String),
  busy_percent: Schema.optional(NullableNumber),
  stalled: Schema.optional(NullableNumber),
  capacity: Schema.NullOr(
    Schema.Array(
      Schema.Struct({
        hour: Schema.String,
        slots: Schema.Number,
        todo: NullableNumber,
      }),
    ),
  ),
  claimed: NullableNumber,
  ready: NullableNumber,
  skipped: NullableNumber,
  skip_reasons: Schema.Array(
    Schema.Struct({
      source: Schema.String,
      reason: Schema.String,
      count: Schema.Number,
    }),
  ),
  merge_entered: NullableNumber,
  merge_landed: NullableNumber,
  merge_wait_p50: NullableNumber,
  merge_wait_p90: NullableNumber,
  merge_wait_max: NullableNumber,
  refused: Schema.NullOr(
    Schema.Array(
      Schema.Struct({ reason: Schema.String, count: Schema.Number }),
    ),
  ),
  coverage: Schema.Array(
    Schema.Struct({
      source: Schema.String,
      observed: NullableNumber,
      total: NullableNumber,
    }),
  ),
  configuration: Schema.optional(
    Schema.Array(
      Schema.Struct({
        name: Schema.String,
        value: Schema.NullOr(Schema.String),
        effect: Schema.NullOr(Schema.String),
      }),
    ),
  ),
});
export type DiagnosticsReport = typeof DiagnosticsReport.Type;
export const decodeDiagnostics = Schema.decodeUnknownSync(DiagnosticsReport);

export function findingDestination(finding: DiagnosticFinding): string | null {
  const subject = finding.subject;
  if (subject.kind === "fleet") return "/fleet";
  if (subject.kind === "issue" && subject.work_item_id)
    return `/work/i/${encodeURIComponent(subject.work_item_id)}?tab=diagnostics`;
  if (subject.kind === "project" && subject.project_id)
    return `/work/p/${encodeURIComponent(subject.project_id)}`;
  return null;
}

export function orderedFindings(
  findings: readonly DiagnosticFinding[],
): readonly DiagnosticFinding[] {
  return findings.toSorted(
    (left, right) =>
      Number(right.severity === "attention") -
        Number(left.severity === "attention") ||
      Date.parse(right.when) - Date.parse(left.when) ||
      left.id.localeCompare(right.id),
  );
}
