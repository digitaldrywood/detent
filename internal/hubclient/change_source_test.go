package hubclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func TestNativeReworkTransfersUnpushedSourceAcrossMachines(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	t.Setenv("TMPDIR", t.TempDir())
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name         string
		updatePolicy bool
		republish    bool
	}{
		{name: "same policy"},
		{name: "updated policy", updatePolicy: true},
		{name: "operator republished same tree", republish: true},
		{name: "operator republished same tree with policy update", republish: true, updatePolicy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			updatePolicy := test.updatePolicy
			h := newNativeChangeHubWithStates(t, "In Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
				{Name: "In Review", Transitions: []string{"Rework"}},
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
			run := func(source string, provider *committingAgent, candidate connector.Issue, execution runner.Execution) runner.RunResult {
				backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				if candidate.State == "Rework" && !updatePolicy {
					if _, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: candidate.ID, Identifier: candidate.Identifier}); err != nil {
						t.Fatal(err)
					}
				}
				runtimeStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
				if err != nil {
					t.Fatal(err)
				}
				defer runtimeStore.Close()
				agent, err := runner.NewRunner(runner.Dependencies{Store: runtimeStore, ProjectID: "local", Workflow: config.Workflow{Config: config.Config{Gate: gate.Config{Run: "test -f CHANGE.md", Validator: gate.ValidatorConfig{Enabled: true}}}, Prompt: "Complete the issue"}, Workspace: backend, AgentBackend: provider})
				if err != nil {
					t.Fatal(err)
				}
				result, err := agent.Run(t.Context(), runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
				if err != nil || result.NativeChange == nil || !result.NativeChange.Reviewed {
					t.Fatalf("runner did not publish newly validated source: %+v, %v", result.NativeChange, err)
				}
				return result
			}
			candidateA := h.claim(t, issue.ID)
			providerA := &committingAgent{staged: true, validator: "pass", complete: true}
			first := run(sourceA, providerA, candidateA, h.scheduler.RunExecution(issue.ID))
			oldHead := first.NativeChange.HeadSHA
			if _, err := exec.CommandContext(t.Context(), "git", "-C", remote, "cat-file", "-e", oldHead+"^{commit}").Output(); err == nil {
				t.Fatal("reviewed commit was pushed to the forge fixture")
			}
			old, err := h.admin.Change(t.Context(), item, first.NativeChange.ChangeID)
			if err != nil || old.Versions[0].Source == nil || len(old.Reviews) != 1 {
				t.Fatalf("reviewed source was not retained: %+v, %v", old, err)
			}
			h.complete(t, issue.ID, first.NativeChange)
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
			if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Rework", Reason: "user_requested"}); err != nil {
				t.Fatal(err)
			}
			h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: "machine-air", Hostname: "Air", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Rework"}})
			if err != nil || len(candidates) != 1 {
				t.Fatalf("second Mac did not claim Rework: %+v, %v", candidates, err)
			}
			if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
				t.Fatal(err)
			}
			providerB := &committingAgent{validator: "pass", complete: true}
			providerB.duringTurn = func() {
				if test.republish {
					if _, err := exec.CommandContext(t.Context(), "git", "-C", sourceB, "cat-file", "-e", oldHead+"^{commit}").Output(); err == nil {
						t.Fatal("obsolete attempt head was available on the second Mac")
					}
				}
				for name, expected := range map[string]string{"CHANGE.md": "changed\n", "UPSTREAM.md": "new dev base\n"} {
					data, err := os.ReadFile(filepath.Join(providerB.workspace, name))
					if err != nil || string(data) != expected {
						t.Fatalf("second Mac started without recovered/refreshed source: %s = %q, %v", name, data, err)
					}
				}
			}
			second := run(sourceB, providerB, candidates[0], h.scheduler.RunExecution(issue.ID))
			if second.NativeChange.HeadSHA == oldHead || second.NativeChange.BaseSHA != newBase || providerB.calls != 2 {
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
