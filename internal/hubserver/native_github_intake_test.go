package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type linkedTestImporter struct{ snapshot tracker.GitHubIssueSnapshot }

func (i linkedTestImporter) FetchImportPage(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
	snapshot := i.snapshot
	if request.Stage == "issue" {
		return GitHubImportPage{Issue: &IssueSource{NodeID: snapshot.Provenance.ExternalID, Number: linkedSourceNumber(snapshot.URL), URL: snapshot.URL, Title: snapshot.Title, Body: snapshot.Body, AuthorID: snapshot.Provenance.AuthorID, CreatedAt: snapshot.Provenance.CreatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}}, nil
	}
	page := GitHubImportPage{}
	for _, comment := range snapshot.Comments {
		page.Records = append(page.Records, GitHubImportRecord{Kind: "comment", Body: comment.Body, Provenance: comment.Provenance})
	}
	return page, nil
}

func linkedWebhookPayload(t *testing.T, action string, snapshot tracker.GitHubIssueSnapshot, comment *githubWebhookComment) []byte {
	t.Helper()
	owner, repository, node, fullName := "acme", "orders", "R_repo", "acme/orders"
	number, state, author := 12, "open", snapshot.Provenance.AuthorID
	if action == "closed" {
		state = "closed"
	}
	created, err := json.Marshal(snapshot.Provenance.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := json.Marshal(snapshot.Provenance.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(snapshot.Body)
	if err != nil {
		t.Fatal(err)
	}
	payload := githubWebhookPayload{Repository: &githubWebhookRepository{NodeID: &node, Name: &repository, FullName: &fullName, Owner: &githubWebhookActor{Login: &owner}},
		Issue: &githubWebhookIssue{NodeID: &snapshot.Provenance.ExternalID, Number: &number, Title: &snapshot.Title, Body: body, HTMLURL: &snapshot.URL, State: &state, User: &githubWebhookActor{Login: &author}, Labels: json.RawMessage(`[]`), Assignees: json.RawMessage(`[]`), CreatedAt: created, UpdatedAt: updated}, Comment: comment}
	raw, err := json.Marshal(struct {
		githubWebhookPayload
		Action string `json:"action"`
	}{payload, action})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func applyLinkedWebhook(t *testing.T, f nativeFixture, action, event string, snapshot tracker.GitHubIssueSnapshot, comment *githubWebhookComment) {
	t.Helper()
	raw := linkedWebhookPayload(t, action, snapshot, comment)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx := context.WithValue(t.Context(), linkedSourceURLKey{}, "https://cloud.example")
	if _, err := applyWebhook(ctx, tx, storedWebhook{EventType: event, Action: action, DeliveryID: action, Payload: raw}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeGitHubWebhookIntake(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, body, author string }{
		{"public reporter", "A public report", "outside-reporter"},
		{"empty body is not a spam signal", "", "reporter"},
		{"unavailable author retains report", "Report", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := linkedFixture(t)
			snapshot := linkedSnapshot()
			snapshot.Body, snapshot.Provenance.AuthorID = test.body, test.author
			f.service.config.GitHubWebhookSecret = []byte(testWebhookSecret)
			f.service.database.linkedSourceBase = "https://cloud.example"
			payload := string(linkedWebhookPayload(t, "opened", snapshot, nil))
			for range 2 {
				requireNativeStatus(t, sendSignedWebhookRequest(t, f.service, "intake-delivery", "issues", payload), http.StatusAccepted)
			}
			applyLinkedWebhook(t, f, "opened", "issues", snapshot, nil)
			var id string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE project_id = ?", f.project.ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			issue, _, err := readNativeIssue(t.Context(), f.service.database.db, scope, id)
			if err != nil {
				t.Fatal(err)
			}
			if issue.State != "Triage" || issue.LinkedSource.Status != "complete" || issue.Body != test.body || issue.Terminal || len(issue.Assignees) != 0 {
				t.Fatalf("intake = %+v", issue)
			}
			if test.author == "" {
				test.author = "unavailable"
			}
			if issue.Provenance.AuthorID != test.author {
				t.Fatalf("author = %+v", issue.Provenance)
			}
			manual := f.link(t, "manual-dedup")
			if manual.WorkItemID != issue.WorkItemID {
				t.Fatal("manual intake created a duplicate")
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", id).Scan(&count); err != nil || count != 1 {
				t.Fatalf("linkbacks = %d, %v", count, err)
			}
			var desired WorkpadDesired
			var raw string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT desired_json FROM github_outbox").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(raw), &desired); err != nil {
				t.Fatal(err)
			}
			if desired.Body != "Tracked in Detent: https://cloud.example/work/i/"+id || desired.CloseSource {
				t.Fatalf("linkback = %+v", desired)
			}
			item, found, err := f.service.claimOutbox(t.Context())
			if err != nil || !found || item.IssueNumber != 12 || item.Profile != "native" || item.RepositoryOwner != "acme" {
				t.Fatalf("outbox source routing = %+v, %t, %v", item, found, err)
			}
		})
	}
}

func TestNativeGitHubSourceUpdates(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"Triage", "Todo", "Cancelled", "Done"} {
		t.Run(state, func(t *testing.T) {
			f := linkedFixture(t)
			snapshot := linkedSnapshot()
			applyLinkedWebhook(t, f, "opened", "issues", snapshot, nil)
			var id string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE project_id = ?", f.project.ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			issue, _, err := readNativeIssue(t.Context(), tx, scope, id)
			if err != nil {
				t.Fatal(err)
			}
			from := issue.State
			issue.State = state
			ctx := context.WithValue(t.Context(), linkedSourceURLKey{}, "https://cloud.example")
			if _, err := persistNativeIssue(ctx, tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: state}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"closed", "reopened"} {
				snapshot.Provenance.UpdatedAt = snapshot.Provenance.UpdatedAt.Add(time.Second)
				applyLinkedWebhook(t, f, action, "issues", snapshot, nil)
				applyLinkedWebhook(t, f, action, "issues", snapshot, nil)
			}
			author := "commenter"
			comment := &githubWebhookComment{NodeID: "C_webhook", Body: "External discussion", User: &githubWebhookActor{Login: &author}, CreatedAt: snapshot.Provenance.UpdatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}
			applyLinkedWebhook(t, f, "created", "issue_comment", snapshot, comment)
			applyLinkedWebhook(t, f, "created", "issue_comment", snapshot, comment)
			issue, _, err = readNativeIssue(t.Context(), f.service.database.db, scope, id)
			if err != nil || issue.State != state {
				t.Fatalf("state changed = %s, %v", issue.State, err)
			}
			var comments, closes int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id = ?", id).Scan(&comments); err != nil || comments != 3 {
				t.Fatalf("comments = %d, %v", comments, err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox WHERE json_extract(desired_json, '$.close_source') = 1 AND json_extract(desired_json, '$.state_reason') = 'not_planned'").Scan(&closes); err != nil {
				t.Fatal(err)
			}
			want := 0
			if state == "Cancelled" {
				want = 1
			}
			if closes != want {
				t.Fatalf("closes = %d, want %d", closes, want)
			}
			r := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+id+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "private"}, Body: "Private Cloud discussion"})
			requireNativeStatus(t, r, http.StatusOK)
			var total int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbox").Scan(&total); err != nil || total != 1+want {
				t.Fatalf("Cloud discussion mirrored: %d, %v", total, err)
			}
		})
	}
}

