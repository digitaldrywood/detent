package hubclient

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeReworkRecoversReviewedSourceAcrossMachines(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Setenv("TMPDIR", t.TempDir())
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name            string
		updatePolicy    bool
		republish       bool
		transferDirty   bool
		transfer        bool
		transferCode    bool
		workerOwnership bool
		planning        bool
	}{
		{name: "same policy"},
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
				backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: root, SourceRoot: source, AutoBranch: true})
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
				runtimeStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
				if err != nil {
					t.Fatal(err)
				}
				defer runtimeStore.Close()
				agent, err := runner.NewRunner(runner.Dependencies{Store: runtimeStore, ProjectID: "local", Workflow: config.Workflow{Config: config.Config{Plan: gate.PlanConfig{Enabled: test.planning}, Gate: gate.Config{Run: "test -f CHANGE.md", Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}, Workspace: backend, AgentBackend: provider})
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
			owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			writerStopped := false
			if test.workerOwnership {
				if err := owner.KeepWorker(func(context.Context) error {
					if owner.data.Outcome != "succeeded" {
						t.Error("worker cleanup preceded acknowledged completion")
					}
					if !writerStopped {
						return errors.New("fixture writer remains active")
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
				if !errors.Is(err, ErrNoClaimableWork) && (!errors.As(err, &conflict) || conflict.Status != http.StatusConflict || conflict.Code != "lease_conflict") {
					t.Fatalf("competing writer was not fenced: %v", err)
				}
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
				if err != nil || evidence.CurrentLease == nil || evidence.CurrentLease.ID != owner.claim.lease.ID || !owner.hasWorkers() {
					t.Fatalf("cleanup refusal lost the current execution owner: %+v, %v", evidence.CurrentLease, err)
				}
				var source tracker.NativeSourceRecovery
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/source-recovery", nil, &source); err != nil || source.Quiesced || source.VersionID != first.NativeChange.VersionID || source.HeadSHA != oldHead {
					t.Fatalf("active writer lost retained source or became transferable: %+v, %v", source, err)
				}
				writerStopped = true
			}
			h.complete(t, issue.ID, first.NativeChange)
			if test.workerOwnership {
				var source tracker.NativeSourceRecovery
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/source-recovery", nil, &source); err != nil || !source.Quiesced || !source.Available || source.VersionID != first.NativeChange.VersionID || source.HeadSHA != oldHead {
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
			h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: "machine-air", Hostname: "Air", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
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
				execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				execution.claim.recovery.SourceVersionID = recovered.Version.ID
				execution.claim.recovery.SourceAttempt().MachineID = execution.claim.lease.MachineID
				recovered, err = execution.RecoverChangeSource(t.Context())
				if err != nil || !recovered.Transferred || recovered.Version.ID != execution.claim.recovery.SourceVersionID {
					t.Fatalf("transfer lost exact source selection: %+v, %v", recovered.Version, err)
				}
			}
			providerB := &committingAgent{validator: "pass", complete: true}
			providerB.duringTurn = func() {
				if identity := h.scheduler.RunExecution(issue.ID).(*nativeExecution).data.Identity; !test.transferCode && (identity == nil || identity.Role != runner.RoleRework) {
					t.Fatalf("Rework provider identity = %+v, want role=rework", identity)
				}
				expectedFiles := map[string]string{"CHANGE.md": "changed\n"}
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
		})
	}
}

func TestNativeRecoveredSourceRequiresCurrentVersion(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    string
		version   string
		attempt   string
		base      string
		policy    string
		wantError bool
	}{
		{name: "original version", change: "change", version: "original"},
		{name: "new operator version at same head", change: "change", version: "new", wantError: true},
		{name: "own publication acknowledgement lost", change: "change", version: "new", attempt: "attempt", base: "base", policy: "policy"},
		{name: "own publication wrong base", change: "change", version: "new", attempt: "attempt", base: "other", policy: "policy", wantError: true},
		{name: "own publication wrong policy", change: "change", version: "new", attempt: "attempt", base: "base", policy: "other", wantError: true},
		{name: "another Change", change: "other", version: "new", attempt: "attempt", base: "base", policy: "policy", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			execution := &nativeExecution{data: tracker.NativeRunData{RunID: "run", AttemptID: "attempt", PolicyID: "policy"}, recoveredSource: &tracker.NativeChangeReference{ChangeID: "change", VersionID: "original", HeadSHA: "head"}}
			detail := tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: test.change, CurrentVersion: test.version}, Versions: []tracker.ChangeVersion{{ID: test.version, ChangeVersionInput: tracker.ChangeVersionInput{RunID: "run", AttemptID: test.attempt, BaseSHA: test.base, HeadSHA: "head", PolicyID: test.policy}}}}
			if err := execution.requireRecoveredSource(detail, tracker.AttemptDiffRequest{BaseSHA: "base", HeadSHA: "head"}); (err != nil) != test.wantError {
				t.Fatalf("recovered source publication authority: %v", err)
			}
		})
	}
}

