package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hostpressure"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type acceptedDispatchScheduling struct {
	*Scheduler
	candidates []connector.Issue
}

func (s *acceptedDispatchScheduling) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	candidates := s.candidates
	s.candidates = nil
	return candidates, nil
}

type refusedDispatchAttempts struct {
	store.WorkAttemptStore
	beforeStart func()
	starts      atomic.Int64
}

func (s *refusedDispatchAttempts) StartWorkAttempt(context.Context, store.WorkAttemptStart) (int64, error) {
	s.starts.Add(1)
	s.beforeStart()
	return 0, errors.New("attempt store unavailable")
}

func TestNativeAcceptedClaimDispatchRefusals(t *testing.T) {
	for _, refusal := range []string{"memory_pressure_high", "io_pressure_high", "cpu_pressure_high", "worker_host_unavailable", "work_attempt_start_failed"} {
		t.Run(refusal, func(t *testing.T) {
			h := newNativeChangeHubTransport(t, "Human Review", hubserverPlanStates(), true)
			h.admin.client.baseURL.Scheme = "https"
			identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
			file, err := runnerauth.Initialize(identityPath, h.admin.client.baseURL.String())
			if err != nil {
				t.Fatal(err)
			}
			enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60})
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: "example", DisplayName: "Example runner", Capacity: 2, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
			if err := h.admin.client.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &file.Identity); err != nil {
				t.Fatal(err)
			}
			if err := runnerauth.Save(identityPath, file); err != nil {
				t.Fatal(err)
			}
			client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}})
			if err != nil {
				t.Fatal(err)
			}
			h.native, err = client.Native(h.organization, h.project)
			if err != nil {
				t.Fatal(err)
			}
			h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: file.Identity.MachineID, Hostname: redemption.Hostname, Capacity: 2, Version: "test", BackendIsolation: redemption.BackendIsolation}, HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			unrelated := h.createInProgress(t, "Unrelated execution")
			h.claim(t, unrelated.ID)
			active := h.scheduler.RunExecution(unrelated.ID).(*nativeExecution)
			if err := active.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			active.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("c")))
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: nativeChangeFixtureHead("c"), ExternalEffect: "none", EffectState: "none"}
			if err := active.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			var stopped atomic.Bool
			if err := active.KeepWorker(func(context.Context) error { stopped.Store(true); return nil }, nil); err != nil {
				t.Fatal(err)
			}
			before, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(unrelated.ID))
			if err != nil || len(before.Attempts) != 1 || before.Attempts[0].Checkpoint == nil {
				t.Fatalf("retained source setup: %+v, %v", before.Attempts, err)
			}
			item := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Accepted dispatch candidate", Body: issueContractTestSections, State: "Todo"})
			issue := h.claim(t, string(item.WorkItemID))
			lease := h.scheduler.nativeClaims[issue.ID].lease
			var releaseMu sync.Mutex
			var releases []tracker.NativeLeaseMutation
			client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/release") {
					data, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					request.Body = io.NopCloser(bytes.NewReader(data))
					var mutation tracker.NativeLeaseMutation
					if err := json.Unmarshal(data, &mutation); err != nil {
						return nil, err
					}
					if !strings.HasSuffix(request.URL.Path, "/leases/"+string(lease.ID)+"/release") || mutation.FencingToken != lease.FencingToken || mutation.Reason != "released" || mutation.CancelAttempt || !h.scheduler.now().Before(lease.ExpiresAt) {
						t.Errorf("release changed lease authority or expired: path=%s mutation=%+v", request.URL.Path, mutation)
					}
					releaseMu.Lock()
					releases = append(releases, mutation)
					releaseMu.Unlock()
				}
				return h.failChanges.RoundTrip(request)
			})
			runtimeStore, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := runtimeStore.Close(); err != nil {
					t.Error(err)
				}
			})
			attempts := &refusedDispatchAttempts{WorkAttemptStore: runtimeStore, beforeStart: func() {
				h.scheduler.mu.Lock()
				defer h.scheduler.mu.Unlock()
				if execution := h.scheduler.nativeClaims[issue.ID].execution; execution == nil || execution.data.Identity != nil {
					t.Error("startup did not reach an allocated, unstarted native execution")
				}
			}}
			if refusal == "worker_host_unavailable" {
				h.scheduler.prepareProject = func(context.Context, string) error { return errors.New("execution artifact unavailable") }
			}
			readPressure := func(kind string) func(context.Context) (hostpressure.Sample, error) {
				return func(context.Context) (hostpressure.Sample, error) {
					sample := hostpressure.Sample{ObservedAt: time.Now()}
					if refusal == kind {
						sample.Some.Avg10, sample.Some.Avg60, sample.Full.Avg10 = 99, 99, 99
					}
					return sample, nil
				}
			}
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, Plan: gate.PlanConfig{Enabled: true, Review: gate.PlanReviewAutomated}, PollInterval: time.Hour, MaxConcurrentAgents: 2, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}, MemoryPressureSomeAvg60Max: 1, IOPressureFullAvg10Max: 1, CPUPressureSomeAvg10Max: 1}, orchestrator.Dependencies{Connector: h.connector, Scheduling: &acceptedDispatchScheduling{Scheduler: h.scheduler, candidates: []connector.Issue{issue}}, Runner: orchestrator.FakeRunner{}, WorkAttempts: attempts, ReadMemoryPressure: readPressure("memory_pressure_high"), ReadIOPressure: readPressure("io_pressure_high"), ReadCPUPressure: readPressure("cpu_pressure_high"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- orch.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			})
			var state orchestrator.State
			testenv.WaitUntil(t, t.Context(), func() error {
				state, err = orch.State(t.Context())
				if err != nil {
					return err
				}
				if state.LastRefreshAt.IsZero() {
					return errors.New("initial dispatch tick has not finished")
				}
				return nil
			})
			releaseMu.Lock()
			releaseCount := len(releases)
			releaseMu.Unlock()
			if releaseCount != 1 {
				t.Fatalf("remote releases=%d starts=%d decisions=%+v; want exact fenced release before expiry", releaseCount, attempts.starts.Load(), state.SchedulerDecisions)
			}
			if _, err := h.native.ValidateLease(t.Context(), lease); err == nil {
				t.Fatal("idle accepted claim remains valid on the Hub")
			}
			found := false
			for _, decision := range state.SchedulerDecisions {
				if decision.IssueID == issue.ID && decision.Result == "skipped" && decision.Reason == refusal {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing refusal %q: %+v", refusal, state.SchedulerDecisions)
			}
			if refusal == "work_attempt_start_failed" && attempts.starts.Load() != 1 {
				t.Fatal("durable startup failure was not exercised")
			}
			candidate, err := h.admin.Recovery(t.Context(), lease.WorkItemID)
			if err != nil || len(candidate.Attempts) != 0 || len(state.Running) != 0 {
				t.Fatalf("refusal started an unexpected execution: attempts=%+v running=%+v error=%v", candidate.Attempts, state.Running, err)
			}
			h.scheduler.mu.Lock()
			_, stillAccepted := h.scheduler.nativeClaims[issue.ID]
			h.scheduler.mu.Unlock()
			if stillAccepted {
				t.Fatal("released idle claim remains in the dispatch owner")
			}
			after, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(unrelated.ID))
			if err != nil || !reflect.DeepEqual(before.Attempts, after.Attempts) || stopped.Load() {
				t.Fatalf("unrelated execution or retained source changed: %+v, %v", after.Attempts, err)
			}
			if _, err := h.native.ValidateLease(t.Context(), active.claim.lease); err != nil {
				t.Fatalf("unrelated lease lost: %v", err)
			}
		})
	}
}
