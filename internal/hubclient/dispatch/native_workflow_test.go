package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkflowWriteReleasesStaleFencing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		failure   string
		successor bool
	}{
		{name: "two stale leases", failure: "stale_fencing_token"},
		{name: "current token"},
		{name: "network retry", failure: "network"},
		{name: "server retry", failure: "server"},
		{name: "uncertain release retains authority", failure: "release_network"},
		{name: "server release failure retains authority", failure: "release_server"},
		{name: "delayed release preserves successor", failure: "stale_fencing_token", successor: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			leases := []tracker.NativeLease{
				{ID: "lease_first", WorkItemID: "wi_first", FencingToken: 1},
				{ID: "lease_second", WorkItemID: "wi_second", FencingToken: 2},
			}
			writes := make(map[tracker.NativeWorkItemID]int)
			releases := make(map[tracker.LeaseID]int)
			var native *NativeClient
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				status, payload := http.StatusOK, `{}`
				if strings.HasSuffix(request.URL.Path, "/workflow") {
					var transition tracker.Transition
					if err := json.NewDecoder(request.Body).Decode(&transition); err != nil {
						return nil, err
					}
					id := tracker.NativeWorkItemID(strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, native.Scope()+"/work-items/"), "/workflow"))
					writes[id]++
					for _, lease := range leases {
						if lease.WorkItemID == id && (transition.LeaseID != lease.ID || transition.FencingToken != lease.FencingToken) {
							return nil, errors.New("workflow lost lease fencing")
						}
					}
					if writes[id] == 1 {
						switch test.failure {
						case "stale_fencing_token":
							status, payload = http.StatusConflict, `{"code":"stale_fencing_token"}`
						case "network":
							return nil, errors.New("connection reset by peer")
						case "server":
							status, payload = http.StatusServiceUnavailable, `{"code":"unavailable"}`
						}
					}
				}
				if strings.HasSuffix(request.URL.Path, "/release") {
					for _, lease := range leases {
						if !strings.HasSuffix(request.URL.Path, "/leases/"+string(lease.ID)+"/release") {
							continue
						}
						releases[lease.ID]++
						if releases[lease.ID] == 1 {
							switch test.failure {
							case "release_network":
								return nil, errors.New("connection reset by peer")
							case "release_server":
								status, payload = http.StatusServiceUnavailable, `{"code":"unavailable"}`
							}
						}
						if test.successor {
							lease.ID, lease.FencingToken = "successor_"+lease.ID, lease.FencingToken+10
							native.Transport().RememberLease(native.Scope(), lease.WorkItemID, lease)
						}
					}
					if test.failure == "stale_fencing_token" {
						status, payload = http.StatusConflict, `{"code":"stale_fencing_token"}`
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: request}, nil
			})
			client, err := newTestClient(t, Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := newTestScheduler(client, SchedulerConfig{OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"local": "prj_test"}, Machine: Machine{ID: "machine_test", Hostname: "host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			source := scheduler.nativeProject("local")
			native = source.NativeClient()
			for _, lease := range leases {
				scheduler.nativeClaims[string(lease.WorkItemID)] = nativeClaim{source: source, lease: lease}
				scheduler.claims[string(lease.WorkItemID)] = nativeTrackerLease(lease)
				client.RememberLease(native.Scope(), lease.WorkItemID, lease)
			}
			for _, lease := range leases {
				request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("workflow_%s", lease.WorkItemID)}, State: "In Review"}
				_, err := native.Transition(t.Context(), lease.WorkItemID, request)
				lost := test.failure == "stale_fencing_token"
				if errors.Is(err, runner.ErrExecutionAuthorityUnavailable) != lost {
					t.Fatalf("workflow authority classification = %v", err)
				}
				if test.failure == "network" || test.failure == "server" {
					if !errors.Is(err, ErrUnavailable) || native.FencedMutation(t.Context(), lease.WorkItemID, tracker.Mutation{}).FencingToken != lease.FencingToken {
						t.Fatalf("transient failure discarded retry authority: %v", err)
					}
					if _, err = native.Transition(t.Context(), lease.WorkItemID, request); err != nil {
						t.Fatalf("retry failed: %v", err)
					}
				} else if !lost && err != nil {
					t.Fatal(err)
				}
				releaseErr := scheduler.ReleaseClaim(t.Context(), string(lease.WorkItemID), "completed")
				if strings.HasPrefix(test.failure, "release_") {
					if !errors.Is(releaseErr, ErrUnavailable) || native.FencedMutation(t.Context(), lease.WorkItemID, tracker.Mutation{}).FencingToken != lease.FencingToken {
						t.Fatalf("uncertain release discarded local authority: %v", releaseErr)
					}
					releaseErr = scheduler.ReleaseClaim(t.Context(), string(lease.WorkItemID), "completed")
				}
				if releaseErr != nil {
					t.Fatal(releaseErr)
				}
				fence := native.FencedMutation(t.Context(), lease.WorkItemID, tracker.Mutation{})
				if test.successor {
					if fence.LeaseID != "successor_"+lease.ID || fence.FencingToken != lease.FencingToken+10 {
						t.Fatalf("delayed release discarded current authority: %+v", fence)
					}
				} else if fence.LeaseID != "" || fence.FencingToken != 0 {
					t.Errorf("released attempt retained workflow authority: %+v", fence)
				}
				wantWrites := 1
				if test.failure == "network" || test.failure == "server" {
					wantWrites = 2
				}
				if writes[lease.WorkItemID] != wantWrites {
					t.Errorf("workflow requests = %d, want %d", writes[lease.WorkItemID], wantWrites)
				}
			}
			if len(scheduler.nativeClaims) != 0 || len(scheduler.claims) != 0 {
				t.Fatal("workflow completion retained local claims")
			}
		})
	}
}

