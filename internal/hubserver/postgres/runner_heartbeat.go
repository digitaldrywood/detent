package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerHeartbeatIdentityKey struct{}
type runnerHeartbeatIdentity struct {
	session  *session
	identity *runnerstore.Identity
}
type runnerHeartbeatSession struct {
	*session
	identity                              runnerstore.Identity
	problems                              []runnerauth.Problem
	repositories                          runnerstore.RepositoryList
	hasRepositories, hasUpdate, hasUrgent bool
}

func (r *Repository) WriteRunnerHeartbeat(ctx context.Context, organization tracker.OrganizationID, credentialID, runnerID string, write func(context.Context, tenantstore.Writer) error) (resultErr error) {
	if _, ok := ctx.Value(readRequestKey{}).(*readRequest); ok {
		return corestore.ErrTenantMismatch
	}
	release, err := r.acquire(ctx, corestore.Serving)
	if err != nil {
		return err
	}
	defer release()
	initial := new(pgx.Batch)
	initial.Queue(runnerIdentitySelect+`token_id=$2 FOR UPDATE`, organization, credentialID)
	initial.Queue(`SELECT ARRAY[COALESCE(r.request_id,''),COALESCE(r.reason,'')],ARRAY[r.runner_id IS NOT NULL,COALESCE(r.pending,false),
 EXISTS(SELECT 1 FROM runner_update_desired WHERE organization_id=$1 AND runner_id=$2) OR EXISTS(SELECT 1 FROM runner_update_observations WHERE organization_id=$1 AND runner_id=$2),
 EXISTS(SELECT 1 FROM runner_urgent_updates WHERE organization_id=$1)],ARRAY[r.listed_at]
 FROM (SELECT 1) seed LEFT JOIN runner_repository_lists r ON r.organization_id=$1 AND r.runner_id=$2
 UNION ALL
 SELECT ARRAY[code,project_id,subject,check_name,error_output,fix_command,message,fix_hint],ARRAY[]::boolean[],ARRAY[reported_at,first_seen]
 FROM (SELECT * FROM runner_problem_reports WHERE organization_id=$1 AND runner_id=$2 ORDER BY code,project_id,subject,check_name LIMIT 129) problems`, organization, runnerID)
	tx, results, err := r.runtime.pools.BeginBatch(ctx, pgtransport.Scope{Organization: organization}, pgtransport.Options{}, initial)
	if err != nil {
		return mapError(ctx, err)
	}
	defer func() { resultErr = errors.Join(resultErr, mapError(ctx, tx.Rollback(ctx))) }()
	identity, err := scanRunnerIdentity(ctx, results.QueryRow())
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			err = accessstore.ErrDenied
		}
		return errors.Join(err, results.Close())
	}
	if identity.RunnerID != runnerID {
		return errors.Join(accessstore.ErrDenied, results.Close())
	}
	db := timedQueries{DBTX: tx}
	h := &runnerHeartbeatSession{session: &session{organization: organization, db: db, queries: pgquery.New(db)}, identity: identity}
	rows, err := results.Query()
	if err != nil {
		return errors.Join(mapError(ctx, err), results.Close())
	}
	for rows.Next() {
		var values []string
		var flags []bool
		var times []*time.Time
		if err := rows.Scan(&values, &flags, &times); err != nil {
			rows.Close()
			return errors.Join(mapError(ctx, err), results.Close())
		}
		if len(flags) > 0 {
			h.hasRepositories, h.hasUpdate, h.hasUrgent = flags[0], flags[2], flags[3]
			h.repositories = runnerstore.RepositoryList{OrganizationID: organization, RunnerID: identity.RunnerID, RequestID: values[0], Reason: values[1], Pending: flags[1], ListedAt: times[0]}
		} else {
			if len(h.problems) == maxRunnerProblemRows {
				rows.Close()
				return errors.Join(corestore.ErrInvalidPage, results.Close())
			}
			h.problems = append(h.problems, runnerauth.Problem{Code: values[0], ProjectID: values[1], Subject: values[2], Check: values[3], ErrorOutput: values[4], FixCommand: values[5], Message: values[6], FixHint: values[7], ReportedAt: *times[0], FirstSeen: *times[1]})
		}
	}
	if err := errors.Join(rows.Err(), results.Close()); err != nil {
		return mapError(ctx, err)
	}
	h.credentials = make(map[string]accessstore.Credential)
	if _, err := h.AccessCredential(ctx, credentialID); err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			err = accessstore.ErrDenied
		}
		return err
	}
	h.identity.ProjectIDs, err = h.runnerRoutingProjects(ctx, &h.identity, runnerstore.MaxRunnerProjectList)
	if err != nil {
		return err
	}
	ctx = context.WithValue(ctx, runnerHeartbeatIdentityKey{}, runnerHeartbeatIdentity{session: h.session, identity: &h.identity})
	if err := write(ctx, h); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mapError(ctx, tx.Commit(ctx))
}

