package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestIntakeWebhookCreatesAndUpdatesWithoutResettingPromotedState(t *testing.T) {
	t.Parallel()

	server, connector, refresher := newIntakeWebhookServer(t, "level:error")
	first := intakeRequest(`{"summary":"Database down","details":"Primary failed","fingerprint":"db-1","level":"error"}`, "secret")
	firstRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusCreated {
		t.Fatalf("first status = %d body = %s", firstRecorder.Code, firstRecorder.Body.String())
	}
	issues, err := connector.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].State != "Backlog" {
		t.Fatalf("issues = %#v, want one Backlog issue", issues)
	}
	if err := connector.UpdateIssueState(context.Background(), issues[0].ID, "Todo"); err != nil {
		t.Fatalf("UpdateIssueState() error = %v", err)
	}

	second := intakeRequest(`{"summary":"Database still down","details":"Replica failed","fingerprint":"db-1","level":"error"}`, "secret")
	secondRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d body = %s", secondRecorder.Code, secondRecorder.Body.String())
	}
	issues, err = connector.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 1 || issues[0].State != "Todo" || issues[0].Title != "[alerts] Database still down" {
		t.Fatalf("updated issues = %#v, want one promoted updated issue", issues)
	}
	if refresher.targetCalls != 2 {
		t.Fatalf("target refresh calls = %d, want 2", refresher.targetCalls)
	}
}

func TestIntakeWebhookRejectsInvalidToken(t *testing.T) {
	t.Parallel()

	server, connector, _ := newIntakeWebhookServer(t, "")
	request := intakeRequest(`{"summary":"Database down","fingerprint":"db-1"}`, "wrong")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	issues, err := connector.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v, want none", issues)
	}
}

func TestIntakeWebhookAcceptsUnmatchedEventWithoutCreatingIssue(t *testing.T) {
	t.Parallel()

	server, connector, _ := newIntakeWebhookServer(t, "level:error")
	request := intakeRequest(`{"summary":"Deploy complete","fingerprint":"deploy-1","level":"info"}`, "secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	issues, err := connector.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v, want none", issues)
	}
}

func TestIntakeWebhookRejectsEventWithoutFingerprint(t *testing.T) {
	t.Parallel()

	server, _, _ := newIntakeWebhookServer(t, "")
	request := intakeRequest(`{"summary":"Database down"}`, "secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func newIntakeWebhookServer(t *testing.T, match string) (*web.Server, *memory.Connector, *targetedRefreshProbe) {
	t.Helper()

	workflow := workflowconfig.Default()
	workflow.Tracker.Kind = workflowconfig.TrackerGitHub
	workflow.Tracker.APIKey = "test-token"
	workflow.Tracker.GitHubStatusSource = workflowconfig.GitHubStatusSourceLabel
	workflow.Tracker.Repository = "example/repo"
	workflow.Intake = intake.Config{Sources: []intake.Source{{
		Name:     "alerts",
		Kind:     intake.KindWebhook,
		Secret:   "$INTAKE_SECRET",
		Match:    match,
		DedupeBy: "fingerprint",
		Creates: intake.Creates{
			Status: "Backlog",
			Labels: []string{"bug"},
			Title:  "[{source}] {summary}",
			Body:   "{details}",
		},
	}}}
	connector := memory.New(memory.Config{Stateful: true})
	trackedProject, err := project.New(project.Config{
		Project: globalconfig.Project{ID: "example", Workdir: t.TempDir(), Weight: 1},
		Workflow: workflowconfig.Workflow{
			Config: workflow,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{Connector: connector})
	if err != nil {
		t.Fatalf("project.New() error = %v", err)
	}

	deps := testDeps(t)
	if err := deps.Registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
	refresher := &targetedRefreshProbe{}
	deps.Refresher = refresher
	server, err := web.NewServer(web.Config{LookupEnv: func(key string) string {
		if key == "INTAKE_SECRET" {
			return "secret"
		}
		return ""
	}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server, connector, refresher
}

func intakeRequest(body string, token string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/intake/example/alerts", strings.NewReader(body))
	request.Header.Set("X-Detent-Intake-Token", token)
	request.Header.Set("Content-Type", "application/json")
	return request
}
