package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeClaimsDoNotWaitForPolicyApproval(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		pending bool
	}{
		{name: "approval pending", pending: true},
		{name: "previous policy approved", pending: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selected := clientTestPolicy()
			previous := selected
			previous.Gates.AutoPromote = true
			previous = previous.WithID()
			approved := false
			claims := 0
			issueID := tracker.NativeWorkItemID("wi_" + strings.Repeat("a", 32))
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			identity, err := runnerauth.Initialize(path, "https://hub.test")
			if err != nil {
				t.Fatal(err)
			}
			identity.Identity.OrganizationID = "org_test"
			identity.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
			identity.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
			if err := runnerauth.Save(path, identity); err != nil {
				t.Fatal(err)
			}
			client, err := newTestClient(t, Config{URL: identity.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v2/capabilities":
					if err := json.NewEncoder(w).Encode(map[string][]string{"features": {tracker.NativeLandingCapacityCapability}}); err != nil {
						t.Fatal(err)
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/policy/observed"):
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/routing"):
					if err := json.NewEncoder(w).Encode(runnerauth.Runner{Binding: identity.Identity.Binding, OrganizationID: "org_test", Routing: runnerauth.Routing{CapacityLimit: 1}, ReportedCapacity: 1, HostCapacity: 1}); err != nil {
						t.Fatal(err)
					}
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/policy"):
					if approved {
						if err := json.NewEncoder(w).Encode(policy.Approval{Policy: selected}); err != nil {
							t.Fatal(err)
						}
					} else if test.pending {
						if _, err := w.Write([]byte(`{"policy":null}`)); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := json.NewEncoder(w).Encode(policy.Approval{Policy: previous}); err != nil {
							t.Fatal(err)
						}
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims"):
					var request tracker.NativeClaim
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					if request.PolicyID != selected.ID {
						t.Fatalf("claim lost its reported configuration: %+v", request)
					}
					claims++
					if err := json.NewEncoder(w).Encode(tracker.NativeLease{
						ID: "lease_test", WorkItemID: issueID, MachineID: request.MachineID, SessionID: request.SessionID,
						PolicyID: selected.ID, FencingToken: 1, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
						IsolationPolicy: &isolation.Policy{Tier: isolation.NativeTrusted},
					}); err != nil {
						t.Fatal(err)
					}
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work-items/"+string(issueID)) && r.URL.Query().Get("view") == "recovery":
					if err := json.NewEncoder(w).Encode(tracker.NativeRecovery{Issue: tracker.NativeIssue{
						NativeReference: tracker.NativeReference{OrganizationID: "org_test", ProjectID: "prj_test", WorkItemID: issueID},
						Title:           "Approved policy work", State: "Todo",
					}}); err != nil {
						t.Fatal(err)
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/leases/lease_test/validate"):
					if err := json.NewEncoder(w).Encode(runnerauth.Runner{
						Binding: identity.Identity.Binding, OrganizationID: "org_test",
					}); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				return w.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := newTestScheduler(client, SchedulerConfig{
				OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"project": "prj_test"},
				Machine: Machine{ID: identity.Identity.MachineID, Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Minute, LeaseTTL: time.Hour,
				LeaseHold: func(context.Context) (func(), error) { return func() {}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			scheduler.nativeHeartbeats["prj_test"] = time.Now()
			client.Enrollment().ApplyRouting(runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{State: "active", Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}}})
			native := scheduler.nativeProjects["project"].NativeClient()
			native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "stable", PolicyID: previous.ID, Claimable: true})
			request := orchestrator.SchedulingRequest{ProjectID: "project", Policy: selected}
			if err := scheduler.CheckProjectPolicy(t.Context(), "project", "", selected); err != nil {
				t.Fatalf("unchanged selected policy stayed rejected after approval: %v", err)
			}
			issues, err := scheduler.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(issues) != 1 || issues[0].ID != string(issueID) || !issues[0].AssignedToWorker || claims != 1 {
				t.Fatalf("reported configuration did not produce a usable claim: issues=%+v claims=%d error=%v", issues, claims, err)
			}
			claimed, err := scheduler.AdoptClaim(t.Context(), issues[0], time.Now())
			if err != nil || claimed.Issue.ID != string(issueID) || !claimed.LeaseExpiresAt.After(time.Now()) {
				t.Fatalf("approved claim was not usable: %+v, %v", claimed, err)
			}
		})
	}
}

func clientTestPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

func TestNativeProjectPolicyInvalidationDuringRead(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"observation", "heartbeat", "no invalidation"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			previous := clientTestPolicy()
			updated := previous
			updated.SourceDigest = policy.Digest([]byte("updated-source"))
			updated = updated.WithID()
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			identity, err := runnerauth.Initialize(path, "https://hub.test")
			if err != nil {
				t.Fatal(err)
			}
			identity.Identity.OrganizationID = "org_test"
			identity.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
			identity.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
			if err := runnerauth.Save(path, identity); err != nil {
				t.Fatal(err)
			}
			var native *NativeClient
			scheduler := &Scheduler{}
			reads := 0
			client, err := newTestClient(t, Config{URL: identity.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/policy/observed") {
					w.WriteHeader(http.StatusNoContent)
					return w.Result(), nil
				}
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/policy") {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				reads++
				approval := policy.Approval{Policy: updated}
				if reads == 1 {
					approval.Policy = previous
					switch name {
					case "observation":
						if err := scheduler.reportObservedPolicy(r.Context(), "project", fixtureConnector(native), policy.Observation{Descriptor: updated}); err != nil {
							return nil, err
						}
					case "heartbeat":
						native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "updated", PolicyID: updated.ID})
					}
				}
				if err := json.NewEncoder(w).Encode(approval); err != nil {
					return nil, err
				}
				return w.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			native, err = client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			native.ApplyHeartbeatChanges(&runnerauth.HeartbeatChanges{Cursor: "initial", PolicyID: previous.ID})
			if approval, err := native.ProjectPolicy(t.Context()); err != nil || approval.Policy.ID != previous.ID {
				t.Fatalf("in-flight policy: id=%s error=%v", approval.Policy.ID, err)
			}
			wantID, wantReads := updated.ID, 2
			if name == "no invalidation" {
				wantID, wantReads = previous.ID, 1
			}
			for range 2 {
				if approval, err := native.ProjectPolicy(t.Context()); err != nil || approval.Policy.ID != wantID || reads != wantReads {
					t.Fatalf("policy after %s: id=%s want=%s reads=%d want=%d error=%v", name, approval.Policy.ID, wantID, reads, wantReads, err)
				}
			}
		})
	}
}

