package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerSessionColumns = `organization_id,project_id,issue_id,attempt_id,id,revision,start_key,started_at,completed_at,backend_id,backend_kind,role,model,policy_id,provider_thread_id,provider_session_id,pr_number,pr_head_sha,pr_base_sha,final_state,worker_pid,worker_group_id,worker_started_at,worker_reaped_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes`

func (s *session) RunnerSessionTime(ctx context.Context) (time.Time, error) {
	return s.queries.LeaseTime(ctx)
}

func scanRunnerSession(row interface{ Scan(...any) error }) (runnersession.Record, error) {
	var record runnersession.Record
	var completed, workerStart, workerReap pgtype.Timestamptz
	var key, digest string
	var size, contentSize int64
	err := row.Scan(&record.Organization, &record.Project, &record.IssueID, &record.AttemptID, &record.ID, &record.Revision, &record.StartKey, &record.StartedAt, &completed, &record.BackendID, &record.BackendKind, &record.Role, &record.Model, &record.PolicyID, &record.ProviderThreadID, &record.ProviderSessionID, &record.PRNumber, &record.PRHeadSHA, &record.PRBaseSHA, &record.FinalState, &record.WorkerPID, &record.WorkerGroupID, &workerStart, &workerReap, &key, &digest, &size, &contentSize)
	if completed.Valid {
		record.CompletedAt = &completed.Time
	}
	if workerStart.Valid {
		record.WorkerStartedAt = &workerStart.Time
	}
	if workerReap.Valid {
		record.WorkerReapedAt = &workerReap.Time
	}
	record.Reference = eventstore.Reference(key, digest, size, contentSize)
	return record, err
}

func (s *session) RunnerSession(ctx context.Context, project tracker.ProjectID, id int64) (runnersession.Record, error) {
	record, err := scanRunnerSession(s.db.QueryRow(ctx, `SELECT `+runnerSessionColumns+` FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND id=$3`, s.organization, project, id))
	return record, mapError(ctx, err)
}

func (s *session) FindRunnerSession(ctx context.Context, project tracker.ProjectID, issue, attempt string, lookup store.AgentResumeLookup) (runnersession.Record, error) {
	record, err := scanRunnerSession(s.db.QueryRow(ctx, `SELECT `+runnerSessionColumns+` FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND issue_id=$3 AND ($4='' OR backend_id=$4) AND ($5='' OR backend_kind=$5) AND ($6='' OR role=$6) AND ($7='' OR model=$7)
 AND (provider_thread_id<>'' OR provider_session_id<>'')
 AND (($8<>'' AND attempt_id=$8) OR ($8='' AND completed_at IS NOT NULL AND final_state='completed' AND $9>0 AND pr_number=$9 AND pr_head_sha=$10 AND pr_base_sha=$11))
 ORDER BY started_at DESC,id DESC LIMIT 1`, s.organization, project, issue, lookup.AgentBackendID, lookup.AgentBackendKind, lookup.AgentRole, lookup.RequestedModel, attempt, lookup.PRNumber, lookup.PRHeadSHA, lookup.PRBaseSHA))
	return record, mapError(ctx, err)
}

func (s *session) CreateRunnerSession(ctx context.Context, record runnersession.Record, history runnersession.History) (runnersession.Record, error) {
	key := history.Key
	ref := record.Reference
	created, err := scanRunnerSession(s.db.QueryRow(ctx, `INSERT INTO runner_sessions(organization_id,project_id,issue_id,attempt_id,start_key,started_at,backend_id,backend_kind,role,model,policy_id,provider_thread_id,provider_session_id,pr_number,pr_head_sha,pr_base_sha,final_state,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$16,$17,$18,$19,$20,$21,$12,$13,$14,$15) ON CONFLICT(organization_id,project_id,start_key) DO NOTHING RETURNING `+runnerSessionColumns, s.organization, record.Project, record.IssueID, record.AttemptID, key, record.StartedAt, record.BackendID, record.BackendKind, record.Role, record.Model, record.PolicyID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, record.ProviderThreadID, record.ProviderSessionID, record.PRNumber, record.PRHeadSHA, record.PRBaseSHA, record.FinalState))
	if errors.Is(err, pgx.ErrNoRows) {
		created, err = scanRunnerSession(s.db.QueryRow(ctx, `SELECT `+runnerSessionColumns+` FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND start_key=$3`, s.organization, record.Project, key))
		if err == nil && (created.AttemptID != record.AttemptID || created.Reference.SHA256 != record.Reference.SHA256) {
			err = corestore.ErrRevisionConflict
		}
		return created, mapError(ctx, err)
	}
	if err != nil {
		return created, mapError(ctx, err)
	}
	history.SessionID, history.Revision = created.ID, 1
	created.ReceiptID, err = s.insertRunnerSessionHistory(ctx, history)
	return created, err
}

