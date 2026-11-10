package scenariotest

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func RunNativeReworkRecoversReviewedSourceAcrossMachines(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	newNativeChangeHubWithStates := func(t *testing.T, review string, states []tracker.NativeState) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, review, states, false, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Conversation: &hubserver.ConversationConfig{Enabled: true}}, factories)
	}
	t.Setenv("TMPDIR", t.TempDir())
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name             string
		updatePolicy     bool
		republish        bool
		transferDirty    bool
		transfer         bool
		transferCode     bool
		workerOwnership  bool
		planning         bool
		absorbedConflict bool
		restartIntent    string
		landing          bool
	}{
		{name: "same policy"},
		{name: "restart unfinished Rework", restartIntent: "unfinished"},
		{name: "restart stale completion Rework", restartIntent: "stale", updatePolicy: true},
		{name: "restart resolved conflict Rework", restartIntent: "resolved", absorbedConflict: true, updatePolicy: true},
		{name: "restart unfinished pending landing", restartIntent: "unfinished", absorbedConflict: true, landing: true},
		{name: "restart stale completion pending landing", restartIntent: "stale", absorbedConflict: true, landing: true},
		{name: "restart resolved conflict pending landing", restartIntent: "resolved", absorbedConflict: true, landing: true},
		{name: "conflict resolution absorbed by target advances the Change head", absorbedConflict: true, updatePolicy: true},
		{name: "planning enabled Rework restores missing published head before provider", planning: true},
		{name: "retained source waits for writer cleanup before replacement", workerOwnership: true},
		{name: "updated policy", updatePolicy: true},
		{name: "operator republished same tree", republish: true},
		{name: "operator republished same tree with policy update", republish: true, updatePolicy: true},
		{name: "version-bound transfer restores selected source", republish: true, transfer: true},
		{name: "same-runner transfer restores source and preserves dirty custody", republish: true, transferDirty: true, transfer: true},
		{name: "code-lane transfer restores source and preserves dirty custody", republish: true, transferDirty: true, transfer: true, transferCode: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			updatePolicy := test.updatePolicy
			states := []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
				{Name: "In Review", Transitions: []string{"Rework", "In Progress"}},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
				{Name: "Done", Terminal: true},
			}
			if test.restartIntent != "" {
				states[1].Transitions = append(states[1].Transitions, "Rework", "Merging")
				states[2].Transitions = append(states[2].Transitions, "Merging")
				states[3].Transitions = append(states[3].Transitions, "Merging")
				states = append(states, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"Rework", "Done"}})
			}
			h := newNativeChangeHubWithStates(t, "In Review", states)
			previousPolicy := h.descriptor.ID
			h.descriptor.Gates.Validator = true
			h.descriptor = h.descriptor.WithID()
			if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previousPolicy, Policy: h.descriptor}); err != nil {
				t.Fatal(err)
			}
			issue := h.createInProgress(t, "Recover the reviewed code on another Mac")
			item := tracker.NativeWorkItemID(issue.ID)
			sourceA := nativeChangeSourceRepo(t)
			nativeChangeGit(t, sourceA, "branch", "-m", "dev")
			remote := filepath.Join(t.TempDir(), "origin.git")
			nativeChangeGit(t, sourceA, "init", "--bare", "-b", "dev", remote)
			nativeChangeGit(t, sourceA, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, sourceA, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, sourceA, "push", "-u", "origin", "dev")
			head := func(directory string) string {
				data, err := exec.CommandContext(t.Context(), "git", "-C", directory, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(data))
			}
			headTree := func(directory, commit string) string {
				data, err := exec.CommandContext(t.Context(), "git", "-C", directory, "rev-parse", commit+"^{tree}").Output()
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(data))
			}
			initialBase := head(sourceA)
			expectPendingLanding := false
			run := func(source string, provider *committingAgent, candidate connector.Issue, execution runner.Execution, targets ...string) runner.RunResult {
				root := filepath.Join(t.TempDir(), "workspaces")
				backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: root, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				transferredCode := test.transferCode && execution.Recovery().SourceVersionID != ""
				if (candidate.State == "Rework" || transferredCode) && !updatePolicy {
					info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: candidate.ID, Identifier: candidate.Identifier})
					if err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, info.Path, "reset", "--hard", initialBase)
					if test.transferDirty {
						if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("local staged source\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						nativeChangeGit(t, info.Path, "add", "README.md")
						if err := os.WriteFile(filepath.Join(info.Path, "PENDING.md"), []byte("local untracked source\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Store: api.NewRunnerSessions(h.native), ProjectID: "local", Workflow: config.Workflow{Config: config.Config{Plan: gate.PlanConfig{Enabled: test.planning}, Gate: gate.Config{Run: "test -f CHANGE.md", Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}, Workspace: backend, AgentBackend: provider})
				if err != nil {
					t.Fatal(err)
				}
				request := runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement}
				var result runner.RunResult
				if len(targets) == 0 {
					result, err = agent.Run(t.Context(), request)
				} else {
					result, _, err = runNativeCompletionBackend(t, h, &nativeCompletionRunner{Runner: agent, completed: make(chan nativeCompletionRun, 1), gate: gate.Config{Run: "test -f CHANGE.md", Validator: gate.ValidatorConfig{Enabled: true}}, expectPendingLanding: expectPendingLanding}, request, targets[0])
				}
				if test.transferDirty && (candidate.State == "Rework" || transferredCode) {
					paths, err := filepath.Glob(filepath.Join(root, ".detent", "quarantine", "*"))
					if err != nil || len(paths) != 1 || head(paths[0]) != initialBase {
						t.Fatalf("transfer did not preserve old source: paths=%v error=%v", paths, err)
					}
					for name, want := range map[string]string{"README.md": "local staged source\n", "PENDING.md": "local untracked source\n"} {
						data, err := os.ReadFile(filepath.Join(paths[0], name))
						if err != nil || string(data) != want {
							t.Fatalf("refusal lost %s: %q, %v", name, data, err)
						}
					}
					data, err := exec.CommandContext(t.Context(), "git", "-C", paths[0], "show", ":README.md").Output()
					if err != nil || string(data) != "local staged source\n" {
						t.Fatalf("refusal lost index custody: %q, %v", data, err)
					}
				}
				if err != nil || len(targets) == 0 && (result.NativeChange == nil || !result.NativeChange.Reviewed) {
					t.Fatalf("runner did not publish newly validated source: %+v, %v", result.NativeChange, err)
				}
				return result
			}
			candidateA := h.claim(t, issue.ID)
			owner := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
			writerStopped := false
			writerActive := errors.New("fixture writer remains active")
			ownerAttempt := owner.CompletionData().AttemptID
			if test.workerOwnership {
				if err := owner.KeepWorker(func(context.Context) error {
					acknowledged, readErr := h.admin.NativeAttempt(t.Context(), item, ownerAttempt)
					if readErr != nil || acknowledged.Outcome != "succeeded" {
						t.Error("worker cleanup preceded acknowledged completion")
					}
					if !writerStopped {
						return writerActive
					}
					return nil
				}, nil); err != nil {
					t.Fatal(err)
				}
			}
			providerA := &committingAgent{staged: true, validator: "pass", complete: true}
			first := run(sourceA, providerA, candidateA, h.scheduler.RunExecution(issue.ID))
			oldHead := first.NativeChange.HeadSHA
			if _, err := exec.CommandContext(t.Context(), "git", "-C", remote, "merge-base", "--is-ancestor", oldHead, "refs/detent/checkpoints/"+issue.ID).Output(); err != nil {
				t.Fatalf("reviewed commit is not reachable from the forge checkpoint ref: %v", err)
			}
			old, err := h.admin.Change(t.Context(), item, first.NativeChange.ChangeID)
			if err != nil || old.Versions[0].Source == nil || len(old.Reviews) != 1 {
				t.Fatalf("reviewed source was not retained: %+v, %v", old, err)
			}
			if test.restartIntent != "" && (old.Change.CurrentLanding() != nil || old.Versions[0].LandingQuality != nil) {
				t.Fatal("unlanded reviewed source acquired landing quality")
			}
			if test.workerOwnership {
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err == nil {
					t.Fatal("claim released while its writer remained active")
				}
				_, err := h.native.Claim(t.Context(), tracker.NativeClaim{PolicyID: h.descriptor.ID, WorkItemID: item, MachineID: owner.claim.lease.MachineID, SessionID: "competing-writer", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				var conflict *APIError
				if !errors.Is(err, api.ErrNoClaimableWork) && (!errors.As(err, &conflict) || conflict.Status != http.StatusConflict || conflict.Code != "lease_conflict") {
					t.Fatalf("competing writer was not fenced: %v", err)
				}
				workerError := owner.CloseWorkers(t.Context())
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
				if err != nil || evidence.CurrentLease == nil || evidence.CurrentLease.ID != owner.claim.lease.ID || !errors.Is(workerError, writerActive) {
					t.Fatalf("cleanup refusal lost the current execution owner: %+v, %v", evidence.CurrentLease, err)
				}
				var source tracker.NativeSourceRecovery
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/source-recovery", nil, &source); err != nil || source.Quiesced || source.VersionID != first.NativeChange.VersionID || source.HeadSHA != oldHead {
					t.Fatalf("active writer lost retained source or became transferable: %+v, %v", source, err)
				}
				writerStopped = true
			}
			h.complete(t, issue.ID, first.NativeChange)
			if test.workerOwnership {
				var source tracker.NativeSourceRecovery
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/source-recovery", nil, &source); err != nil || !source.Quiesced || !source.Available || source.VersionID != first.NativeChange.VersionID || source.HeadSHA != oldHead {
					t.Fatalf("completed writer did not release exact recoverable source: %+v, %v", source, err)
				}
			}
			versionCount := 2
			if test.republish {
				nativeChangeGit(t, providerA.workspace, "commit", "--amend", "-m", "operator recovers the same tree")
				currentHead := head(providerA.workspace)
				originalTree := headTree(providerA.workspace, oldHead)
				currentTree := headTree(providerA.workspace, currentHead)
				if currentHead == oldHead || originalTree != currentTree {
					t.Fatal("operator republication did not preserve the same tree under a new head")
				}
				capture, err := workspace.CaptureChangeSource(t.Context(), providerA.workspace, old.Versions[0].BaseSHA, currentHead)
				if err != nil {
					t.Fatal(err)
				}
				input := old.Versions[0].ChangeVersionInput
				input.HeadSHA, input.RunID, input.AttemptID, input.Validation = currentHead, "", "", nil
				input.Code.URI, input.Code.SHA256 = nativeChangeRepository+"/commit/"+currentHead, policy.Digest([]byte(currentHead))
				input.Source = &capture.Source
				if _, err := h.admin.PublishChangeVersion(t.Context(), item, first.NativeChange.ChangeID, tracker.PublishChangeVersion{Mutation: nativeMutationKey(), ExpectedVersionID: first.NativeChange.VersionID, ChangeVersionInput: input, SourceBundle: capture.Bundle}); err != nil {
					t.Fatal(err)
				}
				versionCount++
			}
			if updatePolicy {
				h.repolicy(t)
			}
			if err := os.WriteFile(filepath.Join(sourceA, "UPSTREAM.md"), []byte("new dev base\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, sourceA, "add", "UPSTREAM.md")
			if test.absorbedConflict {
				if err := os.WriteFile(filepath.Join(sourceA, "CHANGE.md"), []byte("integrated\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, sourceA, "add", "CHANGE.md")
			}
			nativeChangeGit(t, sourceA, "commit", "-m", "advance dev")
			nativeChangeGit(t, sourceA, "push", "origin", "dev")
			newBase := head(sourceA)
			sourceB := filepath.Join(t.TempDir(), "air")
			nativeChangeGit(t, sourceA, "clone", "--no-local", remote, sourceB)
			nativeChangeGit(t, sourceB, "remote", "set-url", "origin", nativeChangeRepository)
			nativeChangeGit(t, sourceB, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, sourceB, "config", "user.name", "Air")
			nativeChangeGit(t, sourceB, "config", "user.email", "air@example.com")
			if _, err := exec.CommandContext(t.Context(), "git", "-C", sourceB, "cat-file", "-e", oldHead+"^{commit}").Output(); err == nil {
				t.Fatal("second Mac already had the unpushed commit")
			}
			current, err := h.admin.Issue(t.Context(), item)
			if err != nil {
				t.Fatal(err)
			}
			state := "Rework"
			if test.transferCode {
				state = "In Progress"
			}
			if test.landing {
				state = "Merging"
			}
			if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: state, Reason: "user_requested"}); err != nil {
				t.Fatal(err)
			}
			if test.landing {
				candidates := h.candidatesIn(t, "Merging")
				if len(candidates) != 1 || candidates[0].ID != issue.ID {
					t.Fatalf("pending landing candidate=%+v", candidates)
				}
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], h.clock()); err != nil {
					t.Fatal(err)
				}
				provider := &committingAgent{validator: "pass", complete: true}
				expectPendingLanding = true
				run(sourceA, provider, candidates[0], h.scheduler.RunExecution(issue.ID), "Merging")
				expectPendingLanding = false
				if provider.writableCalls != 0 || head(remote) != newBase {
					t.Fatalf("initial conflict dispatched provider or changed target: calls=%d target=%s", provider.writableCalls, head(remote))
				}
			}
			if test.restartIntent != "" {
				path, err := api.NativeAttemptDiffPath(ownerAttempt)
				if err != nil {
					t.Fatal(err)
				}
				var stored tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+path, nil, &stored); err != nil {
					t.Fatal(err)
				}
				if stored.AttemptID != ownerAttempt || stored.HeadSHA != oldHead || stored.Producer.LeaseID != owner.claim.lease.ID || stored.Producer.FencingToken != owner.claim.lease.FencingToken || stored.Generation.Source != tracker.DiffSourceAttempt || stored.Generation.Seq <= 0 {
					t.Fatalf("stored diff lost source/producer/generation identity: %+v", stored)
				}
				h.restart()
				admin, err := createTestClient(t, fixtureClient(h.admin.Transport()).config, factories)
				if err != nil {
					t.Fatal(err)
				}
				h.admin, err = admin.Native(h.organization, h.project)
				if err != nil {
					t.Fatal(err)
				}
				var restored tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+path, nil, &restored); err != nil || !reflect.DeepEqual(restored, stored) {
					t.Fatalf("restart lost stored diff/producer/generation: before=%+v after=%+v error=%v", stored, restored, err)
				}
				attempt, err := h.admin.NativeAttempt(t.Context(), item, ownerAttempt)
				if err != nil || attempt.Checkpoint == nil || attempt.Checkpoint.HeadSHA != oldHead || attempt.LeaseID != owner.claim.lease.ID || attempt.FencingToken != owner.claim.lease.FencingToken {
					t.Fatalf("restart lost source checkpoint authority: %+v, %v", attempt, err)
				}
				if test.landing && test.restartIntent == "resolved" {
					if err := os.WriteFile(filepath.Join(sourceA, "CHANGE.md"), []byte("changed\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, sourceA, "add", "CHANGE.md")
					nativeChangeGit(t, sourceA, "commit", "-m", "incorporate reviewed intent")
					nativeChangeGit(t, sourceA, "push", "origin", "dev")
					newBase = head(sourceA)
				}
			}
			h.native = h.otherWorker(t)
			h.connector.worker, err = NewNativeConnector(h.native)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := h.native.Transport().RunnerIdentity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			machine := fixtureSchedulerMachine(h.scheduler)
			machine.ID = identity.MachineID
			h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{state}})
			if err != nil || len(candidates) != 1 {
				t.Fatalf("second Mac did not claim Rework: %+v, %v", candidates, err)
			}
			if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
				t.Fatal(err)
			}
			recovered, err := h.scheduler.RunExecution(issue.ID).(runner.ChangeSourceExecution).RecoverChangeSource(t.Context())
			if err != nil || slices.Contains(recovered.PreviousHeads, oldHead) != test.republish {
				t.Fatalf("superseded source provenance=%v: %v", recovered.PreviousHeads, err)
			}
			if test.transfer {
				execution := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
				execution.claim.recovery.SourceVersionID = recovered.Version.ID
				execution.claim.recovery.SourceAttempt().MachineID = execution.claim.lease.MachineID
				execution = replaceSourceRecovery(t, h, execution, execution.claim.recovery, factories)
				recovered, err = execution.RecoverChangeSource(t.Context())
				if err != nil || !recovered.Transferred || recovered.Version.ID != execution.claim.recovery.SourceVersionID {
					t.Fatalf("transfer lost exact source selection: %+v, %v", recovered.Version, err)
				}
			}
			providerB := &committingAgent{validator: "pass", complete: true}
			resumeVerified, resumedTurns := 0, 0
			if test.restartIntent == "unfinished" {
				providerB.complete, providerB.inProgress = false, true
			}
			if test.restartIntent != "" && test.restartIntent != "resolved" && !test.landing {
				providerB.validator = "fail"
			}
			if test.restartIntent == "stale" && !test.landing {
				originalWorkspace := ""
				providerB.verifyResume = func(process runner.AgentProcessRequest, resume runner.AgentResume) error {
					if resume.ThreadID != "thread-native" || resume.SessionID != "" || originalWorkspace == "" || process.Workspace != originalWorkspace {
						return errors.New("stale completion repair lost its implementation session or workspace")
					}
					resumeVerified++
					return nil
				}
				providerB.duringValidation = func(runner.AgentTurnRequest) {
					providerB.complete, providerB.inProgress = false, true
				}
				providerB.duringLiveTurn = func(request runner.AgentTurnRequest, _ runner.AgentUpdateHandler) {
					if providerB.writableCalls == 1 {
						if request.Workspace == "" || originalWorkspace != "" {
							t.Fatal("first writable request lost its original workspace")
						}
						originalWorkspace = request.Workspace
					} else {
						if request.Resume.ThreadID != "thread-native" || request.Resume.SessionID != "" || originalWorkspace == "" || request.Workspace != originalWorkspace || !strings.Contains(request.Prompt, "Checked acceptance") {
							t.Fatalf("current validator refusal lost resumed repair identity/findings: %+v", request.Resume)
						}
						resumedTurns++
					}
				}
			}
			providerB.duringTurn = func() {
				role := runner.RoleRework
				if test.landing {
					role = runner.RoleMerge
				}
				if identity := h.scheduler.RunExecution(issue.ID).(*ownedExecution).CompletionData().Identity; !test.transferCode && (identity == nil || identity.Role != role) {
					t.Fatalf("provider identity = %+v, want role=%s", identity, role)
				}
				expectedFiles := map[string]string{"CHANGE.md": "changed\n"}
				if test.absorbedConflict {
					conflicts, err := exec.CommandContext(t.Context(), "git", "-C", providerB.workspace, "diff", "--name-only", "--diff-filter=U").Output()
					if err != nil || strings.TrimSpace(string(conflicts)) != "CHANGE.md" {
						t.Fatalf("expected a real source conflict: %q, %v", conflicts, err)
					}
					if test.landing && test.restartIntent != "resolved" {
						return
					}
					resolution := "integrated\n"
					if test.restartIntent == "resolved" && !test.landing {
						resolution += "resolved deliverable\n"
					}
					if err := os.WriteFile(filepath.Join(providerB.workspace, "CHANGE.md"), []byte(resolution), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, providerB.workspace, "add", "CHANGE.md")
					expectedFiles["CHANGE.md"] = resolution
				}
				if test.transferCode {
					if got := head(providerB.workspace); got != recovered.Version.HeadSHA {
						t.Fatalf("code transfer started at %s, want selected head %s", got, recovered.Version.HeadSHA)
					}
				} else {
					expectedFiles["UPSTREAM.md"] = "new dev base\n"
				}
				if test.transferDirty {
					status, err := exec.CommandContext(t.Context(), "git", "-C", providerB.workspace, "status", "--porcelain").Output()
					if err != nil || len(status) != 0 {
						t.Fatalf("transfer resumed local dirty bytes: %q, %v", status, err)
					}
				}
				for name, expected := range expectedFiles {
					data, err := os.ReadFile(filepath.Join(providerB.workspace, name))
					if err != nil || string(data) != expected {
						t.Fatalf("second Mac started without recovered/refreshed source: %s = %q, %v", name, data, err)
					}
				}
			}
			if test.restartIntent != "" {
				if data := h.scheduler.RunExecution(issue.ID).(*ownedExecution).CompletionData(); data.Sequence != 0 || data.Identity != nil {
					t.Fatalf("fresh restored scheduler inherited old execution: %+v", data)
				}
				lease := h.scheduler.RunExecution(issue.ID).Recovery().Lease
				if lease.ID == owner.claim.lease.ID || lease.FencingToken == owner.claim.lease.FencingToken || lease.MachineID == owner.claim.lease.MachineID {
					t.Fatalf("fresh scheduler reused previous execution authority: %+v", lease)
				}
				if recovered.Version.ID != old.Change.CurrentVersion || recovered.Version.Source == nil || recovered.Version.Source.DiffSHA256 != old.Versions[0].Source.DiffSHA256 {
					t.Fatalf("fresh recovery lost selected source: %+v", recovered.Version)
				}
				target := "Rework"
				if test.restartIntent == "resolved" {
					target = "Merging"
					if test.landing {
						target = "Done"
					}
				}
				second := run(sourceB, providerB, candidates[0], h.scheduler.RunExecution(issue.ID), target)
				detail, err := h.admin.Change(t.Context(), item, first.NativeChange.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				original, current := old.Versions[0], detail.Versions[0]
				original.Landing, current.Landing = nil, nil
				original.LandingQuality, current.LandingQuality = nil, nil
				if !reflect.DeepEqual(current, original) || !reflect.DeepEqual(detail.Reviews[0], old.Reviews[0]) {
					t.Fatalf("restart changed original source/review: %+v, %v", detail, err)
				}
				if test.restartIntent != "resolved" {
					if detail.Change.CurrentLanding() != nil || h.state(t, issue.ID) != "Rework" || head(remote) != newBase {
						t.Fatalf("unfinished/stale completion manufactured acceptance: change=%+v result=%+v", detail.Change, second)
					}
					recovery, err := h.admin.Recovery(t.Context(), item)
					if err != nil || recovery.Change == nil || recovery.ChangeDetail == nil || recovery.Change.ChangeID != detail.Change.ID || recovery.Change.VersionID != detail.Change.CurrentVersion || recovery.ChangeDetail.Change.CurrentVersion != detail.Change.CurrentVersion {
						t.Fatalf("retry lost current source authority: %+v, %v", recovery, err)
					}
					for _, attempt := range recovery.Attempts {
						if attempt.Runtime != nil && attempt.Runtime.Completion != nil && attempt.Runtime.Completion.AcceptanceRecorded {
							t.Fatalf("unaccepted source recorded acceptance: %+v", attempt)
						}
					}
					if test.restartIntent == "stale" && !test.landing && (resumeVerified != 1 || resumedTurns != 1 || providerB.writableCalls != 2 || providerB.validator != "fail") {
						t.Fatalf("stale completion did not resume unfinished repair under current validator refusal: verified=%d resumed=%d writable=%d verdict=%s", resumeVerified, resumedTurns, providerB.writableCalls, providerB.validator)
					}
					candidates := h.candidatesIn(t, "Rework")
					if len(candidates) != 1 || candidates[0].ID != issue.ID {
						t.Fatalf("unaccepted source lost its next recovery candidate: %+v", candidates)
					}
					if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], h.clock()); err != nil {
						t.Fatal(err)
					}
					source, err := h.scheduler.RunExecution(issue.ID).(runner.ChangeSourceExecution).RecoverChangeSource(t.Context())
					if err != nil || source.Version.ID != recovery.Change.VersionID || source.Version.HeadSHA != recovery.Change.HeadSHA || source.Version.Source == nil || source.Version.Source.Validate(source.Version.BaseSHA, source.Version.HeadSHA, source.Bundle) != nil {
						t.Fatalf("current retry source cannot be authenticated and restored: %+v, %v", source.Version, err)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "verified recovery source"); err != nil {
						t.Fatal(err)
					}
					return
				}
				if test.landing {
					landing := second.NativeLanding
					if landing == nil && second.NativeChange != nil {
						landing = second.NativeChange.Landing
					}
					version := detail.Versions[0]
					if len(detail.Versions) != len(old.Versions) || version.ID != old.Change.CurrentVersion || version.HeadSHA != oldHead || version.PolicyID != h.descriptor.ID || providerB.writableCalls != 0 || landing == nil || !landing.Landed || landing.VersionID != version.ID || landing.HeadSHA != version.HeadSHA || landing.Gate == nil || landing.Gate.ExitCode != 0 || head(remote) != landing.MergeSHA || detail.Change.CurrentLanding() == nil || !reflect.DeepEqual(version.LandingQuality, detail.Change.CurrentLanding().Quality) {
						t.Fatalf("cleared conflict bypassed current reviewed source/gate/landing authority: version=%+v landing=%+v writable=%d", version, landing, providerB.writableCalls)
					}
					return
				}
				if second.NativeChange == nil || second.NativeChange.VersionID == old.Change.CurrentVersion || !second.NativeChange.Reviewed || second.NativeChange.Files == 0 {
					t.Fatalf("resolved source has no new reviewed version: %+v", second.NativeChange)
				}
				version := detail.Versions[len(detail.Versions)-1]
				review := detail.Reviews[len(detail.Reviews)-1]
				if version.ID != second.NativeChange.VersionID || version.HeadSHA != second.NativeChange.HeadSHA || version.PolicyID != h.descriptor.ID || version.Source == nil || version.Source.DiffSHA256 == tracker.ChangeSourceDigest(nil) || review.VersionID != version.ID || review.Decision != "approved" {
					t.Fatalf("resolved source lost current-head/policy review: version=%+v review=%+v", version, review)
				}
				if detail.Change.CurrentLanding() != nil || version.LandingQuality != nil {
					t.Fatal("reviewed resolution acquired quality before landing")
				}
				path, err := api.NativeAttemptDiffPath(version.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				var finalDiff tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+path, nil, &finalDiff); err != nil {
					t.Fatal(err)
				}
				attempt, err := h.admin.NativeAttempt(t.Context(), item, version.AttemptID)
				if err != nil || attempt.AttemptID != version.AttemptID || attempt.RunID != version.RunID || attempt.PolicyID != version.PolicyID || attempt.Status != "succeeded" || attempt.Outcome != "succeeded" || attempt.Sequence <= 0 || finalDiff.AttemptID != version.AttemptID || finalDiff.BaseSHA != version.BaseSHA || finalDiff.HeadSHA != version.HeadSHA || finalDiff.Generation.Source != tracker.DiffSourceAttempt || finalDiff.Generation.Seq != attempt.Sequence || finalDiff.Producer.Kind != tracker.DiffSourceAttempt || finalDiff.Producer.ID != version.AttemptID || finalDiff.Producer.LeaseID != attempt.LeaseID || finalDiff.Producer.FencingToken != attempt.FencingToken || attempt.Checkpoint == nil || attempt.Checkpoint.HeadSHA != version.HeadSHA {
					t.Fatalf("resolved source lost final diff/checkpoint/producer authority: diff=%+v attempt=%+v error=%v", finalDiff, attempt, err)
				}
				if !test.landing {
					candidates := h.candidatesIn(t, "Merging")
					if len(candidates) != 1 || candidates[0].ID != issue.ID {
						t.Fatalf("reviewed resolution lost its landing candidate: %+v", candidates)
					}
					if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], h.clock()); err != nil {
						t.Fatal(err)
					}
					provider := &committingAgent{validator: "pass", complete: true}
					second = run(sourceB, provider, candidates[0], h.scheduler.RunExecution(issue.ID), "Done")
					if provider.writableCalls != 0 {
						t.Fatalf("current reviewed resolution dispatched another coding turn: %d", provider.writableCalls)
					}
				}
				landing := second.NativeLanding
				if landing == nil && second.NativeChange != nil {
					landing = second.NativeChange.Landing
				}
				if landing == nil || !landing.Landed || landing.VersionID != version.ID || landing.HeadSHA != version.HeadSHA || head(remote) != landing.MergeSHA {
					t.Fatalf("resolved conflict has no matching verified landing: %+v", landing)
				}
				settled, err := h.admin.Change(t.Context(), item, first.NativeChange.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				canonical := settled.Change.CurrentLanding()
				if canonical == nil || canonical.Quality == nil || canonical.VersionID != version.ID || canonical.HeadSHA != version.HeadSHA || canonical.MergeSHA != landing.MergeSHA {
					t.Fatalf("resolved landing lost canonical quality/source: %+v", canonical)
				}
				latestIssue, err := h.admin.Issue(t.Context(), item)
				if err != nil || canonical.Quality.IssueBody != latestIssue.Body {
					t.Fatalf("landing quality did not freeze the canonical issue body: %+v, %v", canonical.Quality, err)
				}
				body := latestIssue.Body + "\nLater issue context.\n"
				updated, err := h.admin.UpdateIssue(t.Context(), item, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: latestIssue.Revision, Body: &body})
				if err != nil || updated.Body != body || updated.Body == canonical.Quality.IssueBody {
					t.Fatalf("later issue update did not change current context: %+v, %v", updated, err)
				}
				selected, err := h.admin.Change(t.Context(), item, first.NativeChange.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil {
					t.Fatal(err)
				}
				for _, read := range []struct {
					name   string
					detail *tracker.ChangeDetail
				}{{"selected", &selected}, {"recovery current", recovery.ChangeDetail}} {
					if read.detail == nil || read.detail.Change.ID != settled.Change.ID || read.detail.Change.CurrentVersion != version.ID || !reflect.DeepEqual(read.detail.Change.CurrentLanding(), canonical) {
						t.Fatalf("%s read changed canonical frozen landing: %+v", read.name, read.detail)
					}
					matched := false
					for _, seen := range read.detail.Versions {
						if seen.ID == version.ID {
							matched = true
							if seen.ChangeID != settled.Change.ID || seen.HeadSHA != version.HeadSHA || !reflect.DeepEqual(seen.LandingQuality, canonical.Quality) {
								t.Fatalf("%s read lost matching frozen quality: %+v", read.name, seen)
							}
						} else if seen.ID == old.Versions[0].ID && seen.LandingQuality != nil {
							t.Fatalf("%s read projected landing quality onto unlanded prior source: %+v", read.name, seen)
						}
					}
					if !matched {
						t.Fatalf("%s read omitted the landed version", read.name)
					}
				}
				return
			}
			second := run(sourceB, providerB, candidates[0], h.scheduler.RunExecution(issue.ID))
			if test.absorbedConflict {
				if second.NativeChange.HeadSHA == newBase || second.NativeChange.Files != 0 || headTree(providerB.workspace, second.NativeChange.HeadSHA) != headTree(sourceA, newBase) {
					t.Fatalf("absorbed resolution must retain a reviewable head with the target tree: %+v", second.NativeChange)
				}
			}
			expectedBase := newBase
			if test.transferCode {
				expectedBase = initialBase
			}
			if second.NativeChange.HeadSHA == oldHead || second.NativeChange.BaseSHA != expectedBase || providerB.calls != 2 {
				t.Fatalf("rework reused old head or validation: %+v, calls=%d", second.NativeChange, providerB.calls)
			}
			detail, err := h.admin.Change(context.Background(), item, first.NativeChange.ChangeID)
			if err != nil || len(detail.Versions) != versionCount || len(detail.Reviews) != 2 || detail.Versions[versionCount-1].PolicyID != h.descriptor.ID || detail.Reviews[1].VersionID != second.NativeChange.VersionID || detail.Versions[0].ID != old.Versions[0].ID {
				t.Fatalf("new source did not get current-policy review: %+v, %v", detail, err)
			}
			if test.absorbedConflict {
				h.complete(t, issue.ID, second.NativeChange)
				current, err := h.admin.Issue(t.Context(), item)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}); err != nil {
					t.Fatal(err)
				}
				h.claim(t, issue.ID)
				retried, err := h.scheduler.RunExecution(issue.ID).(runner.ChangeSourceExecution).RecoverChangeSource(t.Context())
				if err != nil || retried.Version.ID != second.NativeChange.VersionID || retried.Version.HeadSHA != second.NativeChange.HeadSHA {
					t.Fatalf("retry recovered stale Change source: %+v, %v", retried.Version, err)
				}
			}
		})
	}
}

