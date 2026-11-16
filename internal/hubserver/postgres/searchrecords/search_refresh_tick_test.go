package searchrecords_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
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
	t.Parallel()
	for _, test := range []struct {
		name    string
		rebuild bool
	}{{"pending publications", false}, {"project rebuild", true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
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

type expiringContext struct {
	context.Context
	done chan struct{}
	once sync.Once
	stop func() bool
}

func newExpiringContext(parent context.Context) *expiringContext {
	ctx := &expiringContext{Context: parent, done: make(chan struct{})}
	ctx.stop = context.AfterFunc(parent, ctx.expire)
	return ctx
}

func (c *expiringContext) expire() { c.once.Do(func() { close(c.done) }) }

func (c *expiringContext) Done() <-chan struct{} { return c.done }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		if err := c.Context.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type refreshBudgets struct {
	mu    sync.Mutex
	ticks []*expiringContext
	steps []*expiringContext
}

func (b *refreshBudgets) timeout(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	ctx := newExpiringContext(parent)
	b.mu.Lock()
	if budget == searchstore.RefreshTickDuration {
		b.ticks = append(b.ticks, ctx)
	} else {
		b.steps = append(b.steps, ctx)
	}
	b.mu.Unlock()
	return ctx, func() {
		ctx.stop()
		ctx.expire()
	}
}

func (b *refreshBudgets) latest(steps bool) *expiringContext {
	b.mu.Lock()
	defer b.mu.Unlock()
	budgets := b.ticks
	if steps {
		budgets = b.steps
	}
	if len(budgets) == 0 {
		return nil
	}
	return budgets[len(budgets)-1]
}

type hookedSearchObjects struct {
	attachment.DetailStore
	mu     sync.Mutex
	onRead func(context.Context, attachment.ArchiveReference) error
}

func (o *hookedSearchObjects) hook(read func(context.Context, attachment.ArchiveReference) error) {
	o.mu.Lock()
	o.onRead = read
	o.mu.Unlock()
}

func (o *hookedSearchObjects) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	o.mu.Lock()
	read := o.onRead
	o.mu.Unlock()
	if read != nil {
		if err := read(ctx, ref); err != nil {
			return nil, err
		}
	}
	return o.DetailStore.ReadDetail(ctx, org, ref)
}

func searchRefreshBacklog(t *testing.T, f searchFixture, org tracker.OrganizationID) (stale, pending, scopes int) {
	t.Helper()
	if err := f.admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM search_documents WHERE organization_id=$1 AND kind='issue' AND indexed_revision<>revision),
		(SELECT count(*) FROM search_postings WHERE organization_id=$1 AND term='~pending'),
		(SELECT count(*) FROM search_scope_refresh WHERE organization_id=$1)`, org).Scan(&stale, &pending, &scopes); err != nil {
		t.Fatal(err)
	}
	return stale, pending, scopes
}

func TestSearchRefreshTickBudgets(t *testing.T) {
	t.Parallel()
	const documents = 3
	for _, test := range []struct {
		name   string
		poison bool
	}{{"tick budget expires during a slow step", false}, {"step budget expires on a document that cannot finish", true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newSearchFixture(t)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				seedPendingIssues(t, f, org, documents)
				if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id,after_id) VALUES($1,'prj_first','',false,'','issue/wi_tick_1')`, org); err != nil {
					t.Fatal(err)
				}
			}
			second := [3]int{}
			second[0], second[1], second[2] = searchRefreshBacklog(t, f, "org_second")
			if second[0] != documents || second[1] == 0 || second[2] != 1 {
				t.Fatalf("org_second backlog stale=%d pending=%d scopes=%d", second[0], second[1], second[2])
			}
			objects := &hookedSearchObjects{DetailStore: f.objects}
			content := searchContent(objects)
			counter := countPublications(f.repository)
			budgets := &refreshBudgets{}
			var poisonMu sync.Mutex
			poison := ""
			if test.poison {
				objects.hook(func(ctx context.Context, ref attachment.ArchiveReference) error {
					poisonMu.Lock()
					if poison == "" {
						poison = ref.Key
					}
					hit := poison == ref.Key
					poisonMu.Unlock()
					if !hit {
						return nil
					}
					if step := budgets.latest(true); step != nil {
						step.expire()
					} else {
						t.Error("refresh step ran without its own budget")
						budgets.latest(false).expire()
					}
					return ctx.Err()
				})
			} else {
				var once sync.Once
				objects.hook(func(ctx context.Context, _ attachment.ArchiveReference) error {
					var err error
					once.Do(func() {
						budgets.latest(false).expire()
						err = ctx.Err()
					})
					return err
				})
			}
			processed, err := content.RefreshTick(t.Context(), counter, "org_first", budgets.timeout)
			if test.poison {
				if !errors.Is(err, context.DeadlineExceeded) || processed != 0 {
					t.Fatalf("expired step processed=%d err=%v, want reported deadline", processed, err)
				}
				counter.requireEachOnce(t, "issue/wi_tick_", 0)
				processed, err = content.RefreshTick(t.Context(), counter, "org_first", budgets.timeout)
				if !errors.Is(err, context.DeadlineExceeded) || processed != documents-1 {
					t.Fatalf("tick after poison processed=%d err=%v, want the other %d documents and the poison deadline", processed, err, documents-1)
				}
				counter.requireEachOnce(t, "issue/wi_tick_", documents-1)
				if stale, pending, _ := searchRefreshBacklog(t, f, "org_first"); stale != 1 || pending != 1 {
					t.Fatalf("org_first after poison stale=%d pending=%d, want only the poison document", stale, pending)
				}
			} else {
				if err != nil || processed != 1 {
					t.Fatalf("slow step processed=%d err=%v, want the started step published", processed, err)
				}
				counter.requireEachOnce(t, "issue/wi_tick_", 1)
				if stale, _, _ := searchRefreshBacklog(t, f, "org_first"); stale != documents-1 {
					t.Fatalf("org_first stale=%d after one slow step, want %d", stale, documents-1)
				}
				objects.hook(nil)
				for ticks := 0; ; ticks++ {
					processed, err := content.RefreshTick(t.Context(), counter, "org_first", withoutDeadline)
					if err != nil {
						t.Fatal(err)
					}
					if processed == 0 {
						break
					}
					if ticks > documents {
						t.Fatal("refresh did not converge")
					}
				}
				if stale, pending, scopes := searchRefreshBacklog(t, f, "org_first"); stale != 0 || pending != 0 || scopes != 0 {
					t.Fatalf("org_first not ready stale=%d pending=%d scopes=%d", stale, pending, scopes)
				}
			}
			if stale, pending, scopes := searchRefreshBacklog(t, f, "org_second"); [3]int{stale, pending, scopes} != second {
				t.Fatalf("org_first refresh changed org_second backlog to stale=%d pending=%d scopes=%d from %v", stale, pending, scopes, second)
			}
			objects.hook(nil)
			if _, err := content.RefreshTick(t.Context(), counter, "org_second", withoutDeadline); err != nil {
				t.Fatal(err)
			}
			if stale, pending, scopes := searchRefreshBacklog(t, f, "org_second"); stale != 0 || pending != 0 || scopes != 0 {
				t.Fatalf("org_second not ready stale=%d pending=%d scopes=%d", stale, pending, scopes)
			}
		})
	}
}

func TestSearchRefreshTickProgressesEachOrganization(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