func TestSchedulerRejectsMissingConfigurationBeforeWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		descriptor policy.Descriptor
	}{
		{"missing", policy.Descriptor{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Reject missing configuration before work")
			var requests atomic.Int64
			transport := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				return transport.RoundTrip(request)
			}))
			t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
			_, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", WorkflowStates: []string{"In Progress"}, Policy: test.descriptor})
			if err == nil || !strings.Contains(err.Error(), "policy_mismatch") || requests.Load() != 0 {
				t.Fatalf("requests=%d, error=%v", requests.Load(), err)
			}
			claimed := h.claim(t, issue.ID)
			originalPolicy := h.scheduler.claimPolicies[issue.ID]
			lease := h.scheduler.nativeClaims[issue.ID].lease
			t.Cleanup(func() {
				h.scheduler.claimPolicies[issue.ID] = originalPolicy
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cancel()
				if err := h.scheduler.ReleaseClaim(ctx, issue.ID, "dispatch_deferred"); err != nil {
					t.Error(err)
				}
			})
			missingPolicy := originalPolicy
			missingPolicy.descriptor = test.descriptor
			h.scheduler.claimPolicies[issue.ID] = missingPolicy
			before := requests.Load()
			if _, err := h.scheduler.AdoptClaim(t.Context(), claimed, time.Now()); err == nil || !strings.Contains(err.Error(), "policy_mismatch") {
				t.Fatalf("adopted mismatched policy: %v", err)
			}
			if requests.Load() != before {
				t.Fatal("mismatched adoption performed a Hub request")
			}
			current := h.scheduler.nativeClaims[issue.ID].lease
			if current.ID != lease.ID || current.FencingToken != lease.FencingToken {
				t.Fatal("mismatched adoption changed lease ownership")
			}
		})
	}
}

