package searchrecords_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"

	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedPendingIssues(t *testing.T, f searchFixture, org tracker.OrganizationID, count int) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title,actor_principal_id) SELECT $1,'wi_tick_'||n,'prj_first',n+10,'tickterm','example' FROM generate_series(1,$2::int) n`, org, count); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= count; index++ {
		f.put(t, searchIssue(org, fmt.Sprintf("wi_tick_%d", index)), searchstore.Text{Title: fmt.Sprintf("tickterm %d", index)}, 0)
	}
}

func pendingSearchDocuments(t *testing.T, f searchFixture, org tracker.OrganizationID) int {
	t.Helper()
	var pending int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents WHERE organization_id=$1 AND kind='issue' AND indexed_revision<>revision`, org).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	return pending
}

type publicationCounter struct {
	searchstore.Repository
	mu        sync.Mutex
	published map[string]int
}

type countingSearchWriter struct {
	searchstore.Writer
	counter *publicationCounter
}

func withoutDeadline(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

func countPublications(repository searchstore.Repository) *publicationCounter {
	return &publicationCounter{Repository: repository, published: map[string]int{}}
}

func (c *publicationCounter) WriteSearch(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(searchstore.Writer) error) error {
	return c.Repository.WriteSearch(ctx, org, workload, func(w searchstore.Writer) error { return write(countingSearchWriter{w, c}) })
}

func (w countingSearchWriter) PublishSearch(ctx context.Context, value searchstore.Prepared) (bool, error) {
	published, err := w.Writer.PublishSearch(ctx, value)
	if published && err == nil {
		w.counter.mu.Lock()
		w.counter.published[value.Document.ID]++
		w.counter.mu.Unlock()
	}
	return published, err
}

func (c *publicationCounter) requireEachOnce(t *testing.T, prefix string, count int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for id, published := range c.published {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		if published != 1 {
			t.Fatalf("document %s published %d times", id, published)
		}
		total++
	}
	if total != count {
		t.Fatalf("published documents=%d want %d", total, count)
	}
}

func TestSearchRefreshTickRebuildsBacklogInBudgetedTicks(t *testing.T) {
	for _, test := range []struct {
		name    string
		rebuild bool
	}{{"pending publications", false}, {"project rebuild", true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newSearchFixture(t)
			const documents = 2*searchstore.RefreshTickDocuments + 50
			seedPendingIssues(t, f, "org_first", documents)
			if test.rebuild {
				f.drain(t, "org_first")
				if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id) VALUES('org_first','prj_first','',false,'')`); err != nil {
					t.Fatal(err)
				}
			}
			counter := countPublications(f.repository)
			ticks := 0
			for {
				processed, err := f.content.RefreshTick(t.Context(), counter, "org_first", withoutDeadline)
				if err != nil {
					t.Fatal(err)
				}
				if processed > searchstore.RefreshTickDocuments {
					t.Fatalf("tick processed %d documents, budget %d", processed, searchstore.RefreshTickDocuments)
				}
				if processed == 0 {
					break
				}
				ticks++
				if ticks > documents {
					t.Fatal("refresh did not converge")
				}
			}
			want := (documents + searchstore.RefreshTickDocuments - 1) / searchstore.RefreshTickDocuments
			if ticks != want || pendingSearchDocuments(t, f, "org_first") != 0 {
				t.Fatalf("ticks=%d want %d pending=%d", ticks, want, pendingSearchDocuments(t, f, "org_first"))
			}
			counter.requireEachOnce(t, "issue/wi_tick_", documents)
			var refreshing int
			if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_scope_refresh WHERE organization_id='org_first'`).Scan(&refreshing); err != nil || refreshing != 0 {
				t.Fatalf("scope refresh rows=%d err=%v", refreshing, err)
			}
		})
	}
}

