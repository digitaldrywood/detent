package hubserver

import (
	"bytes"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTenantStorageReclaim(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		rows    int
		legacy  bool
		reclaim bool
	}{
		{name: "retention reclaims one bounded step", rows: 600, reclaim: true},
		{name: "below threshold", rows: 8},
		{name: "legacy awaits conversion", rows: 600, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			s := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
			if err := s.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			if err := s.stopGitHubReconciliation(); err != nil {
				t.Fatal(err)
			}
			d := s.database
			var logs bytes.Buffer
			d.logger = slog.New(slog.NewJSONHandler(&logs, nil))
			if test.legacy {
				for _, statement := range []string{"PRAGMA auto_vacuum=NONE", "VACUUM"} {
					if _, err := d.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := d.db.ExecContext(t.Context(), "CREATE TABLE reclaim_payload(id INTEGER PRIMARY KEY, data BLOB)"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.db.ExecContext(t.Context(), `WITH RECURSIVE ids(id) AS (VALUES(1) UNION ALL SELECT id+1 FROM ids WHERE id<?)
INSERT INTO reclaim_payload SELECT id,zeroblob(4096) FROM ids`, test.rows); err != nil {
				t.Fatal(err)
			}
			if _, err := d.db.ExecContext(t.Context(), "DELETE FROM reclaim_payload WHERE id>2"); err != nil {
				t.Fatal(err)
			}
			replicator, err := sql.Open("sqlite", sqliteDSN(d.path, defaultBusyTimeout))
			if err != nil {
				t.Fatal(err)
			}
			defer replicator.Close()
			checkpoint := func() int64 {
				t.Helper()
				var busy, frames, copied int
				if err := replicator.QueryRowContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &copied); err != nil {
					t.Fatal(err)
				}
				if busy != 0 {
					t.Fatalf("checkpoint busy=%d", busy)
				}
				info, err := os.Stat(d.path)
				if err != nil {
					t.Fatal(err)
				}
				return info.Size()
			}
			beforeBytes := checkpoint()
			var before, after int64
			if err := d.reader.QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&before); err != nil {
				t.Fatal(err)
			}
			if test.rows > 8 && before <= storageReclaimThreshold+storageReclaimPages {
				t.Fatalf("fixture has only %d free pages", before)
			}
			now = now.Add(time.Hour)
			s.maintainGitHubWebhooks(t.Context())
			if err := d.reader.QueryRowContext(t.Context(), "PRAGMA freelist_count").Scan(&after); err != nil {
				t.Fatal(err)
			}
			afterBytes := checkpoint()
			if test.reclaim {
				if reclaimed := before - after; reclaimed <= 0 || reclaimed > storageReclaimPages {
					t.Fatalf("reclaimed %d pages, want 1..%d", reclaimed, storageReclaimPages)
				}
				if afterBytes >= beforeBytes {
					t.Fatalf("database did not shrink: before=%d after=%d", beforeBytes, afterBytes)
				}
				for _, field := range []string{`"free_pages_before":`, `"free_pages":`, `"pages_reclaimed":`, `"page_limit":128`} {
					if !strings.Contains(logs.String(), field) {
						t.Fatalf("missing diagnostic %s: %s", field, &logs)
					}
				}
			} else if before != after || beforeBytes != afterBytes {
				t.Fatalf("unexpected reclaim: pages %d -> %d, bytes %d -> %d", before, after, beforeBytes, afterBytes)
			}
			if _, err := replicator.ExecContext(t.Context(), "UPDATE reclaim_payload SET data=zeroblob(4096) WHERE id IN (1,2)"); err != nil {
				t.Fatalf("replication connection cannot write after reclaim: %v", err)
			}
			var count int
			if err := d.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM reclaim_payload WHERE id IN (1,2) AND length(data)=4096").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("retained rows=%d, want 2", count)
			}
		})
	}
}
