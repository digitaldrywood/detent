package hubgithub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledWebhookImportEndToEnd(t *testing.T) {
	t.Parallel()
	databasePath := testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
		return hubserver.Open(ctx, hubserver.Config{DatabasePath: path, GitHubDisabled: true, Logger: slog.New(slog.DiscardHandler)})
	})
	const repository = "example-org/example-project"
	const token = "isolated-scheduled-import-administrator-token"
	const secret = "isolated-scheduled-import-webhook-secret"
	const defaultCriteria = "## Acceptance criteria\nReproduce and minimally repair.\n\n## Must not break\nPreserve intended behavior.\n\n## How we know it worked\nRun make check-land.\n\n## Invariants touched\nPreserve INV-3 and INV-7.\n\n## Safety regression\nPreserve existing regressions."
	const alternate = "New verified GitHub Actions CI repair reports from `.github/workflows/ci.yaml` on `dev` enter Todo with Urgent priority."
	for _, test := range []struct {
		name, workflow, branch, state, delegation, sourceRepository, headRepository, runRepository string
		shortContract, runOrigin                                                                   bool
		reportIdentity                                                                             bool
		native                                                                                     bool
		priority                                                                                   int
	}{
		{name: "native approved project", native: true, workflow: ".github/workflows/ci.yml", branch: "develop", state: "Todo"},
		{name: "native portable release contract", priority: 1, native: true, shortContract: true, runOrigin: true, workflow: ".github/workflows/validated-release.yml", branch: "release/v2.1", state: "Todo", delegation: "New verified GitHub Actions CI repair reports from `.github/workflows/validated-release.yml` on `release/v*` enter Todo with High priority."},
		{name: "release family rejects develop", native: true, workflow: ".github/workflows/validated-release.yml", branch: "develop", state: "Triage", delegation: "New verified GitHub Actions CI repair reports from `.github/workflows/validated-release.yml` on `release/v*` enter Todo with High priority."},
		{name: "release family rejects nested branch", native: true, workflow: ".github/workflows/validated-release.yml", branch: "release/v2.1/feature", state: "Triage", delegation: "New verified GitHub Actions CI repair reports from `.github/workflows/validated-release.yml` on `release/v*` enter Todo with High priority."},
		{name: "release family rejects other workflow", native: true, workflow: ".github/workflows/ci.yml", branch: "release/v2.1", state: "Triage", delegation: "New verified GitHub Actions CI repair reports from `.github/workflows/validated-release.yml` on `release/v*` enter Todo with High priority."},
		{name: "native exact release branch", native: true, workflow: ".github/workflows/ci.yaml", branch: "release/v2.0", state: "Todo", delegation: "New verified GitHub Actions CI repair reports from `.github/workflows/ci.yaml` on `release/v2.0` enter Todo with Urgent priority."},
		{name: "native unapproved workflow", native: true, workflow: ".github/workflows/other.yml", branch: "develop", state: "Triage"},
		{name: "native unapproved branch", native: true, workflow: ".github/workflows/ci.yaml", branch: "release/v2.0", state: "Triage", delegation: alternate},
		{name: "native report identity cannot authorize", native: true, workflow: ".github/workflows/ci.yaml", branch: "dev", state: "Triage", reportIdentity: true},
		{name: "authorized CI", workflow: ".github/workflows/ci.yml", branch: "develop", state: "Todo"},
		{name: "mixed case source URL", workflow: ".github/workflows/ci.yml", branch: "develop", state: "Todo", sourceRepository: "Example-Org/Example-Project"},
		{name: "authorized release CI", workflow: ".github/workflows/ci.yml", branch: "release/v1.0", state: "Todo"},
		{name: "other workflow", workflow: ".github/workflows/other.yml", branch: "develop", state: "Triage"},
		{name: "authorized alternate CI", workflow: ".github/workflows/ci.yaml", branch: "dev", state: "Todo", delegation: alternate},
		{name: "alternate CI mixed case repository", workflow: ".github/workflows/ci.yaml", branch: "dev", state: "Todo", delegation: alternate, sourceRepository: "Example-Org/Example-Project"},
		{name: "alternate CI without approved identity", workflow: ".github/workflows/ci.yaml", branch: "dev", state: "Triage"},
		{name: "report identity does not authorize", workflow: ".github/workflows/ci.yaml", branch: "dev", state: "Triage", reportIdentity: true},
		{name: "alternate CI wrong workflow", workflow: ".github/workflows/ci.yml", branch: "dev", state: "Triage", delegation: alternate},
		{name: "alternate CI arbitrary workflow", workflow: ".github/workflows/other.yaml", branch: "dev", state: "Triage", delegation: alternate},
		{name: "alternate CI legacy workflow and branch", workflow: ".github/workflows/ci.yml", branch: "develop", state: "Triage", delegation: alternate},
		{name: "alternate CI wrong branch", workflow: ".github/workflows/ci.yaml", branch: "develop", state: "Triage", delegation: alternate},
		{name: "alternate CI release branch", workflow: ".github/workflows/ci.yaml", branch: "release/v1.0", state: "Triage", delegation: alternate},
		{name: "alternate CI feature branch", workflow: ".github/workflows/ci.yaml", branch: "feature/repair", state: "Triage", delegation: alternate},
		{name: "alternate CI fork", workflow: ".github/workflows/ci.yaml", branch: "dev", state: "Triage", delegation: alternate, headRepository: "example-org/example-fork"},
		{name: "alternate CI wrong repository", workflow: ".github/workflows/ci.yaml", branch: "dev", state: "Triage", delegation: alternate, runRepository: "example-org/other-project"},
		{name: "default CI wrong branch", workflow: ".github/workflows/ci.yml", branch: "dev", state: "Triage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			criteria := defaultCriteria
			definition := "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n- Invariants touched\n- Safety regression\n"
			sectionCount := 5
			if test.shortContract {
				criteria, _, _ = strings.Cut(criteria, "\n\n## Invariants touched")
				definition, sectionCount = "", 3
			}
			sourceRepository, headRepository, runRepository := repository, repository, repository
			if test.sourceRepository != "" {
				sourceRepository = test.sourceRepository
			}
			if test.headRepository != "" {
				headRepository = test.headRepository
			}
			if test.runRepository != "" {
				runRepository = test.runRepository
			}
			owner, name, _ := strings.Cut(repository, "/")
			sha := strings.Repeat("a", 40)
			evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("b", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Verify", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: sha, Conclusion: "failure"}
			source := evidence.RunURL
			if test.runOrigin {
				source = strings.TrimSuffix(source, "/attempts/1")
			}
			body := issueorigin.Stamp(criteria+"\n\n## Diagnostic evidence\nExpected a healthy worker; got cancellation.", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: source, Fingerprint: "scheduled-verify"}) + evidence.Stamp()
			if test.reportIdentity {
				body += "\n" + alternate
			}
			path := "/repos/" + repository + "/actions/runs/123/attempts/1"
			client := &scriptedRESTClient{t: t, steps: []restStep{
				{method: http.MethodGet, path: "/repos/" + repository, response: map[string]any{"id": 1, "node_id": "R_example", "name": name, "owner": map[string]string{"login": owner}, "updated_at": "2026-10-01T00:00:00Z"}},
				{method: http.MethodGet, path: path, response: map[string]any{"id": 123, "run_attempt": 1, "path": test.workflow, "head_sha": sha, "head_branch": test.branch, "event": "schedule", "repository": map[string]string{"full_name": runRepository}, "head_repository": map[string]string{"full_name": headRepository}}},
			}}
			if test.state == "Todo" {
				client.steps = append(client.steps, restStep{method: http.MethodGet, path: path + "/jobs?per_page=100", response: map[string]any{"jobs": []map[string]any{{"id": 456, "run_id": 123, "name": "Verify", "head_sha": sha, "status": "completed", "conclusion": "failure", "started_at": "2026-10-01T00:00:00Z", "completed_at": "2026-10-01T00:01:00Z"}}}})
			}
			service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: databasePath(t), InitialAdminToken: []byte(token), GitHubWebhookSecret: []byte(secret), ImportBackend: NewImporter(client), ReconcileBackend: NewReconciler(client), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			api := func(method, path string, input, output any) {
				t.Helper()
				raw, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(method, path, bytes.NewReader(raw))
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
				}
				if output != nil {
					if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
						t.Fatal(err)
					}
				}
			}
			var organizations tracker.Page[struct {
				ID tracker.OrganizationID `json:"organization_id"`
			}]
			api(http.MethodGet, "/api/v2/organizations", nil, &organizations)
			if len(organizations.Items) != 1 {
				t.Fatalf("organizations=%+v", organizations.Items)
			}
			base := "/api/v2/organizations/" + string(organizations.Items[0].ID) + "/projects"
			var project tracker.NativeProject
			api(http.MethodPost, base, map[string]any{"idempotency_key": "project", "name": "Scheduled import fixture", "states": []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}}, &project)
			base += "/" + string(project.ID)
			api(http.MethodPost, base+"/integration/repository", map[string]string{"idempotency_key": "bind", "expected_revision": "1", "repository": repository}, nil)
			delegation := test.delegation
			if delegation == "" {
				delegation = "New verified GitHub Actions CI repair reports enter Todo with Urgent priority."
			}
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte(definition + "\n## Scheduled CI repair contract\n" + delegation + "\n" + strings.ReplaceAll(criteria, "## ", "### ")), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: " + repository + "\n  lanes:\n    - {name: Backlog, role: holding}\n    - {name: Blocked, role: holding}\n    - {name: Todo, role: active}\n    - {name: Done, role: terminal}\n"), HasConfig: true, ConfigPath: "detent.yaml"})
			if err != nil {
				t.Fatal(err)
			}
			workflow.Definition.Revision = sha
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			api(http.MethodPut, base+"/policy", policy.Change{Policy: descriptor}, nil)
			if test.native {
				raw, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "native-report"}, Title: "Scheduled Verify failure", Body: strings.Replace(body, criteria, "Failure diagnostic", 1), State: "Todo"})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, base+"/work-items", bytes.NewReader(raw))
				request.Header.Set("Authorization", "Bearer "+token)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				if test.state != "Todo" {
					if response.Code != http.StatusUnprocessableEntity {
						t.Fatalf("unauthorized native report: %d %s", response.Code, response.Body.String())
					}
					client.assertDone()
					return
				}
				if response.Code != http.StatusOK {
					t.Fatalf("native report: %d %s", response.Code, response.Body.String())
				}
			} else {
				payload, err := json.Marshal(map[string]any{"action": "opened", "repository": map[string]any{"node_id": "R_example", "name": name, "full_name": repository, "owner": map[string]string{"login": owner}}, "issue": map[string]any{"node_id": "I_report", "number": 12, "title": "Scheduled Verify failure", "body": body, "html_url": "https://github.com/" + sourceRepository + "/issues/12", "state": "open", "user": map[string]string{"login": "github-actions[bot]"}, "labels": []map[string]string{{"name": "hotfix"}, {"name": "detent:todo"}}, "assignees": []string{}, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z"}})
				if err != nil {
					t.Fatal(err)
				}
				signature := hmac.New(sha256.New, []byte(secret))
				if _, err := signature.Write(payload); err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
				request.Header.Set("X-GitHub-Delivery", "scheduled-import")
				request.Header.Set("X-GitHub-Event", "issues")
				request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(signature.Sum(nil)))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusAccepted {
					t.Fatalf("webhook: %d %s", response.Code, response.Body.String())
				}
			}
			var issues tracker.Page[tracker.NativeIssue]
			api(http.MethodGet, base+"/work-items", nil, &issues)
			if len(issues.Items) != 1 {
				t.Fatalf("issues=%+v", issues.Items)
			}
			issue := issues.Items[0]
			api(http.MethodGet, base+"/work-items/"+string(issue.WorkItemID), nil, &issue)
			if issue.State != test.state {
				t.Fatalf("lane=%s want=%s", issue.State, test.state)
			}
			if test.state == "Todo" {
				if (issue.Priority == nil || *issue.Priority != test.priority) || len(issue.IssueContract.ConfirmedSections) != sectionCount {
					t.Fatalf("admission=%+v", issue)
				}
				contract, err := config.ResolvePolicyIssueContract(descriptor)
				if err != nil {
					t.Fatal(err)
				}
				if evaluation := contract.Evaluate(connector.Issue{Title: issue.Title, Description: issue.Body, IssueContract: issue.IssueContract}); !evaluation.Satisfied() {
					t.Fatalf("claim contract=%+v", evaluation)
				}
			} else if issue.Priority != nil || len(issue.IssueContract.ConfirmedSections) != 0 {
				t.Fatalf("unauthorized admission=%+v", issue)
			}
			t.Logf("verified_import native=%s state=%s policy=%s tested_commit=%s workflow=%s branch=%s repository=%s", issue.WorkItemID, issue.State, descriptor.ID, sha, test.workflow, test.branch, repository)
			client.assertDone()
		})
	}
}
