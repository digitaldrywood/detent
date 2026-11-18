package sqlstmt

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
)

const statementSlots = 64

type Cache struct {
	statements [statementSlots]statement
	next       int
}

type statement struct {
	query string
	value driver.Stmt
	busy  bool
}

func cacheable(query string) bool {
	query = strings.TrimSpace(query)
	if strings.Contains(query, ";") {
		return false
	}
	end := strings.IndexAny(query, " \t\r\n(")
	if end >= 0 {
		query = query[:end]
	}
	return strings.EqualFold(query, "SELECT") || strings.EqualFold(query, "WITH") || strings.EqualFold(query, "INSERT") || strings.EqualFold(query, "UPDATE") || strings.EqualFold(query, "DELETE") || strings.EqualFold(query, "REPLACE")
}

func (c *Cache) prepare(ctx context.Context, conn driver.Conn, query string) (*statement, error) {
	if !cacheable(query) {
		return nil, driver.ErrSkip
	}
	for i := range c.statements {
		prepared := &c.statements[i]
		if prepared.value != nil && prepared.query == query {
			if prepared.busy {
				return nil, driver.ErrSkip
			}
			return prepared, nil
		}
	}
	for offset := range c.statements {
		index := (c.next + offset) % len(c.statements)
		prepared := &c.statements[index]
		if prepared.busy {
			continue
		}
		if prepared.value != nil {
			if err := prepared.value.Close(); err != nil {
				return nil, err
			}
			*prepared = statement{}
		}
		var value driver.Stmt
		var err error
		if preparer, ok := conn.(driver.ConnPrepareContext); ok {
			value, err = preparer.PrepareContext(ctx, query)
		} else {
			value, err = conn.Prepare(query)
		}
		if err != nil {
			return nil, err
		}
		*prepared = statement{query: query, value: value}
		c.next = (index + 1) % len(c.statements)
		return prepared, nil
	}
	return nil, driver.ErrSkip
}

func (c *Cache) ExecContext(ctx context.Context, conn driver.Conn, query string, args []driver.NamedValue) (driver.Result, error) {
	fallback, ok := conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	prepared, err := c.prepare(ctx, conn, query)
	if errors.Is(err, driver.ErrSkip) {
		return fallback.ExecContext(ctx, query, args)
	}
	if err != nil {
		return nil, err
	}
	if prepared != nil {
		if execer, ok := prepared.value.(driver.StmtExecContext); ok {
			return execer.ExecContext(ctx, args)
		}
	}
	return fallback.ExecContext(ctx, query, args)
}

func (c *Cache) QueryContext(ctx context.Context, conn driver.Conn, query string, args []driver.NamedValue) (driver.Rows, error) {
	fallback, ok := conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	prepared, err := c.prepare(ctx, conn, query)
	if errors.Is(err, driver.ErrSkip) {
		return fallback.QueryContext(ctx, query, args)
	}
	if err != nil {
		return nil, err
	}
	if prepared != nil {
		if queryer, ok := prepared.value.(driver.StmtQueryContext); ok {
			value, err := queryer.QueryContext(ctx, args)
			if err != nil {
				return nil, err
			}
			prepared.busy = true
			return &rows{Rows: value, statement: prepared}, nil
		}
	}
	return fallback.QueryContext(ctx, query, args)
}

func (c *Cache) Close() error {
	var err error
	for i := range c.statements {
		if value := c.statements[i].value; value != nil {
			err = errors.Join(err, value.Close())
		}
	}
	*c = Cache{}
	return err
}

type rows struct {
	driver.Rows
	statement *statement
	closed    bool
}

func (r *rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.Rows.Close()
	r.statement.busy = false
	return err
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	if value, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return value.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *rows) ColumnTypeLength(index int) (int64, bool) {
	if value, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return value.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *rows) ColumnTypeNullable(index int) (bool, bool) {
	if value, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return value.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *rows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if value, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return value.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}

func (r *rows) ColumnTypeScanType(index int) reflect.Type {
	if value, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return value.ColumnTypeScanType(index)
	}
	return reflect.TypeFor[any]()
}

func (r *rows) HasNextResultSet() bool {
	if value, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return value.HasNextResultSet()
	}
	return false
}

func (r *rows) NextResultSet() error {
	if value, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return value.NextResultSet()
	}
	return io.EOF
}