func TestNativeTriageWorkflowProjection(t *testing.T) {
	t.Parallel()
	for _, bound := range []bool{false, true} {
		t.Run(strconv.FormatBool(bound), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "triage")
			if bound {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = 'acme/orders' WHERE id = ?", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			if err := applyNativeProjectStates(t.Context(), tx, scope, nativeFixtureStates(), time.Now()); err != nil {
				t.Fatal(err)
			}
			project, err := readNativeProject(t.Context(), tx, scope)
			if err != nil {
				t.Fatal(err)
			}
			if (project.States[0].Name == "Triage") != bound || bound && (project.States[0].Terminal || project.States[0].Dispatchable) {
				t.Fatalf("lanes = %+v", project.States)
			}
		})
	}
}

func pendingLinkedIssue(t *testing.T, f nativeFixture) tracker.NativeIssue {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	issue, err := createLinkedIssueTx(t.Context(), tx, scope, tracker.CreateIssue{GitHubIssueURL: linkedSnapshot().URL}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	issue.State = "Todo"
	issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Triage", ToState: "Todo"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return issue
}

func TestNativeGitHubTriageMigration(t *testing.T) {
	t.Parallel()
	for _, binding := range []string{"", "checkout", "repository"} {
		t.Run(binding, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "triage-migration")
			if binding == "checkout" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = 'acme/orders' WHERE id = ?", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			if binding == "repository" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at) VALUES ('R_migration', 'acme', 'orders', ?, ?)", testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM projects WHERE repository_id IS NOT NULL"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id = (SELECT id FROM repositories WHERE github_node_id = 'R_migration') WHERE id = ?", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			migration, err := migrationFiles.ReadFile("migrations/20261006024500_native_github_triage.sql")
			if err != nil {
				t.Fatal(err)
			}
			up, _, _ := strings.Cut(string(migration), "-- +goose Down")
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), up); err != nil {
				t.Fatal(err)
			}
			project, err := readNativeProject(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			if err != nil {
				t.Fatal(err)
			}
			if binding == "" {
				if project.States[0].Name == "Triage" {
					t.Fatal("unbound project affected")
				}
			} else {
				if project.States[0].Name != "Triage" || project.States[0].Dispatchable || project.States[0].Terminal || !slices.Contains(project.States[0].Transitions, "Todo") {
					t.Fatalf("migrated workflow = %+v", project.States)
				}
				var dispatchable, terminal bool
				if err := tx.QueryRowContext(t.Context(), "SELECT dispatchable, terminal FROM workflow_states WHERE project_id = ? AND detent_state = 'Triage'", f.project.ID).Scan(&dispatchable, &terminal); err != nil || dispatchable || terminal {
					t.Fatalf("migration flags = %t, %t, %v", dispatchable, terminal, err)
				}
			}
		})
	}
}

func (f *browserHostedFixture) seedGitHubTriage(t *testing.T) {
	t.Helper()
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = 'acme/orders' WHERE id = ?", f.project); err != nil {
		t.Fatal(err)
	}
	snapshot := linkedSnapshot()
	snapshot.Title = "Public GitHub report awaiting triage"
	scope := nativeScope{organization: tracker.OrganizationID(f.service.config.Hosted.OrganizationID), project: tracker.ProjectID(f.project)}
	applyLinkedWebhook(t, nativeFixture{service: f.service, project: tracker.NativeProject{ID: scope.project, OrganizationID: scope.organization}}, "opened", "issues", snapshot, nil)
}
