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
	Frozen  eventstore.VersionLanding
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
	err := r.internalWrite(ctx, organization, func(s *session) error {
		var err error
		legacyAnchors, err = s.importReportEvents(ctx, records)
		return err
	})
	if err != nil {
		return 0, err
	}
	return legacyAnchors, nil
}

const reportEventProjectionColumns = "run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds,phase_timing_status,process_evidence_status,process_peak_memory_bytes,process_user_cpu_seconds,process_system_cpu_seconds,process_wall_seconds,process_lease_id,process_fencing_token,process_runner_id,process_machine_id,activity_stage,activity_no_change_status"

const insertReportEventProjection = `INSERT INTO report_event_facts (organization_id,event_id,` + reportEventProjectionColumns + `)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`

const selectReportEventProjection = `SELECT ` + reportEventProjectionColumns + ` FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`

func reportEventProjectionFields(p *reportstore.EventProjection) []any {
	return []any{
		&p.RunSequence,
		&p.RunStatus,
		&p.Role,
		&p.Model,
		&p.Effort,
		&p.Failed,
		&p.FailureClass,
		&p.FailureKind,
		&p.LandingReceipt,
		&p.Landed,
		&p.LandingChangeID,
		&p.ReworkClass,
		&p.ReworkKind,
		&p.DurationSeconds,
		&p.PhaseTimingStatus,
		&p.ProcessEvidenceStatus,
		&p.ProcessPeakMemoryBytes,
		&p.ProcessUserCPUSeconds,
		&p.ProcessSystemCPUSeconds,
		&p.ProcessWallSeconds,
		&p.ProcessLeaseID,
		&p.ProcessFencingToken,
		&p.ProcessRunnerID,
		&p.ProcessMachineID,
		&p.ActivityStage,
		&p.ActivityNoChangeStatus,
	}
}

func (s *session) putReportEventProjection(ctx context.Context, event eventstore.Event) error {
	if event.Report == nil {
		return nil
	}
	p := event.Report
	_, err := s.db.Exec(ctx, insertReportEventProjection+` ON CONFLICT (organization_id,event_id) DO UPDATE SET activity_stage=CASE WHEN report_event_facts.activity_stage='' THEN EXCLUDED.activity_stage ELSE report_event_facts.activity_stage END,activity_no_change_status=CASE WHEN report_event_facts.activity_stage='' THEN EXCLUDED.activity_no_change_status ELSE report_event_facts.activity_no_change_status END,phase_timing_status=CASE WHEN report_event_facts.phase_timing_status='' THEN EXCLUDED.phase_timing_status ELSE report_event_facts.phase_timing_status END,process_evidence_status=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_evidence_status ELSE report_event_facts.process_evidence_status END,process_peak_memory_bytes=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_peak_memory_bytes ELSE report_event_facts.process_peak_memory_bytes END,process_user_cpu_seconds=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_user_cpu_seconds ELSE report_event_facts.process_user_cpu_seconds END,process_system_cpu_seconds=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_system_cpu_seconds ELSE report_event_facts.process_system_cpu_seconds END,process_wall_seconds=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_wall_seconds ELSE report_event_facts.process_wall_seconds END,process_lease_id=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_lease_id ELSE report_event_facts.process_lease_id END,process_fencing_token=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_fencing_token ELSE report_event_facts.process_fencing_token END,process_runner_id=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_runner_id ELSE report_event_facts.process_runner_id END,process_machine_id=CASE WHEN report_event_facts.process_evidence_status='unknown' THEN EXCLUDED.process_machine_id ELSE report_event_facts.process_machine_id END WHERE (report_event_facts.process_evidence_status='unknown' AND EXCLUDED.process_evidence_status<>'unknown') OR (report_event_facts.phase_timing_status='' AND EXCLUDED.phase_timing_status<>'') OR (report_event_facts.activity_stage='' AND EXCLUDED.activity_stage<>'')`, append([]any{s.organization, event.ID}, reportEventProjectionFields(p)...)...)
	if err != nil {
		return mapError(ctx, err)
	}
	var actual reportstore.EventProjection
	err = s.db.QueryRow(ctx, selectReportEventProjection, s.organization, event.ID).Scan(reportEventProjectionFields(&actual)...)
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
	return r.internalWrite(ctx, organization, func(s *session) error {
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
	if _, err := s.db.Exec(ctx, `UPDATE change_evidence SET quality_at=$4 WHERE organization_id=$1 AND change_id=$2 AND sequence=$3 AND kind='discussion' AND quality_at IS NULL`, s.organization, evidence.ChangeID, evidence.Sequence, recordTime(p.ObservedAt)); err != nil {
		return mapError(ctx, err)
	}
	var bound bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND sequence=$3 AND kind='discussion' AND quality_at=$4)`, s.organization, evidence.ChangeID, evidence.Sequence, recordTime(p.ObservedAt)).Scan(&bound); err != nil {
		return mapError(ctx, err)
	}
	if !bound {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (r *Repository) ImportReportChangeLanding(ctx context.Context, record eventstore.Change, proof eventstore.LandingReceipt) error {
	frozen, err := eventstore.VersionLandingFromChange(record)
	if err != nil {
		return err
	}
	return r.ImportReportChangeLandings(ctx, []ReportLandingImport{{Change: record, Receipt: proof, Frozen: frozen}})
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
		frozen := record.Frozen
		if frozen.OrganizationID != organization || frozen.ProjectID != change.ProjectID || frozen.WorkItemID != change.WorkItemID || frozen.ChangeID != change.ID || frozen.VersionID != *change.LandedVersionID || !recordTime(frozen.At).Equal(recordTime(*change.LandedAt)) || frozen.HeadSHA != *change.LandedHeadSHA || frozen.MergeSHA != *change.LandedMergeSHA || frozen.BaseRef != *change.LandedBaseRef {
			return corestore.ErrRevisionConflict
		}

	}
	return r.internalWrite(ctx, organization, func(s *session) error {
		return s.importReportLandings(ctx, records)
	})
}
