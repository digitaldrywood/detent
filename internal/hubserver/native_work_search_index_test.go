package hubserver

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchRefreshRunsAtJitteredOffset(t *testing.T) {
	offsets := map[float64]time.Duration{}
	for _, jitter := range []float64{0.2, 0.7} {
		service := &Service{config: Config{WebhookMaintenanceInterval: time.Minute, deps: hubdeps.Deps{
			Jitter: func() float64 { return jitter },
			NewMaintenanceTimer: func(delay time.Duration) hubdeps.Timer {
				offsets[jitter] = delay
				return hubdeps.Timer{Reset: func(time.Duration) {}, Stop: func() {}}
			},
		}}}
		service.searchRefreshTimer()
	}
	if offsets[0.2] == offsets[0.7] || offsets[0.2] < 10*time.Second || offsets[0.7] < 40*time.Second || offsets[0.7] >= time.Minute {
		t.Fatalf("search refresh offsets=%v", offsets)
	}
	if searchstore.RefreshTickDocuments != 100 || searchstore.RefreshTickDuration != 2*time.Second {
		t.Fatalf("per-tick budget changed: documents=%d duration=%s", searchstore.RefreshTickDocuments, searchstore.RefreshTickDuration)
	}
}

func TestSearchMaintenanceRefreshesSearchAfterIdentityBudgetExpires(t *testing.T) {
	t.Parallel()
	objects := &memoryRunDetails{}
	var budgetsMu sync.Mutex
	var budgets []context.CancelFunc
	service := newPostgresNativeService(t, Config{RunDetails: objects, deps: hubdeps.Deps{SearchRefreshTimeout: func(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
		if budget != searchstore.RefreshTickDuration {
			t.Errorf("tick budget=%s want %s", budget, searchstore.RefreshTickDuration)
		}
		ctx, cancel := context.WithCancel(parent)
		budgetsMu.Lock()
		budgets = append(budgets, cancel)
		budgetsMu.Unlock()
		return ctx, cancel
	}}})
	organizations := []tracker.OrganizationID{"org_first", "org_first_second"}
	var admin *sql.DB
	for _, org := range organizations {
		f := newNativeFixture(t, service, org, "Example project")
		f.create(t, "identity refresh blocked")
		admin = nativeAttemptTestDatabase(t, f)
		if _, err := admin.ExecContext(t.Context(), "UPDATE issues SET doctor_identity_revision=NULL WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(org tracker.OrganizationID) int {
		t.Helper()
		var count int
		if err := admin.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM search_documents WHERE organization_id=$1 AND indexed_revision<>revision)+(SELECT count(*) FROM search_scope_refresh WHERE organization_id=$1)", org).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	initial := map[tracker.OrganizationID]int{}
	for _, org := range organizations {
		if initial[org] = pending(org); initial[org] == 0 {
			t.Fatalf("%s fixture has no pending search work", org)
		}
	}
	parent := t.Context()
	if end, ok := t.Deadline(); ok {
		guarded, cancel := context.WithDeadline(parent, end.Add(-time.Until(end)/10))
		t.Cleanup(cancel)
		parent = guarded
	}
	original := service.localOrganization
	t.Cleanup(func() { service.localOrganization = original })
	for index, org := range organizations {
		budgetsMu.Lock()
		budgets = nil
		budgetsMu.Unlock()
		read := make(chan struct{})
		var readOnce sync.Once
		stop, helperDone := make(chan struct{}), make(chan struct{})
		objects.beforeRead = func(ctx context.Context) error {
			first := false
			readOnce.Do(func() {
				first = true
				close(read)
			})
			if !first {
				return nil
			}
			<-ctx.Done()
			return ctx.Err()
		}
		go func() {
			defer close(helperDone)
			select {
			case <-read:
			case <-stop:
				return
			}
			budgetsMu.Lock()
			expire := budgets[0]
			budgetsMu.Unlock()
			expire()
		}()
		service.localOrganization = string(org)
		service.maintainNativeSearch(parent)
		close(stop)
		<-helperDone
		objects.beforeRead = nil
		select {
		case <-read:
		default:
			t.Fatalf("%s identity refresh did not reach its object read", org)
		}
		if parent.Err() != nil {
			t.Fatalf("%s blocked identity refresh outlived its budget", org)
		}
		if got := pending(org); got != 0 {
			t.Errorf("%s search refresh left %d pending entries after the identity budget expired", org, got)
		}
		for _, later := range organizations[index+1:] {
			if got := pending(later); got != initial[later] {
				t.Errorf("%s maintenance changed %s pending search work from %d to %d", org, later, initial[later], got)
			}
		}
		if more, err := service.refreshNativeIdentity(t.Context()); err != nil || !more {
			t.Fatalf("%s identity refresh after expired budget more=%v err=%v", org, more, err)
		}
	}
}