func RunNativeSourceResumesWithoutPreviousRuntime(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	newNativeChangeHub := func(t *testing.T, memory ...bool) *nativeChangeHub { return newScenarioHub(t, factories, memory...) }
	t.Parallel()
	for _, tc := range []struct {
		name          string
		loseWorkspace bool
	}{
		{name: "fresh runner"},
		{name: "fresh runner without previous workspace", loseWorkspace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Resume published source")
			h.claim(t, issue.ID)
			firstMachine := h.scheduler.RunExecution(issue.ID).Recovery().Lease.MachineID
			firstWorkspace := nativeChangeSourceRepo(t)
			head := func(path string) string {
				data, err := exec.CommandContext(t.Context(), "git", "-C", path, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(data))
			}
			base := head(firstWorkspace)
			remote := filepath.Join(t.TempDir(), "origin.git")
			nativeChangeGit(t, firstWorkspace, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, firstWorkspace, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, firstWorkspace, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, firstWorkspace, "push", "-u", "origin", "main")
			if err := os.WriteFile(filepath.Join(firstWorkspace, "CHANGE.md"), []byte("published work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, firstWorkspace, "add", "CHANGE.md")
			nativeChangeGit(t, firstWorkspace, "commit", "-m", "retain work")
			exactHead := head(firstWorkspace)
			capture, err := workspace.CaptureChangeSource(t.Context(), firstWorkspace, base, exactHead)
			if err != nil {
				t.Fatal(err)
			}
			item := tracker.NativeWorkItemID(issue.ID)
			change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
			if err != nil {
				t.Fatal(err)
			}
			version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{Mutation: nativeMutationKey(), SourceBundle: capture.Bundle, ChangeVersionInput: tracker.ChangeVersionInput{
				BaseSHA: base, HeadSHA: exactHead, MergeBaseSHA: base, Repository: nativeChangeRepository, PolicyID: h.descriptor.ID, Source: &capture.Source,
				Code: tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + exactHead, SHA256: policy.Digest([]byte(exactHead)), Availability: "unverified"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "checkpointed"); err != nil {
				t.Fatal(err)
			}
			h.restart()
			if tc.loseWorkspace {
				if err := os.Rename(firstWorkspace, firstWorkspace+"-unavailable"); err != nil {
					t.Fatal(err)
				}
			}
			h.native = h.otherWorker(t)
			h.connector.worker, err = NewNativeConnector(h.native)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := h.native.Transport().RunnerIdentity(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			machine := fixtureSchedulerMachine(h.scheduler)
			machine.ID = identity.MachineID
			h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: h.scheduler.HeartbeatInterval(), LeaseTTL: h.scheduler.LeaseDuration()})
			if err != nil {
				t.Fatal(err)
			}
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
			if data := execution.(*ownedExecution).CompletionData(); data.Sequence != 0 || data.Identity != nil {
				t.Fatalf("fresh runner inherited prior execution authority: %+v", data)
			}
			if execution.Recovery().Lease.MachineID == firstMachine {
				t.Fatal("second runner reused the first machine identity")
			}
			recovered, err := execution.(runner.ChangeSourceExecution).RecoverChangeSource(t.Context())
			if err != nil || recovered.Version.ID != version.ID || recovered.Version.HeadSHA != exactHead {
				t.Fatalf("fresh runner lost published source: %+v, %v", recovered.Version, err)
			}
			secondSource := filepath.Join(t.TempDir(), "source")
			nativeChangeGit(t, remote, "clone", "--no-local", remote, secondSource)
			nativeChangeGit(t, secondSource, "remote", "set-url", "origin", nativeChangeRepository)
			nativeChangeGit(t, secondSource, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			if _, err := exec.CommandContext(t.Context(), "git", "-C", secondSource, "cat-file", "-e", exactHead+"^{commit}").Output(); err == nil {
				t.Fatal("fresh checkout already contains unpublished work")
			}
			backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: secondSource, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier, Source: &recovered})
			if err != nil || head(info.Path) != exactHead {
				t.Fatalf("fresh workspace did not restore the published head: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(info.Path, "CHANGE.md"))
			if err != nil || string(data) != "published work\n" {
				t.Fatalf("fresh workspace lost published bytes: %q, %v", data, err)
			}
			if tc.loseWorkspace {
				if _, err := os.Stat(firstWorkspace); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("recovery recreated old runner state: %v", err)
				}
			}
		})
	}
}
