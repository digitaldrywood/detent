package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

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
	if queue, ok := s.db.(runEventQueue); ok {
		queue.batch.Queue(b.source()+query, b.args...).QueryRow(func(row pgx.Row) error {
			var count int
			if err := row.Scan(&count); err != nil {
				return err
			}
			if count != len(b.rows) {
				return mismatch
			}
			return nil
		})
		return nil
	}
	var count int
	if err := s.db.QueryRow(ctx, b.source()+query, b.args...).Scan(&count); err != nil {
		return mapError(ctx, err)
	}
	if count != len(b.rows) {
		return mismatch
	}
	return nil
}

func (s *session) mergeReportFacts(ctx context.Context, b *reportBatch, table, columns, keys string) error {
	if _, err := s.db.Exec(ctx, b.source()+"INSERT INTO "+table+"(organization_id,"+columns+") SELECT DISTINCT $1,"+columns+" FROM source ORDER BY "+keys+" ON CONFLICT(organization_id,"+keys+") DO NOTHING", b.args...); err != nil {
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
		b.add(r.ProjectID, r.WorkItemID, r.AttemptID, recordTime(r.Period), r.Provider, r.Model, r.Input, r.CachedInput, r.Output, r.Cost, strings.ToUpper(r.Currency), recordTime(r.UpdatedAt))
	}
	if err := s.lockReportUsage(ctx, b); err != nil {
		return err
	}
	var known bool
	err := s.db.QueryRow(ctx, strings.TrimSuffix(b.source(), " ")+`, fresh AS MATERIALIZED (
 SELECT DISTINCT s.* FROM source s WHERE NOT EXISTS(SELECT 1 FROM attempt_usage u WHERE u.organization_id=$1 AND u.usage_origin='import' AND (u.attempt_id,u.period,u.provider,u.model)=(s.attempt_id,s.period,s.provider,s.model) AND u.currency=ANY(ARRAY[s.currency,lower(s.currency),lower(substr(s.currency,1,1))||substr(s.currency,2),substr(s.currency,1,1)||lower(substr(s.currency,2,1))||substr(s.currency,3,1),substr(s.currency,1,2)||lower(substr(s.currency,3,1)),lower(substr(s.currency,1,2))||substr(s.currency,3,1),lower(substr(s.currency,1,1))||substr(s.currency,2,1)||lower(substr(s.currency,3,1)),substr(s.currency,1,1)||lower(substr(s.currency,2))]))
), pairs AS MATERIALIZED (
 SELECT DISTINCT attempt_id,provider,model FROM fresh
), currencies AS MATERIALIZED (
 SELECT DISTINCT attempt_id,provider,model,currency FROM fresh
), latest AS MATERIALIZED (
 SELECT p.*,u.period,u.currency,u.usage_origin,u.usage_input_total,u.usage_cached_input_total,u.usage_output_total FROM pairs p LEFT JOIN LATERAL (
  SELECT period,currency,usage_origin,usage_input_total,usage_cached_input_total,usage_output_total FROM attempt_usage WHERE organization_id=$1 AND attempt_id=p.attempt_id AND provider=p.provider AND model=p.model AND usage_input_total IS NOT NULL ORDER BY usage_sequence DESC,period DESC,updated_at DESC,currency COLLATE "C" DESC LIMIT 1
 ) u ON true
), latest_currency AS MATERIALIZED (
 SELECT p.*,u.period,u.usage_origin,u.usage_cost_total FROM currencies p LEFT JOIN LATERAL (
  SELECT period,usage_origin,usage_cost_total FROM attempt_usage WHERE organization_id=$1 AND attempt_id=p.attempt_id AND provider=p.provider AND model=p.model AND currency=p.currency AND usage_input_total IS NOT NULL ORDER BY usage_sequence DESC,period DESC,updated_at DESC LIMIT 1
 ) u ON true
), validity AS MATERIALIZED (
 SELECT NOT EXISTS(SELECT 1 FROM latest WHERE period IS NOT NULL AND (usage_input_total IS NULL OR usage_cached_input_total IS NULL OR usage_output_total IS NULL)) AND NOT EXISTS(SELECT 1 FROM latest_currency WHERE period IS NOT NULL AND usage_cost_total IS NULL) AS known
), calculated AS MATERIALIZED (
 SELECT f.*,coalesce(l.usage_input_total,0)+sum(f.input) OVER tokens AS total_input,coalesce(l.usage_cached_input_total,0)+sum(f.cached_input) OVER tokens AS total_cached,coalesce(l.usage_output_total,0)+sum(f.output) OVER tokens AS total_output,
 coalesce(c.usage_cost_total,0)+sum(f.cost_estimate) OVER costs AS total_cost
 FROM fresh f JOIN latest l USING(attempt_id,provider,model) JOIN latest_currency c ON (c.attempt_id,c.provider,c.model,c.currency)=(f.attempt_id,f.provider,f.model,f.currency)
 WINDOW tokens AS (PARTITION BY f.attempt_id,f.provider,f.model ORDER BY f.period,f.updated_at,f.currency COLLATE "C" ROWS UNBOUNDED PRECEDING),costs AS (PARTITION BY f.attempt_id,f.provider,f.model,f.currency ORDER BY f.period,f.updated_at ROWS UNBOUNDED PRECEDING)
), added AS (
 INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at,usage_input_total,usage_cached_input_total,usage_output_total,usage_cost_total,usage_sequence)
 SELECT $1,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at,total_input::bigint,total_cached::bigint,total_output::bigint,total_cost,0 FROM calculated
 ORDER BY attempt_id,period,provider,model,currency ON CONFLICT DO NOTHING RETURNING attempt_id,period,provider,model,currency,input,cached_input,output,cost_estimate
), deltas AS MATERIALIZED (
 SELECT attempt_id,provider,model,sum(input)::bigint AS input,sum(cached_input)::bigint AS cached,sum(output)::bigint AS output FROM added GROUP BY attempt_id,provider,model
), cost_deltas AS MATERIALIZED (
 SELECT attempt_id,provider,model,currency,sum(cost_estimate) AS cost FROM added GROUP BY attempt_id,provider,model,currency
), targets AS MATERIALIZED (
 SELECT attempt_id,provider,model,period,currency,usage_origin FROM latest WHERE period IS NOT NULL UNION SELECT attempt_id,provider,model,period,currency,usage_origin FROM latest_currency WHERE period IS NOT NULL
), refreshed AS (
 UPDATE attempt_usage u SET
 usage_input_total=CASE WHEN (u.period,u.currency,u.usage_origin)=(l.period,l.currency,l.usage_origin) THEN u.usage_input_total+d.input ELSE u.usage_input_total END,
 usage_cached_input_total=CASE WHEN (u.period,u.currency,u.usage_origin)=(l.period,l.currency,l.usage_origin) THEN u.usage_cached_input_total+d.cached ELSE u.usage_cached_input_total END,
 usage_output_total=CASE WHEN (u.period,u.currency,u.usage_origin)=(l.period,l.currency,l.usage_origin) THEN u.usage_output_total+d.output ELSE u.usage_output_total END,
 usage_cost_total=CASE WHEN (u.period,u.usage_origin)=(c.period,c.usage_origin) THEN u.usage_cost_total+coalesce(cd.cost,0) ELSE u.usage_cost_total END
 FROM targets t JOIN latest l USING(attempt_id,provider,model) JOIN deltas d USING(attempt_id,provider,model)
 LEFT JOIN latest_currency c ON (c.attempt_id,c.provider,c.model,c.currency)=(t.attempt_id,t.provider,t.model,t.currency)
 LEFT JOIN cost_deltas cd ON (cd.attempt_id,cd.provider,cd.model,cd.currency)=(t.attempt_id,t.provider,t.model,t.currency)
 WHERE u.organization_id=$1 AND (u.attempt_id,u.period,u.provider,u.model,u.currency,u.usage_origin)=(t.attempt_id,t.period,t.provider,t.model,t.currency,t.usage_origin) RETURNING u.attempt_id
)
SELECT known FROM validity`, b.args...).Scan(&known)
	if err != nil {
		return mapError(ctx, err)
	}
	return s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source s JOIN attempt_usage u ON u.organization_id=$1 AND u.usage_origin='import' AND (u.attempt_id,u.period,u.provider,u.model)=(s.attempt_id,s.period,s.provider,s.model) AND u.currency=ANY(ARRAY[s.currency,lower(s.currency),lower(substr(s.currency,1,1))||substr(s.currency,2),substr(s.currency,1,1)||lower(substr(s.currency,2,1))||substr(s.currency,3,1),substr(s.currency,1,2)||lower(substr(s.currency,3,1)),lower(substr(s.currency,1,2))||substr(s.currency,3,1),lower(substr(s.currency,1,1))||substr(s.currency,2,1)||lower(substr(s.currency,3,1)),substr(s.currency,1,1)||lower(substr(s.currency,2))]) WHERE (u.project_id,u.input,u.cached_input,u.output,u.cost_estimate,u.updated_at) IS NOT DISTINCT FROM (s.project_id,s.input,s.cached_input,s.output,s.cost_estimate,s.updated_at)`, corestore.ErrRevisionConflict)
}

func (s *session) verifyReportUsageBatch(ctx context.Context, sha string, records []ReportUsageImportCoverage) error {
	b := newReportBatch(s.organization, "attempt_id,source_sha256,row_count", "text", "text", "bigint")
	for _, r := range records {
		b.add(r.AttemptID, sha, r.RowCount)
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source WHERE row_count=(SELECT count(*) FROM (SELECT 1 FROM attempt_usage u WHERE u.organization_id=$1 AND u.usage_origin='import' AND u.attempt_id=source.attempt_id LIMIT source.row_count+1) bounded)`, corestore.ErrRevisionConflict); err != nil {
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
	b := newReportBatch(s.organization, "event_id,run_id,attempt_id,project_id,work_item_id,payload_object_key,payload_sha256,legacy_key,legacy_sha,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds,phase_timing_status,process_evidence_status,process_peak_memory_bytes,process_user_cpu_seconds,process_system_cpu_seconds,process_wall_seconds,process_lease_id,process_fencing_token,process_runner_id,process_machine_id,activity_stage,activity_no_change_status", "text", "text", "text", "text", "text", "text", "text", "text", "text", "bigint", "text", "text", "text", "text", "boolean", "text", "text", "boolean", "boolean", "text", "text", "text", "double precision", "text", "text", "bigint", "double precision", "double precision", "double precision", "text", "bigint", "text", "text", "text", "text")
	for _, r := range records {
		e, p := r.Event, r.Event.Report
		legacyKey, legacySHA := "", ""
		if r.Legacy != nil {
			legacyKey, legacySHA = r.Legacy.PayloadObjectKey, r.Legacy.PayloadSHA256
		}
		b.add(e.ID, e.RunID, e.AttemptID, e.ProjectID, e.WorkItemID, e.PayloadObjectKey, e.PayloadSHA256, legacyKey, legacySHA, p.RunSequence, p.RunStatus, p.Role, p.Model, p.Effort, p.Failed, p.FailureClass, p.FailureKind, p.LandingReceipt, p.Landed, p.LandingChangeID, p.ReworkClass, p.ReworkKind, p.DurationSeconds, p.PhaseTimingStatus, p.ProcessEvidenceStatus, p.ProcessPeakMemoryBytes, p.ProcessUserCPUSeconds, p.ProcessSystemCPUSeconds, p.ProcessWallSeconds, p.ProcessLeaseID, p.ProcessFencingToken, p.ProcessRunnerID, p.ProcessMachineID, p.ActivityStage, p.ActivityNoChangeStatus)
	}
	var count, legacy, hydrated int
	err := s.db.QueryRow(ctx, b.source()+`, anchors AS MATERIALIZED (
 SELECT source.event_id,(e.payload_object_key,e.payload_sha256) IS DISTINCT FROM (source.payload_object_key,source.payload_sha256) legacy
 FROM source JOIN collaboration_events e ON e.organization_id=$1 AND e.id=source.event_id AND e.project_id=source.project_id AND e.work_item_id=source.work_item_id AND e.run_id=source.run_id AND e.attempt_id=source.attempt_id
 WHERE (e.payload_object_key,e.payload_sha256)=(source.payload_object_key,source.payload_sha256) OR (source.legacy_sha<>'' AND (e.payload_object_key,e.payload_sha256)=(source.legacy_key,source.legacy_sha))
 ), hydrated AS (
UPDATE report_event_facts f SET process_evidence_status=source.process_evidence_status,process_peak_memory_bytes=source.process_peak_memory_bytes,process_user_cpu_seconds=source.process_user_cpu_seconds,process_system_cpu_seconds=source.process_system_cpu_seconds,process_wall_seconds=source.process_wall_seconds,process_lease_id=source.process_lease_id,process_fencing_token=source.process_fencing_token,process_runner_id=source.process_runner_id,process_machine_id=source.process_machine_id FROM source WHERE EXISTS(SELECT 1 FROM anchors WHERE anchors.event_id=source.event_id) AND f.organization_id=$1 AND f.event_id=source.event_id AND f.process_evidence_status='unknown' AND source.process_evidence_status<>'unknown' RETURNING f.event_id
 ) SELECT count(*),count(*) FILTER(WHERE legacy),(SELECT count(*) FROM hydrated) FROM anchors`, b.args...).Scan(&count, &legacy, &hydrated)
	if err != nil {
		return 0, mapError(ctx, err)
	}
	if count != len(records) {
		return 0, corestore.ErrRevisionConflict
	}
	if _, err := s.db.Exec(ctx, b.source()+`UPDATE report_event_facts f SET phase_timing_status=CASE WHEN f.phase_timing_status='' THEN source.phase_timing_status ELSE f.phase_timing_status END,
 activity_stage=CASE WHEN f.activity_stage='' THEN source.activity_stage ELSE f.activity_stage END,
 activity_no_change_status=CASE WHEN f.activity_stage='' THEN source.activity_no_change_status ELSE f.activity_no_change_status END
 FROM source JOIN collaboration_events e ON e.organization_id=$1 AND e.id=source.event_id AND e.project_id=source.project_id AND e.work_item_id=source.work_item_id AND e.run_id=source.run_id AND e.attempt_id=source.attempt_id
 WHERE f.organization_id=$1 AND f.event_id=source.event_id AND ((f.phase_timing_status='' AND source.phase_timing_status<>'') OR (f.activity_stage='' AND source.activity_stage<>'')) AND ((e.payload_object_key,e.payload_sha256)=(source.payload_object_key,source.payload_sha256) OR (source.legacy_sha<>'' AND (e.payload_object_key,e.payload_sha256)=(source.legacy_key,source.legacy_sha)))`, b.args...); err != nil {
		return 0, mapError(ctx, err)
	}
	if err := s.mergeReportFacts(ctx, b, "report_event_facts", "event_id,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds,phase_timing_status,process_evidence_status,process_peak_memory_bytes,process_user_cpu_seconds,process_system_cpu_seconds,process_wall_seconds,process_lease_id,process_fencing_token,process_runner_id,process_machine_id,activity_stage,activity_no_change_status", "event_id"); err != nil {
		return 0, err
	}
	_, err = s.db.Exec(ctx, b.source()+`, candidates AS (
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
	rows, err := s.db.Query(ctx, b.source()+`SELECT c.id FROM change_requests c JOIN source ON c.organization_id=$1 AND c.id=source.id ORDER BY c.id FOR UPDATE OF c`, b.args...)
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
	rows.Close()
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN change_requests c ON c.organization_id=$1 AND c.id=source.id AND c.revision=source.revision AND c.project_id=source.project_id AND c.work_item_id=source.work_item_id AND c.current_version_id=source.landed_version_id
 JOIN change_landing_receipts r ON r.organization_id=c.organization_id AND r.attempt_id=source.attempt_id AND r.version_id=source.landed_version_id AND r.payload_object_key=source.payload_object_key AND r.payload_sha256=source.payload_sha256 AND r.merge_sha=source.landed_merge_sha AND r.base_ref=source.landed_base_ref
 JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=source.landed_version_id AND v.head_sha=source.landed_head_sha
 WHERE (c.landed_version_id IS NULL AND c.landed_at IS NULL AND c.landed_head_sha IS NULL AND c.landed_merge_sha IS NULL AND c.landed_base_ref IS NULL) OR (c.landed_version_id,c.landed_at,c.landed_head_sha,c.landed_merge_sha,c.landed_base_ref)=(source.landed_version_id,source.landed_at,source.landed_head_sha,source.landed_merge_sha,source.landed_base_ref)`, corestore.ErrRevisionConflict); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, b.source()+`UPDATE change_requests c SET landed_version_id=source.landed_version_id,landed_at=source.landed_at,landed_head_sha=source.landed_head_sha,landed_merge_sha=source.landed_merge_sha,landed_base_ref=source.landed_base_ref FROM source WHERE c.organization_id=$1 AND c.id=source.id AND c.landed_version_id IS NULL`, b.args...)
	if err != nil {
		return mapError(ctx, err)
	}
	frozen := make([]eventstore.VersionLanding, len(records))
	for i, record := range records {
		frozen[i] = record.Frozen
	}
	return s.recordVersionLandingRows(ctx, frozen)
}

