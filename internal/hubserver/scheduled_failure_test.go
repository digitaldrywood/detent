package hubserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledFailureAttribution(t *testing.T) {
	for _, test := range []struct {
		name, state string
		changes     int
		attributed  bool
		want        string
	}{
		{"single landing", "Done", 1, true, "Rework"},
		{"ambiguous landings", "Done", 2, false, "Done"},
		{"no landing", "Done", 0, false, "Done"},
		{"already reworking", "Rework", 1, true, "Rework"},
		{"already implementing", "In Progress", 1, true, "In Progress"},
		{"already merging", "Merging", 1, true, "Merging"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			states := nativeFixtureStates()
			states[2].Transitions = append(states[2].Transitions, "Rework")
			states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true}, tracker.NativeState{Name: "Merging", Dispatchable: true})
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyNativeProjectStates(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, states, f.service.config.now()); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			issue := f.create(t, "attribution")
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) WHERE native_id=?`, f.project.ID, test.state, issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			repository := "example-org/example-project"
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE projects SET checkout_repository=? WHERE id=?`, repository, f.project.ID); err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("b", 40)
			for n := range test.changes {
				id := []string{"change-one", "change-two"}[n]
				merge := []string{head, strings.Repeat("c", 40)}[n]
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json,landed_merge_sha,landed_base_ref) VALUES(?,?,?,?,'{}',?,'develop')`, id, f.project.OrganizationID, f.project.ID, issue.WorkItemID, merge); err != nil {
					t.Fatal(err)
				}
			}
			evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("a", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Coverage", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: head, Conclusion: "failure"}
			f.service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, r GitHubImportRequest) (GitHubImportPage, error) {
				if r.Stage == "scheduled" {
					if r.ScheduledBranch != "develop" {
						t.Fatal("failure verification did not constrain develop")
					}
					return GitHubImportPage{Scheduled: &evidence}, nil
				}
				if r.CompareBase != strings.Repeat("a", 40) || r.CompareHead != head {
					t.Fatal("wrong range")
				}
				return GitHubImportPage{Commits: []string{strings.Repeat("c", 40), head}}, nil
			})
			body := "Run: " + evidence.RunURL + "\nJob: " + evidence.JobURL + "\nTested commit: " + head + "\nFailure: regression" + evidence.Stamp()
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "occurrence"}, Body: body, ScheduledFailure: &tracker.ScheduledFailure{PreviousGreenSHA: strings.Repeat("a", 40), Evidence: evidence}}
			for range 2 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
				requireNativeStatus(t, response, http.StatusOK)
				var result struct {
					Attributed bool `json:"attributed"`
				}
				decodeHubResponse(t, response, &result)
				if result.Attributed != test.attributed {
					t.Fatalf("attributed = %v", result.Attributed)
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			if issue.State != test.want {
				t.Fatalf("state = %s, want %s", issue.State, test.want)
			}
			var comments, transitions, items int
			for _, check := range []struct {
				query string
				dest  *int
			}{
				{`SELECT count(*) FROM native_comments WHERE project_id=?`, &comments},
				{`SELECT count(*) FROM collaboration_events WHERE project_id=? AND type='workflow.transitioned'`, &transitions},
				{`SELECT count(*) FROM issues WHERE project_id=?`, &items},
			} {
				if err := f.service.database.reader.QueryRowContext(t.Context(), check.query, f.project.ID).Scan(check.dest); err != nil {
					t.Fatal(err)
				}
			}
			wantComments, wantTransitions := 0, 0
			if test.attributed {
				wantComments = 1
				if test.state == "Done" {
					wantTransitions = 1
				}
			}
			if comments != wantComments || transitions != wantTransitions || items != 1 {
				t.Fatalf("comments=%d transitions=%d issues=%d", comments, transitions, items)
			}
			if comments > 0 {
				var retained string
				if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT body FROM native_comments WHERE project_id=?`, f.project.ID).Scan(&retained); err != nil {
					t.Fatal(err)
				}
				if retained != body {
					t.Fatal("failure evidence changed")
				}
			}
		})
	}
}
