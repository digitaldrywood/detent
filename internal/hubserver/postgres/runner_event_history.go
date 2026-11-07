package postgres

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func (f *runnerEventSession) AppendEvent(ctx context.Context, record eventstore.Event) (eventstore.Event, error) {
	if record.OrganizationID != f.session.organization || record.ProjectID != f.command.Project || record.WorkItemID != f.command.WorkItem {
		return eventstore.Event{}, corestore.ErrTenantMismatch
	}
	if record.ID == "" || record.Type == "" || record.SchemaVersion != 1 || record.Sequence != 0 || record.ActorKind == "" || record.ActorPrincipalID == "" {
		return eventstore.Event{}, eventstore.ErrInvalidRecord
	}
	if record.PayloadFormatVersion == 0 {
		record.PayloadFormatVersion = 2
	}
	if record.PayloadFormatVersion < 1 || record.PayloadFormatVersion > 2 {
		return eventstore.Event{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.Event{}, err
	}
	if previous, ok := f.events[record.ID]; ok {
		requested := record
		requested.Report = nil
		requested.Sequence = previous.Sequence
		requested.ProjectSequence = previous.ProjectSequence
		if requested.RecordedAt.IsZero() || recordTime(requested.RecordedAt).Equal(previous.RecordedAt) {
			requested.RecordedAt = previous.RecordedAt
		}
		if !reflect.DeepEqual(previous, requested) {
			return eventstore.Event{}, corestore.ErrRevisionConflict
		}
		return previous, nil
	}
	f.item.value.EventSequence++
	record.Sequence = f.item.value.EventSequence
	record.RecordedAt = recordTime(record.RecordedAt)
	_, err := pgquery.New(runEventQueue{f.pending}).CommandAppendRunEvent(ctx, pgquery.CommandAppendRunEventParams{PushKnown: record.PushKnown, PushChangeID: record.PushChangeID, PushVersionID: record.PushVersionID, PushHeadSha: record.PushHeadSHA, PushExternalEffect: record.PushExternalEffect, PushEffectState: record.PushEffectState, OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Type: record.Type, SchemaVersion: record.SchemaVersion, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, EventRevision: record.EventRevision, ChangeID: record.ChangeID, VersionID: record.VersionID, HeadSHA: record.HeadSHA, LandingWaiting: record.LandingWaiting, LandingChangeID: record.LandingChangeID, LandingVersionID: record.LandingVersionID, LandingHeadSHA: record.LandingHeadSHA, RunID: record.RunID, AttemptID: record.AttemptID, FromState: record.FromState, ToState: record.ToState, RecordedAt: record.RecordedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	if err != nil {
		return eventstore.Event{}, err
	}
	f.events[record.ID] = record
	if record.Report != nil {
		p := record.Report
		f.pending.Queue(`INSERT INTO report_event_facts (organization_id,event_id,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT (organization_id,event_id) DO NOTHING`, f.session.organization, record.ID, p.RunSequence, p.RunStatus, p.Role, p.Model, p.Effort, p.Failed, p.FailureClass, p.FailureKind, p.LandingReceipt, p.Landed, p.LandingChangeID, p.ReworkClass, p.ReworkKind, p.DurationSeconds)
		f.pending.Queue(`SELECT run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, f.session.organization, record.ID).QueryRow(func(row pgx.Row) error {
			var actual reportstore.EventProjection
			if err := row.Scan(&actual.RunSequence, &actual.RunStatus, &actual.Role, &actual.Model, &actual.Effort, &actual.Failed, &actual.FailureClass, &actual.FailureKind, &actual.LandingReceipt, &actual.Landed, &actual.LandingChangeID, &actual.ReworkClass, &actual.ReworkKind, &actual.DurationSeconds); err != nil {
				return err
			}
			if actual != *p {
				return corestore.ErrRevisionConflict
			}
			return nil
		})
		f.pending.Queue(`INSERT INTO report_attempt_facts(organization_id,attempt_id,first_event_id,latest_event_id,outcome_event_id) SELECT $1,a.id,$2,$2,CASE WHEN $3='run.finished' THEN $2 END FROM native_attempts a WHERE a.organization_id=$1 AND a.id=$4
 ON CONFLICT(organization_id,attempt_id) DO UPDATE SET first_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.first_event_id)<(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.first_event_id) THEN EXCLUDED.first_event_id ELSE report_attempt_facts.first_event_id END,latest_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.latest_event_id)>(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.latest_event_id) THEN EXCLUDED.latest_event_id ELSE report_attempt_facts.latest_event_id END,outcome_event_id=CASE WHEN report_attempt_facts.outcome_event_id IS NULL THEN EXCLUDED.outcome_event_id WHEN EXCLUDED.outcome_event_id IS NOT NULL AND (SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.outcome_event_id)<(SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.outcome_event_id) THEN EXCLUDED.outcome_event_id ELSE report_attempt_facts.outcome_event_id END`, f.session.organization, record.ID, record.Type, record.AttemptID)
	}
	return record, nil
}

func (f *runnerEventSession) PutReceipt(ctx context.Context, record eventstore.Receipt) (eventstore.Receipt, error) {
	if err := f.session.checkReceipt(record); err != nil {
		return eventstore.Receipt{}, err
	}
	if f.receipt.err == nil {
		old := f.receipt.value
		if old.RequestHash != record.RequestHash || old.ResponseClaimed || old.ResponseExpired || old.PayloadSHA256 != record.PayloadSHA256 {
			return eventstore.Receipt{}, corestore.ErrRevisionConflict
		}
		return old, nil
	}
	if !errors.Is(f.receipt.err, corestore.ErrNotFound) {
		return eventstore.Receipt{}, f.receipt.err
	}
	f.pending.Queue(`INSERT INTO native_command_states(organization_id,actor_id,operation,command_key,request_hash,created_at,response_claimed,response_pending,response_request_id,response_expired,response_replay_until_ns,response_reconciliation_hash)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING command_key`, record.OrganizationID, record.ActorID, record.Operation, record.CommandKey, record.RequestHash, recordTime(record.CreatedAt), record.ResponseClaimed, record.ResponsePending, record.ResponseRequestID, record.ResponseExpired, record.ResponseReplayUntilNs, record.ResponseReconciliationHash).QueryRow(func(row pgx.Row) error { var key string; return row.Scan(&key) })
	q := pgquery.New(runEventQueue{f.pending})
	if _, err := q.RecordInsertReceipt(ctx, pgquery.RecordInsertReceiptParams{OrganizationID: record.OrganizationID, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey, RequestHash: record.RequestHash, CreatedAt: recordTime(record.CreatedAt), PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes}); err != nil {
		return eventstore.Receipt{}, err
	}
	ref := eventstore.Reference(record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes)
	if _, err := f.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: record.OrganizationID, ID: "att_" + artifact.Digest([]byte(string(record.OrganizationID) + "\n" + ref.Key))[:32], Reference: ref, CreatedAt: record.CreatedAt}); err != nil {
		return eventstore.Receipt{}, err
	}
	f.pending.Queue(`INSERT INTO archive_usage_counters(organization_id,compressed_bytes)VALUES($1,0) ON CONFLICT DO NOTHING`, record.OrganizationID)
	f.pending.Queue(`SELECT compressed_bytes FROM archive_usage_counters WHERE organization_id=$1 FOR UPDATE`, record.OrganizationID)
	f.pending.Queue(`WITH object AS MATERIALIZED(SELECT id,compressed_size,customer_owned FROM archive_objects WHERE organization_id=$1 AND object_key=$2 AND sha256=$3 FOR UPDATE),owned AS MATERIALIZED(SELECT EXISTS(SELECT 1 FROM archive_object_owners WHERE organization_id=$1 AND archive_id=(SELECT id FROM object)) AS present),added AS(INSERT INTO archive_object_owners(organization_id,archive_id,owner_kind,owner_id,created_at) SELECT $1,object.id,'native-command',$4,$5 FROM object CROSS JOIN owned ON CONFLICT DO NOTHING RETURNING archive_id) UPDATE archive_usage_counters SET compressed_bytes=compressed_bytes+(SELECT compressed_size FROM object) WHERE organization_id=$1 AND EXISTS(SELECT 1 FROM added) AND NOT(SELECT present FROM owned) AND NOT(SELECT customer_owned FROM object)`, record.OrganizationID, ref.Key, ref.SHA256, receiptOwnerID(record), recordTime(record.CreatedAt))
	f.receipt = runFact[eventstore.Receipt]{value: record}
	return record, nil
}