func TestNativeSchedulerReportsConfigurationOnce(t *testing.T) {
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
	privateSources := *workflow.DefinitionSources
	privateSources.LocalWorkflow, privateSources.HasLocalWorkflow = []byte("Private machine instructions.\n"), true
	privateWorkflow, err := workflowconfig.ParseProjectDefinition(privateSources)
	if err != nil {
		t.Fatal(err)
	}
	privatePolicy, err := workflowconfig.ResolvePolicy(privateWorkflow)
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
		{name: "authored external load retains approved configuration", loadApproved: true, approved: &privatePolicy, status: http.StatusOK, local: privatePolicy, reports: 1},
		{name: "authored external load refuses unapproved instructions", loadApproved: true, approved: &current, status: http.StatusOK, local: current, reports: 1},
		{name: "verified authored source preserves supplied definition", provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40)}, approved: &current, status: http.StatusOK, local: current, reports: 1},
		{name: "legacy external load retains approved configuration", loadApproved: true, approved: &legacy, status: http.StatusOK, local: legacy, reports: 1},
		{name: "verified legacy source preserves supplied definition", provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40), DefaultBranchReachable: true}, approved: &legacy, status: http.StatusOK, local: legacy, reports: 1},
		{name: "canonical version carries feature approval", autoApply: true, provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40)}, approved: &previous, status: http.StatusOK, local: current, reports: 1},
		{name: "canonical version without source is reported", approved: &previous, status: http.StatusOK, local: current, reports: 1},
		{name: "older canonical runner remains approved", approved: &current, status: http.StatusOK, local: previous, reports: 1},
		{name: "default branch applies immediately", autoApply: true, provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40), DefaultBranchReachable: true}, status: http.StatusOK, local: clientTestPolicy(), reports: 1},
		{name: "different default branch configuration is reported", provenance: &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("a", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("b", 40), DefaultBranchReachable: true}, approved: ptr(clientTestPolicy()), status: http.StatusOK, local: changed, reports: 1},
		{name: "nothing approved", status: http.StatusOK, local: clientTestPolicy(), reports: 1},
		{name: "runner B approval preserves runner A heartbeat", approved: ptr(clientTestPolicy()), status: http.StatusOK, local: changed, reports: 1},
		{name: "invalid approved descriptor", approved: ptr(policy.Descriptor{}), status: http.StatusOK, local: clientTestPolicy(), reports: 1},
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
					if test.status == http.StatusServiceUnavailable {
						w.WriteHeader(http.StatusServiceUnavailable)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "message": "down"})
						return
					}
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
						if approved == nil {
							_, _ = w.Write([]byte(`{"policy":null}`))
						} else {
							approval := policy.Approval{Policy: *approved}
							if approved.Configuration != nil || approved.Authored != nil {
								shared, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, *approved)
								if err != nil {
									t.Error(err)
									return
								}
								approval.ExecutionConfiguration, err = workflowconfig.NewExecutionConfiguration(shared, *approved, workflowconfig.ResolveCloudModelSelection(workflowconfig.ModelSelection{}, workflowconfig.ModelSelection{}))
								if err != nil {
									t.Error(err)
									return
								}
							}
							_ = json.NewEncoder(w).Encode(approval)
						}
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
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/work-items/"):
					_ = json.NewEncoder(w).Encode(tracker.NativeIssue{SchedulingEligible: new(true), DependencyReady: true})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})
			client, err := newTestClient(t, Config{URL: "https://policy-hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				return recorder.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := newTestScheduler(client, SchedulerConfig{OrganizationID: "org_site", NativeProjects: map[string]tracker.ProjectID{"site": "prj_site"}, Machine: Machine{ID: "machine_a", Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
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
				if strings.Contains(test.name, "authored") && layout != workflowconfig.ProjectDefinitionCloud {
					sources := workflowconfig.ProjectDefinitionSources{
						Workflow: []byte("Stale supplied instructions.\n"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n"), HasConfig: true,
						LocalConfig: []byte("schema: 1\nworker:\n  github_token: private-token\nworkspace:\n  root: private-worktrees\nhooks:\n  before_run: private-isolation\nagent:\n  max_concurrent_agents: 1\n"), HasLocalConfig: true,
						LocalWorkflow: []byte("Private machine instructions.\n"), HasLocalWorkflow: true,
					}
					if layout == workflowconfig.ProjectDefinitionLegacy {
						sources.Workflow = []byte("---\ntracker:\n  kind: hub_native\n---\nStale supplied instructions.\n")
						sources.Config, sources.HasConfig = nil, false
						sources.LocalWorkflow = append([]byte("---\n"), sources.LocalConfig[len("schema: 1\n"):]...)
						sources.LocalWorkflow = append(sources.LocalWorkflow, []byte("---\nPrivate machine instructions.\n")...)
						sources.LocalConfig, sources.HasLocalConfig = nil, false
					}
					workflow, err = workflowconfig.ParseProjectDefinition(sources)
					if err != nil {
						t.Fatal(err)
					}
					original, err := workflowconfig.ResolvePolicy(workflow)
					if err != nil || original.ID == approved.ID {
						t.Fatalf("fixture must have a different authored policy: %s, %v", original.ID, err)
					}
				}
				resolved, err := scheduler.ResolveProjectWorkflow(t.Context(), "site", workflow, test.provenance)
				if test.status == http.StatusServiceUnavailable && layout != workflowconfig.ProjectDefinitionCloud {
					if !errors.Is(err, ErrUnavailable) || !connector.IsRetryable(err) {
						t.Fatalf("unavailable approval must remain retryable: %v", err)
					}
					continue
				}
				if test.name == "authored external load refuses unapproved instructions" && layout != workflowconfig.ProjectDefinitionCloud {
					if err == nil || !strings.Contains(err.Error(), "policy_mismatch") {
						t.Fatalf("unapproved private instructions were accepted: %v", err)
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
					if test.name == "authored external load retains approved configuration" && (resolved.Config.Worker.GitHubToken != "private-token" || resolved.Config.Workspace.Root != "private-worktrees" || resolved.Config.Hooks.BeforeRun != "private-isolation" || resolved.Config.Agent.MaxConcurrentAgents != 1 || !strings.HasSuffix(strings.TrimSpace(resolved.Prompt), "Private machine instructions.")) {
						t.Fatal("approved configuration discarded private overlays")
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

func TestFleetProjectEligibilityRequiresApproval(t *testing.T) {
	t.Parallel()
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%t", approved), func(t *testing.T) {
			t.Parallel()
			descriptor := clientTestPolicy()
			fleetReads := 0
			client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "administrator" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				switch r.URL.Path {
				case "/api/v2/organizations/org_example/projects/prj_example/policy":
					var selected *policy.Descriptor
					if approved {
						selected = &descriptor
					}
					if err := json.NewEncoder(recorder).Encode(map[string]any{"policy": selected}); err != nil {
						t.Fatal(err)
					}
				case "/api/v2/organizations/org_example/runners":
					fleetReads++
					if err := json.NewEncoder(recorder).Encode([]runnerauth.Runner{{Routing: runnerauth.Routing{ProjectIDs: []tracker.ProjectID{"prj_example"}, CapacityLimit: 1}, Operations: []string{runnerauth.Claim}, HostCapacity: 1, ReportedCapacity: 1}}); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatalf("unexpected request %s", r.URL.Path)
				}
				return recorder.Result(), nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			fleet, err := NewFleetClient(client, "org_example", map[string]tracker.ProjectID{"example": "prj_example"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := fleet.ProjectEligibility(t.Context(), "example")
			if approved {
				if err != nil || result.Policy.ID != descriptor.ID || fleetReads != 1 || len(result.Runners) != 1 || len(result.Runners[0].Exclusions) != 0 || len(result.Exclusions) != 0 {
					t.Fatalf("approved eligibility: %+v, %v; fleet reads=%d", result, err, fleetReads)
				}
			} else if err == nil || fleetReads != 0 || len(result.Runners) != 0 {
				t.Fatalf("missing approval evaluated runner eligibility: %+v, %v; fleet reads=%d", result, err, fleetReads)
			}
		})
	}
}

func TestPolicyRequestsKeepLocalProsePrivate(t *testing.T) {
	t.Parallel()
	for _, local := range []struct{ name, prompt, config string }{
		{"workflow", "outbound-private-marker", "schema: 1\nreview:\n  human: true\n"},
		{"yaml secrets", "", "schema: 1\ntracker:\n  api_key: outbound-private-marker\nworker:\n  github_token: outbound-private-marker\n"},
		{"yaml comment", "", "schema: 1\n# outbound-private-marker\nreview:\n  human: true\n"},
	} {
		t.Run(local.name, func(t *testing.T) {
			workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
				Workflow: []byte("Shared instructions.\n"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n"), HasConfig: true,
				LocalWorkflow: []byte(local.prompt), HasLocalWorkflow: local.prompt != "", LocalConfig: []byte(local.config), HasLocalConfig: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range []string{"scheduler observation", "native observation", "native approval", "repository approval"} {
				t.Run(operation, func(t *testing.T) {
					calls := 0
					client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(r *http.Request) (*http.Response, error) {
						calls++
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							return nil, err
						}
						if strings.Contains(string(raw), "outbound-private-marker") {
							t.Error("outbound request disclosed machine-local content")
						}
						var sent struct {
							policy.Descriptor
							Policy policy.Descriptor `json:"policy"`
						}
						if err := json.Unmarshal(raw, &sent); err != nil {
							return nil, err
						}
						actual := sent.Descriptor
						if r.Method == http.MethodPut {
							actual = sent.Policy
						}
						if actual.ID != descriptor.ID || actual.Authored.Digest != descriptor.Authored.Digest {
							t.Error("request lost effective policy identity")
						}
						w := httptest.NewRecorder()
						if r.Method == http.MethodPost {
							w.WriteHeader(http.StatusNoContent)
						} else if err := json.NewEncoder(w).Encode(policy.Approval{Policy: descriptor}); err != nil {
							return nil, err
						}
						return w.Result(), nil
					})}})
					if err != nil {
						t.Fatal(err)
					}
					native, err := client.Native("org_example", "prj_example")
					if err != nil {
						t.Fatal(err)
					}
					switch operation {
					case "scheduler observation":
						scheduler := &Scheduler{client: client, nativeProjects: map[string]*NativeConnector{"project": fixtureConnector(native)}}
						err = scheduler.CheckProjectPolicy(t.Context(), "project", "example-org/example-project", descriptor)
					case "native observation":
						err = native.ReportObservedPolicy(t.Context(), descriptor)
					case "native approval":
						_, err = native.ApproveProjectPolicy(t.Context(), policy.Change{Policy: descriptor})
					case "repository approval":
						_, err = client.ApproveProjectPolicy(t.Context(), "example-org/example-project", policy.Change{Policy: descriptor})
					}
					if err != nil {
						t.Fatal(err)
					}
					if calls != 1 {
						t.Fatalf("requests = %d, want 1", calls)
					}
				})
			}
		})
	}
}
