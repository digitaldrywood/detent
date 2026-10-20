package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Service) maintainChangeSources(ctx context.Context) {
	deadline := time.Now().Add(analyticsBackfillBudget)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		more, err := s.database.backfillChangeSource(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.config.Logger.WarnContext(ctx, "backfill change source encoding", "error", err)
		}
		if err != nil || !more {
			return
		}
	}
}

func (d *database) backfillChangeSource(ctx context.Context) (bool, error) {
	var versionID, changeID string
	var bundle []byte
	err := d.reader.QueryRowContext(ctx, `SELECT s.version_id,v.change_id,s.bundle FROM change_sources s
JOIN change_versions v ON v.id=s.version_id WHERE substr(s.bundle,1,4)<>x'00444353' ORDER BY s.version_id LIMIT 1`).Scan(&versionID, &changeID, &bundle)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select legacy change source: %w", err)
	}
	version, err := readChangeVersionSnapshot(ctx, d.reader, changeID, versionID)
	if err != nil {
		return false, err
	}
	if version.Source == nil {
		return false, errors.New("legacy change source metadata is missing")
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle); err != nil {
		return false, err
	}
	stored, err := encodeChangeSource(bundle)
	if err != nil {
		return false, err
	}
	restored, err := decodeChangeSource(stored, version.Source.Bytes)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(restored, bundle) {
		return false, errors.New("change source compression changed source bytes")
	}
	if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, restored); err != nil {
		return false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE change_sources SET bundle=? WHERE version_id=? AND bundle=?", stored, versionID, bundle); err != nil {
		return false, fmt.Errorf("store compressed change source: %w", err)
	}
	if err := d.commit(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}
