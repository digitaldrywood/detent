package runnerruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestPostgresRunnerAuditDispositionUsesArchivedSource(t *testing.T) {
	f := newRunnerPGFixture(t)
	for index, lease := range f.leases {
		native, ctx := f.client(t, index)
		now := time.Now().UTC().Truncate(time.Microsecond)
		sessionID, err := api.NewRunnerSessions(native).StartSession(ctx, store.SessionStart{IssueID: string(lease.WorkItemID), StartedAt: now, AgentRole: "security_audit", Model: "example-model"})
		if err != nil {
			t.Fatal(err)
		}
		key := securityaudit.Key{ProjectID: "prj_runner", Repository: "example-org/example-project", PRNumber: index + 1, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat(strconv.Itoa(index+1), 40)}
		changeID, versionID := hubapi.NewID("change"), hubapi.NewID("version")
		change := tracker.ChangeRequest{ID: changeID, OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}
		changeRaw, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		content := eventstore.NewContent(f.service.runDetailStore())
		changeRow, err := content.PrepareChange(ctx, "org_startup", changeRaw)
		if err != nil {
			t.Fatal(err)
		}
		version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, CreatedAt: now, ChangeVersionInput: tracker.ChangeVersionInput{Repository: key.Repository, BaseSHA: key.BaseSHA, HeadSHA: key.HeadSHA, AttemptID: f.data[index].AttemptID, PolicyID: f.policy.PolicyID}}
		versionRaw, err := json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
		versionRow, err := content.PrepareVersion(ctx, "org_startup", versionRaw)
		if err != nil {
			t.Fatal(err)
		}
		run := securityaudit.Run{ID: sessionID, ProjectID: "prj_runner", Repository: key.Repository, PRNumber: key.PRNumber, BaseSHA: key.BaseSHA, HeadSHA: key.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), Findings: []securityaudit.Finding{{ID: "finding_example", Body: "Example finding", Certainty: securityaudit.CertaintyConfirmed}}}
		review := tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: versionID, Decision: "changes_requested", CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: sessionID, VersionID: versionID, Run: run}}
		reviewRaw, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		reviewRow, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: "org_startup", ChangeID: changeID, VersionID: &versionID, Kind: "review", CreatedAt: now}, reviewRaw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateChange(ctx, changeRow); err != nil {
				return err
			}
			if _, err := w.CreateVersion(ctx, versionRow); err != nil {
				return err
			}
			_, err := w.AppendEvidence(ctx, reviewRow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		disposition, err := native.RecordAuditDisposition(ctx, securityaudit.DispositionCommand{Key: key, FindingID: "finding_example", Evidence: "The exact source requires authenticated access.", Confirm: true})
		if err != nil || disposition.ID < 1 || disposition.AuditRunID != sessionID {
			t.Fatalf("Hub disposition: %+v %v", disposition, err)
		}
		restarted, restartedCtx := f.client(t, index)
		evidence, err := restarted.AuditEvidence(restartedCtx, key)
		if err != nil || evidence.Run.ID != sessionID || len(evidence.Dispositions) != 1 || evidence.Dispositions[0].Evidence != disposition.Evidence {
			t.Fatalf("restart lost audit disposition: %+v %v", evidence, err)
		}
		key.HeadSHA = strings.Repeat("f", 40)
		if _, err := native.RecordAuditDisposition(ctx, securityaudit.DispositionCommand{Key: key, FindingID: "finding_example", Evidence: "Wrong source", Confirm: true}); err == nil {
			t.Fatal("unrecorded audit source accepted a disposition")
		}
	}
}

type postgresUpdateReceipts struct {
	ctx     context.Context
	client  *api.Client
	running runnerauth.BuildEvidence
}

func (s postgresUpdateReceipts) Load() (*runnerauth.UpdateReceipt, error) {
	return s.client.RunnerUpdateReceipt(s.ctx)
}

func (s postgresUpdateReceipts) Save(receipt *runnerauth.UpdateReceipt) error {
	return s.client.StoreRunnerUpdateReceipt(s.ctx, receipt, s.running)
}

