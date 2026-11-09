package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

type ReportEventProjectionImport struct {
	Event  eventstore.Event
	Legacy *eventstore.Event
}

type ReportLandingImport struct {
	Change  eventstore.Change
	Receipt eventstore.LandingReceipt
}

func (r *Repository) ImportReportEventProjection(ctx context.Context, event eventstore.Event) error {
	_, err := r.ImportReportEventProjections(ctx, []ReportEventProjectionImport{{Event: event}})
	return err
}

func (r *Repository) ImportReportEventProjections(ctx context.Context, records []ReportEventProjectionImport) (int, error) {
	if len(records) < 1 || len(records) > 100 {
		return 0, errors.New("invalid report event projection batch")
	}
	organization := records[0].Event.OrganizationID
	if organization == "" {
		return 0, eventstore.ErrInvalidRecord
	}
	for _, record := range records {
		event := record.Event
		if event.OrganizationID != organization || event.ProjectID == "" || event.WorkItemID == "" || event.ID == "" || event.Report == nil || event.PayloadSHA256 == "" {
			return 0, eventstore.ErrInvalidRecord
		}
		if record.Legacy != nil {
			legacy := record.Legacy
			if legacy.OrganizationID != organization || legacy.ProjectID != event.ProjectID || legacy.WorkItemID != event.WorkItemID || legacy.ID != event.ID || legacy.Report == nil || *legacy.Report != *event.Report || legacy.PayloadSHA256 == "" {
				return 0, eventstore.ErrInvalidRecord
			}
		}
	}
	legacyAnchors := 0
	err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		var err error
		legacyAnchors, err = s.importReportEvents(ctx, records)
		return err
	})
	if err != nil {
		return 0, err
	}
	return legacyAnchors, nil
}

