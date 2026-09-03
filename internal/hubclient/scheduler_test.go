package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSchedulerDispatchCycleUsesHub(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	lease := tracker.Lease{LeaseSummary: tracker.LeaseSummary{
		ID: "lease-1", FencingToken: 7, Machine: tracker.MachineSummary{ID: "machine-a", Hostname: "host-a"},
		SessionID: "session-1", AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(90 * time.Second),
	}, WorkItemID: 42}
	machine := Machine{ID: "machine-a", Hostname: "host-a", DisplayName: "Build Mac", Capacity: 2, Version: "v1.2.3"}
	item := WorkItem{WorkItem: tracker.WorkItem{
		ID: 42, Repository: tracker.RepositoryReference{ID: 4, Owner: "acme", Name: "widgets"},
		GitHub: tracker.GitHubIssueReference{NodeID: "I_42", Number: 17}, Title: "Ship Hub scheduling",
		BodyExcerpt: "```detent-agent\nschema: 1\neffort: high\n```", URL: "https://github.com/acme/widgets/issues/17",
		SourceState: tracker.SourceStateOpen, WorkflowState: &tracker.WorkflowState{Name: "Todo", Dispatchable: true},
		AuthorID: "alice", Labels: []string{"detent:todo"}, Dispatchability: tracker.Dispatchability{Dispatchable: true}, SyncStatus: tracker.SyncStatusSynced,
	}, Body: "Full issue body from the Hub"}
	var mu sync.Mutex
	var paths []string
	var claims []ClaimRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer worker-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		mu.Lock()
		paths = append(paths, request.Method+" "+request.URL.Path)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/machines/register", "/api/v1/machines/machine-a/heartbeat":
			_ = json.NewEncoder(response).Encode(machine)
		case "/api/v1/claims":
			var claim ClaimRequest
			if err := json.NewDecoder(request.Body).Decode(&claim); err != nil {
				t.Errorf("decode claim: %v", err)
			}
			claims = append(claims, claim)
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(lease)
		case "/api/v1/work-items/42":
			_ = json.NewEncoder(response).Encode(item)
		case "/api/v1/leases/lease-1/renew":
			renewed := lease
			renewed.RenewedAt = now
			renewed.ExpiresAt = now.Add(90 * time.Second)
			_ = json.NewEncoder(response).Encode(renewed)
		case "/api/v1/leases/lease-1/release":
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "worker-token" }})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{
		Machine: machine, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second,
		Now: func() time.Time { return now }, SessionID: func() (string, error) { return "session-1", nil },
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{
		ProjectID: "widgets", Repository: "acme/widgets", WorkflowStates: []string{"Todo"},
		Filter: connector.IssueFilterHint{
			Authors: []string{"alice"}, Assignees: []string{"worker-a"},
			LabelInclude: []string{"detent:todo"}, LabelExclude: []string{"hold"},
		},
	})
	if err != nil || len(issues) != 1 {
		t.Fatalf("FetchCandidateIssues() = %#v, %v", issues, err)
	}
	if issues[0].ID != "I_42" || issues[0].Identifier != "acme/widgets#17" || issues[0].Description != item.Body || issues[0].AuthorID != "alice" {
		t.Fatalf("candidate = %#v", issues[0])
	}
	claimed, err := scheduler.AdoptClaim(t.Context(), issues[0], now)
	if err != nil || claimed.Owner != "machine-a" || claimed.LeaseExpiresAt != lease.ExpiresAt {
		t.Fatalf("AdoptClaim() = %#v, %v", claimed, err)
	}
	now = now.Add(31 * time.Second)
	renewed, err := scheduler.RenewClaim(t.Context(), issues[0].ID, now)
	if err != nil || renewed.LeaseRenewedAt != now {
		t.Fatalf("RenewClaim() = %#v, %v", renewed, err)
	}
	if renewed.Issue.Labels != nil || renewed.Issue.Assignees != nil || renewed.Issue.BlockedBy != nil || renewed.Issue.Fields != nil {
		t.Fatalf("RenewClaim() issue = %#v, want sparse lease metadata", renewed.Issue)
	}
	if err := scheduler.ReleaseClaim(t.Context(), issues[0].ID, "completed"); err != nil {
		t.Fatalf("ReleaseClaim() error = %v", err)
	}

	wantPaths := []string{
		"POST /api/v1/machines/register",
		"POST /api/v1/claims",
		"GET /api/v1/work-items/42",
		"POST /api/v1/machines/machine-a/heartbeat",
		"POST /api/v1/leases/lease-1/renew",
		"POST /api/v1/leases/lease-1/release",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("Hub requests = %v, want %v", paths, wantPaths)
	}
	if len(claims) != 1 || !reflect.DeepEqual(claims[0].Repositories, []string{"acme/widgets"}) || claims[0].SessionID != "session-1" ||
		!reflect.DeepEqual(claims[0].Authors, []string{"alice"}) || !reflect.DeepEqual(claims[0].Assignees, []string{"worker-a"}) ||
		!reflect.DeepEqual(claims[0].LabelInclude, []string{"detent:todo"}) || !reflect.DeepEqual(claims[0].LabelExclude, []string{"hold"}) {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestSchedulerHubOutageIsUnavailableBeforeClaim(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
	}{
		{
			name: "service unavailable",
			handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(http.StatusServiceUnavailable)
				_, _ = response.Write([]byte(`{"code":"database_unavailable","message":"try later"}`))
			}),
		},
		{
			name: "invalid response",
			handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = response.Write([]byte("not-json"))
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "worker-token" }})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			scheduler, err := NewScheduler(client, SchedulerConfig{
				Machine:           Machine{ID: "machine-a", Hostname: "host-a", Capacity: 1, Version: "dev"},
				HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second,
			})
			if err != nil {
				t.Fatalf("NewScheduler() error = %v", err)
			}
			issues, err := scheduler.FetchCandidateIssues(context.Background(), orchestrator.SchedulingRequest{Repository: "acme/widgets"})
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(issues) != 0 {
				t.Fatalf("FetchCandidateIssues() = %#v, %v", issues, err)
			}
			if strings.Contains(err.Error(), "worker-token") {
				t.Fatalf("error leaked token: %v", err)
			}
		})
	}
}