func TestPostgresRunnerUpdateReceiptRestoresWithoutLocalState(t *testing.T) {
	f := newRunnerPGFixture(t)
	for index, binding := range f.bindings {
		now := time.Now().UTC().Truncate(time.Microsecond)
		running := runnerauth.BuildEvidence{Version: "1.0.0", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: runtime.GOOS, Architecture: runtime.GOARCH, VerifiedRelease: true, ObservedAt: now}
		request := runnerauth.UpdateRequest{ID: fmt.Sprintf("update_%d", index), Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64), RequestedAt: now}
		if index == 1 {
			request.Urgent = true
			request.ExpectedBuildRevision = ""
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			requestRunner := binding.RunnerID
			if request.Urgent {
				requestRunner = ""
			}
			if err := w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: "org_startup", RunnerID: requestRunner, Request: request}); err != nil {
				return err
			}
			if request.Urgent {
				return w.SetRunnerUrgentUpdate(t.Context(), 0, request.ID, now)
			}
			return w.SetRunnerUpdateDesired(t.Context(), binding.RunnerID, 1, request.ID)
		}); err != nil {
			t.Fatal(err)
		}
		identityDirectory := t.TempDir()
		if err := os.Chmod(identityDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		identityPath := filepath.Join(identityDirectory, "identity.json")
		file := runnerauth.File{Schema: 1, HubURL: f.server.URL, Credential: f.tokens[index], Identity: runnerauth.Identity{Binding: binding, OrganizationID: "org_startup", Scope: "projects", ProjectIDs: []tracker.ProjectID{"prj_runner"}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, ExpiresAt: now.Add(time.Hour)}}
		raw, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(identityPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		newClient := func() *api.Client {
			client, err := api.New(api.Config{URL: f.server.URL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: &http.Transport{}}})
			if err != nil {
				t.Fatal(err)
			}
			return client
		}
		client := newClient()
		receipt := &runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}
		if err := client.StoreRunnerUpdateReceipt(t.Context(), receipt, running); err != nil {
			t.Fatal(err)
		}
		restarted := newClient()
		if _, err := detentupdate.NewScheduler(detentupdate.SchedulerConfig{CheckInterval: time.Minute, RunningBuild: running, ReceiptStore: postgresUpdateReceipts{ctx: t.Context(), client: restarted, running: running}}); err != nil {
			t.Fatal(err)
		}
		loaded, err := restarted.RunnerUpdateReceipt(t.Context())
		if err != nil || loaded == nil || loaded.Status != "uncertain" || loaded.Request.ID != request.ID || loaded.FailureReason == "" {
			t.Fatalf("restart lost update recovery receipt: %+v %v", loaded, err)
		}
		path := "/api/v2/organizations/org_startup/runners/" + binding.RunnerID + "/update/receipt"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[1-index], nil), http.StatusUnauthorized)
	}
}

