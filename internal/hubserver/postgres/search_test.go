package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/testenv/searchtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type searchFixture struct {
	repository *postgres.Repository
	content    *searchstore.Content
	objects    *historyObjects
	admin      *sql.DB
	pool       *sql.DB
}

func newSearchFixture(t *testing.T) searchFixture {
	t.Helper()
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	objects := newHistoryObjects()
	f := searchFixture{repository, searchstore.NewContent(objects), objects, admin, pool}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		seedRepository(t, repository, org)
	}
	return f
}

func (f searchFixture) put(t *testing.T, d searchstore.Document, text searchstore.Text, expected int64) searchstore.Document {
	t.Helper()
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = historyAt()
	}
	prepared, err := f.content.Prepare(t.Context(), d, text)
	if err != nil {
		t.Fatal(err)
	}
	err = f.repository.WriteSearch(t.Context(), d.OrganizationID, corestore.Serving, func(w searchstore.Writer) error { return w.PutSearch(t.Context(), prepared, expected) })
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func searchIssue(org tracker.OrganizationID, item string) searchstore.Document {
	return searchstore.Document{OrganizationID: org, ID: "issue/" + item, SourceID: item, ProjectID: "prj_first", WorkItemID: tracker.NativeWorkItemID(item), Kind: "issue", Scope: "issue", Revision: 1}
}