func (s *session) UpdateRunnerSession(ctx context.Context, record runnersession.Record, history runnersession.History) (int64, error) {
	if record.Revision < 2 || history.Revision != record.Revision || history.SessionID != record.ID {
		return 0, corestore.ErrRevisionConflict
	}
	ref := record.Reference
	result, err := s.db.Exec(ctx, `UPDATE runner_sessions SET revision=$1,completed_at=$2,backend_id=$3,backend_kind=$4,role=$5,model=$6,payload_object_key=$7,payload_sha256=$8,payload_bytes=$9,payload_content_bytes=$10,worker_pid=$15,worker_group_id=$16,worker_started_at=$17,worker_reaped_at=$18,provider_thread_id=$19,provider_session_id=$20,final_state=$21 WHERE organization_id=$11 AND project_id=$12 AND id=$13 AND revision=$14`, record.Revision, record.CompletedAt, record.BackendID, record.BackendKind, record.Role, record.Model, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, s.organization, record.Project, record.ID, record.Revision-1, record.WorkerPID, record.WorkerGroupID, record.WorkerStartedAt, record.WorkerReapedAt, record.ProviderThreadID, record.ProviderSessionID, record.FinalState)
	if err != nil {
		return 0, mapError(ctx, err)
	}
	count := result.RowsAffected()
	if count != 1 {
		return 0, corestore.ErrRevisionConflict
	}
	return s.insertRunnerSessionHistory(ctx, history)
}

func (s *session) RunnerSessionProcesses(ctx context.Context, project tracker.ProjectID, machine, attempt string, limit int) ([]runnersession.Record, error) {
	if limit < 1 || limit > 200 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT `+runnerSessionColumns+` FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND attempt_id IN (SELECT a.id FROM native_attempts a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id WHERE a.organization_id=$1 AND l.machine_id=$3 AND ($4='' OR a.id=$4)) AND worker_reaped_at IS NULL AND (worker_pid>0 OR completed_at IS NULL) ORDER BY id LIMIT $5`, s.organization, project, machine, attempt, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []runnersession.Record
	for rows.Next() {
		record, err := scanRunnerSession(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, record)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) insertRunnerSessionHistory(ctx context.Context, history runnersession.History) (int64, error) {
	ref := history.Reference
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO runner_session_history(organization_id,session_id,revision,mutation_key,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, s.organization, history.SessionID, history.Revision, history.Key, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize).Scan(&id)
	return id, mapError(ctx, err)
}

func (s *session) RunnerSessionHistory(ctx context.Context, project tracker.ProjectID, id, after int64, limit int) ([]runnersession.History, error) {
	if limit < 1 || limit > 200 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT h.id,h.session_id,h.revision,h.mutation_key,h.payload_object_key,h.payload_sha256,h.payload_bytes,h.payload_content_bytes FROM runner_session_history h JOIN runner_sessions s ON s.organization_id=h.organization_id AND s.id=h.session_id WHERE s.organization_id=$1 AND s.project_id=$2 AND s.id=$3 AND h.revision>$4 ORDER BY h.revision LIMIT $5`, s.organization, project, id, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []runnersession.History
	for rows.Next() {
		var entry runnersession.History
		var key, digest string
		var size, contentSize int64
		if err := rows.Scan(&entry.ID, &entry.SessionID, &entry.Revision, &entry.Key, &key, &digest, &size, &contentSize); err != nil {
			return nil, mapError(ctx, err)
		}
		entry.Reference = eventstore.Reference(key, digest, size, contentSize)
		result = append(result, entry)
	}
	return result, mapError(ctx, rows.Err())
}
