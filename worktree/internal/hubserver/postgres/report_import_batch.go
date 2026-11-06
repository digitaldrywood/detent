package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

type reportBatch struct {
	columns string
	types   []string
	rows    []string
	args    []any
}

func newReportBatch(organization any, columns string, types ...string) *reportBatch {
	return &reportBatch{columns: columns, types: types, args: []any{organization}}
}

func (b *reportBatch) add(values ...any) {
	fields := make([]string, len(values))
	for i, value := range values {
		b.args = append(b.args, value)
		fields[i] = fmt.Sprintf("$%d::%s", len(b.args), b.types[i])
	}
	b.rows = append(b.rows, "("+strings.Join(fields, ",")+")")
}

func (b *reportBatch) source() string {
	return "WITH source(" + b.columns + ") AS (VALUES " + strings.Join(b.rows, ",") + ") "
}

func (s *session) verifyReportBatch(ctx context.Context, b *reportBatch, query string, mismatch error) error {
	var count int
	if err := s.db.QueryRowContext(ctx, b.source()+query, b.args...).Scan(&count); err != nil {
		return mapError(ctx, err)
	}
	if count != len(b.rows) {
		return mismatch
	}
	return nil
}

func (s *session) mergeReportFacts(ctx context.Context, b *reportBatch, table, columns, keys string) error {
	if _, err := s.db.ExecContext(ctx, b.source()+"INSERT INTO "+table+"(organization_id,"+columns+") SELECT DISTINCT $1,"+columns+" FROM source ORDER BY "+keys+" ON CONFLICT(organization_id,"+keys+") DO NOTHING", b.args...); err != nil {
		return mapError(ctx, err)
	}
	fields := strings.Split(columns, ",")
	comparisons := make([]string, len(fields))
	for i, field := range fields {
		comparisons[i] = "target." + field + " IS NOT DISTINCT FROM source." + field
	}
	return s.verifyReportBatch(ctx, b, "SELECT count(*) FROM source JOIN "+table+" target ON target.organization_id=$1 AND "+strings.Join(comparisons, " AND "), corestore.ErrRevisionConflict)
}

func (s *session) importReportUsage(ctx context.Context, records []reportstore.UsageRecord) error {
	b := newReportBatch(s.organization, "project_id,work_item_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at", "text", "text", "text", "timestamptz", "text", "text", "bigint", "bigint", "bigint", "double precision", "text", "timestamptz")
	for _, r := range records {
		b.add(r.ProjectID, r.WorkItemID, r.AttemptID, recordTime(r.Period), r.Provider, r.Model, r.Input, r.CachedInput, r.Output, r.Cost, "USD", recordTime(r.UpdatedAt))
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN native_attempts a ON a.organization_id=$1 AND a.id=source.attempt_id JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=source.project_id AND i.native_id=source.work_item_id`, corestore.ErrTenantMismatch); err != nil {
		return err
	}
	return s.mergeReportFacts(ctx, b, "attempt_usage", "project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at", "attempt_id,period,provider,model")
}

func (s *session) verifyReportUsageBatch(ctx context.Context, sha string, records []ReportUsageImportCoverage) error {
	b := newReportBatch(s.organization, "attempt_id,source_sha256,row_count", "text", "text", "bigint")
	for _, r := range records {
		b.add(r.AttemptID, sha, r.RowCount)
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source WHERE row_count=(SELECT count(*) FROM (SELECT 1 FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=source.attempt_id LIMIT source.row_count+1) bounded)`, corestore.ErrRevisionConflict); err != nil {
		return err
	}
	return s.mergeReportFacts(ctx, b, "report_usage_import_coverage", "attempt_id,source_sha256,row_count", "attempt_id")
}

