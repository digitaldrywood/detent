package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTimedConnectionsAttributeStatements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timed.db")
	writer, err := openTimedWriter(sqliteWriterDSN(path, time.Second, false), slog.New(slog.DiscardHandler))
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
		name       string
		run        func(context.Context) error
		holdsTxs   int64
		statements int
		commit     bool
	}{
		{"writer statement holds the writer", func(ctx context.Context) error {
			_, err := writer.ExecContext(ctx, "INSERT INTO items(name) VALUES (?)", "item")
			return err
		}, 1, 1, false},
		{"writer query holds until rows close", func(ctx context.Context) error {
			rows, err := writer.QueryContext(ctx, "UPDATE items SET name = ? RETURNING name", "updated")
			if err != nil {
				return err
			}
			defer rows.Close()
			if writerTimingFrom(ctx).txs.Load() != 0 {
				return errors.New("writer query counted before rows closed")
			}
			if !rows.Next() {
				return fmt.Errorf("missing returned row: %w", rows.Err())
			}
			return rows.Close()
		}, 1, 1, false},
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
		}, 0, 1, true},
		{"empty writer transaction attributes commit", func(ctx context.Context) error {
			tx, err := writer.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			return tx.Commit()
		}, 1, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			traced := 0
			timing := &requestTiming{trace: func(string, []driver.NamedValue) { traced++ }}
			ctx, hold := withWriterTiming(withRequestTiming(t.Context(), timing))
			if err := test.run(ctx); err != nil {
				t.Fatal(err)
			}
			if test.commit {
				if len(timing.spans) != 1 || timing.spans[0].name != "tx_commit" || timing.db.Load() < int64(timing.spans[0].duration) {
					t.Fatalf("commit missing from DB time: db=%d spans=%+v", timing.db.Load(), timing.spans)
				}
			}
			if hold.txs.Load() != test.holdsTxs || timing.db.Load() <= 0 || traced != test.statements {
				t.Fatalf("writer txs=%d db=%d traced=%d", hold.txs.Load(), timing.db.Load(), traced)
			}
		})
	}
}

func TestLongWriterHoldLogsOwner(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		hold    time.Duration
		action  string
		request bool
		writer  bool
		logs    bool
	}{
		{"background commit", 1500 * time.Millisecond, "commit", false, true, true},
		{"request rollback", 1500 * time.Millisecond, "rollback", true, true, true},
		{"threshold included", time.Second, "commit", true, true, true},
		{"below threshold", time.Second - time.Nanosecond, "commit", false, true, false},
		{"autocommit", 2 * time.Second, "exec", false, true, true},
		{"runner wrapper", 2 * time.Second, "runner", true, true, true},
		{"runner admin wrapper", 2 * time.Second, "admin", false, true, true},
		{"conversation wrapper", 2 * time.Second, "conversation", false, true, true},
		{"hub wrapper", 2 * time.Second, "hub", false, true, true},
		{"native mutation wrappers", 2 * time.Second, "native", false, true, true},
		{"outbox wrapper", 2 * time.Second, "outbox", false, true, true},
		{"secret wrapper", 2 * time.Second, "secret", false, true, true},
		{"autocommit query close", 2 * time.Second, "query close", false, true, true},
		{"autocommit query exhaustion", 2 * time.Second, "query exhaustion", true, true, true},
		{"autocommit query row", 2 * time.Second, "query row", false, true, true},
		{"autocommit query error", 2 * time.Second, "query error", false, true, true},
		{"short autocommit query", time.Second - time.Nanosecond, "query close", false, true, false},
		{"reader query excluded", 2 * time.Second, "query close", false, false, false},
		{"transaction query counted once", 2 * time.Second, "transaction query", true, true, true},
		{"reader excluded", 2 * time.Second, "commit", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			logger := slog.New(hostedLogHandler{output: slog.NewJSONHandler(&output, nil)})
			probe, err := sql.Open("sqlite", "")
			if err != nil {
				t.Fatal(err)
			}
			base := probe.Driver()
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}
			calls := 0
			var hold time.Duration
			queryClosed := false
			db := sql.OpenDB(timedConnector{driver: base, dsn: ":memory:", writer: test.writer, logger: logger, now: func() time.Time {
				calls++
				now := time.Unix(0, 0)
				if calls > 1 && (!strings.HasPrefix(test.action, "query ") || test.action == "query error" || queryClosed) {
					return now.Add(hold)
				}
				return now
			}})
			t.Cleanup(func() { db.Close() })
			db.SetMaxOpenConns(1)
			if _, err := db.ExecContext(t.Context(), "CREATE TABLE items(name TEXT)"); err != nil {
				t.Fatal(err)
			}
			calls, hold = 0, test.hold
			closeQuery := func() {
				if output.Len() != 0 {
					t.Fatalf("logged before query closed: %s", output.String())
				}
				queryClosed = true
			}
			if test.request {
				s := &Service{config: Config{Logger: logger}}
				s.writerStatsAt.Store(time.Now().UnixNano())
				request := httptest.NewRequest("POST", "/items/private-path-sentinel?token=private-token-sentinel", strings.NewReader("private-body-sentinel"))
				c := echo.New().NewContext(request.WithContext(t.Context()), httptest.NewRecorder())
				c.SetPath("/items/:item")
				if err := s.timeWriterUse(func(c echo.Context) error {
					runWriterHoldTest(t, db, c, test.action, closeQuery)
					return nil
				})(c); err != nil {
					t.Fatal(err)
				}
			} else {
				c := echo.New().NewContext(httptest.NewRequestWithContext(t.Context(), "POST", "/items", nil), httptest.NewRecorder())
				runWriterHoldTest(t, db, c, test.action, closeQuery)
			}
			for _, sentinel := range []string{"private-path-sentinel", "private-token-sentinel", "private-body-sentinel", "private-query-sentinel", "private-value-sentinel"} {
				if strings.Contains(output.String(), sentinel) {
					t.Fatalf("log contains customer content: %s", output.String())
				}
			}
			if !test.logs && output.Len() != 0 {
				t.Fatalf("unexpected logs: %s", output.String())
			}
			var records []map[string]any
			decoder := json.NewDecoder(&output)
			for decoder.More() {
				var record map[string]any
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] == "hub writer held" {
					records = append(records, record)
				}
			}
			if !test.logs {
				if len(records) != 0 {
					t.Fatalf("unexpected long hold logs: %v", records)
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("long hold logs = %d, want 1: %v", len(records), records)
			}
			delete(records[0], "time")
			want := map[string]any{"level": "WARN", "msg": "hub writer held", "owner": "github.com/digitaldrywood/detent/internal/hubserver.runWriterHoldTest", "hold_ms": float64(test.hold.Milliseconds()), "txs": float64(1)}
			if test.request {
				want["route"] = "/items/:item"
			}
			if !reflect.DeepEqual(records[0], want) {
				t.Fatalf("record = %v, want %v", records[0], want)
			}
		})
	}
}

