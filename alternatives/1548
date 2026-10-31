package hubserver

import (
	"bytes"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func localBoardFixture(t *testing.T, kind string) (string, []localboard.Record) {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "localboard", "testdata", kind+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "board.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), string(source)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records, err := localboard.Read(t.Context(), path, "default", kind, "example-board", []string{"Done", "Cancelled", "Closed"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("reader changed source database")
	}
	return path, records
}

func localBoardTarget(t *testing.T, service *Service, name string) nativeFixture {
	t.Helper()
	f := newNativeFixture(t, service, "", name)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	states := nativeTriageStates(append(nativeFixtureStates(), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}}))
	if err := applyNativeProjectStates(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, states, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

func seedLocalBoardIntake(t *testing.T, f nativeFixture, records []localboard.Record) map[string]tracker.NativeIssue {
	t.Helper()
	now := formatHubTime(time.Now())
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO repositories (github_node_id,github_owner,github_name,created_at,updated_at) VALUES (?, 'example-org','example-project',?,?) ON CONFLICT DO NOTHING`, "R_example", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE projects SET checkout_repository='example-org/example-project' WHERE id=?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	items := map[string]tracker.NativeIssue{}
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		snapshot := linkedSnapshot()
		snapshot.URL = "https://github.com/example-org/example-project/issues/" + strconv.Itoa(record.Number)
		snapshot.Provenance.ExternalID = "I_" + strconv.Itoa(record.Number)
		f.service.config.ImportBackend = linkedTestImporter{snapshot: snapshot}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "intake-" + record.Key}, GitHubIssueURL: snapshot.URL})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		items[record.SourceID] = issue
	}
	return items
}

func TestLocalBoardImportFixtures(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"local_sqlite", "github_local"} {
		t.Run(kind, func(t *testing.T) {
			path, records := localBoardFixture(t, kind)
			if len(records) != 2 || records[0].SourceID != "z-first" || records[1].SourceID != "a-second" {
				t.Fatalf("dependency order or open selection: %+v", records)
			}
			if len(records[1].Comments) != 1 || records[1].Comments[0].Body != "Edited comment" {
				t.Fatalf("comment event replay: %+v", records[1].Comments)
			}
			records[0].Body += "\n\nhttps://figma.com/design/example"
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			f := localBoardTarget(t, nil, "import")
			targets := []nativeFixture{f, localBoardTarget(t, f.service, "second-target")}
			for _, f := range targets {
				intake := map[string]tracker.NativeIssue{}
				if kind == "github_local" {
					intake = seedLocalBoardIntake(t, f, records)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_visual_attachments=1 WHERE id=?", f.project.ID); err != nil {
					t.Fatal(err)
				}
				dryCounts := localBoardCounts(t, f)
				plans := []localboard.Result{}
				for _, record := range records {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record, DryRun: true})
					requireNativeStatus(t, response, http.StatusOK)
					var plan localboard.Result
					decodeHubResponse(t, response, &plan)
					plans = append(plans, plan)
				}
				if !reflect.DeepEqual(dryCounts, localBoardCounts(t, f)) {
					t.Fatal("dry-run wrote native data")
				}
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM local_board_imports WHERE project_id=?", f.project.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("dry-run wrote imports: %d, %v", count, err)
				}
				imported := []tracker.NativeIssue{}
				for i, record := range records {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record})
					requireNativeStatus(t, response, http.StatusOK)
					var result localboard.Result
					decodeHubResponse(t, response, &result)
					plan := plans[i]
					plan.WorkItemID = result.WorkItemID
					if !reflect.DeepEqual(plan, result) {
						t.Fatalf("dry-run %+v != real-run %+v", plan, result)
					}
					var issue tracker.NativeIssue
					decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(result.WorkItemID), f.token, nil), &issue)
					wantState := "Todo"
					if i == 1 {
						wantState = "Backlog"
					}
					if issue.State != wantState || result.UnknownLane != (i == 1) {
						t.Fatalf("lane: %+v, %+v", issue, result)
					}
					if kind == "github_local" {
						if issue.WorkItemID != intake[record.SourceID].WorkItemID || issue.Title != intake[record.SourceID].Title || issue.Body != intake[record.SourceID].Body {
							t.Fatalf("GitHub match overwrote content or created duplicate: %+v", issue)
						}
					} else if issue.Title != record.Title || issue.Body != record.Body || issue.Provenance == nil || issue.Provenance.AuthorID != "source-author" {
						t.Fatalf("local content/provenance: %+v", issue)
					}
					if kind == "local_sqlite" {
						if i == 0 && (issue.Priority == nil || *issue.Priority != 2) || i == 1 && issue.Priority != nil {
							t.Fatalf("source priority lost: %v", issue.Priority)
						}
					}
					imported = append(imported, issue)
				}
				if !reflect.DeepEqual(imported[1].Dependencies, []tracker.NativeWorkItemID{imported[0].WorkItemID}) {
					t.Fatalf("dependencies: %+v", imported[1].Dependencies)
				}
				var comments tracker.Page[tracker.NativeComment]
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(imported[1].WorkItemID)+"/comments", f.token, nil), &comments)
				found := false
				for _, comment := range comments.Items {
					if comment.Body == "Edited comment" {
						found = true
						if comment.Provenance == nil || comment.Provenance.Provider != kind || comment.Provenance.AuthorID != "detent" || !comment.Provenance.UpdatedAt.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
							t.Fatalf("comment provenance: %+v", comment)
						}
					}
					if comment.Body == "Deleted comment" || comment.Body == "Original comment" {
						t.Fatalf("replayed deleted or stale comment: %+v", comment)
					}
				}
				if !found {
					t.Fatal("missing imported comment")
				}
				transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "native-progress"}, ExpectedRevision: imported[0].Revision, State: "In Progress", Reason: "user_requested"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(imported[0].WorkItemID)+"/workflow", f.token, transition), http.StatusOK)
				counts := localBoardCounts(t, f)
				for _, record := range records {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record})
					requireNativeStatus(t, response, http.StatusOK)
					var result localboard.Result
					decodeHubResponse(t, response, &result)
					if result.Action != "unchanged" || result.Comments != 0 || result.Dependencies != 0 {
						t.Fatalf("replay wrote data: %+v", result)
					}
					if record.SourceID == "z-first" && result.State != "In Progress" {
						t.Fatalf("replay reset native lane: %+v", result)
					}
				}
				if after := localBoardCounts(t, f); !reflect.DeepEqual(counts, after) {
					t.Fatalf("second import created rows: %v -> %v", counts, after)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("source copy changed")
			}
		})
	}
}

func localBoardCounts(t *testing.T, f nativeFixture) []int {
	t.Helper()
	counts := []int{}
	for _, query := range []string{"SELECT count(*) FROM issues", "SELECT count(*) FROM native_comments", "SELECT count(*) FROM local_board_imports", "SELECT count(*) FROM native_commands", "SELECT count(*) FROM issue_dependencies"} {
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts = append(counts, count)
	}
	return counts
}

func TestLocalBoardImportRefusals(t *testing.T) {
	t.Parallel()
	_, records := localBoardFixture(t, "local_sqlite")
	f := localBoardTarget(t, nil, "refusals")
	for _, test := range []struct {
		name, token string
		record      localboard.Record
		status      int
	}{
		{"worker", f.worker(t, "import-worker"), records[0], http.StatusForbidden},
		{"missing grant", newNativeFixture(t, f.service, "", "no-grant").token, records[0], http.StatusNotFound},
		{"missing prerequisite", f.token, records[1], http.StatusUnprocessableEntity},
		{"invalid content", f.token, func() localboard.Record {
			r := records[0]
			r.Title = strings.Repeat("title-secret", 100)
			r.Body = "body-secret"
			return r
		}(), http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := localBoardCounts(t, f)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", test.token, localboard.Request{Record: test.record})
			requireNativeStatus(t, response, test.status)
			if strings.Contains(response.Body.String(), "title-secret") || strings.Contains(response.Body.String(), "body-secret") {
				t.Fatal("error exposed issue content")
			}
			if after := localBoardCounts(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("refused import wrote data")
			}
		})
	}
}

func TestLocalBoardImportHostedAuthorityAndAllowances(t *testing.T) {
	t.Parallel()
	_, records := localBoardFixture(t, "local_sqlite")
	for _, test := range []struct {
		name, role, scope string
		limits            map[string]int64
		status            int
	}{
		{"operator", "owner", "write", nil, http.StatusOK},
		{"viewer", "viewer", "read", nil, http.StatusNotFound},
		{"reporter", "reporter", "write", nil, http.StatusForbidden},
		{"storage allowance", "owner", "write", map[string]int64{"collaboration_bytes": 1}, http.StatusTooManyRequests},
		{"issue allowance", "owner", "write", map[string]int64{"unarchived_issues": 0}, http.StatusTooManyRequests},
		{"terminal issue allowance", "owner", "write", map[string]int64{"unarchived_issues": 0}, http.StatusOK},
		{"content exceeds storage", "owner", "write", map[string]int64{"collaboration_bytes": int64(len(records[0].Title) + len(records[0].Body) - 1)}, http.StatusTooManyRequests},
		{"history allowance", "owner", "write", map[string]int64{"history_records": 0}, http.StatusTooManyRequests},
		{"mutation allowance", "owner", "write", map[string]int64{"api_mutations": 1}, http.StatusTooManyRequests},
		{"event allowance", "owner", "write", map[string]int64{"ingested_events": 0}, http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			hostedTestPlans(t, f.service, test.limits)
			user := f.user(t, test.role, test.role, test.role+"@example.test", test.scope, "")
			record := records[0]
			if test.name == "terminal issue allowance" {
				record.State = "Done"
			}
			before := localBoardCounts(t, nativeFixture{service: f.service})
			usageBefore, err := f.service.database.hostedConsumption(t.Context(), f.service.database.reader, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			dry := f.request(t, user, http.MethodPost, f.base+"/local-board-import", localboard.Request{Record: record, DryRun: true})
			requireNativeStatus(t, dry, test.status)
			if !reflect.DeepEqual(before, localBoardCounts(t, nativeFixture{service: f.service})) {
				t.Fatal("dry-run persisted rows")
			}
			usageAfter, err := f.service.database.hostedConsumption(t.Context(), f.service.database.reader, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			for _, metric := range []string{"api_mutations", "ingested_events", "collaboration_bytes", "history_records", "events_total", "unarchived_issues"} {
				if usageBefore[metric] != usageAfter[metric] {
					t.Fatalf("dry-run changed %s: before=%d after=%d", metric, usageBefore[metric], usageAfter[metric])
				}
			}
			response := f.request(t, user, http.MethodPost, f.base+"/local-board-import", localboard.Request{Record: record})
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK && !reflect.DeepEqual(before, localBoardCounts(t, nativeFixture{service: f.service})) {
				t.Fatal("rejected import persisted rows")
			}
			if test.name == "operator" {
				hostedTestPlans(t, f.service, map[string]int64{"unarchived_issues": 0, "collaboration_bytes": 0, "history_records": 0, "api_mutations": 0, "ingested_events": 0})
				before := localBoardCounts(t, nativeFixture{service: f.service})
				for _, dryRun := range []bool{true, false} {
					response := f.request(t, user, http.MethodPost, f.base+"/local-board-import", localboard.Request{Record: record, DryRun: dryRun})
					requireNativeStatus(t, response, http.StatusOK)
					var result localboard.Result
					decodeHubResponse(t, response, &result)
					if result.Action != "unchanged" {
						t.Fatalf("replay at allowance limit: %+v", result)
					}
				}
				if !reflect.DeepEqual(before, localBoardCounts(t, nativeFixture{service: f.service})) {
					t.Fatal("replay at allowance limit wrote rows")
				}
			}
		})
	}
}

func TestLocalBoardGitHubMatchRefusals(t *testing.T) {
	t.Parallel()
	_, records := localBoardFixture(t, "github_local")
	for _, test := range []string{"missing intake", "repository mismatch", "native lane changed", "terminal native item"} {
		t.Run(test, func(t *testing.T) {
			f := localBoardTarget(t, nil, test)
			intake := seedLocalBoardIntake(t, f, records)
			record := records[0]
			switch test {
			case "missing intake":
				record.Number = 999
			case "repository mismatch":
				record.Repository = "other-org/other-project"
			case "native lane changed", "terminal native item":
				issue := intake[record.SourceID]
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-native"}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
				if test == "terminal native item" {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "complete-native"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
					requireNativeStatus(t, response, http.StatusOK)
				}
			}
			before := localBoardCounts(t, f)
			for _, dry := range []bool{true, false} {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record, DryRun: dry}), http.StatusUnprocessableEntity)
			}
			if after := localBoardCounts(t, f); !reflect.DeepEqual(before, after) {
				t.Fatal("unmatched or protected GitHub item changed")
			}
		})
	}
}
