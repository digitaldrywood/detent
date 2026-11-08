package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func VerifyNeutralCoreCutover(ctx context.Context, directory string, organization tracker.OrganizationID) error {
	return ReadNeutralImportBundle(ctx, directory, organization, func(source *VerifiedNeutralImport) error {
		for _, table := range []string{"artifact_services", "artifact_grants", "artifact_references"} {
			rows, err := source.Page(ctx, table, nil, 1)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				return errors.New("production source contains artifact records requiring import before cutover")
			}
		}
		return nil
	})
}

func VerifyCoreCutoverCopy(ctx context.Context, path string) error {
	return ReadCoreImportCopy(ctx, path, func(tx *sql.Tx, _ string) error {
		var populated bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifact_services) OR EXISTS(SELECT 1 FROM artifact_grants) OR EXISTS(SELECT 1 FROM artifact_references)`).Scan(&populated); err != nil {
			return err
		}
		if populated {
			return errors.New("production copy contains artifact records requiring import before cutover")
		}
		return nil
	})
}