func (h *runnerHeartbeatSession) RunnerProjects(_ context.Context, id string, limit int) ([]tracker.ProjectID, error) {
	if id != h.identity.RunnerID {
		return nil, corestore.ErrNotFound
	}
	if limit < 1 || limit > runnerstore.MaxRunnerProjectList {
		return nil, corestore.ErrInvalidPage
	}
	if len(h.identity.ProjectIDs) > limit {
		return nil, runnerstore.ErrLimitExceeded
	}
	return append([]tracker.ProjectID{}, h.identity.ProjectIDs...), nil
}
func (h *runnerHeartbeatSession) RunnerProblems(_ context.Context, id string) ([]runnerauth.Problem, error) {
	if id != h.identity.RunnerID {
		return nil, corestore.ErrNotFound
	}
	return append([]runnerauth.Problem{}, h.problems...), nil
}
func (h *runnerHeartbeatSession) RunnerRepositoryList(_ context.Context, id string) (runnerstore.RepositoryList, error) {
	if id != h.identity.RunnerID || !h.hasRepositories {
		return runnerstore.RepositoryList{}, corestore.ErrNotFound
	}
	return h.repositories, nil
}
func (h *runnerHeartbeatSession) RunnerUpdateState(ctx context.Context, id string) (runnerstore.UpdateState, error) {
	if id == h.identity.RunnerID && !h.hasUpdate {
		return runnerstore.UpdateState{OrganizationID: h.organization, RunnerID: id}, nil
	}
	return h.session.RunnerUpdateState(ctx, id)
}
func (h *runnerHeartbeatSession) RunnerUrgentUpdate(ctx context.Context) (runnerstore.UrgentUpdate, error) {
	if !h.hasUrgent {
		return runnerstore.UrgentUpdate{OrganizationID: h.organization}, nil
	}
	return h.session.RunnerUrgentUpdate(ctx)
}
func (h *runnerHeartbeatSession) ReplaceRunnerUpdateObservation(ctx context.Context, id string, report *runnerauth.UpdateObservation) error {
	h.hasUpdate = true
	return h.session.ReplaceRunnerUpdateObservation(ctx, id, report)
}

func (h *runnerHeartbeatSession) LockRunnerUpdate(_ context.Context, id string) error {
	if id != h.identity.RunnerID {
		return corestore.ErrNotFound
	}
	return nil
}

func (h *runnerHeartbeatSession) UpdateRunnerIdentity(ctx context.Context, row runnerstore.Identity) error {
	if err := h.session.UpdateRunnerIdentity(ctx, row); err != nil {
		return err
	}
	if row.RunnerID == h.identity.RunnerID {
		h.identity.DisplayName = row.DisplayName
		h.identity.CapacityLimit = row.CapacityLimit
		h.identity.ReportedCapacity = row.ReportedCapacity
		h.identity.OS = row.OS
		h.identity.Architecture = row.Architecture
		h.identity.LastHeartbeatAt = pgtransport.Timestamp(row.LastHeartbeatAt)
		h.identity.ExpiresAt = pgtransport.Timestamp(row.ExpiresAt)
	}
	return nil
}

func (h *runnerHeartbeatSession) SetRunnerUpdateDesired(ctx context.Context, id string, revision int64, requestID string) error {
	if err := h.session.SetRunnerUpdateDesired(ctx, id, revision, requestID); err != nil {
		return err
	}
	if id == h.identity.RunnerID {
		h.identity.Revision = revision + 1
		h.hasUpdate = true
	}
	return nil
}
