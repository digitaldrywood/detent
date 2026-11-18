package diagnostics_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type heldSnapshot struct {
	diagnosticstore.Reader
	visibility string
	release    chan struct{}
	mu         sync.Mutex
}

func (r *heldSnapshot) DiagnosticsReadVersion(context.Context) (string, error) {
	return r.visibility, nil
}

func (r *heldSnapshot) DiagnosticsSnapshot(ctx context.Context, projects []tracker.ProjectID, from, to, observed time.Time, principal string) (diagnosticstore.Snapshot, error) {
	<-r.release
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Reader.DiagnosticsSnapshot(ctx, projects, from, to, observed, principal)
}

func TestDiagnosticsWindowReads(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	fixtures := tenants(t)
	for _, f := range fixtures {
		seedItem(t, f, project, tracker.NativeWorkItemID(f.organization), "Todo", now.Add(-time.Hour))
	}
	var windows diagnosticstore.Windows
	for _, f := range fixtures {
		t.Run(string(f.organization), func(t *testing.T) {
			err := f.repository.ReadTenantSnapshot(t.Context(), f.organization, corestore.Serving, func(reader tenantstore.Reader) error {
				visibility, err := reader.(interface {
					DiagnosticsReadVersion(context.Context) (string, error)
				}).DiagnosticsReadVersion(t.Context())
				if err != nil {
					return err
				}
				if visibility == "" {
					t.Fatal("missing repeatable-read visibility")
				}
				for _, tc := range []struct {
					name    string
					overlap bool
					want    int32
				}{{"sequential", false, 2}, {"overlap", true, 1}} {
					t.Run(tc.name, func(t *testing.T) {
						synctest.Test(t, func(t *testing.T) {
							var leases atomic.Int32
							ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
								if strings.Contains(query, "WITH RECURSIVE lease_machines") {
									leases.Add(1)
								}
							})
							r := &heldSnapshot{Reader: reader.(diagnosticstore.Reader), visibility: visibility, release: make(chan struct{})}
							release := sync.OnceFunc(func() { close(r.release) })
							defer release()
							read := func() {
								got, err := windows.Read(ctx, r, f.organization, 1, []tracker.ProjectID{project}, diagnosticstore.ReportingWindow{From: now.Add(-24 * time.Hour), To: now, Observed: now}, "")
								if err != nil {
									t.Error(err)
									return
								}
								if len(got.Issues) != 1 || got.Issues[0].WorkItemID != tracker.NativeWorkItemID(f.organization) || got.Issues[0].State != "Todo" {
									t.Errorf("scoped issues = %+v", got.Issues)
								}
							}
							if tc.overlap {
								go read()
								go read()
								synctest.Wait()
								release()
								synctest.Wait()
							} else {
								release()
								read()
								read()
							}
							if got := leases.Load(); got != tc.want {
								t.Errorf("two %s calls executed %d lease-history reads, want %d", tc.name, got, tc.want)
							}
						})
					})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
