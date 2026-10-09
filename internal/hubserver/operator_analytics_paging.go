package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
)

type analyticsRows struct {
	rows     *sql.Rows
	nextPage func(int) (*sql.Rows, error)
	err      error
	count    int
	offset   int
	closed   bool
}

func queryAnalyticsRows(ctx context.Context, q nativeQueryer, statement string, args ...any) (*analyticsRows, error) {
	values := slices.Clone(args)
	r := &analyticsRows{}
	if strings.HasSuffix(statement, "LIMIT ?") && len(values) > 0 {
		if limit, ok := values[len(values)-1].(int); ok && (limit == 0 || limit == maxAnalyticsPopulation+1) {
			statement += " OFFSET ?"
			values[len(values)-1] = maxAnalyticsPopulation
			values = append(values, 0)
			r.nextPage = func(offset int) (*sql.Rows, error) {
				values[len(values)-1] = offset
				return q.QueryContext(ctx, statement, values...)
			}
		}
	}
	var err error
	r.rows, err = q.QueryContext(ctx, statement, values...)
	return r, err
}

func (r *analyticsRows) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	if r.rows.Next() {
		r.count++
		return true
	}
	r.err = errors.Join(r.rows.Err(), r.rows.Close())
	if r.err != nil || r.nextPage == nil || r.count < maxAnalyticsPopulation {
		return false
	}
	r.offset += r.count
	r.count = 0
	r.rows, r.err = r.nextPage(r.offset)
	return r.err == nil && r.Next()
}

func (r *analyticsRows) Scan(dest ...any) error {
	return r.rows.Scan(dest...)
}

func (r *analyticsRows) Err() error {
	if r.err != nil {
		return r.err
	}
	if r.rows == nil {
		return nil
	}
	return r.rows.Err()
}

func (r *analyticsRows) Close() {
	r.closed = true
	if r.rows != nil {
		r.err = errors.Join(r.err, r.rows.Close())
	}
}