func (s *session) lockReportUsage(ctx context.Context, b *reportBatch) error {
	return s.verifyReportBatch(ctx, b, `, projects AS MATERIALIZED (
 SELECT p.organization_id,p.id FROM projects p JOIN (SELECT DISTINCT project_id FROM source) q ON q.project_id=p.id WHERE p.organization_id=$1 ORDER BY p.id FOR SHARE OF p
), items AS MATERIALIZED (
 SELECT i.organization_id,i.id,i.project_id,i.native_id FROM issues i JOIN (SELECT DISTINCT project_id,work_item_id FROM source) q ON q.project_id=i.project_id AND q.work_item_id=i.native_id JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id WHERE i.organization_id=$1 ORDER BY i.project_id,i.native_id FOR UPDATE OF i
), attempts AS MATERIALIZED (
 SELECT a.organization_id,a.id,a.issue_id FROM native_attempts a JOIN (SELECT DISTINCT attempt_id FROM source) q ON q.attempt_id=a.id JOIN items i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 ORDER BY a.id FOR UPDATE OF a
)
SELECT count(*) FROM source s JOIN attempts a ON a.organization_id=$1 AND a.id=s.attempt_id JOIN items i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=s.project_id AND i.native_id=s.work_item_id`, corestore.ErrTenantMismatch)
}

