package postgres

import (
	"context"
	"database/sql"
	"errors"
)

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