func TestNativeLaneEntrySurvivesConnectorRestart(t *testing.T) {
	var clock atomic.Int64
	initial := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	clock.Store(initial.UnixNano())
	h := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "In Review"}},
		{Name: "In Review", Transitions: []string{"In Progress"}},
	}, true, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Clock: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
	reopen := func() *NativeConnector {
		t.Helper()
		client, err := newTestClient(t, fixtureClient(h.admin.Transport()).config)
		if err != nil {
			t.Fatal(err)
		}
		native, err := client.Native(h.organization, h.project)
		if err != nil {
			t.Fatal(err)
		}
		connector, err := NewNativeConnector(native)
		if err != nil {
			t.Fatal(err)
		}
		return connector
	}
	first := h.createInProgress(t, "First work item")
	second := h.createInProgress(t, "Second lane entry")
	for _, source := range []*NativeConnector{h.connector, reopen()} {
		for _, issue := range []connector.Issue{first, second} {
			entry, found, err := source.CurrentLaneEntry(t.Context(), issue)
			if err != nil || found || !entry.EnteredAt.IsZero() {
				t.Fatalf("creation invented a persisted lane transition: %+v, %t, %v", entry, found, err)
			}
		}
	}
	for _, issue := range []connector.Issue{first, second} {
		history, err := h.admin.HistoryPage(t.Context(), tracker.NativeWorkItemID(issue.ID), "", 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range history.Items {
			if event.Type == "workflow.transitioned" {
				t.Fatalf("lane observation wrote a workflow transition for %s: %+v", issue.ID, event)
			}
		}
	}
	transition := func(issue connector.Issue, state string, at time.Time) connector.Issue {
		t.Helper()
		clock.Store(at.UnixNano())
		current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil {
			t.Fatal(err)
		}
		updated, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: state, Reason: "user_requested"})
		if err != nil {
			t.Fatal(err)
		}
		return issueFromNative(updated)
	}
	firstEntered := initial.Add(time.Hour)
	first = transition(first, "Todo", firstEntered)
	secondEntered := initial.Add(2 * time.Hour)
	second = transition(second, "Todo", secondEntered)
	assertEntry := func(conn *NativeConnector, issue connector.Issue, expected time.Time) {
		t.Helper()
		entry, found, err := conn.CurrentLaneEntry(t.Context(), issue)
		if err != nil || !found || !entry.EnteredAt.Equal(expected) {
			t.Fatalf("lane entry for %s = %+v, %t, %v; want %s", issue.ID, entry, found, err, expected)
		}
		if entry.Actor.Kind == "" || entry.Actor.Login == "" {
			t.Fatalf("lane entry lost its persisted actor for %s: %+v", issue.ID, entry.Actor)
		}
	}
	for range 2 {
		assertEntry(h.connector, first, firstEntered)
		assertEntry(h.connector, second, secondEntered)
	}
	first = transition(first, "In Progress", initial.Add(3*time.Hour))
	firstEntered = initial.Add(4 * time.Hour)
	first = transition(first, "Todo", firstEntered)
	assertEntry(h.connector, first, firstEntered)
	assertEntry(h.connector, second, secondEntered)
	clock.Store(initial.Add(5 * time.Hour).UnixNano())
	current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(first.ID))
	if err != nil {
		t.Fatal(err)
	}
	title := "Updated first work item"
	updated, err := h.admin.UpdateIssue(t.Context(), current.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	first = issueFromNative(updated)
	for range 2 {
		reopened := reopen()
		assertEntry(reopened, first, firstEntered)
		assertEntry(reopened, second, secondEntered)
	}
}
