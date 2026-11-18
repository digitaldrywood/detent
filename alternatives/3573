package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (f *runnerEventSession) FindRunnerSession(ctx context.Context, p tracker.ProjectID, issue, attempt string, lookup store.AgentResumeLookup) (runnersession.Record, error) {
	return f.sessions.FindRunnerSession(ctx, p, issue, attempt, lookup)
}
func (f *runnerEventSession) RunnerSessionHistory(ctx context.Context, p tracker.ProjectID, id, after int64, limit int) ([]runnersession.History, error) {
	return f.sessions.RunnerSessionHistory(ctx, p, id, after, limit)
}
func (f *runnerEventSession) RunnerSessionProcesses(ctx context.Context, p tracker.ProjectID, machine, attempt string, limit int) ([]runnersession.Record, error) {
	return f.sessions.RunnerSessionProcesses(ctx, p, machine, attempt, limit)
}

func (f *runnerEventSession) CreateRunnerSession(ctx context.Context, r runnersession.Record, h runnersession.History) (runnersession.Record, error) {
	f.queueRunFence(false)
	ref := r.Reference
	history := h.Reference
	query := f.pending.Queue(`WITH created AS(INSERT INTO runner_sessions(organization_id,project_id,issue_id,attempt_id,start_key,started_at,backend_id,backend_kind,role,model,policy_id,provider_thread_id,provider_session_id,pr_number,pr_head_sha,pr_base_sha,final_state,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$16,$17,$18,$19,$20,$21,$12,$13,$14,$15) ON CONFLICT(organization_id,project_id,start_key) DO NOTHING RETURNING *), history AS(INSERT INTO runner_session_history(organization_id,session_id,revision,mutation_key,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)SELECT $1,id,1,$5,$22,$23,$24,$25 FROM created RETURNING id) SELECT `+runnerSessionColumns+`,(SELECT id FROM history) FROM created`, f.session.organization, r.Project, r.IssueID, r.AttemptID, h.Key, r.StartedAt, r.BackendID, r.BackendKind, r.Role, r.Model, r.PolicyID, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, r.ProviderThreadID, r.ProviderSessionID, r.PRNumber, r.PRHeadSHA, r.PRBaseSHA, r.FinalState, history.Key, history.SHA256, history.CompressedSize, history.UncompressedSize)
	var receipt int64
	var created runnersession.Record
	query.QueryRow(func(row pgx.Row) error {
		var err error
		created, err = scanRunnerSession(sessionReceiptRow{row: row, receipt: &receipt})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	err := f.flushRunEvents(ctx)
	if err == nil && created.ID == 0 {
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) {
		created, err = scanRunnerSession(f.session.db.QueryRow(ctx, `SELECT `+runnerSessionColumns+` FROM runner_sessions WHERE organization_id=$1 AND project_id=$2 AND start_key=$3`, f.session.organization, r.Project, h.Key))
		if err == nil && (created.AttemptID != r.AttemptID || created.Reference.SHA256 != ref.SHA256) {
			err = corestore.ErrRevisionConflict
		}
	}
	created.ReceiptID = receipt
	return created, mapError(ctx, err)
}

type sessionReceiptRow struct {
	row     pgx.Row
	receipt *int64
}

func (r sessionReceiptRow) Scan(dest ...any) error { return r.row.Scan(append(dest, r.receipt)...) }

func (f *runnerEventSession) UpdateRunnerSession(ctx context.Context, r runnersession.Record, h runnersession.History) (int64, error) {
	if r.Revision < 2 || h.Revision != r.Revision || h.SessionID != r.ID {
		return 0, corestore.ErrRevisionConflict
	}
	f.queueRunFence(false)
	ref, history := r.Reference, h.Reference
	var id int64
	f.pending.Queue(`WITH changed AS(UPDATE runner_sessions SET revision=$1,completed_at=$2,backend_id=$3,backend_kind=$4,role=$5,model=$6,payload_object_key=$7,payload_sha256=$8,payload_bytes=$9,payload_content_bytes=$10,worker_pid=$15,worker_group_id=$16,worker_started_at=$17,worker_reaped_at=$18,provider_thread_id=$19,provider_session_id=$20,final_state=$21 WHERE organization_id=$11 AND project_id=$12 AND id=$13 AND revision=$14 RETURNING id) INSERT INTO runner_session_history(organization_id,session_id,revision,mutation_key,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes) SELECT $11,id,$1,$22,$23,$24,$25,$26 FROM changed RETURNING id`, r.Revision, r.CompletedAt, r.BackendID, r.BackendKind, r.Role, r.Model, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, f.session.organization, r.Project, r.ID, r.Revision-1, r.WorkerPID, r.WorkerGroupID, r.WorkerStartedAt, r.WorkerReapedAt, r.ProviderThreadID, r.ProviderSessionID, r.FinalState, h.Key, history.Key, history.SHA256, history.CompressedSize, history.UncompressedSize).QueryRow(func(row pgx.Row) error {
		err := row.Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return corestore.ErrRevisionConflict
		}
		return err
	})
	err := f.flushRunEvents(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		err = corestore.ErrRevisionConflict
	}
	return id, mapError(ctx, err)
}

func (f *runnerEventSession) RegisterSessionWorkerProcess(ctx context.Context, receipt ProcessReceipt, at time.Time) (ProcessReceipt, error) {
	if err := f.flushRunEvents(ctx); err != nil {
		return ProcessReceipt{}, err
	}
	return f.session.RegisterSessionWorkerProcess(ctx, receipt, at)
}

var _ runnersession.Writer = (*runnerEventSession)(nil)
