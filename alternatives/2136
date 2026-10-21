package hubserver

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
)

// requestTiming splits one API request's wall time into the spans reported
// in its Server-Timing header. Database statements and JSON encoding record
// their own spans; compute is the remainder of the total. Named spans time
// the parts of one read, such as each Reports sub-read, by wall clock.
type requestTiming struct {
	start  time.Time
	db     atomic.Int64
	encode atomic.Int64
	trace  func(query string, args []driver.NamedValue)
	mu     sync.Mutex
	spans  []timingSpan
}

type timingSpan struct {
	name     string
	duration time.Duration
}

type requestTimingKey struct{}

func withRequestTiming(ctx context.Context, timing *requestTiming) context.Context {
	return context.WithValue(ctx, requestTimingKey{}, timing)
}

func requestTimingFrom(ctx context.Context) *requestTiming {
	if timing, ok := ctx.Value(requestTimingKey{}).(*requestTiming); ok {
		return timing
	}
	return nil
}

func (t *requestTiming) addDB(start time.Time) {
	if t != nil {
		t.db.Add(int64(time.Since(start)))
	}
}

func (t *requestTiming) statement(start time.Time, query string, args []driver.NamedValue) {
	if t == nil {
		return
	}
	t.addDB(start)
	if t.trace != nil {
		t.trace(query, args)
	}
}

// span adds the wall time since start to the named span. Concurrent parts
// of one request, such as the projects of an organization report, add up.
func (t *requestTiming) span(name string, start time.Time) {
	if t == nil {
		return
	}
	elapsed := time.Since(start)
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.spans {
		if t.spans[i].name == name {
			t.spans[i].duration += elapsed
			return
		}
	}
	t.spans = append(t.spans, timingSpan{name, elapsed})
}

// timeSpan times the named part of the request in ctx until the returned
// function runs.
func timeSpan(ctx context.Context, name string) func() {
	timing, start := requestTimingFrom(ctx), time.Now()
	return func() { timing.span(name, start) }
}

func (t *requestTiming) namedSpans() []timingSpan {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]timingSpan(nil), t.spans...)
}

// logAttrs are the request's db time and named spans in whole milliseconds.
func (t *requestTiming) logAttrs() []any {
	if t == nil {
		return nil
	}
	attrs := []any{"db_ms", time.Duration(t.db.Load()).Milliseconds()}
	spans := t.namedSpans()
	if len(spans) == 0 {
		return attrs
	}
	group := make([]any, 0, len(spans))
	for _, span := range spans {
		group = append(group, slog.Int64(span.name, span.duration.Milliseconds()))
	}
	return append(attrs, slog.Group("spans_ms", group...))
}

func (t *requestTiming) header(now time.Time) string {
	total := max(0, now.Sub(t.start))
	db := min(time.Duration(t.db.Load()), total)
	encode := min(time.Duration(t.encode.Load()), total-db)
	var header strings.Builder
	fmt.Fprintf(&header, "db;dur=%.1f, compute;dur=%.1f, encode;dur=%.1f, total;dur=%.1f", milliseconds(db), milliseconds(total-db-encode), milliseconds(encode), milliseconds(total))
	for _, span := range t.namedSpans() {
		fmt.Fprintf(&header, ", %s;dur=%.1f", span.name, milliseconds(span.duration))
	}
	return header.String()
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// serverTiming adds a Server-Timing header to /api/v2 responses whose caller
// authenticated. Unauthenticated responses never reveal timing.
func (s *Service) serverTiming(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !strings.HasPrefix(c.Request().URL.Path, "/api/v2/") {
			return next(c)
		}
		timing := requestTimingFrom(c.Request().Context())
		if timing == nil {
			timing = &requestTiming{}
			c.SetRequest(c.Request().WithContext(withRequestTiming(c.Request().Context(), timing)))
		}
		timing.start = time.Now()
		c.Response().Before(func() {
			if _, ok := c.Get("hub_api_credential").(apiCredential); ok {
				c.Response().Header().Set("Server-Timing", timing.header(time.Now()))
			}
		})
		return next(c)
	}
}

// timedJSONSerializer encodes a response before writing it so the encode span
// is known when the header is written.
type timedJSONSerializer struct {
	echo.DefaultJSONSerializer
}

func (timedJSONSerializer) Serialize(c echo.Context, i any, indent string) error {
	start := time.Now()
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err := encoder.Encode(i); err != nil {
		return fmt.Errorf("encode json response: %w", err)
	}
	if timing := requestTimingFrom(c.Request().Context()); timing != nil {
		timing.encode.Add(int64(time.Since(start)))
	}
	_, err := c.Response().Write(body.Bytes())
	return err
}

// timedRows counts the time spent stepping a result set toward the request's
// db span, since SQLite does most of a query's work while rows are read.
type timedRows struct {
	driver.Rows
	timing *requestTiming
}

func (r *timedRows) Next(dest []driver.Value) error {
	start := time.Now()
	err := r.Rows.Next(dest)
	r.timing.addDB(start)
	return err
}

func (r *timedRows) Close() error {
	start := time.Now()
	err := r.Rows.Close()
	r.timing.addDB(start)
	return err
}

func (r *timedRows) ColumnTypeDatabaseTypeName(index int) string {
	if typed, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return typed.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *timedRows) ColumnTypeLength(index int) (int64, bool) {
	if typed, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return typed.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *timedRows) ColumnTypeNullable(index int) (bool, bool) {
	if typed, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return typed.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *timedRows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if typed, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return typed.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}

func (r *timedRows) ColumnTypeScanType(index int) reflect.Type {
	if typed, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return typed.ColumnTypeScanType(index)
	}
	return reflect.TypeFor[any]()
}
