package maintenancerecords

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
)

type Transaction func(context.Context, tracker.OrganizationID, int64, bool, func(pgquery.DBTX) error) error

type MaintenanceCursor struct {
	Key, Project, Kind string
	ID                 int64
	At                 time.Time
}

func (run Transaction) MaintenanceCursor(ctx context.Context, organization tracker.OrganizationID, generation int64, feature string) (MaintenanceCursor, error) {
	var cursor MaintenanceCursor
	if feature == "" || generation < 1 {
		return cursor, corestore.ErrInvalidPage
	}
	err := run(ctx, organization, generation, true, func(db pgquery.DBTX) error {
		var at *time.Time
		err := db.QueryRow(ctx, `SELECT cursor_key,project,kind,cursor_id,cursor_at FROM hosted_maintenance_cursors WHERE organization_id=$1 AND feature=$2 AND generation=$3`, organization, feature, generation).Scan(&cursor.Key, &cursor.Project, &cursor.Kind, &cursor.ID, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if at != nil {
			cursor.At = *at
		}
		return err
	})
	return cursor, err
}

func (run Transaction) SetMaintenanceCursor(ctx context.Context, organization tracker.OrganizationID, generation int64, feature string, cursor MaintenanceCursor) error {
	if feature == "" || generation < 1 {
		return corestore.ErrInvalidPage
	}
	return run(ctx, organization, generation, false, func(db pgquery.DBTX) error {
		var at *time.Time
		if !cursor.At.IsZero() {
			at = &cursor.At
		}
		tag, err := db.Exec(ctx, `INSERT INTO hosted_maintenance_cursors AS stored (organization_id,feature,generation,cursor_key,project,kind,cursor_id,cursor_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (organization_id,feature) DO UPDATE SET generation=EXCLUDED.generation,cursor_key=EXCLUDED.cursor_key,project=EXCLUDED.project,kind=EXCLUDED.kind,cursor_id=EXCLUDED.cursor_id,cursor_at=EXCLUDED.cursor_at WHERE stored.generation<=EXCLUDED.generation`, organization, feature, generation, cursor.Key, cursor.Project, cursor.Kind, cursor.ID, at)
		if err == nil && tag.RowsAffected() != 1 {
			return corestore.ErrRevisionConflict
		}
		return err
	})
}
