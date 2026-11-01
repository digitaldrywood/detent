package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"modernc.org/sqlite"
)

type onlineBackuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

func (d *database) backup(ctx context.Context, destination string) (resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	destinationPath, err := canonicalDatabasePath(destination)
	if err != nil {
		return fmt.Errorf("resolve hub backup destination: %w", err)
	}
	if destinationPath == d.path {
		return ErrBackupSource
	}

	reserved, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reserve hub backup destination: %w", err)
	}
	if err := reserved.Close(); err != nil {
		return errors.Join(fmt.Errorf("close reserved hub backup destination: %w", err), os.Remove(destinationPath))
	}
	complete := false
	defer func() {
		if complete {
			return
		}
		removeErr := os.Remove(destinationPath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		resultErr = errors.Join(resultErr, removeErr)
	}()

	connection, err := d.reader.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire hub backup connection: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	transaction, err := connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("start hub backup read transaction: %w", err)
	}
	defer func() {
		if err := transaction.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	var applicationID int64
	if err := transaction.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return fmt.Errorf("establish hub backup snapshot: %w", err)
	}
	backupErr := connection.Raw(func(driverConnection any) error {
		backuper, ok := driverConnection.(onlineBackuper)
		if !ok {
			return errors.New("sqlite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(destinationPath)
		if err != nil {
			return fmt.Errorf("start hub online backup: %w", err)
		}
		var stepErr error
		for more := true; more && stepErr == nil; {
			if err := ctx.Err(); err != nil {
				stepErr = err
				break
			}
			more, stepErr = backup.Step(256)
		}
		return errors.Join(stepErr, backup.Finish())
	})
	if backupErr != nil {
		return fmt.Errorf("create hub online backup: %w", backupErr)
	}
	complete = true
	return nil
}
