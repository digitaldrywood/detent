package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
)

type searchObjectReads struct {
	attachment.DetailStore
	reads atomic.Int64
}

func (s *searchObjectReads) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	s.reads.Add(1)
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestSearchIndexedRankingHydratesOnlySelectedSnippets(t *testing.T) {
	f := newSearchFixture(t)
	f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "Sentinel marker", Body: "Sentinel"}, 0)
	second := f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "Sentinel other", Body: "Separate"}, 0)
	f.drain(t, "org_first")
	err := f.repository.ReadSearch(t.Context(), "org_first", corestore.Serving, func(r searchstore.Reader) error {
		hits, err := r.Search(t.Context(), searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "sentinel"})
		if err != nil {
			return err
		}
		hits = slices.DeleteFunc(hits, func(hit searchstore.Hit) bool { return hit.ScanBoundary })
		if len(hits) != 2 {
			t.Fatalf("indexed candidates=%d want two", len(hits))
		}
		for _, hit := range hits {
			if hit.Document.WorkItemID == "wi_first_1" && !reflect.DeepEqual(hit.Index, &searchstore.Statistics{Tokens: 3, Terms: []string{"marker", "sentinel"}, Frequencies: []int64{5, 6}}) {
				t.Fatalf("weighted term statistics=%+v", hit.Index)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	objects := &searchObjectReads{DetailStore: f.objects}
	content := searchstore.NewContent(objects)
	query := searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "sentinel", Limit: 1}
	for _, missingOther := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy objects", true: "nonselected object missing"}[missingOther], func(t *testing.T) {
			if missingOther {
				f.objects.mu.Lock()
				delete(f.objects.objects, second.Payload.Key)
				f.objects.mu.Unlock()
			}
			objects.reads.Store(0)
			page, err := content.Find(t.Context(), f.repository, "org_first", query)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].Document.WorkItemID != "wi_first_1" || page.NextCursor == "" || objects.reads.Load() != 1 {
				t.Fatalf("page=%+v object reads=%d", page, objects.reads.Load())
			}
			if missingOther {
				next := query
				next.Cursor = page.NextCursor
				if _, err := content.Find(t.Context(), f.repository, "org_first", next); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("selected missing object error=%v", err)
				}
			}
		})
	}
}

func TestSearchRefreshContinuesBoundedSourcesAfterEdit(t *testing.T) {
	f := newSearchFixture(t)
	d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "Bounded history"}, 0)
	var first searchstore.Document
	for index := range 33 {
		id := fmt.Sprintf("bounded_comment_%02d", index)
		if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO native_comments(organization_id,id,project_id,work_item_id,sequence,body_object_key,actor_kind,actor_principal_id) VALUES('org_first',$1,'prj_first','wi_first_1',$2,'archive','human','example')`, id, index+100); err != nil {
			t.Fatal(err)
		}
		source := d
		source.ID, source.Kind, source.SourceID = "comment/"+id, "comment", id
		text := "small"
		if index == 0 {
			text = "oldmarker"
		}
		if index == 32 {
			text = "tailmarker"
		}
		source = f.put(t, source, searchstore.Text{Body: text}, 0)
		if index == 0 {
			first = source
		}
	}
	objects := &searchObjectReads{DetailStore: f.objects}
	content := searchstore.NewContent(objects)
	if _, err := content.Find(t.Context(), f.repository, "org_first", searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "oldmarker", Limit: 1}); !errors.Is(err, searchstore.ErrIndexPending) {
		t.Fatalf("large pending aggregate error=%v", err)
	}
	if reads := objects.reads.Load(); reads > 33 {
		t.Fatalf("pending query read %d objects, limit 33", reads)
	}
	tick := func() {
		t.Helper()
		objects.reads.Store(0)
		if more, err := content.RefreshOne(t.Context(), f.repository, "org_first"); err != nil || !more {
			t.Fatalf("refresh more=%t error=%v", more, err)
		}
		if reads := objects.reads.Load(); reads > 34 {
			t.Fatalf("refresh read %d objects, limit 34", reads)
		}
	}
	tick()
	var indexed int64
	var after string
	if err := f.admin.QueryRowContext(t.Context(), `SELECT indexed_revision,progress_after_id FROM search_documents WHERE organization_id='org_first' AND id=$1`, d.ID).Scan(&indexed, &after); err != nil {
		t.Fatal(err)
	}
	if indexed != 0 || after == "" {
		t.Fatalf("first page indexed=%d cursor=%q", indexed, after)
	}
	first.Revision++
	first.UpdatedAt = first.UpdatedAt.Add(1)
	f.put(t, first, searchstore.Text{Body: "newmarker"}, 1)
	tick()
	tick()
	for _, query := range []string{"newmarker", "tailmarker"} {
		page := f.find(t, "org_first", searchstore.Query{Text: query})
		if len(page.Items) != 1 || page.Items[0].Document.WorkItemID != d.WorkItemID {
			t.Fatalf("continued result %q: %+v", query, page)
		}
	}
	if page := f.find(t, "org_first", searchstore.Query{Text: "oldmarker"}); len(page.Items) != 0 {
		t.Fatalf("stale aggregate survived edit: %+v", page)
	}
}