func (s *session) lockUsageReplay(ctx context.Context, b *reportBatch) error {
	return s.verifyReportBatch(ctx, b, `, projects AS MATERIALIZED (
 SELECT p.organization_id,p.id FROM projects p JOIN (SELECT DISTINCT project_id FROM source) q ON q.project_id=p.id WHERE p.organization_id=$1 ORDER BY p.id FOR SHARE OF p
), items AS MATERIALIZED (
 SELECT i.organization_id,i.id,i.project_id,i.native_id FROM work_item_identities i JOIN (SELECT DISTINCT project_id,work_item_id FROM source) q ON q.project_id=i.project_id AND q.work_item_id=i.native_id JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id WHERE i.organization_id=$1 ORDER BY i.project_id,i.native_id
), attempts AS MATERIALIZED (
 SELECT a.organization_id,a.id,a.issue_id FROM native_attempts a JOIN (SELECT DISTINCT attempt_id FROM source) q ON q.attempt_id=a.id JOIN items i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 ORDER BY a.id FOR UPDATE OF a
)
SELECT count(*) FROM source s JOIN attempts a ON a.organization_id=$1 AND a.id=s.attempt_id JOIN items i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=s.project_id AND i.native_id=s.work_item_id`, corestore.ErrTenantMismatch)
}