func (f searchFixture) drain(t *testing.T, org tracker.OrganizationID) {
	t.Helper()
	for {
		more, err := f.content.RefreshOne(t.Context(), f.repository, org)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
}

func (f searchFixture) find(t *testing.T, org tracker.OrganizationID, query searchstore.Query) searchstore.Page {
	t.Helper()
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Kind == "" {
		query.Kind, query.ProjectID = "work", "prj_first"
	}
	page, err := f.content.Find(t.Context(), f.repository, org, query)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func searchIDs(page searchstore.Page) []string {
	var ids []string
	for _, hit := range page.Items {
		id := string(hit.Document.WorkItemID)
		if hit.Document.Kind == "thread" {
			id = hit.Document.ID
		}
		ids = append(ids, id)
	}
	return ids
}

func TestSearchExistingSnippets(t *testing.T) {
	f := newSearchFixture(t)
	for _, test := range searchtest.Snippets() {
		t.Run(test.Name, func(t *testing.T) {
			d := searchIssue("org_first", "wi_first_1")
			var revision int64
			if err := f.admin.QueryRowContext(t.Context(), `SELECT COALESCE(max(revision), 0) FROM search_documents WHERE organization_id='org_first' AND id=$1`, d.ID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			d.Revision = revision + 1
			f.put(t, d, searchstore.Text{Title: test.Title, Body: test.Body}, revision)
			for _, mode := range []string{"pending", "indexed"} {
				t.Run(mode, func(t *testing.T) {
					if mode == "indexed" {
						f.drain(t, d.OrganizationID)
					}
					page := f.find(t, d.OrganizationID, searchstore.Query{Text: test.Query})
					if len(page.Items) != 1 || page.Items[0].Snippet != test.Want {
						t.Fatalf("snippet page=%+v want=%q", page, test.Want)
					}
				})
			}
		})
	}
}

func TestSearchRankingAndCursor(t *testing.T) {
	f := newSearchFixture(t)
	org := tracker.OrganizationID("org_first")
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		err := f.repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
			for number := int64(3); number <= 4; number++ {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_first_%d", number)), ProjectID: "prj_first", Number: number, Title: "Example title"})
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct{ id, title, body string }{
			{"wi_first_1", "Separate symptom", "Repository association deadlocks"},
			{"wi_first_2", "Repository association deadlocks", ""},
			{"wi_first_3", "Repository association", ""},
			{"wi_first_4", "Repository association deadlocks", ""},
		} {
			f.put(t, searchIssue(org, test.id), searchstore.Text{Title: test.title, Body: test.body}, 0)
		}
	}
	terms := []string{"repos", "associ", "deadlock"}
	for index := range 29 {
		terms = append(terms, fmt.Sprintf("unmatched%d", index))
	}
	for _, mode := range []string{"pending", "indexed"} {
		if mode == "indexed" {
			f.drain(t, org)
		}
		for _, query := range []string{"repos associ deadlock", strings.Join(terms, " "), strings.Join(terms, " ") + " separate"} {
			t.Run(mode+"/"+query, func(t *testing.T) {
				q := searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: query, Limit: 1}
				var got []string
				for {
					page := f.find(t, org, q)
					got = append(got, searchIDs(page)...)
					if page.NextCursor == "" {
						break
					}
					if q.Cursor == "" {
						for _, changed := range []string{"tenant", "project", "text", "reporter", "exact"} {
							wrong, tenant := q, org
							wrong.Cursor = page.NextCursor
							switch changed {
							case "tenant":
								tenant = "org_second"
							case "project":
								wrong.ProjectID = "prj_second"
							case "text":
								wrong.Text = "other"
							case "reporter":
								wrong.Reporter = new("example-principal")
							case "exact":
								wrong.Exact = true
							}
							if _, err := f.content.Find(t.Context(), f.repository, tenant, wrong); !errors.Is(err, corestore.ErrInvalidCursor) {
								t.Fatalf("cursor reused after %s changed: %v", changed, err)
							}
						}
					}
					q.Cursor = page.NextCursor
				}
				want := []string{"wi_first_4", "wi_first_2", "wi_first_1", "wi_first_3"}
				if !slices.Equal(got, want) {
					t.Fatalf("coverage/title/tie order=%v want=%v", got, want)
				}
			})
		}
	}
}

func TestSearchAcceptanceMatrix(t *testing.T) {
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "repo/path-name:word@example.org #123", Body: "Café naïve Ελληνικά 中文 Unicorns decorate mock fixture data. rate%_\\marker %% __", Labels: []string{"unique-xyz", "leftedge", "rightedge"}}, 0)
		f.put(t, searchIssue(org, "wi_first_2"), searchstore.Text{Title: "Other title", Body: "separate"}, 0)
		other := searchIssue(org, "wi_second_1")
		other.ProjectID = "prj_second"
		f.put(t, other, searchstore.Text{Title: "foreignsentinel Unicorns decorate mock fixture data"}, 0)
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id=$1 AND native_id='wi_first_1'`, org); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"pending", "indexed"} {
		if mode == "indexed" {
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				f.drain(t, org)
			}
		}
		for _, test := range []struct {
			name, query string
			project     tracker.ProjectID
			reporter    *string
			want        []string
		}{
			{"punctuation", "path-name", "prj_first", nil, []string{"wi_first_1"}},
			{"diacritics", "cafe", "prj_first", nil, []string{"wi_first_1"}},
			{"Greek", "Ελληνικά", "prj_first", nil, []string{"wi_first_1"}},
			{"Chinese", "中文", "prj_first", nil, []string{"wi_first_1"}},
			{"title substring", "ath-na", "prj_first", nil, []string{"wi_first_1"}},
			{"body substring", "corns decor", "prj_first", nil, []string{"wi_first_1"}},
			{"label substring", "nique-xy", "prj_first", nil, []string{"wi_first_1"}},
			{"short substring", "@", "prj_first", nil, []string{"wi_first_1"}},
			{"literal LIKE metacharacters", `%_\`, "prj_first", nil, []string{"wi_first_1"}},
			{"doubled percent is literal", "%%", "prj_first", nil, []string{"wi_first_1"}},
			{"doubled underscore is literal", "__", "prj_first", nil, []string{"wi_first_1"}},
			{"label boundary", "leftedgerightedge", "prj_first", nil, nil},
			{"foreign project", "foreignsentinel", "prj_first", nil, nil},
			{"project name substring", "first#", "prj_first", nil, []string{"wi_first_2", "wi_first_1"}},
			{"project ID substring", "rj_first#", "prj_first", nil, []string{"wi_first_2", "wi_first_1"}},
			{"project name number substring", "irst#2", "prj_first", nil, []string{"wi_first_2"}},
			{"project number", "prj_first#1", "prj_first", nil, []string{"wi_first_1"}},
			{"project name number", "first#1", "prj_first", nil, []string{"wi_first_1"}},
			{"number", "#1", "prj_first", nil, []string{"wi_first_1"}},
			{"bare number before literal substring", "2", "prj_first", nil, []string{"wi_first_2", "wi_first_1"}},
			{"signed number is not an identity", "+2", "prj_first", nil, nil},
			{"reporter own", "unicorn", "prj_first", new("example-principal"), []string{"wi_first_1"}},
			{"reporter denied", "unicorn", "prj_first", new("other-principal"), nil},
		} {
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				t.Run(mode+"/"+string(org)+"/"+test.name, func(t *testing.T) {
					page := f.find(t, org, searchstore.Query{Kind: "work", ProjectID: test.project, Text: test.query, Reporter: test.reporter})
					if !slices.Equal(searchIDs(page), test.want) {
						t.Fatalf("matches=%v want=%v", searchIDs(page), test.want)
					}
					for _, hit := range page.Items {
						if hit.Document.OrganizationID != org {
							t.Fatalf("tenant escaped: %+v", hit)
						}
					}
				})
			}
		}
	}
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=true WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		archived string
		want     int
	}{{"", 0}, {"true", 1}, {"all", 1}} {
		page := f.find(t, "org_first", searchstore.Query{Text: "unicorn", Archived: test.archived})
		if len(page.Items) != test.want {
			t.Fatalf("archived=%q results=%d want=%d", test.archived, len(page.Items), test.want)
		}
	}
}

func TestSearchProjectRenameRefreshesMetadataSubstrings(t *testing.T) {
	f := newSearchFixture(t)
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE projects SET name='Original label' WHERE organization_id='org_first' AND id='prj_first'`); err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"wi_first_1", "wi_first_2"} {
		f.put(t, searchIssue("org_first", item), searchstore.Text{Title: "Example title"}, 0)
	}
	f.drain(t, "org_first")
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE projects SET name='Renamed' WHERE organization_id='org_first' AND id='prj_first'`); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"refreshing", "indexed"} {
		if mode == "indexed" {
			f.drain(t, "org_first")
		}
		for _, test := range []struct {
			query string
			want  []string
		}{
			{"named#1", []string{"wi_first_1"}},
			{"label#", nil},
			{"first#1", []string{"wi_first_1"}},
			{"first#", []string{"wi_first_2", "wi_first_1"}},
		} {
			page := f.find(t, "org_first", searchstore.Query{Text: test.query})
			if !slices.Equal(searchIDs(page), test.want) {
				t.Fatalf("%s query=%q matches=%v want=%v", mode, test.query, searchIDs(page), test.want)
			}
		}
	}
}

func TestSearchRefreshFencesUpdatesAndDeletion(t *testing.T) {
	for _, operation := range []string{"update", "delete", "delete and reuse", "comment update", "comment delete", "comment hide"} {
		t.Run(operation, func(t *testing.T) {
			f := newSearchFixture(t)
			d := searchIssue("org_first", "wi_first_1")
			if strings.HasPrefix(operation, "comment ") {
				f.put(t, d, searchstore.Text{Title: "Issue title"}, 0)
				d.ID, d.Kind, d.SourceID = "comment/example", "comment", "comment_first_1_1"
			}
			d = f.put(t, d, searchstore.Text{Title: "oldsentinel"}, 0)
			read := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			done := make(chan error, 1)
			var work sync.WaitGroup
			work.Add(1)
			barrier := &searchReadBarrier{DetailStore: f.objects, read: read, release: release}
			t.Cleanup(func() { unblock(); work.Wait() })
			go func() {
				defer work.Done()
				_, err := searchstore.NewContent(barrier).RefreshOne(t.Context(), f.repository, "org_first")
				done <- err
			}()
			select {
			case <-read:
			case err := <-done:
				t.Fatalf("refresh ended before reading object: %v", err)
			}
			if operation != "update" && operation != "comment update" && operation != "comment hide" {
				err := f.repository.WriteSearch(t.Context(), "org_first", corestore.Serving, func(w searchstore.Writer) error { return w.DeleteSearch(t.Context(), d.ID, d.Revision) })
				if err != nil {
					unblock()
					<-done
					t.Fatal(err)
				}
			}
			if operation == "comment hide" {
				if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_comments SET deleted=true WHERE organization_id='org_first' AND id='comment_first_1_1'`); err != nil {
					t.Fatal(err)
				}
			}
			if operation != "delete" && operation != "comment delete" && operation != "comment hide" {
				expected := d.Revision
				if operation == "update" || operation == "comment update" {
					d.Revision++
				} else {
					expected = 0
				}
				f.put(t, d, searchstore.Text{Title: "newsentinel"}, expected)
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := f.find(t, "org_first", searchstore.Query{Text: "oldsentinel"}); len(got.Items) != 0 {
				t.Fatalf("stale refresh published old source: %+v", got)
			}
			if strings.HasPrefix(operation, "comment ") {
				var indexed int64
				if err := f.admin.QueryRowContext(t.Context(), `SELECT indexed_revision FROM search_documents WHERE organization_id='org_first' AND id='issue/wi_first_1'`).Scan(&indexed); err != nil || indexed != 0 {
					t.Fatalf("stale aggregate published: indexed=%d error=%v", indexed, err)
				}
			}
			if operation != "delete" && operation != "comment delete" && operation != "comment hide" {
				for _, mode := range []string{"pending", "indexed"} {
					if mode == "indexed" {
						f.drain(t, "org_first")
					}
					if got := f.find(t, "org_first", searchstore.Query{Text: "newsentinel"}); len(got.Items) != 1 {
						t.Fatalf("new source missing in %s: %+v", mode, got)
					}
				}
			}
		})
	}
}

