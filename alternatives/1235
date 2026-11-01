package scenariotest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func RunNativeRecordedDependencyAdmission(t *testing.T, factories Constructors) {
	runtimeDatabasePath := testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
		return store.Open(ctx, store.Config{Path: path})
	})
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
			h := newNativeChangeHubConfigured(t, "Human Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "Human Review"}},
				{Name: "Blocked", Transitions: []string{"In Progress", "Blocked"}},
				{Name: "Human Review"},
				{Name: "Backlog", Transitions: []string{"Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Backlog"}},
			}, true, true, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
			if test.repositoryAlias {
				h.reopen(func(path string) error {
					db, err := sql.Open("sqlite", path)
					if err != nil {
						return err
					}
					_, err = db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = ? WHERE organization_id = ? AND id = ?", "example-org/example-project", h.organization, h.project)
					return errors.Join(err, db.Close())
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
			blocked, err := h.native.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
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
				var retiredPath string
				if test.retainedWriter {
					h.reopen(func(path string) error {
						retiredPath = path
						return nil
					})
				}
				h.reopen(func(path string) error {
					if retiredPath != "" {
						retired, err := sql.Open("sqlite", retiredPath)
						if err != nil {
							return err
						}
						defer retired.Close()
						tx, err := retired.BeginTx(t.Context(), nil)
						if err != nil {
							return err
						}
						defer tx.Rollback()
						if _, err := tx.ExecContext(t.Context(), "UPDATE leases SET renewed_at = renewed_at WHERE lease_id = ?", attempt.LeaseID); err != nil {
							return err
						}
					}
					db, err := sql.Open("sqlite", path)
					if err != nil {
						return err
					}
					now := time.Now().UTC()
					expires := now.Add(time.Hour)
					var released any
					switch test.laterLease {
					case "released", "attempt":
						released = now.Format(time.RFC3339Nano)
					case "expired":
						expires = now.Add(-time.Minute)
					}
					result, err := db.ExecContext(t.Context(), `INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at, work_item_revision)
SELECT 'later-lease', issue_id, machine_id, 'later-session', ?, acquired_at, renewed_at, ?, created_at, updated_at, work_item_revision FROM leases WHERE lease_id = ?`, expires.Format(time.RFC3339Nano), released, attempt.LeaseID)
					if err != nil {
						return errors.Join(err, db.Close())
					}
					fence, err := result.LastInsertId()
					if err != nil {
						return errors.Join(err, db.Close())
					}
					latestFence = tracker.FencingToken(fence)
					if test.laterLease == "attempt" {
						data := attempt.NativeRunData
						data.AttemptID, data.LeaseID, data.FencingToken = "later-attempt", "later-lease", latestFence
						data.RunID, data.SessionID = "later-run", "later-session"
						encoded, err := json.Marshal(data)
						if err != nil {
							return errors.Join(err, db.Close())
						}
						_, err = db.ExecContext(t.Context(), `INSERT INTO native_attempts (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, started_at, updated_at, work_item_revision, dispatch_generation)
SELECT ?, organization_id, project_id, work_item_id, ?, ?, ?, sequence, status, ?, started_at, updated_at, work_item_revision, dispatch_generation FROM native_attempts WHERE id = ?`, data.AttemptID, data.LeaseID, data.FencingToken, data.RunID, string(encoded), attempt.AttemptID)
						if err != nil {
							return errors.Join(err, db.Close())
						}
					}
					return db.Close()
				})
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
			runtimeStore, err := store.Open(t.Context(), store.Config{Path: runtimeDatabasePath(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtimeStore.Close() })
			started := make(chan runner.RunRequest, 1)
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor, PollInterval: 20 * time.Millisecond, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, ObservedStates: []string{"Blocked", "Human Review"}, TerminalStates: []string{"Done"}}, orchestrator.Dependencies{Connector: h.connector, Scheduling: h.scheduler, Runner: nativeDependencyRecoveryRunner{started: started}, WorkAttempts: runtimeStore, LaneLedger: runtimeStore, WorkflowMetrics: runtimeStore, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
