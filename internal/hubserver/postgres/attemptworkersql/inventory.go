package attemptworkersql

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func Read(ctx context.Context, queries *pgquery.Queries, organization tracker.OrganizationID, runner string, machine tracker.MachineID, project tracker.ProjectID, after string, limit int, now time.Time) (runnerauth.AttemptWorkerPage, error) {
	if runner == "" || machine == "" || project == "" || limit < 1 || limit > runnerauth.AttemptWorkerPageSize || now.IsZero() {
		return runnerauth.AttemptWorkerPage{}, corestore.ErrInvalidPage
	}
	rows, err := queries.RunnerAttemptWorkers(ctx, pgquery.RunnerAttemptWorkersParams{OrganizationID: organization, ProjectID: project, RunnerID: runner, AfterID: after, PageSize: int32(limit + 1), Now: now})
	if err != nil {
		return runnerauth.AttemptWorkerPage{}, err
	}
	page := runnerauth.AttemptWorkerPage{ProjectID: project, Workers: make([]runnerauth.AttemptWorker, 0, min(limit, len(rows)))}
	if len(rows) > limit {
		page.Next = rows[limit-1].ID
		rows = rows[:limit]
	}
	for _, row := range rows {
		if row.MachineID != string(machine) || !row.WorkerPid.Valid || !row.WorkerPgid.Valid || row.WorkerStartedAt == nil || row.WorkerStartKey == nil {
			return runnerauth.AttemptWorkerPage{}, corestore.ErrTenantMismatch
		}
		page.Workers = append(page.Workers, runnerauth.AttemptWorker{
			ProjectID: project, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID), AttemptID: row.ID,
			LeaseID: tracker.LeaseID(row.LeaseID), FencingToken: tracker.FencingToken(row.FencingToken),
			MachineID: machine, LeaseState: runnerauth.AttemptLeaseState(row.LeaseState), ExpiresAt: row.ExpiresAt,
			Worker: tracker.NativeAttemptWorker{PID: row.WorkerPid.Int32, PGID: row.WorkerPgid.Int32, StartedAt: *row.WorkerStartedAt, StartKey: *row.WorkerStartKey},
		})
	}
	return page, nil
}

func Acknowledge(ctx context.Context, queries *pgquery.Queries, organization tracker.OrganizationID, cleanup leasestore.AttemptWorkerCleanup, clock func(context.Context) (time.Time, error)) error {
	if !cleanup.Worker.Valid() {
		return leasestore.ErrAttemptConflict
	}
	now, err := clock(ctx)
	if err != nil {
		return err
	}
	worker := cleanup.Worker
	_, err = queries.AcknowledgeAttemptWorker(ctx, pgquery.AcknowledgeAttemptWorkerParams{
		OrganizationID: organization, ProjectID: cleanup.ProjectID, WorkItemID: string(cleanup.WorkItemID), AttemptID: cleanup.AttemptID,
		LeaseID: cleanup.Fence.LeaseID, FencingToken: cleanup.Fence.FencingToken, RunnerID: new(cleanup.RunnerID), MachineID: string(cleanup.MachineID),
		Pid: worker.PID, Pgid: worker.PGID, StartedAt: worker.StartedAt.UTC(), StartKey: worker.StartKey, AcknowledgedAt: now,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return leasestore.ErrAttemptConflict
	}
	return err
}

func Columns(worker *tracker.NativeAttemptWorker) (pid, pgid pgtype.Int4, start *time.Time, key *string) {
	if worker != nil {
		return pgtype.Int4{Int32: worker.PID, Valid: true}, pgtype.Int4{Int32: worker.PGID, Valid: true}, new(worker.StartedAt.UTC()), &worker.StartKey
	}
	return
}
