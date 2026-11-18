package pgcore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type fakeRows struct {
	pgx.Rows
	remaining int
	closed    bool
}

func (r *fakeRows) Next() bool {
	if r.closed || r.remaining == 0 {
		return false
	}
	r.remaining--
	return true
}

func (r *fakeRows) Close()     { r.closed = true }
func (r *fakeRows) Err() error { return nil }

type fakeRow struct{}

func (fakeRow) Scan(...any) error { return nil }

type fakeDB struct {
	rows     *fakeRows
	queryErr error
}

func (f fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.rows, nil
}

func (f fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return fakeRow{} }

func TestTimedQueriesObserveStatementWhenResultIsConsumed(t *testing.T) {
	t.Parallel()
	drain := func(rows pgx.Rows) {
		for rows.Next() {
		}
	}
	for _, test := range []struct {
		name string
		run  func(TimedQueries, context.Context) (func(), error)
	}{
		{"rows drained by Next", func(q TimedQueries, ctx context.Context) (func(), error) {
			rows, err := q.Query(ctx, "SELECT board")
			return func() { drain(rows); rows.Close() }, err
		}},
		{"rows drained without Close", func(q TimedQueries, ctx context.Context) (func(), error) {
			rows, err := q.Query(ctx, "SELECT board")
			return func() { drain(rows) }, err
		}},
		{"rows closed early", func(q TimedQueries, ctx context.Context) (func(), error) {
			rows, err := q.Query(ctx, "SELECT board")
			return func() { rows.Next(); rows.Close() }, err
		}},
		{"row scanned", func(q TimedQueries, ctx context.Context) (func(), error) {
			row := q.QueryRow(ctx, "SELECT one")
			return func() { _ = row.Scan() }, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			observed := 0
			ctx := WithTransactionTiming(context.Background(), nil, func(time.Time, string) { observed++ })
			consume, err := test.run(TimedQueries{DBTX: fakeDB{rows: &fakeRows{remaining: 3}}}, ctx)
			if err != nil {
				t.Fatal(err)
			}
			if observed != 0 {
				t.Fatalf("statement observed %d times before its result was read", observed)
			}
			consume()
			if observed != 1 {
				t.Fatalf("observations = %d, want exactly one once the result was read", observed)
			}
		})
	}
}

func TestTimedQueriesObserveFailedQuery(t *testing.T) {
	t.Parallel()
	var observed int
	ctx := WithTransactionTiming(context.Background(), nil, func(time.Time, string) { observed++ })
	if _, err := (TimedQueries{DBTX: fakeDB{queryErr: errors.New("refused")}}).Query(ctx, "SELECT board"); err == nil {
		t.Fatal("Query succeeded, want the driver error")
	}
	if observed != 1 {
		t.Fatalf("observations = %d, want 1", observed)
	}
}
