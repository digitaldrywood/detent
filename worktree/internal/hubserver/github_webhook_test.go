package hubserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testWebhookSecret = "webhook-secret"

type issueProjectionSnapshot struct {
	Title           string
	Body            string
	State           string
	AuthorID        string
	LabelsJSON      string
	AssigneesJSON   string
	SourceVersion   string
	SourceUpdatedAt string
}

func readIssueProjectionSnapshot(t *testing.T, service *Service) issueProjectionSnapshot {
	t.Helper()
	var snapshot issueProjectionSnapshot
	if err := service.database.db.QueryRowContext(t.Context(), `
		SELECT title, body, github_state, author_login, labels_json, assignees_json, source_version, source_updated_at
		FROM issues WHERE github_node_id = 'I_issue'
	`).Scan(
		&snapshot.Title,
		&snapshot.Body,
		&snapshot.State,
		&snapshot.AuthorID,
		&snapshot.LabelsJSON,
		&snapshot.AssigneesJSON,
		&snapshot.SourceVersion,
		&snapshot.SourceUpdatedAt,
	); err != nil {
		t.Fatalf("read issue projection: %v", err)
	}
	return snapshot
}

func openWebhookTestService(t *testing.T, databasePath string, now func() time.Time, compatibility bool) *Service {
	t.Helper()
	service := openTestService(t, Config{
		DatabasePath:               databasePath,
		GitHubWebhookSecret:        []byte(testWebhookSecret),
		WebhookMaintenanceInterval: time.Hour,
		now:                        now,
	})
	if compatibility {
		seedWebhookCompatibilityProject(t, service)
	}
	return service
}

func seedWebhookCompatibilityProject(t *testing.T, service *Service) {
	t.Helper()
	if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO repositories(github_node_id,github_owner,github_name,created_at,updated_at) VALUES ('R_repo','digitaldrywood','detent',?,?) ON CONFLICT DO NOTHING`, testTimestamp, testTimestamp); err != nil {
		t.Fatal(err)
	}
	var repositoryID int64
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM repositories WHERE github_owner='digitaldrywood' AND github_name='detent'").Scan(&repositoryID); err != nil {
		t.Fatal(err)
	}
	seedCompatibilityProject(t, service.database.db, repositoryID)
}

func sendSignedWebhookRequest(t *testing.T, service *Service, deliveryID string, event string, payload string) *httptest.ResponseRecorder {
	t.Helper()
	return sendWebhookRequest(t, service, deliveryID, event, payload, signWebhookPayload(testWebhookSecret, payload))
}

func sendWebhookRequest(t *testing.T, service *Service, deliveryID string, event string, payload string, signature string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-Hub-Signature-256", signature)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func signWebhookPayload(secret string, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func completeIssueWebhookPayload(t *testing.T, title string, updatedAt string) string {
	t.Helper()
	payload := map[string]any{
		"action": "edited",
		"repository": map[string]any{
			"id":         100,
			"node_id":    "R_repo",
			"name":       "detent",
			"full_name":  "digitaldrywood/detent",
			"owner":      map[string]any{"login": "digitaldrywood"},
			"updated_at": updatedAt,
		},
		"issue": map[string]any{
			"id":         200,
			"node_id":    "I_issue",
			"number":     2069,
			"title":      title,
			"body":       "Issue body",
			"html_url":   "https://github.com/digitaldrywood/detent/issues/2069",
			"state":      "open",
			"user":       map[string]any{"login": "octocat"},
			"labels":     []any{map[string]any{"name": "feature"}, map[string]any{"name": "detent:todo"}},
			"assignees":  []any{map[string]any{"login": "example-user"}},
			"created_at": "2026-09-01T12:00:00Z",
			"updated_at": updatedAt,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode issue webhook payload: %v", err)
	}
	return string(encoded)
}

func issueWebhookPayloadWithAction(t *testing.T, action string, title string, updatedAt string) string {
	t.Helper()
	payload := make(map[string]any)
	if err := json.Unmarshal([]byte(completeIssueWebhookPayload(t, title, updatedAt)), &payload); err != nil {
		t.Fatalf("decode issue webhook payload: %v", err)
	}
	payload["action"] = action
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode issue webhook payload: %v", err)
	}
	return string(encoded)
}

func completePullRequestWebhookPayload(t *testing.T, title string, updatedAt string) string {
	t.Helper()
	payload := map[string]any{
		"action": "edited",
		"repository": map[string]any{
			"id":         100,
			"node_id":    "R_repo",
			"name":       "detent",
			"full_name":  "digitaldrywood/detent",
			"owner":      map[string]any{"login": "digitaldrywood"},
			"updated_at": updatedAt,
		},
		"pull_request": map[string]any{
			"id":         300,
			"node_id":    "PR_node",
			"number":     2070,
			"title":      title,
			"html_url":   "https://github.com/digitaldrywood/detent/pull/2070",
			"state":      "open",
			"draft":      false,
			"head":       map[string]any{"ref": "feature", "sha": "abc123"},
			"base":       map[string]any{"ref": "main", "sha": "def456"},
			"created_at": "2026-09-01T12:00:00Z",
			"updated_at": updatedAt,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode pull request webhook payload: %v", err)
	}
	return string(encoded)
}
