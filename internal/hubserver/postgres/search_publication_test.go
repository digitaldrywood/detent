package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSearchPublicationReusesUnchangedPostings(t *testing.T) {
	f := newSearchFixture(t)
	if _, err := f.admin.ExecContext(t.Context(), `CREATE TABLE search_publication_writes(organization_id text,document_id text,operation text,writes bigint,PRIMARY KEY(organization_id,document_id,operation));
GRANT SELECT,INSERT,UPDATE ON search_publication_writes TO detent_runtime_v2;
CREATE FUNCTION count_search_publication_writes() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE posting public.search_postings;
BEGIN
 IF TG_OP='DELETE' THEN posting:=OLD; ELSE posting:=NEW; END IF;
 IF posting.term NOT IN ('~all','~pending') THEN
  INSERT INTO search_publication_writes VALUES(posting.organization_id,posting.document_id,TG_OP,1)
  ON CONFLICT(organization_id,document_id,operation) DO UPDATE SET writes=search_publication_writes.writes+1;
 END IF;
 RETURN NULL;
END $fn$;
CREATE TRIGGER count_search_publication_writes AFTER INSERT OR UPDATE OR DELETE ON search_postings FOR EACH ROW EXECUTE FUNCTION count_search_publication_writes()`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE projects SET name='path-name' WHERE organization_id=$1 AND id='prj_first'`, org); err != nil {
			t.Fatal(err)
		}
		d := searchIssue(org, "wi_first_1")
		text := searchstore.Text{Title: "Example source", Body: "path-name "}
		for i := 0; i < 60; i++ {
			text.Body += fmt.Sprintf("exampleword%03d ", i)
		}
		d = f.put(t, d, text, 0)
		comment := searchstore.Document{OrganizationID: org, ID: "comment:publication", Kind: "comment", SourceID: "publication", ProjectID: d.ProjectID, WorkItemID: d.WorkItemID, Scope: "issue", Revision: 1, UpdatedAt: d.UpdatedAt}
		if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO native_comments(organization_id,project_id,work_item_id,id,revision,sequence,body_object_key,actor_kind,actor_principal_id,created_at,updated_at) VALUES($1,$2,$3,'publication',1,999,'example','human','example',$4,$4)`, org, d.ProjectID, d.WorkItemID, d.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		f.put(t, comment, searchstore.Text{Body: "commentretainedsentinel"}, 0)
		f.drain(t, org)
		var postings int64
		if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_postings WHERE organization_id=$1 AND document_id=$2 AND term NOT IN ('~all','~pending')`, org, d.ID).Scan(&postings); err != nil {
			t.Fatal(err)
		}
		if postings < 1000 {
			t.Fatalf("fixture has only %d postings", postings)
		}
		for _, mode := range []string{"identical refresh", "revision and time", "literal source change"} {
			t.Run(string(org)+"/"+mode, func(t *testing.T) {
				staleLiterals := []string{}
				if mode == "identical refresh" {
					if err := f.repository.WriteSearch(t.Context(), org, corestore.Background, func(w searchstore.Writer) error { return w.AdvanceSearch(t.Context(), d) }); err != nil {
						t.Fatal(err)
					}
				} else {
					expected := d.Revision
					d.Revision++
					d.UpdatedAt = d.UpdatedAt.Add(time.Minute + time.Nanosecond)
					if mode == "literal source change" {
						previous := searchstore.LiteralPostings(d, text)
						text.Body = strings.ReplaceAll(text.Body, "path-name", "path+name")
						current := map[string]bool{}
						for _, posting := range searchstore.LiteralPostings(d, text) {
							current[posting.Term] = true
						}
						for _, posting := range previous {
							if !current[posting.Term] {
								staleLiterals = append(staleLiterals, posting.Term)
							}
						}
					}
					d = f.put(t, d, text, expected)
				}
				if _, err := f.admin.ExecContext(t.Context(), `DELETE FROM search_publication_writes WHERE organization_id=$1 AND document_id=$2`, org, d.ID); err != nil {
					t.Fatal(err)
				}
				f.drain(t, org)
				var inserts, updates, deletes int64
				if err := f.admin.QueryRowContext(t.Context(), `SELECT coalesce(sum(writes) FILTER(WHERE operation='INSERT'),0),coalesce(sum(writes) FILTER(WHERE operation='UPDATE'),0),coalesce(sum(writes) FILTER(WHERE operation='DELETE'),0) FROM search_publication_writes WHERE organization_id=$1 AND document_id=$2`, org, d.ID).Scan(&inserts, &updates, &deletes); err != nil {
					t.Fatal(err)
				}
				t.Logf("postings=%d inserts=%d updates=%d deletes=%d", postings, inserts, updates, deletes)
				if mode == "identical refresh" && inserts+updates+deletes != 0 {
					t.Errorf("identical publication rewrote postings: insert=%d update=%d delete=%d", inserts, updates, deletes)
				}
				if mode == "revision and time" && (inserts != 0 || deletes != 0 || updates != postings) {
					t.Errorf("revision-only publication recreated postings or lost current ordering: insert=%d update=%d delete=%d want_updates=%d", inserts, updates, deletes, postings)
				}
				if mode != "identical refresh" {
					var stale int64
					if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_postings WHERE organization_id=$1 AND document_id=$2 AND (updated_at<>$3 OR updated_at_submicrosecond<>$4)`, org, d.ID, d.UpdatedAt.Truncate(time.Microsecond), d.UpdatedAt.Nanosecond()%1000).Scan(&stale); err != nil {
						t.Fatal(err)
					}
					if stale != 0 {
						t.Errorf("%d postings retained stale ordering timestamps", stale)
					}
				}
				for _, query := range []string{"exampleword059", "commentretainedsentinel"} {
					page := f.find(t, org, searchstore.Query{Text: query})
					if len(page.Items) != 1 || page.Items[0].Document.Revision != d.Revision || !page.Items[0].Document.UpdatedAt.Equal(d.UpdatedAt) {
						t.Fatal("reused publication lost current source identity or comment content")
					}
				}
				if mode == "literal source change" {
					if page := f.find(t, org, searchstore.Query{Text: "path+name"}); len(page.Items) != 1 {
						t.Fatal("changed punctuation is not searchable")
					}
					var stale int64
					if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM search_postings WHERE organization_id=$1 AND document_id=$2 AND term=ANY($3::text[]) AND cardinality(literal_positions)>0`, org, d.ID, staleLiterals).Scan(&stale); err != nil {
						t.Fatal(err)
					}
					if len(staleLiterals) == 0 || stale != 0 {
						t.Fatal("stale punctuation positions remain on metadata postings")
					}
				}
			})
		}
	}
}

func TestSearchPublicationAllowsCurrentWritesDuringPostingIO(t *testing.T) {
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, mode := range []string{"source revision", "visibility ABA", "duplicate publisher", "metadata change"} {
			t.Run(string(org)+"/"+mode, func(t *testing.T) {
				f := newSearchFixture(t)
				f.admin.SetMaxOpenConns(3)
				var database string
				if err := f.admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 5"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=false,actor_principal_id='reporter_a' WHERE organization_id=$1 AND native_id='wi_first_1'`, org); err != nil {
					t.Fatal(err)
				}
				if mode == "metadata change" {
					if _, err := f.admin.ExecContext(t.Context(), `UPDATE projects SET name='originalmeta' WHERE organization_id=$1 AND id='prj_first'`, org); err != nil {
						t.Fatal(err)
					}
				}
				d := searchIssue(org, "wi_first_1")
				f.put(t, d, searchstore.Text{Title: "originalsentinel", Body: "original literal sequence"}, 0)
				f.drain(t, org)
				d.Revision = 2
				text := searchstore.Text{Title: "stalesentinel", Body: "stale literal sequence"}
				f.put(t, d, text, 1)
				if err := f.repository.ReadSearch(t.Context(), org, corestore.Background, func(r searchstore.Reader) error { var err error; d, err = r.PendingSearch(t.Context()); return err }); err != nil {
					t.Fatal(err)
				}
				prepared := searchstore.Prepared{Document: d, Vector: searchstore.Vector(d, text), Statistics: searchstore.Describe(d, text), Literals: searchstore.LiteralPostings(d, text)}
				next := d
				next.Revision++
				nextText := searchstore.Text{Title: "currentsentinel", Body: "current literal sequence"}
				next, err := f.content.Prepare(t.Context(), next, nextText)
				if err != nil {
					t.Fatal(err)
				}
				operation := "DELETE"
				if mode == "visibility ABA" || mode == "metadata change" {
					operation = "INSERT"
				}
				_, err = f.admin.ExecContext(t.Context(), `CREATE FUNCTION search_publication_test_barrier() RETURNS trigger LANGUAGE plpgsql AS $fn$
DECLARE posting public.search_postings; phase text;
BEGIN
 IF TG_OP='DELETE' THEN posting:=OLD; ELSE posting:=NEW; END IF;
 IF posting.organization_id=TG_ARGV[0] AND posting.document_id=TG_ARGV[1] AND posting.term NOT IN ('~all','~pending') AND TG_OP=TG_ARGV[2] THEN
  phase:=CASE WHEN TG_OP='DELETE' OR coalesce(array_length(posting.literal_positions,1),0)=0 THEN 'words' ELSE 'literals' END;
  PERFORM pg_advisory_xact_lock(hashtext(current_database()),hashtext(TG_ARGV[0]||TG_ARGV[1]||phase)&2147483647);
 END IF;
 RETURN posting;
END $fn$`)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER search_publication_test_barrier BEFORE DELETE OR INSERT ON search_postings FOR EACH ROW EXECUTE FUNCTION search_publication_test_barrier('%s','%s','%s')`, org, d.ID, operation)); err != nil {
					t.Fatal(err)
				}
				conn, err := f.admin.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				held := map[string]bool{}
				var workers sync.WaitGroup
				unlock := func(phase string) {
					if !held[phase] {
						return
					}
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
					defer cancel()
					if _, err := conn.ExecContext(cleanup, `SELECT pg_advisory_unlock(hashtext(current_database()),hashtext($1)&2147483647)`, string(org)+d.ID+phase); err != nil {
						t.Error(err)
					}
					held[phase] = false
				}
				t.Cleanup(func() { unlock("words"); unlock("literals"); conn.Close(); workers.Wait() })
				for _, phase := range []string{"words", "literals"} {
					if _, err := conn.ExecContext(t.Context(), `SELECT pg_advisory_lock(hashtext(current_database()),hashtext($1)&2147483647)`, string(org)+d.ID+phase); err != nil {
						t.Fatal(err)
					}
					held[phase] = true
				}
				type publication struct {
					published bool
					err       error
				}
				published := make(chan publication, 1)
				waitBlocked := func(phase string) {
					t.Helper()
					testenv.WaitUntil(t, t.Context(), func() error {
						select {
						case result := <-published:
							t.Fatalf("publication ended before %s barrier: published=%t error=%v", phase, result.published, result.err)
						default:
						}
						var blocked bool
						err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=hashtext(current_database())::oid AND objid=(hashtext($1)&2147483647)::oid)`, string(org)+d.ID+phase).Scan(&blocked)
						if err != nil {
							return err
						}
						if !blocked {
							return errors.New("publication has not reached the posting barrier")
						}
						return nil
					})
				}
				workers.Go(func() {
					var ok bool
					err := f.repository.WriteSearch(t.Context(), org, corestore.Background, func(w searchstore.Writer) error {
						var err error
						ok, err = w.PublishSearch(t.Context(), prepared)
						return err
					})
					published <- publication{ok, err}
				})
				waitBlocked("words")

				if mode == "duplicate publisher" {
					other, err := postgres.NewRepository(t.Context(), f.pool, smallPool())
					if err != nil {
						t.Fatal(err)
					}
					duplicate := make(chan publication, 1)
					workers.Go(func() {
						var ok bool
						err := other.WriteSearch(t.Context(), org, corestore.Background, func(w searchstore.Writer) error {
							var err error
							ok, err = w.PublishSearch(t.Context(), prepared)
							return err
						})
						duplicate <- publication{ok, err}
					})
					testenv.WaitUntil(t, t.Context(), func() error {
						var waiting bool
						if err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND wait_event<>'advisory' AND (query LIKE '-- name: SearchPreparePublicationPostings%' OR query LIKE '-- name: SearchPublish :execrows%'))`).Scan(&waiting); err != nil {
							return err
						}
						if !waiting {
							return errors.New("duplicate publication has not reached existing postings")
						}
						return nil
					})
					unlock("words")
					unlock("literals")
					first, second := <-published, <-duplicate
					if first.err != nil || second.err != nil {
						t.Fatalf("duplicate publications failed: first=%v second=%v", first.err, second.err)
					}
					if !first.published && !second.published {
						t.Fatal("neither duplicate publication committed")
					}
					var revision, indexed int64
					if err := f.admin.QueryRowContext(t.Context(), `SELECT revision,indexed_revision FROM search_documents WHERE organization_id=$1 AND id=$2`, org, d.ID).Scan(&revision, &indexed); err != nil {
						t.Fatal(err)
					}
					if revision != 2 || indexed != 2 {
						t.Fatal("duplicate publication mixed generations")
					}
					if got := f.find(t, org, searchstore.Query{Text: "stalesentinel"}); len(got.Items) != 1 {
						t.Fatal("duplicate publication lost current terms")
					}
					if got := f.find(t, org, searchstore.Query{Text: "stale literal sequence", Exact: true}); len(got.Items) != 1 {
						t.Fatal("duplicate publication lost current literals")
					}
					if got := f.find(t, org, searchstore.Query{Text: "originalsentinel"}); len(got.Items) != 0 {
						t.Fatal("duplicate publication retained stale terms")
					}
					return
				}
				currentWrite := make(chan error, 1)
				workers.Go(func() {
					if mode == "source revision" {
						currentWrite <- f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error { return w.PutSearch(t.Context(), next, d.Revision) })
						return
					}
					if mode == "metadata change" {
						tx, err := f.admin.BeginTx(t.Context(), nil)
						if err != nil {
							currentWrite <- err
							return
						}
						defer tx.Rollback()
						if _, err = tx.ExecContext(t.Context(), `UPDATE projects SET name='currentmeta' WHERE organization_id=$1 AND id='prj_first'`, org); err == nil {
							_, err = tx.ExecContext(t.Context(), `UPDATE issues SET number=99 WHERE organization_id=$1 AND native_id='wi_first_1'`, org)
						}
						if err == nil {
							err = tx.Commit()
						}
						currentWrite <- err
						return
					}
					tx, err := f.admin.BeginTx(t.Context(), nil)
					if err != nil {
						currentWrite <- err
						return
					}
					defer tx.Rollback()
					if _, err = tx.ExecContext(t.Context(), `UPDATE issues SET archived=true,actor_principal_id='reporter_b' WHERE organization_id=$1 AND native_id='wi_first_1'`, org); err == nil {
						_, err = tx.ExecContext(t.Context(), `UPDATE search_documents SET updated_at=updated_at WHERE organization_id=$1 AND id=$2`, org, d.ID)
					}
					if err == nil {
						err = tx.Commit()
					}
					currentWrite <- err
				})
				testenv.WaitUntil(t, t.Context(), func() error {
					select {
					case err := <-currentWrite:
						if err != nil {
							t.Fatal(err)
						}
						return nil
					default:
					}
					var blocked bool
					if err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND cardinality(pg_blocking_pids(pid))>0 AND (query LIKE '-- name: SearchUpdate :execrows%' OR query LIKE 'UPDATE search_documents SET updated_at=updated_at%'))`).Scan(&blocked); err != nil {
						return err
					}
					if blocked {
						t.Fatal("current source write waits behind background posting IO")
					}
					return errors.New("current source write has not completed")
				})
				unlock("words")
				if mode == "visibility ABA" {
					waitBlocked("literals")
					if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=false,actor_principal_id='reporter_a' WHERE organization_id=$1 AND native_id='wi_first_1'`, org); err != nil {
						t.Fatal(err)
					}
				}
				unlock("literals")
				result := <-published
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.published != (mode == "visibility ABA") {
					t.Fatalf("publication=%t for %s", result.published, mode)
				}
				if mode == "metadata change" {
					var indexed int64
					if err := f.admin.QueryRowContext(t.Context(), `SELECT indexed_revision FROM search_documents WHERE organization_id=$1 AND id=$2`, org, d.ID).Scan(&indexed); err != nil {
						t.Fatal(err)
					}
					if indexed != 0 {
						t.Fatal("stale parent metadata publication committed")
					}
					for _, indexed := range []bool{false, true} {
						if indexed {
							f.drain(t, org)
						}
						if got := f.find(t, org, searchstore.Query{Text: "currentmeta#99"}); len(got.Items) != 1 {
							t.Fatal("current typed parent identity is not searchable")
						}
						if got := f.find(t, org, searchstore.Query{Text: "originalmeta#1"}); len(got.Items) != 0 {
							t.Fatal("stale typed parent identity is searchable")
						}
					}
					return
				}
				if mode == "source revision" {
					var revision, indexed int64
					if err := f.admin.QueryRowContext(t.Context(), `SELECT revision,indexed_revision FROM search_documents WHERE organization_id=$1 AND id=$2`, org, d.ID).Scan(&revision, &indexed); err != nil {
						t.Fatal(err)
					}
					if revision != 3 || indexed != 0 {
						t.Fatalf("stale publication changed current revision: revision=%d indexed=%d", revision, indexed)
					}
					for _, indexed := range []bool{false, true} {
						if indexed {
							f.drain(t, org)
						}
						if got := f.find(t, org, searchstore.Query{Text: "stalesentinel"}); len(got.Items) != 0 {
							t.Fatal("stale publication is searchable")
						}
						if got := f.find(t, org, searchstore.Query{Text: "currentsentinel"}); len(got.Items) != 1 {
							t.Fatal("current source is not searchable")
						}
						if got := f.find(t, org, searchstore.Query{Text: "current literal sequence", Exact: true}); len(got.Items) != 1 {
							t.Fatal("current literal is not searchable")
						}
					}
					return
				}
				var wrongWords, wrongLiterals int
				if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FILTER(WHERE cardinality(literal_positions)=0),count(*) FILTER(WHERE cardinality(literal_positions)>0) FROM search_postings WHERE organization_id=$1 AND document_id=$2 AND term NOT IN ('~all','~pending') AND NOT (scope=ANY(ARRAY(SELECT search_scope_keys('prj_first','wi_first_1','issue','issue','',false,'reporter_a'))))`, org, d.ID).Scan(&wrongWords, &wrongLiterals); err != nil {
					t.Fatal(err)
				}
				if wrongWords != 0 || wrongLiterals != 0 {
					t.Fatalf("publication mixed visibility snapshots: words=%d literals=%d", wrongWords, wrongLiterals)
				}
				reporterA, reporterB := "reporter_a", "reporter_b"
				for _, exact := range []bool{false, true} {
					query := "stalesentinel"
					if exact {
						query = "stale literal sequence"
					}
					if got := f.find(t, org, searchstore.Query{Text: query, Exact: exact, Reporter: &reporterA}); len(got.Items) != 1 {
						t.Fatal("current reporter cannot search captured content")
					}
					if got := f.find(t, org, searchstore.Query{Text: query, Exact: exact, Reporter: &reporterB, Archived: "all"}); len(got.Items) != 0 {
						t.Fatal("intermediate reporter can search captured content")
					}
				}
			})
		}
	}
}
