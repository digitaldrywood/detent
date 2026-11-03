package postgres

import (
	"context"
	"errors"
	"reflect"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func (r *Repository) ImportReportEventProjection(ctx context.Context, event eventstore.Event) error {
	if event.Report == nil || event.OrganizationID == "" || event.PayloadSHA256 == "" {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, event.OrganizationID, corestore.Background, false, func(s *session) error {
		stored, err := s.queries.RecordGetEvent(ctx, pgquery.RecordGetEventParams{OrganizationID: event.OrganizationID, ProjectID: event.ProjectID, WorkItemID: event.WorkItemID, ID: event.ID})
		if err != nil {
			return mapError(ctx, err)
		}
		if stored.PayloadSHA256 != event.PayloadSHA256 || stored.PayloadObjectKey != event.PayloadObjectKey {
			return corestore.ErrRevisionConflict
		}
		return s.putReportEventProjection(ctx, event)
	})
}

func (s *session) putReportEventProjection(ctx context.Context, event eventstore.Event) error {
	if event.Report == nil {
		return nil
	}
	p := event.Report
	_, err := s.db.ExecContext(ctx, `INSERT INTO report_event_facts (organization_id,event_id,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT (organization_id,event_id) DO NOTHING`, s.organization, event.ID, p.RunSequence, p.RunStatus, p.Role, p.Model, p.Effort, p.Failed, p.FailureClass, p.FailureKind, p.LandingReceipt, p.Landed, p.LandingChangeID, p.ReworkClass, p.ReworkKind, p.DurationSeconds)
	if err != nil {
		return mapError(ctx, err)
	}
	var actual reportstore.EventProjection
	err = s.db.QueryRowContext(ctx, `SELECT run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, s.organization, event.ID).Scan(&actual.RunSequence, &actual.RunStatus, &actual.Role, &actual.Model, &actual.Effort, &actual.Failed, &actual.FailureClass, &actual.FailureKind, &actual.LandingReceipt, &actual.Landed, &actual.LandingChangeID, &actual.ReworkClass, &actual.ReworkKind, &actual.DurationSeconds)
	if err != nil {
		return mapError(ctx, err)
	}
	if actual != *p {
		return errors.Join(corestore.ErrRevisionConflict, errors.New("report projection differs from immutable source"))
	}
	if event.AttemptID != "" {
		_, err := s.db.ExecContext(ctx, `INSERT INTO report_attempt_facts(organization_id,attempt_id,first_event_id,latest_event_id,outcome_event_id) SELECT $1,a.id,$2,$2,CASE WHEN $3='run.finished' THEN $2 END FROM native_attempts a WHERE a.organization_id=$1 AND a.id=$4
 ON CONFLICT(organization_id,attempt_id) DO UPDATE SET first_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.first_event_id)<(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.first_event_id) THEN EXCLUDED.first_event_id ELSE report_attempt_facts.first_event_id END,latest_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.latest_event_id)>(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.latest_event_id) THEN EXCLUDED.latest_event_id ELSE report_attempt_facts.latest_event_id END,outcome_event_id=CASE WHEN report_attempt_facts.outcome_event_id IS NULL THEN EXCLUDED.outcome_event_id WHEN EXCLUDED.outcome_event_id IS NOT NULL AND (SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.outcome_event_id)<(SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.outcome_event_id) THEN EXCLUDED.outcome_event_id ELSE report_attempt_facts.outcome_event_id END`, s.organization, event.ID, event.Type, event.AttemptID)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (r *Repository) ImportReportEscapeProjection(ctx context.Context, evidence eventstore.Evidence) error {
	if evidence.ReportEscape == nil || evidence.OrganizationID == "" || evidence.Sequence < 1 {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, evidence.OrganizationID, corestore.Background, false, func(s *session) error {
		stored, err := s.queries.RecordGetEvidenceSequence(ctx, pgquery.RecordGetEvidenceSequenceParams{OrganizationID: evidence.OrganizationID, ChangeID: evidence.ChangeID, Sequence: evidence.Sequence})
		if err != nil {
			return mapError(ctx, err)
		}
		if stored.PayloadSHA256 != evidence.PayloadSHA256 || stored.PayloadObjectKey != evidence.PayloadObjectKey {
			return corestore.ErrRevisionConflict
		}
		return s.putReportEscapeProjection(ctx, evidence)
	})
}

func (s *session) putReportEscapeProjection(ctx context.Context, evidence eventstore.Evidence) error {
	p := evidence.ReportEscape
	if p == nil {
		return nil
	}
	if p.OccurrenceID == "" || p.ObservedAt.IsZero() {
		return eventstore.ErrInvalidRecord
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO report_escape_facts(organization_id,change_id,evidence_sequence,occurrence_id,kind,cause,observed_at,instance_id,human_override) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(organization_id,change_id,evidence_sequence) DO NOTHING`, s.organization, evidence.ChangeID, evidence.Sequence, p.OccurrenceID, p.Kind, p.Cause, p.ObservedAt, p.InstanceID, p.HumanOverride)
	if err != nil {
		return mapError(ctx, err)
	}
	var actual reportstore.EscapeProjection
	err = s.db.QueryRowContext(ctx, `SELECT occurrence_id,kind,cause,observed_at,instance_id,human_override FROM report_escape_facts WHERE organization_id=$1 AND change_id=$2 AND evidence_sequence=$3`, s.organization, evidence.ChangeID, evidence.Sequence).Scan(&actual.OccurrenceID, &actual.Kind, &actual.Cause, &actual.ObservedAt, &actual.InstanceID, &actual.HumanOverride)
	if err != nil {
		return mapError(ctx, err)
	}
	if actual.ObservedAt.Equal(p.ObservedAt) {
		actual.ObservedAt = p.ObservedAt
	}
	if actual != *p {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (r *Repository) ImportReportChangeLanding(ctx context.Context, record eventstore.Change, proof eventstore.LandingReceipt) error {
	if record.OrganizationID == "" || record.OrganizationID != proof.OrganizationID || record.LandedVersionID == nil || record.LandedAt == nil || record.LandedHeadSHA == nil || record.LandedMergeSHA == nil || record.LandedBaseRef == nil {
		return eventstore.ErrInvalidRecord
	}
	return r.transaction(ctx, record.OrganizationID, corestore.Background, false, func(s *session) error {
		current, err := s.LockChange(ctx, record.ID)
		if err != nil {
			return err
		}
		if current.Revision != record.Revision || current.ProjectID != record.ProjectID || current.WorkItemID != record.WorkItemID || current.CurrentVersionID == nil || *current.CurrentVersionID != proof.VersionID || *record.LandedVersionID != proof.VersionID {
			return corestore.ErrRevisionConflict
		}
		receipt, err := s.LandingReceipt(ctx, proof.AttemptID)
		if err != nil {
			return err
		}
		if receipt.PayloadObjectKey != proof.PayloadObjectKey || receipt.PayloadSHA256 != proof.PayloadSHA256 || receipt.VersionID != proof.VersionID || receipt.MergeSHA != *record.LandedMergeSHA || receipt.BaseRef != *record.LandedBaseRef {
			return corestore.ErrRevisionConflict
		}
		version, err := s.Version(ctx, record.ID, proof.VersionID)
		if err != nil {
			return err
		}
		if version.HeadSHA != *record.LandedHeadSHA {
			return corestore.ErrRevisionConflict
		}
		at := recordTimePointer(record.LandedAt)
		if current.LandedVersionID != nil || current.LandedAt != nil || current.LandedHeadSHA != nil || current.LandedMergeSHA != nil || current.LandedBaseRef != nil {
			if !reflect.DeepEqual(current.LandedVersionID, record.LandedVersionID) || !sameCoreImportOptionalTime(current.LandedAt, at) || !reflect.DeepEqual(current.LandedHeadSHA, record.LandedHeadSHA) || !reflect.DeepEqual(current.LandedMergeSHA, record.LandedMergeSHA) || !reflect.DeepEqual(current.LandedBaseRef, record.LandedBaseRef) {
				return corestore.ErrRevisionConflict
			}
			return nil
		}
		_, err = s.db.ExecContext(ctx, `UPDATE change_requests SET landed_version_id=$1,landed_at=$2,landed_head_sha=$3,landed_merge_sha=$4,landed_base_ref=$5 WHERE organization_id=$6 AND id=$7`, record.LandedVersionID, at, record.LandedHeadSHA, record.LandedMergeSHA, record.LandedBaseRef, s.organization, record.ID)
		return mapError(ctx, err)
	})
}
