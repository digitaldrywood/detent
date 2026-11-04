package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedDuplicateSources(t *testing.T, f searchFixture) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,operator_only,board_rank) SELECT organization_id,id,'Todo',false,true,false,0 FROM projects; UPDATE issues SET workflow_state_name='Todo'`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []string{"first", "second"} {
			for number := 1; number <= 2; number++ {
				item := fmt.Sprintf("wi_%s_%d", project, number)
				d := searchIssue(org, item)
				d.ProjectID = tracker.ProjectID("prj_" + project)
				f.put(t, d, searchstore.Text{Title: "Example title", Body: fmt.Sprintf("body %s %d", org, number)}, 0)
				for sequence := 1; sequence <= 2; sequence++ {
					id := fmt.Sprintf("comment_%s_%d_%d", project, number, sequence)
					comment := d
					comment.ID = "comment/" + id
					comment.Kind = "comment"
					comment.SourceID = id
					f.put(t, comment, searchstore.Text{Body: fmt.Sprintf("comment %s %d", org, sequence)}, 0)
				}
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		f.drain(t, org)
	}
}

func duplicateState(t *testing.T, f searchFixture, org tracker.OrganizationID) searchstore.DuplicateSourceState {
	t.Helper()
	var state searchstore.DuplicateSourceState
	err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
		var err error
		state, err = r.(searchstore.DuplicateSourceReader).DuplicateSourceState(t.Context(), "prj_first")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestDuplicateSourceFenceRejectsChangedVisibilityAndSerializesCandidateMutation(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	var noMatch searchstore.DuplicateSourcePage
	if err := f.repository.ReadSearch(t.Context(), "org_first", corestore.Serving, func(r searchstore.Reader) error {
		var err error
		noMatch, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "absentmarker", Now: historyAt()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(noMatch.Sources) != 0 || !noMatch.Exhausted {
		t.Fatal("no-match fixture returned candidates")
	}
	state := duplicateState(t, f, "org_first")
	second := duplicateState(t, f, "org_second")
	if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=true WHERE organization_id='org_first' AND project_id='prj_first' AND native_id='wi_first_1'`); err != nil {
		t.Fatal(err)
	}
	if duplicateState(t, f, "org_second") != second {
		t.Fatal("source fence crossed tenant boundary")
	}
	err := f.repository.WriteSearch(t.Context(), "org_first", corestore.Serving, func(w searchstore.Writer) error {
		return w.(searchstore.DuplicateSourceWriter).LockDuplicateSource(t.Context(), "prj_first", state.Sequence)
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale visibility fence=%v", err)
	}
	f.drain(t, "org_first")
	state = duplicateState(t, f, "org_first")
	f.admin.SetMaxOpenConns(3)
	competing, err := f.admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer competing.Close()
	var pid int
	if err := competing.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	joined := make(chan struct{})
	err = f.repository.WriteSearch(t.Context(), "org_first", corestore.Serving, func(w searchstore.Writer) error {
		if err := w.(searchstore.DuplicateSourceWriter).LockDuplicateSource(t.Context(), "prj_first", state.Sequence); err != nil {
			return err
		}
		go func() {
			defer close(joined)
			close(started)
			_, err := competing.ExecContext(t.Context(), `UPDATE issues SET actor_principal_id='other' WHERE organization_id='org_first' AND project_id='prj_first' AND native_id='wi_first_2'`)
			finished <- err
		}()
		t.Cleanup(func() { <-joined })
		<-started
		testenv.WaitUntil(t, t.Context(), func() error {
			var waiting bool
			if err := f.admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&waiting); err != nil {
				return err
			}
			if !waiting {
				return errors.New("source writer has not reached project fence")
			}
			return nil
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	err = f.repository.WriteSearch(context.WithoutCancel(t.Context()), "org_first", corestore.Serving, func(w searchstore.Writer) error {
		return w.(searchstore.DuplicateSourceWriter).LockDuplicateSource(t.Context(), "prj_first", state.Sequence)
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("post-commit source mutation was not fenced: %v", err)
	}
}

func TestDuplicateSourceCandidatesRetainSourceRankingAndLiteralIdentity(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for number := 0; number < 130; number++ {
			id := fmt.Sprintf("extra_%03d", number)
			body := "unrelated background content"
			if number == 129 {
				body = "unique-731 marker"
			}
			err := f.repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
				_, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: org, ID: id, ProjectID: "prj_first", WorkItemID: "wi_first_1", Sequence: int64(number + 3), BodyObjectKey: "example/comment", ActorKind: "human", ActorPrincipalID: "example"})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			d := searchIssue(org, "wi_first_1")
			d.ID = "comment/z" + id
			d.Kind = "comment"
			d.SourceID = id
			f.put(t, d, searchstore.Text{Body: body}, 0)
		}
		f.drain(t, org)
		state := duplicateState(t, f, org)
		if !state.Ready {
			t.Fatal("source corpus is not ready")
		}
		f.drain(t, org)
		var page searchstore.DuplicateSourcePage
		err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
			var err error
			page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "unique-731", Exact: true, Sequence: &state.Sequence, Now: historyAt()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		found := page.Sources
		if !page.Exhausted || page.NextID != "" || len(found) != 1 || found[0].Document.SourceID != "extra_129" {
			t.Fatal("one query lost matching source beyond 128 unrelated rows")
		}
		source := found[0]
		text, err := f.content.Read(t.Context(), source.Document)
		if err != nil {
			t.Fatal(err)
		}
		if !searchstore.MatchDuplicateLiteral(text, "unique-731") || searchstore.MatchDuplicateLiteral(text, "unique 731") {
			t.Fatal("literal punctuation identity changed")
		}
		match, ok := searchstore.MatchDuplicateSource(source, text, searchstore.Text{}, searchstore.Text{Title: "unique marker"})
		if !ok || match.Score != float64(2)/float64(4) {
			t.Fatalf("per-source score=%v matched=%t", match.Score, ok)
		}
	}
}

func TestDuplicateSourcesSkipDeletedUnpublishedComments(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_comments SET deleted=true,revision=revision+1 WHERE organization_id=$1 AND id='comment_first_1_1'`, org); err != nil {
			t.Fatal(err)
		}

		f.drain(t, org)
		state := duplicateState(t, f, org)
		if !state.Ready {
			t.Fatal("deleted unpublished source kept readiness pending")
		}
		f.drain(t, org)
		var page searchstore.DuplicateSourcePage
		err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
			var err error
			page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "comment", Sequence: &state.Sequence, Limit: 128, Now: historyAt()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range page.Sources {
			if source.Document.SourceID == "comment_first_1_1" {
				t.Fatal("deleted comment remained a duplicate candidate")
			}
		}
	}
}

func TestDuplicateSourceCommentFingerprintUsesParentIssueContext(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	diagnostic := "duplicate migration version 20261006023000"
	request := searchstore.Text{Body: issueorigin.Stamp(diagnostic, issueorigin.Origin{Kind: "worker", Fingerprint: issueorigin.DefectFingerprint(diagnostic)})}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET revision=2,body_object_key='example/parent2' WHERE organization_id=$1 AND native_id='wi_first_1';`, org); err != nil {
			t.Fatal(err)
		}
		parent := searchIssue(org, "wi_first_1")
		parent.Revision = 2
		f.put(t, parent, searchstore.Text{Title: "Example title", Body: issueorigin.Stamp("Previous report", issueorigin.Origin{Kind: "worker", Fingerprint: "previous"}) + "\n" + strings.Repeat("padding ", 10000)}, 1)
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_comments SET revision=2,body_object_key='example/comment2' WHERE organization_id=$1 AND id='comment_first_1_1'`, org); err != nil {
			t.Fatal(err)
		}
		comment := searchIssue(org, "wi_first_1")
		comment.ID = "comment/comment_first_1_1"
		comment.Kind = "comment"
		comment.SourceID = "comment_first_1_1"
		comment.Revision = 2
		f.put(t, comment, searchstore.Text{Body: diagnostic}, 1)

		f.drain(t, org)
		var page searchstore.DuplicateSourcePage
		err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
			var err error
			page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: diagnostic, Limit: 128, Now: historyAt()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		matched := false
		for _, source := range page.Sources {
			if source.Document.Kind != "comment" {
				continue
			}
			text, err := f.content.Read(t.Context(), source.Document)
			if err != nil {
				t.Fatal(err)
			}
			var aggregate searchstore.Document
			if err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
				hits, err := r.Search(t.Context(), searchstore.Query{ProjectID: "prj_first", Kind: "work", Text: "Previous report", Archived: "false"})
				if err == nil && len(hits) > 0 {
					aggregate = hits[0].Document
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			aggregateText, err := f.content.Read(t.Context(), aggregate)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(aggregateText.Body, diagnostic) {
				t.Fatal("late fingerprint unexpectedly fit aggregate fixture")
			}
			parentText, err := f.content.Read(t.Context(), source.Parent)
			if err != nil {
				t.Fatal(err)
			}
			result, ok := searchstore.MatchDuplicateSource(source, text, parentText, request)
			matched = matched || ok && result.Fingerprint
		}
		if !matched {
			t.Fatal("unstamped diagnostic comment lost the parent worker fingerprint context")
		}
	}
}

func TestDuplicateSourceVisibilityAndTerminalBoundary(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,operator_only,board_rank) SELECT organization_id,id,'Done',true,false,false,1 FROM projects`); err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {

		for _, test := range []struct {
			name     string
			archived bool
			state    string
			created  time.Time
			reporter string
			want     int
		}{
			{"owner", false, "Todo", historyAt(), "example", 2},
			{"unknown workflow", false, "", historyAt(), "example", 1},
			{"other principal", false, "Todo", historyAt(), "other", 0},
			{"archive", true, "Todo", historyAt(), "example", 1},
			{"terminal boundary", false, "Done", historyAt().AddDate(0, 0, -30), "example", 2},
			{"outside terminal window", false, "Done", historyAt().AddDate(0, 0, -30).Add(-time.Microsecond), "example", 1},
		} {
			t.Run(string(org)+"/"+test.name, func(t *testing.T) {
				previous := duplicateState(t, f, org)
				if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=$2,workflow_state_name=NULLIF($3,''),native_created_at=$4 WHERE organization_id=$1 AND native_id='wi_first_1'`, org, test.archived, test.state, test.created); err != nil {
					t.Fatal(err)
				}
				f.drain(t, org)
				current := duplicateState(t, f, org)
				if current.Sequence <= previous.Sequence {
					t.Fatal("candidate window mutation did not advance fence")
				}
				var page searchstore.DuplicateSourcePage
				err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
					var err error
					page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "Example body comment", Reporter: &test.reporter, Limit: 128, Now: historyAt()})
					return err
				})
				if test.want < 0 {
					if !errors.Is(err, searchstore.ErrIndexPending) {
						t.Fatalf("unknown workflow became no-match: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Sources) != test.want {
					t.Fatalf("visible sources=%d want %d", len(page.Sources), test.want)
				}
			})
		}
	}
}

func TestDuplicateSourceNoMatchFenceRejectsConcurrentMatchingCreate(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {

		f.drain(t, org)
		state := duplicateState(t, f, org)
		f.drain(t, org)
		var page searchstore.DuplicateSourcePage
		err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
			var err error
			page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "novel sentinel", Sequence: &state.Sequence, Limit: 128, Now: historyAt()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Sources) != 0 || !page.Exhausted {
			t.Fatal("fixture did not establish exhaustive fresh no-match")
		}
		document := searchIssue(org, "wi_first_3")
		document.UpdatedAt = historyAt()
		prepared, err := f.content.Prepare(t.Context(), document, searchstore.Text{Title: "novel sentinel", Body: "matching payload"})
		if err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			finished <- f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, NativeID: "wi_first_3", ProjectID: "prj_first", Number: 3, Title: "novel sentinel", BodyObjectKey: new("example/new-body"), IssueContractObjectKey: new("example/contract"), ActorKind: new("human"), ActorPrincipalID: new("example"), WorkflowStateName: new("Todo")})
				if err != nil {
					return err
				}
				return w.PutSearch(t.Context(), prepared, 0)
			})
		}()
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		err = f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error {
			if err := w.(searchstore.DuplicateSourceWriter).LockDuplicateSource(t.Context(), "prj_first", state.Sequence); err != nil {
				return err
			}
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, NativeID: "wi_first_4", ProjectID: "prj_first", Number: 4, Title: "novel sentinel"})
			return err
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) && !errors.Is(err, searchstore.ErrIndexPending) {
			t.Fatalf("fresh no-match fence accepted changed source: %v", err)
		}
		err = f.repository.Read(t.Context(), org, corestore.Serving, func(r corestore.Reader) error {
			_, err := r.WorkItem(t.Context(), "prj_first", "wi_first_4")
			return err
		})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("stale no-match created another issue: %v", err)
		}
	}
}

func TestDuplicateSourceTopFiveFiltersBeforeLimitAndReadsCurrentPayload(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for number := 3; number <= 16; number++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_rank_%d", number))
			err := f.repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
				_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, NativeID: id, ProjectID: "prj_first", Number: int64(number), Title: "rankmarker", BodyObjectKey: new("example/body"), IssueContractObjectKey: new("example/contract"), ActorKind: new("human"), ActorPrincipalID: new("example"), WorkflowStateName: new("Todo")})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			title := "rankmarker retained"
			if number > 10 {
				title = "rankmarker rankmarker rankmarker rankmarker rankmarker rankmarker"
			}
			f.put(t, searchIssue(org, string(id)), searchstore.Text{Title: title, Body: "current immutable payload"}, 0)
		}
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET archived=(native_id='wi_rank_10'),actor_principal_id=CASE WHEN native_id='wi_rank_9' THEN 'other' ELSE actor_principal_id END WHERE organization_id=$1 AND native_id IN ('wi_rank_10','wi_rank_9')`, org); err != nil {
			t.Fatal(err)
		}

		f.drain(t, org)
		var page searchstore.DuplicateSourcePage
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE search_documents SET vector=ts_delete(vector,'~complete') WHERE organization_id=$1 AND kind='issue' AND source_id IN ('wi_rank_11','wi_rank_12','wi_rank_13','wi_rank_14','wi_rank_15','wi_rank_16')`, org); err != nil {
			t.Fatal(err)
		}
		read := func() error {
			return f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
				var err error
				page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "rankmarker", Request: &searchstore.Text{Title: "rankmarker retained"}, Reporter: new("example"), Now: historyAt()})
				return err
			})
		}
		if err := read(); err != nil {
			t.Fatal(err)
		}
		if len(page.Sources) != 5 || !page.Exhausted {
			t.Fatalf("bounded current visible hits=%d", len(page.Sources))
		}
		for index, source := range page.Sources {
			if source.Number != int64(8-index) {
				t.Fatalf("ranked visible issue=%d want=%d", source.Number, 8-index)
			}
			text, err := f.content.Read(t.Context(), source.Document)
			if err != nil {
				t.Fatal(err)
			}
			if text.Body != "current immutable payload" {
				t.Fatal("candidate did not carry current payload")
			}
			if _, ok := searchstore.MatchDuplicateSource(source, text, text, searchstore.Text{Title: "rankmarker"}); !ok {
				t.Fatal("current source failed original token challenge semantics")
			}
		}
		previous := page.State
		if _, err := f.admin.ExecContext(t.Context(), `UPDATE issues SET revision=2,body_object_key='example/updated' WHERE organization_id=$1 AND native_id='wi_rank_8'`, org); err != nil {
			t.Fatal(err)
		}
		if err := read(); !errors.Is(err, searchstore.ErrIndexPending) {
			t.Fatalf("dirty canonical source became a no-match: %v", err)
		}
		document := searchIssue(org, "wi_rank_8")
		document.Revision = 2
		f.put(t, document, searchstore.Text{Title: "changed", Body: "replacement payload"}, 1)
		f.drain(t, org)
		if err := read(); err != nil {
			t.Fatal(err)
		}
		for _, source := range page.Sources {
			if source.Document.SourceID == "wi_rank_8" {
				t.Fatal("stale source vector returned replaced payload")
			}
		}
		err := f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error {
			return w.(searchstore.DuplicateSourceWriter).LockDuplicateSource(t.Context(), "prj_first", previous.Sequence)
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("replaced current payload accepted stale fence: %v", err)
		}
	}
}

func TestDuplicateSourcePendingRetainsFilingFallback(t *testing.T) {
	for _, pending := range []string{"dirty", "rebuild"} {
		t.Run(pending, func(t *testing.T) {
			f := newSearchFixture(t)
			seedDuplicateSources(t, f)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO project_settings(organization_id,project_id,duplicate_challenge) VALUES($1,'prj_first',true)`, org); err != nil {
					t.Fatal(err)
				}
				if pending == "dirty" {
					if err := f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error { return w.AdvanceSearch(t.Context(), searchIssue(org, "wi_first_1")) }); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id) VALUES($1,'prj_first','',false,'')`, org); err != nil {
						t.Fatal(err)
					}
				}
				state := duplicateState(t, f, org)
				if state.Ready {
					t.Fatal("pending search reported available")
				}
				for _, machine := range []bool{false, true} {
					query := searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "Example", Machine: machine, Now: historyAt()}
					check := func(r searchstore.Reader) error {
						page, err := r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), query)
						if !errors.Is(err, searchstore.ErrIndexPending) || page.State.Ready || page.Exhausted || len(page.Sources) != 0 {
							t.Fatalf("unavailable page=%+v error=%v", page, err)
						}
						return nil
					}
					if err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, check); err != nil {
						t.Fatal(err)
					}
					if err := f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error { return check(w) }); err != nil {
						t.Fatal(err)
					}
					number := int64(3)
					if machine {
						number = 4
					}
					id := tracker.NativeWorkItemID(fmt.Sprintf("fallback_%d", number))
					if err := f.repository.WriteSearch(t.Context(), org, corestore.Serving, func(w searchstore.Writer) error {
						err := w.(searchstore.DuplicateSourceWriter).LockDuplicateSource(t.Context(), "prj_first", state.Sequence)
						if !errors.Is(err, searchstore.ErrIndexPending) {
							t.Fatalf("prepared pending fence=%v", err)
						}
						_, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_first", NativeID: id, Number: number, Title: "Example"})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if err := f.repository.Read(t.Context(), org, corestore.Serving, func(r corestore.Reader) error { _, err := r.WorkItem(t.Context(), "prj_first", id); return err }); err != nil {
						t.Fatal(err)
					}
					state = duplicateState(t, f, org)
				}
				var enabled bool
				if err := f.admin.QueryRowContext(t.Context(), `SELECT duplicate_challenge FROM project_settings WHERE organization_id=$1 AND project_id='prj_first'`, org).Scan(&enabled); err != nil || !enabled {
					t.Fatalf("fallback changed duplicate policy: enabled=%t error=%v", enabled, err)
				}
			}
		})
	}
}

func TestDuplicateSourceUsesExistingAggregateHintsAndImmutableMachineSource(t *testing.T) {
	f := newSearchFixture(t)
	seedDuplicateSources(t, f)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		event := searchIssue(org, "wi_first_1")
		event.ID = "event/example"
		event.Kind = "event"
		event.SourceID = "example"
		record := historyEvent(t, eventstore.NewContent(f.objects), org, "example")
		if err := f.repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.AppendEvent(t.Context(), record); return err }); err != nil {
			t.Fatal(err)
		}
		f.put(t, event, searchstore.Text{Body: "aggregatehint"}, 0)
		f.drain(t, org)
		for _, machine := range []bool{false, true} {
			var page searchstore.DuplicateSourcePage
			if err := f.repository.ReadSearch(t.Context(), org, corestore.Serving, func(r searchstore.Reader) error {
				var err error
				page, err = r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: "prj_first", Text: "aggregatehint", Machine: machine, Now: historyAt()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(page.Sources) != 1 || page.Sources[0].Document.Kind != "issue" {
				t.Fatalf("aggregate hint candidates=%+v", page.Sources)
			}
			source := page.Sources[0]
			text, err := f.content.Read(t.Context(), source.Document)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := f.content.Read(t.Context(), source.Parent)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(parent.Body, "aggregatehint") || strings.Contains(text.Body, "aggregatehint") == machine {
				t.Fatalf("machine=%t lost source/aggregate distinction", machine)
			}
			if !machine && source.Document.Payload == source.Parent.Payload {
				t.Fatal("human aggregate hint used issue-only payload")
			}
			source.Document.Payload.SHA256 = strings.Repeat("0", 64)
			if _, err := f.content.Read(t.Context(), source.Document); err == nil {
				t.Fatal("selected payload digest corruption was accepted")
			}
		}
	}
}
