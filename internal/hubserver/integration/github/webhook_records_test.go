package github_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const webhookRecordToken = "example-webhook-admin"
const webhookRecordSecret = "example-webhook-secret"
const webhookRecordOrganization tracker.OrganizationID = "org_example"

const webhookRecordPayload = `{"action":"edited","repository":{"full_name":"example-org/example-project"},"issue":{"node_id":"I_source","number":1,"title":"Updated source","body":"Updated source body","html_url":"https://github.com/example-org/example-project/issues/1","state":"open","user":{"login":"source-author"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z","labels":[],"assignees":[]},"comment":{"node_id":"C_source","body":"Source comment","user":{"login":"source-author"},"created_at":"2026-11-02T00:00:00Z","updated_at":"2026-11-02T00:00:00Z"}}`

type webhookRecordImporter struct{}

func (webhookRecordImporter) FetchImportPage(_ context.Context, request githubcontract.GitHubImportRequest) (githubcontract.GitHubImportPage, error) {
	if request.Stage != "issue" {
		return githubcontract.GitHubImportPage{}, nil
	}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return githubcontract.GitHubImportPage{Issue: &githubcontract.IssueSource{NodeID: "I_source", Number: request.IssueNumber, URL: fmt.Sprintf("https://github.com/%s/issues/%d", request.Repository, request.IssueNumber), Title: "Source title", Body: "Complete source body", AuthorID: "source-author", CreatedAt: now, UpdatedAt: now}}, nil
}

type webhookRecordFixture struct {
	service    *hubserver.Service
	repository *postgres.Repository
	now        *time.Time
	items      map[tracker.ProjectID]tracker.NativeWorkItemID
}

func newWebhookRecordFixture(t *testing.T) webhookRecordFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	cfg, err := gitHubServiceConfig(t, hubserver.Config{InitialAdminToken: []byte(webhookRecordToken), GitHubWebhookSecret: []byte(webhookRecordSecret), ImportBackend: webhookRecordImporter{}, Clock: func() time.Time { return now }, WebhookMaintenanceInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.Open(t.Context(), *cfg.Postgres, postgresFiles(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	now = time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture := webhookRecordFixture{service: service, repository: repository, now: &now, items: make(map[tracker.ProjectID]tracker.NativeWorkItemID)}
	for _, project := range []tracker.ProjectID{"first", "second"} {
		hubtest.Project(t, repository, webhookRecordOrganization, hubtest.ProjectOptions{ID: project, States: projectstore.TriageStates(hubtest.States())})
		if err := repository.WriteTenant(t.Context(), webhookRecordOrganization, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), project)
			if err != nil {
				return err
			}
			record.Settings.Repository, record.Settings.Intake = "example-org/example-project", "manual"
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var issue tracker.NativeIssue
		fixture.api(t, http.MethodPost, fixture.base(project)+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "seed:" + string(project)}, GitHubIssueURL: "https://github.com/example-org/example-project/issues/1", State: "Triage"}, &issue)
		fixture.items[project] = issue.WorkItemID
	}
	return fixture
}
func (f webhookRecordFixture) base(project tracker.ProjectID) string {
	return "/api/v2/organizations/" + string(webhookRecordOrganization) + "/projects/" + string(project)
}
func (f webhookRecordFixture) api(t *testing.T, method, path string, input, output any) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+webhookRecordToken)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
	}
	if output != nil {
		if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
			t.Fatal(err)
		}
	}
}
func (f webhookRecordFixture) send(t *testing.T, event, action string) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(webhookRecordPayload), &payload); err != nil {
		t.Fatal(err)
	}
	payload["action"] = action
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(webhookRecordSecret))
	if _, err := mac.Write(raw); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Delivery", "delivery")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookReceiptsAcrossProjects(t *testing.T) {
	f := newWebhookRecordFixture(t)
	for _, project := range []tracker.ProjectID{"first", "second"} {
		f.send(t, "issues", "edited")
		var issue tracker.NativeIssue
		f.api(t, http.MethodGet, f.base(project)+"/work-items/"+string(f.items[project]), nil, &issue)
		if issue.Title != "Updated source" || issue.Body != "Updated source body" {
			t.Fatalf("project=%s title=%q body=%q", project, issue.Title, issue.Body)
		}
	}
}

func TestGitHubWebhookCursorRecoveryKeepsObservation(t *testing.T) {
	for _, test := range []struct{ event, action string }{{"issues", "edited"}, {"issue_comment", "created"}, {"issues", "closed"}, {"issues", "reopened"}} {
		t.Run(test.event+"/"+test.action, func(t *testing.T) {
			f := newWebhookRecordFixture(t)
			f.send(t, test.event, test.action)
			var row projectstore.GitHubWebhook
			if err := f.repository.WriteTenant(t.Context(), webhookRecordOrganization, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				owner := w.(projectstore.GitHubIntakeWriter)
				row, err = owner.GitHubWebhook(t.Context(), "delivery", "")
				if err != nil {
					return err
				}
				previous, attempts := row.AfterProject, row.Attempts
				row.AfterProject = ""
				return owner.UpdateGitHubWebhook(t.Context(), row, previous, attempts)
			}); err != nil {
				t.Fatal(err)
			}
			*f.now = f.now.Add(time.Hour)
			f.send(t, test.event, test.action)
			if err := f.repository.ReadTenant(t.Context(), webhookRecordOrganization, corestore.Serving, func(r tenantstore.Reader) error {
				cursor, err := r.(projectstore.GitHubIntakeReader).GitHubWebhook(t.Context(), row.DeliveryID, "")
				if err != nil {
					return err
				}
				if cursor.AfterProject != "first" || cursor.ReceivedAt != row.ReceivedAt {
					t.Fatalf("cursor=%+v", cursor)
				}
				comments, err := r.SelectComments(t.Context(), "first", f.items["first"], 0, false, 10)
				if err == nil && (test.event == "issue_comment" || test.action == "closed" || test.action == "reopened") && len(comments) != 1 {
					t.Fatalf("replayed comments=%+v", comments)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
