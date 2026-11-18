package requesttiming

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTimedConnectionsAttributeStatements(t *testing.T) {
	repository := hubtest.Repository(t)
	service := New(Deps{Bind: func(ctx context.Context, timing *Timing) context.Context {
		return postgres.WithTransactionTiming(ctx, timing.AddSpan, func(at time.Time, query string) { timing.Statement(at, query, nil) })
	}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), After: func() {}})
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization})
		t.Run(string(organization), func(t *testing.T) {
			readProject := corestore.Project{OrganizationID: organization, ID: "prj_timing_read", Name: "Example timing read", Profile: "native"}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProject(t.Context(), readProject)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name       string
				run        func(context.Context) error
				statements []string
			}{
				{"write statement and commit", func(ctx context.Context) error {
					return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.CreateProject(ctx, corestore.Project{OrganizationID: organization, ID: "prj_timing_write", Name: "Example timing write", Profile: "native"})
						return err
					})
				}, []string{"INSERT INTO projects", "COMMIT"}},
				{"read statement and commit", func(ctx context.Context) error {
					return repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
						got, err := r.Project(ctx, readProject.ID)
						if err != nil {
							return err
						}
						if got.OrganizationID != organization || got.ID != readProject.ID {
							return fmt.Errorf("read project = %s/%s", got.OrganizationID, got.ID)
						}
						return nil
					})
				}, []string{"FROM projects", "COMMIT"}},
				{"empty transaction attributes commit", func(ctx context.Context) error {
					return repository.WriteTenant(ctx, organization, corestore.Serving, func(tenantstore.Writer) error { return nil })
				}, []string{"COMMIT"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					var statements []string
					timing := &Timing{Trace: func(query string, _ []driver.NamedValue) {
						statements = append(statements, query)
					}}
					ctx := With(t.Context(), timing)
					request := httptest.NewRequestWithContext(ctx, "GET", "/api/v2/example", nil)
					c := echo.New().NewContext(request, httptest.NewRecorder())
					if err := service.Writer(func(c echo.Context) error {
						return test.run(c.Request().Context())
					})(c); err != nil {
						t.Fatal(err)
					}
					if len(statements) != len(test.statements) {
						t.Fatalf("statement count = %d, want %d", len(statements), len(test.statements))
					}
					for i, want := range test.statements {
						if !strings.Contains(statements[i], want) {
							t.Errorf("statement %d = %q, want %q", i, statements[i], want)
						}
					}
					var names []string
					var commit time.Duration
					for _, span := range timing.NamedSpans() {
						names = append(names, span.Name)
						if span.Name == "pg_commit" {
							commit = span.Duration
						}
					}
					if want := []string{"pg_admission", "pg_begin", "pg_callback", "pg_commit"}; !slices.Equal(names, want) {
						t.Fatalf("request spans = %v, want %v", names, want)
					}
					if commit <= 0 || timing.DB.Load() <= 0 {
						t.Fatalf("missing PostgreSQL duration: commit=%s db=%s", commit, time.Duration(timing.DB.Load()))
					}
				})
			}
		})
	}
}

func TestRequestTimingNamedSpans(t *testing.T) {
	start := time.Now()
	for _, test := range []struct {
		name   string
		spans  []Span
		header string
		logged []slog.Attr
	}{
		{"no spans", nil, "", nil},
		{"spans", []Span{{"reports_analytics", 30 * time.Millisecond}, {"reports_prior", 5 * time.Millisecond}}, ", reports_analytics;dur=30.0, reports_prior;dur=5.0",
			[]slog.Attr{slog.Group("spans_ms", slog.Int64("reports_analytics", 30), slog.Int64("reports_prior", 5))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			timing := &Timing{Start: start, spans: test.spans}
			if header := timing.Header(start.Add(time.Second)); !strings.HasSuffix(header, "total;dur=1000.0"+test.header) {
				t.Fatalf("header %q, want spans %q", header, test.header)
			}
			want := append([]any{"db_ms", int64(0)}, toAny(test.logged)...)
			if got := timing.LogAttrs(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("log attrs %v, want %v", got, want)
			}
		})
	}
	t.Run("parts of one span add up", func(t *testing.T) {
		timing := &Timing{}
		for range 2 {
			timing.AddSpan("reports_analytics", time.Now().Add(-20*time.Millisecond))
		}
		if spans := timing.NamedSpans(); len(spans) != 1 || spans[0].Duration < 40*time.Millisecond {
			t.Fatalf("spans %v, want one reports_analytics span of at least 40ms", spans)
		}
	})
}

func toAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, attr)
	}
	return out
}

func TestMain(m *testing.M) { hubtest.Main(m) }
