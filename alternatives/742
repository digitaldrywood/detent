package cloudentry

import (
	"context"
	"time"
)

type store struct {
	db     *entryDatabase
	reader *entryDatabase
	close  func() error
}

func (s *store) schemaVersion(ctx context.Context) (int64, error) {
	return s.db.postgres.SchemaVersion(ctx)
}

func (s *store) schemaFingerprint(ctx context.Context) (string, error) {
	return s.db.postgres.SchemaFingerprint(ctx)
}

func (s *store) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	return s.close()
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
