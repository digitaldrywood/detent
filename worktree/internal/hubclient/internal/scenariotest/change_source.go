package scenariotest

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func RunNativeReworkRecoversReviewedSourceAcrossMachines(t *testing.T, factories Constructors) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	newNativeChangeHubWithStates := func(t *testing.T, review string, states []tracker.NativeState) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, review, states, false, false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	}
	if testing.Short() {
		t.Skip("git subprocess integration")
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
	}{
		{name: "same policy"},
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
			h := newNativeChangeHubWithStates(t, "In Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
				{Name: "In Review", Transitions: []string{"Rework", "In Progress"}},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
				{Name: "Done", Terminal: true},
			})
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
			run := func(source string, provider *committingAgent, candidate connector.Issue, execution runner.Execution) runner.RunResult {
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
				runtimeStore, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
				if err != nil {
					t.Fatal(err)
				}
				defer runtimeStore.Close()
				agent, err := runner.NewRunner(runner.Dependencies{ProcessSource: emptyProcessSource, Store: runtimeStore, ProjectID: "local", Workflow: config.Workflow{Config: config.Config{Plan: gate.PlanConfig{Enabled: test.planning}, Gate: gate.Config{Run: "test -f CHANGE.md", Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}, Workspace: backend, AgentBackend: provider})
				if err != nil {
					t.Fatal(err)
				}
				result, err := agent.Run(t.Context(), runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
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
				if err != nil || result.NativeChange == nil || !result.NativeChange.Reviewed {
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
			if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: state, Reason: "user_requested"}); err != nil {
				t.Fatal(err)
			}
			h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: "machine-air", Hostname: "Air", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
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
			providerB.duringTurn = func() {
				if identity := h.scheduler.RunExecution(issue.ID).(*ownedExecution).CompletionData().Identity; !test.transferCode && (identity == nil || identity.Role != runner.RoleRework) {
					t.Fatalf("Rework provider identity = %+v, want role=rework", identity)
				}
				expectedFiles := map[string]string{"CHANGE.md": "changed\n"}
				if test.absorbedConflict {
					conflicts, err := exec.CommandContext(t.Context(), "git", "-C", providerB.workspace, "diff", "--name-only", "--diff-filter=U").Output()
					if err != nil || strings.TrimSpace(string(conflicts)) != "CHANGE.md" {
						t.Fatalf("expected a real source conflict: %q, %v", conflicts, err)
					}
					if err := os.WriteFile(filepath.Join(providerB.workspace, "CHANGE.md"), []byte("integrated\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, providerB.workspace, "add", "CHANGE.md")
					expectedFiles["CHANGE.md"] = "integrated\n"
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
		{name: "previous database missing"},
		{name: "previous database and workspace missing", loseWorkspace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			issue := h.createInProgress(t, "Resume published source")
			h.claim(t, issue.ID)
			firstMachine := h.scheduler.RunExecution(issue.ID).Recovery().Lease.MachineID
			oldDatabase := nativeRuntimeDatabasePath(t)
			firstStore, err := store.Open(t.Context(), store.Config{Path: oldDatabase})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := firstStore.Close(); err != nil {
					t.Error(err)
				}
			})
			attemptID, err := firstStore.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "local", IssueID: issue.ID, WorkerType: "agent", StartedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
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
			if err := firstStore.Close(); err != nil {
				t.Fatal(err)
			}
			missing := []string{oldDatabase}
			if tc.loseWorkspace {
				missing = append(missing, firstWorkspace)
			}
			for _, path := range missing {
				if err := os.Rename(path, path+"-unavailable"); err != nil {
					t.Fatal(err)
				}
			}
			secondStore, err := store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := secondStore.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := secondStore.WorkAttempt(t.Context(), attemptID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("fresh runner inherited a local attempt: %v", err)
			}
			h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: "machine-second", Hostname: "example", Capacity: 1, Version: "test"}, HeartbeatInterval: h.scheduler.HeartbeatInterval(), LeaseTTL: h.scheduler.LeaseDuration()})
			if err != nil {
				t.Fatal(err)
			}
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
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
			for _, path := range missing {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("recovery recreated old runner state: %v", err)
				}
			}
		})
	}
}
