package hubserver

import (
	"context"
	"database/sql"

	"github.com/digitaldrywood/detent/internal/hubsql"
)

const maxSQLParameters = hubsql.MaxParameters

type sqlParameterRows = hubsql.ParameterRows

func sqlList[T ~string | ~int64 | ~int](values []T) (sqlParameterRows, error) {
	return hubsql.List(values)
}
func sqlTypedRows(columns []string, rows [][]any) (sqlParameterRows, error) {
	return hubsql.TypedRows(columns, rows)
}
func bindSQLParameters(statement string, args []any) (string, []any, error) {
	return hubsql.Bind(statement, args)
}
func querySQL(ctx context.Context, q nativeQueryer, statement string, args ...any) (*sql.Rows, error) {
	return hubsql.Query(ctx, q, statement, args...)
}
func queryRowSQL(ctx context.Context, q nativeQueryer, statement string, args ...any) hubsql.Row {
	return hubsql.QueryRow(ctx, q, statement, args...)
}
func execSQL(ctx context.Context, q nativeExecer, statement string, args ...any) (sql.Result, error) {
	return hubsql.Exec(ctx, q, statement, args...)
}
