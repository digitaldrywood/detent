package hubclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRunnerReconnectsRetainedAttempt(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		outcome          string
		legacy           bool
		hiddenDependency bool
		hold             bool
		loseRelease      bool
	}{
		{name: "running"},
		{name: "interrupted", outcome: "interrupted"},
		{name: "legacy Hub running", legacy: true},
		{name: "legacy Hub interrupted", outcome: "interrupted", legacy: true},
		{name: "hidden dependency", hiddenDependency: true},
		{name: "operator hold", hold: true},
		{name: "hold release response lost", hiddenDependency: true, loseRelease: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHubDependencies(t, "In Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo", "Blocked"}},
				{Name: "Blocked", Transitions: []string{"In Progress"}},
				{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
			}, true, true)
			h.admin.client.baseURL.Scheme = "https"
			issue := h.createInProgress(t, "Retained execution")
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			identityURL := *h.admin.client.baseURL
			identityURL.Scheme = "https"
			file, err := runnerauth.Initialize(path, identityURL.String())
			if err != nil {
				t.Fatal(err)
			}
			enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{
				Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project},
				Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60,
			})
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: "retained", DisplayName: "Retained", Capacity: 1, Version: "test", OS: "darwin", Architecture: "arm64", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
			var identity runnerauth.Identity
			if err := h.admin.client.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &identity); err != nil {
				t.Fatal(err)
			}
			file.Identity = identity
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			newScheduler := func() *Scheduler {
				t.Helper()
				client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: h.admin.client.httpClient})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: identity.MachineID, Hostname: redemption.Hostname, Capacity: 1, Version: "test", BackendIsolation: redemption.BackendIsolation}, HeartbeatInterval: 30 * time.Second, LeaseTTL: 10 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				return scheduler
			}
			otherScheduler := h.scheduler
			h.scheduler = newScheduler()
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			executionIdentity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
			if err := execution.Start(t.Context(), executionIdentity); err != nil {
				t.Fatal(err)
			}
			if err := execution.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: 41, Generation: 1, HeartbeatAt: time.Now(), Phase: "implementation"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorkspaceDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ExternalEffect: "none", EffectState: "none"}
			if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			if test.outcome != "" {
				if err := execution.Finish(t.Context(), test.outcome); err != nil {
					t.Fatal(err)
				}
			}
			original := execution.claim.lease
			sequence := execution.data.Sequence
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "orchestrator_release"); err != nil {
				t.Fatal(err)
			}
			candidates, err := otherScheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
			if err != nil || len(candidates) != 0 {
				t.Fatalf("foreign claim = %+v, %v", candidates, err)
			}
			current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			if test.hold {
				current, err = h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Blocked", Reason: "user_requested"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.hiddenDependency {
				var private tracker.NativeProject
				if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", map[string]any{"name": "Private prerequisites", "idempotency_key": "private-project", "states": []tracker.NativeState{{Name: "Backlog"}}}, &private); err != nil {
					t.Fatal(err)
				}
				privateClient, err := h.admin.client.Native(h.organization, private.ID)
				if err != nil {
					t.Fatal(err)
				}
				blocker, err := privateClient.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Private prerequisite", State: "Backlog"})
				if err != nil {
					t.Fatal(err)
				}
				current, err = h.admin.Dependency(t.Context(), current.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"})
				if err != nil {
					t.Fatal(err)
				}
				projected, err := execution.claim.source.client.Issue(t.Context(), current.WorkItemID)
				if err != nil || !projected.DependencyReady || len(projected.Blockers) != 0 {
					t.Fatalf("hidden blocker projection = %+v, %v", projected, err)
				}
			}
			var dropped atomic.Bool
			baseTransport := h.admin.client.httpClient.Transport
			h.admin.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if (test.hold || test.hiddenDependency) && strings.HasSuffix(request.URL.Path, "/events") {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
				}
				response, err := baseTransport.RoundTrip(request)
				if err != nil {
					return response, err
				}
				if test.loseRelease && strings.HasSuffix(request.URL.Path, "/release") && dropped.CompareAndSwap(false, true) {
					response.Body.Close()
					return nil, errors.New("release response lost after commit")
				}
				if test.legacy && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/work-items/") && !strings.Contains(request.URL.RawQuery, "recovery") {
					var fields map[string]json.RawMessage
					if err := json.NewDecoder(response.Body).Decode(&fields); err != nil {
						return nil, err
					}
					response.Body.Close()
					delete(fields, "dispatchable")
					delete(fields, "scheduling_eligible")
					raw, err := json.Marshal(fields)
					if err != nil {
						return nil, err
					}
					response.Body = io.NopCloser(strings.NewReader(string(raw)))
				}
				return response, nil
			})
			h.scheduler = newScheduler()
			if test.hold || test.hiddenDependency {
				candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local"})
				if test.loseRelease {
					if err == nil || !dropped.Load() {
						t.Fatalf("release response loss = %v, dropped=%v", err, dropped.Load())
					}
				} else if err != nil || len(candidates) != 0 {
					t.Fatalf("held recovery candidates=%+v, %v", candidates, err)
				}
				recovery, err := h.admin.Recovery(t.Context(), current.WorkItemID)
				if err != nil || len(recovery.Attempts) != 1 {
					t.Fatalf("recovery=%+v, %v", recovery, err)
				}
				attempt := recovery.Attempts[0]
				if recovery.Issue.State != current.State || recovery.Issue.Revision != current.Revision || attempt.ClaimReleasedAt == nil || attempt.Outcome != "cancelled" || attempt.Status != "cancelled" || attempt.TerminalFailure == nil || attempt.TerminalFailure.ErrorClass != "lease_released" || !strings.HasSuffix(attempt.TerminalFailure.Error, "release reason: cancelled") {
					t.Fatalf("rejected recovery: state=%s revision=%d status=%s outcome=%s released=%v", recovery.Issue.State, recovery.Issue.Revision, attempt.Status, attempt.Outcome, attempt.ClaimReleasedAt)
				}
				candidates, err = newScheduler().FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local"})
				if err != nil || len(candidates) != 0 {
					t.Fatalf("repeated recovery=%+v, %v", candidates, err)
				}
				return
			}
			h.claim(t, issue.ID)
			resumed := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if !sameCompletionLease(resumed.claim.lease, original) || resumed.data.Sequence != sequence || resumed.worktreeState != "dirty" || resumed.worktreeHead != checkpoint.HeadSHA {
				t.Fatalf("reconnect replaced authority or checkpoint: lease=%s original=%s sequence=%d want=%d", resumed.claim.lease.ID, original.ID, resumed.data.Sequence, sequence)
			}
			if err := resumed.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 2, HeartbeatAt: time.Now(), Phase: "implementation"}); err != nil {
				t.Fatal(err)
			}
			if err := resumed.Start(t.Context(), executionIdentity); err != nil {
				t.Fatal(err)
			}
			if err := resumed.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			if err := resumed.claim.source.UpdateIssueState(t.Context(), issue.ID, "In Review"); err != nil {
				t.Fatalf("reconnected actor transition: %v", err)
			}
			if err := resumed.Finish(t.Context(), "failed"); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "orchestrator_release"); err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.claim.source.client.Renew(t.Context(), original, 600); !nativeLeaseLost(err) {
				t.Fatalf("finished execution retained lease: %v", err)
			}
		})
	}
}
