package hubserver

import (
	"context"
	"database/sql"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
)

func queryChangeLegacyRows(ctx context.Context, query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, statement string, args ...any) (*hubrecords.ChangeLegacyRows, error) {
	rows := &hubrecords.ChangeLegacyRows{QueryPage: func(offset int) (*sql.Rows, error) {
		pageArgs := append(append([]any{}, args...), hubrecords.ChangeLegacyReadLimit, offset)
		return query.QueryContext(ctx, statement+" LIMIT ? OFFSET ?", pageArgs...)
	}}
	if err := rows.NextPage(); err != nil {
		return nil, err
	}
	return rows, nil
}

func queryLegacyPageRows(ctx context.Context, query hubapi.NativeQueryer, statement, order string, args ...any) (*hubrecords.ChangeLegacyRows, error) {
	keys := strings.Split(order, ",")
	rows := &hubrecords.ChangeLegacyRows{Cursor: make([]any, len(keys))}
	statement = strings.Replace(statement, "SELECT ", "SELECT "+order+",", 1)
	rows.QueryPage = func(offset int) (*sql.Rows, error) {
		pageArgs := append([]any{}, args...)
		page := statement
		if offset != 0 {
			page += " AND (" + order + ")>(" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ")"
			pageArgs = append(pageArgs, rows.Cursor...)
		}
		pageArgs = append(pageArgs, hubrecords.ChangeLegacyReadLimit)
		return query.QueryContext(ctx, page+" ORDER BY "+order+" LIMIT ?", pageArgs...)
	}
	if err := rows.NextPage(); err != nil {
		return nil, err
	}
	return rows, nil
}
