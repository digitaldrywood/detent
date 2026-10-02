package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeAdmissionBatch(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		slots     int
		budget    int
		waiting   int
		preview   bool
		failAfter int
		failClaim int
		want      int
	}{
		{name: "six slots", slots: 6, budget: 14, preview: true, want: 6},
		{name: "project slots", slots: 2, budget: 10, preview: true, want: 2},
		{name: "one evaluation budget", slots: 6, budget: 3, preview: true, want: 3},
		{name: "waiting head", slots: 6, budget: 6, waiting: 4, preview: true, want: 2},
		{name: "direct claims", slots: 6, budget: 14, want: 6},
		{name: "hydration releases whole batch", slots: 6, budget: 14, preview: true, failAfter: 2},
		{name: "claim failure releases partial batch", slots: 6, budget: 14, preview: true, failClaim: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, true)
			h.scheduler.machine.Capacity = 6
			for i := range 8 {
				if _, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: fmt.Sprintf("work-%d", i), State: "Todo"}); err != nil {
					t.Fatal(err)
				}
			}
			evaluations := 0
			var readyIDs []string
			request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}, AdmissionLimit: test.slots, CandidateLimit: test.budget}
			if test.preview {
				request.CandidateReady = func(_ context.Context, issue connector.Issue) bool {
					evaluations++
					if evaluations <= test.waiting {
						return false
					}
					readyIDs = append(readyIDs, issue.ID)
					return true
				}
			}
			transport := h.failChanges.next
			recoveries := 0
			claims := 0
			h.failChanges.next = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") {
					claims++
					if claims == test.failClaim {
						return nil, errors.New("injected claim failure")
					}
				}
				if strings.HasSuffix(r.URL.Path, "/comments") {
					recoveries++
					if recoveries == test.failAfter {
						return nil, errors.New("injected hydration failure")
					}
				}
				return transport.RoundTrip(r)
			})
			issues, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
			if test.failAfter != 0 || test.failClaim != 0 {
				if err == nil || len(issues) != 0 {
					t.Fatalf("hydration failure = %d issues, %v", len(issues), err)
				}
				h.failChanges.next = transport
				readyIDs = nil
				issues, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
				if err != nil || len(issues) != 6 {
					t.Fatalf("failed batch retained leases: %d issues, %v", len(issues), err)
				}
			} else if err != nil || len(issues) != test.want || evaluations > test.budget {
				t.Fatalf("admitted=%d evaluated=%d error=%v, want %d", len(issues), evaluations, err, test.want)
			}
			seen := make(map[string]bool)
			sessions := make(map[string]bool)
			for i, issue := range issues {
				lease := h.scheduler.nativeClaims[issue.ID].lease
				if seen[issue.ID] || sessions[lease.SessionID] || lease.FencingToken == 0 || lease.PolicyID != h.descriptor.ID {
					t.Fatalf("batch lost distinct fenced claims: %+v", lease)
				}
				seen[issue.ID], sessions[lease.SessionID] = true, true
				if test.preview && issue.ID != readyIDs[i] {
					t.Fatalf("queue order = %s at %d, want %s", issue.ID, i, readyIDs[i])
				}
				if _, err := h.scheduler.AdoptClaim(t.Context(), issue, time.Now()); err != nil {
					t.Fatal(err)
				}
				execution := h.scheduler.RunExecution(issue.ID)
				if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "running" {
					t.Fatalf("batch start missing: %+v, %v", recovery.Attempts, err)
				}
			}
			for _, issue := range issues {
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "dispatch_deferred"); err != nil {
					t.Fatal(err)
				}
			}
			request.CandidateReady = nil
			if again, err := h.scheduler.FetchCandidateIssues(t.Context(), request); err != nil || len(again) != min(test.slots, test.budget) {
				t.Fatalf("released batch did not free slots: %d, %v", len(again), err)
			}
		})
	}
}

func TestNativeAdmissionCompetingRunners(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, true)
	h.scheduler.machine.Capacity = 6
	other, err := NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: "other-machine", Hostname: "other-host", Version: "test", Capacity: 6}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if _, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: fmt.Sprintf("work-%d", i), State: "Todo"}); err != nil {
			t.Fatal(err)
		}
	}
	ready := make(chan struct{})
	var previews atomic.Int32
	transport := h.failChanges.next
	h.failChanges.next = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(r)
		if strings.HasSuffix(r.URL.Path, "/claims/preview") {
			if previews.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return response, err
	})
	type result struct {
		issues []connector.Issue
		err    error
	}
	results := make(chan result, 2)
	for _, scheduler := range []*Scheduler{h.scheduler, other} {
		go func() {
			issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}, AdmissionLimit: 6, CandidateLimit: 14, CandidateReady: func(context.Context, connector.Issue) bool { return true }})
			results <- result{issues, err}
		}()
	}
	seen := make(map[string]bool)
	for range 2 {
		result := <-results
		if result.err != nil && !errors.Is(result.err, ErrNoClaimableWork) {
			t.Fatal(result.err)
		}
		for _, issue := range result.issues {
			if seen[issue.ID] {
				t.Fatalf("competing runners both admitted %s", issue.ID)
			}
			seen[issue.ID] = true
		}
	}
	if len(seen) != 8 {
		t.Fatalf("competing batches admitted %d distinct issues, want 8", len(seen))
	}
}
