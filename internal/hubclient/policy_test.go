package hubclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
	workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte("Run the work.\n"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n"), HasConfig: true, ConfigPath: "detent.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	legacyWorkflow := workflow
	legacyWorkflow.Authored, legacyWorkflow.DefinitionSources = nil, nil
	legacy, err := workflowconfig.ResolvePolicy(legacyWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Authored.Version = 1
	previous, err := workflowconfig.ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		autoApply    bool
		loadApproved bool
		provenance   *policy.RepositorySource
		approved     *policy.Descriptor
		status       int
		local        policy.Descriptor
		wantError    bool
		wantLost     bool
		reports      int
	}{
		{name: "authored external load retains approved configuration", loadApproved: true, approved: &current, status: http.StatusOK, local: current, reports: 1},
		{name: "legacy external load retains approved configuration", loadApproved: true, approved: &legacy, status: http.StatusOK, local: legacy, reports: 1},
		{name: "verified legacy source preserves supplied definition", provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40), DefaultBranchReachable: true}, approved: &legacy, status: http.StatusOK, local: legacy, reports: 1},
		{name: "canonical version carries feature approval", autoApply: true, provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40)}, approved: &previous, status: http.StatusOK, local: current, reports: 1},
		{name: "canonical version without source remains pending", approved: &previous, status: http.StatusOK, local: current, wantError: true, reports: 1},
		{name: "older canonical runner remains approved", approved: &current, status: http.StatusOK, local: previous, reports: 1},
		{name: "default branch applies immediately", autoApply: true, provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40), DefaultBranchReachable: true}, status: http.StatusConflict, local: clientTestPolicy(), reports: 1},
		{name: "different default branch policy waits for administrator", provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40), DefaultBranchReachable: true}, approved: ptr(clientTestPolicy()), status: http.StatusOK, local: changed, wantError: true, reports: 1},
		{name: "nothing approved", status: http.StatusConflict, local: clientTestPolicy(), wantError: true, wantLost: true, reports: 1},
		{name: "runner B approval preserves runner A heartbeat", approved: ptr(clientTestPolicy()), status: http.StatusOK, local: changed, wantError: true, reports: 1},
		{name: "invalid approved descriptor", approved: ptr(policy.Descriptor{}), status: http.StatusOK, local: clientTestPolicy(), wantError: true, wantLost: true},
		{name: "approved policy matches", approved: ptr(clientTestPolicy()), status: http.StatusOK, local: clientTestPolicy(), reports: 1},
		{name: "hub unavailable", status: http.StatusServiceUnavailable, local: clientTestPolicy(), wantError: true, reports: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var reported []policy.Descriptor
			status, approved := test.status, test.approved
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/prj_site"):
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{WorkflowMarkdown: "---\ntracker:\n  kind: hub_native\n---\nCloud instructions.\n"})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/prj_site/policy/observed"):
					var observation policy.Observation
					if err := json.NewDecoder(r.Body).Decode(&observation); err != nil {
						t.Error(err)
					}
					reported = append(reported, observation.Descriptor)
					if test.autoApply {
						if observation.Source == nil || *observation.Source != *test.provenance {
							t.Fatalf("lost repository provenance: %+v", observation.Source)
						}
						status, approved = http.StatusOK, &observation.Descriptor
					}
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/prj_site/policy"):
					w.WriteHeader(status)
					switch status {
					case http.StatusOK:
						_ = json.NewEncoder(w).Encode(policy.Approval{Policy: *approved})
					case http.StatusConflict:
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "policy_mismatch", "message": "No approved repository policy"})
					default:
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "message": "down"})
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/renew"):
					if test.status == http.StatusServiceUnavailable {
						w.WriteHeader(http.StatusServiceUnavailable)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "message": "down"})
					} else if test.wantLost {
						w.WriteHeader(http.StatusConflict)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "policy_mismatch", "message": "Pinned approval revoked"})
					} else {
						_ = json.NewEncoder(w).Encode(tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID("wi_" + strings.Repeat("a", 32)), PolicyID: test.local.ID, FencingToken: 1})
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/validate"):
					_ = json.NewEncoder(w).Encode(map[string]any{})
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
				if test.name == "authored external load retains approved configuration" {
					workflow.DefinitionSources = &workflowconfig.ProjectDefinitionSources{Workflow: []byte("Stale supplied instructions.\n"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n"), HasConfig: true}
				}
				resolved, err := scheduler.ResolveProjectWorkflow(t.Context(), "site", workflow, test.provenance)
				if test.status == http.StatusServiceUnavailable && layout != workflowconfig.ProjectDefinitionCloud {
					if err == nil {
						t.Fatal("unavailable approval was ignored")
					}
					continue
				}
				if test.approved != nil && (test.approved.Configuration != nil || test.approved.Authored != nil) && test.provenance == nil && layout != workflowconfig.ProjectDefinitionCloud {
					if err != nil {
						t.Fatal(err)
					}
					actual, err := workflowconfig.ResolvePolicy(resolved)
					if err != nil || !reflect.DeepEqual(actual, *approved) {
						t.Fatalf("legacy load replaced the approved descriptor: %+v %v", actual, err)
					}
					continue
				}
				if err != nil || resolved.Prompt != workflow.Prompt {
					t.Fatalf("supplied %s definition was replaced by shared approval: %q, %v", layout, resolved.Prompt, err)
				}
			}
			for range 3 {
				err = scheduler.CheckProjectPolicyWithSource(t.Context(), "site", "", test.local, test.provenance)
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
				scheduler.nativeHeartbeats["prj_site"] = scheduler.now()
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
