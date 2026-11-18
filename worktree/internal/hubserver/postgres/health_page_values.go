package postgres

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
)

const healthPageColumns = `f.sequence,f.id,f.fingerprint,f.signal,f.class,f.subject_kind,f.subject_id,f.opened_at,f.last_seen_at,f.resolved_at,f.severity,f.summary,f.next_action,f.email_unavailable,f.slack_unavailable_at,f.slack_status_code,f.page_event_ids, f.page_attempt_ids, f.page_work_item_ids, f.page_signatures, f.page_reason, f.page_count_keys, f.page_count_values, f.page_value_keys, f.page_value_values, f.page_queue_keys, f.page_queue_depths, f.page_queue_short, f.page_queue_long, f.page_queue_baseline, f.page_productivity_from, f.page_productivity_to, f.page_failure_stage, f.page_last_report_at, f.page_last_run_url, f.page_last_green_at, f.page_cadence_seconds, f.page_window_seconds,f.page_generation,f.page_checkpoint,f.evidence_object_key,f.evidence_sha256,f.evidence_bytes,f.evidence_content_bytes`

func scanHealthPage(row interface{ Scan(...any) error }, tail ...any) (healthstore.Finding, int64, error) {
	var r healthstore.Finding
	e := &r.DisplayEvidence
	var countKeys, valueKeys, queueKeys []string
	var countValues, queueDepths []int64
	var valueValues, queueShort, queueLong, queueBaseline []float64
	var productivityFrom, productivityTo, lastReportAt, lastGreenAt *time.Time
	var failureStage, lastRunURL string
	var cadence, window, checkpoint int64
	args := []any{&r.Sequence, &r.ID, &r.Fingerprint, &r.Signal, &r.Class, &r.SubjectKind, &r.SubjectID, &r.OpenedAt, &r.LastSeenAt, &r.ResolvedAt, &r.Severity, &r.Summary, &r.NextAction, &r.EmailUnavailable, &r.SlackUnavailableAt, &r.SlackStatusCode, &e.EventIDs, &e.AttemptIDs, &e.WorkItemIDs, &e.Signatures, &e.Reason, &countKeys, &countValues, &valueKeys, &valueValues, &queueKeys, &queueDepths, &queueShort, &queueLong, &queueBaseline, &productivityFrom, &productivityTo, &failureStage, &lastReportAt, &lastRunURL, &lastGreenAt, &cadence, &window, &r.Generation, &checkpoint, &r.Evidence.Key, &r.Evidence.SHA256, &r.Evidence.CompressedSize, &r.Evidence.UncompressedSize}
	if err := row.Scan(append(args, tail...)...); err != nil {
		return r, checkpoint, err
	}
	r.Evidence = eventstore.Reference(r.Evidence.Key, r.Evidence.SHA256, r.Evidence.CompressedSize, r.Evidence.UncompressedSize)
	if len(countKeys) > 0 {
		e.Counts = map[string]int{}
		for i, k := range countKeys {
			e.Counts[k] = int(countValues[i])
		}
	}
	if len(valueKeys) > 0 {
		e.Values = map[string]float64{}
		for i, k := range valueKeys {
			e.Values[k] = valueValues[i]
		}
	}
	if len(queueKeys) > 0 {
		e.Queues = map[string]healthstore.QueueEvidence{}
		for i, k := range queueKeys {
			e.Queues[k] = healthstore.QueueEvidence{QueueDepth: int(queueDepths[i]), ShortP50: queueShort[i], LongP50: queueLong[i], BaselineP90: queueBaseline[i]}
		}
	}
	if productivityFrom != nil && productivityTo != nil {
		e.Productivity = &healthstore.ProductivityEvidence{WindowFrom: *productivityFrom, WindowTo: *productivityTo, LatestFailureStage: failureStage}
	}
	if lastReportAt != nil {
		e.ScheduledValidation = &healthstore.ScheduledEvidence{LastReportAt: *lastReportAt, LastRunURL: lastRunURL, LastGreenAt: lastGreenAt, CadenceSeconds: int(cadence), WindowSeconds: int(window)}
	}
	return r, checkpoint, nil
}
