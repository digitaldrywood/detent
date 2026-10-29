package hubserver

import (
	"context"
	"database/sql"
	"errors"
)

const changeLegacyReadLimit = 100

type changeLegacyRows struct {
	queryPage     func(int) (*sql.Rows, error)
	rows          *sql.Rows
	offset, count int
	err           error
	closed        bool
}

func queryChangeLegacyRows(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, statement string, args ...any) (*changeLegacyRows, error) {
	rows := &changeLegacyRows{queryPage: func(offset int) (*sql.Rows, error) {
		pageArgs := append(append([]any{}, args...), changeLegacyReadLimit, offset)
		return query.QueryContext(ctx, statement+" LIMIT ? OFFSET ?", pageArgs...)
	}}
	if err := rows.nextPage(); err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *changeLegacyRows) nextPage() error {
	rows, err := r.queryPage(r.offset)
	if err != nil {
		return err
	}
	r.rows, r.count = rows, 0
	return nil
}

func (r *changeLegacyRows) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	if r.rows.Next() {
		r.count++
		return true
	}
	r.err = errors.Join(r.rows.Err(), r.rows.Close())
	if r.err != nil || r.count < changeLegacyReadLimit {
		return false
	}
	r.offset += r.count
	r.err = r.nextPage()
	if r.err != nil {
		return false
	}
	return r.Next()
}

func (r *changeLegacyRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *changeLegacyRows) Err() error             { return r.err }
func (r *changeLegacyRows) Close() error           { r.closed = true; return r.rows.Close() }
