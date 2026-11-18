package searchrecords_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func duplicateState(t *testing.T, f searchFixture, org tracker.OrganizationID) searchstore.DuplicateSourceState {
	t.Helper()
	var state searchstore.DuplicateSourceState
	err := f.repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		state, err = r.(searchstore.DuplicateSourceReader).DuplicateSourceState(t.Context(), "prj_first")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestDuplicateSourcesReadTheBoundedPageWindow(t *testing.T) {
	t.Parallel()
	f := newBoardSearchFixture(t)
	ctx := t.Context()
	now := pgstoretest.HistoryAt()
	fingerprint := issueorigin.Origin{Kind: "worker", Fingerprint: "example-duplicate-fingerprint"}
	republishSearchText(t, f, "wi_first_1", 1, "Magazine count reason code", "Counting magazines by reason code")
	republishSearchText(t, f, "wi_first_2", 2, "Example unrelated", issueorigin.Stamp("Unrelated body", fingerprint))
	for _, statement := range []string{
		`UPDATE native_work_item_page_progress SET backfill_phase=2`,
		`DELETE FROM native_work_item_page_dirty WHERE kind='item'`,
	} {
		if _, err := f.admin.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_page_progress(organization_id,backfill_phase) VALUES('org_first',2),('org_second',2) ON CONFLICT (organization_id) DO UPDATE SET backfill_phase=2`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, row := range searchstore.FingerprintRows("prj_first", issueorigin.Stamp("Unrelated body", fingerprint)) {
			if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind,release_series) VALUES($1,'prj_first','wi_first_2','item','wi_first_2',$2,$3,$4,$5) ON CONFLICT DO NOTHING`, org, row.Digest, row.Origin, row.OriginKind, row.ReleaseSeries); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func(org tracker.OrganizationID, query searchstore.DuplicateSourceQuery) ([]tracker.NativeWorkItemID, error) {
		var page searchstore.DuplicateSourcePage
		err := f.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(ctx, query)
			return err
		})
		var ids []tracker.NativeWorkItemID
		for _, source := range page.Sources {
			if source.Document.OrganizationID != org || source.Text.Title == "" {
				t.Fatalf("tenant %s duplicate source %+v", org, source)
			}
			ids = append(ids, source.Document.WorkItemID)
		}
		return ids, err
	}
	request := func(title, body string) searchstore.DuplicateSourceQuery {
		return searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Request: &searchstore.Text{Title: title, Body: body}, Text: title + "\n" + body, Now: now}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if state := duplicateState(t, f, org); !state.Ready {
			t.Fatalf("tenant %s duplicate search not ready after projection", org)
		}
		for _, test := range []struct {
			name  string
			query searchstore.DuplicateSourceQuery
			want  []tracker.NativeWorkItemID
		}{
			{"title words", request("Magazine count reason code", ""), []tracker.NativeWorkItemID{"wi_first_1"}},
			{"body words", request("Unrelated heading", "Count the magazines for every reason code"), []tracker.NativeWorkItemID{"wi_first_1"}},
			{"fingerprint bypasses terms", request("Completely different", issueorigin.Stamp("Other text", fingerprint)), []tracker.NativeWorkItemID{"wi_first_2"}},
			{"no match", request("Absentuniqueterm", ""), nil},
		} {
			got, err := read(org, test.query)
			if err != nil {
				t.Fatalf("tenant %s %s: %v", org, test.name, err)
			}
			if len(got) != len(test.want) || len(got) > 0 && got[0] != test.want[0] {
				t.Errorf("tenant %s %s sources=%v want %v", org, test.name, got, test.want)
			}
		}
	}
	if _, err := f.admin.ExecContext(ctx, `UPDATE native_work_item_pages SET terminal=true,closed_at=$1 WHERE organization_id='org_first' AND work_item_id='wi_first_1'`, now.Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, machine := range []bool{false, true} {
		query := request("Magazine count reason code", "")
		query.Machine = machine
		got, err := read("org_first", query)
		if err != nil || (len(got) == 1) != machine {
			t.Errorf("machine=%t sources for an issue closed 40 days ago=%v err=%v", machine, got, err)
		}
	}
	reporter := "another-owner"
	query := request("Magazine count reason code", "")
	query.Reporter = &reporter
	if got, err := read("org_second", query); err != nil || len(got) != 0 {
		t.Errorf("reporter-limited sources=%v err=%v", got, err)
	}
	if _, err := f.admin.ExecContext(ctx, `INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind) VALUES('org_second','prj_first','wi_first_1','wi_first_1','item')`); err != nil {
		t.Fatal(err)
	}
	if state := duplicateState(t, f, "org_second"); state.Ready {
		t.Fatal("pending item page left duplicate search ready")
	}
	if _, err := read("org_second", request("Magazine count reason code", "")); !errors.Is(err, searchstore.ErrIndexPending) {
		t.Fatalf("pending projection error=%v", err)
	}
	if state := duplicateState(t, f, "org_first"); !state.Ready {
		t.Fatal("another tenant's pending page changed readiness")
	}
}
