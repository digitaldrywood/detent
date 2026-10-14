package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"testing"
	"time"
)

func TestTimedConnectionsAttributeStatements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timed.db")
	writer, err := openTimedWriter(sqliteWriterDSN(path, time.Second, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	if _, err := writer.ExecContext(t.Context(), "CREATE TABLE items(id INTEGER PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	reader, err := openTimedReader(sqliteReaderDSN(path, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	for _, test := range []struct {
		name     string
		run      func(context.Context) error
		holdsTxs int64
	}{
		{"writer statement holds the writer", func(ctx context.Context) error {
			_, err := writer.ExecContext(ctx, "INSERT INTO items(name) VALUES (?)", "item")
			return err
		}, 1},
		{"reader transaction never counts as writer hold", func(ctx context.Context) error {
			tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var name string
			if err := tx.QueryRowContext(ctx, "SELECT name FROM items").Scan(&name); err != nil {
				return err
			}
			return tx.Commit()
		}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			traced := 0
			timing := &requestTiming{trace: func(string, []driver.NamedValue) { traced++ }}
			ctx, hold := withWriterTiming(withRequestTiming(t.Context(), timing))
			if err := test.run(ctx); err != nil {
				t.Fatal(err)
			}
			if hold.txs.Load() != test.holdsTxs || timing.db.Load() <= 0 || traced != 1 {
				t.Fatalf("writer txs=%d db=%d traced=%d", hold.txs.Load(), timing.db.Load(), traced)
			}
		})
	}
}
