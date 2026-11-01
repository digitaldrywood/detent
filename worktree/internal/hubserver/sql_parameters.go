package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const maxSQLParameters = 30000

type sqlParameterRows struct {
	columns []string
	rows    [][]any
}

func sqlList[T ~string | ~int64 | ~int](values []T) (sqlParameterRows, error) {
	if len(values) > maxSQLParameters {
		return sqlParameterRows{}, errors.New("SQL parameter relation exceeds bound")
	}
	rows := make([][]any, len(values))
	for i, value := range values {
		rows[i] = []any{value}
	}
	return sqlTypedRows([]string{"value"}, rows)
}

func sqlTypedRows(columns []string, rows [][]any) (sqlParameterRows, error) {
	if len(columns) == 0 || len(rows) > maxSQLParameters/len(columns) {
		return sqlParameterRows{}, errors.New("SQL parameter relation exceeds bound")
	}
	for _, row := range rows {
		if len(row) != len(columns) {
			return sqlParameterRows{}, errors.New("SQL parameter relation width mismatch")
		}
	}
	return sqlParameterRows{columns: columns, rows: rows}, nil
}

func bindSQLParameters(statement string, args []any) (string, []any, error) {
	var out strings.Builder
	var values []any
	next := 0
	for i := 0; i < len(statement); {
		start := i
		relation := strings.HasPrefix(statement[i:], "sql_rows(?)")
		if relation {
			i += len("sql_rows(")
		}
		switch statement[i] {
		case '\'', '"', '`', '[':
			end := statement[i]
			if end == '[' {
				end = ']'
			}
			i++
			for i < len(statement) {
				if statement[i] == end {
					i++
					if i < len(statement) && statement[i] == end && end != ']' {
						i++
						continue
					}
					break
				}
				i++
			}
		case '-':
			if i+1 < len(statement) && statement[i+1] == '-' {
				for i < len(statement) && statement[i] != '\n' {
					i++
				}
			} else {
				i++
			}
		case '/':
			if i+1 < len(statement) && statement[i+1] == '*' {
				i += 2
				for i+1 < len(statement) && statement[i:i+2] != "*/" {
					i++
				}
				i = min(len(statement), i+2)
			} else {
				i++
			}
		case '?':
			i++
			digits := i
			for i < len(statement) && statement[i] >= '0' && statement[i] <= '9' {
				i++
			}
			index := next
			if i > digits {
				number, err := strconv.Atoi(statement[digits:i])
				if err != nil || number < 1 {
					return "", nil, errors.New("invalid SQL parameter index")
				}
				index = number - 1
			}
			if relation {
				i++
			}
			next = max(next, index+1)
			if index >= len(args) {
				return "", nil, errors.New("missing SQL parameter")
			}
			replacement, updated, err := appendSQLParameter(args[index], values)
			if err != nil {
				return "", nil, err
			}
			values = updated
			out.WriteString(replacement)
			continue
		default:
			i++
		}
		out.WriteString(statement[start:i])
	}
	if next != len(args) {
		return "", nil, errors.New("unused SQL parameters")
	}
	return out.String(), values, nil
}

func appendSQLParameter(value any, args []any) (string, []any, error) {
	rows, relation := value.(sqlParameterRows)
	if !relation {
		if len(args) == maxSQLParameters {
			return "", nil, errors.New("SQL parameters exceed bound")
		}
		args = append(args, value)
		return "?", args, nil
	}
	if len(rows.rows)*len(rows.columns) > maxSQLParameters-len(args) {
		return "", nil, errors.New("SQL parameters exceed bound")
	}
	var selectColumns []string
	selectColumns = append(selectColumns, "column1 AS key")
	for i, column := range rows.columns {
		selectColumns = append(selectColumns, "column"+strconv.Itoa(i+2)+` AS "`+strings.ReplaceAll(column, `"`, `""`)+`"`)
	}
	if len(rows.rows) == 0 {
		empty := []string{"NULL AS key"}
		for _, column := range rows.columns {
			empty = append(empty, `NULL AS "`+strings.ReplaceAll(column, `"`, `""`)+`"`)
		}
		return "(SELECT " + strings.Join(empty, ",") + " WHERE 0)", args, nil
	}
	var records []string
	for i, row := range rows.rows {
		cells := []string{strconv.Itoa(i)}
		for _, value := range row {
			args = append(args, value)
			cells = append(cells, "?")
		}
		records = append(records, "("+strings.Join(cells, ",")+")")
	}
	return "(SELECT " + strings.Join(selectColumns, ",") + " FROM (VALUES " + strings.Join(records, ",") + "))", args, nil
}

func querySQL(ctx context.Context, query nativeQueryer, statement string, args ...any) (*sql.Rows, error) {
	statement, args, err := bindSQLParameters(statement, args)
	if err != nil {
		return nil, fmt.Errorf("bind SQL parameters: %w", err)
	}
	return query.QueryContext(ctx, statement, args...)
}

type sqlParameterRow struct {
	row *sql.Row
	err error
}

func (r sqlParameterRow) Scan(values ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(values...)
}

func queryRowSQL(ctx context.Context, query nativeQueryer, statement string, args ...any) sqlParameterRow {
	statement, args, err := bindSQLParameters(statement, args)
	if err != nil {
		return sqlParameterRow{err: fmt.Errorf("bind SQL parameters: %w", err)}
	}
	return sqlParameterRow{row: query.QueryRowContext(ctx, statement, args...)}
}

func execSQL(ctx context.Context, query nativeExecer, statement string, args ...any) (sql.Result, error) {
	statement, args, err := bindSQLParameters(statement, args)
	if err != nil {
		return nil, fmt.Errorf("bind SQL parameters: %w", err)
	}
	return query.ExecContext(ctx, statement, args...)
}
