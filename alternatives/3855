package requesttiming

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
)

type Timing struct {
	Start             time.Time
	DB                atomic.Int64
	Encode            atomic.Int64
	Trace             func(query string, args []driver.NamedValue)
	WriterTrace       func(query string, args []driver.NamedValue)
	WriterTransaction func(active bool)
	mu                sync.Mutex
	spans             []Span
}

type Span struct {
	Name     string
	Duration time.Duration
}

type requestTimingKey struct{}

func With(ctx context.Context, timing *Timing) context.Context {
	return context.WithValue(ctx, requestTimingKey{}, timing)
}

func From(ctx context.Context) *Timing {
	if timing, ok := ctx.Value(requestTimingKey{}).(*Timing); ok {
		return timing
	}
	return nil
}

func (t *Timing) AddDB(start time.Time) {
	if t != nil {
		t.DB.Add(int64(time.Since(start)))
	}
}

func (t *Timing) Statement(start time.Time, query string, args []driver.NamedValue) {
	if t == nil {
		return
	}
	t.AddDB(start)
	if t.Trace != nil {
		t.Trace(query, args)
	}
}

func (t *Timing) AddSpan(name string, start time.Time) {
	if t == nil {
		return
	}
	elapsed := time.Since(start)
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.spans {
		if t.spans[i].Name == name {
			t.spans[i].Duration += elapsed
			return
		}
	}
	t.spans = append(t.spans, Span{name, elapsed})
}

func TimeSpan(ctx context.Context, name string) func() {
	timing, start := From(ctx), time.Now()
	return func() { timing.AddSpan(name, start) }
}

func (t *Timing) NamedSpans() []Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Span(nil), t.spans...)
}

func (t *Timing) LogAttrs() []any {
	if t == nil {
		return nil
	}
	attrs := []any{"db_ms", time.Duration(t.DB.Load()).Milliseconds()}
	spans := t.NamedSpans()
	if len(spans) == 0 {
		return attrs
	}
	group := make([]any, 0, len(spans))
	for _, span := range spans {
		group = append(group, slog.Int64(span.Name, span.Duration.Milliseconds()))
	}
	return append(attrs, slog.Group("spans_ms", group...))
}

func (t *Timing) Header(now time.Time) string {
	total := max(0, now.Sub(t.Start))
	db := min(time.Duration(t.DB.Load()), total)
	encode := min(time.Duration(t.Encode.Load()), total-db)
	var header strings.Builder
	fmt.Fprintf(&header, "db;dur=%.1f, compute;dur=%.1f, encode;dur=%.1f, total;dur=%.1f", milliseconds(db), milliseconds(total-db-encode), milliseconds(encode), milliseconds(total))
	for _, span := range t.NamedSpans() {
		fmt.Fprintf(&header, ", %s;dur=%.1f", span.Name, milliseconds(span.Duration))
	}
	return header.String()
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

func Server(authenticated func(echo.Context) bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !strings.HasPrefix(c.Request().URL.Path, "/api/v2/") {
				return next(c)
			}
			timing := From(c.Request().Context())
			if timing == nil {
				timing = &Timing{}
				c.SetRequest(c.Request().WithContext(With(c.Request().Context(), timing)))
			}
			timing.Start = time.Now()
			c.Response().Before(func() {
				if authenticated(c) {
					c.Response().Header().Set("Server-Timing", timing.Header(time.Now()))
				}
			})
			return next(c)
		}
	}

}

type JSONSerializer struct {
	echo.DefaultJSONSerializer
}

func (JSONSerializer) Serialize(c echo.Context, i any, indent string) error {
	start := time.Now()
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err := encoder.Encode(i); err != nil {
		return fmt.Errorf("encode json response: %w", err)
	}
	if timing := From(c.Request().Context()); timing != nil {
		timing.Encode.Add(int64(time.Since(start)))
	}
	_, err := c.Response().Write(body.Bytes())
	return err
}