type searchReadBarrier struct {
	once sync.Once
	attachment.DetailStore
	read    chan struct{}
	release chan struct{}
}

func (s *searchReadBarrier) ReadDetail(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	s.once.Do(func() { close(s.read) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.DetailStore.ReadDetail(ctx, org, ref)
}

func TestSearchRefreshBoundsAndIntegrity(t *testing.T) {
	f := newSearchFixture(t)
	first := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "first sentinel"}, 0)
	f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "second sentinel"}, 0)
	more, err := f.content.RefreshOne(t.Context(), f.repository, "org_first")
	if err != nil || !more {
		t.Fatalf("refresh=%v err=%v", more, err)
	}
	var pending int
	if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents WHERE organization_id='org_first' AND indexed_revision<>revision`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("one tick pending=%d err=%v", pending, err)
	}
	f.objects.mu.Lock()
	delete(f.objects.objects, first.Payload.Key)
	f.objects.mu.Unlock()
	if _, err := f.content.Find(t.Context(), f.repository, "org_first", searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "first", Limit: 10}); !errors.Is(err, artifact.ErrIntegrity) {
		t.Fatalf("missing object error=%v", err)
	}
	foreign := first
	foreign.OrganizationID = "org_second"
	if _, err := f.content.Read(t.Context(), foreign); !errors.Is(err, artifact.ErrIntegrity) {
		t.Fatalf("foreign object error=%v", err)
	}
	if !reflect.DeepEqual(searchIDs(f.find(t, "org_second", searchstore.Query{Text: "sentinel"})), []string(nil)) {
		t.Fatal("tenant leaked")
	}
}

func TestSearchDocumentByteBudget(t *testing.T) {
	const budget = 64 * 1024
	for _, test := range []struct{ name, title, body string }{
		{"body", "Titlemarker", strings.Repeat("narwhal ", 25000) + " tailmarker"},
		{"unicode boundary", "Titlemarker", strings.Repeat("界 ", 50000) + " tailmarker"},
		{"title priority", "Titlemarker " + strings.Repeat("narwhal ", 10000), "tailmarker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSearchFixture(t)
			d := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: test.title, Body: test.body}, 0)
			text, err := f.content.Read(t.Context(), d)
			if err != nil {
				t.Fatal(err)
			}
			if size := len(text.Title) + len(text.Body); size > budget || !utf8.ValidString(text.Title+text.Body) {
				t.Errorf("document bytes=%d, budget=%d, valid=%v", size, budget, utf8.ValidString(text.Title+text.Body))
			}
			for _, mode := range []string{"pending", "indexed"} {
				if mode == "indexed" {
					f.drain(t, "org_first")
				}
				for _, query := range []string{"Titlemarker", "#1"} {
					if got := searchIDs(f.find(t, "org_first", searchstore.Query{Text: query})); !slices.Equal(got, []string{"wi_first_1"}) {
						t.Errorf("%s query=%q matches=%v", mode, query, got)
					}
				}
				if got := searchIDs(f.find(t, "org_first", searchstore.Query{Text: "tailmarker"})); len(got) != 0 {
					t.Errorf("%s indexed tail=%v", mode, got)
				}
			}
		})
	}
}

func TestSearchOversizedLexemeDoesNotStarveRefresh(t *testing.T) {
	var dense strings.Builder
	for index := range 13000 {
		fmt.Fprintf(&dense, "%04x ", index)
	}
	for _, text := range []searchstore.Text{
		{Title: "Titlemarker", Body: strings.Repeat("a", 3000) + " bodysentinel"},
		{Title: "Titlemarker", Body: strings.Repeat("a", 2000) + " bodysentinel"},
		{Title: "Titlemarker " + strings.Repeat("界", 1000), Body: "bodysentinel"},
		{Title: "Titlemarker", Body: dense.String() + "bodysentinel"},
	} {
		t.Run(fmt.Sprintf("token bytes %d", len(text.Title)+len(text.Body)), func(t *testing.T) {
			f := newSearchFixture(t)
			f.put(t, searchIssue("org_first", "wi_first_1"), text, 0)
			f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "secondmarker"}, 0)
			for tick := 0; tick < 2; tick++ {
				more, err := f.content.RefreshOne(t.Context(), f.repository, "org_first")
				if err != nil || !more {
					t.Errorf("tick=%d more=%v error=%v", tick, more, err)
				}
			}
			var pending int
			if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_documents WHERE organization_id='org_first' AND indexed_revision<>revision`).Scan(&pending); err != nil || pending != 0 {
				t.Errorf("pending=%d error=%v", pending, err)
			}
			for _, query := range []string{"Titlemarker", "bodysentinel", "secondmarker"} {
				if got := f.find(t, "org_first", searchstore.Query{Text: query}); len(got.Items) != 1 {
					t.Errorf("query=%q matches=%v", query, searchIDs(got))
				}
			}
		})
	}
}