func (s *session) importReportEscapes(ctx context.Context, records []eventstore.Evidence) error {
	b := newReportBatch(s.organization, "change_id,evidence_sequence,payload_object_key,payload_sha256,occurrence_id,kind,cause,observed_at,instance_id,human_override", "text", "bigint", "text", "text", "text", "text", "text", "timestamptz", "text", "boolean")
	for _, r := range records {
		p := r.ReportEscape
		if p.OccurrenceID == "" || p.ObservedAt.IsZero() {
			return eventstore.ErrInvalidRecord
		}
		b.add(r.ChangeID, r.Sequence, r.PayloadObjectKey, r.PayloadSHA256, p.OccurrenceID, p.Kind, p.Cause, recordTime(p.ObservedAt), p.InstanceID, p.HumanOverride)
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN change_evidence e ON e.organization_id=$1 AND e.change_id=source.change_id AND e.sequence=source.evidence_sequence AND e.payload_object_key=source.payload_object_key AND e.payload_sha256=source.payload_sha256`, corestore.ErrRevisionConflict); err != nil {
		return err
	}
	return s.mergeReportFacts(ctx, b, "report_escape_facts", "change_id,evidence_sequence,occurrence_id,kind,cause,observed_at,instance_id,human_override", "change_id,evidence_sequence")
}

func (s *session) importReportEvents(ctx context.Context, records []ReportEventProjectionImport) (int, error) {
	b := newReportBatch(s.organization, "event_id,project_id,work_item_id,payload_object_key,payload_sha256,legacy_key,legacy_sha,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds", "text", "text", "text", "text", "text", "text", "text", "bigint", "text", "text", "text", "text", "boolean", "text", "text", "boolean", "boolean", "text", "text", "text", "double precision")
	for _, r := range records {
		e, p := r.Event, r.Event.Report
		legacyKey, legacySHA := "", ""
		if r.Legacy != nil {
			legacyKey, legacySHA = r.Legacy.PayloadObjectKey, r.Legacy.PayloadSHA256
		}
		b.add(e.ID, e.ProjectID, e.WorkItemID, e.PayloadObjectKey, e.PayloadSHA256, legacyKey, legacySHA, p.RunSequence, p.RunStatus, p.Role, p.Model, p.Effort, p.Failed, p.FailureClass, p.FailureKind, p.LandingReceipt, p.Landed, p.LandingChangeID, p.ReworkClass, p.ReworkKind, p.DurationSeconds)
	}
	var count, legacy int
	err := s.db.QueryRowContext(ctx, b.source()+`SELECT count(*),count(*) FILTER(WHERE (e.payload_object_key,e.payload_sha256) IS DISTINCT FROM (source.payload_object_key,source.payload_sha256)) FROM source JOIN collaboration_events e ON e.organization_id=$1 AND e.id=source.event_id AND e.project_id=source.project_id AND e.work_item_id=source.work_item_id WHERE (e.payload_object_key,e.payload_sha256)=(source.payload_object_key,source.payload_sha256) OR (source.legacy_sha<>'' AND (e.payload_object_key,e.payload_sha256)=(source.legacy_key,source.legacy_sha))`, b.args...).Scan(&count, &legacy)
	if err != nil {
		return 0, mapError(ctx, err)
	}
	if count != len(records) {
		return 0, corestore.ErrRevisionConflict
	}
	if err := s.mergeReportFacts(ctx, b, "report_event_facts", "event_id,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds", "event_id"); err != nil {
		return 0, err
	}
	_, err = s.db.ExecContext(ctx, b.source()+`, candidates AS (
 SELECT e.* FROM source JOIN collaboration_events e ON e.organization_id=$1 AND e.id=source.event_id JOIN native_attempts a ON a.organization_id=e.organization_id AND a.id=e.attempt_id
 ), incoming AS (
 SELECT organization_id,attempt_id,(array_agg(id ORDER BY sequence,id))[1] first_event_id,(array_agg(id ORDER BY sequence DESC,id))[1] latest_event_id,(array_agg(id ORDER BY recorded_at,sequence,id) FILTER(WHERE type='run.finished'))[1] outcome_event_id FROM candidates GROUP BY organization_id,attempt_id
 ) INSERT INTO report_attempt_facts(organization_id,attempt_id,first_event_id,latest_event_id,outcome_event_id)
 SELECT * FROM incoming ORDER BY attempt_id ON CONFLICT(organization_id,attempt_id) DO UPDATE SET
 first_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.first_event_id)<(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.first_event_id) THEN EXCLUDED.first_event_id ELSE report_attempt_facts.first_event_id END,
 latest_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.latest_event_id)>(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.latest_event_id) THEN EXCLUDED.latest_event_id ELSE report_attempt_facts.latest_event_id END,
 outcome_event_id=CASE WHEN report_attempt_facts.outcome_event_id IS NULL THEN EXCLUDED.outcome_event_id WHEN EXCLUDED.outcome_event_id IS NOT NULL AND (SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.outcome_event_id)<(SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.outcome_event_id) THEN EXCLUDED.outcome_event_id ELSE report_attempt_facts.outcome_event_id END`, b.args...)
	return legacy, mapError(ctx, err)
}

func (s *session) importReportLandings(ctx context.Context, records []ReportLandingImport) error {
	b := newReportBatch(s.organization, "id,revision,project_id,work_item_id,landed_version_id,landed_at,landed_head_sha,landed_merge_sha,landed_base_ref,attempt_id,payload_object_key,payload_sha256", "text", "bigint", "text", "text", "text", "timestamptz", "text", "text", "text", "text", "text", "text")
	seen := map[string]bool{}
	for _, r := range records {
		c, p := r.Change, r.Receipt
		if seen[c.ID] {
			return eventstore.ErrInvalidRecord
		}
		seen[c.ID] = true
		if *c.LandedVersionID != p.VersionID {
			return corestore.ErrRevisionConflict
		}
		b.add(c.ID, c.Revision, c.ProjectID, c.WorkItemID, *c.LandedVersionID, recordTime(*c.LandedAt), *c.LandedHeadSHA, *c.LandedMergeSHA, *c.LandedBaseRef, p.AttemptID, p.PayloadObjectKey, p.PayloadSHA256)
	}
	rows, err := s.db.QueryContext(ctx, b.source()+`SELECT c.id FROM change_requests c JOIN source ON c.organization_id=$1 AND c.id=source.id ORDER BY c.id FOR UPDATE OF c`, b.args...)
	if err != nil {
		return mapError(ctx, err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return mapError(ctx, err)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return mapError(ctx, err)
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN change_requests c ON c.organization_id=$1 AND c.id=source.id AND c.revision=source.revision AND c.project_id=source.project_id AND c.work_item_id=source.work_item_id AND c.current_version_id=source.landed_version_id
 JOIN change_landing_receipts r ON r.organization_id=c.organization_id AND r.attempt_id=source.attempt_id AND r.version_id=source.landed_version_id AND r.payload_object_key=source.payload_object_key AND r.payload_sha256=source.payload_sha256 AND r.merge_sha=source.landed_merge_sha AND r.base_ref=source.landed_base_ref
 JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=source.landed_version_id AND v.head_sha=source.landed_head_sha
 WHERE (c.landed_version_id IS NULL AND c.landed_at IS NULL AND c.landed_head_sha IS NULL AND c.landed_merge_sha IS NULL AND c.landed_base_ref IS NULL) OR (c.landed_version_id,c.landed_at,c.landed_head_sha,c.landed_merge_sha,c.landed_base_ref)=(source.landed_version_id,source.landed_at,source.landed_head_sha,source.landed_merge_sha,source.landed_base_ref)`, corestore.ErrRevisionConflict); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, b.source()+`UPDATE change_requests c SET landed_version_id=source.landed_version_id,landed_at=source.landed_at,landed_head_sha=source.landed_head_sha,landed_merge_sha=source.landed_merge_sha,landed_base_ref=source.landed_base_ref FROM source WHERE c.organization_id=$1 AND c.id=source.id AND c.landed_version_id IS NULL`, b.args...)
	return mapError(ctx, err)
}
