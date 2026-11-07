package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeAdmissionBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	database := testsupport.DispatchPostgresDatabase(t)
	base := hubserver.Config{Clock: time.Now, Conversation: &hubserver.ConversationConfig{Enabled: true}, RunDetails: &archivedRunDetails{}, Content: newHubContentStore()}
	for _, test := range []struct {
		name             string
		slots            int
		budget           int
		waiting          int
		preview          bool
		failAfter        int
		failClaim        int
		want             int
		failCode         string
		failStatus       int
		failPreview      bool
		oversized        bool
		emptyPreview     bool
		wantError        bool
		cancelDiscovery  bool
		cancelParent     bool
		cancelRelease    bool
		missingIsolation bool
		refusalBody      string
	}{
		{name: "late missing isolation with failed release", slots: 6, budget: 14, preview: true, missingIsolation: true, wantError: true},
		{name: "late claim unauthorized oversized body releases batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "refusal", failStatus: http.StatusUnauthorized, refusalBody: "oversized", wantError: true},
		{name: "late claim forbidden oversized body releases batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "refusal", failStatus: http.StatusForbidden, refusalBody: "oversized", wantError: true},
		{name: "late claim badrequest oversized body releases batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "refusal", failStatus: http.StatusBadRequest, refusalBody: "oversized", wantError: true},
		{name: "late claim unauthorized failing body releases batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "refusal", failStatus: http.StatusUnauthorized, refusalBody: "failing", wantError: true},
		{name: "late claim forbidden failing body releases batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "refusal", failStatus: http.StatusForbidden, refusalBody: "failing", wantError: true},
		{name: "late claim badrequest failing body releases batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "refusal", failStatus: http.StatusBadRequest, refusalBody: "failing", wantError: true},
		{name: "late hydration unauthorized oversized body releases batch", slots: 6, budget: 14, preview: true, failAfter: 2, failCode: "refusal", failStatus: http.StatusUnauthorized, refusalBody: "oversized", wantError: true},
		{name: "late hydration forbidden oversized body releases batch", slots: 6, budget: 14, preview: true, failAfter: 2, failCode: "refusal", failStatus: http.StatusForbidden, refusalBody: "oversized", wantError: true},
		{name: "late hydration badrequest oversized body releases batch", slots: 6, budget: 14, preview: true, failAfter: 2, failCode: "refusal", failStatus: http.StatusBadRequest, refusalBody: "oversized", wantError: true},
		{name: "late hydration unauthorized failing body releases batch", slots: 6, budget: 14, preview: true, failAfter: 2, failCode: "refusal", failStatus: http.StatusUnauthorized, refusalBody: "failing", wantError: true},
		{name: "late hydration forbidden failing body releases batch", slots: 6, budget: 14, preview: true, failAfter: 2, failCode: "refusal", failStatus: http.StatusForbidden, refusalBody: "failing", wantError: true},
		{name: "late hydration badrequest failing body releases batch", slots: 6, budget: 14, preview: true, failAfter: 2, failCode: "refusal", failStatus: http.StatusBadRequest, refusalBody: "failing", wantError: true},
		{name: "six slots", slots: 6, budget: 14, preview: true, want: 6},
		{name: "oversized recovery preserves both candidates", slots: 2, budget: 2, preview: true, oversized: true, want: 2},
		{name: "bounded hydration failure releases both candidates", slots: 2, budget: 2, preview: true, oversized: true, failAfter: 2, want: 2, wantError: true},
		{name: "project slots", slots: 2, budget: 10, preview: true, want: 2},
		{name: "one evaluation budget", slots: 6, budget: 3, preview: true, want: 3},
		{name: "waiting head", slots: 6, budget: 6, waiting: 4, preview: true, want: 2},
		{name: "direct claims", slots: 6, budget: 14, want: 6},
		{name: "hydration retains prefix", slots: 6, budget: 14, preview: true, failAfter: 2, want: 1},
		{name: "direct hydration retains prefix", slots: 6, budget: 14, failAfter: 2, want: 1},
		{name: "hydration cleanup cancellation releases each lease once", slots: 6, budget: 14, preview: true, failAfter: 2, cancelRelease: true, wantError: true},
		{name: "claim failure retains partial batch", slots: 6, budget: 14, preview: true, failClaim: 3, want: 2},
		{name: "direct claim failure retains partial batch", slots: 6, budget: 14, failClaim: 3, want: 2},
		{name: "preview discovery cancellation retains claims", slots: 6, budget: 14, preview: true, cancelDiscovery: true, want: 2},
		{name: "direct discovery cancellation is unused", slots: 6, budget: 14, cancelDiscovery: true, want: 6},
		{name: "preview parent cancellation releases batch", slots: 6, budget: 14, preview: true, cancelParent: true, wantError: true},
		{name: "direct parent cancellation releases batch", slots: 6, budget: 14, cancelParent: true, wantError: true},
		{name: "direct policy denial releases batch", slots: 6, budget: 14, failClaim: 3, failCode: "claim_not_permitted", failStatus: http.StatusForbidden, wantError: true},
		{name: "direct policy mismatch releases batch", slots: 6, budget: 14, failClaim: 3, failCode: "policy_mismatch", wantError: true},
		{name: "preview late unauthorized releases batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "unauthorized", failStatus: http.StatusUnauthorized, wantError: true},
		{name: "direct late unauthorized releases batch", slots: 6, budget: 14, failClaim: 3, failCode: "unauthorized", failStatus: http.StatusUnauthorized, wantError: true},
		{name: "preview hydration unauthorized releases batch", slots: 6, budget: 14, preview: true, failAfter: 2, failCode: "unauthorized", failStatus: http.StatusUnauthorized, wantError: true},
		{name: "direct hydration unauthorized releases batch", slots: 6, budget: 14, failAfter: 2, failCode: "unauthorized", failStatus: http.StatusUnauthorized, wantError: true},
		{name: "direct provider full", slots: 6, budget: 14, failClaim: 1, failCode: "provider_capacity"},
		{name: "direct runner full", slots: 6, budget: 14, failClaim: 1, failCode: "runner_capacity"},
		{name: "direct host full", slots: 6, budget: 14, failClaim: 1, failCode: "host_capacity"},
		{name: "preview provider full", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "provider_capacity"},
		{name: "preview runner full", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "runner_capacity"},
		{name: "preview host full", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "host_capacity"},
		{name: "preview claim provider full", slots: 6, budget: 14, preview: true, failClaim: 1, failCode: "provider_capacity"},
		{name: "preview claim runner full", slots: 6, budget: 14, preview: true, failClaim: 1, failCode: "runner_capacity"},
		{name: "preview claim host full", slots: 6, budget: 14, preview: true, failClaim: 1, failCode: "host_capacity"},
		{name: "empty provider preview strict provider full", slots: 6, budget: 14, preview: true, emptyPreview: true, failClaim: 1, failCode: "provider_capacity"},
		{name: "empty provider preview strict runner full", slots: 6, budget: 14, preview: true, emptyPreview: true, failClaim: 1, failCode: "runner_capacity"},
		{name: "empty provider preview strict host full", slots: 6, budget: 14, preview: true, emptyPreview: true, failClaim: 1, failCode: "host_capacity"},
		{name: "partial provider full", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "provider_capacity", want: 2},
		{name: "partial runner full", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "runner_capacity", want: 2},
		{name: "partial host full", slots: 6, budget: 14, failClaim: 3, failCode: "host_capacity", want: 2},
		{name: "direct plan full", slots: 6, budget: 14, failClaim: 1, failCode: "plan_capacity"},
		{name: "partial plan full", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "plan_capacity", want: 2},
		{name: "unauthorized", slots: 6, budget: 14, failClaim: 1, failCode: "unauthorized", failStatus: http.StatusUnauthorized, wantError: true},
		{name: "policy denied releases partial batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "claim_not_permitted", failStatus: http.StatusForbidden, wantError: true},
		{name: "unrelated conflict releases partial batch", slots: 6, budget: 14, preview: true, failClaim: 3, failCode: "policy_mismatch", wantError: true},
		{name: "draining", slots: 6, budget: 14, preview: true, failPreview: true, failCode: "runner_draining", wantError: true},
		{name: "disabled", slots: 6, budget: 14, failClaim: 1, failCode: "runner_disabled", wantError: true},
		{name: "offline", slots: 6, budget: 14, failClaim: 1, failCode: "runner_offline", wantError: true},
		{name: "malformed preview", slots: 6, budget: 14, preview: true, failPreview: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := testsupport.DispatchPostgresOrganizationConfig(t, base, database)
			h := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, true, false, config)
			fixtureCapacity(t, h, 6)
			if test.missingIsolation {
				fixtureEnableEnrollment(t, h)
			}
			count := 6
			if test.oversized {
				count = 2
			}
			bodies := make(map[string]string)
			for i := range count {
				body := issueContractTestSections
				if test.oversized && i == 0 {
					body += strings.Repeat("<", 145<<10) + strings.Repeat("b", (240<<10)-len(body)-(145<<10))
				}
				item := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: fmt.Sprintf("work-%d", i), Body: body, State: "Todo"})
				bodies[string(item.WorkItemID)] = body
				if test.oversized {
					for range 18 {
						if _, err := h.admin.CreateComment(t.Context(), item.WorkItemID, tracker.CreateComment{Mutation: nativeMutationKey(), Body: strings.Repeat("x", 60<<10)}); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			evaluations := 0
			var readyIDs []string
			request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo"}, AdmissionLimit: test.slots, CandidateLimit: test.budget}
			if test.emptyPreview {
				h.scheduler.providerReports = func() ([]providercapacity.Report, error) { return nil, nil }
				request.ProviderRequirement = func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
					t.Error("empty preview resolved a provider requirement")
					return providercapacity.Requirement{}, nil
				}
			}
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
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			discoveryCtx, cancelDiscovery := context.WithCancel(ctx)
			defer cancelDiscovery()
			request.BeginDiscovery = func(context.Context) (context.Context, context.CancelFunc) {
				return discoveryCtx, func() {}
			}
			acquired := make(map[string]tracker.NativeLease)
			released := make(map[string]bool)
			releaseAttempts := make(map[string]int)
			rejectedLease := ""
			bodyError := errors.New("injected response body failure")
			releaseError := errors.New("injected uncertain release failure")
			refusalResponse := func(response *httptest.ResponseRecorder) *http.Response {
				result := response.Result()
				switch test.refusalBody {
				case "oversized":
					result.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", maxResponseBytes+1)))
				case "failing":
					result.Body = io.NopCloser(iotest.ErrReader(bodyError))
				}
				return result
			}
			transport := h.failChanges.next
			recoveries := 0
			claims := 0
			h.failChanges.next = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/release") {
					parts := strings.Split(r.URL.Path, "/")
					id := parts[len(parts)-2]
					releaseAttempts[id]++
					if id == rejectedLease {
						return nil, releaseError
					}
				}
				if test.emptyPreview && strings.HasSuffix(r.URL.Path, "/claims/preview") {
					response := httptest.NewRecorder()
					response.WriteString(`{"items":[]}`)
					return response.Result(), nil
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") {
					claims++
					if claims == test.failClaim && test.failCode == "" {
						return nil, context.DeadlineExceeded
					}
				}
				if test.failPreview && strings.HasSuffix(r.URL.Path, "/claims/preview") || test.failCode != "" && claims == test.failClaim && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") {
					response := httptest.NewRecorder()
					if test.failCode == "" {
						response.WriteString(`{"items":`)
					} else {
						status := test.failStatus
						if status == 0 {
							status = http.StatusConflict
						}
						response.WriteHeader(status)
						fmt.Fprintf(response, `{"code":%q,"message":"injected refusal"}`, test.failCode)
					}
					return refusalResponse(response), nil
				}
				if r.Method == http.MethodGet && r.URL.Query().Get("view") == "recovery" {
					recoveries++
					if recoveries == test.failAfter {
						if test.oversized && r.URL.Query().Get("cursor") == "" {
							t.Fatal("hydration failure did not reach required body continuation")
						}
						if test.failCode != "" {
							response := httptest.NewRecorder()
							response.WriteHeader(test.failStatus)
							fmt.Fprintf(response, `{"code":%q}`, test.failCode)
							return refusalResponse(response), nil
						}
						return nil, errors.New("injected hydration failure")
					}
				}
				response, err := transport.RoundTrip(r)
				if err == nil && response.StatusCode == http.StatusOK && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") {
					payload, readErr := io.ReadAll(response.Body)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if err := response.Body.Close(); err != nil {
						t.Fatal(err)
					}
					response.Body = io.NopCloser(strings.NewReader(string(payload)))
					var lease tracker.NativeLease
					if err := json.Unmarshal(payload, &lease); err != nil {
						t.Fatal(err)
					}
					acquired[string(lease.ID)] = lease
					if test.missingIsolation && claims == 3 {

						rejectedLease = string(lease.ID)
						lease.IsolationPolicy = nil
						payload, err := json.Marshal(lease)
						if err != nil {
							t.Fatal(err)
						}
						response.Body = io.NopCloser(strings.NewReader(string(payload)))
					}
					if len(acquired) == 2 {
						if test.cancelDiscovery {
							cancelDiscovery()
						}
						if test.cancelParent {
							cancel()
						}
					}
				}
				if err == nil && response.StatusCode < 300 && strings.HasSuffix(r.URL.Path, "/release") {
					parts := strings.Split(r.URL.Path, "/")
					released[parts[len(parts)-2]] = true
					if test.cancelRelease {
						cancel()
					}
				}
				return response, err
			})
			issues, err := h.scheduler.FetchCandidateIssues(ctx, request)
			retained := make(map[string]bool)
			for _, issue := range issues {
				claim := h.scheduler.nativeClaims[issue.ID]
				retained[string(claim.lease.ID)] = true
				if claim.recovery.Issue.WorkItemID != tracker.NativeWorkItemID(issue.ID) {
					t.Fatal("returned an unhydrated claim")
				}
			}
			for id, lease := range acquired {
				_, cached := fixtureLeaseCached(t, h.native.Transport(), h.native.Scope(), lease.WorkItemID)
				if cached != retained[id] {
					t.Fatalf("lease cache differs from returned batch: cached=%t retained=%t", cached, retained[id])
				}
				if id == rejectedLease {
					if releaseAttempts[id] != 1 || retained[id] || released[id] {
						t.Fatalf("uncertain release retried or retained: attempts=%d retained=%t released=%t", releaseAttempts[id], retained[id], released[id])
					}
					continue
				}
				if test.wantError && releaseAttempts[id] != 1 {
					t.Fatalf("lease %s cleanup attempts = %d, want 1", id, releaseAttempts[id])
				}
				if retained[id] == released[id] {
					t.Fatalf("acquired lease %s must be returned or released: retained=%t released=%t", id, retained[id], released[id])
				}
			}
			if len(h.scheduler.nativeClaims) != len(issues) || len(h.scheduler.claims) != len(issues) || len(h.scheduler.claimPolicies) != len(issues) {
				t.Fatal("scheduler installed claims outside the returned batch")
			}
			if test.failClaim != 0 && test.failCode == "" && claims != test.failClaim {
				t.Fatal("ambiguous failed claim was retried")
			}
			request.BeginDiscovery = nil
			if test.wantError {
				if err == nil || len(issues) != 0 {
					t.Fatalf("hydration failure = %d issues, %v", len(issues), err)
				}
				if test.failCode != "" {
					var failure *APIError
					if !errors.As(err, &failure) || (test.refusalBody == "" && failure.Code != test.failCode) || (test.failStatus != 0 && failure.Status != test.failStatus) {
						t.Fatalf("refusal lost its typed owner: %v", err)
					}
				}
				if test.refusalBody == "failing" && !errors.Is(err, bodyError) {
					t.Fatalf("lost body error: %v", err)
				}
				if test.missingIsolation {
					if rejectedLease == "" || !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || !errors.Is(err, releaseError) || !strings.Contains(err.Error(), "isolation policy") {
						t.Fatalf("lost contract or cleanup error: %v", err)
					}
					return
				}
				h.failChanges.next = transport
				readyIDs = nil
				issues, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
				if err != nil || len(issues) != min(6, count) {
					t.Fatalf("failed batch retained leases: %d issues, %v", len(issues), err)
				}
			} else if err != nil || len(issues) != test.want || evaluations > test.budget {
				t.Fatalf("admitted=%d evaluated=%d error=%v, want %d", len(issues), evaluations, err, test.want)
			}
			if test.oversized {
				for _, issue := range issues {
					if issue.Description != bodies[issue.ID] || h.scheduler.nativeClaims[issue.ID].recovery.Issue.WorkItemID != tracker.NativeWorkItemID(issue.ID) || h.scheduler.nativeClaims[issue.ID].recovery.Read == nil {
						t.Fatal("oversized recovery lost exact candidate content or scope")
					}
					if len(bodies[issue.ID]) > 200<<10 && h.scheduler.nativeClaims[issue.ID].recovery.Read.IssueBody == nil {
						t.Fatal("large first candidate did not restore required body fragments")
					}
					seen := map[string]bool{}
					cursor := ""
					cursors := map[string]bool{}
					for {
						page, err := h.native.RecoveryEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), cursor)
						if err != nil {
							t.Fatal(err)
						}
						if page.Section == "complete" {
							break
						}
						if page.Section == "discussion" {
							var comment tracker.NativeComment
							if err := json.Unmarshal(page.Data, &comment); err != nil {
								t.Fatal(err)
							}
							if page.TotalBytes >= 64<<10 || comment.Body != strings.Repeat("x", 60<<10) || comment.WorkItemID != tracker.NativeWorkItemID(issue.ID) {
								t.Fatal("detail read loaded aggregate history or lost scoped discussion")
							}
							seen[comment.ID] = true
						}
						if page.NextCursor == "" || cursors[page.NextCursor] {
							t.Fatal("discussion continuation did not advance")
						}
						cursors[page.NextCursor] = true
						cursor = page.NextCursor
					}
					if len(seen) != 18 {
						t.Fatal("bounded hydration made retained discussion inaccessible")
					}
				}
			}
			if test.failCode != "" && !test.wantError && (claims != test.failClaim || len(h.scheduler.nativeClaims) != test.want) {
				t.Fatalf("capacity refusal overclaimed: claims=%d retained=%d", claims, len(h.scheduler.nativeClaims))
			}
			h.failChanges.next = transport
			h.scheduler.providerReports = nil
			request.ProviderRequirement = nil
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, true)
	fixtureCapacity(t, h, 6)

	other, err := newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: "other-machine", Hostname: "other-host", Version: "test", Capacity: 6}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	second := nativeChangeHub{native: h.native, admin: h.admin, organization: h.organization, project: h.project, scheduler: other}
	fixtureEnableEnrollment(t, &second)
	other = second.scheduler
	for i := range 8 {
		createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: fmt.Sprintf("work-%d", i), Body: issueContractTestSections, State: "Todo"})
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

type nativeDependencyRecoveryRunner struct {
	started chan runner.RunRequest
}

func (r nativeDependencyRecoveryRunner) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	r.started <- request
	<-ctx.Done()
	return runner.RunResult{}, ctx.Err()
}
