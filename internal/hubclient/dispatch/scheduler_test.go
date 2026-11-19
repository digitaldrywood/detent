package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestForgottenNativeClaimHeartbeatAvoidsLegacyRegistration(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	client, err := newTestClient(t, Config{URL: server.URL, TokenSource: func() string { return "example-token" }})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "example-machine", Hostname: "example", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, renew := range []func(context.Context, string, time.Time) (orchestrator.Claimed, error){scheduler.RenewClaim, scheduler.RenewClaimHeartbeat} {
		if _, err := renew(t.Context(), "wi_forgotten", time.Now()); !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
			t.Errorf("forgotten native claim = %v, want claim lost", err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("forgotten native claim sent %d legacy requests", requests.Load())
	}
}

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
	var supportsSetup, supportsDeclaration atomic.Bool
	var supportsRanking, supportsMetrics atomic.Bool
	var supportsLanding atomic.Bool
	var expectedLanding atomic.Int64
	var claimCalls, previewCalls atomic.Int64
	var mu sync.Mutex
	var reports []struct {
		repository *string
		checks     *runnerauth.LocalChecks
		metrics    []hostmetrics.Summary
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
			if supportsDeclaration.Load() {
				features = append(features, tracker.NativeRunnerSetupDeclarationCapability)
			}
			if supportsMetrics.Load() {
				features = append(features, tracker.NativeHostMetricsCapability)
			}
			if supportsRanking.Load() {
				features = append(features, tracker.NativeDispatchPriorityCapability)
			}
			if supportsLanding.Load() {
				features = append(features, tracker.NativeLandingCapacityCapability)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": features})
		case r.URL.Path == "/api/v2/organizations/org_test/projects/prj_test":
			_ = json.NewEncoder(w).Encode(tracker.NativeProject{Profile: "native"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/routing"):
			_ = json.NewEncoder(w).Encode(runnerauth.Runner{Routing: snapshot.Routing, ReportedCapacity: 3, HostCapacity: 3})
		case strings.HasSuffix(r.URL.Path, "/policy"):
			_ = json.NewEncoder(w).Encode(policy.Approval{Policy: clientTestPolicy()})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/organizations/org_test/projects/prj_test/policy/observed":
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/claims"), strings.HasSuffix(r.URL.Path, "/claims/preview"):
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var err error
			if supportsRanking.Load() || supportsLanding.Load() {
				var claim tracker.NativeClaim
				err = decoder.Decode(&claim)
				if supportsRanking.Load() && (!reflect.DeepEqual(claim.DispatchPriorityByState, []string{"Merging"}) || !reflect.DeepEqual(claim.DispatchPriorityByLabel, []string{"hotfix"}) || !claim.PrioritizeUnblockers) {
					t.Errorf("claim or preview lost ranking: %+v", claim)
				}
				if claim.LandingCapacity != int(expectedLanding.Load()) {
					t.Errorf("claim or preview landing capacity = %d, want %d", claim.LandingCapacity, expectedLanding.Load())
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
			var withMetrics struct {
				HostMetrics []hostmetrics.Summary `json:"host_metrics,omitempty"`
			}
			var negotiated struct {
				HostMetrics []hostmetrics.Summary `json:"host_metrics,omitempty"`
				originalHeartbeat
				CheckoutRepository *string                 `json:"checkout_repository"`
				LocalChecks        *runnerauth.LocalChecks `json:"local_checks"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			var err error
			if supportsMetrics.Load() {
				err = decoder.Decode(&negotiated)
				current.originalHeartbeat = negotiated.originalHeartbeat
				current.CheckoutRepository, current.LocalChecks = negotiated.CheckoutRepository, negotiated.LocalChecks
				withMetrics.HostMetrics = negotiated.HostMetrics
			} else if supportsChecks.Load() {
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
				metrics    []hostmetrics.Summary
			}{current.CheckoutRepository, current.LocalChecks, withMetrics.HostMetrics})
			mu.Unlock()
			response := snapshot
			for _, summary := range withMetrics.HostMetrics {
				response.HostMetricsAcknowledged = append(response.HostMetricsAcknowledged, hostmetrics.Acknowledgment{Hour: summary.Hour, SegmentID: summary.SegmentID})
			}
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
	client, err := newTestClient(t, Config{URL: hubURL, IdentityFile: path, HTTPClient: providerHandlerClient(handler)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	repository := "Acme/Private"
	checks := runnerauth.LocalChecks{Checkout: "passed", Doctor: "failed", Provider: "failed", ProviderKinds: []string{"codex"}, Setup: "failed", RunnerSetupDeclared: new(false)}
	scheduler, err := newTestScheduler(client, SchedulerConfig{
		OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"native": "prj_test"},
		CheckoutRepository: func(string) string { return repository },
		Skills: func(string) []skills.ProviderSkill {
			t.Error("a Hub without skill support must not request the inventory")
			return nil
		},
		Machine:           Machine{ID: file.Identity.MachineID, Hostname: "host", DisplayName: "Runner", Version: "test", Capacity: 3},
		LocalChecks:       map[string]runnerauth.LocalChecks{"native": checks, "unrelated": {Checkout: "passed", Doctor: "passed", Provider: "passed"}},
		HeartbeatInterval: time.Second, LeaseTTL: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	source := scheduler.nativeProjects["native"]
	if source == nil {
		t.Fatal("native project connector is missing")
	}
	for _, step := range []struct {
		name                                      string
		checkout, diagnostics, setup, declaration bool
		repository                                string
		evidence                                  *runnerauth.LocalChecks
	}{
		{"older Hub", false, false, false, false, "Acme/Private", &checks},
		{"checkout-report Hub", true, false, false, false, "Acme/Private", &checks},
		{"local-checks Hub", true, true, false, false, "Acme/Private", &checks},
		{"current Hub failed evidence", true, true, true, false, "Acme/Private", &checks},
		{"setup declaration Hub", true, true, true, true, "Acme/Private", &checks},
		{"current Hub missing evidence", true, true, true, true, "", nil},
		{"neither optional field", false, false, false, false, "", &checks},
	} {
		t.Run(step.name, func(t *testing.T) {
			supportsCheckout.Store(step.checkout)
			supportsChecks.Store(step.diagnostics)
			supportsSetup.Store(step.setup)
			supportsDeclaration.Store(step.declaration)
			expireCapabilities(client)
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
			if want != nil {
				copy := *want
				if !step.setup {
					copy.Setup = ""
				}
				if !step.declaration {
					copy.RunnerSetupDeclared = nil
				}
				want = &copy
			}
			if !reflect.DeepEqual(got.checks, want) {
				t.Fatalf("local checks = %+v, want %+v", got.checks, want)
			}
			now = now.Add(2 * time.Second)
		})
	}
	for _, test := range []struct {
		name               string
		supported, present bool
	}{
		{"summaries on unsupported Hub", false, true}, {"negotiated summaries", true, true}, {"omitted summaries", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			supportsMetrics.Store(test.supported)
			supportsChecks.Store(true)
			expireCapabilities(client)
			machine := scheduler.machine
			summary := hostmetrics.Summary{Hour: now.UTC().Truncate(time.Hour), SegmentID: now, SampleCount: 1, LogicalCores: 1}
			if test.present {
				machine.HostMetrics = []hostmetrics.Summary{summary}
			}
			var acknowledged []hostmetrics.Acknowledgment
			machine.HostMetricsAcknowledged = &acknowledged
			if err := source.NativeClient().HeartbeatMachine(t.Context(), machine); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			got := reports[len(reports)-1]
			mu.Unlock()
			want := 0
			if test.supported && test.present {
				want = 1
			}
			if len(got.metrics) != want || len(acknowledged) != want {
				t.Fatalf("summaries=%d acknowledged=%d want=%d", len(got.metrics), len(acknowledged), want)
			}
		})
	}
	supportsMetrics.Store(false)
	for _, supported := range []bool{false, true} {
		supportsChecks.Store(supported)
		expireCapabilities(client)
		machine := scheduler.machine
		machine.LocalChecks = &checks
		machine.CheckoutRepository = nil
		// Registration's one-shot report uses the same negotiated heartbeat encoder.
		if err := source.NativeClient().RegisterMachine(t.Context(), machine); err != nil {
			t.Fatal(err)
		}
		wrongIdentity.Store(true)
		if err := source.NativeClient().HeartbeatMachine(t.Context(), machine); err == nil || !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "routing identity") {
			t.Fatalf("foreign routing identity accepted (diagnostics=%v): %v", supported, err)
		}
		wrongIdentity.Store(false)
	}
	for _, step := range []struct {
		name             string
		supported        bool
		preview          bool
		states           []string
		labels           []string
		unblocker        bool
		wantWait         bool
		landingCapacity  int
		landingSupported bool
	}{
		{name: "old Hub state ranking", states: []string{"Merging"}, wantWait: true},
		{name: "old Hub label ranking", labels: []string{"hotfix"}, wantWait: true},
		{name: "old Hub unblocker ranking", unblocker: true, preview: true, wantWait: true},
		{name: "old Hub without ranking"},
		{name: "compatible Hub claim", supported: true, states: []string{"Merging"}, labels: []string{"hotfix"}, unblocker: true},
		{name: "compatible Hub preview", supported: true, preview: true, states: []string{"Merging"}, labels: []string{"hotfix"}, unblocker: true},
		{name: "old Hub landing capacity", landingCapacity: 1, wantWait: true},
		{name: "compatible Hub landing claim", landingCapacity: 1, landingSupported: true},
		{name: "compatible Hub landing preview", landingCapacity: 1, landingSupported: true, preview: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			supportsRanking.Store(step.supported)
			supportsLanding.Store(step.landingSupported)
			expectedLanding.Store(int64(step.landingCapacity))
			expireCapabilities(client)
			beforeClaims, beforePreviews := claimCalls.Load(), previewCalls.Load()
			request := orchestrator.SchedulingRequest{LandingCapacity: step.landingCapacity, ProjectID: "native", Policy: clientTestPolicy(), WorkflowStates: []string{"Todo"}, DispatchPriorityByState: step.states, DispatchPriorityByLabel: step.labels, PrioritizeUnblockers: step.unblocker}
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
	h := newNativeChangeHub(t, true)
	importedAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	var items []tracker.NativeIssue
	for _, label := range []string{"first", "second"} {
		item, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Dispatch " + label, Body: "Full issue body from the Hub" + issueContractTestSections, State: "In Progress", Labels: []string{"detent:todo", label}, Assignees: []string{"worker-a"}, Provenance: &tracker.Provenance{Provider: "github", ExternalID: label, AuthorID: "example-author", CreatedAt: importedAt, UpdatedAt: importedAt, ObservedAt: importedAt}})
		if err != nil {
			t.Fatal(err)
		}
		confirmIssueContract(t, h.admin, string(item.WorkItemID))
		items = append(items, item)
	}
	clock := time.Now()
	h.scheduler.now = func() time.Time { return clock }
	h.native.Transport().SetLocalPolicy(h.project, h.descriptor.ID)
	if err := h.scheduler.ensureNativeMachine(t.Context(), h.scheduler.nativeProject("local")); err != nil {
		t.Fatal(err)
	}
	identity, err := runnerauth.Load(h.native.Transport().IdentityFile())
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var responses []struct {
		path       string
		status     int
		code       string
		lease      bool
		candidates int
	}
	var claims []tracker.NativeClaim
	transport := fixtureClient(h.native.Transport()).requests.Load()
	fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		path := request.Method + " " + request.URL.Path
		if request.URL.Query().Get("view") != "" {
			path += "?view=" + request.URL.Query().Get("view")
		}
		if request.URL.Query().Get("cursor") != "" {
			path += "&cursor=present"
		}
		paths = append(paths, path)
		if request.Method == http.MethodPost && request.URL.Path == h.native.Scope()+"/claims" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
			var claim tracker.NativeClaim
			if err := json.Unmarshal(body, &claim); err != nil {
				return nil, err
			}
			claims = append(claims, claim)
		}
		response, err := transport.RoundTrip(request)
		if err == nil && request.Method == http.MethodPost && (request.URL.Path == h.native.Scope()+"/claims" || request.URL.Path == h.native.Scope()+"/claims/preview") {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(body))
			if readErr != nil {
				return response, readErr
			}
			var result struct {
				Code    string            `json:"code"`
				LeaseID string            `json:"lease_id"`
				Items   []json.RawMessage `json:"items"`
			}
			json.Unmarshal(body, &result)
			responses = append(responses, struct {
				path       string
				status     int
				code       string
				lease      bool
				candidates int
			}{request.URL.Path, response.StatusCode, result.Code, result.LeaseID != "", len(result.Items)})
		}
		return response, err
	}))
	t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
	for index, item := range items {
		cycleStart := len(paths)
		label := []string{"first", "second"}[index]
		filter := connector.IssueFilterHint{Authors: []string{"example-author"}, Assignees: []string{"worker-a"}, LabelInclude: []string{"detent:todo", label}, LabelExclude: []string{"hold"}}
		issues, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"In Progress"}, Filter: filter})
		if err != nil || len(issues) != 1 {
			t.Logf("dispatch candidate request paths=%v responses=%+v filter=%+v", paths[cycleStart:], responses, filter)
			for _, claim := range claims {
				t.Logf("dispatch claim policy=%s states=%v authors=%v assignees=%v include=%v exclude=%v", claim.PolicyID, claim.WorkflowStates, claim.Authors, claim.Assignees, claim.LabelInclude, claim.LabelExclude)
			}
			for _, candidate := range items {
				current, readErr := h.admin.Issue(t.Context(), candidate.WorkItemID)
				if readErr != nil {
					t.Logf("dispatch current item id=%s read_error=%v", candidate.WorkItemID, readErr)
					continue
				}
				sourceAuthor := ""
				if current.Provenance != nil {
					sourceAuthor = current.Provenance.AuthorID
				}
				confirmed := 0
				if current.IssueContract != nil {
					confirmed = len(current.IssueContract.ConfirmedSections)
				}
				t.Logf("dispatch current item id=%s state=%s archived=%t terminal=%t source_author=%s actor_kind=%s labels=%v assignees=%v contract_present=%t confirmed_sections=%d", current.WorkItemID, current.State, current.Archived, current.Terminal, sourceAuthor, current.Actor.Kind, current.Labels, current.Assignees, current.IssueContract != nil, confirmed)
				h.seed(func(w tenantstore.Writer) error {
					facts, err := w.WorkItemFacts(t.Context(), h.project, candidate.WorkItemID)
					if err != nil {
						t.Logf("dispatch optional claim facts id=%s read_error=%v", candidate.WorkItemID, err)
						return nil
					}
					queue, err := w.QueueEntry(t.Context(), h.project, candidate.WorkItemID, string(h.project))
					if err != nil {
						t.Logf("dispatch optional queue entry id=%s read_error=%v", candidate.WorkItemID, err)
						return nil
					}
					t.Logf("dispatch canonical facts id=%s author=%s assignees=%v queue_state=%s", candidate.WorkItemID, facts.Author, facts.Assignees, queue.State)
					labels, err := w.WorkItemLabels(t.Context(), h.project, candidate.WorkItemID, corestore.LabelCursor{}, 200)
					if err != nil {
						t.Logf("dispatch optional canonical labels id=%s read_error=%v", candidate.WorkItemID, err)
						return nil
					}
					t.Logf("dispatch canonical labels id=%s labels=%v has_more=%t", candidate.WorkItemID, labels.Items, labels.Next.After != "")
					base := claimstore.CandidateQuery{ProjectID: h.project, Scope: string(h.project), WorkItemID: candidate.WorkItemID, AvailableAt: time.Now(), Limit: 1, RunnerSelection: true}
					for _, predicate := range []string{"base", "state", "author", "assignee", "labels", "all"} {
						query := base
						if predicate == "state" || predicate == "all" {
							query.States = []string{"In Progress"}
						}
						if predicate == "author" || predicate == "all" {
							query.Authors = filter.Authors
						}
						if predicate == "assignee" || predicate == "all" {
							query.Assignees = filter.Assignees
						}
						if predicate == "labels" || predicate == "all" {
							query.IncludeLabels = filter.LabelInclude
							query.ExcludeLabels = filter.LabelExclude
						}
						page, err := w.CandidatePage(t.Context(), query)
						if err != nil {
							t.Logf("dispatch optional candidate id=%s predicate=%s read_error=%v", candidate.WorkItemID, predicate, err)
							return nil
						}
						t.Logf("dispatch canonical candidate id=%s predicate=%s rows=%d", candidate.WorkItemID, predicate, len(page.Items))
						for _, row := range page.Items {
							t.Logf("dispatch candidate row id=%s state=%s author=%s assignees=%v labels=%v", row.WorkItemID, row.State, row.Author, row.Assignees, row.Labels)
						}
					}
					approval, err := w.ProjectPolicy(t.Context(), h.project)
					if err != nil {
						t.Logf("dispatch optional claim policy id=%s read_error=%v", candidate.WorkItemID, err)
						return nil
					}
					gateKnown, gateEnabled := approval.GitHubPullRequest != nil, false
					if gateKnown {
						gateEnabled = *approval.GitHubPullRequest
					}
					t.Logf("dispatch policy id=%s github_gate_known=%t stored_github_gate=%t descriptor_github_gate=%t", approval.PolicyID, gateKnown, gateEnabled, h.descriptor.Gates.GitHubPullRequest)
					return nil
				})
				evidence, readErr := h.admin.RuntimeEvidence(t.Context(), candidate.WorkItemID, "")
				if readErr != nil {
					t.Logf("dispatch admission id=%s read_error=%v", candidate.WorkItemID, readErr)
					continue
				}
				t.Logf("dispatch admission id=%s entries=%d selection=%s scheduling_source=%s scheduling_outcome=%s", candidate.WorkItemID, len(evidence.Admission), evidence.Selection, evidence.Scheduling.Source, evidence.Scheduling.Outcome)
				for _, capacity := range evidence.Capacity {
					t.Logf("dispatch capacity id=%s runner=%s health=%s available=%d exclusions=%v", candidate.WorkItemID, capacity.RunnerID, capacity.Health, capacity.Available, capacity.Exclusions)
				}
				for _, admission := range evidence.Admission {
					t.Logf("dispatch admission id=%s runner=%s revision=%d source=%s outcome=%s reason_code=%s", candidate.WorkItemID, admission.RunnerID, admission.RunnerRevision, admission.Source, admission.Outcome, admission.ReasonCode)
				}
			}
			t.Fatalf("FetchCandidateIssues() = %#v, %v", issues, err)
		}
		if issues[0].ID != string(item.WorkItemID) || issues[0].Identifier != fmt.Sprintf("%s#%d", item.ProjectID, item.Number) || issues[0].Description != item.Body || issues[0].AuthorID != "example-author" {
			t.Fatalf("candidate = %#v", issues[0])
		}
		lease := h.scheduler.nativeClaims[issues[0].ID].lease
		claimed, err := h.scheduler.AdoptClaim(t.Context(), issues[0], time.Now())
		if err != nil || claimed.Owner != string(h.scheduler.machine.ID) || !claimed.LeaseExpiresAt.Equal(lease.ExpiresAt) {
			t.Fatalf("AdoptClaim() = %#v, %v", claimed, err)
		}
		renewed, err := h.scheduler.RenewClaim(t.Context(), issues[0].ID, time.Now())
		if err != nil || !renewed.LeaseRenewedAt.After(claimed.LeaseRenewedAt) || renewed.LeaseExpiresAt.Before(claimed.LeaseExpiresAt) {
			t.Fatalf("RenewClaim() = %#v, %v", renewed, err)
		}
		if renewed.Issue.Labels != nil || renewed.Issue.Assignees != nil || renewed.Issue.BlockedBy != nil || renewed.Issue.Fields != nil {
			t.Fatalf("RenewClaim() issue = %#v, want sparse lease metadata", renewed.Issue)
		}
		if err := h.scheduler.ReleaseClaim(t.Context(), issues[0].ID, "completed"); err != nil {
			t.Fatal(err)
		}
		if len(h.scheduler.claimPolicies) != 0 {
			t.Fatal("released claim retained policy state")
		}
		if _, err := h.native.Renew(t.Context(), lease, 60); !nativeLeaseLost(err) {
			t.Fatalf("released Hub lease remains renewable: %v", err)
		}
		if len(claims) != index+1 {
			t.Fatalf("claim count = %d, want %d", len(claims), index+1)
		}
		cycle := paths[cycleStart:]
		counts := make(map[string]int)
		for _, path := range cycle {
			counts[path]++
		}
		base := h.native.Scope()
		leasePath := base + "/leases/" + string(lease.ID)
		wantCounts := map[string]int{
			"GET /api/v2/organizations/" + string(h.organization) + "/runners/" + identity.Identity.RunnerID + "/routing": 1,
			"POST " + base + "/claims": 1,
			"GET " + base + "/work-items/" + string(item.WorkItemID) + "?view=recovery": 1,
			"POST " + leasePath + "/validate?view=owner":                                2,
			"POST " + leasePath + "/renew":                                              2,
			"POST " + leasePath + "/release":                                            1,
		}
		if index == 0 {
			wantCounts["POST "+base+"/policy/observed"] = 1
		}
		if !reflect.DeepEqual(counts, wantCounts) {
			t.Fatalf("native cycle requests = %v, want %v; order=%v", counts, wantCounts, cycle)
		}
		observedAt, claimedAt := -1, -1
		for position, path := range paths {
			if path == "POST "+base+"/policy/observed" {
				observedAt = position
			}
			if position >= cycleStart && path == "POST "+base+"/claims" {
				claimedAt = position
			}
		}
		if observedAt < 0 || claimedAt <= observedAt {
			t.Fatalf("native claim preceded policy observation: %v", paths)
		}
		claim := claims[index]
		if claim.PolicyID != h.descriptor.ID || claim.SessionID == "" || !reflect.DeepEqual(claim.Authors, filter.Authors) || !reflect.DeepEqual(claim.Assignees, filter.Assignees) || !reflect.DeepEqual(claim.LabelInclude, filter.LabelInclude) || !reflect.DeepEqual(claim.LabelExclude, filter.LabelExclude) {
			t.Fatalf("claim lost scope: %+v", claim)
		}
	}
}

func TestIssueFromNativeMapsQueuePriorityAndScopeMetadata(t *testing.T) {
	for _, test := range []struct {
		name         string
		rank         int
		wantPriority int
	}{
		{"urgent", tracker.QueuePriorityUrgent, 1}, {"high", tracker.QueuePriorityHigh, 2}, {"normal", tracker.QueuePriorityNormal, 3}, {"low", tracker.QueuePriorityLow, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: "wi_example", OrganizationID: "org_example", ProjectID: "project_example", Revision: 7}, Priority: &test.rank}
			issue := issueFromNative(item)
			if issue.Priority == nil || *issue.Priority != test.wantPriority {
				t.Fatalf("Priority = %v, want %d", issue.Priority, test.wantPriority)
			}
			if issue.PriorityName != test.name {
				t.Fatalf("PriorityName = %q, want %q", issue.PriorityName, test.name)
			}
			if issue.Metadata["hub_organization_id"] != "org_example" || issue.Metadata["hub_project_id"] != "project_example" || issue.Metadata["hub_revision"] != "7" || issue.Metadata["hub_profile"] != "native" {
				t.Fatalf("native scope metadata = %#v", issue.Metadata)
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
			h := newNativeChangeHub(t, true)
			h.createInProgress(t, "Wait through a Hub outage")
			transport := fixtureClient(h.native.Transport()).requests.Load()
			fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				test.handler.ServeHTTP(recorder, request)
				return recorder.Result(), nil
			}))
			t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
			issues, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"In Progress"}})
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(issues) != 0 {
				t.Fatalf("FetchCandidateIssues() = %#v, %v", issues, err)
			}
			identity, loadErr := runnerauth.Load(h.native.Transport().IdentityFile())
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if strings.Contains(err.Error(), string(identity.Credential)) {
				t.Fatal("error leaked runner credential")
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
		{"plan_capacity", true},
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

func TestNativeClaimsOnlyAdmittedContinuation(t *testing.T) {
	h := newNativeChangeHub(t, true)
	queued := h.createInProgress(t, "Queued item")
	admitted := h.createInProgress(t, "Admitted continuation")
	released := 0
	request := orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"In Progress"},
		CandidateKnownWait: func(issue connector.Issue) bool { return issue.ID == queued.ID },
		CandidateReady: func(_ context.Context, issue connector.Issue) bool {
			if issue.ID == queued.ID {
				t.Fatal("known wait reached readiness evaluation")
			}
			return true
		},
		CandidateAdmission: func(issue connector.Issue) (func(), bool) { return func() { released++ }, issue.ID == admitted.ID },
	}
	issues, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(issues) != 1 || issues[0].ID != admitted.ID || released != 1 {
		t.Fatalf("continuation claims = %v, admission releases=%d, err=%v", issues, released, err)
	}
	if _, found := h.scheduler.nativeClaims[queued.ID]; found {
		t.Fatal("known wait acquired a lease")
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), admitted.ID, "dispatch_deferred"); err != nil {
		t.Fatal(err)
	}
	request.CandidateKnownWait = nil
	request.CandidateReady = func(context.Context, connector.Issue) bool { return true }
	request.CandidateAdmission = func(issue connector.Issue) (func(), bool) { return func() {}, issue.ID == queued.ID }
	issues, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
	if err != nil || len(issues) != 1 || issues[0].ID != queued.ID {
		t.Fatalf("previously waiting item is not usable: %v, %v", issues, err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), queued.ID, "dispatch_deferred"); err != nil {
		t.Fatal(err)
	}
}

func expireCapabilities(client *Client) {
	fixtureClient(client).offset.Add(int64(2 * time.Hour))
}
