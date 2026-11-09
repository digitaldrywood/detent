package nativechange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
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
	cfg := testsupport.NativeChangePostgresConfig(t, hubserver.Config{Clock: time.Now, Content: newHubContentStore(), RunDetails: &archivedRunDetails{}, InitialAdminToken: []byte(admin)})
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work-items") {
			if err := testsupport.NativeChangeRefreshBoard(r.Context(), service, tracker.OrganizationID(cfg.Hosted.OrganizationID)); err != nil {
				t.Errorf("refresh fixture board: %v", err)
				http.Error(w, "fixture board unavailable", 500)
				return
			}
		}
		service.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := newTestClient(t, Config{URL: server.URL, TokenSource: func() string { return admin }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}
	project := testsupport.NativeChangeProject(t, cfg, states, false)
	workerToken := testsupport.NativeChangeWorker(t, cfg, project.ID, testsupport.NativeChangeMachineID)

	descriptor := clientTestPolicy()
	adminNative, err := client.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminNative.ApproveProjectPolicy(t.Context(), policy.Change{Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	workerClient, err := newTestClient(t, Config{URL: server.URL, TokenSource: func() string { return workerToken }, HTTPClient: &http.Client{Transport: nativeOnlyTransport{next: server.Client().Transport, host: parsed.Host}}})
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
	ownerConnector, err := NewNativeConnector(adminNative)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("Complete worker context.\n", 60) + issueContractTestSections
	issue, err := ownerConnector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Native work", Body: body, Labels: []string{"native"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"assignee", func(ctx context.Context) error { return ownerConnector.SetAssignee(ctx, issue.ID, "worker") }},
		{"title", func(ctx context.Context) error {
			return ownerConnector.SetField(ctx, issue.ID, "title", "Updated work")
		}},
		{"priority", func(ctx context.Context) error { return ownerConnector.SetField(ctx, issue.ID, "priority", "1") }},
		{"body", func(ctx context.Context) error { return ownerConnector.UpdateIssueBody(ctx, issue.ID, body+"Edited") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	blocker, err := ownerConnector.CreateIssue(t.Context(), connector.IssueDraft{Title: "Blocker", Body: "blocking" + issueContractTestSections})
	if err != nil {
		t.Fatal(err)
	}
	if err := ownerConnector.AddIssueDependency(t.Context(), issue.ID, blocker.ID); err != nil {
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
	_, err = adminNative.UpdateIssue(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: 1, Title: &staleTitle})
	var conflict *APIError
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict || conflict.CurrentRevision != nativeIssue.Revision {
		t.Fatalf("typed conflict = %#v, error = %v", conflict, err)
	}
	if err := ownerConnector.RemoveIssueDependency(t.Context(), issue.ID, blocker.ID); err != nil {
		t.Fatal(err)
	}
	blockerNow, err := adminNative.Issue(t.Context(), tracker.NativeWorkItemID(blocker.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminNative.Transition(t.Context(), blockerNow.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: blockerNow.Revision, State: "Done", Reason: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	confirmIssueContract(t, adminNative, issue.ID)
	scheduler, err := newTestScheduler(workerClient, SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"local": project.ID},
		Machine: Machine{ID: testsupport.NativeChangeMachineID, Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	provided, ok := scheduler.ConnectorForProject("local")
	if !ok || provided.Name() != "hub_native" {
		t.Fatal("native project did not receive native connector")
	}
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"}})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %#v, error = %v", candidates, err)
	}
	if candidates[0].ID != issue.ID || candidates[0].Description != body+"Edited" {
		t.Fatalf("candidate = %#v", candidates[0])
	}
	claim, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
	if err != nil || claim.Owner != string(testsupport.NativeChangeMachineID) {
		t.Fatalf("claim = %#v, error = %v", claim, err)
	}
	conn = provided.(*NativeConnector)
	exerciseNativeExecution(t, scheduler, native, issue.ID, func(ctx context.Context) {
		for index := range 23 {
			if err := conn.CreateComment(ctx, issue.ID, fmt.Sprintf("Discussion %d", index)); err != nil {
				t.Fatal(err)
			}
		}
		comments, err := conn.FetchIssueComments(t.Context(), issue)
		if err != nil || len(comments) != 23 {
			t.Fatalf("comments = %d, error = %v", len(comments), err)
		}
		if err := conn.UpdateIssueComment(ctx, issue.ID, comments[len(comments)-1].ID, "Updated discussion"); err != nil {
			t.Fatal(err)
		}

		recovery, err := native.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil {
			t.Fatal(err)
		}
		scheduler.RunExecution(issue.ID).(*nativeExecution).recovery.Store(&recovery)

	})

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
	if err := conn.UpdateIssueState(t.Context(), issue.ID, "Done"); err != nil {
		t.Fatal(err)
	}
	schedulerLease := scheduler.RunExecution(issue.ID).(*nativeExecution).claim.lease
	if err := scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := conn.CreateComment(api.WithMutationAuthority(t.Context(), native.Scope(), schedulerLease, nil), issue.ID, "stale worker comment"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("released worker mutation = %v, want permanent authority loss", err)
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
