package postgres

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type OperatorOrphanCursor struct {
	StartedAt time.Time
	SessionID int64
}

type OperatorOrphanQuery struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	RunnerID       string
	EnrollmentID   string
	MachineID      tracker.MachineID
	Before         *OperatorOrphanCursor
	Now            time.Time
	Limit          int
}

type OperatorOrphanSession = pgquery.OperatorOrphanSessionsRow

type OperatorOrphanPage struct {
	Sessions []OperatorOrphanSession
	Next     *OperatorOrphanCursor
	Scanned  int64
	Done     bool
}

type OperatorOrphanReader interface {
	ScanOrphanedAgentSessions(context.Context, OperatorOrphanQuery) (OperatorOrphanPage, error)
}

func (r *Repository) ReadOperatorOrphans(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(OperatorOrphanReader) error) error {
	return r.transactionIsolation(ctx, org, workload, true, pgx.RepeatableRead, func(s *session) error { return run(s) })
}

func (s *session) ScanOrphanedAgentSessions(ctx context.Context, arg OperatorOrphanQuery) (OperatorOrphanPage, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return OperatorOrphanPage{}, err
	}
	if arg.ProjectID == "" || arg.RunnerID == "" || arg.EnrollmentID == "" || arg.MachineID == "" || arg.Now.IsZero() {
		return OperatorOrphanPage{}, corestore.ErrInvalidPage
	}
	if err := checkPage(arg.Limit); err != nil {
		return OperatorOrphanPage{}, err
	}
	before := OperatorOrphanCursor{StartedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), SessionID: math.MaxInt64}
	if arg.Before != nil {
		if arg.Before.StartedAt.IsZero() || arg.Before.SessionID < 1 {
			return OperatorOrphanPage{}, corestore.ErrInvalidPage
		}
		before = *arg.Before
	}
	identity, err := s.runnerIdentityRow(ctx, `runner_id=$2`, arg.RunnerID)
	if err != nil {
		return OperatorOrphanPage{}, err
	}
	if identity.EnrollmentID != arg.EnrollmentID || identity.MachineID != arg.MachineID {
		return OperatorOrphanPage{}, corestore.ErrRevisionConflict
	}
	through, err := s.queries.OperatorOrphanScanThrough(ctx, pgquery.OperatorOrphanScanThroughParams{
		OrganizationID: arg.OrganizationID, ProjectID: arg.ProjectID, BeforeTime: before.StartedAt, BeforeID: before.SessionID, PageLimit: int32(arg.Limit),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return OperatorOrphanPage{Done: true}, nil
	}
	if err != nil {
		return OperatorOrphanPage{}, mapError(ctx, err)
	}
	rows, err := s.queries.OperatorOrphanSessions(ctx, pgquery.OperatorOrphanSessionsParams{
		OrganizationID: arg.OrganizationID, ProjectID: arg.ProjectID, BeforeTime: before.StartedAt, BeforeID: before.SessionID,
		ThroughTime: through.StartedAt, ThroughID: through.ID, RunnerID: arg.RunnerID, MachineID: string(arg.MachineID), Now: arg.Now, PageLimit: int32(arg.Limit),
	})
	if err != nil {
		return OperatorOrphanPage{}, mapError(ctx, err)
	}
	return OperatorOrphanPage{Sessions: rows, Next: &OperatorOrphanCursor{StartedAt: through.StartedAt, SessionID: through.ID}, Scanned: through.Scanned, Done: through.Scanned < int64(arg.Limit)}, nil
}