func TestPostgresRunnerUsageAndCheckpointSurviveClientRestart(t *testing.T) {
	f := newRunnerPGFixture(t)
	day := time.Now().UTC()
	for index, lease := range f.leases {
		native, ctx := f.client(t, index)
		data := f.data[index]
		data.Sequence = 2
		data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
		data.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: int64(17 + index), CachedInput: 2, Output: 3, BillingMode: "metered", Currency: "USD", To: day}}
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "usage_" + data.AttemptID}, Type: "run.observed", SchemaVersion: 1, Data: data}
		if err := native.AppendEvent(ctx, lease.WorkItemID, event); err != nil {
			t.Fatal(err)
		}
		if err := native.AppendEvent(ctx, lease.WorkItemID, event); err != nil {
			t.Fatalf("usage replay: %v", err)
		}
		data.Sequence = 3
		data.Handoff = &tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "git_ref", WorktreeState: "clean", HeadSHA: strings.Repeat("a", 40), Ref: tracker.CheckpointRefPrefix + string(lease.WorkItemID), CommitSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40), ExternalEffect: "none", EffectState: "none"}
		if err := native.AppendEvent(ctx, lease.WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "checkpoint_" + data.AttemptID}, Type: "run.checkpointed", SchemaVersion: 1, Data: data}); err != nil {
			t.Fatal(err)
		}
		restarted, restartedCtx := f.client(t, index)
		spend, err := restarted.IssueTokenSpend(restartedCtx, store.IssueIdentity{IssueID: string(lease.WorkItemID)})
		if err != nil || spend.InputTokens != int64(17+index) || spend.OutputTokens != 3 || spend.Sessions != 1 {
			t.Fatalf("usage replay changed spend: %+v %v", spend, err)
		}
		var attempt tracker.NativeAttempt
		if err := restarted.Transport().Request(restartedCtx, http.MethodGet, restarted.Scope()+"/work-items/"+string(lease.WorkItemID)+"/attempts/"+data.AttemptID, nil, &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.Checkpoint == nil || !attempt.Checkpoint.GitRef() || attempt.Checkpoint.CommitSHA != data.Handoff.CommitSHA || len(attempt.Usage) != 1 || attempt.Usage[0].Input != int64(17+index) {
			t.Fatalf("restart lost fenced checkpoint/usage: %+v", attempt)
		}
		name := "normal"
		if index == 1 {
			name = "fallback"
		}
		t.Run("model selection "+name, func(t *testing.T) {
			native, ctx := f.client(t, index)
			sessions := api.NewRunnerSessions(native)
			now := time.Now().UTC().Truncate(time.Microsecond)
			identity := agentidentity.Configured("example", "example", "default", "code", "example-model", "", "medium", "", now)
			identity.Selection = agentidentity.Selection{Policy: "example-policy", Reason: "default_complexity", RequestedModel: "example-model", ModelSource: "preset:normal_model", EffortSource: "preset:levels.normal.effort"}
			if index == 1 {
				identity.Selection.RequestedModel = "example-preferred-model"
				identity.Selection.FallbackReason = "configured model unavailable"
			}
			id, err := sessions.StartSession(ctx, store.SessionStart{ProjectID: "local-project", IssueID: string(lease.WorkItemID), StartedAt: now, RuntimeIdentity: identity})
			if err != nil {
				t.Fatal(err)
			}
			identity = identity.Merge(agentidentity.RuntimeUpdate("example-model", "example", "medium", "default", now))
			if err := sessions.FinishSession(ctx, id, store.SessionFinish{CompletedAt: now.Add(time.Second), FinalState: "completed", ProviderThreadID: "thread-" + name, RuntimeIdentity: identity}); err != nil {
				t.Fatal(err)
			}
			replacement, replacementCtx := f.client(t, index)
			resume, err := api.NewRunnerSessions(replacement).LatestIssueAgentResumeState(replacementCtx, store.IssueIdentity{ProjectID: "local-project", IssueID: string(lease.WorkItemID)})
			if err != nil || resume.DetentSessionID != id || !resume.RuntimeIdentity.MateriallyEqual(identity) {
				t.Fatalf("resume identity = %+v, %v, want %+v", resume, err, identity)
			}
			var snapshot runnersession.Snapshot
			if err := replacement.Transport().Request(replacementCtx, http.MethodGet, replacement.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10), nil, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ID != id || snapshot.Start.IssueID != string(lease.WorkItemID) || snapshot.Identity.Selection != identity.Selection {
				t.Fatalf("session provenance = %+v, want %+v", snapshot, identity.Selection)
			}
		})
	}
	native, ctx := f.client(t, 0)
	spend, err := native.ProjectDailyTokenSpend(ctx, "local-project", day)
	if err != nil || spend.InputTokens != 35 || spend.OutputTokens != 6 || spend.Sessions != 2 {
		t.Fatalf("daily typed spend: %+v %v", spend, err)
	}
	quantiles, err := native.RecentModelTokenQuantiles(ctx, store.ModelTokenQuantileQuery{Model: "example-model", Limit: 50})
	if err != nil || quantiles.Sessions != 2 || quantiles.P90InputTokens != 18 || quantiles.P90OutputTokens != 3 {
		t.Fatalf("typed quantiles: %+v %v", quantiles, err)
	}
	data := f.data[0]
	data.Sequence = 4
	data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
	data.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: 27, CachedInput: 2, Output: 3, BillingMode: "metered", Currency: "USD", To: day.AddDate(0, 0, 1)}}
	if err := native.AppendEvent(ctx, f.leases[0].WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "next_day_" + data.AttemptID}, Type: "run.observed", SchemaVersion: 1, Data: data}); err != nil {
		t.Fatal(err)
	}
	previous, err := native.ProjectDailyTokenSpend(ctx, "local-project", day)
	if err != nil || previous.InputTokens != 35 || previous.OutputTokens != 6 {
		t.Fatalf("new day moved previously accounted spend: %+v %v", previous, err)
	}
	next, err := native.ProjectDailyTokenSpend(ctx, "local-project", day.AddDate(0, 0, 1))
	if err != nil || next.InputTokens != 10 || next.OutputTokens != 0 {
		t.Fatalf("new day did not account only incremental spend: %+v %v", next, err)
	}

	for index, lease := range f.leases {
		t.Run(fmt.Sprintf("late_blocker_usage_%d", index), func(t *testing.T) {
			native, ctx := f.client(t, index)
			bindRepository := func(repository string) {
				t.Helper()
				if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(ctx, "prj_runner")
					if err != nil {
						return err
					}
					project.Settings.Repository = repository
					project.Settings.CheckoutRepository = ""
					_, err = w.UpdateProjectRecord(ctx, project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if index == 0 {
				bindRepository("example-org/example-project")
			} else {
				bindRepository("")
			}
			path := native.Scope() + "/work-items/" + string(lease.WorkItemID) + "/attempts/" + f.data[index].AttemptID
			var current tracker.NativeAttempt
			if err := native.Transport().Request(ctx, http.MethodGet, path, nil, &current); err != nil {
				t.Fatal(err)
			}
			data := current.NativeRunData
			data.Sequence++
			data.Outcome, data.Handoff = "succeeded", nil
			const reference = "example-org/example-project#999"
			data.CompletionBody = "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '" + reference + "'\n    reason: prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action: null\n```"
			data.Disposition = &tracker.NativeDisposition{Status: workpad.StatusBlocked, Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: reference, Owner: workpad.BlockerOwnerOrchestrator, Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Identifier: reference, States: []string{"open"}}}}}
			intervalEnd := time.Now().UTC()
			data.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-correction-model", UsageKind: "incremental", SourceID: "blocker-receipt", Revision: 1, Input: 1, BillingMode: "metered", Currency: "USD", From: current.StartedAt, To: intervalEnd, ReportedAt: intervalEnd}}
			finish := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "blocker_finish_" + data.AttemptID}, Type: "run.finished", SchemaVersion: 1, OccurredAt: intervalEnd, Data: data}
			if err := native.AppendEvent(ctx, lease.WorkItemID, finish); err != nil {
				t.Fatal(err)
			}
			wantReference := "prj_runner#999"
			if index == 1 {
				wantReference = reference
				bindRepository("example-org/example-project")
			}
			visibleHistory := func() int {
				t.Helper()
				count := 0
				if err := f.service.config.TenantRepository.ReadTenant(ctx, "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.Events(ctx, "prj_runner", lease.WorkItemID, 0, 20)
					if err != nil {
						return err
					}
					if len(rows) == 20 {
						t.Error("late usage fixture exceeded its complete history bound")
					}
					for _, row := range rows {
						if !row.HistoryHidden {
							count++
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return count
			}
			historyCount := visibleHistory()
			correction := finish
			correction.IdempotencyKey = "blocker_cost_" + data.AttemptID
			correction.Data.Sequence++
			correction.Data.Usage = append([]tracker.NativeUsage(nil), finish.Data.Usage...)
			if len(correction.Data.Usage) != 1 {
				t.Fatal("late blocker fixture lost its existing usage source")
			}
			paid := int64(1000 + index)
			correction.Data.Usage[0].Revision = 2
			correction.Data.Usage[0].ReportedCostMicros = &paid
			correction.Data.Usage[0].CostSource = "runner_report"
			if err := native.AppendEvent(ctx, lease.WorkItemID, correction); err != nil {
				t.Fatalf("late blocker usage correction: %v", err)
			}
			if err := native.Transport().Request(ctx, http.MethodGet, path, nil, &current); err != nil {
				t.Fatal(err)
			}
			if current.CompletionBody != data.CompletionBody || current.Disposition == nil || len(current.Disposition.BlockerEvidence) != 1 || current.Disposition.BlockerEvidence[0].Ref != wantReference || current.Disposition.BlockerEvidence[0].Predicate == nil || current.Disposition.BlockerEvidence[0].Predicate.Identifier != wantReference || finish.Data.Disposition.BlockerEvidence[0].Ref != reference {
				t.Fatal("late usage rewrote original report/input or changed established blocker representation")
			}
			if after := visibleHistory(); after != historyCount {
				t.Fatalf("late usage added visible terminal history: before=%d after=%d", historyCount, after)
			}
			if err := f.service.config.TenantRepository.ReadTenant(ctx, "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItem(ctx, "prj_runner", lease.WorkItemID)
				if err != nil {
					return err
				}
				page, err := r.Relationships(ctx, "prj_runner", lease.WorkItemID, corestore.RelationshipCursor{}, 10)
				if err == nil && (item.Revision != 1 || len(page.Items) != 0) {
					t.Errorf("late usage changed issue/dependency history: revision=%d edges=%d", item.Revision, len(page.Items))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

}

func TestPostgresRunnerRestartDoesNotReleaseLiveProvider(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX process groups")
	}
	f := newRunnerPGFixture(t)
	native, ctx := f.client(t, 0)
	sessions := api.NewRunnerSessions(native)
	id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(f.leases[0].WorkItemID), StartedAt: time.Now().UTC(), Model: "example-model"})
	if err != nil {
		t.Fatal(err)
	}
	restarted, restartedCtx := f.client(t, 0)
	stopped, err := restarted.ReleasedProvidersStopped(restartedCtx, f.data[0].AttemptID)
	if err != nil || stopped {
		t.Fatalf("unregistered provider state authorized cleanup: %t %v", stopped, err)
	}
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	procgroup.Configure(t.Context(), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.UpdateSessionWorkerProcess(ctx, id, store.WorkerProcessRegistration{WorkerProcessIdentity: store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt}}); err != nil {
		t.Fatal(err)
	}
	stopped, err = restarted.ReleasedProvidersStopped(restartedCtx, f.data[0].AttemptID)
	if err != nil || stopped {
		t.Fatalf("live Hub-recorded provider authorized cleanup: %t %v", stopped, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	stopped, err = restarted.ReleasedProvidersStopped(restartedCtx, f.data[0].AttemptID)
	if err != nil || !stopped {
		t.Fatalf("stopped Hub-recorded provider could not reconcile: %t %v", stopped, err)
	}
}

func TestPostgresRunnerProcessRegistrationRequiresSessionAuthority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX process groups")
	}
	f := newRunnerPGFixture(t)
	native, ctx := f.client(t, 0)
	trusted := api.NewRunnerSessions(native)
	attackerNative, attackerCtx := f.client(t, 0)
	attacker := api.NewRunnerSessions(attackerNative)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, victim := filepath.Join(base, "workspace"), filepath.Join(base, "victim")
	scratch, secret := filepath.Join(workspaceRoot, "scratch"), filepath.Join(victim, "secret")
	for _, dir := range []string{scratch, victim} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	identities := make([]store.WorkerProcessIdentity, 2)
	for index := range identities {
		cmd := exec.CommandContext(t.Context(), "sleep", "30")
		procgroup.Configure(t.Context(), cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		identity, err := procgroup.Inspect(cmd)
		if err != nil {
			t.Fatal(err)
		}
		identities[index] = store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt}
	}
	owned, unrelated := identities[0], identities[1]
	start := store.SessionStart{IssueID: string(f.leases[0].WorkItemID), StartedAt: time.Now().UTC(), Model: "example-model"}
	trustedID, err := trusted.StartSession(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := trusted.UpdateSessionWorkerProcess(ctx, trustedID, store.WorkerProcessRegistration{CleanupRoot: workspaceRoot, CleanupPath: scratch}); err != nil {
		t.Fatalf("trusted reservation refused: %v", err)
	}
	if err := trusted.UpdateSessionWorkerProcess(ctx, trustedID, store.WorkerProcessRegistration{WorkerProcessIdentity: owned, CleanupRoot: workspaceRoot, CleanupPath: scratch}); err != nil {
		t.Fatalf("trusted started registration refused: %v", err)
	}
	forged := store.WorkerProcessRegistration{WorkerProcessIdentity: unrelated, CleanupRoot: victim, CleanupPath: secret}
	for name, err := range map[string]error{
		"reserved overwrite": attacker.UpdateSessionWorkerProcess(attackerCtx, trustedID, store.WorkerProcessRegistration{CleanupRoot: victim, CleanupPath: secret}),
		"started overwrite":  attacker.UpdateSessionWorkerProcess(attackerCtx, trustedID, forged),
		"reap":               attacker.MarkSessionWorkerProcessReaped(attackerCtx, trustedID, store.WorkerProcessReap{ReapedAt: time.Now().UTC(), Outcome: "exited", Reason: "forged"}),
	} {
		if err == nil {
			t.Fatalf("Hub accepted %s without the session process authority", name)
		}
	}
	foreignID, err := attacker.StartSession(attackerCtx, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := attacker.UpdateSessionWorkerProcess(attackerCtx, foreignID, forged); err != nil {
		t.Fatal(err)
	}
	restartedNative, restartedCtx := f.client(t, 0)
	restarted := api.NewRunnerSessions(restartedNative)
	for name, sessions := range map[string]*api.RunnerSessions{"trusted": trusted, "restarted": restarted} {
		listCtx := ctx
		if name == "restarted" {
			listCtx = restartedCtx
		}
		workers, err := sessions.ListActiveWorkerProcesses(listCtx)
		if err != nil {
			t.Fatal(err)
		}
		var terminated []store.WorkerProcessIdentity
		for _, worker := range workers {
			terminated = append(terminated, worker.WorkerProcessIdentity)
			if err := workspace.CleanupOwnedPath(worker.CleanupRoot, worker.CleanupPath); err != nil {
				t.Fatal(err)
			}
		}
		if slices.Contains(terminated, unrelated) {
			t.Fatalf("%s client would terminate a forged process identity: %+v", name, workers)
		}
		if name == "trusted" && (len(workers) != 1 || workers[0].SessionID != trustedID || workers[0].WorkerProcessIdentity != owned || workers[0].CleanupRoot != workspaceRoot || workers[0].CleanupPath != scratch) {
			t.Fatalf("trusted client lost its own registration: %+v", workers)
		}
		if name == "restarted" && len(workers) != 0 {
			t.Fatalf("restarted client trusted Hub-only registrations: %+v", workers)
		}
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("trusted cleanup did not remove its scratch: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("Hub-supplied cleanup path deleted a file outside the local registration: %v", err)
	}
	if err := trusted.MarkSessionWorkerProcessReaped(ctx, trustedID, store.WorkerProcessReap{ReapedAt: time.Now().UTC(), Outcome: "exited", Reason: "test"}); err != nil {
		t.Fatalf("trusted reap refused: %v", err)
	}
}

func TestPostgresRunnerSessionRestartRetainsFencedEvidence(t *testing.T) {
	f := newRunnerPGFixture(t)
	otherConfig := startupConfig(t, f.admin)
	otherConfig.Hosted.OrganizationID = "org_other"
	otherConfig.Hosted.WorkOSOrganizationID = "provider_org_other"
	otherService := openStartupHub(t, f.admin, otherConfig)
	otherTenant := seedRunnerPGFixture(t, f.admin, otherService, "org_other")
	progressSessionIDs := make(map[tracker.OrganizationID]int64)
	for _, pair := range [][2]runnerPGFixture{{f, otherTenant}, {otherTenant, f}} {
		for index := range pair[0].leases {
			testRunnerSessionFinalizationRecovery(t, pair[0], pair[1], index)
		}
		native, ctx := pair[0].client(t, 0)
		id, err := api.NewRunnerSessions(native).StartSession(ctx, store.SessionStart{IssueID: string(pair[0].leases[0].WorkItemID), StartedAt: time.Now().UTC().Truncate(time.Microsecond)})
		if err != nil {
			t.Fatal(err)
		}
		progressSessionIDs[pair[0].organization] = id
	}

	for _, pair := range [][2]runnerPGFixture{{f, otherTenant}, {otherTenant, f}} {
		f := pair[0]
		t.Run(string(f.organization), func(t *testing.T) {
			for index := range f.leases {
				native, ctx := f.client(t, index)
				sessions := api.NewRunnerSessions(native)
				started := time.Now().UTC().Truncate(time.Microsecond)
				start := store.SessionStart{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), StartedAt: started, RequestedModel: "example-model", Model: "example-model", AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code"}
				committed := make(chan runnersession.Snapshot, 1)
				lostResponse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, r)
					var snapshot runnersession.Snapshot
					if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.ID < 1 {
						t.Error("session start did not commit before response loss")
					}
					committed <- snapshot
					w.Header().Set("Content-Length", strconv.Itoa(response.Body.Len()+1))
					w.WriteHeader(response.Code)
					if _, err := w.Write(response.Body.Bytes()); err != nil {
						t.Error(err)
					}
				}))
				t.Cleanup(lostResponse.Close)
				transport, err := api.New(api.Config{URL: lostResponse.URL, TokenSource: func() string { return f.tokens[index] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
				if err != nil {
					t.Fatal(err)
				}
				uncertainNative, err := transport.Native(f.organization, "prj_runner")
				if err != nil {
					t.Fatal(err)
				}
				uncertainID, err := api.NewRunnerSessions(uncertainNative).StartSession(ctx, start)
				if uncertainID != 0 || !errors.Is(err, api.ErrUnavailable) {
					t.Fatalf("lost start response: id=%d error=%v", uncertainID, err)
				}
				orphan := <-committed
				lostResponse.Close()
				var persistedStart runnersession.Snapshot
				if err := native.Transport().Request(ctx, http.MethodGet, native.Scope()+"/runner-sessions/"+strconv.FormatInt(orphan.ID, 10), nil, &persistedStart); err != nil {
					t.Fatal(err)
				}
				if persistedStart.Revision != 1 || persistedStart.Start.IssueID != start.IssueID || persistedStart.Provider.ThreadID != "" || persistedStart.Provider.SessionID != "" || persistedStart.Worker != nil || persistedStart.Progress != nil || persistedStart.Finish != nil || orphan.ReceiptID < 1 {
					t.Fatalf("lost response changed committed pre-provider session: %+v", persistedStart)
				}
				if stopped, err := native.ReleasedProvidersStopped(ctx, f.data[index].AttemptID); err != nil || stopped {
					t.Fatalf("pre-provider session process observation: stopped=%t error=%v", stopped, err)
				}
				if _, err := sessions.LatestIssueAgentResumeState(ctx, store.IssueIdentity{IssueID: start.IssueID}); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("pre-provider session selected for current-attempt resume: %v", err)
				}
				id, err := sessions.StartSession(ctx, start)
				if id == orphan.ID {
					t.Fatal("new start reused an uncertain request's session")
				}
				if err != nil || id < 1 {
					t.Fatalf("start: id=%d error=%v", id, err)
				}
				provider := store.SessionProviderIdentity{ThreadID: fmt.Sprintf("thread_%d", index), SessionID: fmt.Sprintf("provider_%d", index)}
				identity := agentidentity.Identity{BackendID: "example", BackendKind: "example", Role: "code", ResolvedModel: agentidentity.NewValue("example-model", agentidentity.ProvenanceConfigured)}
				if err := sessions.UpdateSessionIdentity(ctx, id, identity); err != nil {
					t.Fatal(err)
				}
				if err := sessions.UpdateSessionProviderIdentity(ctx, id, provider); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.SessionProgress(ctx, id); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("missing progress error = %v", err)
				}
				var progress store.SessionProgress
				for _, head := range []string{"first", "second"} {
					progress = store.SessionProgress{HeadSHA: head, CommitFingerprint: "patch", TrackedFingerprint: "tracked-" + head, WorkspaceFingerprint: "workspace-" + head, WorkpadFingerprint: "workpad-" + head, Local: true, LastProgressAt: started}
					if err := sessions.SaveSessionProgress(ctx, id, progress); err != nil {
						t.Fatal(err)
					}
					got, err := sessions.SessionProgress(ctx, id)
					if err != nil || got.HeadSHA != progress.HeadSHA || got.CommitFingerprint != progress.CommitFingerprint || got.TrackedFingerprint != progress.TrackedFingerprint || got.WorkspaceFingerprint != progress.WorkspaceFingerprint || got.WorkpadFingerprint != progress.WorkpadFingerprint || got.Local != progress.Local || !got.LastProgressAt.Equal(progress.LastProgressAt) {
						t.Fatalf("progress = %+v, %v, want %+v", got, err, progress)
					}
				}
				var beforeRejected runnersession.Snapshot
				if err := native.Transport().Request(ctx, http.MethodGet, native.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10), nil, &beforeRejected); err != nil {
					t.Fatal(err)
				}
				foreign, foreignCtx := pair[1].client(t, 0)
				var beforeForeign runnersession.Snapshot
				if err := foreign.Transport().Request(foreignCtx, http.MethodGet, foreign.Scope()+"/runner-sessions/"+strconv.FormatInt(progressSessionIDs[pair[1].organization], 10), nil, &beforeForeign); err != nil {
					t.Fatal(err)
				}
				if beforeForeign.Progress != nil {
					t.Fatal("foreign fixture already has progress")
				}
				for _, test := range []struct {
					name     string
					id       int64
					progress store.SessionProgress
					status   int
					code     string
				}{
					{name: "missing progress timestamp", id: id, status: http.StatusUnprocessableEntity, code: "invalid_request"},
					{name: "unknown progress session", id: int64(1<<63 - 1), progress: progress, status: http.StatusNotFound, code: "not_found"},
					{name: "foreign tenant progress session", id: progressSessionIDs[pair[1].organization], progress: progress, status: http.StatusNotFound, code: "not_found"},
					{name: "zero progress session", progress: progress, status: http.StatusNotFound, code: "not_found"},
				} {
					t.Run(fmt.Sprintf("runner_%d/%s", index, test.name), func(t *testing.T) {
						err := sessions.SaveSessionProgress(ctx, test.id, test.progress)
						var apiErr *api.APIError
						if !errors.As(err, &apiErr) || apiErr.Status != test.status || apiErr.Code != test.code {
							t.Fatalf("progress mutation error = %v, want %d %s", err, test.status, test.code)
						}
						if test.id == progressSessionIDs[pair[1].organization] {
							var afterForeign runnersession.Snapshot
							if err := foreign.Transport().Request(foreignCtx, http.MethodGet, foreign.Scope()+"/runner-sessions/"+strconv.FormatInt(test.id, 10), nil, &afterForeign); err != nil {
								t.Fatal(err)
							}
							if afterForeign.Revision != beforeForeign.Revision || afterForeign.Progress != nil {
								t.Fatalf("foreign session changed after rejection: %+v", afterForeign)
							}
						}
						var afterRejected runnersession.Snapshot
						if err := native.Transport().Request(ctx, http.MethodGet, native.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10), nil, &afterRejected); err != nil {
							t.Fatal(err)
						}
						if afterRejected.Revision != beforeRejected.Revision || afterRejected.Progress == nil {
							t.Fatalf("rejected progress mutation changed the session: %+v", afterRejected)
						}
						got := *afterRejected.Progress
						if got.HeadSHA != progress.HeadSHA || got.CommitFingerprint != progress.CommitFingerprint || got.TrackedFingerprint != progress.TrackedFingerprint || got.WorkspaceFingerprint != progress.WorkspaceFingerprint || got.WorkpadFingerprint != progress.WorkpadFingerprint || got.Local != progress.Local || !got.LastProgressAt.Equal(progress.LastProgressAt) {
							t.Fatalf("rejected progress mutation changed progress: %+v, want %+v", got, progress)
						}
					})
				}
				if err := sessions.UpdateSessionResumeState(ctx, id, store.SessionResumeState{ResumedFromSessionID: id, OrphanRecoveryOutcome: "resumed"}); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{SessionID: id, PhaseName: "agent_active", StartedAt: started, FinishedAt: started.Add(time.Second)}); err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.SaveWorkflowActivityProfile(ctx, 0, store.WorkflowPhaseEvent{SessionID: id, PhaseName: "profile"}, workflowmetrics.ActivityProfile{Schema: 1, SessionID: id, Status: "running"}); err != nil {
					t.Fatal(err)
				}
				usageReceipt, err := sessions.RecordUsageEvent(ctx, store.UsageEvent{SessionID: id, Model: "example-model", InputTokens: 17, OutputTokens: 3})
				if err != nil {
					t.Fatal(err)
				}
				restartedNative, restartedCtx := f.client(t, index)
				restarted := api.NewRunnerSessions(restartedNative)
				resume, err := restarted.LatestAgentResumeState(restartedCtx, store.AgentResumeLookup{NativeAttemptID: f.data[index].AttemptID, IssueID: string(f.leases[index].WorkItemID), AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code", RequestedModel: "example-model"})
				if err != nil || resume.DetentSessionID != id || resume.ProviderThreadID != provider.ThreadID || resume.ProviderSessionID != provider.SessionID || !resume.Orphaned || resume.RuntimeIdentity.ResolvedModel.Value != "example-model" {
					t.Fatalf("restart lost provider identity: %+v %v", resume, err)
				}
				replacement, replacementCtx := f.client(t, 1-index)
				recovered, err := api.NewRunnerSessions(replacement).LatestAgentResumeState(replacementCtx, store.AgentResumeLookup{NativeAttemptID: f.data[index].AttemptID, IssueID: string(f.leases[index].WorkItemID), AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code", RequestedModel: "example-model"})
				if err != nil || recovered.DetentSessionID != id || recovered.ProviderThreadID != provider.ThreadID {
					t.Fatalf("replacement runner lost Hub resume identity: %+v %v", recovered, err)
				}
				persisted, err := restarted.SessionProgress(restartedCtx, id)
				if err != nil || persisted.HeadSHA != progress.HeadSHA || persisted.CommitFingerprint != progress.CommitFingerprint || persisted.TrackedFingerprint != progress.TrackedFingerprint || persisted.WorkspaceFingerprint != progress.WorkspaceFingerprint || persisted.WorkpadFingerprint != progress.WorkpadFingerprint || persisted.Local != progress.Local || !persisted.LastProgressAt.Equal(progress.LastProgressAt) {
					t.Fatalf("restart lost progress: %+v %v", persisted, err)
				}
				pinned, err := restarted.SessionPolicy(restartedCtx, "local-project", id)
				if err != nil || pinned.ID != f.policy.PolicyID {
					t.Fatalf("restart lost policy: %+v %v", pinned, err)
				}
				t.Run(fmt.Sprintf("runner_%d/unknown project policy", index), func(t *testing.T) {
					unknownProject, err := native.Transport().Native(f.organization, "prj_other")
					if err != nil {
						t.Fatal(err)
					}
					_, err = api.NewRunnerSessions(unknownProject).SessionPolicy(restartedCtx, "local-project", id)
					var denial *api.APIError
					if !errors.As(err, &denial) || denial.Status != http.StatusForbidden || denial.Code != "access_denied" {
						t.Fatalf("unknown project policy read = %v, want 403 access_denied", err)
					}
				})
				if err := restarted.FinishSession(restartedCtx, id, store.SessionFinish{CompletedAt: started.Add(time.Second), FinalState: "completed", ProviderThreadID: provider.ThreadID, ProviderSessionID: provider.SessionID}); err != nil {
					t.Fatal(err)
				}
				for _, test := range []struct {
					name   string
					parent int64
					turns  int64
				}{
					{name: "fresh startup"},
					{name: "resumed failure", parent: id, turns: 1},
				} {
					t.Run(fmt.Sprintf("runner_%d/%s", index, test.name), func(t *testing.T) {
						nextStart := start
						nextStart.ResumedFromSessionID = test.parent
						if test.parent > 0 {
							nextStart.ProviderThreadID, nextStart.ProviderSessionID = provider.ThreadID, provider.SessionID
							nextStart.OrphanRecoveryOutcome = store.OrphanRecoveryResumed
						}
						nextID, err := restarted.StartSession(restartedCtx, nextStart)
						if err != nil {
							t.Fatal(err)
						}
						finish := store.SessionFinish{CompletedAt: started.Add(2 * time.Second), FinalState: "failed", Turns: test.turns, ResumedFromSessionID: test.parent}
						if err := restarted.FinishSession(restartedCtx, nextID, finish); err != nil {
							t.Fatal(err)
						}
						reader, readCtx := f.client(t, index)
						var snapshot runnersession.Snapshot
						if err := reader.Transport().Request(readCtx, http.MethodGet, reader.Scope()+"/runner-sessions/"+strconv.FormatInt(nextID, 10), nil, &snapshot); err != nil {
							t.Fatal(err)
						}
						if snapshot.Start.ResumedFromSessionID != test.parent || snapshot.Resume.ResumedFromSessionID != test.parent || snapshot.Finish == nil || snapshot.Finish.Turns != test.turns || snapshot.Finish.FinalState != "failed" {
							t.Fatalf("Hub restart lost session provenance or actual turns: %+v", snapshot)
						}
						if snapshot.Provider.ThreadID != nextStart.ProviderThreadID || snapshot.Provider.SessionID != nextStart.ProviderSessionID {
							t.Fatalf("Hub restart mixed fresh and resumed provider identities: %+v", snapshot.Provider)
						}
					})
				}
				var history []runnersession.History
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					history, err = r.(runnersession.Reader).RunnerSessionHistory(t.Context(), "prj_runner", id, 0, 20)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if len(history) != 10 {
					t.Fatalf("session history lost writes: %d", len(history))
				}
				if usageReceipt <= 0 || history[8].ID != usageReceipt {
					t.Fatalf("usage receipt does not identify durable history: receipt=%d history=%+v", usageReceipt, history[7])
				}
				var archived runnersession.Mutation
				if err := restartedNative.Transport().Request(restartedCtx, http.MethodGet, restartedNative.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10)+"/history/8", nil, &archived); err != nil {
					t.Fatal(err)
				}
				if archived.Profile == nil || archived.Profile.Profile.SessionID != id {
					t.Fatalf("activity profile was not retained in Hub history: %+v", archived)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(f.leases[index].ID), FencingToken: int64(f.leases[index].FencingToken)}, "released")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := restarted.SaveSessionProgress(restartedCtx, id, progress); err == nil {
					t.Fatal("revoked lease accepted a session mutation")
				}
				other, _ := f.client(t, 1-index)
				if err := api.NewRunnerSessions(other).UpdateSessionProviderIdentity(restartedCtx, id, provider); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
					t.Fatalf("other runner accepted identity mutation: %v", err)
				}
			}
		})
	}
}
