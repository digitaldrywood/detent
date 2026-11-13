package searchrecords_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

func frozenClock() func() time.Time {
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
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
			ticks := 0
			for {
				processed, err := f.content.RefreshTick(t.Context(), f.repository, "org_first", frozenClock())
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
			var refreshing int
			if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_scope_refresh WHERE organization_id='org_first'`).Scan(&refreshing); err != nil || refreshing != 0 {
				t.Fatalf("scope refresh rows=%d err=%v", refreshing, err)
			}
		})
	}
}

func TestSearchRefreshTickStopsAtDurationBudget(t *testing.T) {
	f := newSearchFixture(t)
	seedPendingIssues(t, f, "org_first", 50)
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	step := searchstore.RefreshTickDuration / 2
	clock := func() time.Time {
		at = at.Add(step)
		return at
	}
	processed, err := f.content.RefreshTick(t.Context(), f.repository, "org_first", clock)
	if err != nil || processed != 2 || pendingSearchDocuments(t, f, "org_first") != 48 {
		t.Fatalf("processed=%d pending=%d err=%v", processed, pendingSearchDocuments(t, f, "org_first"), err)
	}
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
			if _, err := f.content.RefreshTick(t.Context(), f.repository, org, frozenClock()); err != nil {
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
	calls := 0
	clock := func() time.Time {
		calls++
		if calls == 6 {
			cancel()
		}
		return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	}
	processed, err := f.content.RefreshTick(ctx, f.repository, "org_first", clock)
	if !errors.Is(err, context.Canceled) || processed != 5 || pendingSearchDocuments(t, f, "org_first") != documents-processed {
		t.Fatalf("processed=%d pending=%d err=%v", processed, pendingSearchDocuments(t, f, "org_first"), err)
	}
	processed, err = f.content.RefreshTick(t.Context(), f.repository, "org_first", frozenClock())
	if err != nil || processed != documents-5 || pendingSearchDocuments(t, f, "org_first") != 0 {
		t.Fatalf("resumed processed=%d pending=%d err=%v", processed, pendingSearchDocuments(t, f, "org_first"), err)
	}
	var published, terms int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE indexed_revision=revision),
 (SELECT count(DISTINCT document_id) FROM search_postings p WHERE p.organization_id='org_first' AND left(p.term,1)<>'~' AND p.document_id LIKE 'issue/wi_tick_%')
 FROM search_documents WHERE organization_id='org_first' AND id LIKE 'issue/wi_tick_%'`).Scan(&published, &terms); err != nil || published != documents || terms != documents {
		t.Fatalf("published=%d documents with terms=%d err=%v", published, terms, err)
	}
}
