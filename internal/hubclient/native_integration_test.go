package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeOnlyTransport struct {
	next http.RoundTripper
	host string
}

func (t nativeOnlyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != t.host || !strings.HasPrefix(request.URL.Path, "/api/v2/") {
		return nil, fmt.Errorf("non-native networking is disabled: %s", request.URL.Redacted())
	}
	return t.next.RoundTrip(request)
}

func TestNativeSchedulerAndConnectorWithoutGitHub(t *testing.T) {
	t.Parallel()
	const admin = "native-integration-admin"
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte(admin)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return admin }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	if err := client.request(t.Context(), http.MethodGet, "/api/v2/organizations", nil, &organizations); err != nil {
		t.Fatal(err)
	}
	organization := organizations.Items[0].ID
	var project tracker.NativeProject
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}
	if err := client.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", map[string]any{"name": "native", "idempotency_key": "project", "states": states, "require_dependencies": false}, &project); err != nil {
		t.Fatal(err)
	}
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := client.request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "native-worker", "scope": "worker"}, &token); err != nil {
		t.Fatal(err)
	}
	if err := client.request(t.Context(), http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": organization, "project_id": project.ID}, nil); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	workerClient, err := New(Config{URL: server.URL, TokenSource: func() string { return token.Token }, HTTPClient: &http.Client{Transport: nativeOnlyTransport{next: server.Client().Transport, host: parsed.Host}}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := workerClient.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("Complete worker context.\n", 60)
	issue, err := conn.CreateIssue(t.Context(), connector.IssueDraft{Title: "Native work", Body: body, Labels: []string{"native"}})
	if err != nil {
		t.Fatal(err)
	}
	for index := range 23 {
		if err := conn.CreateComment(t.Context(), issue.ID, fmt.Sprintf("Discussion %d", index)); err != nil {
			t.Fatal(err)
		}
	}
	comments, err := conn.FetchIssueComments(t.Context(), issue)
	if err != nil || len(comments) != 23 {
		t.Fatalf("comments = %d, error = %v", len(comments), err)
	}
	if err := conn.UpdateIssueComment(t.Context(), issue.ID, comments[0].ID, "Updated discussion"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"assignee", func(ctx context.Context) error { return conn.SetAssignee(ctx, issue.ID, "worker") }},
		{"title", func(ctx context.Context) error { return conn.SetField(ctx, issue.ID, "title", "Updated work") }},
		{"priority", func(ctx context.Context) error { return conn.SetField(ctx, issue.ID, "priority", "1") }},
		{"body", func(ctx context.Context) error { return conn.UpdateIssueBody(ctx, issue.ID, body+"Edited") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	blocker, err := conn.CreateIssue(t.Context(), connector.IssueDraft{Title: "Blocker", Body: "blocking"})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.AddIssueDependency(t.Context(), issue.ID, blocker.ID); err != nil {
		t.Fatal(err)
	}
	nativeIssue, err := native.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil || len(nativeIssue.Dependencies) != 1 {
		t.Fatalf("stored dependencies = %#v, error = %v", nativeIssue.Dependencies, err)
	}
	dispatchIssue, err := conn.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(dispatchIssue) != 1 || len(dispatchIssue[0].BlockedBy) != 0 {
		t.Fatalf("disabled readiness still blocked: %#v, error = %v", dispatchIssue, err)
	}
	staleTitle := "stale edit"
	_, err = native.UpdateIssue(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: 1, Title: &staleTitle})
	var conflict *APIError
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict || conflict.CurrentRevision != nativeIssue.Revision {
		t.Fatalf("typed conflict = %#v, error = %v", conflict, err)
	}
	if err := conn.RemoveIssueDependency(t.Context(), issue.ID, blocker.ID); err != nil {
		t.Fatal(err)
	}
	if err := conn.UpdateIssueState(t.Context(), blocker.ID, "Done"); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(workerClient, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"local": project.ID},
		Machine: Machine{ID: "machine-native", Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	provided, ok := scheduler.ConnectorForProject("local")
	if !ok || provided.Name() != "hub_native" {
		t.Fatal("native project did not receive native connector")
	}
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", WorkflowStates: []string{"Todo"}})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %#v, error = %v", candidates, err)
	}
	if candidates[0].ID != issue.ID || candidates[0].Description != body+"Edited" {
		t.Fatalf("candidate = %#v", candidates[0])
	}
	claim, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
	if err != nil || claim.Owner != "machine-native" {
		t.Fatalf("claim = %#v, error = %v", claim, err)
	}
	if _, err := scheduler.RenewClaim(t.Context(), issue.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := conn.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
		t.Fatal(err)
	}
	refreshed, err := conn.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
	if err != nil || len(refreshed) != 1 || refreshed[0].State != "In Progress" {
		t.Fatalf("refreshed = %#v, error = %v", refreshed, err)
	}
	if err := scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := conn.UpdateIssueState(t.Context(), issue.ID, "Done"); err != nil {
		t.Fatal(err)
	}
	events, err := conn.FetchIssueEvents(t.Context(), issue)
	if err != nil || len(events) < 30 {
		t.Fatalf("events = %d, error = %v", len(events), err)
	}
	for _, event := range events {
		if event.Body != "" {
			t.Fatal("history duplicated content into event payload")
		}
	}
	ready, err := conn.FetchCandidateIssues(t.Context())
	if err != nil || len(ready) != 0 {
		t.Fatalf("ready = %#v, error = %v", ready, err)
	}
}
