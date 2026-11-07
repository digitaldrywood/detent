package postgres

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"time"
)

func (s *session) NextHealthPage(ctx context.Context) (healthstore.Finding, error) {
	r := healthstore.Finding{OrganizationID: s.organization}
	err := s.db.QueryRow(ctx, `SELECT id,page_generation,page_source_at,evidence_object_key,evidence_sha256,evidence_bytes,evidence_content_bytes
 FROM health_findings WHERE organization_id=$1 AND page_checkpoint<page_generation ORDER BY page_examined_at,id LIMIT 1`, s.organization).Scan(&r.ID, &r.Generation, &r.SourceAt, &r.Evidence.Key, &r.Evidence.SHA256, &r.Evidence.CompressedSize, &r.Evidence.UncompressedSize)
	r.Evidence = eventstore.Reference(r.Evidence.Key, r.Evidence.SHA256, r.Evidence.CompressedSize, r.Evidence.UncompressedSize)
	return r, mapError(ctx, err)
}

func (s *session) PublishHealthPage(ctx context.Context, r healthstore.Finding, e healthstore.Evidence) (bool, error) {
	if r.OrganizationID != s.organization {
		return false, corestore.ErrTenantMismatch
	}
	if r.Generation < 1 {
		return false, corestore.ErrInvalidPage
	}
	var countKeys, valueKeys, queueKeys []string
	var countValues, queueDepths []int64
	var valueValues, queueShort, queueLong, queueBaseline []float64
	for k, v := range e.Counts {
		countKeys = append(countKeys, k)
		countValues = append(countValues, int64(v))
	}
	for k, v := range e.Values {
		valueKeys = append(valueKeys, k)
		valueValues = append(valueValues, v)
	}
	for k, v := range e.Queues {
		queueKeys = append(queueKeys, k)
		queueDepths = append(queueDepths, int64(v.QueueDepth))
		queueShort = append(queueShort, v.ShortP50)
		queueLong = append(queueLong, v.LongP50)
		queueBaseline = append(queueBaseline, v.BaselineP90)
	}
	var productivityFrom, productivityTo, lastReportAt, lastGreenAt *time.Time
	var failureStage, lastRunURL string
	var cadence, window int64
	if p := e.Productivity; p != nil {
		productivityFrom = &p.WindowFrom
		productivityTo = &p.WindowTo
		failureStage = p.LatestFailureStage
	}
	if p := e.ScheduledValidation; p != nil {
		lastReportAt = &p.LastReportAt
		lastGreenAt = p.LastGreenAt
		lastRunURL = p.LastRunURL
		cadence = int64(p.CadenceSeconds)
		window = int64(p.WindowSeconds)
	}
	tag, err := s.db.Exec(ctx, `UPDATE health_findings SET page_event_ids=$4,page_attempt_ids=$5,page_work_item_ids=$6,page_signatures=$7,page_reason=$8,page_count_keys=$9,page_count_values=$10,page_value_keys=$11,page_value_values=$12,page_queue_keys=$13,page_queue_depths=$14,page_queue_short=$15,page_queue_long=$16,page_queue_baseline=$17,page_productivity_from=$18,page_productivity_to=$19,page_failure_stage=$20,page_last_report_at=$21,page_last_run_url=$22,page_last_green_at=$23,page_cadence_seconds=$24,page_window_seconds=$25,page_checkpoint=$3,page_projected_at=clock_timestamp()
 WHERE organization_id=$1 AND id=$2 AND page_generation=$3`, s.organization, r.ID, r.Generation, e.EventIDs, e.AttemptIDs, e.WorkItemIDs, e.Signatures, e.Reason, countKeys, countValues, valueKeys, valueValues, queueKeys, queueDepths, queueShort, queueLong, queueBaseline, productivityFrom, productivityTo, failureStage, lastReportAt, lastRunURL, lastGreenAt, cadence, window)
	return tag.RowsAffected() == 1, mapError(ctx, err)
}

func (s *session) AdvanceHealthPage(ctx context.Context, r healthstore.Finding) error {
	if r.OrganizationID != s.organization {
		return corestore.ErrTenantMismatch
	}
	_, err := s.db.Exec(ctx, `UPDATE health_findings SET page_examined_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 AND page_generation=$3`, s.organization, r.ID, r.Generation)
	return mapError(ctx, err)
}
