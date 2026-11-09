package scenariotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func RunNativeRecordedDependencyAdmission(t *testing.T, factories Constructors) {
	for _, test := range []struct {
		name, refusal   string
		reasonCode      string
		laterLease      string
		qualified       bool
		repositoryAlias bool
		advancePolicy   bool
		dirty           bool
		second          bool
		external        bool
		instance        bool
		retainedWriter  bool
	}{
		{name: "reported prerequisite registers dependency hold"},
		{name: "instance-owned blocker recovers on any runner without a dependency", instance: true},
		{name: "typed instance reason recovers after ended claim", instance: true, reasonCode: "instance_limitation", laterLease: "released", dirty: true},
		{name: "typed instance reason preserves human hold", instance: true, reasonCode: "instance_limitation", refusal: "human"},
		{name: "configuration change permits instance recovery", instance: true, reasonCode: "instance_limitation", refusal: "policy"},
		{name: "instance evidence preserves permission reason", instance: true, reasonCode: "permission_wait", refusal: "reason"},
		{name: "instance evidence preserves unknown reason", instance: true, reasonCode: "unknown_hold", refusal: "reason"},
		{name: "canonical native reference", qualified: true},
		{name: "bound repository references retain both dependencies", repositoryAlias: true, second: true},
		{name: "bound repository reference preserves human hold", repositoryAlias: true, refusal: "human"},
		{name: "last reported prerequisite releases hold", second: true},
		{name: "external prerequisite retains hold", repositoryAlias: true, external: true, refusal: "external"},
		{name: "dirty1067 canonical prerequisite retains checkpoint and recovery", qualified: true, dirty: true},
		{name: "new approved policy recovers historical report", advancePolicy: true},
		{name: "later released claim retains recorded recovery", laterLease: "released", dirty: true},
		{name: "later expired claim retains recorded recovery", laterLease: "expired", dirty: true},
		{name: "later expired claim seeding isolates retired writer", laterLease: "expired", dirty: true, retainedWriter: true},
		{name: "later active claim retains hold", laterLease: "active", refusal: "active"},
		{name: "later completed attempt supersedes report", laterLease: "attempt", refusal: "attempt"},
		{name: "human action retains hold", refusal: "human"},
		{name: "operator transition retains hold", refusal: "operator"},
		{name: "stale revision cannot recover", refusal: "revision"},
		{name: "stale fence cannot recover", refusal: "fence"},
		{name: "stale report cannot recover", refusal: "report"},
		{name: "unrelated native dependency retains hold", refusal: "relation"},
		{name: "configuration change permits dependency recovery", refusal: "policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var clockOffset atomic.Int64
			clock := func() time.Time { return time.Now().Add(time.Duration(clockOffset.Load())) }
			h := newNativeChangeHubConfigured(t, "Human Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "Human Review"}},
				{Name: "Blocked", Transitions: []string{"In Progress", "Blocked"}},
				{Name: "Human Review"},
				{Name: "Backlog", Transitions: []string{"Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Backlog"}},
			}, true, true, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Clock: clock}, factories)
			if test.repositoryAlias {
				h.seed(func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), h.project)
					if err != nil {
						return err
					}
					project.Settings.CheckoutRepository = "example-org/example-project"
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				})
			}
			prerequisite, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Actual prerequisite", State: "Backlog"})
			if err != nil {
				t.Fatal(err)
			}
			var second tracker.NativeIssue
			if test.second {
				second, err = h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Second prerequisite", State: "Backlog"})
				if err != nil {
					t.Fatal(err)
				}
			}
			issue := h.createInProgress(t, "Reported dependency")
			var unrelated tracker.NativeIssue
			if test.refusal == "relation" {
				other, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Unrelated prerequisite", State: "Done"})
				if err != nil {
					t.Fatal(err)
				}
				unrelated = other
				if err := h.connector.AddIssueDependency(t.Context(), issue.ID, string(other.WorkItemID)); err != nil {
					t.Fatal(err)
				}
			}
			h.claim(t, issue.ID)
			if unrelated.WorkItemID != "" {
				if _, err := h.admin.Transition(t.Context(), unrelated.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: unrelated.Revision, State: "Backlog", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
			}
			execution := h.scheduler.RunExecution(issue.ID)
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", HeadSHA: strings.Repeat("a", 40), ExternalEffect: "none", EffectState: "none"}
			diff := nativeChangeDiff(checkpoint.HeadSHA)
			if test.dirty {
				checkpoint.Resume, checkpoint.WorktreeState = "resume_session", "dirty"
				checkpoint.HeadSHA = "60c199a71dfe408cc26e730b9567c8606985ebfb"
				checkpoint.WorkspaceDigest = strings.Repeat("b", 64)
				diff = nativeChangeDiff(checkpoint.HeadSHA, "inventory.go", "inventory_test.go")
			}
			execution.(runner.DiffExecution).SetDiffSource(diff)
			if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			reference := fmt.Sprintf("#%d", prerequisite.Number)
			if test.qualified {
				reference = string(h.project) + reference
			}
			if test.repositoryAlias {
				reference = "example-org/example-project" + reference
			}
			human := "null"
			if test.refusal == "human" {
				human = "Approve the release"
			}
			report := fmt.Sprintf("```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '%s'\n    reason: prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action: %s\n```", reference, human)
			if test.dirty {
				report = fmt.Sprintf("```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '%s'\n    reason: prerequisite must finish\nhuman_action: null\n```", reference)
			}
			if test.external {
				foreignReference := fmt.Sprintf("vendor/service#%d", prerequisite.Number)
				report = strings.Replace(report, reference, foreignReference, 1)
				reference = foreignReference
			}
			if test.instance {
				report = fmt.Sprintf("```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: 'instance:check-barrier'\n    reason: barrier failed on this host\n    owner: instance\nhuman_action: %s\n```", human)
			}
			if test.reasonCode != "" {
				report = strings.Replace(report, "status: blocked", "status: blocked\nreason_code: "+test.reasonCode, 1)
			}
			if test.second {
				report = strings.Replace(report, "human_action:", fmt.Sprintf("  - ref: '#%d'\n    reason: second prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action:", second.Number), 1)
			}
			if err := execution.(runner.CompletionExecution).PrepareFinish(t.Context(), "succeeded", report, nil); err != nil {
				t.Fatal(err)
			}
			if err := execution.Finish(t.Context(), "succeeded"); err != nil {
				t.Fatal(err)
			}
			if test.dirty && (execution.(runner.ChangeExecution).NativeChange() != nil || len(h.changes(t, issue.ID)) != 0) {
				t.Fatal("dirty prerequisite report published unfinished source")
			}
			if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			recovery, err := h.native.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			blocked := recovery.Issue
			if slices.Contains(blocked.OmittedFields, "dependencies") {
				t.Fatal("dependency assertion requires the authoritative relationship read")
			}
			wantDependencies := 1
			if test.refusal == "human" || test.external || test.instance {
				wantDependencies = 0
			} else if test.refusal == "relation" || test.second {
				wantDependencies = 2
			}
			if len(blocked.Dependencies) != wantDependencies {
				t.Fatalf("reported dependency links = %+v, want %d", blocked.Dependencies, wantDependencies)
			}
			hydrated, err := h.connector.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			wantBlockers := 1
			if test.second {
				wantBlockers = 2
			}
			if err != nil || len(hydrated) != 1 || hydrated[0].WorkpadSignal == nil || len(hydrated[0].WithNativeWorkpadAuthority().WorkpadSignal.Blockers) != wantBlockers {
				t.Fatalf("native disposition lost its prerequisite: %+v, %v", hydrated, err)
			}
			resolved, err := h.connector.FetchIssueStatesByIdentifiers(t.Context(), []string{string(h.project) + fmt.Sprintf("#%d", prerequisite.Number)})
			if err != nil || len(resolved) != 1 || resolved[0].ID != string(prerequisite.WorkItemID) || resolved[0].Closed {
				t.Fatalf("native reference resolved incorrectly: %+v, %v", resolved, err)
			}
			attempts, history, err := h.connector.RecordedBlockerContext(t.Context(), blocked.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			attempt, prior, valid := tracker.RecordedNativeBlockers(blocked, attempts, history)
			if !valid || prior != "In Progress" {
				t.Fatalf("current report missing: %+v, %s, %t", attempt, prior, valid)
			}
			if test.repositoryAlias {
				wantReference := reference
				if !test.external && test.refusal != "human" {
					wantReference = fmt.Sprintf("%s#%d", h.project, prerequisite.Number)
				}
				blocker := attempt.Disposition.BlockerEvidence[0]
				if blocker.Ref != wantReference || blocker.Predicate.Identifier != wantReference || attempt.CompletionBody != report {
					t.Fatalf("durable prerequisite identity or original report changed: %+v", attempt)
				}
			}
			if test.dirty {
				signal, valid := workpad.SignalFromComment(report, "", "")
				if !valid || signal.Invalid != nil || attempt.Status != "succeeded" || !reflect.DeepEqual(attempt.Checkpoint, &checkpoint) || !reflect.DeepEqual(attempt.Disposition.BlockerEvidence, signal.Blockers) {
					t.Fatalf("dirty terminal authority or checkpoint lost: %+v", attempt)
				}
			}
			if test.dirty || test.repositoryAlias {
				page, err := h.admin.History(t.Context(), blocked.WorkItemID, "")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, event := range page.Items {
					if event.Type == "run.finished" && event.Data.Run != nil && event.Data.Run.AttemptID == attempt.AttemptID {
						found = reflect.DeepEqual(event.Data.Run.Disposition, attempt.Disposition)
					}
				}
				if !found {
					t.Fatal("run.finished omitted its durable blocker disposition")
				}
			}
			latestFence := attempt.FencingToken
			if test.laterLease != "" {
				var later leasestore.Lease
				seedLater := func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), h.project, blocked.WorkItemID)
					if err != nil {
						return err
					}
					ttl := time.Hour
					if test.laterLease == "expired" {
						ttl = time.Minute
					}
					later, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(attempt.MachineID), SessionID: "later-session", LeaseID: "later-lease-" + issue.ID, TTL: ttl})
					if err != nil {
						return err
					}
					latestFence = tracker.FencingToken(later.FencingToken)
					if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: h.organization, ProjectID: h.project, LeaseID: tracker.LeaseID(later.LeaseID), PolicyID: h.descriptor.ID, BoundAt: h.clock()}); err != nil {
						return err
					}
					if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: h.organization, LeaseID: later.LeaseID, RunnerID: attempt.RunnerID, AssignedAt: h.clock()}); err != nil {
						return err
					}
					if test.laterLease == "attempt" {
						data := attempt.NativeRunData
						data.AttemptID = api.ExecutionID("attempt", "later-"+issue.ID)
						data.RunID = api.ExecutionID("run", "later-"+issue.ID)
						data.LeaseID, data.FencingToken, data.SessionID, data.Sequence = tracker.LeaseID(later.LeaseID), latestFence, later.SessionID, 1
						raw, err := json.Marshal(data)
						if err != nil {
							return err
						}
						ref, err := h.config.RunDetails.StoreDetail(t.Context(), string(h.organization), raw)
						if err != nil {
							return err
						}
						if _, _, err := w.RecordAttemptEvent(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, RunID: data.RunID, Fence: leasestore.Fence{LeaseID: later.LeaseID, FencingToken: later.FencingToken}, Sequence: 1, Status: "succeeded", DispatchGeneration: attempt.DispatchGeneration, PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
							return err
						}
						history, err := json.Marshal(tracker.CollaborationEvent{ID: api.ExecutionID("evt", data.AttemptID), OrganizationID: h.organization, ProjectID: h.project, AggregateID: blocked.WorkItemID, Type: "run.finished", SchemaVersion: 1, RecordedAt: h.clock(), Actor: tracker.Actor{Kind: "runner", PrincipalID: attempt.RunnerID}, Data: tracker.CollaborationData{Run: &data}})
						if err != nil {
							return err
						}
						event, err := eventstore.NewContent(h.config.RunDetails).PrepareEvent(t.Context(), h.organization, history)
						if err != nil {
							return err
						}
						if _, err := w.AppendEvent(t.Context(), event); err != nil {
							return err
						}
					}
					if test.laterLease == "released" || test.laterLease == "attempt" {
						_, err = w.Release(t.Context(), leasestore.Fence{LeaseID: later.LeaseID, FencingToken: later.FencingToken}, "fixture_complete")
					}
					return err
				}
				if test.retainedWriter {
					h.seed(func(retired tenantstore.Writer) error {
						if _, err := retired.(leasestore.FencedLeaseLocker).LockLease(t.Context(), string(attempt.LeaseID)); err != nil {
							return err
						}
						h.seed(seedLater)
						return nil
					})
				} else {
					h.seed(seedLater)
				}
				if test.laterLease == "expired" {
					clockOffset.Add(int64(2 * time.Minute))
					h.seed(func(w tenantstore.Writer) error {
						_, err := w.Expire(t.Context(), leasestore.Fence{LeaseID: later.LeaseID, FencingToken: later.FencingToken})
						return err
					})
				}
			}
			request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "too-early", LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken}, PolicyID: h.descriptor.ID, BlockerAttemptID: attempt.AttemptID, ExpectedRevision: blocked.Revision, State: prior, Reason: "dependency_ready", ReasonDetail: "recorded_blocker_recovery"}
			if test.instance {
				if test.refusal == "policy" {
					h.repolicy(t)
				}
				if test.refusal != "" && test.refusal != "policy" {
					if _, err := h.otherWorker(t).Transition(t.Context(), blocked.WorkItemID, request); err == nil || h.state(t, issue.ID) != "Blocked" {
						t.Fatalf("%s authority was bypassed: %v", test.refusal, err)
					}
					return
				}
				if _, err := h.otherWorker(t).Transition(t.Context(), blocked.WorkItemID, request); err != nil || h.state(t, issue.ID) != prior {
					t.Fatalf("instance-owned blocker recovery = %v, state %s, want %s", err, h.state(t, issue.ID), prior)
				}
				return
			}
			if _, err := h.native.Transition(t.Context(), blocked.WorkItemID, request); err == nil {
				t.Fatal("recovery cleared a genuinely unresolved prerequisite")
			}
			if len(h.candidates(t)) != 0 {
				t.Fatal("blocked prerequisite was admitted")
			}
			if test.refusal != "" || test.laterLease != "" {
				if _, err := h.admin.Transition(t.Context(), prerequisite.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Done", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
			}
			request.IdempotencyKey = "ready"
			switch test.refusal {
			case "revision":
				request.ExpectedRevision--
			case "fence":
				request.FencingToken++
			case "report":
				request.BlockerAttemptID = "attempt_unrelated"
			case "operator":
				current, err := h.admin.Transition(t.Context(), blocked.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: blocked.Revision, State: "Blocked", Reason: "user_requested"})
				if err != nil {
					t.Fatal(err)
				}
				if current.Revision != blocked.Revision {
					t.Fatalf("same-lane operator hold created a record revision: %d/%d", current.Revision, blocked.Revision)
				}
				request.ExpectedRevision = current.Revision
			case "policy":
				h.repolicy(t)
			}
			if test.refusal == "policy" {
				if _, err := h.native.Transition(t.Context(), blocked.WorkItemID, request); err != nil || h.state(t, issue.ID) != prior {
					t.Fatalf("configuration change prevented recovery: %v", err)
				}
				return
			}
			if test.refusal != "" {
				_, err := h.native.Transition(t.Context(), blocked.WorkItemID, request)
				if err == nil || h.state(t, issue.ID) != "Blocked" {
					t.Fatalf("%s authority was bypassed: %v", test.refusal, err)
				}
				if test.laterLease != "" {
					var failure *APIError
					if !errors.As(err, &failure) || failure.Status != http.StatusConflict || failure.Code != "stale_fencing_token" {
						t.Fatalf("newer authority refusal = %v, want stale fencing", err)
					}
				}
				return
			}
			if test.laterLease != "" {
				request.ReasonDetail = "dependency_auto_unblock"
				if _, err := h.native.Transition(t.Context(), blocked.WorkItemID, request); err != nil {
					t.Fatalf("ended claim without an attempt prevented recorded recovery: %v", err)
				}
			}
			if test.advancePolicy {
				h.repolicy(t)
			}
			started := make(chan runner.RunRequest, 1)
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: 20 * time.Millisecond, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Blocked", "Human Review"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: nativeDependencyRecoveryRunner{started: started}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- orch.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			waitHolding := func(want int) {
				t.Helper()
				testenv.WaitUntil(t, t.Context(), func() error {
					current, err := orch.State(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					entry := current.Blocked[issue.ID]
					holding := 0
					for _, evidence := range entry.BlockerEvidence {
						if evidence.Status == "holds" {
							holding++
						}
					}
					if len(entry.BlockerEvidence) == wantBlockers && holding == want {
						if h.state(t, issue.ID) != "Blocked" {
							t.Fatal("unfinished prerequisite released the dependency hold")
						}
						return nil
					}
					select {
					case <-started:
						t.Fatal("native recovery dispatched before prerequisite readiness")
					default:
					}
					return fmt.Errorf("native recovery never observed the unmet predicate: %+v", entry)
				})
			}
			if test.laterLease == "" {
				waitHolding(wantBlockers)
				if _, err := h.admin.Transition(t.Context(), prerequisite.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Done", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				if _, err := orch.RequestRefresh(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if test.second {
				waitHolding(1)
				if _, err := h.admin.Transition(t.Context(), second.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: second.Revision, State: "Done", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				if _, err := orch.RequestRefresh(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case run := <-started:
				if run.Issue.ID != issue.ID || run.Issue.State != prior {
					t.Fatalf("recovery dispatched wrong lane: %+v", run.Issue)
				}
				if test.dirty {
					fresh, ok := run.Execution.(Lifecycle)
					if !ok || fresh.Recovery().Lease.FencingToken <= latestFence || len(fresh.Recovery().Attempts) != 1 || !reflect.DeepEqual(fresh.Recovery().Attempts[0].Checkpoint, &checkpoint) {
						t.Fatal("prerequisite recovery lost fresh fencing or retained dirty checkpoint")
					}
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatalf("native recorded recovery did not autonomously dispatch: state=%s", h.state(t, issue.ID))
			}
			page, err := h.admin.History(t.Context(), blocked.WorkItemID, "")
			if err != nil {
				t.Fatal(err)
			}
			var recovered, claimed int64
			for _, event := range page.Items {
				if event.Type == "workflow.transitioned" && strings.ReplaceAll(event.Data.ReasonDetail, " ", "_") == "dependency_auto_unblock" && event.Data.BlockerAttemptID == attempt.AttemptID {
					recovered = event.AggregateSequence
				}
				if event.Type == "scheduler.decision" && event.Data.Decision != nil && event.Data.Decision.Outcome == "claimed" {
					claimed = event.AggregateSequence
				}
			}
			if recovered == 0 || claimed <= recovered {
				t.Fatalf("recovery transition then fresh claim missing: recovered=%d claimed=%d", recovered, claimed)
			}
		})
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
