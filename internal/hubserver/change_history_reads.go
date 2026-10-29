package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const changeLegacyReadLimit = 100

type changeLegacyRows struct {
	queryPage     func(int) (*sql.Rows, error)
	rows          *sql.Rows
	offset, count int
	err           error
	closed        bool
	cursor        []any
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

func queryLegacyPageRows(ctx context.Context, query nativeQueryer, statement, order string, args ...any) (*changeLegacyRows, error) {
	keys := strings.Split(order, ",")
	rows := &changeLegacyRows{cursor: make([]any, len(keys))}
	statement = strings.Replace(statement, "SELECT ", "SELECT "+order+",", 1)
	rows.queryPage = func(offset int) (*sql.Rows, error) {
		pageArgs := append([]any{}, args...)
		page := statement
		if offset != 0 {
			page += " AND (" + order + ")>(" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")"
			pageArgs = append(pageArgs, rows.cursor...)
		}
		pageArgs = append(pageArgs, changeLegacyReadLimit)
		return query.QueryContext(ctx, page+" ORDER BY "+order+" LIMIT ?", pageArgs...)
	}
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

func (r *changeLegacyRows) Scan(dest ...any) error {
	columns := make([]any, 0, len(r.cursor)+len(dest))
	for index := range r.cursor {
		columns = append(columns, &r.cursor[index])
	}
	return r.rows.Scan(append(columns, dest...)...)
}
func (r *changeLegacyRows) Err() error   { return r.err }
func (r *changeLegacyRows) Close() error { r.closed = true; return r.rows.Close() }
