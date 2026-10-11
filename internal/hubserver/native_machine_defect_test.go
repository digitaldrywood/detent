package hubserver

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeMachineDefectOccurrences(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		first          string
		second         string
		otherProject   bool
		state          string
		scheduledFirst bool
	}{
		{"test failure", "Gate failed:\n--- FAIL: TestHostedChangePolicyJourney (0.03s)\nFAIL github.com/digitaldrywood/detent/internal/hubserver 0.04s", "Another worker found:\n--- FAIL: TestHostedChangePolicyJourney (0.08s)\nFAIL github.com/digitaldrywood/detent/internal/hubserver 0.09s", false, "Todo", false},
		{"migration collision", "panic: goose: duplicate version 20261006023000 detected:\nsprite_placement.sql", "Gate blocked by duplicate migration version 20261006023000: github_references.go and sprite_placement.sql", false, "Todo", false},
		{"compiler diagnostic", "internal/hub/pool.go:23:4: undefined: capacity", "internal/hub/pool.go:31:2: undefined: capacity", false, "Todo", false},
		{"scheduled owner", "Problem: `go-test:example/hub:TestJourney`", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", false, "Todo", true},
		{"other project policy", "goose: duplicate version 20261006023000 detected:", "duplicate migration version 20261006023000", true, "Backlog", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			states := append(f.project.States, tracker.NativeState{Name: "Backlog"})
			raw, err := json.Marshal(states)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", string(raw), f.project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, 'Backlog', 'Backlog', 0, 0, ?, ?)", f.project.ID, formatHubTime(f.service.config.now()), formatHubTime(f.service.config.now())); err != nil {
				t.Fatal(err)
			}
			project := f.project.ID
			if !test.otherProject {
				project = tracker.ProjectID(issueorigin.DetentCloudProjectID)
				for _, statement := range []string{
					"INSERT INTO projects (id, organization_id, name, profile, states_json, require_dependencies, created_at, github_repository_enabled) SELECT ?, organization_id, 'defect-reporting', profile, states_json, require_dependencies, created_at, github_repository_enabled FROM projects WHERE id = ?",
					"INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) SELECT ?, source_name, detent_state, terminal, dispatchable, created_at, updated_at FROM workflow_states WHERE project_id = ?",
				} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement, project, f.project.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			credential, _, err := f.service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: project, credential: credential}
			var owner tracker.NativeIssue
			for i, evidence := range []string{test.first, test.second} {
				origin := issueorigin.Origin{Kind: "worker", Instance: "runner-" + strconv.Itoa(i), Source: "attempt-" + strconv.Itoa(i), Fingerprint: "invented-" + strconv.Itoa(i)}
				if test.scheduledFirst && i == 0 {
					origin.Kind = "doctor"
					origin.Instance = "github-actions"
				}
				request := tracker.CreateIssue{Title: "Different wording " + strconv.Itoa(i), Body: issueorigin.Stamp(evidence, origin), State: "Backlog", Priority: new(3)}
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				issue, err := createNativeMachineIntakeTx(t.Context(), tx, scope, request, f.service.config.now())
				if err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					owner = issue
				} else if issue.WorkItemID != owner.WorkItemID || !issue.PublicationReused {
					t.Fatalf("duplicate report created a second issue: %#v", issue)
				}
			}
			wantPriority := 1
			if test.otherProject {
				wantPriority = 3
			}
			if owner.State != test.state || owner.Priority == nil || *owner.Priority != wantPriority {
				t.Fatalf("owner = %#v, want %s priority %d", owner, test.state, wantPriority)
			}
			var issues, comments int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id = ?", project).Scan(&issues); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id = ?", owner.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if issues != 1 || comments != 1 {
				t.Fatalf("issues=%d comments=%d, want one issue and one occurrence", issues, comments)
			}
			base := "/api/v2/organizations/" + string(scope.organization) + "/projects/" + string(project)
			fingerprint := issueorigin.DefectFingerprint(test.first)
			response := performHubAPIRequest(t, f.service, http.MethodGet, base+"/work-items?open=true&fingerprint="+fingerprint, testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.Page[tracker.NativeIssue]
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].WorkItemID != owner.WorkItemID {
				t.Fatalf("bounded fingerprint search = %#v", page)
			}
			origin := issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "scheduled-run", Fingerprint: "scheduled-wording"}
			if test.scheduledFirst {
				previous, _ := issueorigin.Parse(owner.Body)
				origin.Fingerprint = previous.Fingerprint
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "scheduled-occurrence"}, Title: "Scheduled failure", Body: issueorigin.Stamp(test.second, origin), State: "Todo", Priority: new(1)})
			requireNativeStatus(t, response, http.StatusOK)
			var scheduled tracker.NativeIssue
			decodeHubResponse(t, response, &scheduled)
			if scheduled.WorkItemID != owner.WorkItemID || scheduled.State != owner.State || !scheduled.PublicationReused || scheduled.Body != owner.Body {
				t.Fatalf("scheduled report did not retain worker owner: %#v", scheduled)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Done') WHERE native_id = ?", project, owner.WorkItemID); err != nil {
				t.Fatal(err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			request := tracker.CreateIssue{Title: "Late report", State: "Backlog", Priority: new(0), Body: issueorigin.Stamp(test.second, issueorigin.Origin{Kind: "worker", Source: "late-attempt", Fingerprint: "late-wording"})}
			closed, err := createNativeMachineIntakeTx(t.Context(), tx, scope, request, f.service.config.now())
			if err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if closed.WorkItemID != owner.WorkItemID || !closed.Terminal || !closed.PublicationReused || closed.Priority == nil || *closed.Priority != 1 {
				t.Fatalf("terminal report changed handled owner: %#v", closed)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id = ?", owner.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if comments != 2 {
				t.Fatalf("terminal report added an occurrence: %d", comments)
			}

		})
	}
}
