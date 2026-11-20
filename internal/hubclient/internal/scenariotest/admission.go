package scenariotest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariohooks"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func RunNativeRecordedDependencyAdmission(t *testing.T, factories Constructors) {
	for _, test := range []struct {
		name, refusal   string
		transfer        string
		reasonCode      string
		laterLease      string
		qualified       bool
		repositoryAlias bool
		advancePolicy   bool
		dirty           bool
		second          bool
		external        bool
		instance        bool
		retryInPlace    bool
		retainedWriter  bool
	}{

		{name: "saved commit dependency wait across Hub restart", transfer: "commit"},
		{name: "unowned holding evidence retries after terminal receipt", transfer: "unowned"},
		{name: "saved PR dependency wait across Hub restart", transfer: "pr"},
		{name: "merge return lane across Hub restart", transfer: "merge"},
		{name: "completed dependency releases saved source after restart", transfer: "ready"},
		{name: "removed native edge releases saved source after restart", transfer: "removed"},
		{name: "reported prerequisite registers dependency hold"},
		{name: "instance failure retries on another runner without a dependency", instance: true, retryInPlace: true},
		{name: "typed instance reason retries after a later ended claim", instance: true, retryInPlace: true, reasonCode: "instance_limitation", laterLease: "released", dirty: true},
		{name: "bare human action on instance failure stays dispatchable", instance: true, retryInPlace: true, reasonCode: "instance_limitation", refusal: "bare human"},
		{name: "typed instance reason preserves human hold", instance: true, reasonCode: "instance_limitation", refusal: "human"},
		{name: "configuration change permits instance retry", instance: true, retryInPlace: true, reasonCode: "instance_limitation", refusal: "policy"},
		{name: "instance evidence preserves permission reason", instance: true, reasonCode: "permission_wait", refusal: "reason"},
		{name: "instance evidence with unknown reason stays dispatchable", instance: true, retryInPlace: true, reasonCode: "unknown_hold"},
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
			if test.transfer != "" {
				runNativeDependencyTransfer(t, factories, test.transfer)
				return
			}
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
			prerequisite, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: "Actual prerequisite", State: "Backlog"})
			if err != nil {
				t.Fatal(err)
			}
			var second tracker.NativeIssue
			if test.second {
				second, err = h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: "Second prerequisite", State: "Backlog"})
				if err != nil {
					t.Fatal(err)
				}
			}
			issue := h.createInProgress(t, "Reported dependency")
			var unrelated tracker.NativeIssue
			if test.refusal == "relation" {
				other, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: "Unrelated prerequisite", State: "Done"})
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
				if _, err := h.admin.Transition(t.Context(), unrelated.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: unrelated.Revision, State: "Backlog", Reason: "user_requested"}); err != nil {
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
			if test.refusal == "human" || test.refusal == "bare human" {
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
				if test.refusal == "human" {
					report = strings.Replace(report, "human_action:", "  - owner: human\n    reason: Release approval required\nhuman_action:", 1)
				}
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
			recordedIssue := recovery.Issue
			wantState := "Blocked"
			if test.retryInPlace {
				wantState = "In Progress"
			}
			if recordedIssue.State != wantState {
				t.Fatalf("lane after attempt = %q, want %q", recordedIssue.State, wantState)
			}
			if slices.Contains(recordedIssue.OmittedFields, "dependencies") {
				t.Fatal("dependency assertion requires the authoritative relationship read")
			}
			wantDependencies := 1
			if test.refusal == "human" || test.external || test.instance {
				wantDependencies = 0
			} else if test.refusal == "relation" || test.second {
				wantDependencies = 2
			}
			if len(recordedIssue.Dependencies) != wantDependencies {
				t.Fatalf("reported dependency links = %+v, want %d", recordedIssue.Dependencies, wantDependencies)
			}
			hydrated, err := h.connector.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			wantBlockers := 1
			if test.second || test.instance && test.refusal == "human" {
				wantBlockers = 2
			}
			if err != nil || len(hydrated) != 1 {
				t.Fatalf("hydrate issue: got %d issues, error %v", len(hydrated), err)
			}
			signal := hydrated[0].WithNativeWorkpadAuthority().WorkpadSignal
			if test.retryInPlace {
				if signal != nil {
					t.Fatalf("dispatchable issue has a Blocked signal: %+v", signal)
				}
			} else if signal == nil || len(signal.Blockers) != wantBlockers {
				t.Fatalf("native disposition lost its prerequisite: signal=%+v, want %d blockers", signal, wantBlockers)
			}
			resolved, err := h.connector.FetchIssueStatesByIdentifiers(t.Context(), []string{string(h.project) + fmt.Sprintf("#%d", prerequisite.Number)})
			if err != nil || len(resolved) != 1 || resolved[0].ID != string(prerequisite.WorkItemID) || resolved[0].Closed {
				t.Fatalf("native reference resolved incorrectly: %+v, %v", resolved, err)
			}
			var attempt tracker.NativeAttempt
			prior := "In Progress"
			if test.retryInPlace {
				page, err := h.admin.Attempts(t.Context(), recordedIssue.WorkItemID, "")
				if err != nil || len(page.Items) != 1 || page.NextCursor != "" {
					t.Fatalf("read instance attempt: got %d attempts, next %q, error %v", len(page.Items), page.NextCursor, err)
				}
				attempt = page.Items[0]
				if attempt.Status != "succeeded" || attempt.CompletionBody != report || attempt.Disposition == nil || attempt.Disposition.Status != workpad.StatusBlocked || attempt.Disposition.ReasonCode != test.reasonCode || len(attempt.Disposition.BlockerEvidence) != 1 || attempt.Disposition.BlockerEvidence[0].Owner != workpad.BlockerOwnerInstance || attempt.Disposition.BlockerEvidence[0].Ref != "instance:check-barrier" {
					t.Fatalf("instance attempt lost its recorded failure: %+v, disposition=%+v", attempt, attempt.Disposition)
				}
			} else {
				attempts, history, err := h.connector.RecordedBlockerContext(t.Context(), recordedIssue.WorkItemID)
				if err != nil {
					t.Fatal(err)
				}
				var valid bool
				attempt, prior, valid = tracker.RecordedNativeBlockers(recordedIssue, attempts, history)
				if !valid || prior != "In Progress" {
					t.Fatalf("current report missing: %+v, %s, %t", attempt, prior, valid)
				}
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
				page, err := h.admin.History(t.Context(), recordedIssue.WorkItemID, "")
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
				laterRunner, laterMachine := attempt.RunnerID, attempt.MachineID
				if test.laterLease == "active" {
					identity, err := h.otherWorker(t).Transport().RunnerIdentity(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					laterRunner, laterMachine = identity.RunnerID, identity.MachineID
				}
				var later leasestore.Lease
				seedLater := func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), h.project, recordedIssue.WorkItemID)
					if err != nil {
						return err
					}
					ttl := time.Hour
					if test.laterLease == "expired" {
						ttl = time.Minute
					}
					later, err = w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(laterMachine), RunnerID: laterRunner, SessionID: "later-session", LeaseID: "later-lease-" + issue.ID, TTL: ttl})
					if err != nil {
						return err
					}
					latestFence = tracker.FencingToken(later.FencingToken)
					if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: h.organization, ProjectID: h.project, LeaseID: tracker.LeaseID(later.LeaseID), PolicyID: h.descriptor.ID, BoundAt: h.clock()}); err != nil {
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
						history, err := json.Marshal(tracker.CollaborationEvent{ID: api.ExecutionID("evt", data.AttemptID), OrganizationID: h.organization, ProjectID: h.project, AggregateID: recordedIssue.WorkItemID, Type: "run.finished", SchemaVersion: 1, RecordedAt: h.clock(), Actor: tracker.Actor{Kind: "runner", PrincipalID: attempt.RunnerID}, Data: tracker.CollaborationData{Run: &data}})
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
			request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "too-early", LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken}, PolicyID: h.descriptor.ID, BlockerAttemptID: attempt.AttemptID, ExpectedRevision: recordedIssue.Revision, State: prior, Reason: "dependency_ready", ReasonDetail: "recorded_blocker_recovery"}
			if test.instance {
				worker := h.otherWorker(t)
				if !test.retryInPlace {
					if _, err := worker.Transition(t.Context(), recordedIssue.WorkItemID, request); err == nil || h.state(t, issue.ID) != "Blocked" {
						t.Fatalf("%s authority was bypassed: %v", test.refusal, err)
					}
					return
				}
				if test.refusal == "policy" {
					h.repolicy(t)
				}
				identity, err := worker.Transport().RunnerIdentity(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				machine := fixtureSchedulerMachine(h.scheduler)
				machine.ID = identity.MachineID
				secondRunner, err := newTestScheduler(worker.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, Now: h.clock})
				if err != nil {
					t.Fatal(err)
				}
				candidates := CandidatesIn(t, secondRunner, h.descriptor, wantState)
				if len(candidates) != 1 || candidates[0].ID != issue.ID {
					t.Fatalf("second runner candidates = %+v, want %s", candidates, issue.ID)
				}
				if _, err := secondRunner.AdoptClaim(t.Context(), candidates[0], h.clock()); err != nil {
					t.Fatal(err)
				}
				claim, ok := secondRunner.ExecutionClaim(issue.ID)
				if !ok || claim.Lease.MachineID != machine.ID || claim.Lease.FencingToken <= latestFence {
					t.Fatalf("second runner lease = %+v, want machine %s with fence above %d", claim.Lease, machine.ID, latestFence)
				}
				if err := secondRunner.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				return
			}
			if _, err := h.native.Transition(t.Context(), recordedIssue.WorkItemID, request); err == nil {
				t.Fatal("recovery cleared a genuinely unresolved prerequisite")
			}
			if len(h.candidates(t)) != 0 {
				t.Fatal("blocked prerequisite was admitted")
			}
			if test.refusal != "" || test.laterLease != "" {
				if _, err := h.admin.Transition(t.Context(), prerequisite.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Done", Reason: "user_requested"}); err != nil {
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
				current, err := h.admin.Transition(t.Context(), recordedIssue.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: recordedIssue.Revision, State: "Blocked", Reason: "user_requested"})
				if err != nil {
					t.Fatal(err)
				}
				if current.Revision != recordedIssue.Revision {
					t.Fatalf("same-lane operator hold created a record revision: %d/%d", current.Revision, recordedIssue.Revision)
				}
				request.ExpectedRevision = current.Revision
			case "policy":
				h.repolicy(t)
			}
			if test.refusal == "policy" {
				if _, err := h.native.Transition(t.Context(), recordedIssue.WorkItemID, request); err != nil || h.state(t, issue.ID) != prior {
					t.Fatalf("configuration change prevented recovery: %v", err)
				}
				return
			}
			if test.refusal != "" {
				_, err := h.native.Transition(t.Context(), recordedIssue.WorkItemID, request)
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
				if _, err := h.native.Transition(t.Context(), recordedIssue.WorkItemID, request); err != nil {
					t.Fatalf("ended claim without an attempt prevented recorded recovery: %v", err)
				}
			}
			if test.advancePolicy {
				h.repolicy(t)
			}
			started := make(chan runner.RunRequest, 1)
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: 20 * time.Millisecond, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Blocked", "Human Review"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: scenariohooks.StartedRunner{Started: started}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
				if _, err := h.admin.Transition(t.Context(), prerequisite.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Done", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				if _, err := orch.RequestRefresh(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if test.second {
				waitHolding(1)
				if _, err := h.admin.Transition(t.Context(), second.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: second.Revision, State: "Done", Reason: "user_requested"}); err != nil {
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
			page, err := h.admin.History(t.Context(), recordedIssue.WorkItemID, "")
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

func runNativeDependencyTransfer(t *testing.T, factories Constructors, transfer string) {
	t.Helper()
	ctx := t.Context()
	var retryClockOffset atomic.Int64
	retryNow := func() time.Time { return time.Now().Add(time.Duration(retryClockOffset.Load())) }
	var unlink func(context.Context) error
	if transfer == "unowned" {
		create := factories.New
		var unlinked atomic.Bool
		factories.New = func(host executioncontract.ExecutionHost, view executioncontract.ExecutionClaim) executioncontract.Execution {
			actual := create(host, view)
			return &nativeDependencyUnlinkedExecution{Lifecycle: actual.(Lifecycle), afterFinish: func(ctx context.Context) error {
				if unlinked.Swap(true) {
					return nil
				}
				if unlink == nil {
					return errors.New("native relation removal fixture unavailable")
				}
				return unlink(ctx)
			}}
		}
	}
	lane := "Rework"
	initialLane := "In Progress"
	if transfer == "merge" {
		lane = "Merging"
	}
	withPR := transfer == "pr" || transfer == "merge"
	if withPR {
		initialLane = lane
	}
	ready := transfer == "ready"
	removed := transfer == "removed"
	states := []tracker.NativeState{}
	transitions := []string{"Todo", "In Progress", "Rework", "Merging", "Blocked", "Backlog", "Human Review", "Done"}
	for _, name := range transitions {
		states = append(states, tracker.NativeState{Name: name, Dispatchable: name == "Todo" || name == "In Progress" || name == "Rework" || name == "Merging", Terminal: name == "Done", Transitions: transitions})
	}
	h := newNativeChangeHubConfigured(t, "Human Review", states, true, true, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Clock: retryNow}, factories)
	priority := 1
	created, err := h.admin.CreateIssue(ctx, tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: "Saved implementation", Body: "Preserve the saved implementation." + issueContractTestSections, State: initialLane, Priority: &priority})
	if err != nil {
		t.Fatal(err)
	}
	issue := api.IssueFromNative(created)
	request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, WorkflowStates: []string{"Todo", "In Progress", "Rework", "Merging"}, PrioritizeUnblockers: true, DispatchPriorityByState: []string{"Merging", "Rework", "In Progress", "Todo"}}
	priority = 2
	prerequisite, err := h.admin.CreateIssue(ctx, tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: "Prerequisite", Body: testenv.DistinctText("prerequisite", 24) + "\nComplete the prerequisite." + issueContractTestSections, State: "Backlog", Priority: &priority})
	if err != nil {
		t.Fatal(err)
	}
	unlink = func(ctx context.Context) error {
		current, err := h.admin.Issue(ctx, created.WorkItemID)
		if err != nil {
			return err
		}
		if len(current.Dependencies) != 1 || current.Dependencies[0] != prerequisite.WorkItemID {
			return errors.New("terminal report did not create native dependency")
		}
		_, err = h.admin.Dependency(ctx, created.WorkItemID, tracker.DependencyMutation{Mutation: api.NativeMutationKey(), ExpectedRevision: current.Revision, RelatedWorkItemID: prerequisite.WorkItemID, Operation: "remove"})
		return err
	}
	priority = 3
	unrelated, err := h.admin.CreateIssue(ctx, tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: "Unrelated work", Body: testenv.DistinctText("unrelated-work", 24) + "\nComplete unrelated work." + issueContractTestSections, State: "Backlog", Priority: &priority})
	if err != nil {
		t.Fatal(err)
	}
	gitOutput := func(directory string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("repository fixture: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	source := nativeChangeSourceRepo(t)
	remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
	nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
	nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
	nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
	nativeChangeGit(t, source, "push", "-u", "origin", "main")
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "original"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	workspaceIssue := workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier}
	original, err := backend.Create(ctx, workspaceIssue)
	if err != nil {
		t.Fatal(err)
	}
	base := gitOutput(original.Path, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(original.Path, "saved-change.txt"), []byte("saved implementation"), 0600); err != nil {
		t.Fatal(err)
	}
	nativeChangeGit(t, original.Path, "add", "saved-change.txt")
	nativeChangeGit(t, original.Path, "commit", "-m", "Save implementation")
	head := gitOutput(original.Path, "rev-parse", "HEAD")
	capture, err := workspace.CaptureChangeSource(ctx, original.Path, base, head)
	if err != nil {
		t.Fatal(err)
	}
	change, err := h.admin.CreateChange(ctx, created.WorkItemID, tracker.CreateChange{Mutation: api.NativeMutationKey(), Title: "Saved implementation"})
	if err != nil {
		t.Fatal(err)
	}
	var external *tracker.ChangeExternalReference
	if withPR {
		external = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: nativeChangeRepository + "/pull/7"}
	}
	version := h.publishCapturedExternal(t, created.WorkItemID, change.ID, head, &capture, external, "", base)
	if _, err := h.admin.ReviewChange(ctx, created.WorkItemID, change.ID, version.ID, tracker.ReviewChange{Mutation: api.NativeMutationKey(), Decision: "approved"}); err != nil {
		t.Fatal(err)
	}
	request.LandingCapacity = 1
	request.NativeLandingBatch = transfer == "merge"
	candidates, err := h.scheduler.FetchCandidateIssues(ctx, request)
	if err != nil || len(candidates) != 1 || candidates[0].ID != issue.ID {
		t.Fatalf("initial candidates=%+v error=%v", candidates, err)
	}
	if _, err := h.scheduler.AdoptClaim(ctx, candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	execution := h.scheduler.RunExecution(issue.ID)
	if err := execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "example-model"}); err != nil {
		t.Fatal(err)
	}
	checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", HeadSHA: head, ExternalEffect: "none", EffectState: "none", Change: &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: head}}
	execution.(runner.DiffExecution).SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
		return tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: head, Files: []tracker.AttemptDiffFile{{Path: "saved-change.txt", Status: tracker.DiffStatusAdded, Additions: 1, Patch: capture.Patch}}}, true
	})
	if err := execution.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	report := fmt.Sprintf("```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '%s#%d'\n    reason: prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action: null\n```", h.project, prerequisite.Number)
	globalGate := scheduler.NewGlobalDispatchGate(scheduler.NewStrictPriority(scheduler.Config{Capacity: 1}))
	candidateChanges := make(chan struct{}, 1)
	completionSource := &nativeCompletionScheduling{Scheduler: h.scheduler, issue: candidates[0], released: make(chan error, 1), wake: candidateChanges, idleFetchHold: scenariohooks.New(func() bool { return globalGate.PoolSnapshot().Available == 1 })}
	completed := make(chan nativeCompletionRun, 1)
	runtimeRunner, err := runner.NewRunner(runner.Dependencies{ProjectID: "local", Now: retryNow, ProcessSource: emptyProcessSource, Store: api.NewRunnerSessions(h.native), Workspace: backend, AgentBackend: &committingAgent{}, Workflow: config.Workflow{Config: config.Config{Policy: h.descriptor, Agents: config.Agents{Routes: []config.AgentRoute{{Name: "default", Backend: config.DefaultAgentBackendID, Model: "example-model", Default: true}}}}, Prompt: "Preserve the saved implementation"}})
	if err != nil {
		t.Fatal(err)
	}
	backendRunner := &nativeCompletionRunner{Runner: runtimeRunner, prepared: &runner.RunResult{FinalState: runner.FinalStateCompleted, FinalMessage: report, TurnStarted: true, DiffStats: runner.DiffStats{Status: "clean", HeadSHA: head, UnpushedCommits: 1}, NativeChange: &runner.NativeChange{Changed: true, ChangeID: change.ID, VersionID: version.ID, BaseSHA: base, HeadSHA: head, Reviewed: true, MergeResolution: transfer == "merge"}}, completed: completed, prepare: func(ctx context.Context, request runner.RunRequest) error {
		if globalGate.PoolSnapshot().Available != 0 {
			return errors.New("dependency worker did not acquire global slot")
		}
		completionSource.idleFetchHold.Arm()
		return request.Execution.(runner.CompletionExecution).PrepareFinish(ctx, "succeeded", report, nil)
	}}
	var retryStarted chan nativeCompletionRun
	if transfer == "unowned" {
		retryStarted = make(chan nativeCompletionRun, 1)
		backendRunner.retryStarted = retryStarted
		completionSource.retryCandidates = make(chan error, 1)
	}
	completionOrch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: time.Hour, MaxConcurrentAgents: 1, FailureRetryBaseDelay: time.Hour, MaxRetryBackoff: time.Hour, ActiveStates: request.WorkflowStates, TerminalStates: []string{"Done"}, AutoPromote: orchestrator.AutoPromoteConfig{HumanReview: new(false)}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: completionSource, Runner: backendRunner, GlobalDispatchGate: globalGate, Now: retryNow, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	completionCtx, completionCancel := context.WithCancel(ctx)
	completionDone := make(chan error, 1)
	go func() { completionDone <- completionOrch.Run(completionCtx) }()
	var completionStop sync.Once
	stopCompletion := func() {
		completionStop.Do(func() {
			completionCancel()
			if err := <-completionDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stopCompletion)
	select {
	case result := <-completed:
		if result.err != nil {
			t.Fatalf("dependency worker preparation: %v", result.err)
		}
	case <-ctx.Done():
		t.Fatal("dependency worker did not complete")
	}
	completionSource.idleFetchHold.AwaitHeld(t, ctx)
	var settled orchestrator.State
	testenv.WaitUntil(t, ctx, func() error {
		current, err := completionOrch.State(ctx)
		if err != nil {
			return err
		}
		if len(current.Running) != 0 || !current.RuntimeObservation.IsZero() {
			completionSource.idleFetchHold.Release()
			return errors.New("dependency completion is still running")
		}
		settled = current
		return nil
	})
	if transfer == "unowned" {
		if globalGate.PoolSnapshot().Available != 1 || len(settled.Running) != 0 || len(settled.Claimed) != 0 || len(settled.Retry) != 1 || len(settled.Blocked) != 0 {
			t.Fatalf("terminal unowned report lost retry or retained claim: %+v", settled)
		}
		retry, ok := settled.Retry[issue.ID]
		if !ok || retry.Attempt != 1 {
			t.Fatalf("unowned retry=%+v", retry)
		}
		select {
		case err := <-completionSource.released:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("terminal retry did not release its finished execution")
		}
		originalAttempt := execution.(Lifecycle).CompletionData()
		if originalAttempt.Runtime == nil || originalAttempt.Runtime.Completion == nil || originalAttempt.Runtime.Completion.AcceptanceRecorded {
			t.Fatal("terminal dependency receipt lost unaccepted completion observation")
		}
		h.restart()
		history, err := h.admin.History(ctx, created.WorkItemID, "")
		if err != nil {
			t.Fatal(err)
		}
		finished := 0
		for _, event := range history.Items {
			if event.Type == "run.finished" && event.Data.Run != nil && event.Data.Run.AttemptID == originalAttempt.AttemptID {
				finished++
			}
		}
		if finished != 1 {
			t.Fatalf("duplicate Finish produced %d terminal events", finished)
		}
		retryClockOffset.Add(int64(2 * time.Hour))
		completionSource.resumeCandidates.Store(true)
		candidateChanges <- struct{}{}
		select {
		case started := <-retryStarted:
			if started.err != nil {
				t.Fatalf("successor start: %v", started.err)
			}
			next := started.request
			current := next.Execution.(Lifecycle).CompletionData()
			if current.AttemptID == "" || current.AttemptID == originalAttempt.AttemptID || current.FencingToken <= originalAttempt.FencingToken || backendRunner.calls.Load() != 2 || globalGate.PoolSnapshot().Available != 0 {
				t.Fatalf("retry reused terminal execution: previous=%+v current=%+v", originalAttempt, current)
			}
			evidence, err := h.native.RuntimeEvidence(ctx, created.WorkItemID, current.AttemptID)
			if err != nil || evidence.Attempt == nil || evidence.Attempt.Status != "running" {
				t.Fatalf("successor did not actually start: %+v error=%v", evidence.Attempt, err)
			}
		case err := <-completionSource.retryCandidates:
			t.Fatalf("successor candidate fetch: %v", err)
		case <-ctx.Done():
			t.Fatal("terminal unowned report never started successor")
		}
		return
	}
	if globalGate.PoolSnapshot().Available != 1 || len(settled.Running) != 0 || len(settled.Claimed) != 0 || len(settled.Retry) != 0 || len(settled.Blocked) != 0 || len(settled.Completed) != 0 {
		t.Fatalf("completed dependency wait: available=%d running=%d claimed=%d retry=%d blocked=%d completed=%d retry_error=%q", globalGate.PoolSnapshot().Available, len(settled.Running), len(settled.Claimed), len(settled.Retry), len(settled.Blocked), len(settled.Completed), settled.Retry[issue.ID].Error)
	}
	select {
	case err := <-completionSource.released:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("completion returned without a claim release receipt")
	}
	if actual := h.state(t, issue.ID); actual != lane {
		t.Fatalf("completed dependency wait lane=%s want=%s", actual, lane)
	}
	stopCompletion()
	identity, err := runnerauth.Load(h.native.Transport().IdentityFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.native.Claim(ctx, tracker.NativeClaim{PolicyID: h.descriptor.ID, WorkItemID: created.WorkItemID, MachineID: identity.Identity.MachineID, SessionID: "dependency-refusal", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: request.WorkflowStates}); !errors.Is(err, api.ErrNoClaimableWork) {
		t.Fatalf("unfinished dependency claim=%v", err)
	}
	before, err := h.admin.History(ctx, created.WorkItemID, "")
	if err != nil {
		t.Fatal(err)
	}
	decisions := []tracker.CollaborationEvent{}
	for _, event := range before.Items {
		if event.Type == "scheduler.decision" {
			decisions = append(decisions, event)
		}
	}
	if len(decisions) != 2 {
		t.Fatalf("scheduler decisions=%+v want claim and refusal", decisions)
	}
	var refusal tracker.CollaborationEvent
	for _, event := range decisions {
		if event.Data.Decision != nil && event.Data.Decision.Outcome == "skipped" {
			refusal = event
		}
	}
	if refusal.ID == "" || refusal.Data.Decision.Reason != "no_claimable_work" {
		t.Fatalf("dependency refusal=%+v", refusal)
	}
	h.restart()
	recovered, err := h.native.Recovery(ctx, created.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Issue.State != lane || slices.Contains(recovered.Issue.OmittedFields, "dependencies") || len(recovered.Issue.Dependencies) != 1 || recovered.Issue.Dependencies[0] != prerequisite.WorkItemID {
		t.Fatalf("current dispatchable dependency intent=%+v", recovered.Issue)
	}
	if len(recovered.Attempts) != 1 {
		t.Fatalf("persisted completed dependency attempts=%+v", recovered.Attempts)
	}
	previous := recovered.Attempts[0]
	if previous.Runtime == nil || previous.Runtime.Completion == nil || previous.Runtime.Completion.AcceptanceRecorded {
		t.Fatal("persisted dependency receipt lost unaccepted completion observation")
	}
	if previous.Status != "succeeded" || previous.Disposition == nil || previous.Disposition.Status != "blocked" || !reflect.DeepEqual(previous.Checkpoint, &checkpoint) {
		t.Fatalf("durable dependency/source=%+v", previous)
	}
	detail, err := h.admin.Change(ctx, created.WorkItemID, change.ID)
	if err != nil {
		t.Fatal(err)
	}
	var retained *tracker.ChangeVersion
	for index := range detail.Versions {
		if detail.Versions[index].ID == version.ID {
			retained = &detail.Versions[index]
		}
	}
	if retained == nil || retained.HeadSHA != head || retained.BaseSHA != base || !reflect.DeepEqual(retained.External, external) || !reflect.DeepEqual(retained.Source, version.Source) {
		t.Fatalf("canonical saved version=%+v", retained)
	}
	bundle, err := h.admin.ChangeSource(ctx, created.WorkItemID, change.ID, version.ID)
	if err != nil {
		t.Fatal(err)
	}
	freshSource := nativeChangeSourceRepo(t)
	nativeChangeGit(t, freshSource, "remote", "add", "origin", nativeChangeRepository)
	nativeChangeGit(t, freshSource, "config", "url."+remote+".insteadOf", nativeChangeRepository)
	if err := exec.CommandContext(ctx, "git", "-C", freshSource, "cat-file", "-e", head+"^{commit}").Run(); err == nil {
		t.Fatal("fresh repository already contains saved source")
	}
	freshBackend, err := workspace.NewLocalGit(workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "restored"), SourceRoot: freshSource, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	workspaceIssue.NativeRework = true
	workspaceIssue.Source = &workspace.ChangeSource{Version: *retained, Bundle: bundle, Transferred: true}
	restored, err := freshBackend.Create(ctx, workspaceIssue)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(restored.Path, "saved-change.txt"))
	if err != nil || string(contents) != "saved implementation" || restored.Branch != original.Branch || gitOutput(restored.Path, "rev-parse", "HEAD") != head {
		t.Fatalf("restored saved source branch=%s want=%s contents=%q error=%v", restored.Branch, original.Branch, contents, err)
	}
	after, err := h.admin.History(ctx, created.WorkItemID, "")
	if err != nil {
		t.Fatal(err)
	}
	foundRefusal := false
	for _, event := range after.Items {
		if event.ID == refusal.ID {
			foundRefusal = reflect.DeepEqual(event, refusal)
		}
	}
	if !foundRefusal {
		t.Fatal("restart lost exact persisted scheduler refusal")
	}
	runtime, err := h.native.RuntimeEvidence(ctx, created.WorkItemID, "", tracker.NativeAdmissionContext{PolicyID: h.descriptor.ID, WorkflowStates: request.WorkflowStates, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.LatestDecision == nil || runtime.LatestDecision.ID != refusal.ID || runtime.LatestDecision.Data.Decision == nil || runtime.LatestDecision.Data.Decision.Reason != refusal.Data.Decision.Reason {
		t.Fatalf("persisted refusal explanation=%+v want event=%s", runtime.LatestDecision, refusal.ID)
	}
	dependencyRefusal := false
	for _, admission := range runtime.Admission {
		for _, dependency := range admission.UnresolvedDependencies {
			dependencyRefusal = dependencyRefusal || dependency.ID == prerequisite.WorkItemID
		}
	}
	if !dependencyRefusal {
		t.Fatalf("generic refusal lacks actual current dependency evidence: %+v", runtime.Admission)
	}
	if ready {
		if _, err := h.admin.Transition(ctx, prerequisite.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Done", Reason: "user_requested"}); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := h.admin.Transition(ctx, prerequisite.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: prerequisite.Revision, State: "Todo", Reason: "user_requested"}); err != nil {
			t.Fatal(err)
		}
	}
	if removed {
		current, err := h.admin.Issue(ctx, created.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.admin.Dependency(ctx, created.WorkItemID, tracker.DependencyMutation{Mutation: api.NativeMutationKey(), ExpectedRevision: current.Revision, RelatedWorkItemID: prerequisite.WorkItemID, Operation: "remove"}); err != nil {
			t.Fatal(err)
		}
		changed, err := h.admin.Issue(ctx, created.WorkItemID)
		if err != nil || len(changed.Dependencies) != 0 {
			t.Fatalf("native edge removal=%+v error=%v", changed.Dependencies, err)
		}
	}
	if _, err := h.admin.Transition(ctx, unrelated.WorkItemID, tracker.Transition{Mutation: api.NativeMutationKey(), ExpectedRevision: unrelated.Revision, State: "Todo", Reason: "user_requested"}); err != nil {
		t.Fatal(err)
	}
	started := make(chan runner.RunRequest, 1)
	orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: time.Hour, MaxConcurrentAgents: 1, PrioritizeUnblockers: true, ActiveStates: request.WorkflowStates, ObservedStates: []string{"Blocked"}, TerminalStates: []string{"Done"}, DispatchPriorityByState: request.DispatchPriorityByState}, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: scenariohooks.StartedRunner{Started: started}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- orch.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	want := string(prerequisite.WorkItemID)
	if ready || removed {
		want = issue.ID
	}
	select {
	case run := <-started:
		if run.Issue.ID != want {
			t.Fatalf("capacity-one dispatched %s state=%s want %s; saved=%s prerequisite=%s unrelated=%s unrelated_priority=%d", run.Issue.ID, run.Issue.State, want, created.WorkItemID, prerequisite.WorkItemID, unrelated.WorkItemID, priority)
		}
		if want == issue.ID && run.Issue.State != lane {
			t.Fatalf("return lane=%s want=%s", run.Issue.State, lane)
		}
	case <-ctx.Done():
		t.Fatal("dependency restart did not dispatch")
	}
	for tick := 0; tick < 3; tick++ {
		before, err := orch.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := orch.RequestRefresh(ctx); err != nil {
			t.Fatal(err)
		}
		testenv.WaitUntil(t, ctx, func() error {
			current, err := orch.State(ctx)
			if err != nil {
				return err
			}
			if len(current.Running) != 1 || current.Running[want].Issue.ID != want {
				t.Fatalf("refresh displaced executable worker: %+v", current.Running)
			}
			if !current.LastRefreshAt.After(before.LastRefreshAt) {
				return errors.New("refresh has not completed")
			}
			return nil
		})
	}
}

type nativeDependencyUnlinkedExecution struct {
	Lifecycle
	afterFinish func(context.Context) error
}

func (e *nativeDependencyUnlinkedExecution) Finish(ctx context.Context, outcome string) error {
	if err := e.Lifecycle.Finish(ctx, outcome); err != nil {
		return err
	}
	if outcome != "succeeded" {
		return nil
	}
	runtime, ok := e.Lifecycle.(runner.RuntimeExecution)
	if !ok {
		return errors.New("native completion runtime unavailable")
	}
	observed := time.Now().UTC()
	if err := runtime.ObserveRuntime(ctx, tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: observed, Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: false, ObservedAt: observed}}); err != nil {
		return err
	}
	if err := e.Lifecycle.Finish(ctx, outcome); err != nil {
		return err
	}
	return e.afterFinish(ctx)
}