func (s *session) putReportEventProjection(ctx context.Context, event eventstore.Event) error {
	if event.Report == nil {
		return nil
	}
	p := event.Report
	_, err := s.db.Exec(ctx, `INSERT INTO report_event_facts (organization_id,event_id,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds,process_evidence_status,process_peak_memory_bytes,process_user_cpu_seconds,process_system_cpu_seconds,process_wall_seconds,process_lease_id,process_fencing_token,process_runner_id,process_machine_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25) ON CONFLICT (organization_id,event_id) DO UPDATE SET process_evidence_status=EXCLUDED.process_evidence_status,process_peak_memory_bytes=EXCLUDED.process_peak_memory_bytes,process_user_cpu_seconds=EXCLUDED.process_user_cpu_seconds,process_system_cpu_seconds=EXCLUDED.process_system_cpu_seconds,process_wall_seconds=EXCLUDED.process_wall_seconds,process_lease_id=EXCLUDED.process_lease_id,process_fencing_token=EXCLUDED.process_fencing_token,process_runner_id=EXCLUDED.process_runner_id,process_machine_id=EXCLUDED.process_machine_id WHERE report_event_facts.process_evidence_status='unknown' AND EXCLUDED.process_evidence_status<>'unknown'`, s.organization, event.ID, p.RunSequence, p.RunStatus, p.Role, p.Model, p.Effort, p.Failed, p.FailureClass, p.FailureKind, p.LandingReceipt, p.Landed, p.LandingChangeID, p.ReworkClass, p.ReworkKind, p.DurationSeconds, p.ProcessEvidenceStatus, p.ProcessPeakMemoryBytes, p.ProcessUserCPUSeconds, p.ProcessSystemCPUSeconds, p.ProcessWallSeconds, p.ProcessLeaseID, p.ProcessFencingToken, p.ProcessRunnerID, p.ProcessMachineID)
	if err != nil {
		return mapError(ctx, err)
	}
	var actual reportstore.EventProjection
	err = s.db.QueryRow(ctx, `SELECT run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds,process_evidence_status,process_peak_memory_bytes,process_user_cpu_seconds,process_system_cpu_seconds,process_wall_seconds,process_lease_id,process_fencing_token,process_runner_id,process_machine_id FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, s.organization, event.ID).Scan(&actual.RunSequence, &actual.RunStatus, &actual.Role, &actual.Model, &actual.Effort, &actual.Failed, &actual.FailureClass, &actual.FailureKind, &actual.LandingReceipt, &actual.Landed, &actual.LandingChangeID, &actual.ReworkClass, &actual.ReworkKind, &actual.DurationSeconds, &actual.ProcessEvidenceStatus, &actual.ProcessPeakMemoryBytes, &actual.ProcessUserCPUSeconds, &actual.ProcessSystemCPUSeconds, &actual.ProcessWallSeconds, &actual.ProcessLeaseID, &actual.ProcessFencingToken, &actual.ProcessRunnerID, &actual.ProcessMachineID)
	if err != nil {
		return mapError(ctx, err)
	}
	if actual != *p {
		return errors.Join(corestore.ErrRevisionConflict, errors.New("report projection differs from immutable source"))
	}
	if event.AttemptID != "" {
		_, err := s.db.Exec(ctx, `INSERT INTO report_attempt_facts(organization_id,attempt_id,first_event_id,latest_event_id,outcome_event_id) SELECT $1,a.id,$2,$2,CASE WHEN $3='run.finished' THEN $2 END FROM native_attempts a WHERE a.organization_id=$1 AND a.id=$4
 ON CONFLICT(organization_id,attempt_id) DO UPDATE SET first_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.first_event_id)<(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.first_event_id) THEN EXCLUDED.first_event_id ELSE report_attempt_facts.first_event_id END,latest_event_id=CASE WHEN (SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.latest_event_id)>(SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.latest_event_id) THEN EXCLUDED.latest_event_id ELSE report_attempt_facts.latest_event_id END,outcome_event_id=CASE WHEN report_attempt_facts.outcome_event_id IS NULL THEN EXCLUDED.outcome_event_id WHEN EXCLUDED.outcome_event_id IS NOT NULL AND (SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=EXCLUDED.outcome_event_id)<(SELECT (recorded_at,sequence) FROM collaboration_events WHERE organization_id=$1 AND id=report_attempt_facts.outcome_event_id) THEN EXCLUDED.outcome_event_id ELSE report_attempt_facts.outcome_event_id END`, s.organization, event.ID, event.Type, event.AttemptID)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (r *Repository) ImportReportEscapeProjection(ctx context.Context, evidence eventstore.Evidence) error {
	return r.ImportReportEscapeProjections(ctx, []eventstore.Evidence{evidence})
}

func (r *Repository) ImportReportEscapeProjections(ctx context.Context, records []eventstore.Evidence) error {
	if len(records) < 1 || len(records) > 100 {
		return errors.New("invalid report escape projection batch")
	}
	organization := records[0].OrganizationID
	if organization == "" {
		return eventstore.ErrInvalidRecord
	}
	for _, record := range records {
		if record.OrganizationID != organization || record.ChangeID == "" || record.Sequence < 1 || record.ReportEscape == nil || record.PayloadSHA256 == "" {
			return eventstore.ErrInvalidRecord
		}
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		return s.importReportEscapes(ctx, records)
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
	_, err := s.db.Exec(ctx, `INSERT INTO report_escape_facts(organization_id,change_id,evidence_sequence,occurrence_id,kind,cause,observed_at,instance_id,human_override) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(organization_id,change_id,evidence_sequence) DO NOTHING`, s.organization, evidence.ChangeID, evidence.Sequence, p.OccurrenceID, p.Kind, p.Cause, p.ObservedAt, p.InstanceID, p.HumanOverride)
	if err != nil {
		return mapError(ctx, err)
	}
	var actual reportstore.EscapeProjection
	err = s.db.QueryRow(ctx, `SELECT occurrence_id,kind,cause,observed_at,instance_id,human_override FROM report_escape_facts WHERE organization_id=$1 AND change_id=$2 AND evidence_sequence=$3`, s.organization, evidence.ChangeID, evidence.Sequence).Scan(&actual.OccurrenceID, &actual.Kind, &actual.Cause, &actual.ObservedAt, &actual.InstanceID, &actual.HumanOverride)
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
	return r.ImportReportChangeLandings(ctx, []ReportLandingImport{{Change: record, Receipt: proof}})
}

func (r *Repository) ImportReportChangeLandings(ctx context.Context, records []ReportLandingImport) error {
	if len(records) < 1 || len(records) > 100 {
		return errors.New("invalid report landing batch")
	}
	organization := records[0].Change.OrganizationID
	if organization == "" {
		return eventstore.ErrInvalidRecord
	}
	for _, record := range records {
		change, proof := record.Change, record.Receipt
		if change.OrganizationID != organization || proof.OrganizationID != organization || change.ID == "" || change.LandedVersionID == nil || change.LandedAt == nil || change.LandedHeadSHA == nil || change.LandedMergeSHA == nil || change.LandedBaseRef == nil || proof.AttemptID == "" || proof.PayloadSHA256 == "" {
			return eventstore.ErrInvalidRecord
		}
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		return s.importReportLandings(ctx, records)
	})
}
