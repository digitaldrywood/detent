package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeOptionalReportsNegotiateHubSupport(t *testing.T) {
	t.Parallel()
	// This is the older supported Hub heartbeat schema: unknown fields are errors.
	type originalHeartbeat struct {
		Problems              json.RawMessage `json:"problems"`
		ProtocolMajor         int             `json:"protocol_major"`
		SettingsRejected      bool            `json:"settings_rejected"`
		BackendIsolation      json.RawMessage `json:"backend_isolation"`
		ProviderReports       json.RawMessage `json:"provider_reports"`
		DisplayName           string          `json:"display_name"`
		Capacity              int             `json:"capacity"`
		Version               string          `json:"version"`
		OS                    string          `json:"os"`
		Architecture          string          `json:"architecture"`
		WorkspaceCapabilities json.RawMessage `json:"workspace_capabilities"`
		WorkspaceIsolation    string          `json:"workspace_isolation"`
	}
	type checkoutHeartbeat struct {
		originalHeartbeat
		CheckoutRepository *string `json:"checkout_repository"`
	}
	type originalClaim struct {
		ProviderCandidates []tracker.NativeCapacityCandidate `json:"provider_candidates,omitempty"`
		PolicyID           string                            `json:"policy_id"`
		WorkItemID         tracker.NativeWorkItemID          `json:"work_item_id,omitempty"`
		MachineID          tracker.MachineID                 `json:"machine_id"`
		SessionID          string                            `json:"session_id"`
		TTLSeconds         int64                             `json:"ttl_seconds"`
		ProtocolMajor      int                               `json:"protocol_major"`
		Capabilities       []string                          `json:"capabilities"`
		WorkflowStates     []string                          `json:"workflow_states,omitempty"`
		Authors            []string                          `json:"authors,omitempty"`
		Assignees          []string                          `json:"assignees,omitempty"`
		LabelInclude       []string                          `json:"label_include,omitempty"`
		LabelExclude       []string                          `json:"label_exclude,omitempty"`
	}
	var supportsChecks, supportsCheckout, wrongIdentity atomic.Bool
	var supportsSetup atomic.Bool
	var supportsRanking atomic.Bool
	var claimCalls, previewCalls atomic.Int64
	var mu sync.Mutex
	var reports []struct {
		repository *string
		checks     *runnerauth.LocalChecks
	}
	var snapshot runnerauth.RoutingSnapshot
	var machineID tracker.MachineID
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/capabilities":
			features := []string{"native_issues", "scoped_collaboration", "repository_policy"}
			if supportsCheckout.Load() {
				features = append(features, tracker.NativeCheckoutRepositoryCapability)
			}
			if supportsChecks.Load() {
				features = append(features, tracker.NativeLocalChecksCapability)
			}
			if supportsSetup.Load() {
				features = append(features, tracker.NativeRunnerSetupCapability)
			}
			if supportsRanking.Load() {
				features = append(features, tracker.NativeDispatchPriorityCapability)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": features})
		case r.URL.Path == "/api/v2/organizations/org_test/projects/prj_test":
			_ = json.NewEncoder(w).Encode(tracker.NativeProject{Profile: "native"})
		case strings.HasSuffix(r.URL.Path, "/policy"):
			_ = json.NewEncoder(w).Encode(policy.Approval{Policy: clientTestPolicy()})
		case strings.HasSuffix(r.URL.Path, "/claims"), strings.HasSuffix(r.URL.Path, "/claims/preview"):
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var err error
			if supportsRanking.Load() {
				var claim tracker.NativeClaim
				err = decoder.Decode(&claim)
				if !reflect.DeepEqual(claim.DispatchPriorityByState, []string{"Merging"}) || !reflect.DeepEqual(claim.DispatchPriorityByLabel, []string{"hotfix"}) || !claim.PrioritizeUnblockers {
					t.Errorf("claim or preview lost ranking: %+v", claim)
				}
			} else {
				err = decoder.Decode(new(originalClaim))
			}
			if err != nil {
				t.Errorf("unsupported claim fields reached strict Hub decoder: %v", err)
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/preview") {
				previewCalls.Add(1)
				_ = json.NewEncoder(w).Encode(tracker.NativeCapacityPage{Items: []tracker.NativeIssue{{NativeReference: tracker.NativeReference{WorkItemID: "wi_ready"}, State: "Todo"}}})
				return
			}
			claimCalls.Add(1)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"no_claimable_work","message":"No work"}`))
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			var current struct {
				checkoutHeartbeat
				LocalChecks *runnerauth.LocalChecks `json:"local_checks"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var err error
			if supportsChecks.Load() {
				err = decoder.Decode(&current)
			} else if supportsCheckout.Load() {
				err = decoder.Decode(&current.checkoutHeartbeat)
			} else {
				err = decoder.Decode(&current.originalHeartbeat)
			}
			if err != nil {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"code":"invalid_request","message":"Request body is invalid"}`))
				return
			}
			if current.CheckoutRepository != nil && !supportsCheckout.Load() {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
			if current.LocalChecks != nil && current.LocalChecks.Setup != "" && !supportsSetup.Load() {
				t.Error("setup report reached an older Hub schema")
			}
			if current.ProtocolMajor != 2 || current.DisplayName != "Runner" || current.Capacity != 3 || current.Version != "test" || current.OS != runtime.GOOS || current.Architecture != runtime.GOARCH || r.URL.Path != "/api/v2/organizations/org_test/projects/prj_test/machines/"+string(machineID)+"/heartbeat" {
				t.Errorf("original heartbeat or machine identity changed: %+v, path=%s", current.originalHeartbeat, r.URL.Path)
			}
			mu.Lock()
			reports = append(reports, struct {
				repository *string
				checks     *runnerauth.LocalChecks
			}{current.CheckoutRepository, current.LocalChecks})
			mu.Unlock()
			response := snapshot
			if wrongIdentity.Load() {
				response.RunnerID = "runner_foreign"
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	})
	const hubURL = "https://hub.test"
	path := filepath.Join(t.TempDir(), "private", "runner.json")
	file, err := runnerauth.Initialize(path, hubURL)
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_test"
	file.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
	file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	machineID = file.Identity.MachineID
	snapshot = runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 3}.Normalized()}
	if err := runnerauth.SaveRoutingCache(path, snapshot); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: hubURL, IdentityFile: path, HTTPClient: providerHandlerClient(handler)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	repository := "Acme/Private"
	checks := runnerauth.LocalChecks{Checkout: "passed", Doctor: "failed", Provider: "failed", ProviderKinds: []string{"codex"}, Setup: "failed"}
	scheduler, err := NewScheduler(client, SchedulerConfig{
		OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"native": "prj_test"},
		CheckoutRepository: func(string) string { return repository },
		Machine:            Machine{ID: file.Identity.MachineID, Hostname: "host", DisplayName: "Runner", Version: "test", Capacity: 3},
		LocalChecks:        map[string]runnerauth.LocalChecks{"native": checks, "unrelated": {Checkout: "passed", Doctor: "passed", Provider: "passed"}},
		HeartbeatInterval:  time.Second, LeaseTTL: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	source := scheduler.nativeProjects["native"]
	if source == nil {
		t.Fatal("native project connector is missing")
	}
	for _, step := range []struct {
		name                         string
		checkout, diagnostics, setup bool
		repository                   string
		evidence                     *runnerauth.LocalChecks
	}{
		{"older Hub", false, false, false, "Acme/Private", &checks},
		{"checkout-report Hub", true, false, false, "Acme/Private", &checks},
		{"local-checks Hub", true, true, false, "Acme/Private", &checks},
		{"current Hub failed evidence", true, true, true, "Acme/Private", &checks},
		{"current Hub missing evidence", true, true, true, "", nil},
		{"neither optional field", false, false, false, "", &checks},
	} {
		t.Run(step.name, func(t *testing.T) {
			supportsCheckout.Store(step.checkout)
			supportsChecks.Store(step.diagnostics)
			supportsSetup.Store(step.setup)
			repository = step.repository
			delete(scheduler.localChecks, "native")
			if step.evidence != nil {
				scheduler.localChecks["native"] = *step.evidence
			}
			if err := scheduler.ensureNativeMachine(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			got := reports[len(reports)-1]
			mu.Unlock()
			if (got.repository != nil) != step.checkout || got.repository != nil && *got.repository != step.repository {
				t.Fatalf("checkout report = %+v", got)
			}
			want := step.evidence
			if !step.diagnostics {
				want = nil
			}
			if want != nil && !step.setup {
				copy := *want
				copy.Setup = ""
				want = &copy
			}
			if !reflect.DeepEqual(got.checks, want) {
				t.Fatalf("local checks = %+v, want %+v", got.checks, want)
			}
			now = now.Add(2 * time.Second)
		})
	}
	for _, supported := range []bool{false, true} {
		supportsChecks.Store(supported)
		machine := scheduler.machine
		machine.LocalChecks = &checks
		machine.CheckoutRepository = nil
		// Registration's one-shot report uses the same negotiated heartbeat encoder.
		if err := source.client.RegisterMachine(t.Context(), machine); err != nil {
			t.Fatal(err)
		}
		wrongIdentity.Store(true)
		if err := source.client.HeartbeatMachine(t.Context(), machine); err == nil || !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "routing identity") {
			t.Fatalf("foreign routing identity accepted (diagnostics=%v): %v", supported, err)
		}
		wrongIdentity.Store(false)
	}
	for _, step := range []struct {
		name      string
		supported bool
		preview   bool
		states    []string
		labels    []string
		unblocker bool
		wantWait  bool
	}{
		{name: "old Hub state ranking", states: []string{"Merging"}, wantWait: true},
		{name: "old Hub label ranking", labels: []string{"hotfix"}, wantWait: true},
		{name: "old Hub unblocker ranking", unblocker: true, preview: true, wantWait: true},
		{name: "old Hub without ranking"},
		{name: "compatible Hub claim", supported: true, states: []string{"Merging"}, labels: []string{"hotfix"}, unblocker: true},
		{name: "compatible Hub preview", supported: true, preview: true, states: []string{"Merging"}, labels: []string{"hotfix"}, unblocker: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			supportsRanking.Store(step.supported)
			beforeClaims, beforePreviews := claimCalls.Load(), previewCalls.Load()
			request := orchestrator.SchedulingRequest{ProjectID: "native", Policy: clientTestPolicy(), WorkflowStates: []string{"Todo"}, DispatchPriorityByState: step.states, DispatchPriorityByLabel: step.labels, PrioritizeUnblockers: step.unblocker}
			if step.preview {
				request.CandidateReady = func(context.Context, connector.Issue) bool { return true }
			}
			candidates, err := scheduler.FetchCandidateIssues(t.Context(), request)
			if errors.Is(err, orchestrator.ErrSchedulingUnavailable) != step.wantWait || !step.wantWait && err != nil || len(candidates) != 0 {
				t.Fatalf("negotiation candidates=%v error=%v, want scheduling wait=%v", candidates, err, step.wantWait)
			}
			wantClaims, wantPreviews := int64(1), int64(0)
			if step.wantWait {
				wantClaims = 0
			} else if step.preview {
				wantPreviews = 1
			}
			if claimCalls.Load()-beforeClaims != wantClaims || previewCalls.Load()-beforePreviews != wantPreviews {
				t.Fatalf("claim/preview calls = %d/%d, want %d/%d", claimCalls.Load()-beforeClaims, previewCalls.Load()-beforePreviews, wantClaims, wantPreviews)
			}
		})
	}
}

func TestSchedulerDispatchCycleUsesHub(t *testing.T) {
	descriptor := clientTestPolicy()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	lease := tracker.Lease{LeaseSummary: tracker.LeaseSummary{
		PolicyID: descriptor.ID,
		ID:       "lease-1", FencingToken: 7, Machine: tracker.MachineSummary{ID: "machine-a", Hostname: "host-a"},
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
		case "/api/v1/repositories/acme/widgets/policy":
			_ = json.NewEncoder(response).Encode(policy.Approval{Policy: descriptor})
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
		Policy:    descriptor,
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
	if len(scheduler.claimPolicies) != 0 {
		t.Fatal("released claim retained policy state")
	}

	wantPaths := []string{
		"GET /api/v1/repositories/acme/widgets/policy",
		"POST /api/v1/machines/register",
		"POST /api/v1/claims",
		"GET /api/v1/work-items/42",
		"GET /api/v1/repositories/acme/widgets/policy",
		"POST /api/v1/machines/machine-a/heartbeat",
		"GET /api/v1/repositories/acme/widgets/policy",
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

func TestIssueFromWorkItemMapsQueuePriorityAndSyncMetadata(t *testing.T) {
	syncedAt := time.Date(2026, 9, 3, 12, 34, 56, 0, time.FixedZone("offset", -5*60*60))
	tests := []struct {
		name         string
		rank         int
		wantPriority int
		wantName     string
	}{
		{name: "urgent", rank: tracker.QueuePriorityUrgent, wantPriority: 1, wantName: "urgent"},
		{name: "high", rank: tracker.QueuePriorityHigh, wantPriority: 2, wantName: "high"},
		{name: "normal", rank: tracker.QueuePriorityNormal, wantPriority: 3, wantName: "normal"},
		{name: "low", rank: tracker.QueuePriorityLow, wantPriority: 4, wantName: "low"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := WorkItem{WorkItem: tracker.WorkItem{
				ID:             42,
				Repository:     tracker.RepositoryReference{Owner: "acme", Name: "widgets"},
				GitHub:         tracker.GitHubIssueReference{NodeID: "I_42", Number: 17},
				Queue:          &tracker.QueueSummary{PriorityRank: &test.rank},
				SyncStatus:     tracker.SyncStatusPending,
				SourceSyncedAt: &syncedAt,
			}}

			issue := issueFromWorkItem(item)
			if issue.Priority == nil || *issue.Priority != test.wantPriority {
				t.Fatalf("Priority = %v, want %d", issue.Priority, test.wantPriority)
			}
			if issue.PriorityName != test.wantName {
				t.Fatalf("PriorityName = %q, want %q", issue.PriorityName, test.wantName)
			}
			if got := issue.Metadata["hub_sync_status"]; got != string(tracker.SyncStatusPending) {
				t.Fatalf("hub_sync_status = %q, want %q", got, tracker.SyncStatusPending)
			}
			if got := issue.Metadata["hub_source_synced_at"]; got != syncedAt.UTC().Format(time.RFC3339) {
				t.Fatalf("hub_source_synced_at = %q, want UTC timestamp", got)
			}
		})
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
			issues, err := scheduler.FetchCandidateIssues(context.Background(), orchestrator.SchedulingRequest{Policy: clientTestPolicy(), Repository: "acme/widgets"})
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(issues) != 0 {
				t.Fatalf("FetchCandidateIssues() = %#v, %v", issues, err)
			}
			if strings.Contains(err.Error(), "worker-token") {
				t.Fatalf("error leaked token: %v", err)
			}
		})
	}
}

func TestSchedulerRoutingDeferralsRemainQueued(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		code     string
		deferred bool
	}{
		{"selector_no_match", true},
		{"runner_disabled", true},
		{"runner_draining", true},
		{"runner_offline", true},
		{"runner_capacity", true},
		{"host_capacity", true},
		{"project_access_denied", true},
		{"claim_not_permitted", true},
		{"invalid_request", false},
	} {
		t.Run(test.code, func(t *testing.T) {
			t.Parallel()
			failure := &APIError{Code: test.code}
			err := schedulingError(errors.Join(errors.New("claim rejected"), failure))
			if errors.Is(err, orchestrator.ErrSchedulingUnavailable) != test.deferred {
				t.Fatalf("scheduling availability for %s: %v", test.code, err)
			}
			var got *APIError
			if !errors.As(err, &got) || got != failure {
				t.Fatalf("structured exclusion was lost: %v", err)
			}
		})
	}
}
