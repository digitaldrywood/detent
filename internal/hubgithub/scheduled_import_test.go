package hubgithub

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledWebhookImportEndToEnd(t *testing.T) {
	t.Parallel()
	const repository = "example-org/example-project"
	const token = "isolated-scheduled-import-administrator-token"
	const secret = "isolated-scheduled-import-webhook-secret"
	const criteria = "## Acceptance criteria\nReproduce and minimally repair.\n\n## Must not break\nPreserve intended behavior.\n\n## How we know it worked\nRun make check-land.\n\n## Invariants touched\nPreserve INV-3 and INV-7.\n\n## Safety regression\nPreserve existing regressions."
	for _, test := range []struct{ name, workflow, state, sourceRepository, repository, branch, headRepository string }{
		{"authorized CI", ".github/workflows/ci.yml", "Todo", repository, repository, "develop", repository},
		{"mixed case source URL", ".github/workflows/ci.yml", "Todo", "Example-Org/Example-Project", repository, "develop", repository},
		{"authorized release CI", ".github/workflows/ci.yml", "Todo", repository, repository, "release/v1.0", repository},
		{"other workflow", ".github/workflows/other.yml", "Triage", repository, repository, "develop", repository},
		{"authorized alternate CI", ".github/workflows/ci.yaml", "Todo", "getparable/parable", "getparable/parable", "dev", "getparable/parable"},
		{"alternate CI mixed case repository", ".github/workflows/ci.yaml", "Todo", "GetParable/Parable", "getparable/parable", "dev", "getparable/parable"},
		{"alternate CI wrong workflow", ".github/workflows/ci.yml", "Triage", "getparable/parable", "getparable/parable", "dev", "getparable/parable"},
		{"alternate CI arbitrary workflow", ".github/workflows/other.yaml", "Triage", "getparable/parable", "getparable/parable", "dev", "getparable/parable"},
		{"alternate CI legacy workflow and branch", ".github/workflows/ci.yml", "Triage", "getparable/parable", "getparable/parable", "develop", "getparable/parable"},
		{"alternate CI wrong branch", ".github/workflows/ci.yaml", "Triage", "getparable/parable", "getparable/parable", "develop", "getparable/parable"},
		{"alternate CI release branch", ".github/workflows/ci.yaml", "Triage", "getparable/parable", "getparable/parable", "release/v1.0", "getparable/parable"},
		{"alternate CI feature branch", ".github/workflows/ci.yaml", "Triage", "getparable/parable", "getparable/parable", "feature/repair", "getparable/parable"},
		{"alternate CI fork", ".github/workflows/ci.yaml", "Triage", "getparable/parable", "getparable/parable", "dev", "example-org/example-fork"},
		{"default CI wrong branch", ".github/workflows/ci.yml", "Triage", repository, repository, "dev", repository},
		{"alternate CI wrong repository", ".github/workflows/ci.yaml", "Triage", repository, repository, "dev", repository},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := test.repository
			owner, name, _ := strings.Cut(repository, "/")
			sha := strings.Repeat("a", 40)
			evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("b", 64), RunID: "123", RunAttempt: "1", JobID: "456", JobName: "Verify", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/1", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: sha, Conclusion: "failure"}
			body := issueorigin.Stamp(criteria+"\n\n## Diagnostic evidence\nExpected a healthy worker; got cancellation.", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: evidence.RunURL, Fingerprint: "scheduled-verify"}) + evidence.Stamp()
			path := "/repos/" + repository + "/actions/runs/123/attempts/1"
			client := &scriptedRESTClient{t: t, steps: []restStep{
				{method: http.MethodGet, path: "/repos/" + repository, response: map[string]any{"id": 1, "node_id": "R_example", "name": name, "owner": map[string]string{"login": owner}, "updated_at": "2026-10-01T00:00:00Z"}},
				{method: http.MethodGet, path: path, response: map[string]any{"id": 123, "run_attempt": 1, "path": test.workflow, "head_sha": sha, "head_branch": test.branch, "event": "schedule", "repository": map[string]string{"full_name": repository}, "head_repository": map[string]string{"full_name": test.headRepository}}},
			}}
			if test.state == "Todo" {
				client.steps = append(client.steps, restStep{method: http.MethodGet, path: path + "/jobs?per_page=100", response: map[string]any{"jobs": []map[string]any{{"id": 456, "run_id": 123, "name": "Verify", "head_sha": sha, "status": "completed", "conclusion": "failure", "started_at": "2026-10-01T00:00:00Z", "completed_at": "2026-10-01T00:01:00Z"}}}})
			}
			service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte(token), GitHubWebhookSecret: []byte(secret), ImportBackend: NewImporter(client), ReconcileBackend: NewReconciler(client), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte("## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n- Invariants touched\n- Safety regression\n\n## Scheduled CI repair contract\nNew verified GitHub Actions CI repair reports enter Todo with Urgent priority.\n" + strings.ReplaceAll(criteria, "## ", "### ")), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: " + repository + "\n  lanes:\n    - {name: Backlog, role: holding}\n    - {name: Blocked, role: holding}\n    - {name: Todo, role: active}\n    - {name: Done, role: terminal}\n"), HasConfig: true, ConfigPath: "detent.yaml"})
			if err != nil {
				t.Fatal(err)
			}
			workflow.Definition.Revision = sha
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			api(http.MethodPut, base+"/policy", policy.Change{Policy: descriptor}, nil)
			payload, err := json.Marshal(map[string]any{"action": "opened", "repository": map[string]any{"node_id": "R_example", "name": name, "full_name": repository, "owner": map[string]string{"login": owner}}, "issue": map[string]any{"node_id": "I_report", "number": 12, "title": "Scheduled Verify failure", "body": body, "html_url": "https://github.com/" + test.sourceRepository + "/issues/12", "state": "open", "user": map[string]string{"login": "github-actions[bot]"}, "labels": []map[string]string{{"name": "hotfix"}, {"name": "detent:todo"}}, "assignees": []string{}, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z"}})
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
				if issue.Priority == nil || *issue.Priority != 0 || len(issue.IssueContract.ConfirmedSections) != 5 {
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