func TestNativeChangeSourceRecoversOnlyVerifiedOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name                                       string
		status                                     int
		foreign, wrongHead, lostLease, wrongDiff   bool
		missingCapture, wrongWorktree              bool
		wantError                                  bool
		transferred, staleTransfer, recovered      bool
		foreignChange, dirty, unpublished, unknown bool
		available, retained, activePublication     bool
		uncertain                                  bool
		currentCheckpoint, cleanCheckpoint         bool
	}{
		{name: "owner recaptures missing artifact", status: http.StatusInternalServerError},
		{name: "transferred version retains exact selection during recapture", status: http.StatusInternalServerError, transferred: true},
		{name: "stale transfer advances to current published source", transferred: true, staleTransfer: true, available: true},
		{name: "stale transfer with current unpushed checkpoint advances", transferred: true, staleTransfer: true, currentCheckpoint: true, available: true},
		{name: "stale transfer with current clean checkpoint advances", transferred: true, staleTransfer: true, currentCheckpoint: true, cleanCheckpoint: true, available: true},
		{name: "current dirty checkpoint refuses substitution", transferred: true, staleTransfer: true, currentCheckpoint: true, dirty: true, available: true, wantError: true},
		{name: "current uncertain checkpoint refuses substitution", transferred: true, staleTransfer: true, currentCheckpoint: true, uncertain: true, available: true, wantError: true},
		{name: "current checkpoint cannot accept corrupt current bundle", transferred: true, staleTransfer: true, currentCheckpoint: true, missingCapture: true, wantError: true},
		{name: "recovered source advances without transfer selection", staleTransfer: true, recovered: true, available: true},
		{name: "recovered transfer advances to current published source", transferred: true, staleTransfer: true, recovered: true, available: true},
		{name: "different Change transfer refuses substitution", transferred: true, staleTransfer: true, foreignChange: true, available: true, wantError: true},
		{name: "different Change refuses substitution", transferred: true, staleTransfer: true, recovered: true, foreignChange: true, available: true, wantError: true},
		{name: "unknown version refuses substitution", transferred: true, staleTransfer: true, unknown: true, available: true, wantError: true},
		{name: "uncertain checkpoint refuses substitution", transferred: true, staleTransfer: true, uncertain: true, available: true, wantError: true},
		{name: "dirty checkpoint refuses substitution", transferred: true, staleTransfer: true, dirty: true, available: true, wantError: true},
		{name: "unpublished checkpoint refuses substitution", transferred: true, staleTransfer: true, unpublished: true, available: true, wantError: true},
		{name: "retained unpublished source refuses substitution", transferred: true, staleTransfer: true, retained: true, available: true, wantError: true},
		{name: "active publication refuses substitution", transferred: true, staleTransfer: true, activePublication: true, available: true, wantError: true},
		{name: "stale selection cannot accept corrupt current bundle", transferred: true, staleTransfer: true, missingCapture: true, wantError: true},
		{name: "owner recaptures corrupt artifact"},
		{name: "foreign runner cannot recapture", foreign: true, wantError: true},
		{name: "wrong local checkpoint cannot recapture", wrongHead: true, wantError: true},
		{name: "changed immutable diff cannot recapture", wrongDiff: true, wantError: true},
		{name: "owner workspace cannot be read", missingCapture: true, wantError: true},
		{name: "local worktree differs from checkpoint", wrongWorktree: true, wantError: true},
		{name: "lease lost during capture", lostLease: true, wantError: true},
		{name: "authorization denial cannot recapture", status: http.StatusForbidden, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t)
			issue := h.createInProgress(t, "Recover owner source")
			h.claim(t, issue.ID)
			execution, ok := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if !ok {
				t.Fatal("native execution missing")
			}
			repo := nativeChangeSourceRepo(t)
			head := func() string {
				data, err := exec.CommandContext(t.Context(), "git", "-C", repo, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(string(data))
			}
			base := head()
			if err := os.WriteFile(filepath.Join(repo, "CHANGE.md"), []byte("saved work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			nativeChangeGit(t, repo, "add", "CHANGE.md")
			nativeChangeGit(t, repo, "commit", "-m", "preserved source")
			exactHead := head()
			capture, err := workspace.CaptureChangeSource(t.Context(), repo, base, exactHead)
			if err != nil {
				t.Fatal(err)
			}
			item := tracker.NativeWorkItemID(issue.ID)
			change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
			if err != nil {
				t.Fatal(err)
			}
			if test.wrongDiff {
				capture.Source.DiffSHA256 = strings.Repeat("d", 64)
			}
			version, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
				Mutation: nativeMutationKey(), ChangeVersionInput: tracker.ChangeVersionInput{
					BaseSHA: base, HeadSHA: exactHead, MergeBaseSHA: base, Repository: nativeChangeRepository,
					Code:      tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + exactHead, SHA256: policy.Digest([]byte(exactHead)), Availability: "unverified"},
					Artifacts: []tracker.ChangeArtifact{}, PolicyID: h.descriptor.ID, Source: &capture.Source,
				}, SourceBundle: capture.Bundle,
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := execution.claim.lease.MachineID
			if test.foreign {
				owner = "other"
			}
			checkpointHead := exactHead
			if test.wrongHead {
				checkpointHead = base
			}
			execution.claim.recovery.SourceAttemptID = "saved-source"
			if test.transferred {
				execution.claim.recovery.SourceVersionID = version.ID
			}
			if test.recovered {
				execution.recoveredSource = &tracker.NativeChangeReference{ChangeID: change.ID, VersionID: version.ID, HeadSHA: exactHead}
			}
			if test.activePublication {
				execution.change = &runner.NativeChange{ChangeID: change.ID, VersionID: version.ID, HeadSHA: exactHead}
			}
			if test.staleTransfer {
				if err := os.WriteFile(filepath.Join(repo, "NEWER.md"), []byte("newer published work\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, repo, "add", "NEWER.md")
				nativeChangeGit(t, repo, "commit", "-m", "newer source")
				capture, err = workspace.CaptureChangeSource(t.Context(), repo, base, head())
				if err != nil {
					t.Fatal(err)
				}
				input := version.ChangeVersionInput
				input.HeadSHA, input.Source = head(), &capture.Source
				input.Code.SHA256 = strings.Repeat("a", 64)
				expected := version.ID
				if test.foreignChange {
					change, err = h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Other Change"})
					if err != nil {
						t.Fatal(err)
					}
					expected = ""
				}
				version, err = h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
					Mutation: nativeMutationKey(), ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: capture.Bundle,
				})
				if err != nil {
					t.Fatal(err)
				}
				if version.ID == execution.claim.recovery.SourceVersionID {
					t.Fatal("publication did not advance the current version")
				}
			}
			execution.claim.recovery.Attempts = []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "saved-source", MachineID: owner}, Checkpoint: &tracker.NativeCheckpoint{Resume: "resume_session", WorktreeState: "unpushed", HeadSHA: checkpointHead, Availability: "available", Storage: "local_only"}}}
			if test.unknown {
				execution.claim.recovery.SourceVersionID = "version_unknown"
			}
			checkpoint := execution.claim.recovery.SourceAttempt().Checkpoint
			if test.currentCheckpoint {
				checkpoint.HeadSHA = version.HeadSHA
			}
			if test.cleanCheckpoint {
				checkpoint.WorktreeState = "clean"
			}
			if test.uncertain {
				checkpoint.ExternalEffect, checkpoint.EffectState = "git_push", "ambiguous"
			}
			if test.dirty {
				checkpoint.WorktreeState, checkpoint.WorkspaceDigest = "dirty", "unpublished-digest"
			}
			if test.unpublished {
				checkpoint.HeadSHA = strings.Repeat("e", 40)
			}
			if test.retained {
				execution.retainedSource = &tracker.ChangeSourceCapture{Source: tracker.ChangeSource{HeadSHA: strings.Repeat("e", 40)}}
			}
			original := h.native.client.httpClient.Transport
			downloads := 0
			h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/source") {
					downloads++
					status := test.status
					body := []byte("corrupt source")
					if test.available {
						body = capture.Bundle
					}
					if status != 0 {
						body = []byte(`{"error":{"code":"internal_error","message":"source unavailable"}}`)
					} else {
						status = http.StatusOK
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				}
				return original.RoundTrip(request)
			})
			if test.wrongWorktree {
				nativeChangeGit(t, repo, "checkout", "--detach", base)
			}
			calls := 0
			if !test.missingCapture {
				execution.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					calls++
					if test.lostLease {
						h.scheduler.mu.Lock()
						delete(h.scheduler.nativeClaims, issue.ID)
						h.scheduler.mu.Unlock()
					}
					return workspace.CaptureChangeSource(ctx, repo, base, head)
				})
			}
			pending := filepath.Join(repo, "PENDING.md")
			if err := os.WriteFile(pending, []byte("pending local work"), 0o600); err != nil {
				t.Fatal(err)
			}
			selected := execution.claim.recovery.SourceVersionID
			previousSource, retainedSource := execution.recoveredSource, execution.retainedSource
			recovered, err := execution.RecoverChangeSource(t.Context())
			if (err != nil) != test.wantError {
				t.Fatalf("recovery error = %v", err)
			}
			if test.staleTransfer && test.wantError && !test.missingCapture {
				if !errors.Is(err, runner.ErrChangeSourceVersionSuperseded) {
					t.Fatalf("stale selection lost refusal classification: %v", err)
				}
				for _, identity := range []string{change.ID, selected, version.ID} {
					if !strings.Contains(err.Error(), identity) {
						t.Errorf("refusal omits source identity %q: %v", identity, err)
					}
				}
				if !strings.Contains(err.Error(), "re-read source recovery") || !strings.Contains(err.Error(), "before transfer") {
					t.Errorf("refusal omits reconciliation action: %v", err)
				}
				if calls != 0 || downloads != 0 || recovered.Version.ID != "" || execution.claim.recovery.SourceVersionID != selected || execution.recoveredSource != previousSource {
					t.Fatal("stale selection downloaded, recaptured or substituted source")
				}
				if err := execution.Validate(t.Context()); err != nil {
					t.Fatalf("source refusal changed execution authority: %v", err)
				}
			}
			wantCalls := 1
			if test.available {
				wantCalls = 0
			}
			if !test.wantError && (calls != wantCalls || recovered.Version.ID != version.ID || recovered.Version.BaseSHA != base || recovered.Version.HeadSHA != version.HeadSHA || !bytes.Equal(recovered.Bundle, capture.Bundle)) {
				t.Fatalf("exact source not recovered: %+v calls=%d", recovered.Version, calls)
			}
			if !test.wantError && recovered.Transferred != (test.transferred && !test.staleTransfer) {
				t.Fatal("source recovery lost transfer binding")
			}
			if test.wantError && (execution.recoveredSource != previousSource || execution.retainedSource != retainedSource) {
				t.Fatal("refusal changed recovered or retained unpublished source")
			}
			if !test.wantError && (execution.recoveredSource == nil || execution.recoveredSource.ChangeID != change.ID || execution.recoveredSource.VersionID != version.ID || execution.recoveredSource.HeadSHA != version.HeadSHA) {
				t.Fatal("recovery did not retain the validated current publication identity")
			}
			if test.currentCheckpoint && !test.wantError {
				retried, err := execution.RecoverChangeSource(t.Context())
				if err != nil || retried.Version.ID != version.ID || !bytes.Equal(retried.Bundle, capture.Bundle) {
					t.Fatalf("repeated recovery did not retain current published source: %q, %v", retried.Version.ID, err)
				}
			}
			if test.foreign || test.wrongHead || test.status == http.StatusForbidden {
				if calls != 0 {
					t.Fatal("denied recovery read local source")
				}
			}
			if test.lostLease && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("lost lease accepted: %v", err)
			}
			if data, err := os.ReadFile(pending); err != nil || string(data) != "pending local work" {
				t.Fatalf("pending work changed: %q %v", data, err)
			}
		})
	}
}
