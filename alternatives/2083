package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
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

func TestRequestTimingNamedSpans(t *testing.T) {
	start := time.Now()
	for _, test := range []struct {
		name   string
		spans  []timingSpan
		header string
		logged []slog.Attr
	}{
		{"no spans", nil, "", nil},
		{"spans", []timingSpan{{"reports_analytics", 30 * time.Millisecond}, {"reports_prior", 5 * time.Millisecond}}, ", reports_analytics;dur=30.0, reports_prior;dur=5.0",
			[]slog.Attr{slog.Group("spans_ms", slog.Int64("reports_analytics", 30), slog.Int64("reports_prior", 5))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			timing := &requestTiming{start: start, spans: test.spans}
			if header := timing.header(start.Add(time.Second)); !strings.HasSuffix(header, "total;dur=1000.0"+test.header) {
				t.Fatalf("header %q, want spans %q", header, test.header)
			}
			want := append([]any{"db_ms", int64(0)}, toAny(test.logged)...)
			if got := timing.logAttrs(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("log attrs %v, want %v", got, want)
			}
		})
	}
	t.Run("parts of one span add up", func(t *testing.T) {
		timing := &requestTiming{}
		for range 2 {
			timing.span("reports_analytics", time.Now().Add(-20*time.Millisecond))
		}
		if spans := timing.namedSpans(); len(spans) != 1 || spans[0].duration < 40*time.Millisecond {
			t.Fatalf("spans %v, want one reports_analytics span of at least 40ms", spans)
		}
	})
}

func toAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, attr)
	}
	return out
}