func TestSearchFailedObjectDoesNotStarveRefresh(t *testing.T) {
	f := newSearchFixture(t)
	first := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "firstmarker"}, 0)
	f.put(t, searchIssue("org_first", "wi_first_2"), searchstore.Text{Title: "secondmarker"}, 0)
	f.objects.mu.Lock()
	raw := f.objects.objects[first.Payload.Key]
	delete(f.objects.objects, first.Payload.Key)
	f.objects.mu.Unlock()
	if more, err := f.content.RefreshOne(t.Context(), f.repository, "org_first"); !more || !errors.Is(err, artifact.ErrIntegrity) {
		t.Fatalf("failed object refresh more=%v error=%v", more, err)
	}
	if more, err := f.content.RefreshOne(t.Context(), f.repository, "org_first"); !more || err != nil {
		t.Fatalf("next document refresh more=%v error=%v", more, err)
	}
	var revision int64
	if err := f.admin.QueryRowContext(t.Context(), `SELECT indexed_revision FROM search_documents WHERE organization_id='org_first' AND id='issue/wi_first_2'`).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("next indexed revision=%d error=%v", revision, err)
	}
	f.objects.mu.Lock()
	f.objects.objects[first.Payload.Key] = raw
	f.objects.mu.Unlock()
	f.drain(t, "org_first")
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("scope refresh/indexed=%v", indexed), func(t *testing.T) {
			g := newSearchFixture(t)
			first := g.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "firstmarker"}, 0)
			later := searchIssue("org_first", "wi_first_1")
			later.ID, later.SourceID, later.Kind, later.Status = "zzthread", "zzthread", "thread", "open"
			g.put(t, later, searchstore.Text{Title: "latermarker"}, 0)
			if indexed {
				g.drain(t, "org_first")
			}
			g.objects.mu.Lock()
			raw := g.objects.objects[first.Payload.Key]
			delete(g.objects.objects, first.Payload.Key)
			g.objects.mu.Unlock()
			if _, err := g.admin.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='example-principal' WHERE organization_id='org_first' AND native_id='wi_first_1'`); err != nil {
				t.Fatal(err)
			}
			for tick := 0; tick < 3; tick++ {
				more, err := g.content.RefreshOne(t.Context(), g.repository, "org_first")
				if !more || err != nil && !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("tick=%d more=%v error=%v", tick, more, err)
				}
			}
			var pending int
			if err := g.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_scope_refresh WHERE organization_id='org_first'`).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("scope cursors=%d error=%v", pending, err)
			}
			var indexed int64
			if err := g.admin.QueryRowContext(t.Context(), `SELECT indexed_revision FROM search_documents WHERE organization_id='org_first' AND id='zzthread'`).Scan(&indexed); err != nil || indexed != 1 {
				t.Fatalf("later indexed revision=%d error=%v", indexed, err)
			}
			g.objects.mu.Lock()
			g.objects.objects[first.Payload.Key] = raw
			g.objects.mu.Unlock()
			g.drain(t, "org_first")
		})

	}
}

