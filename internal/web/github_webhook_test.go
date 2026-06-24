package web_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestGitHubWebhookVerifiesSignatureAndTargetsRefresh(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	refresher := &targetedRefreshProbe{
		response: web.RefreshResponse{
			Queued:      true,
			RequestedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		},
	}
	deps.Refresher = refresher
	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.GitHubWebhookSecret = "webhook-secret"
	server, err := web.NewServer(web.Config{KanbanWorkflow: workflowCfg}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := `{"repository":{"full_name":"digitaldrywood/detent"},"issue":{"number":666}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "delivery-1")
	req.Header.Set("X-Hub-Signature-256", githubWebhookSignature("webhook-secret", []byte(body)))
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s, want %d", rec.Code, rec.Body.String(), http.StatusAccepted)
	}
	if refresher.targetCalls != 1 {
		t.Fatalf("target refresh calls = %d, want 1", refresher.targetCalls)
	}
	if refresher.target.Repository != "digitaldrywood/detent" || refresher.target.IssueNumber != 666 {
		t.Fatalf("target = %#v, want digitaldrywood/detent#666", refresher.target)
	}
	if refresher.target.Event != "issues" || refresher.target.DeliveryID != "delivery-1" {
		t.Fatalf("target event metadata = %#v, want issues delivery-1", refresher.target)
	}

	var response web.RefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(response.Operations) == 0 || response.Operations[0] != "webhook:issues" {
		t.Fatalf("operations = %#v, want webhook:issues", response.Operations)
	}
}

func TestGitHubWebhookRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	refresher := &targetedRefreshProbe{}
	deps.Refresher = refresher
	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.GitHubWebhookSecret = "webhook-secret"
	server, err := web.NewServer(web.Config{KanbanWorkflow: workflowCfg}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	body := `{"repository":{"full_name":"digitaldrywood/detent"},"issue":{"number":666}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s, want %d", rec.Code, rec.Body.String(), http.StatusUnauthorized)
	}
	if refresher.calls != 0 || refresher.targetCalls != 0 {
		t.Fatalf("refresh calls = full %d targeted %d, want none", refresher.calls, refresher.targetCalls)
	}
}

func githubWebhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type targetedRefreshProbe struct {
	response    web.RefreshResponse
	calls       int
	targetCalls int
	target      web.RefreshTarget
}

func (p *targetedRefreshProbe) RequestRefresh(context.Context) (web.RefreshResponse, error) {
	p.calls++
	return p.response, nil
}

func (p *targetedRefreshProbe) RequestTargetedRefresh(_ context.Context, target web.RefreshTarget) (web.RefreshResponse, error) {
	p.targetCalls++
	p.target = target
	response := p.response
	response.Operations = append([]string{"webhook:" + target.Event}, response.Operations...)
	return response, nil
}
