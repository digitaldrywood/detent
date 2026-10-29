package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRunnerReconnectsRetainedAttempt(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                string
		outcome             string
		effort              string
		observedIdentity    bool
		legacy              bool
		hiddenDependency    bool
		hold                bool
		loseRelease         bool
		released            bool
		missingLaunch       bool
		staleLaunch         bool
		missingProcessProof bool
		stopCheckFails      bool
		replacement         bool
	}{
		{name: "running"},
		{name: "released source after restart", released: true},
		{name: "released source preserves newer claim", released: true, replacement: true},
		{name: "released interrupted source after restart", released: true, outcome: "interrupted"},
		{name: "released interrupted source with configured effort", released: true, outcome: "interrupted", effort: "high"},
		{name: "released interrupted source with observed identity", released: true, outcome: "interrupted", observedIdentity: true, replacement: true},
		{name: "released held source after restart", released: true, hold: true},
		{name: "released source without worker proof", released: true, missingLaunch: true},
		{name: "released source without cleanup owner", released: true, missingProcessProof: true},
		{name: "released source cleanup failure keeps heartbeat", released: true, stopCheckFails: true},
		{name: "released source with stale worker identity", released: true, staleLaunch: true},
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
			var providersStopped atomic.Bool
			newScheduler := func() *Scheduler {
				t.Helper()
				var held atomic.Bool
				httpClient := *h.admin.client.httpClient
				transport := httpClient.Transport
				httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.URL.Query().Get("view") == "recovery" && !held.Load() {
						t.Error("restart recovery ran without a scheduler hold")
					}
					return transport.RoundTrip(request)
				})
				client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &httpClient})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: identity.MachineID, Hostname: redemption.Hostname, Capacity: 1, Version: "test", BackendIsolation: redemption.BackendIsolation}, HeartbeatInterval: 30 * time.Second, LeaseTTL: 10 * time.Minute, LeaseHold: func(context.Context) (func(), error) {
					held.Store(true)
					return func() { held.Store(false) }, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				if !test.missingProcessProof {
					scheduler.releasedStopped = func(context.Context, tracker.NativeWorkItemID, tracker.NativeAttempt) (bool, error) {
						if test.stopCheckFails && providersStopped.Load() {
							return false, errors.New("provider stop check failed")
						}
						return providersStopped.Load(), nil
					}
				}
				return scheduler
			}
			otherScheduler := h.scheduler
			h.scheduler = newScheduler()
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			executionIdentity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test", ReasoningEffort: test.effort}
			if err := execution.Start(t.Context(), executionIdentity); err != nil {
				t.Fatal(err)
			}
			var launch *tracker.NativeWorkerLaunch
			var stopWriter func()
			if test.released {
				cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "git", "hash-object", "--stdin")
				procgroup.Configure(t.Context(), cmd)
				input, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				worker, err := procgroup.Inspect(cmd)
				if err != nil {
					t.Fatal(err)
				}
				stopped := false
				stopWriter = func() {
					if stopped {
						return
					}
					stopped = true
					if err := input.Close(); err != nil {
						t.Fatal(err)
					}
					if err := cmd.Wait(); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(stopWriter)
				if !test.missingLaunch {
					if test.staleLaunch {
						worker.StartedAt = worker.StartedAt.Add(-time.Hour)
					}
					launch = &tracker.NativeWorkerLaunch{PID: worker.PID, GroupID: worker.GroupID, ProcessStart: worker.StartedAt, PolicyID: h.descriptor.ID, ExecutionVersion: "test", ControlVersion: "test", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), Protocol: 6, SelectedAt: time.Now()}
				}
			}
			observation := tracker.NativeRuntimeObservation{WorkerLaunch: launch, LocalAttemptID: 41, Generation: 1, HeartbeatAt: time.Now(), Phase: "implementation"}
			if test.observedIdentity {
				observation.Identity = agentidentity.RuntimeUpdate("resolved-model", "openai", "medium", "", time.Now())
			}
			if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorkspaceDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ExternalEffect: "none", EffectState: "none"}

			var retained tracker.ChangeDetail
			if test.released {
				execution.SetRepository(nativeChangeRepository)
				diff, _ := nativeChangeDiff(nativeChangeFixtureHead("b"), "README.md")(t.Context())
				checkpoint.HeadSHA = diff.HeadSHA
				checkpoint.WorktreeState = "clean"
				execution.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return nativeChangeSourceFixture(ctx, t, base, head), nil
				})
				change, err := execution.openChange(t.Context(), diff)
				if err != nil {
					t.Fatal(err)
				}
				version, _, err := execution.publishChangeVersion(t.Context(), change, diff)
				if err != nil {
					t.Fatal(err)
				}
				checkpoint.Change = &tracker.NativeChangeReference{ChangeID: change, VersionID: version, HeadSHA: diff.HeadSHA}
				retained, err = h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change)
				if err != nil {
					t.Fatal(err)
				}
			}
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
			if test.released {
				if err := execution.claim.source.client.Release(t.Context(), original, "cancelled"); err != nil {
					t.Fatal(err)
				}
				h.scheduler = newScheduler()
				h.claim(t, issue.ID)
				newer := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				if err := newer.Start(t.Context(), executionIdentity); err != nil {
					t.Fatal(err)
				}
				if err := newer.Finish(t.Context(), "failed"); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "orchestrator_release"); err != nil {
					t.Fatal(err)
				}
			} else if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "orchestrator_release"); err != nil {
				t.Fatal(err)
			}
			if !test.released {
				candidates, err := otherScheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
				if err != nil || len(candidates) != 0 {
					t.Fatalf("foreign claim = %+v, %v", candidates, err)
				}
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

			if test.released {
				var replacement tracker.NativeLease
				var successor *nativeExecution
				if test.replacement {
					h.claim(t, issue.ID)
					successor = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
					if err := successor.Start(t.Context(), executionIdentity); err != nil {
						t.Fatal(err)
					}
					replacement = successor.claim.lease
					if replacement.FencingToken <= original.FencingToken {
						t.Fatalf("replacement claim = %+v", replacement)
					}
				}
				unrelated := h.createInProgress(t, "Independent execution")
				before, err := h.admin.Recovery(t.Context(), current.WorkItemID)
				wantReleasedOutcome := test.outcome
				if wantReleasedOutcome == "" {
					wantReleasedOutcome = "cancelled"
				}
				if err != nil || len(before.Attempts) != 2 || before.Attempts[0].Outcome != wantReleasedOutcome {
					t.Fatalf("before restart = %+v, %v", before.Attempts, err)
				}
				h.scheduler = newScheduler()
				if err := h.scheduler.Heartbeat(t.Context()); err != nil {
					t.Fatal(err)
				}
				live, err := h.admin.Recovery(t.Context(), current.WorkItemID)
				if err != nil || live.Attempts[0].Outcome != wantReleasedOutcome {
					t.Fatalf("live writer acknowledged: %+v, %v", live.Attempts, err)
				}
				if test.staleLaunch {
					return
				}
				stopWriter()
				h.scheduler = newScheduler()
				if err := h.scheduler.Heartbeat(t.Context()); err != nil {
					t.Fatal(err)
				}

				uncertain, err := h.admin.Recovery(t.Context(), current.WorkItemID)
				if err != nil || uncertain.Attempts[0].Outcome != wantReleasedOutcome {
					t.Fatalf("unresolved provider writer acknowledged: %+v, %v", uncertain.Attempts, err)
				}
				providersStopped.Store(true)
				h.scheduler = newScheduler()
				if err := h.scheduler.CheckProjectPolicyWithSource(t.Context(), "local", nativeChangeRepository, h.descriptor, nil); err != nil {
					t.Fatal(err)
				}
				after, err := h.admin.Recovery(t.Context(), current.WorkItemID)
				want := "cancelled"
				if test.missingLaunch || test.missingProcessProof || test.stopCheckFails || runtime.GOOS == "windows" || runtime.GOOS == "plan9" || runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
					want = wantReleasedOutcome
				}
				if err != nil || len(after.Attempts) != 2 || after.Attempts[0].Outcome != want || !reflect.DeepEqual(after.Attempts[0].Checkpoint, before.Attempts[0].Checkpoint) || !reflect.DeepEqual(after.Attempts[1].NativeRunData, before.Attempts[1].NativeRunData) || after.Issue.Revision != current.Revision || after.Issue.State != current.State {
					t.Fatalf("released restart reconciliation = %+v, %v", after, err)
				}
				if want == "cancelled" {
					if err := cancelNativeReleasedAttempt(t.Context(), execution.claim.source.client, current.WorkItemID, before.Attempts[0]); err != nil {
						t.Fatalf("duplicate termination acknowledgment: %v", err)
					}
				}

				detail, err := h.admin.Change(t.Context(), current.WorkItemID, retained.Change.ID)
				if err != nil || !reflect.DeepEqual(detail.Versions, retained.Versions) || !reflect.DeepEqual(detail.Reviews, retained.Reviews) || !reflect.DeepEqual(detail.Checks, retained.Checks) || detail.Change.CurrentVersion != retained.Change.CurrentVersion {
					t.Fatalf("reconciliation changed immutable source or audit evidence: %+v, %v", detail, err)
				}

				if test.replacement {
					runtime, err := h.admin.RuntimeEvidence(t.Context(), current.WorkItemID, "")
					if err != nil || runtime.CurrentLease == nil || runtime.CurrentLease.ID != replacement.ID || runtime.CurrentLease.FencingToken != replacement.FencingToken {
						t.Fatalf("acknowledgement changed newer authority: %+v, %v", runtime.CurrentLease, err)
					}
					if err := successor.Finish(t.Context(), "failed"); err != nil {
						t.Fatalf("successor lost completion authority: %v", err)
					}
					if err := execution.claim.source.client.Release(t.Context(), replacement, "released"); err != nil {
						t.Fatal(err)
					}
				}
				var independent *nativeExecution
				candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{
					Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"},
					CandidateReady: func(_ context.Context, candidate connector.Issue) bool { return candidate.ID == unrelated.ID },
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, candidate := range candidates {
					if candidate.ID != unrelated.ID {
						continue
					}
					if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
						t.Fatal(err)
					}
					independent = h.scheduler.RunExecution(unrelated.ID).(*nativeExecution)
				}
				if independent == nil {
					t.Fatal("released reconciliation prevented unrelated dispatch")
				}
				if err := independent.Start(t.Context(), executionIdentity); err != nil {
					t.Fatal(err)
				}
				if err := independent.Finish(t.Context(), "failed"); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), unrelated.ID, "orchestrator_release"); err != nil {
					t.Fatal(err)
				}

				var source tracker.NativeSourceRecovery
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/source-recovery", nil, &source); err != nil || source.Quiesced != (want == "cancelled") || source.HeadSHA != checkpoint.HeadSHA || source.VersionID != checkpoint.Change.VersionID {
					t.Fatalf("retained source recovery = %+v, %v", source, err)
				}
				staleCheckpoint := checkpoint
				staleCheckpoint.HeadSHA = strings.Repeat("c", 40)
				if err := execution.Checkpoint(t.Context(), staleCheckpoint); err == nil {
					t.Fatal("released writer mutated its checkpoint")
				}
				h.scheduler = newScheduler()
				if err := h.scheduler.Heartbeat(t.Context()); err != nil {
					t.Fatal(err)
				}
				replay, err := h.admin.Recovery(t.Context(), current.WorkItemID)
				if err != nil || replay.Attempts[0].Sequence != after.Attempts[0].Sequence {
					t.Fatalf("replay duplicated acknowledgement: %+v, %v", replay.Attempts, err)
				}

				if want == "cancelled" && !test.hold {
					h.claim(t, issue.ID)
					resumed := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
					recovered := resumed.Recovery().SourceAttempt()
					if recovered == nil || recovered.AttemptID != execution.data.AttemptID || recovered.Checkpoint.Change.VersionID != retained.Change.CurrentVersion || resumed.claim.lease.FencingToken <= original.FencingToken {
						t.Fatalf("ordinary claim lost retained source: %+v", recovered)
					}
					if err := resumed.Validate(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				return
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