func TestSearchCoverageSurvivesCandidateLimit(t *testing.T) {
	f := newSearchFixture(t)
	base := f.put(t, searchIssue("org_first", "wi_first_1"), searchstore.Text{Title: "commonterm"}, 0)
	f.drain(t, "org_first")
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,native_id,project_id,number,title)
 SELECT 'org_first','wi_common_'||n,'prj_first',n+10,'commonterm' FROM generate_series(1,2100) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_documents(organization_id,id,project_id,work_item_id,kind,source_id,revision,indexed_revision,title,scope,updated_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,vector,token_count,terms,term_frequencies)
 SELECT 'org_first','issue/wi_common_'||n,'prj_first','wi_common_'||n,'issue','wi_common_'||n,1,1,'commonterm','issue',CURRENT_TIMESTAMP,$1,$2,$3,$4,$5::tsvector,1,ARRAY['commonterm'],ARRAY[5]::bigint[] FROM generate_series(1,2100) n`, base.Payload.Key, base.Payload.SHA256, base.Payload.CompressedSize, base.Payload.UncompressedSize, searchstore.Vector(searchstore.Document{OrganizationID: base.OrganizationID, ProjectID: base.ProjectID, Kind: "thread"}, searchstore.Text{Title: "commonterm"})); err != nil {
		t.Fatal(err)
	}
	target := searchIssue("org_first", "wi_first_2")
	f.put(t, target, searchstore.Text{Title: "commonterm rareterm"}, 0)
	f.drain(t, "org_first")
	var candidates []searchstore.Hit
	if err := f.repository.ReadSearch(t.Context(), "org_first", corestore.Serving, func(r searchstore.Reader) error {
		var err error
		candidates, err = r.Search(t.Context(), searchstore.Query{Kind: "work", ProjectID: "prj_first", Text: "commonterm rareterm"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(candidates, func(hit searchstore.Hit) bool { return hit.Document.ID == target.ID }) {
		t.Fatalf("all-term match missing from %d candidates", len(candidates))
	}
	page := f.find(t, "org_first", searchstore.Query{Text: "commonterm rareterm", Limit: 1})
	if got := searchIDs(page); !slices.Equal(got, []string{"wi_first_2"}) || page.Items[0].Coverage != 2 {
		t.Fatalf("top result=%+v", page)
	}
}

func TestSearchTargetRankCD(t *testing.T) {
	t.Skip("S3_TARGET_RANK_CD: weighted ts_rank_cd reader is not implemented")
	for _, test := range []struct {
		name, kind, firstTitle, firstBody, secondTitle, secondBody string
	}{
		{"work title above body", "work", "alpha beta", "filler", "filler", "alpha beta"},
		{"work rank without BM25 length penalty", "work", "alpha alpha beta beta " + strings.Repeat("filler ", 32), "", "alpha beta", ""},
		{"thread relevance above recency", "thread", "alpha beta", "", "alpha", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSearchFixture(t)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				for index, text := range []searchstore.Text{{Title: test.firstTitle, Body: test.firstBody}, {Title: test.secondTitle, Body: test.secondBody}} {
					d := searchIssue(org, fmt.Sprintf("wi_first_%d", index+1))
					if test.kind == "thread" {
						d.Status = "open"
						d.UpdatedAt = historyAt().AddDate(0, 0, index)
						d.ID, d.SourceID, d.Kind, d.Scope, d.ProjectID, d.WorkItemID = fmt.Sprintf("thread_%d", index), fmt.Sprintf("thread_%d", index), "thread", "general", "", ""
					}
					f.put(t, d, text, 0)
				}
			}
			for _, mode := range []string{"pending", "indexed"} {
				for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
					if mode == "indexed" {
						f.drain(t, org)
					}
					q := searchstore.Query{Kind: test.kind, Text: "alpha beta", Limit: 1}
					if test.kind == "work" {
						q.ProjectID = "prj_first"
					}
					page := f.find(t, org, q)
					want := "wi_first_1"
					if test.kind == "thread" {
						want = "thread_0"
					}
					if !slices.Equal(searchIDs(page), []string{want}) || page.NextCursor == "" {
						t.Fatalf("%s tenant=%s rank page=%+v want first=%s and continuation", mode, org, page, want)
					}
				}
			}
		})
	}
}

func TestSearchTargetCappedCounts(t *testing.T) {
	t.Skip("S3_TARGET_CAPPED_COUNTS: search Page Total and TotalCapped metadata is not implemented")
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		documents := make([]searchstore.Document, 0, 1001)
		for index := range 1001 {
			id := fmt.Sprintf("count_%04d", index)
			title := "allcount"
			if index < 1000 {
				title += " exactcap"
			}
			if index < 999 {
				title += " belowcap"
			}
			d, err := f.content.Prepare(t.Context(), searchstore.Document{OrganizationID: org, ID: id, SourceID: id, Kind: "thread", Scope: "general", Status: "open", Revision: 1, UpdatedAt: historyAt()}, searchstore.Text{Title: title})
			if err != nil {
				t.Fatal(err)
			}
			documents = append(documents, d)
		}
		if err := f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error {
			for _, d := range documents {
				if err := w.PutSearch(t.Context(), d, 0); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		text   string
		total  int
		capped bool
	}{{"absent", 0, false}, {"belowcap", 999, false}, {"exactcap", 1000, false}, {"allcount", 1000, true}} {
		t.Run(test.text, func(t *testing.T) {
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				page := f.find(t, org, searchstore.Query{Kind: "thread", Text: test.text, Limit: 1})
				raw, err := json.Marshal(page)
				if err != nil {
					t.Fatal(err)
				}
				var count struct {
					Total       *int
					TotalCapped *bool
				}
				if err := json.Unmarshal(raw, &count); err != nil {
					t.Fatal(err)
				}
				if count.Total == nil || count.TotalCapped == nil {
					t.Fatal("search page must distinguish an exact total from a capped lower bound")
				}
				if *count.Total != test.total || *count.TotalCapped != test.capped {
					t.Fatalf("tenant=%s count=%d capped=%t want=%d capped=%t", org, *count.Total, *count.TotalCapped, test.total, test.capped)
				}
				if test.total > 1 && page.NextCursor == "" {
					t.Fatal("count cap truncated continuation")
				}
			}
		})
	}
}

func TestSearchTargetTrigramRepresentation(t *testing.T) {
	t.Skip("S2_TARGET_TRIGRAM: bounded normalized search_document_index.literal_text with pg_trgm GIN is not implemented")
	f := newSearchFixture(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f.put(t, searchIssue(org, "wi_first_1"), searchstore.Text{Title: "leftedge", Body: `rightedge PATH/Substring rate%_\marker`, Labels: []string{"labelleft", "labelright"}}, 0)
		f.put(t, searchIssue(org, "wi_first_2"), searchstore.Text{Title: "unrelated", Body: strings.Repeat("x", 64*1024) + "outsidebudget"}, 0)
		f.drain(t, org)
		for _, test := range []struct {
			text  string
			exact bool
			want  []string
		}{
			{"ubstri", false, []string{"wi_first_1"}},
			{`%_\`, false, []string{"wi_first_1"}},
			{"abelle", false, []string{"wi_first_1"}},
			{"leftedgerightedge", false, nil},
			{"labelleftlabelright", false, nil},
			{"outsidebudget", false, nil},
			{"ubstri", true, nil},
		} {
			page := f.find(t, org, searchstore.Query{Text: test.text, Exact: test.exact})
			if !slices.Equal(searchIDs(page), test.want) {
				t.Fatalf("tenant=%s literal=%q exact=%t matches=%v want=%v", org, test.text, test.exact, searchIDs(page), test.want)
			}
		}
		var rows, oversized int
		if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER (WHERE octet_length(literal_text)>65536) FROM search_document_index WHERE organization_id=$1`, org).Scan(&rows, &oversized); err != nil {
			t.Fatal(err)
		}
		if rows < 2 || oversized != 0 {
			t.Fatalf("bounded search representation: rows=%d oversized=%d", rows, oversized)
		}
	}
	var indexed bool
	if err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am a ON a.oid=c.relam JOIN pg_opclass o ON o.oid=ANY(i.indclass) WHERE i.indrelid='search_document_index'::regclass AND i.indisvalid AND a.amname='gin' AND o.opcname='gin_trgm_ops')`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("search representation has no valid pg_trgm GIN index")
	}
}
