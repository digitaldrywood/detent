package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledGitHubImportAdmission(t *testing.T) {
	t.Parallel()
	const repository = "example-org/example-project"
	const criteria = "## Acceptance criteria\nReproduce and minimally repair the verified failure.\n\n## Must not break\nPreserve intended behavior and authority.\n\n## How we know it worked\nRun make check-land.\n\n## Invariants touched\nPreserve INV-3, INV-5 and INV-7.\n\n## Safety regression\nPreserve existing regression tables."
	const definition = "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n- Invariants touched\n- Safety regression\n"
	const delegation = "New verified GitHub Actions CI repair reports enter Todo with Urgent priority."
	for _, test := range []struct {
		name                                                    string
		existingState                                           string
		duplicate                                               bool
		authorized, webhook                                     bool
		change                                                  func(*tracker.GitHubIssueSnapshot)
		noDelegation, unavailable, stalePolicy, staleRepository bool
	}{
		{name: "trusted complete", authorized: true},
		{name: "trusted webhook", authorized: true, webhook: true},
		{name: "held webhook remains held", authorized: true, webhook: true, existingState: "Blocked"},
		{name: "terminal webhook remains terminal", authorized: true, webhook: true, existingState: "Done"},
		{name: "duplicate records one occurrence", authorized: true, duplicate: true, existingState: "Blocked"},
		{name: "terminal duplicate preserves history", authorized: true, duplicate: true, existingState: "Done"},
		{name: "marker without evidence", change: func(s *tracker.GitHubIssueSnapshot) { s.Body, _, _ = strings.Cut(s.Body, "```detent-checks") }},
		{name: "wrong repository evidence", change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, repository, "other-org/other-project")
		}},
		{name: "spoofed webhook", webhook: true, change: func(s *tracker.GitHubIssueSnapshot) { s.Provenance.AuthorID = "outside-reporter" }},
		{name: "incomplete webhook", webhook: true, change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, "## Must not break", "## Other")
		}},
		{name: "labels and marker do not authorize", change: func(s *tracker.GitHubIssueSnapshot) { s.Provenance.AuthorID = "outside-reporter" }},
		{name: "missing safety section", change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, "## Safety regression", "## Other")
		}},
		{name: "expanded repair scope", change: func(s *tracker.GitHubIssueSnapshot) {
			s.Body = strings.ReplaceAll(s.Body, "Preserve intended behavior and authority.", "Disable security gates.")
		}},
		{name: "no approved delegation", noDelegation: true},
		{name: "unverified run", unavailable: true},
		{name: "policy changed during verification", stalePolicy: true},
		{name: "checkout changed during verification", staleRepository: true},
		{name: "no-dispatch hold", change: func(s *tracker.GitHubIssueSnapshot) { s.Body += "\n<!-- detent:no-dispatch -->" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "scheduled-import")
			prompt := definition + "## Scheduled CI repair contract\n"
			if !test.noDelegation {
				prompt += delegation + "\n"
			}
			prompt += strings.ReplaceAll(criteria, "## ", "### ")
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte(prompt), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: " + repository + "\n  lanes:\n    - {name: Todo, role: active}\n    - {name: Backlog, role: holding}\n    - {name: Blocked, role: holding}\n    - {name: Done, role: terminal}\n"), HasConfig: true, ConfigPath: "detent.yaml"})
			if err != nil {
				t.Fatal(err)
			}
			workflow.Definition.Revision = strings.Repeat("a", 40)
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository=? WHERE id=?", repository, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at) VALUES ('R_example', 'example-org', 'example-project', ?, ?)", testTimestamp, testTimestamp); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id=(SELECT id FROM repositories WHERE github_node_id='R_example') WHERE id=?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			for number := 12; number <= 13; number++ {
				evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: fmt.Sprintf("%064x", number), RunID: "123", RunAttempt: "1", JobID: strconv.Itoa(number), JobName: "Verify", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: "failure"}
				evidence.JobURL = "https://github.com/" + repository + "/actions/jobs/" + evidence.JobID
				origin := issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: evidence.RunURL, Fingerprint: fmt.Sprintf("scheduled-job-%d", number)}
				snapshot := tracker.GitHubIssueSnapshot{URL: fmt.Sprintf("https://github.com/%s/issues/%d", repository, number), Title: "Scheduled Verify failure", Body: issueorigin.Stamp(criteria+"\n\n## Diagnostic evidence\nAssertion: expected healthy worker.", origin) + evidence.Stamp(), Provenance: tracker.Provenance{Provider: "github", ExternalID: fmt.Sprintf("I_%d", number), AuthorID: "github-actions[bot]", CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(), ObservedAt: time.Now()}}
				if test.change != nil {
					test.change(&snapshot)
				}
				f.service.config.ImportBackend = scheduledContractImporter(func(ctx context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
					if request.Stage != "scheduled" {
						return (linkedTestImporter{snapshot: snapshot}).FetchImportPage(ctx, request)
					}
					if request.Repository != repository || request.Scheduled == nil || request.Scheduled.JobID != evidence.JobID {
						t.Fatalf("verification request = %+v", request)
					}
					if test.unavailable {
						return GitHubImportPage{}, errors.New("run verification refused")
					}
					if test.stalePolicy {
						if _, err := f.service.database.db.ExecContext(ctx, "DELETE FROM project_policies WHERE scope=?", string(scope.organization)+"/"+string(scope.project)); err != nil {
							t.Fatal(err)
						}
					}
					if test.staleRepository {
						if _, err := f.service.database.db.ExecContext(ctx, "UPDATE projects SET checkout_repository='other-org/other-project' WHERE id=?", scope.project); err != nil {
							t.Fatal(err)
						}
					}
					return GitHubImportPage{Scheduled: &evidence}, nil
				})
				request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: strconv.Itoa(number)}, GitHubIssueURL: snapshot.URL, Labels: []string{"detent:todo", "hotfix"}}
				importReport := func(key string) tracker.NativeIssue {
					t.Helper()
					if !test.webhook {
						request.IdempotencyKey = key
						response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
						requireNativeStatus(t, response, http.StatusOK)
						var issue tracker.NativeIssue
						decodeHubResponse(t, response, &issue)
						return issue
					}
					payload, err := json.Marshal(map[string]any{
						"action":     "opened",
						"repository": map[string]any{"node_id": "R_example", "name": "example-project", "full_name": repository, "owner": map[string]string{"login": "example-org"}},
						"issue":      map[string]any{"node_id": snapshot.Provenance.ExternalID, "number": number, "title": snapshot.Title, "body": snapshot.Body, "html_url": snapshot.URL, "state": "open", "user": map[string]string{"login": snapshot.Provenance.AuthorID}, "labels": []map[string]string{{"name": "detent:todo"}, {"name": "hotfix"}}, "assignees": []string{}, "created_at": snapshot.Provenance.CreatedAt, "updated_at": snapshot.Provenance.UpdatedAt},
					})
					if err != nil {
						t.Fatal(err)
					}
					f.service.config.GitHubWebhookSecret = []byte(testWebhookSecret)
					requireNativeStatus(t, sendSignedWebhookRequest(t, f.service, key, "issues", string(payload)), http.StatusAccepted)
					var id string
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE project_id=? AND native_source_key=?", scope.project, "github:"+snapshot.URL).Scan(&id); err != nil {
						t.Fatal(err)
					}
					issue, _, err := readNativeIssue(t.Context(), f.service.database.reader, scope, id)
					if err != nil {
						t.Fatal(err)
					}
					return issue
				}
				issue := importReport(strconv.Itoa(number))
				if test.authorized {
					if issue.State != "Todo" || issue.Priority == nil || *issue.Priority != 0 || len(issue.IssueContract.ConfirmedSections) != 5 {
						t.Fatalf("admission = %+v", issue)
					}
					var id tracker.WorkItemID
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", issue.WorkItemID).Scan(&id); err != nil {
						t.Fatal(err)
					}
					ok, reason, err := nativeIssueContractClaimable(t.Context(), f.service.database.reader, scope, id)
					if err != nil || !ok || reason != "" {
						t.Fatalf("claim contract = %v %q %v", ok, reason, err)
					}
				} else if issue.State != "Triage" || issue.Priority != nil || len(issue.IssueContract.ConfirmedSections) != 0 {
					t.Fatalf("untrusted admission = %+v", issue)
				}
				if !test.authorized {
					contract, err := config.ResolvePolicyIssueContract(descriptor)
					if err != nil || contract.Evaluate(nativeContractIssue(issue)).Satisfied() {
						t.Fatalf("untrusted contract became eligible: %v", err)
					}
					var dispatchable bool
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT dispatchable FROM workflow_states WHERE project_id=? AND detent_state=?", scope.project, issue.State).Scan(&dispatchable); err != nil {
						t.Fatal(err)
					}
					if dispatchable {
						t.Fatal("untrusted import entered a dispatchable lane")
					}
					var reason string
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT json_extract(data_json,'$.reason') FROM collaboration_events WHERE work_item_id=? AND type='github.imported' ORDER BY sequence DESC LIMIT 1", issue.WorkItemID).Scan(&reason); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(reason, "Scheduled CI import") {
						t.Fatalf("missing explicit refusal: %q", reason)
					}
				}
				priority := "unset"
				if issue.Priority != nil {
					priority = strconv.Itoa(*issue.Priority)
				}
				t.Logf("source=%s native=%s lane=%s priority=%s confirmed_sections=%d", snapshot.URL, issue.WorkItemID, issue.State, priority, len(issue.IssueContract.ConfirmedSections))
				if issue.LinkedSource == nil || issue.LinkedSource.Status != "complete" || issue.LinkedSource.Snapshot.Body != snapshot.Body {
					t.Fatal("source evidence was lost")
				}
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("human-%d", number)}, ExpectedRevision: issue.Revision, Body: new(issueorigin.Stamp("Human decision: retain this hold. detent:no-dispatch", origin)), Priority: tracker.SetPriority(new(3))})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &issue)
				if test.existingState != "" {
					tx, err := f.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					issue.State = test.existingState
					issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: issue.State, Reason: "user_requested"}, time.Now())
					if err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						t.Fatal(err)
					}
				}
				if test.duplicate {
					snapshot.URL = fmt.Sprintf("https://github.com/%s/issues/%d", repository, number+100)
					snapshot.Provenance.ExternalID += "-duplicate"
					request.GitHubIssueURL = snapshot.URL
				}
				replay := importReport(fmt.Sprintf("%d-repeat", number))
				if replay.WorkItemID != issue.WorkItemID || replay.State != issue.State || !reflect.DeepEqual(replay.Priority, issue.Priority) || replay.Body != issue.Body {
					t.Fatalf("replay changed report: %+v", replay)
				}
				if test.duplicate {
					importReport(fmt.Sprintf("%d-third", number))
					var count int
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=? AND source_key=?", issue.WorkItemID, "github:scheduled:"+evidence.OccurrenceKey).Scan(&count); err != nil {
						t.Fatal(err)
					}
					want := 1
					if issue.Terminal {
						want = 0
					}
					if count != want {
						t.Fatalf("occurrence comments=%d want=%d", count, want)
					}
				}
				if test.staleRepository || test.stalePolicy {
					break
				}
			}
		})
	}
}