func TestSearchRefreshTickDeadlineCancelsBlockedOperation(t *testing.T) {
	f := newSearchFixture(t)
	seedPendingIssues(t, f, "org_first", 3)
	barrier := &searchReadBarrier{DetailStore: f.objects, read: make(chan struct{}), release: make(chan struct{})}
	content := searchstore.NewContent(barrier)
	counter := countPublications(f.repository)
	var expire context.CancelFunc
	deadline := func(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
		if budget != searchstore.RefreshTickDuration {
			t.Errorf("tick budget=%s want %s", budget, searchstore.RefreshTickDuration)
		}
		ctx, cancel := context.WithCancel(parent)
		expire = cancel
		return ctx, cancel
	}
	go func() {
		<-barrier.read
		expire()
	}()
	processed, err := content.RefreshTick(t.Context(), counter, "org_first", deadline)
	if err != nil || processed != 0 || pendingSearchDocuments(t, f, "org_first") != 3 {
		t.Fatalf("expired tick processed=%d pending=%d err=%v", processed, pendingSearchDocuments(t, f, "org_first"), err)
	}
	counter.requireEachOnce(t, "issue/wi_tick_", 0)
	close(barrier.release)
	processed, err = content.RefreshTick(t.Context(), counter, "org_first", withoutDeadline)
	if err != nil || processed != 3 || pendingSearchDocuments(t, f, "org_first") != 0 {
		t.Fatalf("next tick processed=%d pending=%d err=%v", processed, pendingSearchDocuments(t, f, "org_first"), err)
	}
	counter.requireEachOnce(t, "issue/wi_tick_", 3)
}

func TestSearchRefreshTickProgressesEachOrganization(t *testing.T) {
	f := newSearchFixture(t)
	const documents = searchstore.RefreshTickDocuments + 50
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedPendingIssues(t, f, org, documents)
	}
	for tick := 1; tick <= 2; tick++ {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			other := map[tracker.OrganizationID]tracker.OrganizationID{"org_first": "org_second", "org_second": "org_first"}[org]
			before := pendingSearchDocuments(t, f, other)
			if _, err := f.content.RefreshTick(t.Context(), f.repository, org, withoutDeadline); err != nil {
				t.Fatal(err)
			}
			if after := pendingSearchDocuments(t, f, other); after != before {
				t.Fatalf("tick for %s changed %s pending from %d to %d", org, other, before, after)
			}
			want := max(0, documents-tick*searchstore.RefreshTickDocuments)
			if got := pendingSearchDocuments(t, f, org); got != want {
				t.Fatalf("%s pending after tick %d=%d want %d", org, tick, got, want)
			}
		}
	}
}

func TestSearchRefreshTickCancellationKeepsRemainingDocuments(t *testing.T) {
	f := newSearchFixture(t)
	const documents = 50
	seedPendingIssues(t, f, "org_first", documents)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	counter := countPublications(f.repository)
	stopping := &cancelAfterPublications{publicationCounter: counter, after: 5, cancel: cancel}
	processed, err := f.content.RefreshTick(ctx, stopping, "org_first", withoutDeadline)
	if !errors.Is(err, context.Canceled) || processed != 5 || pendingSearchDocuments(t, f, "org_first") != documents-processed {
		t.Fatalf("processed=%d pending=%d err=%v", processed, pendingSearchDocuments(t, f, "org_first"), err)
	}
	processed, err = f.content.RefreshTick(t.Context(), counter, "org_first", withoutDeadline)
	if err != nil || processed != documents-5 || pendingSearchDocuments(t, f, "org_first") != 0 {
		t.Fatalf("resumed processed=%d pending=%d err=%v", processed, pendingSearchDocuments(t, f, "org_first"), err)
	}
	counter.requireEachOnce(t, "issue/wi_tick_", documents)
}

type cancelAfterPublications struct {
	*publicationCounter
	after  int
	cancel context.CancelFunc
}

func (c *cancelAfterPublications) WriteSearch(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(searchstore.Writer) error) error {
	err := c.publicationCounter.WriteSearch(ctx, org, workload, write)
	c.mu.Lock()
	total := 0
	for _, published := range c.published {
		total += published
	}
	c.mu.Unlock()
	if total == c.after {
		c.cancel()
	}
	return err
}
