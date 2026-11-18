package hubapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func ValidateNativeQuery(params url.Values, fields ...string) error {
	fields = append(fields, "archive_url")
	if value, present := params["archive_url"]; present && (len(value) != 1 || value[0] != "true" && value[0] != "false") {
		return NativePublicInvalid("archive_url supports true or false")
	}
	allowed := map[string]bool{"limit": true, "cursor": true}
	for _, field := range fields {
		allowed[field] = true
	}
	for key, values := range params {
		if !allowed[key] || len(values) != 1 || len(values[0]) > 4096 {
			return NativePublicInvalid("Query contains an unsupported field or value")
		}
	}
	return nil
}

func NativeWorkflowInvalid(message string) error {
	return &NativeError{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, PublicMessage: true}
}

type NativeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

const MaxSQLParameters = 30000

type SQLParameterRows struct {
	columns []string
	rows    [][]any
}

func SQLList[T ~string | ~int64 | ~int](values []T) (SQLParameterRows, error) {
	if len(values) > MaxSQLParameters {
		return SQLParameterRows{}, errors.New("SQL parameter relation exceeds bound")
	}
	rows := make([][]any, len(values))
	for i, value := range values {
		rows[i] = []any{value}
	}
	return SQLTypedRows([]string{"value"}, rows)
}

func SQLTypedRows(columns []string, rows [][]any) (SQLParameterRows, error) {
	if len(columns) == 0 || len(rows) > MaxSQLParameters/len(columns) {
		return SQLParameterRows{}, errors.New("SQL parameter relation exceeds bound")
	}
	for _, row := range rows {
		if len(row) != len(columns) {
			return SQLParameterRows{}, errors.New("SQL parameter relation width mismatch")
		}
	}
	return SQLParameterRows{columns: columns, rows: rows}, nil
}

func BindSQLParameters(statement string, args []any) (string, []any, error) {
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
			replacement, updated, err := AppendSQLParameter(args[index], values)
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

func AppendSQLParameter(value any, args []any) (string, []any, error) {
	rows, relation := value.(SQLParameterRows)
	if !relation {
		if len(args) == MaxSQLParameters {
			return "", nil, errors.New("SQL parameters exceed bound")
		}
		args = append(args, value)
		return "?", args, nil
	}
	if len(rows.rows)*len(rows.columns) > MaxSQLParameters-len(args) {
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

func QuerySQL(ctx context.Context, query NativeQueryer, statement string, args ...any) (*sql.Rows, error) {
	statement, args, err := BindSQLParameters(statement, args)
	if err != nil {
		return nil, fmt.Errorf("bind SQL parameters: %w", err)
	}
	return query.QueryContext(ctx, statement, args...)
}
