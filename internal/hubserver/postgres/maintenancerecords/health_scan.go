package maintenancerecords

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HealthScan struct {
	Project          tracker.ProjectID
	RunnerName       string
	RunnerID         string
	AfterProject     tracker.ProjectID
	ItemProject      tracker.ProjectID
	ItemState        string
	ItemID           int64
	Conversation     string
	Question         string
	Finding          string
	WaitFinding      string
	WaitRevision     int64
	WaitConversation string
	WaitQuestion     string
	StartedAt        time.Time
	CycleFailed      bool
	Completed        [5]bool
}

func (run Transaction) HealthScan(ctx context.Context, org tracker.OrganizationID, generation int64) (HealthScan, error) {
	var c HealthScan
	var completed []bool
	err := run(ctx, org, generation, true, func(db pgquery.DBTX) error {
		err := db.QueryRow(ctx, `SELECT project, runner_name, runner_id, after_project, item_project, item_state, item_id, conversation, question, finding, wait_finding, wait_revision, wait_conversation, wait_question, started_at, cycle_failed, completed
FROM hosted_health_scan_cursors WHERE organization_id=$1 AND generation=$2`, org, generation).Scan(&c.Project, &c.RunnerName, &c.RunnerID, &c.AfterProject, &c.ItemProject, &c.ItemState, &c.ItemID, &c.Conversation, &c.Question, &c.Finding, &c.WaitFinding, &c.WaitRevision, &c.WaitConversation, &c.WaitQuestion, &c.StartedAt, &c.CycleFailed, &completed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err == nil {
			if len(completed) != len(c.Completed) {
				return errors.New("invalid health scan completion flags")
			}
			copy(c.Completed[:], completed)
		}
		return err
	})
	return c, err
}

func (run Transaction) SetHealthScan(ctx context.Context, org tracker.OrganizationID, generation int64, c HealthScan) error {
	return run(ctx, org, generation, false, func(db pgquery.DBTX) error {
		tag, err := db.Exec(ctx, `INSERT INTO hosted_health_scan_cursors AS stored (organization_id,generation,project,runner_name,runner_id,after_project,item_project,item_state,item_id,conversation,question,finding,wait_finding,wait_revision,wait_conversation,wait_question,started_at,cycle_failed,completed)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
ON CONFLICT(organization_id) DO UPDATE SET
generation=EXCLUDED.generation,
project=EXCLUDED.project,
runner_name=EXCLUDED.runner_name,
runner_id=EXCLUDED.runner_id,
after_project=EXCLUDED.after_project,
item_project=EXCLUDED.item_project,
item_state=EXCLUDED.item_state,
item_id=EXCLUDED.item_id,
conversation=EXCLUDED.conversation,
question=EXCLUDED.question,
finding=EXCLUDED.finding,
wait_finding=EXCLUDED.wait_finding,
wait_revision=EXCLUDED.wait_revision,
wait_conversation=EXCLUDED.wait_conversation,
wait_question=EXCLUDED.wait_question,
started_at=EXCLUDED.started_at,
cycle_failed=EXCLUDED.cycle_failed,
completed=EXCLUDED.completed
WHERE stored.generation<=EXCLUDED.generation`, org, generation, c.Project, c.RunnerName, c.RunnerID, c.AfterProject, c.ItemProject, c.ItemState, c.ItemID, c.Conversation, c.Question, c.Finding, c.WaitFinding, c.WaitRevision, c.WaitConversation, c.WaitQuestion, c.StartedAt, c.CycleFailed, c.Completed[:])
		if err == nil && tag.RowsAffected() != 1 {
			return corestore.ErrRevisionConflict
		}
		return err
	})
}
