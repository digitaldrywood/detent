package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
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
		if budget != searchstore.RefreshTickDuration && budget != searchstore.RefreshStepDuration {
			t.Errorf("refresh budget=%s want %s or %s", budget, searchstore.RefreshTickDuration, searchstore.RefreshStepDuration)
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

type expiringBudget struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func (b *expiringBudget) expire() { b.once.Do(func() { close(b.done) }) }

func (b *expiringBudget) Done() <-chan struct{} { return b.done }

func (b *expiringBudget) Err() error {
	select {
	case <-b.done:
		if err := b.Context.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestSearchMaintenanceLogsExpiredRefreshStepOnce(t *testing.T) {
	t.Parallel()
	objects := &memoryRunDetails{}
	var logs bytes.Buffer
	var logsMu sync.Mutex
	var budgetsMu sync.Mutex
	var budgets []*expiringBudget
	service := newPostgresNativeService(t, Config{RunDetails: objects, Logger: slog.New(slog.NewJSONHandler(lockedWriter{&logs, &logsMu}, nil)), deps: hubdeps.Deps{SearchRefreshTimeout: func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		budget := &expiringBudget{Context: parent, done: make(chan struct{})}
		stop := context.AfterFunc(parent, budget.expire)
		budgetsMu.Lock()
		budgets = append(budgets, budget)
		budgetsMu.Unlock()
		return budget, func() {
			stop()
			budget.expire()
		}
	}}})
	const title = "customer sentinel title"
	organizations := []tracker.OrganizationID{"org_first", "org_first_second"}
	var admin *sql.DB
	for _, org := range organizations {
		f := newNativeFixture(t, service, org, "Example project")
		f.create(t, title)
		admin = nativeAttemptTestDatabase(t, f)
		if _, err := admin.ExecContext(t.Context(), "UPDATE search_documents SET indexed_revision=0 WHERE organization_id=$1 AND kind='issue'", org); err != nil {
			t.Fatal(err)
		}
	}
	stale := func(org tracker.OrganizationID) int {
		t.Helper()
		var count int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM search_documents WHERE organization_id=$1 AND kind='issue' AND indexed_revision<>revision", org).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	for _, org := range organizations {
		if stale(org) == 0 {
			t.Fatalf("%s fixture has no stale search document", org)
		}
	}
	original := service.localOrganization
	t.Cleanup(func() { service.localOrganization = original })
	for index, org := range organizations {
		before := stale(org)
		logsMu.Lock()
		logs.Reset()
		logsMu.Unlock()
		objects.beforeRead = func(ctx context.Context) error {
			budgetsMu.Lock()
			for _, budget := range budgets {
				budget.expire()
			}
			budgetsMu.Unlock()
			return ctx.Err()
		}
		service.localOrganization = string(org)
		service.maintainNativeSearch(t.Context())
		objects.beforeRead = nil
		logsMu.Lock()
		output := logs.String()
		logsMu.Unlock()
		if got := strings.Count(output, "rebuild scoped search index"); got != 1 || !strings.Contains(output, "deadline exceeded") {
			t.Fatalf("%s expired refresh step logged %d times, want once with its deadline: %s", org, got, output)
		}
		if strings.Contains(output, title) {
			t.Fatalf("%s refresh log contains customer content: %s", org, output)
		}
		if got := stale(org); got != before {
			t.Fatalf("%s stale documents=%d after expired step, want %d", org, got, before)
		}
		for _, later := range organizations[index+1:] {
			if got := stale(later); got == 0 {
				t.Fatalf("%s maintenance refreshed %s", org, later)
			}
		}
		service.maintainNativeSearch(t.Context())
		if got := stale(org); got != 0 {
			t.Fatalf("%s stale documents=%d after the next tick, want 0", org, got)
		}
	}
}

type lockedWriter struct {
	buffer *bytes.Buffer
	mu     *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(p)
}