func BenchmarkWriterHoldAcquisition(b *testing.B) {
	ctx := context.Background()
	now := time.Unix(0, 0)
	conn := &timedConn{writer: true, now: func() time.Time { return now }}
	hold := conn.startWriterHold(ctx)
	if hold.n == 0 || hold.n > len(hold.pcs) {
		b.Fatalf("captured frames = %d, capacity = %d", hold.n, len(hold.pcs))
	}
	b.ReportAllocs()
	for b.Loop() {
		hold = conn.startWriterHold(ctx)
		conn.endWriterHold(&hold)
	}
	b.ReportMetric(float64(len(hold.pcs)), "pc_capacity/op")
}

func runWriterHoldTest(t *testing.T, db *sql.DB, c echo.Context, action string, closeQuery func()) {
	t.Helper()
	ctx := c.Request().Context()
	query := "INSERT INTO items(name) VALUES (? || 'private-query-sentinel')"
	if strings.HasPrefix(action, "query ") {
		query += " RETURNING name"
		if action == "query error" {
			rows, err := db.QueryContext(ctx, "UPDATE missing_items SET name = ? RETURNING name", "private-value-sentinel")
			if err == nil {
				defer rows.Close()
				t.Fatal("expected query error")
			}
			return
		}
		if action == "query row" {
			row := db.QueryRowContext(ctx, query, "private-value-sentinel")
			closeQuery()
			var name string
			if err := row.Scan(&name); err != nil {
				t.Fatal(err)
			}
			return
		}
		rows, err := db.QueryContext(ctx, query, "private-value-sentinel")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("missing returned row: %v", rows.Err())
		}
		closeQuery()
		if action == "query exhaustion" && rows.Next() {
			t.Fatal("unexpected second row")
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return
	}
	service := &Service{database: &database{db: db, now: time.Now}, config: Config{Logger: slog.New(slog.DiscardHandler)}}
	stop := errors.New("stop wrapper transaction")
	switch action {
	case "runner":
		service.database.now = func() time.Time { return time.Time{} }
		c.SetPath("/heartbeat")
		if err := service.runnerTransaction(c, 200, nil); err != nil {
			t.Fatal(err)
		}
		return
	case "admin":
		service.database.now = func() time.Time { return time.Time{} }
		if _, err := service.runnerAdminTransaction(ctx, nativeScope{}, false, nil); !errors.Is(err, ErrInvalidClock) {
			t.Fatalf("wrapper error = %v", err)
		}
		return
	case "conversation":
		conversation := &conversationService{server: service, store: newConversationStore(db)}
		if err := conversation.transact(ctx, func(*sql.Tx, time.Time) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("wrapper error = %v", err)
		}
		return
	case "native":
		if err := service.nativeMutation(c, tracker.Mutation{IdempotencyKey: "example-key"}, struct{}{}, nil); err != nil {
			t.Fatal(err)
		}
		return
	case "outbox":
		if _, err := service.commitOutbox(ctx, outboxRecord{}, nil); err == nil {
			t.Fatal("expected missing repository error")
		}
		return
	case "secret":
		if err := service.secretMutation(ctx, nativeScope{}, func(*sql.Tx) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("wrapper error = %v", err)
		}
		return
	case "hub":
		if err := service.hubTransact(ctx, func(*sql.Tx, time.Time) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("wrapper error = %v", err)
		}
		return
	}
	if action == "exec" {
		if _, err := db.ExecContext(ctx, query, "private-value-sentinel"); err != nil {
			t.Fatal(err)
		}
		return
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if action == "transaction query" {
		var name string
		if err := tx.QueryRowContext(ctx, query+" RETURNING name", "private-value-sentinel").Scan(&name); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := tx.ExecContext(ctx, query, "private-value-sentinel"); err != nil {
			t.Fatal(err)
		}
	}
	finishWriterHoldTest(t, tx, action)
}

func finishWriterHoldTest(t *testing.T, tx *sql.Tx, action string) {
	t.Helper()
	var err error
	if action == "rollback" {
		err = tx.Rollback()
	} else {
		err = tx.Commit()
	}
	if err != nil {
		t.Fatal(err)
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
