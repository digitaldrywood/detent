package hubclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func clientTestPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

func TestSchedulerRejectsUnapprovedPolicyBeforeWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		descriptor policy.Descriptor
	}{
		{"missing", policy.Descriptor{}},
		{"changed", func() policy.Descriptor { d := clientTestPolicy(); d.Gates.AutoPromote = true; return d.WithID() }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/policy") {
					claims++
				}
				if err := json.NewEncoder(w).Encode(policy.Approval{Policy: clientTestPolicy()}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "worker" }})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine_a", Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			_, err = scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "p", Repository: "acme/repo", Policy: test.descriptor})
			if err == nil || !strings.Contains(err.Error(), "policy_mismatch") || claims != 0 {
				t.Fatalf("claims=%d, error=%v", claims, err)
			}
			scheduler.claims["issue"] = tracker.Lease{LeaseSummary: tracker.LeaseSummary{PolicyID: clientTestPolicy().ID}}
			scheduler.claimPolicies["issue"] = claimPolicy{project: "p", repository: "acme/repo", descriptor: test.descriptor}
			if _, err := scheduler.AdoptClaim(t.Context(), connector.Issue{ID: "issue"}, time.Now()); err == nil {
				t.Fatal("adopted mismatched policy")
			}
		})
	}
}

func TestNativeSchedulerReportsItsUnapprovedPolicyOnce(t *testing.T) {
	t.Parallel()
	changed := func() policy.Descriptor { d := clientTestPolicy(); d.Gates.AutoPromote = true; return d.WithID() }()
	for _, test := range []struct {
		name      string
		approved  *policy.Descriptor
		status    int
		local     policy.Descriptor
		wantError bool
		wantLost  bool
		reports   int
	}{
		{name: "nothing approved", status: http.StatusConflict, local: clientTestPolicy(), wantError: true, wantLost: true, reports: 1},
		{name: "approved policy differs", approved: ptr(clientTestPolicy()), status: http.StatusOK, local: changed, wantError: true, wantLost: true, reports: 1},
		{name: "invalid approved descriptor", approved: ptr(policy.Descriptor{}), status: http.StatusOK, local: clientTestPolicy(), wantError: true, wantLost: true},
		{name: "approved policy matches", approved: ptr(clientTestPolicy()), status: http.StatusOK, local: clientTestPolicy(), reports: 1},
		{name: "hub unavailable", status: http.StatusServiceUnavailable, local: clientTestPolicy(), wantError: true, reports: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var reported []policy.Descriptor
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/prj_site"):
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{WorkflowMarkdown: "---\ntracker:\n  kind: hub_native\n---\nCloud instructions.\n"})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/prj_site/policy/observed"):
					var descriptor policy.Descriptor
					if err := json.NewDecoder(r.Body).Decode(&descriptor); err != nil {
						t.Error(err)
					}
					reported = append(reported, descriptor)
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/prj_site/policy"):
					w.WriteHeader(test.status)
					switch test.status {
					case http.StatusOK:
						_ = json.NewEncoder(w).Encode(policy.Approval{Policy: *test.approved})
					case http.StatusConflict:
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "policy_mismatch", "message": "No approved repository policy"})
					default:
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "message": "down"})
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})
			client, err := New(Config{URL: "https://policy-hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				return recorder.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: "org_site", NativeProjects: map[string]tracker.ProjectID{"site": "prj_site"}, Machine: Machine{ID: "machine_a", Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := scheduler.ProjectWorkflowMarkdown(t.Context(), "site")
			if err != nil || !strings.Contains(markdown, "Cloud instructions.") {
				t.Fatalf("mapped project workflow = %q, %v", markdown, err)
			}
			markdown, err = scheduler.ProjectWorkflowMarkdown(t.Context(), "unmapped")
			if err != nil || markdown != "" {
				t.Fatalf("unmapped project supplied a Cloud workflow: %q, %v", markdown, err)
			}
			for _, layout := range []workflowconfig.ProjectDefinitionLayout{workflowconfig.ProjectDefinitionLegacy, workflowconfig.ProjectDefinitionSplit, workflowconfig.ProjectDefinitionCloud} {
				workflow := workflowconfig.Workflow{Config: workflowconfig.Default(), Prompt: "Current supplied instructions.", Definition: workflowconfig.ProjectDefinition{Layout: layout}}
				workflow.Config.Tracker.Kind = workflowconfig.TrackerHubNative
				resolved, err := scheduler.ResolveProjectWorkflow(t.Context(), "site", workflow)
				if err != nil || resolved.Prompt != workflow.Prompt {
					t.Fatalf("supplied %s definition was replaced by shared approval: %q, %v", layout, resolved.Prompt, err)
				}
			}
			for range 3 {
				err = scheduler.CheckProjectPolicy(t.Context(), "site", "", test.local)
				if (err != nil) != test.wantError {
					t.Fatalf("CheckProjectPolicy() error = %v, want error %v", err, test.wantError)
				}
				if test.wantError && test.reports > 0 && !connector.IsRetryable(err) {
					t.Fatalf("unapproved policy should keep retrying: %v", err)
				}
			}
			if len(reported) != test.reports {
				t.Fatalf("reports = %d, want %d", len(reported), test.reports)
			}
			if test.reports > 0 && reported[0].ID != test.local.ID {
				t.Fatalf("reported %s, want %s", reported[0].ID, test.local.ID)
			}
			if test.wantError {
				id := "wi_" + strings.Repeat("a", 32)
				lease := tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(id), PolicyID: test.local.ID, FencingToken: 1}
				scheduler.nativeClaims[id] = nativeClaim{source: scheduler.nativeProjects["site"], lease: lease}
				scheduler.claims[id] = nativeTrackerLease(lease)
				scheduler.claimPolicies[id] = claimPolicy{project: "site", descriptor: test.local}
				_, err := scheduler.RenewClaim(t.Context(), id, time.Now())
				if errors.Is(err, orchestrator.ErrSchedulingClaimLost) != test.wantLost {
					t.Fatalf("heartbeat authority loss = %v, want %v", err, test.wantLost)
				}
				_, retained := scheduler.nativeClaims[id]
				if retained == test.wantLost {
					t.Fatalf("heartbeat retained claim = %v, want %v", retained, !test.wantLost)
				}
			}
		})
	}
}

func ptr[T any](value T) *T { return &value }
