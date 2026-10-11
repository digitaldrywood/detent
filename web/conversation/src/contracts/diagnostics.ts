import * as Schema from "effect/Schema";

const IdentityValue = Schema.Struct({ value: Schema.optional(Schema.String) });

export const AttemptDiagnostics = {
  runtime_freshness: Schema.optional(Schema.String),
  terminal_failure: Schema.optional(Schema.Struct({ summary: Schema.String })),
  disposition: Schema.optional(Schema.Struct({
    status: Schema.String,
    human_action: Schema.Boolean,
    final_summary: Schema.optional(Schema.String),
  })),
  usage: Schema.optional(Schema.Array(Schema.Struct({
    input: Schema.Number,
    output: Schema.Number,
    cost_estimate: Schema.Number,
    currency: Schema.String,
    reported_cost_micros: Schema.optional(Schema.Number),
    cost_coverage: Schema.optional(Schema.String),
  }))),
  runtime: Schema.optional(Schema.Struct({
    phase: Schema.String,
    phases: Schema.optional(Schema.NullOr(Schema.Array(Schema.Struct({
      name: Schema.String,
      started_at: Schema.String,
      finished_at: Schema.optional(Schema.String),
    })))),
    phases_dropped: Schema.optional(Schema.Number),
    identity: Schema.optional(Schema.Struct({
      role: Schema.optional(Schema.String),
      resolved_model: Schema.optional(IdentityValue),
      requested_model: Schema.optional(IdentityValue),
      reasoning_effort: Schema.optional(IdentityValue),
    })),
    activity: Schema.optional(Schema.Struct({
      stage: Schema.String,
      coverage: Schema.String,
      instructions: Schema.NullOr(Schema.Array(Schema.Struct({ name: Schema.String, sha256: Schema.String }))),
      timing_summary: Schema.optional(Schema.Struct({
        breakdown: Schema.Struct({
          elapsed_seconds: Schema.Number,
          observed_seconds: Schema.Number,
          unknown_seconds: Schema.Number,
          concurrent_seconds: Schema.Number,
          by_kind_seconds: Schema.NullOr(Schema.Record(Schema.String, Schema.Number)),
        }),
      })),
    })),
  })),
};

const Decision = Schema.Struct({
  state: Schema.String,
  reason: Schema.optional(Schema.String),
  at: Schema.String,
});

export const IssueExplanation = Schema.Struct({
  observed_at: Schema.String,
  current_lane: Schema.Struct({ name: Schema.optional(Schema.String), freshness: Schema.String }),
  latest_transition: Schema.optional(Schema.Struct({ at: Schema.String, reason: Schema.optional(Schema.String) })),
  eligibility: Schema.Struct({
    state: Schema.String,
    source_state: Schema.String,
    current: Schema.optional(Decision),
    latest: Schema.optional(Decision),
  }),
  required_gate: Schema.Struct({
    state: Schema.String,
    source_state: Schema.String,
    reason: Schema.optional(Schema.String),
    human_action: Schema.optional(Schema.String),
  }),
  reasons: Schema.NullOr(Schema.Array(Schema.Struct({ code: Schema.String, detail: Schema.String, action: Schema.optional(Schema.String) }))),
  sources: Schema.NullOr(Schema.Array(Schema.Struct({ name: Schema.String, state: Schema.String, code: Schema.optional(Schema.String) }))),
  native_runtime: Schema.optional(Schema.Struct({
    capacity: Schema.NullOr(Schema.Array(Schema.Struct({
      runner_id: Schema.String,
      observed_at: Schema.String,
      available: Schema.Number,
      health: Schema.String,
      exclusions: Schema.NullOr(Schema.Array(Schema.String)),
    }))),
    unavailable: Schema.NullOr(Schema.Array(Schema.String)),
  })),
});
export type IssueExplanation = typeof IssueExplanation.Type;

const NullableNumber = Schema.NullOr(Schema.Number);
export const DiagnosticFinding = Schema.Struct({
  id: Schema.String,
  severity: Schema.Literals(["attention", "watch"]),
  class: Schema.Literals(["instance", "flow", "capacity", "cost", "human"]),
  when: Schema.String,
  resolved_at: Schema.optional(Schema.NullOr(Schema.String)),
  summary: Schema.String,
  next_action: Schema.String,
  subject: Schema.Struct({
    kind: Schema.Literals(["issue", "fleet", "project"]),
    project_id: Schema.optional(Schema.String),
    work_item_id: Schema.optional(Schema.String),
  }),
});
export type DiagnosticFinding = typeof DiagnosticFinding.Type;

export const HealthFinding = Schema.Struct({
  id: Schema.String,
  severity: DiagnosticFinding.fields.severity,
  class: DiagnosticFinding.fields.class,
  opened_at: Schema.String,
  resolved_at: Schema.optional(Schema.NullOr(Schema.String)),
  summary: Schema.String,
  next_action: Schema.String,
  subject: Schema.Struct({
    kind: Schema.Literals(["work_item", "runner", "project", "organization"]),
    id: Schema.String,
    project_id: Schema.optional(Schema.String),
  }),
});

export type HealthFinding = typeof HealthFinding.Type;

export const HealthFindingsRead = Schema.Struct({
  items: Schema.Array(HealthFinding),
  last_tick_at: Schema.NullOr(Schema.String),
  next_cursor: Schema.optional(Schema.String),
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
    resolved_at: finding.resolved_at,
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
