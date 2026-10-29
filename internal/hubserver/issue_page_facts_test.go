package hubserver

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssuePageFactsAcrossBackfill(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "example-library")
	type entry struct {
		scope     nativeScope
		issue     tracker.NativeIssue
		commenter string
	}
	var entries []entry
	priority := 2
	for index, fixture := range []nativeFixture{f, other} {
		actor := tracker.Actor{Kind: "human", PrincipalID: fmt.Sprintf("example-actor-%d", index)}
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID, sourceActor: &actor, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		issue, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "Filterable issue", Body: issueContractTestSections, Type: "bug", State: "Todo", Labels: []string{"Example-Label", "EXAMPLE-LABEL"}, Assignees: []string{"Example-Assignee", "EXAMPLE-ASSIGNEE"}, Priority: &priority}, f.service.config.now())
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		issue.State = "Done"
		issue.Revision++
		issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Done", Reason: "example transition", Run: &tracker.NativeRunData{AttemptID: "example-attempt", FencingToken: 4}}, f.service.config.now())
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		commenter := tracker.Actor{Kind: "human", PrincipalID: fmt.Sprintf("example-commenter-%d", index)}
		commentScope := scope
		commentScope.sourceActor = &commenter
		if err := appendNativeHistory(t.Context(), tx, commentScope, string(issue.WorkItemID), "comment.created", tracker.CollaborationData{}, f.service.config.now()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{scope, issue, commenter.PrincipalID})
	}
	drainTestNativeBoard(t, f.service)
	for _, stage := range []string{"typed", "unknown filters", "unknown", "backfilled", "opaque payload differs"} {
		t.Run(stage, func(t *testing.T) {
			switch stage {
			case "unknown filters":
				for _, statement := range []string{"UPDATE issues SET page_filters_ready=NULL", "UPDATE native_board_items SET page_filters_ready=NULL", "DELETE FROM native_issue_page_filter_values", "DELETE FROM native_board_filter_values", "INSERT INTO native_board_filter_values(work_item_id,kind,value) SELECT i.native_id,v.kind,v.value FROM issues i JOIN issue_filter_values v ON v.issue_id=i.id"} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			case "unknown":
				for _, statement := range []string{"UPDATE issues SET actor_kind=NULL,actor_principal_id=NULL,page_filters_ready=NULL", "UPDATE collaboration_events SET page_projection_ready=NULL,event_revision=NULL,from_state=NULL,to_state=NULL", "UPDATE workflow_states SET board_rank=NULL", "UPDATE native_board_items SET actor_principal_id=NULL,issue_type=NULL,verification_state=NULL,priority=NULL,page_filters_ready=NULL", "DELETE FROM native_board_filter_values", "DELETE FROM native_issue_page_filter_values"} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			case "backfilled":
				if err := f.service.convertIssuePageFacts(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := f.service.convertNativeBoardRanks(t.Context()); err != nil {
					t.Fatal(err)
				}
				var unknown int
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM issues WHERE actor_principal_id IS NULL OR page_filters_ready IS NULL)+(SELECT count(*) FROM collaboration_events WHERE page_projection_ready IS NULL)+(SELECT count(*) FROM native_board_items WHERE actor_principal_id IS NULL OR page_filters_ready IS NULL)+(SELECT count(*) FROM workflow_states WHERE board_rank IS NULL)`).Scan(&unknown); err != nil || unknown != 0 {
					t.Fatalf("unknown=%d err=%v", unknown, err)
				}
			case "opaque payload differs":
				for _, entry := range entries {
					raw, err := json.Marshal(tracker.Actor{Kind: "integration", PrincipalID: "opaque-actor"})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET actor_json=?,labels_json='[]',assignees_json='[]' WHERE native_id=?", string(raw), entry.issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
					card := tracker.NativeBoardCard{Issue: entry.issue}
					card.Issue.Actor.PrincipalID = "opaque-actor"
					card.Issue.Type = "change_request"
					card.Issue.Labels = nil
					card.Issue.Assignees = nil
					card.Issue.Priority = nil
					card.Issue.Verification = nil
					payload, err := json.Marshal(card)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_items SET record_json=? WHERE work_item_id=?", string(payload), entry.issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, entry := range entries {
				event := issueEmailEvent{item: string(entries[0].issue.WorkItemID), project: string(entries[0].scope.project), sequence: 100, at: f.service.config.now()}
				participants, next, err := readIssueEmailParticipants(t.Context(), f.service.database.reader, event, string(entry.issue.WorkItemID), string(entry.scope.organization), "", 10)
				if err != nil || next != "" || len(participants) != 2 || !participants[entry.issue.Actor.PrincipalID] {
					t.Fatalf("project %s participants=%v next=%s err=%v", entry.scope.project, participants, next, err)
				}
				if filer, exists := participants[entry.commenter]; !exists || filer {
					t.Fatalf("project %s commenter missing or marked as filer: %v", entry.scope.project, participants)
				}
				for _, filter := range []struct {
					name, value string
					want        int
				}{{"created_by", entry.issue.Actor.PrincipalID, 1}, {"type", "bug", 1}, {"verification", "pending", 1}, {"label", "Example-Label", 1}, {"label", "EXAMPLE-LABEL", 1}, {"assignee", "Example-Assignee", 1}, {"assignee", "EXAMPLE-ASSIGNEE", 1}, {"priority", "2", 1}, {"q", "example-label", 1}, {"label", "example-label", 0}, {"assignee", "example-assignee", 0}, {"label", " Example-Label ", 0}, {"assignee", " Example-Assignee ", 0}} {
					params := url.Values{filter.name: {filter.value}}
					want := filter.want
					query, args, err := nativeIssueSelection(entry.scope, params, false)
					if err != nil {
						t.Fatal(err)
					}
					q, err := prepareNativePageQuery(t.Context(), f.service.database.reader, entry.scope, "", "native_issue_actors", "native_transition_facts", "native_workflow_ranks", "native_board_filter_facts", "native_issue_page_filter_values")
					if err != nil {
						t.Fatal(err)
					}
					ids, err := nativePageIDs(t.Context(), q, query, args...)
					if err != nil || len(ids) != want || want == 1 && ids[0] != string(entry.issue.WorkItemID) {
						t.Errorf("issue %s=%q: ids=%v want=%d err=%v", filter.name, filter.value, ids, want, err)
					}
					where, args, err := nativeBoardFilter(entry.scope, params, true)
					if err != nil {
						t.Fatal(err)
					}
					ids, err = nativePageIDs(t.Context(), q, "SELECT b.work_item_id FROM native_board_items b WHERE "+where, args...)
					if err != nil || len(ids) != want || want == 1 && ids[0] != string(entry.issue.WorkItemID) {
						t.Errorf("board %s=%q: ids=%v want=%d err=%v", filter.name, filter.value, ids, want, err)
					}
				}
				for _, filter := range []struct{ name, value string }{{"created_by", entry.issue.Actor.PrincipalID}, {"type", "bug"}, {"label", "Example-Label"}, {"assignee", "Example-Assignee"}, {"reporter", ""}, {"reporter foreign", ""}} {
					scope := entry.scope
					params := url.Values{"completed_window": {"1h"}}
					want := 1
					if strings.HasPrefix(filter.name, "reporter") {
						scope.credential.HostedRole = "reporter"
						if filter.name == "reporter foreign" {
							scope.sourceActor = &tracker.Actor{Kind: "human", PrincipalID: "another-actor"}
							want = 0
						}
					} else {
						params.Set(filter.name, filter.value)
					}
					summary, err := readNativeBoardSummary(t.Context(), f.service.database.reader, scope, params, f.service.config.now().Add(time.Second))
					if err != nil {
						t.Fatal(err)
					}
					if len(summary.Lanes) != want || want == 1 && summary.Lanes[0].Total != 1 || summary.Completed != want || summary.Cancelled != 0 || len(summary.ShippedWindows) != 3 {
						t.Errorf("%s summary=%+v", filter.name, summary)
					}
					for _, window := range summary.ShippedWindows {
						if window.Shipped != want || window.Cancelled != 0 {
							t.Errorf("%s window=%+v", filter.name, window)
						}
					}
				}
				reporter := entry.scope
				reporter.credential.HostedRole = "reporter"
				if err := requireReporterItem(t.Context(), f.service.database.reader, reporter, string(entry.issue.WorkItemID)); err != nil {
					t.Fatal(err)
				}
				intruder := tracker.Actor{Kind: "human", PrincipalID: "another-actor"}
				reporter.sourceActor = &intruder
				if err := requireReporterItem(t.Context(), f.service.database.reader, reporter, string(entry.issue.WorkItemID)); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("unknown actor authorized: %v", err)
				}
				rows, err := f.service.projectIssuePage(t.Context(), f.service.database.reader, mustIssueSelection(t, entry.scope), []any{entry.scope.organization, entry.scope.project}, "", "", entry.scope)
				if err != nil || len(rows) != 1 || issuePageItem(rows[0]).Position.Lane == 2147483647 {
					t.Fatalf("lane rows=%v err=%v", rows, err)
				}
				issue, _, err := readNativeIssueProjection(t.Context(), f.service.database.reader, entry.scope, string(entry.issue.WorkItemID), true)
				if err != nil || issue.ClosedAt == nil {
					t.Fatalf("closed issue=%+v err=%v", issue, err)
				}
			}
		})
	}
}

func TestIssuePageFiltersExceedCompoundSelectLimit(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "example-library")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	var known string
	want := map[string]bool{}
	for _, fixture := range []nativeFixture{f, other} {
		for index := range 103 {
			group := "unknown"
			if index == 100 {
				group = "known"
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			issue, err := createNativeFixtureIssueTx(t.Context(), tx, nativeScope{organization: scope.organization, project: fixture.project.ID}, tracker.CreateIssue{
				Title: fmt.Sprintf("Filter population %d", index), Body: issueContractTestSections, State: "Todo",
				Labels:    []string{"Common-Label", "common-label", "label'quote", "label-four", "label-five", group},
				Assignees: []string{"Common-Assignee", "common-assignee", "assignee'quote", "assignee-four", "assignee-five", group},
			}, f.service.config.now())
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if fixture.project.ID == scope.project {
				want[string(issue.WorkItemID)] = true
				if group == "known" {
					known = string(issue.WorkItemID)
				}
			}
		}
	}
	drainTestNativeBoard(t, f.service)
	for _, stage := range []string{"typed", "unknown filters", "backfilled"} {
		t.Run(stage, func(t *testing.T) {
			switch stage {
			case "unknown filters":
				for _, statement := range []string{"UPDATE issues SET page_filters_ready=NULL WHERE native_id<>?", "UPDATE native_board_items SET page_filters_ready=NULL WHERE work_item_id<>?"} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement, known); err != nil {
						t.Fatal(err)
					}
				}
			case "backfilled":
				for range 5 {
					if err := f.service.convertIssuePageFacts(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, filter := range []struct{ name, value string }{
				{"label", "Common-Label"}, {"label", "label'quote"}, {"label", "known"}, {"label", "unknown"},
				{"assignee", "Common-Assignee"}, {"assignee", "assignee'quote"}, {"assignee", "known"}, {"assignee", "unknown"},
			} {
				params := url.Values{filter.name: {filter.value}}
				selection, args, err := nativeIssueSelection(scope, params, false)
				if err != nil {
					t.Fatal(err)
				}
				where, boardArgs, err := nativeBoardFilter(scope, params, true)
				if err != nil {
					t.Fatal(err)
				}
				for _, surface := range []struct {
					name, query string
					args        []any
				}{{"issues", selection, args}, {"board", "SELECT b.work_item_id FROM native_board_items b WHERE " + where, boardArgs}} {
					for _, prefix := range []string{"", "WITH ", "WITH RECURSIVE "} {
						query := surface.query
						if prefix != "" {
							query = prefix + "selected AS (" + query + ") SELECT * FROM selected"
						}
						q, err := prepareNativePageStatement(t.Context(), f.service.database.reader, scope, "", query)
						if err != nil {
							t.Fatal(err)
						}
						ids, err := nativePageIDs(t.Context(), q, query, surface.args...)
						if err != nil {
							t.Errorf("%s %s=%q %s: %v", surface.name, filter.name, filter.value, prefix, err)
							continue
						}
						count := len(want)
						switch filter.value {
						case "known":
							count = 1
						case "unknown":
							count--
						}
						if len(ids) != count {
							t.Errorf("%s %s=%q: count=%d want=%d", surface.name, filter.name, filter.value, len(ids), count)
						}
						seen := map[string]bool{}
						for _, id := range ids {
							if !want[id] || seen[id] || filter.value == "known" && id != known || filter.value == "unknown" && id == known {
								t.Errorf("%s %s=%q: unexpected issue %s", surface.name, filter.name, filter.value, id)
							}
							seen[id] = true
						}
					}
				}
			}
		})
	}
}

func mustIssueSelection(t *testing.T, scope nativeScope) string {
	t.Helper()
	query, _, err := nativeIssueSelection(scope, url.Values{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return query
}

func TestIssuePageBackfillOwnsOneRowPerTransaction(t *testing.T) {
	for _, test := range []struct {
		name, table, column, family, update string
	}{
		{"actors", "issues", "actor_principal_id", "native_issue_actors", "UPDATE ISSUES SET ACTOR_KIND"},
		{"board facts", "native_board_items", "actor_principal_id", "native_board_filter_facts", "UPDATE NATIVE_BOARD_ITEMS SET ACTOR_PRINCIPAL_ID"},
		{"issue filters", "issues", "page_filters_ready", "native_issue_page_filter_values", "UPDATE ISSUES SET PAGE_FILTERS_READY"},
		{"board filters", "native_board_items", "page_filters_ready", "native_board_filter_values", "UPDATE NATIVE_BOARD_ITEMS SET PAGE_FILTERS_READY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			var first tracker.NativeIssue
			for i := range storageConversionRows + 1 {
				issue := f.create(t, fmt.Sprintf("backfill item %d", i))
				if i == 0 {
					first = issue
				}
			}
			drainTestNativeBoard(t, f.service)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE "+test.table+" SET "+test.column+"=NULL"); err != nil {
				t.Fatal(err)
			}
			count := func() int {
				t.Helper()
				var n int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+test.table+" WHERE "+test.column+" IS NULL").Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			if strings.Contains(test.name, "filters") {
				statement := "INSERT INTO native_issue_page_filter_values(issue_id,kind,value) SELECT id,'label','ghost' FROM issues WHERE native_id=?"
				if test.name == "board filters" {
					statement = "INSERT INTO native_board_filter_values(work_item_id,kind,value) VALUES(?,'label','ghost')"
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), statement, first.WorkItemID); err != nil {
					t.Fatal(err)
				}
				query, err := prepareNativePageQuery(t.Context(), f.service.database.reader, scope, string(first.WorkItemID), test.family)
				if err != nil {
					t.Fatal(err)
				}
				var stale int
				if err := query.QueryRowContext(t.Context(), "SELECT count(*) FROM "+test.family+" WHERE value='ghost'").Scan(&stale); err != nil || stale != 0 {
					t.Fatalf("unknown empty list matched stale filter: count=%d err=%v", stale, err)
				}
			}
			query, err := prepareNativePageQuery(t.Context(), f.service.database.reader, scope, "", test.family)
			if err != nil {
				t.Fatalf("pending backfill prevents reads: %v", err)
			}
			var got int
			if err := query.QueryRowContext(t.Context(), "SELECT count(*) FROM "+test.family).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(test.name, "filters") && got != storageConversionRows+1 {
				t.Fatalf("legacy actors=%d", got)
			}
			remaining, observed := count(), 0
			ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(statement string, bindings []driver.NamedValue) {
				if strings.HasPrefix(statement, "SELECT organization_id,project_id,") {
					args := make([]any, len(bindings))
					for index, binding := range bindings {
						args[index] = binding.Value
					}
					plan := explainQueryPlan(t, f.service.database.reader, statement, args)
					if strings.Contains(strings.Join(plan, "\n"), "USE TEMP B-TREE") {
						t.Fatalf("conversion batch sorts the pending backlog: %v", plan)
					}
				}
				if strings.HasPrefix(strings.ToUpper(statement), test.update) {
					if committed := count(); committed != remaining {
						t.Fatalf("backfill wrote a second row before committing: unknown=%d want=%d", committed, remaining)
					}
					remaining--
					observed++
				}
			}})
			for tick, want := range []int{1, 0} {
				if err := f.service.convertIssuePageFacts(ctx); err != nil {
					t.Fatal(err)
				}
				if got := count(); got != want {
					t.Fatalf("tick %d unknown=%d want=%d", tick, got, want)
				}
			}
			if observed != storageConversionRows+1 {
				t.Fatalf("observed writes=%d", observed)
			}
			if err := f.service.convertIssuePageFacts(t.Context()); err != nil || count() != 0 {
				t.Fatalf("completed backfill: %v", err)
			}
		})
	}
}

func TestBlockerHistoryUsesTypedPageFacts(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "Blocker history")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{Revision: 2, FromState: "Todo", ToState: "Done", Reason: "opaque reason", Run: &tracker.NativeRunData{AttemptID: "opaque-attempt", FencingToken: 3}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"unknown", "typed"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "unknown" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_events SET page_projection_ready=NULL WHERE work_item_id=?", issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.service.convertIssuePageFacts(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_events SET event_revision=7,from_state='Backlog',to_state='Cancelled',reason='typed reason',run_attempt_id='typed-attempt',run_fencing_token=9 WHERE work_item_id=? AND type='workflow.transitioned'", issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			page, err := f.service.readHistory(t.Context(), scope, string(issue.WorkItemID), url.Values{"view": {"blockers"}})
			if err != nil {
				t.Fatal(err)
			}
			data := page.Items[len(page.Items)-1].Data
			revision, from, to, reason, attempt, fence := tracker.Revision(2), "Todo", "Done", "opaque reason", "opaque-attempt", tracker.FencingToken(3)
			if stage == "typed" {
				revision, from, to, reason, attempt, fence = 7, "Backlog", "Cancelled", "typed reason", "typed-attempt", 9
			}
			if data.Revision != revision || data.FromState != from || data.ToState != to || data.Reason != reason || data.Run == nil || data.Run.AttemptID != attempt || data.Run.FencingToken != fence {
				t.Fatalf("data=%+v", data)
			}
			events, err := readNativeBlockerHistory(t.Context(), f.service.database.reader, scope, issue)
			if err != nil || events[len(events)-1].Data.Revision != revision {
				t.Fatalf("events=%v err=%v", events, err)
			}
		})
	}
}

func TestIssuePagePullRequestBackfill(t *testing.T) {
	for _, operation := range []string{"insert", "update", "delete", "concurrent update"} {
		for _, projection := range []string{"typed", "unknown", "typed URL differs"} {
			t.Run(operation+"/"+projection, func(t *testing.T) {
				unknown := projection == "unknown"
				f := newPullRequestFixture(t, true)
				linked := f.create(t, "Linked example")
				unrelated := f.create(t, "Unrelated example")
				path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/changes"
				response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{
					Mutation: tracker.Mutation{IdempotencyKey: "linked-change"}, Title: "Linked change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID},
				})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &f.change)
				f.changeFixture.path = path + "/" + f.change.ID
				reference := tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: pullRequestTestURL}
				if projection == "typed URL differs" {
					reference.ID = "42"
					reference.URL = "https://github.com/owner/repository/pull/42"
				}
				version := f.publishReference(t, "linked-version", reference)
				if projection == "typed URL differs" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_versions SET external_url=? WHERE id=?", pullRequestTestURL, version.ID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET issue_id=NULL,updated_at=?,synchronized_at=? WHERE repository_id=?", formatHubTime(f.now), formatHubTime(f.now), f.repositoryID); err != nil {
					t.Fatal(err)
				}
				if operation == "insert" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM pull_requests WHERE repository_id=?", f.repositoryID); err != nil {
						t.Fatal(err)
					}
				}
				if unknown {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_versions SET external_url=NULL WHERE id=?", version.ID); err != nil {
						t.Fatal(err)
					}
				}
				drainTestNativeBoard(t, f.service)
				at := f.now.Add(time.Hour)
				stamp := formatHubTime(at)
				var err error
				switch operation {
				case "insert":
					_, err = f.service.database.db.ExecContext(t.Context(), `INSERT INTO pull_requests(repository_id,github_node_id,github_number,title,url,github_state,head_ref,head_sha,base_ref,source_version,source_updated_at,synchronized_at,created_at,updated_at) VALUES(?,'example-pr',1,'Example pull request',?,'open','feature','abc','develop','v1',?,?,?,?)`, f.repositoryID, pullRequestTestURL, stamp, stamp, stamp, stamp)
				case "update", "concurrent update":
					_, err = f.service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET updated_at=?,synchronized_at=? WHERE repository_id=?", stamp, stamp, f.repositoryID)
				case "delete":
					_, err = f.service.database.db.ExecContext(t.Context(), "DELETE FROM pull_requests WHERE repository_id=?", f.repositoryID)
				}
				if err != nil {
					t.Fatal(err)
				}
				observed := 0
				injected := false
				ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(statement string, _ []driver.NamedValue) {
					if operation == "concurrent update" && unknown && !injected && statement == issuePageVersionActivityItemQuery {
						injected = true
						at = at.Add(time.Minute)
						stamp = formatHubTime(at)
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET updated_at=?,synchronized_at=? WHERE repository_id=?", stamp, stamp, f.repositoryID); err != nil {
							t.Fatal(err)
						}
					}
					if strings.HasPrefix(statement, "UPDATE issues SET last_activity_at=") {
						var committed int
						if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE native_id IN (?,?) AND last_activity_at=?", f.issue.WorkItemID, linked.WorkItemID, stamp).Scan(&committed); err != nil {
							t.Fatal(err)
						}
						if committed != observed {
							t.Fatalf("activity backfill did not commit one issue at a time: committed=%d writes=%d", committed, observed)
						}
						observed++
					}
				}})
				if err := f.service.convertIssuePageFacts(ctx); err != nil {
					t.Fatal(err)
				}
				var ready bool
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT external_url IS NOT NULL FROM change_versions WHERE id=?", version.ID).Scan(&ready); err != nil || !ready {
					t.Fatalf("version URL not backfilled: ready=%t err=%v", ready, err)
				}
				for _, item := range []tracker.NativeIssue{f.issue, linked} {
					var dirty bool
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM native_board_dirty WHERE work_item_id=?)", item.WorkItemID).Scan(&dirty); err != nil || !dirty {
						t.Fatalf("board refresh lost for %s: dirty=%t err=%v", item.WorkItemID, dirty, err)
					}
				}
				drainTestNativeBoard(t, f.service)
				for _, item := range []tracker.NativeIssue{f.issue, linked, unrelated} {
					want := f.now
					if operation != "delete" && item.WorkItemID != unrelated.WorkItemID {
						want = at
					}
					issue := readWorkItem(t, f.nativeFixture, item.WorkItemID, "")
					var raw string
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT record_json FROM native_board_items WHERE work_item_id=?", item.WorkItemID).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var card tracker.NativeBoardCard
					if err := json.Unmarshal([]byte(raw), &card); err != nil {
						t.Fatal(err)
					}
					if !issue.LastActivityAt.Equal(want) || !card.Issue.LastActivityAt.Equal(want) || !issue.UpdatedAt.Equal(f.now) {
						t.Fatalf("issue=%s activity=%s board=%s updated=%s want activity=%s", item.WorkItemID, issue.LastActivityAt, card.Issue.LastActivityAt, issue.UpdatedAt, want)
					}
				}
				if unknown && operation != "delete" && observed != 2 {
					t.Fatalf("backfilled issues=%d want=2", observed)
				}
				if unknown && operation == "concurrent update" && !injected {
					t.Fatal("pull request did not change during backfill preparation")
				}
			})
		}
	}
}

func TestLegacyPageEventsReadPastConversionBudget(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "Legacy event pages")
	actor := tracker.Actor{Kind: "human", PrincipalID: strings.Repeat("example-", 1500)}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, sourceActor: &actor}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO linked_issue_sources(work_item_id,source_url,title_supplied,body_supplied) VALUES(?,?,0,0)", issue.WorkItemID, "https://github.com/example-org/example-project/issues/1"); err != nil {
		t.Fatal(err)
	}
	issue.State = "Done"
	issue.Revision++
	if _, err := persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Done"}, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	for index := range storageConversionRows + 3 {
		data := tracker.CollaborationData{Reason: strings.Repeat("x", 11000), FromState: "Todo", ToState: "Done"}
		if index == storageConversionRows+2 {
			data.Fields = []string{"title", "body"}
		}
		for _, kind := range []string{"issue.edited", "comment.created", "workflow.transitioned"} {
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), kind, data, f.service.config.now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	for index := range storageConversionRows + 3 {
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,created_at,updated_at) VALUES(?,?,?,?,?)", scope.project, fmt.Sprintf("example-state-%d", index), "Todo", formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE collaboration_events SET page_projection_ready=NULL,edited_title=NULL,edited_body=NULL WHERE work_item_id=?", issue.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"native_transition_facts", "native_workflow_ranks"} {
		t.Run(family, func(t *testing.T) {
			q, err := prepareNativePageQuery(t.Context(), f.service.database.reader, scope, "", family)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err := q.QueryRowContext(t.Context(), "SELECT count(*) FROM "+family).Scan(&count); err != nil || count < storageConversionRows+3 {
				t.Fatalf("facts=%d error=%v", count, err)
			}
		})
	}
	t.Run("edited ownership", func(t *testing.T) {
		title, body, err := readNativeEditedOwnership(t.Context(), f.service.database.reader, issue.WorkItemID)
		if err != nil || !title || !body {
			t.Fatalf("late edits lost: title=%t body=%t error=%v", title, body, err)
		}
	})
	t.Run("ownership during conversion", func(t *testing.T) {
		pages := 0
		ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(statement string, _ []driver.NamedValue) {
			if !strings.Contains(statement, "type='issue.edited' AND page_projection_ready IS NULL") {
				return
			}
			pages++
			if pages == 2 {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_events SET page_projection_ready=1,edited_title=1,edited_body=1 WHERE work_item_id=? AND type='issue.edited' AND sequence=(SELECT max(sequence) FROM collaboration_events WHERE work_item_id=? AND type='issue.edited')", issue.WorkItemID, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
		}})
		title, body, err := readNativeEditedOwnership(ctx, f.service.database.reader, issue.WorkItemID)
		if err != nil || !title || !body || pages < 2 {
			t.Fatalf("concurrent conversion lost edit ownership: title=%t body=%t pages=%d error=%v", title, body, pages, err)
		}
	})
	t.Run("participants", func(t *testing.T) {
		event := issueEmailEvent{item: "other-item", at: f.service.config.now()}
		participants, next, err := readIssueEmailParticipants(t.Context(), f.service.database.reader, event, string(issue.WorkItemID), string(scope.organization), "", 10)
		if err != nil || next != "" || len(participants) != 2 {
			t.Fatalf("participants=%d next=%q error=%v", len(participants), next, err)
		}
		if _, ok := participants[actor.PrincipalID]; !ok {
			t.Fatal("legacy commenter missing")
		}
	})
}

func TestLegacyPageFactsExceedSQLiteParameterLimit(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	fact := nativePageFact{columns: "id,actor_kind,actor_principal_id", known: "SELECT id,actor_kind,actor_principal_id FROM main.issues WHERE actor_principal_id IS NOT NULL"}
	principal := "example'中文\x00principal"
	for index := range 12000 {
		fact.values = append(fact.values, []any{index, "human", principal})
	}
	q := &nativePageQuery{nativeQueryer: f.service.database.reader, facts: map[string]nativePageFact{"native_issue_actors": fact}}
	var count int
	if err := q.QueryRowContext(t.Context(), "SELECT count(*) FROM native_issue_actors WHERE actor_principal_id=?", principal).Scan(&count); err != nil || count != 12000 {
		t.Fatalf("facts=%d error=%v", count, err)
	}
}

func TestLegacyPageReadsSurviveConcurrentConversion(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	for index := range storageConversionRows + 3 {
		f.create(t, fmt.Sprintf("Concurrent legacy item %d", index))
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET actor_principal_id=NULL"); err != nil {
		t.Fatal(err)
	}
	pages := 0
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(statement string, _ []driver.NamedValue) {
		if !strings.Contains(statement, "FROM main.issues WHERE actor_principal_id IS NULL") {
			return
		}
		pages++
		if pages == 2 {
			if err := f.service.convertIssuePageFacts(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	q, err := prepareNativePageQuery(ctx, f.service.database.reader, scope, "", "native_issue_actors")
	if err != nil {
		t.Fatal(err)
	}
	var total, distinct int
	if err := q.QueryRowContext(t.Context(), "SELECT count(*),count(DISTINCT id) FROM native_issue_actors").Scan(&total, &distinct); err != nil || total != storageConversionRows+3 || distinct != total {
		t.Fatalf("concurrent conversion lost or duplicated rows: total=%d distinct=%d error=%v", total, distinct, err)
	}
	if pages < 2 {
		t.Fatal("read did not cross a page boundary")
	}
}

func TestIssuePageBackfillSkipsFailedRowsAndFamilies(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	for index := range storageConversionRows + 3 {
		f.create(t, fmt.Sprintf("Failed conversion item %d", index))
	}
	drainTestNativeBoard(t, f.service)
	for _, statement := range []string{"UPDATE issues SET actor_principal_id=NULL,actor_json='[]'", "UPDATE native_board_items SET actor_principal_id=NULL"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	failures := 0
	for range 6 {
		if err := f.service.convertIssuePageFacts(t.Context()); err != nil {
			failures++
		}
	}
	var pending int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_items WHERE actor_principal_id IS NULL").Scan(&pending); err != nil || pending != 0 || failures == 0 {
		t.Fatalf("failed actors starve board conversion: pending=%d failures=%d error=%v", pending, failures, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET actor_json='{}' WHERE actor_principal_id IS NULL"); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if err := f.service.convertIssuePageFacts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE actor_principal_id IS NULL").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("repaired actors were never retried: pending=%d error=%v", pending, err)
	}
}
