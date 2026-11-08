package scenariotest

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (a *committingAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handler runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if a.duringValidation != nil && strings.Contains(request.Prompt, "Detent validator-agent") {
		a.duringValidation(request)
	}
	if a.validatorCommand != "" && strings.Contains(request.Prompt, "Detent validator-agent") {
		arguments, err := json.Marshal(map[string]string{"command": a.validatorCommand})
		if err != nil {
			return runner.AgentTurnResult{}, err
		}
		result, err := handler(ctx, runner.AgentToolCall{Name: "detent_run_validation", Arguments: arguments})
		if err != nil || strings.Contains(result.Content, `"exit_code":0`) != (a.validator == "pass") {
			return runner.AgentTurnResult{}, errors.Join(fmt.Errorf("unexpected prerequisite evidence: %+v", result), err)
		}
	}
	if a.dependencyOnly && strings.Contains(request.Prompt, "Detent validator-agent") {
		if !request.ReadOnly || !request.SupplementalTools || !strings.Contains(request.Prompt, "make check-fast passed") {
			return runner.AgentTurnResult{}, errors.New("native review lacks configured command evidence")
		}
		for _, command := range []string{`cd frontend && PATH="$PWD/../.test-bin:$PATH" pnpm build`, `cd frontend && PATH="$PWD/../.test-bin:$PATH" pnpm check`} {
			if a.failedCheck && strings.HasSuffix(command, "pnpm check") {
				command += " && exit 7"
			}
			arguments, _ := json.Marshal(map[string]string{"command": command})
			result, err := handler(ctx, runner.AgentToolCall{Name: "detent_run_validation", Arguments: arguments})
			if err != nil || !strings.Contains(result.Content, "passed") {
				return runner.AgentTurnResult{}, errors.Join(fmt.Errorf("missing host command output: %+v", result), err)
			}
		}
	}
	return a.RunTurn(ctx, request, update)
}

func nativeChangeRepositorySeed(name string, build func(context.Context, string) error) func(*testing.T) string {
	seed := sync.OnceValues(func() (string, error) {
		dir, err := os.MkdirTemp(nativeChangeFixtureRoot, name+"-seed-")
		if err != nil {
			return "", err
		}
		return dir, build(context.Background(), dir)
	})
	return func(t *testing.T) string {
		t.Helper()
		source, err := seed()
		if err != nil {
			t.Fatalf("build %s repository fixture: %v", name, err)
		}
		dir := nativeChangeTempDir(t)
		if err := os.CopyFS(dir, os.DirFS(source)); err != nil {
			t.Fatalf("copy %s repository fixture: %v", name, err)
		}
		return dir
	}
}

func (e *nativePublicationFixtureExecution) SetPublicationSource(source func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)) {
	pushConfirmationLost := false
	e.ownedExecution.SetPublicationSource(func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
		if e.beforePublication != nil {
			e.beforePublication(opts)
		}
		if e.publicationFault != nil {
			defer e.publicationFault(&opts)()
		}
		opts.GitHubClient = e.forge
		if e.retainedHead != "" {
			opts.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: nativeChangeRepository + "/pull/7"}
			opts.ExternalHeadSHA = e.retainedHead
		}
		if e.losePush != nil {
			effect := opts.PublicationEffect
			opts.PublicationEffect = func(ctx context.Context, kind, state string, publication workspace.GitHubPublication) error {
				if (*e.losePush || pushConfirmationLost) && kind == "git_push" && state == "confirmed" {
					*e.losePush = false
					pushConfirmationLost = true
					return fmt.Errorf("%w: push confirmation lost after remote accepted head", ErrUnavailable)
				}
				return effect(ctx, kind, state, publication)
			}
		}
		return source(ctx, version, opts)
	})
}

// TestNativeRunnerOpensChangeAndLeavesDispatch runs the production runner with
// a fake agent in a real git worktree against a real hub. A run that commits
// opens a Change Request carrying the stored attempt diff and the item moves
// to review; a run that commits nothing opens none and the item leaves the
// dispatchable set, so the claim offers it no more.
func Run(t *testing.T, scenarios []Scenario, factories Constructors, checks Checks) {
	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	newTestClient := func(t testing.TB, config Config) (*Client, error) { return createTestClient(t, config, factories) }

	sourceSeed := nativeChangeSourceRepo(t)

	for _, test := range scenarios {

		t.Run(test.Name, func(t *testing.T) {
			if test.Publication != "head_read" {
				t.Parallel()
			}
			genuinePublication := test.Publication != "" && test.Validator != "" && (test.PublicationFault == "" || test.PublicationFault == "policy")
			review := "In Review"
			states := []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
				{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
			}
			if test.Hosted {
				// The default auto_promote.source_state names the review lane.
				review, states = "Human Review", hubserver.HostedProjectStates()
			}
			if test.Rework {
				review, states = "Human Review", []tracker.NativeState{
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
					{Name: "In Progress", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
					{Name: "Human Review", Transitions: []string{"Rework", "Done"}},
					{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
					{Name: "Done", Terminal: true},
				}
			}
			if test.Land {
				state := 1
				if test.Rework {
					state = 3
				}
				states[state].Transitions = append(states[state].Transitions, "Merging")
				transitions := []string{"Done", review}
				if test.Rework {
					states[2].Transitions = append(states[2].Transitions, "Merging")
					transitions = append(transitions, "Rework")
				}
				states = append(states, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: transitions})
			}
			if test.Reopened {
				states[4].Transitions = []string{"Rework"}
			}
			if test.FinalMessage != "" {
				states[3].Transitions = append(states[3].Transitions, "Blocked")
				states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Rework"}})
			}
			if test.SummaryFailure {
				states[1].Transitions = append(states[1].Transitions, "Blocked")
				states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"In Progress"}})
				if test.Feedback != "" {
					states[1].Transitions = append(states[1].Transitions, "Rework")
					states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", review, "Done"}})
				}
			}
			clock := time.Now
			h := newNativeChangeHubConfigured(t, review, states, true, false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken), Conversation: &hubserver.ConversationConfig{Enabled: true}, Clock: clock}, factories)
			var publicationCI *NativeClient
			if test.Publication != "" {
				previous := h.descriptor.ID
				h.descriptor.Gates.GitHubPullRequest = true
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{Policy: h.descriptor, ExpectedID: previous}); err != nil {
					t.Fatal(err)
				}
			}
			if test.Validator != "" {
				previous := h.descriptor.ID
				h.descriptor.Gates.Validator = true
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
					t.Fatal(err)
				}
			}
			if test.SummaryFailure {
				previous := h.descriptor.ID
				h.descriptor.Gates.AutomatedReview = gate.AutomatedReviewRequired
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
					t.Fatal(err)
				}
			}
			identityPath := ""
			if test.Enrolled {
				identityPath = filepath.Join(t.TempDir(), "private", "runner.json")
				hubURL := strings.Replace(fixtureClient(h.native.Transport()).baseURL.String(), "http://", "https://", 1)
				file, err := hubclientInitializeIdentity(identityPath, hubURL)
				if err != nil {
					t.Fatal(err)
				}
				enrollmentAdmin, err := newTestClient(t, Config{URL: hubURL, TokenSource: func() string { return nativeChangeAdminToken }, HTTPClient: &http.Client{Transport: h.failChanges}})
				if err != nil {
					t.Fatal(err)
				}
				enrollment, err := enrollmentAdmin.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
				if err != nil {
					t.Fatal(err)
				}
				machine := Machine{ID: file.Identity.MachineID, Hostname: "host", DisplayName: "Example runner", Capacity: 1, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
				var identity runnerauth.Identity
				redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version, OS: runtime.GOOS, Architecture: runtime.GOARCH, IsolationTier: machine.IsolationTier, BackendIsolation: machine.BackendIsolation, SpriteName: runnerSpriteName(machine.Hostname)}
				if err := fixtureRunnerRequest(t, enrollmentAdmin, enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &identity); err != nil {
					t.Fatal(err)
				}
				file.Identity = identity
				if err := hubclientSaveIdentity(identityPath, file); err != nil {
					t.Fatal(err)
				}
				fleet, err := NewFleetClient(enrollmentAdmin, h.organization, map[string]tracker.ProjectID{"local": h.project})
				if err != nil {
					t.Fatal(err)
				}
				if err := fleet.UpdateRunner(t.Context(), file.Identity.RunnerID, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Source reviewer", State: "active", CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{h.project}, IsolationTier: isolation.NativeTrusted, Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}}}); err != nil {
					t.Fatal(err)
				}
				client, err := newTestClient(t, Config{URL: hubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}})
				if err != nil {
					t.Fatal(err)
				}
				if h.native, err = client.Native(h.organization, h.project); err != nil {
					t.Fatal(err)
				}
				if h.scheduler, err = newTestScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold}); err != nil {
					t.Fatal(err)
				}
				if !h.scheduler.Transport().Enrolled() {
					t.Fatal("fixture did not enroll the claim owner")
				}
			}
			if test.LocalIntakeOff {
				local, err := orchestrator.New(orchestrator.Config{LocalIntakeDisabled: true, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}}, orchestrator.Dependencies{Connector: fakehub.New(fakehub.Config{Issues: []connector.Issue{{ID: "local-queued", State: "Todo"}}}), Runner: orchestrator.FakeRunner{}})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- local.Run(ctx) }()
				t.Cleanup(func() { cancel(); <-done })
				state, err := local.State(t.Context())
				if err != nil || state.LocalIntake.Enabled || len(state.Running) != 0 {
					t.Fatalf("local intake state: %+v, %v", state.LocalIntake, err)
				}
			}
			if test.Publication != "" {
				publicationCI = h.publicationCI(t)
			}
			var issue connector.Issue
			if test.Interactive {
				var created struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
				}
				if err := h.admin.Transport().Request(t.Context(), http.MethodPost, h.admin.Scope()+"/conversations", map[string]any{"key": "interactive", "title": "Interactive"}, &created); err != nil {
					t.Fatal(err)
				}
				var linked struct {
					Issue tracker.NativeIssue `json:"issue"`
				}
				if err := h.admin.Transport().Request(t.Context(), http.MethodPost, h.admin.Scope()+"/conversations/"+created.Conversation.ID+"/link", map[string]any{"key": "link", "share_history": true, "issue": map[string]any{"title": "Update the README", "description": "Interactive work" + issueContractTestSections}}, &linked); err != nil {
					t.Fatal(err)
				}
				issue = connector.Issue{ID: string(linked.Issue.WorkItemID), Identifier: "native#1", Title: linked.Issue.Title}
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
					t.Fatal(err)
				}
			} else {
				issue = h.createInProgress(t, "Update the README")
			}
			if test.SummaryFailure {
				confirmIssueContract(t, h.admin, issue.ID)
			}
			source := nativeChangeTempDir(t)
			if err := os.CopyFS(source, os.DirFS(sourceSeed)); err != nil {
				t.Fatal(err)
			}
			fixtures := map[string]string{}
			credential := "ghp_" + strings.Repeat("Z", 36)
			if test.SourceFixture != "" {
				for _, name := range []string{"health_email_test.go", "slack_delivery_test.go"} {
					data, err := os.ReadFile(filepath.Join("..", "..", "hubserver", name))
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join("internal", "hubserver", name)
					fixtures[path] = string(data)
					if err := os.MkdirAll(filepath.Join(source, "internal", "hubserver"), 0o700); err != nil {
						t.Fatal(err)
					}
					content := string(data)
					if test.SourceFixture == "inherited" {
						content += "\n" + credential
					}
					if err := os.WriteFile(filepath.Join(source, path), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				nativeChangeGit(t, source, "add", "internal/hubserver")
				nativeChangeGit(t, source, "commit", "-m", "inherited notification fixtures")
			}
			if test.DependencyOnly {
				for name, content := range map[string]string{
					"frontend/package.json": `{"dependencies":{"@radix-ui/themes":"1"}}`,
					".gitignore":            "frontend/.generated/\n.generated/\n",
					".test-bin/pnpm":        "#!/bin/sh\ntest \"$(cat package.json)\" = '{\"dependencies\":{}}' || exit 2\nmkdir -p .generated\nprintf 'pnpm %s passed\\n' \"$1\" | tee .generated/result\n",
					".test-bin/make":        "#!/bin/sh\ntest \"$(cat frontend/package.json)\" = '{\"dependencies\":{}}' || exit 2\nmkdir -p .generated\nprintf 'make %s passed\\n' \"$1\" | tee .generated/result\n",
				} {
					if test.LargeOutput && name == ".test-bin/make" {
						content = strings.Replace(content, "mkdir -p .generated\n", "mkdir -p .generated\nprintf '%070000d\\n' 0\n", 1)
					}
					path := filepath.Join(source, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				nativeChangeGit(t, source, "add", ".")
				nativeChangeGit(t, source, "commit", "-m", "dependency fixture")
			}

			remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
			nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, source, "push", "-u", "origin", "main")
			if test.BrokenSigning {
				nativeChangeGit(t, source, "config", "commit.gpgsign", "true")
				nativeChangeGit(t, source, "config", "gpg.program", filepath.Join(t.TempDir(), "unavailable-signer"))
			}
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			var expected *tracker.ChangeDetail
			var deliveryHeadRef string
			var deliveredTargetHead string
			var candidate connector.Issue
			if test.Rework && !genuinePublication {
				info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
				if err != nil {
					t.Fatal(err)
				}
				reviewedPath := info.Path
				if err := os.WriteFile(filepath.Join(reviewedPath, "PRESERVED.md"), []byte("reviewed source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, reviewedPath, "add", "PRESERVED.md")
				nativeChangeGit(t, reviewedPath, "-c", "commit.gpgsign=false", "commit", "-m", "preserved reviewed source")
				item := tracker.NativeWorkItemID(issue.ID)
				if test.StaleBase {
					if _, err := h.admin.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: "Human hold: preserve the original checkpoint and staged source"}); err != nil {
						t.Fatal(err)
					}
				}
				change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
				if err != nil {
					t.Fatal(err)
				}
				old := h.publish(t, item, change.ID, nativeChangeFixtureHead("a"))
				if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: old.ID, Body: "Historical discussion"}); err != nil {
					t.Fatal(err)
				}
				historicalDecision := "changes_requested"
				if test.Reopened || test.Publication != "" {
					historicalDecision = "approved"
				}
				if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, old.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: historicalDecision, Body: "Historical review"}); err != nil {
					t.Fatal(err)
				}
				if test.Reopened {
					if _, err := h.admin.LandChangeVersion(t.Context(), item, change.ID, old.ID, tracker.LandChangeVersion{Mutation: nativeMutationKey(), MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}); err != nil {
						t.Fatal(err)
					}
					currentIssue, err := h.admin.Issue(t.Context(), item)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: "Rework", Reason: "user_requested"}); err != nil {
						t.Fatal(err)
					}
				}
				head, err := exec.CommandContext(t.Context(), "git", "-C", reviewedPath, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				baseOutput, err := exec.CommandContext(t.Context(), "git", "-C", source, "rev-parse", "HEAD").Output()
				if err != nil {
					t.Fatal(err)
				}
				base := strings.TrimSpace(string(baseOutput))
				capture, err := workspace.CaptureChangeSource(t.Context(), reviewedPath, base, strings.TrimSpace(string(head)))
				if err != nil {
					t.Fatal(err)
				}
				current := h.publishCaptured(t, item, change.ID, strings.TrimSpace(string(head)), &capture, old.ID, base)
				if test.Delivered == "release" {
					deliveryHeadRef = info.Branch
					nativeChangeGit(t, source, "config", "--add", "url."+remote+".insteadOf", nativeChangeRepository+".git")
					nativeChangeGit(t, source, "push", "origin", current.HeadSHA+":refs/heads/release/v1.2", current.HeadSHA+":refs/heads/"+info.Branch)
				}
				if publicationCI != nil {
					check := current.Checks[0]
					if _, err := publicationCI.SubmitChangeCheck(t.Context(), item, change.ID, current.ID, tracker.SubmitChangeCheck{Mutation: nativeMutationKey(), ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: current.HeadSHA, RunID: current.RunID, PolicyID: current.PolicyID, ConfigDigest: current.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: current.CreatedAt, Evidence: []tracker.ChangeArtifact{{Kind: "test", URI: "s3://fixture/test", SHA256: policy.Digest([]byte("test receipt")), Availability: "available"}}}}); err != nil {
						t.Fatal(err)
					}
				}

				if test.AdvanceTarget || test.Delivered == "exact" {
					nativeChangeGit(t, source, "merge", "--ff-only", current.HeadSHA)
					nativeChangeGit(t, source, "push", "origin", "main")
				}
				if test.Absorbed {
					nativeChangeGit(t, source, "merge", "--squash", current.HeadSHA)
					nativeChangeGit(t, source, "commit", "-m", "absorb reviewed source")
					nativeChangeGit(t, source, "push", "origin", "main")
				}
				if test.Formal {
					if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, current.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "changes_requested", Body: "Serialize global config updates"}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: current.ID, Body: "Global config loses concurrent updates"}); err != nil {
					t.Fatal(err)
				}
				currentIssue, err := h.admin.Issue(t.Context(), item)
				if err != nil {
					t.Fatal(err)
				}
				if test.StaleBase {
					for _, lane := range []string{"Human Review", "Merging"} {
						currentIssue, err = h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: lane, Reason: "user_requested"})
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(source, "UPSTREAM.md"), []byte("unrelated work\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, source, "add", "UPSTREAM.md")
					nativeChangeGit(t, source, "commit", "-m", "parallel landing")
					nativeChangeGit(t, source, "push", "origin", "main")
					freshOutput, err := exec.CommandContext(t.Context(), "git", "-C", source, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					freshBase := strings.TrimSpace(string(freshOutput))
					info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
					if err != nil {
						t.Fatal(err)
					}
					local, err := backend.(workspace.RecoveryStateProvider).RecoveryState(t.Context(), info, workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
					if err != nil {
						t.Fatal(err)
					}
					checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: local.HeadSHA, WorkspaceDigest: local.WorkspaceFingerprint, ExternalEffect: "none", EffectState: "none"}
					var fencing tracker.FencingToken
					for i := range 105 {
						candidates := h.candidatesIn(t, "Merging")
						if len(candidates) != 1 {
							t.Fatalf("reviewed unlanded version lost its claim at retry %d: %+v", i, candidates)
						}
						if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
							t.Fatal(err)
						}
						landing := h.scheduler.RunExecution(issue.ID)
						compact, err := json.Marshal(landing.Recovery())
						if err != nil || len(compact) > 100000 || len(landing.Recovery().Attempts) > 4 || len(landing.Recovery().History) != 0 || !strings.Contains(string(compact), "Human hold: preserve the original checkpoint") {
							t.Fatalf("historic retries inflated a fresh claim or lost human context: bytes=%d, error=%v", len(compact), err)
						}
						if i == 104 {
							t.Logf("fresh recovery after 104 historical attempts: %d bytes, %d attempts, %d comments, %d history events", len(compact), len(landing.Recovery().Attempts), len(landing.Recovery().Discussion), len(landing.Recovery().History))
						}
						if _, err := h.native.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: strings.Repeat("Historical worker output\n", 900)}); err != nil {
							t.Fatal(err)
						}
						guarded, stop, err := landing.Guard(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						if landing.Recovery().Lease.FencingToken <= fencing {
							t.Fatal("repeated landing reused released authority")
						}
						if i > 0 {
							previous := landing.Recovery().Attempts[len(landing.Recovery().Attempts)-1]
							if previous.Runtime == nil || previous.Runtime.Landing == nil || previous.Runtime.Landing.BaseSHA != freshBase || previous.Runtime.Landing.VersionID != current.ID || previous.Runtime.Landing.HeadSHA != current.HeadSHA || previous.Checkpoint == nil || previous.Checkpoint.HeadSHA != checkpoint.HeadSHA || previous.Checkpoint.WorkspaceDigest != checkpoint.WorkspaceDigest {
								t.Fatalf("landing continuation lost owned source receipt: %+v", previous)
							}
						}
						fencing = landing.Recovery().Lease.FencingToken
						if err := landing.(runner.LandingRuntimeExecution).StartLanding(guarded, int64(655+i), 0); err != nil {
							t.Fatal(err)
						}
						refusal := runner.NativeLanding{ChangeID: change.ID, VersionID: current.ID, HeadSHA: current.HeadSHA, BaseSHA: freshBase, RefusalKind: workspace.LandRefusalBaseMoved}
						if err := landing.(runner.LandingRuntimeExecution).ObserveLanding(guarded, refusal); err != nil {
							t.Fatal(err)
						}
						if err := landing.Checkpoint(guarded, checkpoint); err != nil {
							t.Fatal(err)
						}
						if err := landing.Finish(guarded, "succeeded"); err != nil {
							t.Fatal(err)
						}
						if i == 104 {
							if err := h.connector.UpdateIssueState(guarded, issue.ID, "Rework"); err != nil {
								t.Fatal(err)
							}
						}
						if err := h.scheduler.ReleaseClaim(guarded, issue.ID, "completed"); err != nil {
							t.Fatal(err)
						}
						stop()
					}
				} else {
					for _, state := range []string{"Human Review", "Rework"} {
						currentIssue, err = h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: state, Reason: "user_requested"})
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				detail, err := h.admin.Change(t.Context(), item, change.ID)
				if err != nil {
					t.Fatal(err)
				}
				expected = &detail
				if test.Absorbed || test.Delivered != "" {
					target := "main"
					if test.Delivered == "release" {
						target = "release/v1.2"
					}
					head, err := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+target).Output()
					if err != nil {
						t.Fatal(err)
					}
					deliveredTargetHead = strings.TrimSpace(string(head))
				}
				if test.FailDetail {
					next := fixtureClient(h.native.Transport()).requests.Load()
					fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodGet && (strings.HasSuffix(request.URL.Path, "/changes/"+change.ID) || request.URL.Query().Get("view") == "recovery") {
							response := httptest.NewRecorder()
							response.WriteHeader(http.StatusForbidden)
							return response.Result(), nil
						}
						return next.RoundTrip(request)
					}))
				}
				candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Rework"}})
				if test.FailDetail {
					var failure *APIError
					if !errors.As(err, &failure) || failure.Status != http.StatusForbidden || len(candidates) != 0 || len(h.scheduler.ActiveExecutions()) != 0 {
						t.Fatalf("failed hydration dispatched work: candidates=%v, error=%v", candidates, err)
					}
					if state := h.state(t, issue.ID); state != "Rework" {
						t.Fatalf("hydration failure changed lane to %s", state)
					}
					fixtureClient(h.native.Transport()).requests.Store(h.failChanges)
					candidates, err = h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Rework"}})
					if err != nil || len(candidates) != 1 {
						t.Fatalf("failed hydration did not release claim: candidates=%v, error=%v", candidates, err)
					}
					return
				}
				if err != nil || len(candidates) != 1 {
					t.Fatalf("Rework candidates = %v, %v", candidates, err)
				}
				candidate = candidates[0]
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
					t.Fatal(err)
				}
			} else {
				candidate = h.claim(t, issue.ID)
			}
			if test.LateConflict {
				info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{info.Path, source} {
					content := "preserved staged work\n"
					if path == source {
						content = "parallel source\n"
					}
					if err := os.WriteFile(filepath.Join(path, "CHANGE.md"), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, path, "add", "CHANGE.md")
				}
				nativeChangeGit(t, source, "commit", "-m", "parallel source")
				nativeChangeGit(t, source, "push", "origin", "main")
			}
			execution := h.scheduler.RunExecution(issue.ID)
			if execution == nil {
				t.Fatal("claimed native issue has no execution lifecycle")
			}
			if test.Delivered == "release" {
				current := expected.Versions[len(expected.Versions)-1]
				execution = replaceRecovery(t, h, execution, &tracker.ReleaseDelivery{ReleasePickInput: tracker.ReleasePickInput{Repository: current.Repository, PRNumber: 7, BaseRef: "release/v1.2", BaseSHA: current.BaseSHA, HeadSHA: current.HeadSHA, HeadRef: deliveryHeadRef, SourceMergeSHA: current.HeadSHA}, ChangeID: expected.Change.ID, VersionID: current.ID}, factories)
				next := fixtureClient(h.native.Transport()).requests.Load()
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/release-pick") {
						status := tracker.ReleaseDeliveryStatus{ReleasePickInput: execution.Recovery().Issue.ReleaseDelivery.ReleasePickInput, ReleaseHeadSHA: current.HeadSHA}
						raw, err := json.Marshal(status)
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw))), Request: request}, err
					}
					return next.RoundTrip(request)
				}))
			}
			if test.Enrolled {
				h.connector.worker, err = NewNativeConnector(h.native)
				if err != nil {
					t.Fatal(err)
				}
			}
			provider := &committingAgent{finalMessage: test.FinalMessage, dependencyOnly: test.DependencyOnly, failedCheck: test.FailedCheck, commit: test.Commit, dirty: test.Dirty, staged: test.Staged || genuinePublication, validator: test.Validator, lowScore: test.LowScore, complete: test.Absorbed || test.Delivered != "" || test.SummaryFailure, hold: test.Hold, inProgress: test.LateConflict}
			if test.SourceFixture != "" {
				provider.duringTurn = func() {
					for path, original := range fixtures {
						file := filepath.Join(provider.workspace, path)
						if test.SourceFixture == "history" {
							if err := os.WriteFile(file, []byte(original+"\n"+credential), 0o600); err != nil {
								t.Fatal(err)
							}
							nativeChangeGit(t, provider.workspace, "add", path)
							nativeChangeGit(t, provider.workspace, "commit", "-m", "unsafe intermediate fixture")
						}
						content := original + "\n"
						if test.SourceFixture == "inherited" || test.SourceFixture == "introduced" {
							content += credential + "\n"
						}
						if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
							t.Fatal(err)
						}
						nativeChangeGit(t, provider.workspace, "add", path)
					}
				}
			}
			if test.Publication == "confirmed" {
				provider.duringValidation = func(request runner.AgentTurnRequest) {
					t.Helper()
					owner := execution.(*ownedExecution)
					if !request.ReadOnly || owner.CompletionSnapshot().Publication == nil || owner.CompletionData().Outcome != "" || owner.CompletionData().Finalization != nil {
						t.Fatal("acceptance must read confirmed publication before attempt completion")
					}
					validation, err := owner.ValidatorVersion(t.Context())
					if err != nil || validation.Version == nil || validation.Version.External == nil || *validation.Version.External != owner.CompletionSnapshot().Publication.External || validation.Version.ID != owner.NativeChange().VersionID || validation.Version.HeadSHA != owner.CompletionSnapshot().Publication.HeadSHA {
						t.Fatalf("acceptance lacks the exact confirmed current PR binding: %+v error=%v", validation.Version, err)
					}

				}
			}
			var targetHead string
			if test.AdvanceTarget {
				provider.duringTurn = func() {
					if err := os.WriteFile(filepath.Join(source, "UPSTREAM.md"), []byte("unrelated work\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, source, "add", "UPSTREAM.md")
					nativeChangeGit(t, source, "commit", "-m", "unrelated target work")
					nativeChangeGit(t, source, "push", "origin", "main")
					head, err := exec.CommandContext(t.Context(), "git", "-C", source, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					targetHead = strings.TrimSpace(string(head))
				}
			}
			var providerStartHead string
			if test.Delivered != "" {
				beforeTurn := provider.duringTurn
				provider.duringTurn = func() {
					head, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					providerStartHead = strings.TrimSpace(string(head))
					if beforeTurn != nil {
						beforeTurn()
					}
				}
			}
			if test.Reconnect {
				provider.duringLiveTurn = func(request runner.AgentTurnRequest, update runner.AgentUpdateHandler) {
					ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
					defer cancel()
					command := exec.CommandContext(ctx, "sh", "-c", "read release")
					input, err := command.StdinPipe()
					if err != nil {
						t.Fatal(err)
					}
					defer input.Close()
					if err := command.Start(); err != nil {
						t.Fatal(err)
					}
					defer func() {
						_, _ = io.WriteString(input, "release\n")
						if err := command.Wait(); err != nil {
							t.Error(err)
						}
					}()
					if err := update(runner.AgentUpdate{Type: runner.AgentUpdateToolStarted, ThreadID: "thread-native", TurnID: "turn-1", ItemID: "validation", Tool: "commandExecution", Command: "long validation"}); err != nil {
						t.Fatal(err)
					}
					var before struct {
						Conversation struct {
							ID        string                 `json:"id"`
							Execution conversation.Execution `json:"execution"`
						} `json:"conversation"`
					}
					testenv.WaitUntil(t, ctx, func() error {
						if err := h.admin.Transport().Request(ctx, http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/conversation", nil, &before); err != nil {
							t.Fatal(err)
						}
						if before.Conversation.Execution.Owner.TurnID != "turn-1" {
							return errors.New("provider turn was not reported")
						}
						return nil
					})
					h.failChanges.controlsFailed = make(chan int, 2)
					h.failChanges.controlFailures.Store(2)
					for range 2 {
						select {
						case <-h.failChanges.controlsFailed:
						case <-ctx.Done():
							t.Fatal("worker did not retry the transport failure")
						}
					}
					h.restart()
					h.failChanges.controlFailures.Store(0)
					after := before
					if err := h.admin.Transport().Request(ctx, http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/conversation", nil, &after); err != nil {
						t.Fatal(err)
					}
					if after.Conversation.Execution.Owner != before.Conversation.Execution.Owner || after.Conversation.Execution.Status != conversation.ExecutionRunning || after.Conversation.Execution.Capabilities != before.Conversation.Execution.Capabilities {
						t.Fatalf("restart lost current worker: before=%+v after=%+v", before.Conversation.Execution, after.Conversation.Execution)
					}
					var receipt conversation.Receipt
					if err := h.admin.Transport().Request(ctx, http.MethodPost, h.admin.Scope()+"/conversations/"+after.Conversation.ID+"/commands", conversation.Command{Key: "reconnected-steer", Kind: conversation.CommandMessage, Text: "Keep validating", Expected: conversation.Expected{AttemptID: after.Conversation.Execution.Owner.AttemptID, TurnID: "turn-1"}}, &receipt); err != nil {
						t.Fatal(err)
					}
					select {
					case control := <-request.ConversationControl.Commands:
						if control.ThreadID != "thread-native" || control.TurnID != "turn-1" || control.Text != "Keep validating" {
							t.Fatalf("control lost provider identity: %+v", control)
						}
						if err := control.Check(ctx); err != nil {
							t.Fatal(err)
						}
						control.Reply <- nil
					case <-ctx.Done():
						t.Fatal("original worker did not resume controls")
					}
					if err := update(runner.AgentUpdate{Type: runner.AgentUpdateToolCompleted, ThreadID: "thread-native", TurnID: "turn-1", ItemID: "validation", Tool: "commandExecution", Status: "completed"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			validationCommand := "true"
			prerequisite := ""
			if test.Publication == "confirmed" {
				validationCommand = "test -f CHANGE.md"
				if test.Validator == "rework" {
					prerequisite = filepath.Join(t.TempDir(), "validator-ready")
					provider.validatorCommand = "test -f '" + strings.ReplaceAll(prerequisite, "'", "'\\''") + "'"
				}
			}
			if test.DependencyOnly {
				validationCommand = `PATH="$PWD/.test-bin:$PATH" make check-fast`
				candidate.Description = "Remove the unused direct @radix-ui/themes dependency. Required validation: frontend pnpm build, frontend pnpm check, root make check-fast." + issueContractTestSections
			}
			gateMarker := filepath.Join(t.TempDir(), "gate-invoked")
			if test.FinalMessage != "" {
				validationCommand = "printf gate > '" + strings.ReplaceAll(gateMarker, "'", "'\"'\"'") + "'; exit 2"
			}
			automatedReview := ""
			if test.SummaryFailure {
				automatedReview = gate.AutomatedReviewRequired
			}
			var runLogs bytes.Buffer
			t.Cleanup(func() {
				if t.Failed() {
					t.Log(runLogs.String())
				}
			})
			dependencies := runner.Dependencies{Store: api.NewRunnerSessions(h.native), ProcessSource: emptyProcessSource,
				Logger:       slog.New(slog.NewTextHandler(&runLogs, nil)),
				ProjectID:    "local",
				Workflow:     config.Workflow{Config: config.Config{Gate: gate.Config{Run: validationCommand, AutomatedReview: automatedReview, Validator: gate.ValidatorConfig{Enabled: test.Validator != ""}}}, Prompt: "Complete the issue"},
				Workspace:    backend,
				AgentBackend: provider,
				FinalizationContext: func(ctx context.Context, _ time.Duration, _ error) (context.Context, context.CancelFunc) {
					return context.WithCancel(testenv.WaitContext(t, ctx))
				},
				ReapWorkspaceProcesses: func(context.Context, string, time.Duration) (int, error) {
					return 0, nil
				},
			}
			agent, err := runner.NewRunner(dependencies)
			if err != nil {
				t.Fatal(err)
			}
			if test.FailVersion {
				// A transport outage is deferred, not a definitive refusal. This
				// case exercises the existing review handoff for a refused version.
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") {
						return &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"invalid_request","message":"version publication refused"}`)), Request: request}, nil
					}
					return h.failChanges.RoundTrip(request)
				}))
			}
			runExecution := execution
			stopPublicationGuard := func() {}
			if test.SSH {
				remote, closePeers := nativeSSHExecution(t, t.Context(), execution, t.TempDir())
				defer closePeers()
				runExecution = remote
			}
			var oldPublicationHead, oldPublicationBranch string
			var forge *github.Client
			var forgeCreates int
			var forgePull string
			var publicationRetry bool
			losePush := test.Publication == "lost_push"
			if test.Publication != "" {
				forge, err = github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body := "[]"
					switch request.Method {
					case http.MethodGet:
						if test.Publication != "confirmed" && !publicationRetry && (forgeCreates > 0 || test.Publication == "lost_push" && !losePush) {
							return nil, fmt.Errorf("%w: forge observation unavailable after lost reply", ErrUnavailable)
						}
						if publicationRetry && test.PublicationFault == "unavailable_forge" {
							return nil, fmt.Errorf("%w: forge unavailable", ErrUnavailable)
						}
						observedPull := forgePull
						if publicationRetry {
							switch test.PublicationFault {
							case "missing_pr":
								observedPull = ""
							case "wrong_base":
								observedPull = strings.ReplaceAll(observedPull, `"ref":"main"`, `"ref":"wrong-base"`)
							case "wrong_repository":
								observedPull = strings.ReplaceAll(observedPull, "example/native-change", "foreign/repository")
							case "wrong_head":
								observedPull = strings.ReplaceAll(observedPull, oldPublicationHead, strings.Repeat("d", 40))
							case "wrong_branch":
								observedPull = strings.ReplaceAll(observedPull, oldPublicationBranch, "foreign-branch")
							}
						}
						if observedPull != "" {
							body = "[" + observedPull + "]"
							if publicationRetry && test.PublicationFault == "multiple_prs" {
								body = "[" + observedPull + "," + strings.Replace(observedPull, `"number":7`, `"number":8`, 1) + "]"
							}
							if strings.HasSuffix(request.URL.Path, "/pulls/7") {
								body = observedPull
							}
						}
					case http.MethodPost:
						forgeCreates++
						headOutput, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD").Output()
						if err != nil {
							t.Fatal(err)
						}
						branchOutput, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "symbolic-ref", "--short", "HEAD").Output()
						if err != nil {
							t.Fatal(err)
						}
						forgePull = fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/native-change"}},"base":{"ref":"main","repo":{"full_name":"example/native-change"}}}`, strings.TrimSpace(string(headOutput)), strings.TrimSpace(string(branchOutput)))
						if forgeCreates == 1 && test.Publication == "lost_create" {
							return nil, errors.New("create reply lost after forge accepted PR")
						}
						body = forgePull
					default:
						t.Fatalf("publication attempted forbidden forge operation: %s %s", request.Method, request.URL.Path)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				runExecution = &nativePublicationFixtureExecution{ownedExecution: execution.(*ownedExecution), forge: forge, losePush: &losePush, keepGuard: func(stop func()) {
					stopPublicationGuard = sync.OnceFunc(stop)
					t.Cleanup(stopPublicationGuard)
				}}
			}
			runCtx, cancelRun := context.WithCancelCause(t.Context())
			defer cancelRun(context.Canceled)
			if test.Expired {
				provider.afterTurn = func() { cancelRun(context.DeadlineExceeded) }
			}
			transport := fixtureClient(h.native.Transport()).requests.Load()
			var postedSource atomic.Bool
			if test.SummaryFailure {
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if postedSource.Load() && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("summary unavailable")), Request: request}, nil
					}
					response, err := transport.RoundTrip(request)
					if err == nil && response != nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/reviews") && response.StatusCode >= 200 && response.StatusCode < 300 {
						postedSource.Store(true)
					}
					return response, err
				}))
			}
			var editedComment tracker.NativeComment
			if test.Feedback == "edit" {
				var err error
				editedComment, err = h.native.CreateComment(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateComment{Mutation: nativeMutationKey(), Body: "Earlier runner context."})
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.InitialSourceWait {
				provider.sourceVerdict = "wait"
			}
			var publicationRefsBefore []byte
			if test.Publication == "head_read" {
				runExecution.(*nativePublicationFixtureExecution).beforePublication = func(options workspace.LandOptions) {
					if options.UnresolvedEffect {
						t.Fatal("first publication already has an uncertain checkpoint")
					}
					publicationRefsBefore, err = exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)").Output()
					if err != nil {
						t.Fatal(err)
					}
					realGit, lookupErr := exec.LookPath("git")
					if lookupErr != nil {
						t.Fatal(lookupErr)
					}
					wrapper := t.TempDir()
					script := "#!/bin/sh\nread_ref=0\nfor arg in \"$@\"; do if [ \"$arg\" = ls-remote ]; then read_ref=1; fi; if [ \"$read_ref\" = 1 ] && [ \"$arg\" = HEAD ]; then exit 1; fi; done\nexec '" + realGit + "' \"$@\"\n"
					if writeErr := os.WriteFile(filepath.Join(wrapper, "git"), []byte(script), 0o700); writeErr != nil {
						t.Fatal(writeErr)
					}
					t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))
				}
			}
			var implementationSession atomic.Int64
			request := runner.RunRequest{Execution: runExecution, DeferExecutionFinish: test.FailVersion || test.SummaryFailure, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement, OnUsageUpdate: func(update runner.UsageUpdate) error {
				if test.SummaryFailure && update.DetentSessionID > 0 {
					implementationSession.CompareAndSwap(0, update.DetentSessionID)
				}
				return nil
			}}
			var result runner.RunResult
			if test.Delivered != "" {
				result, err = runNativeCompletionOrchestrator(t, h, agent, request)
			} else {
				result, err = agent.Run(runCtx, request)
			}
			if test.Publication == "head_read" {
				owner := execution.(*ownedExecution)
				if !nativeTransportUnavailable(err) || owner.Settled() || owner.CompletionData().Outcome != "" || owner.CompletionData().Handoff.UncertainForgeEffect() || owner.CompletionSnapshot().Publication != nil || forgeCreates != 0 || forgePull != "" {
					t.Fatalf("HEAD discovery failure lost infrastructure retry: settled=%t outcome=%q checkpoint=%+v error=%v", owner.Settled(), owner.CompletionData().Outcome, owner.CompletionData().Handoff, err)
				}
				if after, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)").Output(); readErr != nil || string(after) != string(publicationRefsBefore) {
					t.Fatalf("HEAD discovery failure wrote remote refs: before=%s after=%s", publicationRefsBefore, after)
				}
				return
			}
			if test.SummaryFailure {
				fixtureClient(h.native.Transport()).requests.Store(transport)
				owner := execution.(*ownedExecution)
				var producerErr *runner.NativeImplementationReviewError
				change := result.NativeChange
				if !postedSource.Load() || !errors.As(err, &producerErr) || change == nil || change.VersionID == "" || change.ImplementationReviewError == "" || owner.CompletionData().Outcome != "" || provider.calls != 2 {
					t.Fatalf("deferred producer failure lost acknowledged source: result=%+v turns=%d prepared=%q finished=%q error=%v", result, provider.calls, owner.CompletionSnapshot().PreparedOutcome, owner.CompletionData().Outcome, err)
				}
				item := tracker.NativeWorkItemID(issue.ID)
				original, err := h.admin.Change(t.Context(), item, change.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				wantDecision := "approved"
				if test.InitialSourceWait {
					wantDecision = "commented"
				}
				if len(original.Versions) != 1 || len(original.Reviews) != 1 || original.Reviews[0].Decision != wantDecision {
					t.Fatalf("source POST was not durable: %+v", original)
				}
				restorePreparedOwner := func(owner *ownedExecution, savedIssue connector.Issue) *ownedExecution {
					transitionAttempts := 0
					ownerClient := owner.claim.source.NativeClient().Transport()
					ownerTransport := fixtureClient(ownerClient).requests.Load()
					ownerConnector, err := NewNativeConnector(owner.claim.source.NativeClient())
					if err != nil {
						t.Fatal(err)
					}
					fixtureClient(ownerClient).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/workflow") {
							transitionAttempts++
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("completion unavailable")), Request: request}, nil
						}
						return ownerTransport.RoundTrip(request)
					}))
					if err := ownerConnector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err == nil || transitionAttempts == 0 {
						t.Fatalf("completion outage did not reach the tracker transition: requests=%d error=%v", transitionAttempts, err)
					}
					fixtureClient(ownerClient).requests.Store(ownerTransport)
					saved := owner.CompletionState()
					var decoded nativeCompletionState
					if err := json.Unmarshal(saved, &decoded); err != nil || decoded.PreparedOutcome != "interrupted" || decoded.Data.Outcome != "" || decoded.Pending != nil {
						t.Fatalf("deferred interruption capture was not before first Finish: %+v error=%v", decoded.Data, err)
					}
					wantFailure, err := json.Marshal(owner.CompletionSnapshot().PreparedFailure)
					if err != nil {
						t.Fatal(err)
					}
					storedFailure, err := json.Marshal(decoded.PreparedFailure)
					if err != nil || string(storedFailure) != string(wantFailure) {
						t.Fatalf("capture lost actual prepared terminal failure: want=%s got=%s error=%v", wantFailure, storedFailure, err)
					}
					if decoded.Data.Finalization != nil || decoded.Data.Disposition != nil || decoded.Data.TerminalFailure != nil || decoded.PreparedFinalization == nil || !decoded.PreparedFinalization.Settled || decoded.PreparedFinalization.ChangeID != change.ChangeID || decoded.PreparedFinalization.VersionID != change.VersionID || decoded.PreparedFinalization.HeadSHA != change.HeadSHA {
						t.Fatalf("capture mixed prepared completion evidence into live run data: %+v finalization=%+v", decoded.Data, decoded.PreparedFinalization)
					}
					machine := fixtureSchedulerMachine(h.scheduler)
					h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: source}, savedIssue, saved); err != nil {
						t.Fatal(err)
					}
					h.connector.worker = ownerConnector
					return h.scheduler.RunExecution(issue.ID).(*ownedExecution)
				}
				if test.RestartCompletion {
					owner = restorePreparedOwner(owner, candidate)
				}
				oldLease := owner.Recovery().Lease
				if test.Feedback == "bot" {
					if _, err := h.native.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: "CHANGE.md must contain the repaired feedback value before this source can be accepted."}); err != nil {
						t.Fatal(err)
					}
				}
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err != nil {
					t.Fatal(err)
				}
				if test.LegacyReleaseRestart {
					if err := owner.Finish(t.Context(), "interrupted"); err != nil || owner.CompletionData().Outcome != "interrupted" || owner.CompletionData().Disposition == nil || owner.CompletionData().Disposition.Status != "complete" || owner.CompletionData().Finalization == nil || !owner.CompletionData().Finalization.Settled {
						t.Fatalf("legacy release retry lacks acknowledged complete interruption: %+v error=%v", owner.CompletionData(), err)
					}
					before := owner
					originalCompletion, err := json.Marshal([]any{owner.CompletionData().Finalization, owner.CompletionData().Disposition, owner.CompletionData().TerminalFailure, owner.CompletionData().Outcome})
					if err != nil {
						t.Fatal(err)
					}
					var originalData []byte
					ownerClient := owner.claim.source.NativeClient().Transport()
					transport := fixtureClient(ownerClient).requests.Load()
					releaseAttempts := 0
					fixtureClient(ownerClient).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release") {
							releaseAttempts++
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("release unavailable")), Request: request}, nil
						}
						return transport.RoundTrip(request)
					}))
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err == nil || releaseAttempts == 0 {
						t.Fatalf("acknowledged interruption did not reach release outage: requests=%d error=%v", releaseAttempts, err)
					}
					fixtureClient(ownerClient).requests.Store(transport)
					for restart := range 2 {
						var saved json.RawMessage
						if restart > 0 {
							saved = owner.CompletionState()
						}
						machine := fixtureSchedulerMachine(h.scheduler)
						h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: source}, candidate, saved); err != nil {
							t.Fatal(err)
						}
						owner = h.scheduler.RunExecution(issue.ID).(*ownedExecution)
						if owner.CompletionData().RunID != before.CompletionData().RunID || owner.CompletionData().AttemptID != before.CompletionData().AttemptID || owner.CompletionData().PolicyID != before.CompletionData().PolicyID || owner.CompletionData().Identity == nil || before.CompletionData().Identity == nil || *owner.CompletionData().Identity != *before.CompletionData().Identity || !sameCompletionLease(owner.claim.lease, before.claim.lease) {
							t.Fatal("acknowledged completion restart changed run authority")
						}
						actualData, err := json.Marshal(owner.CompletionData())
						if restart == 0 {
							actualCompletion, completionErr := json.Marshal([]any{owner.CompletionData().Finalization, owner.CompletionData().Disposition, owner.CompletionData().TerminalFailure, owner.CompletionData().Outcome})
							if completionErr != nil || string(actualCompletion) != string(originalCompletion) {
								t.Fatalf("legacy restore lost actual recorded completion: want=%s got=%s error=%v", originalCompletion, actualCompletion, completionErr)
							}
							originalData = actualData
						}
						if err != nil || string(actualData) != string(originalData) {
							t.Errorf("restart %d changed acknowledged completion data: want=%s got=%s error=%v", restart+1, originalData, actualData, err)
						}
					}
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, owner.CompletionData().AttemptID)
				if err != nil || evidence.Attempt == nil || evidence.Attempt.Finalization == nil {
					t.Fatalf("released claim has no authoritative finalization: %+v error=%v", evidence.Attempt, err)
				}
				attempt, finalization := evidence.Attempt, evidence.Attempt.Finalization
				if attempt.Status != "interrupted" || attempt.Disposition == nil || attempt.Disposition.Status != "complete" || attempt.TerminalFailure == nil || !finalization.Changed || finalization.Reviewed || finalization.ChangeID != change.ChangeID || finalization.VersionID != change.VersionID || finalization.HeadSHA != change.HeadSHA || attempt.PolicyID != h.descriptor.ID {
					t.Errorf("claim release lost pending technical evidence: attempt=%+v finalization=%+v", attempt, finalization)
				}
				if test.Enrolled {
					identity, err := h.scheduler.Transport().RunnerIdentity(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					var routing runnerauth.Runner
					path := "/api/v2/organizations/" + string(h.organization) + "/runners/" + identity.RunnerID + "/routing"
					if err := h.scheduler.Transport().Request(t.Context(), http.MethodGet, path, nil, &routing); err != nil {
						t.Fatal(err)
					}
					if routing.Used != 0 || routing.HostUsed != 0 || len(routing.Leases) != 0 {
						t.Errorf("settled source evidence hold retained runner capacity: used=%d host_used=%d leases=%+v", routing.Used, routing.HostUsed, routing.Leases)
					}
					if _, err := h.native.Renew(t.Context(), oldLease, 90); !nativeLeaseLost(err) {
						t.Errorf("settled source evidence hold retained renewable authority: %v", err)
					}
					client, err := newTestClient(t, Config{URL: fixtureClient(h.native.Transport()).baseURL.String(), IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}})
					if err != nil {
						t.Fatal(err)
					}
					machine := fixtureSchedulerMachine(h.scheduler)
					h.scheduler, err = newTestScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
					if err != nil {
						t.Fatal(err)
					}
				}
				if held := h.candidates(t); len(held) != 0 {
					t.Errorf("reconnect reoffered technical evidence hold with retained authority: candidates=%+v", held)
					return
				}
				if test.CompletedContinue {
					current, err := h.admin.Issue(t.Context(), item)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}); err != nil {
						t.Fatal(err)
					}
					bare := h.claim(t, issue.ID)
					beforeCalls, beforeWritable := provider.calls, provider.writableCalls
					wantSeedCalls := beforeCalls
					if test.Validator != "" {
						wantSeedCalls++
					}
					oldLease = h.scheduler.RunExecution(issue.ID).Recovery().Lease
					completed, err := agent.Run(t.Context(), runner.RunRequest{Execution: h.scheduler.RunExecution(issue.ID), DeferExecutionFinish: true, ProjectID: "local", Issue: bare, Mode: runner.RunModeImplement})
					if err != nil || completed.NativeChange == nil || !completed.NativeChange.Reviewed || completed.TurnStarted || provider.writableCalls != beforeWritable || provider.calls != wantSeedCalls {
						t.Fatalf("bare evidence resume did not complete reviews without coding: result=%+v calls=%d->%d error=%v", completed, beforeCalls, provider.calls, err)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					original, err = h.admin.Change(t.Context(), item, change.ChangeID)
					if err != nil {
						t.Fatal(err)
					}
				}
				feedbackBody := "CHANGE.md must contain the repaired feedback value before this source can be accepted."
				feedbackOrigin := &tracker.Provenance{Provider: "github", ExternalID: "feedback-original", AuthorID: "feedback-author", CreatedAt: time.Unix(100, 0), UpdatedAt: time.Unix(100, 0), ObservedAt: time.Unix(100, 0)}
				feedbackRework := test.Feedback == "change" || test.Feedback == "issue" || test.Feedback == "edit"
				freshSourceReview := test.InitialSourceWait || test.Feedback != "" && test.Feedback != "bot"
				if test.Feedback == "technical" {
					feedbackBody = "The host evidence is now available; independently check it without changing source."
				}
				var feedbackRecord any
				switch test.Feedback {
				case "change", "technical":
					feedbackRecord, err = h.admin.DiscussChange(t.Context(), item, change.ChangeID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: change.VersionID, Body: feedbackBody, Provenance: feedbackOrigin})
				case "issue":
					feedbackRecord, err = h.admin.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: feedbackBody, Provenance: feedbackOrigin})
				case "edit":
					feedbackRecord, err = h.admin.UpdateComment(t.Context(), item, editedComment.ID, tracker.UpdateComment{Mutation: nativeMutationKey(), ExpectedRevision: editedComment.Revision, Body: feedbackBody})
				case "bot":
					err = nil
				}
				if err != nil {
					t.Fatal(err)
				}
				provider.sourceFeedback = ""
				provider.sourceVerdict = "pass"
				if test.Feedback != "" && test.Feedback != "bot" {
					provider.sourceFeedback = feedbackBody
					raw, err := json.Marshal(feedbackRecord)
					if err != nil {
						t.Fatal(err)
					}
					provider.sourceRecord = string(raw)
				}
				if feedbackRework {
					provider.sourceVerdict = "rework"
				}
				current, err := h.admin.Issue(t.Context(), item)
				if err != nil {
					t.Fatal(err)
				}
				var continued runnerSnapshot
				continueText := "Explain the retained source and next evidence step."
				if test.ContinueMessage {
					if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/conversation", nil, &continued); err != nil {
						t.Fatal(err)
					}
					var receipt conversation.Receipt
					if err := h.admin.Transport().Request(t.Context(), http.MethodPost, h.admin.Scope()+"/conversations/"+continued.Conversation.ID+"/commands", conversation.Command{Key: "continue-evidence", Kind: conversation.CommandContinue, Text: continueText, Expected: conversation.Expected{AttemptID: continued.attempt()}}, &receipt); err != nil || receipt.Status != conversation.DeliverySaved {
						t.Fatalf("authenticated Continue was not saved: %+v error=%v", receipt, err)
					}
				}
				if current.State != "In Progress" {
					if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}); err != nil {
						t.Fatal(err)
					}
				}
				if test.Enrolled {
					source := h.scheduler.ExecutionSource("local")
					if source == nil {
						t.Fatal("reconnected source owner is missing")
					}
					if err := source.NativeClient().HeartbeatMachine(t.Context(), fixtureSchedulerMachine(h.scheduler)); err != nil {
						t.Fatal(err)
					}
				}
				resume := h.claim(t, issue.ID)
				resumedExecution := h.scheduler.RunExecution(issue.ID)
				if resumedExecution == nil || resumedExecution.Recovery().Lease.ID == oldLease.ID || resumedExecution.Recovery().Lease.FencingToken <= oldLease.FencingToken {
					t.Fatal("explicit resume did not receive fresh execution authority")
				}
				freshRoot := filepath.Join(nativeChangeTempDir(t), "review-resume")
				freshBackend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: freshRoot, SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				dependencies.Workspace = freshBackend
				resumeRunner, err := runner.NewRunner(dependencies)
				if err != nil {
					t.Fatal(err)
				}
				calls, writableCalls := provider.calls, provider.writableCalls
				wantCalls := calls
				if freshSourceReview {
					wantCalls++
				}
				if test.Validator != "" && !feedbackRework {
					wantCalls++
				}
				if test.ContinueMessage {
					provider.duringLiveTurn = func(request runner.AgentTurnRequest, _ runner.AgentUpdateHandler) {
						if !strings.Contains(request.Prompt, continueText) || request.ConversationControl == nil {
							t.Fatal("primary turn omitted bound Continue prompt or controls")
						}
						testenv.WaitUntil(t, t.Context(), func() error {
							var started runnerSnapshot
							if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/conversation", nil, &started); err != nil {
								t.Fatal(err)
							}
							if started.delivery("continue-evidence") == conversation.DeliveryDelivered && started.attempt() == resumedExecution.(*ownedExecution).CompletionData().AttemptID && started.Conversation.Execution.Status == conversation.ExecutionRunning {
								return nil
							}
							return fmt.Errorf("primary turn did not acknowledge Continue under its fresh attempt: %+v", started)
						})
					}
				}
				var resumedSession atomic.Int64
				resumed, resumeErr := resumeRunner.Run(t.Context(), runner.RunRequest{Execution: resumedExecution, DeferExecutionFinish: true, ProjectID: "local", Issue: resume, Mode: runner.RunModeImplement, OnUsageUpdate: func(update runner.UsageUpdate) error {
					if update.DetentSessionID > 0 {
						resumedSession.CompareAndSwap(0, update.DetentSessionID)
					}
					return nil
				}})
				if test.ContinueMessage {
					var delivered runnerSnapshot
					if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/conversation", nil, &delivered); err != nil {
						t.Fatal(err)
					}
					if resumeErr != nil || resumedSession.Load() == 0 || resumedSession.Load() == implementationSession.Load() || provider.calls != calls+1 || provider.writableCalls != writableCalls+1 || !resumed.TurnStarted || !strings.Contains(provider.prompt, continueText) || delivered.delivery("continue-evidence") != conversation.DeliveryDelivered || !strings.Contains(delivered.assistantText(), "Finished the native work") || delivered.attempt() == continued.attempt() || !resumedExecution.(*ownedExecution).CompletionSnapshot().ConversationContinuation {
						t.Fatalf("Continue skipped its authorized primary turn: calls=%d->%d writable=%d->%d started=%t delivery=%q attempt=%q error=%v", calls, provider.calls, writableCalls, provider.writableCalls, resumed.TurnStarted, delivered.delivery("continue-evidence"), delivered.attempt(), resumeErr)
					}
					latest, err := h.admin.Change(t.Context(), item, change.ChangeID)
					if err != nil || !reflect.DeepEqual(original.Versions, latest.Versions) || !reflect.DeepEqual(original.Reviews, latest.Reviews) || resumed.NativeChange != nil && resumed.NativeChange.Changed {
						t.Fatalf("Continue fabricated source approval or publication: change=%+v error=%v", resumed.NativeChange, err)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					if state := h.state(t, issue.ID); state != "In Progress" {
						t.Fatalf("conversation Continue changed issue completion lane: %s", state)
					}
					t.Logf("authenticated Continue saved then delivered on actual primary turn; lease/fence/attempt and session fresh; streamed response; unchanged source version/head/reviews; issue still In Progress")
					return
				}
				if resumeErr != nil || resumed.NativeChange == nil || provider.calls != wantCalls || provider.writableCalls != writableCalls || resumed.TurnStarted || resumed.NativeChange.VersionID != change.VersionID || resumed.NativeChange.HeadSHA != change.HeadSHA {
					t.Errorf("explicit technical resume performed writable work: result=%+v turns=%d->%d error=%v", resumed, calls, provider.calls, resumeErr)
				}
				latest, err := h.admin.Change(t.Context(), item, change.ChangeID)
				wantReviews := len(original.Reviews)
				if freshSourceReview {
					wantReviews++
				}
				if test.Validator != "" && !feedbackRework {
					wantReviews++
				}
				if err != nil || len(latest.Reviews) != wantReviews || !reflect.DeepEqual(original.Versions, latest.Versions) || !reflect.DeepEqual(original.Reviews, latest.Reviews[:min(len(original.Reviews), len(latest.Reviews))]) {
					t.Errorf("resume changed immutable source or recorded review: original=%+v latest=%+v error=%v", original, latest, err)
				}
				if feedbackRework {
					if resumed.NativeChange == nil || resumed.NativeChange.ImplementationReview == nil || resumed.NativeChange.ImplementationReview.Verdict != "rework" || resumed.NativeChange.Reviewed || latest.Reviews[len(latest.Reviews)-1].Validator != nil || latest.Reviews[len(latest.Reviews)-1].Decision != "changes_requested" {
						t.Fatalf("new source feedback bypassed independent rework: change=%+v reviews=%+v", resumed.NativeChange, latest.Reviews)
					}
				} else if test.Validator != "" {
					if len(latest.Reviews) != 2 || latest.Reviews[1].Validator == nil || latest.Reviews[1].Validator.SessionID == 0 || original.Reviews[0].ImplementationReview == nil || latest.Reviews[1].Validator.SessionID == original.Reviews[0].ImplementationReview.SessionID || latest.Reviews[1].Validator.SessionID == implementationSession.Load() || !resumed.NativeChange.Reviewed {
						t.Fatalf("resume did not separately record current source and acceptance: reviews=%+v change=%+v", latest.Reviews, resumed.NativeChange)
					}
				}
				freshChanges, err := filepath.Glob(filepath.Join(freshRoot, "*", "CHANGE.md"))
				wantWorkspaces := 1
				if freshSourceReview || test.Validator != "" {
					wantWorkspaces++
				}
				if err != nil || len(freshChanges) != wantWorkspaces {
					t.Fatalf("resume did not restore source into a fresh workspace: paths=%v error=%v", freshChanges, err)
				}
				for _, path := range freshChanges {
					directory := filepath.Dir(path)
					head, err := exec.CommandContext(t.Context(), "git", "-C", directory, "rev-parse", "HEAD").Output()
					if err != nil || strings.TrimSpace(string(head)) != change.HeadSHA || directory == provider.workspace {
						t.Fatalf("resumed workspace lost fresh immutable source: path=%s head=%s error=%v", directory, head, err)
					}
					nativeChangeGit(t, directory, "diff", "--exit-code", change.HeadSHA, "--", "CHANGE.md")
				}
				resumedOwner := resumedExecution.(*ownedExecution)
				stored := original.Versions[0]
				review := original.Reviews[0].ImplementationReview
				if stored.Source == nil || stored.Validation == nil || stored.RunID != owner.CompletionData().RunID || stored.AttemptID != owner.CompletionData().AttemptID || resumedOwner.CompletionData().RunID != stored.RunID || resumedOwner.CompletionData().AttemptID == stored.AttemptID || implementationSession.Load() == 0 || review == nil || review.SessionID == 0 || review.SessionID == implementationSession.Load() {
					t.Fatalf("resume lost source, session, or fresh-workspace identity: stored=%+v review=%+v original=%+v resumed=%+v", stored, review, evidence.Attempt, resumedOwner.CompletionData())
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				if feedbackRework {
					current, err := h.admin.Issue(t.Context(), item)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Rework", Reason: "user_requested"}); err != nil {
						t.Fatal(err)
					}
					repairCandidates := h.candidatesIn(t, "Rework")
					if len(repairCandidates) != 1 || repairCandidates[0].ID != issue.ID {
						t.Fatalf("genuine feedback has no Rework candidate: %+v", repairCandidates)
					}
					repairClaim, err := h.scheduler.AdoptClaim(t.Context(), repairCandidates[0], time.Now())
					if err != nil {
						t.Fatal(err)
					}
					repair := repairClaim.Issue
					provider.sourceFeedback, provider.sourceRecord, provider.sourceVerdict = "", "", "pass"
					_, repairErr := resumeRunner.Run(t.Context(), runner.RunRequest{Execution: h.scheduler.RunExecution(issue.ID), DeferExecutionFinish: true, ProjectID: "local", Issue: repair, Mode: runner.RunModeImplement})
					if provider.writableCalls != writableCalls+1 {
						t.Fatalf("genuine source feedback did not reach writable repair: turns=%d error=%v", provider.writableCalls, repairErr)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
				}
				t.Logf("source POST acknowledged; claim release status=%s complete disposition retained; resume model turns=%d->%d, writable turns=%d->%d fresh workspace=true; immutable version/head/policy/source retained; original and resumed attempts plus implementation/review sessions distinct", attempt.Status, calls, provider.calls, writableCalls, provider.writableCalls)
				return
			}
			if test.FinalMessage != "" {
				if err != nil || result.FinalState != runner.FinalStateCompleted || result.NativeChange != nil || provider.calls != 1 {
					t.Fatalf("blocked Rework ran source completion: result=%+v turns=%d error=%v", result, provider.calls, err)
				}
				if _, err := os.Stat(gateMarker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("blocked Rework consumed the failing gate: %v", err)
				}
				owner := execution.(*ownedExecution)
				checkpoint := owner.CompletionData().Handoff
				if checkpoint == nil || checkpoint.HeadSHA != expected.Versions[len(expected.Versions)-1].HeadSHA || checkpoint.WorkspaceDigest == "" || checkpoint.Availability != "available" || checkpoint.ExternalEffect != "none" || checkpoint.EffectState != "none" {
					t.Fatalf("blocked Rework lost its source checkpoint: %+v", checkpoint)
				}
				if test.Staged {
					staged, readErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "diff", "--cached", "--name-only").Output()
					if readErr != nil || strings.TrimSpace(string(staged)) != "CHANGE.md" || checkpoint.WorktreeState != "dirty" {
						t.Fatalf("blocked Rework finalized staged source: staged=%s checkpoint=%+v", staged, checkpoint)
					}
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if err != nil || !reflect.DeepEqual(detail, *expected) || h.state(t, issue.ID) != "Rework" {
					t.Fatalf("blocked Rework changed current version or feedback: %+v, %v", detail, err)
				}
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" || recovery.Attempts[0].TerminalFailure != nil || !reflect.DeepEqual(recovery.Attempts[0].Disposition, test.WantDisposition) || !reflect.DeepEqual(recovery.Attempts[0].Checkpoint, checkpoint) {
					t.Fatalf("blocked Rework lost its durable handoff: %+v, %v", recovery.Attempts, err)
				}
				if len(h.candidatesIn(t, "Rework")) != 0 || h.state(t, issue.ID) != "Blocked" {
					t.Fatal("blocked unchanged source was immediately offered again")
				}
				return
			}
			if test.Publication != "" {
				owner := execution.(*ownedExecution)
				if genuinePublication {
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), owner.NativeChange().ChangeID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					for _, version := range current.Versions {
						if version.ID != current.Change.CurrentVersion {
							continue
						}
						for _, check := range version.Checks {
							if _, err := publicationCI.SubmitChangeCheck(t.Context(), tracker.NativeWorkItemID(issue.ID), current.Change.ID, version.ID, tracker.SubmitChangeCheck{Mutation: nativeMutationKey(), ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: version.HeadSHA, RunID: version.RunID, PolicyID: version.PolicyID, ConfigDigest: version.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: version.CreatedAt, Evidence: []tracker.ChangeArtifact{{Kind: "test", URI: "s3://fixture/test", SHA256: policy.Digest([]byte("test receipt")), Availability: "available"}}}}); err != nil {
								t.Fatal(err)
							}
						}
					}
					current, readErr = h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), current.Change.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if test.Formal {
						if _, err := h.admin.ReviewChange(t.Context(), tracker.NativeWorkItemID(issue.ID), current.Change.ID, current.Change.CurrentVersion, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "changes_requested", Body: "Preserve human hold"}); err != nil {
							t.Fatal(err)
						}
						current, readErr = h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), current.Change.ID)
						if readErr != nil {
							t.Fatal(readErr)
						}
					}
					expected = &current
					provider.staged = false
				}
				wantEffect, wantState, wantCreates := "pr_create", "ambiguous", 1
				if test.Publication == "lost_push" {
					wantEffect, wantState, wantCreates = "git_push", "pending", 0
				}
				if test.Publication == "confirmed" {
					if err != nil || owner.CompletionSnapshot().Publication == nil || owner.CompletionData().Handoff == nil || forgeCreates != 1 || result.NativeChange == nil || result.NativeChange.Validator == nil {
						t.Fatalf("initial confirmed publication lost native validation: change=%+v error=%v", result.NativeChange, err)
					}
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if readErr != nil || current.Change.CurrentVersion != expected.Change.CurrentVersion || result.NativeChange.Validator.Verdict != test.Validator {
						t.Fatalf("initial validator did not retain immutable source: detail=%+v error=%v", current, readErr)
					}
					if test.Validator != "pass" && (current.Summary.Status != "needs_evidence" || result.NativeChange.Reviewed) {
						t.Fatal("confirmed publication alone yielded readiness")
					}
					if prerequisite != "" {
						commands := result.NativeChange.Validator.Commands
						if len(commands) != 3 || commands[2].Command != provider.validatorCommand || commands[2].ExitCode == 0 {
							t.Fatalf("validator rework lacks failed prerequisite command: %+v", commands)
						}
						if commands[0].TimingStage != "finalization" || commands[0].ExitCode != 0 || commands[0].Command != commands[1].Command || commands[1].ExitCode != 0 {
							t.Fatalf("validator rework lost successful finalization and validation gates: %+v", commands)
						}
						if err := os.WriteFile(prerequisite, []byte("ready"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					expected = &current
					provider.validator = "pass"
				} else if err == nil || owner.CompletionData().Handoff == nil || owner.CompletionData().Handoff.ExternalEffect != wantEffect || owner.CompletionData().Handoff.EffectState != wantState || forgeCreates != wantCreates {
					t.Fatalf("first Runner attempt did not preserve lost create: effect=%+v creates=%d error=%v", owner.CompletionData().Handoff, forgeCreates, err)
				}
				if expected == nil {
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), owner.NativeChange().ChangeID)
					if readErr != nil || len(current.Versions) != 1 {
						t.Fatalf("implementation publication did not retain its version: %+v, %v", current.Versions, readErr)
					}
					expected = &current
				} else if test.Staged {
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if readErr != nil || len(current.Versions) != len(expected.Versions)+1 || !reflect.DeepEqual(current.Reviews, expected.Reviews) || !reflect.DeepEqual(current.Checks, expected.Checks) {
						t.Fatal("staged publication changed prior immutable evidence")
					}
					expected = &current
				}
				oldCheckpoint := *owner.CompletionData().Handoff
				oldPublicationHead = oldCheckpoint.HeadSHA
				treeOutput, readErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD^{tree}").Output()
				if readErr != nil {
					t.Fatal(readErr)
				}
				oldPublicationTree := strings.TrimSpace(string(treeOutput))
				branchOutput, readErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "symbolic-ref", "--short", "HEAD").Output()
				if readErr != nil {
					t.Fatal(readErr)
				}
				oldPublicationBranch = strings.TrimSpace(string(branchOutput))
				retainedHead := ""
				if test.PublicationFault == "advanced_pr" {
					advanced, advanceErr := exec.CommandContext(t.Context(), "git", "-C", remote, "commit-tree", oldPublicationTree, "-p", oldPublicationHead, "-m", "third party advancement").Output()
					if advanceErr != nil {
						t.Fatal(advanceErr)
					}
					advancedHead := strings.TrimSpace(string(advanced))
					nativeChangeGit(t, remote, "update-ref", "refs/heads/"+oldPublicationBranch, advancedHead)
					forgePull = fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/native-change"}},"base":{"ref":"main","repo":{"full_name":"example/native-change"}}}`, advancedHead, oldPublicationBranch)
					retainedHead = oldPublicationHead
				}
				if test.PublicationFault == "post_push_read" {
					nativeChangeGit(t, remote, "update-ref", "-d", "refs/heads/"+oldPublicationBranch)
				}
				if test.PublicationFault == "dirty_source" {
					if err := os.WriteFile(filepath.Join(provider.workspace, "UNFINISHED.md"), []byte("preserve unfinished source"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if test.PublicationFault == "changed_head" {
					nativeChangeGit(t, provider.workspace, "commit", "--allow-empty", "-m", "new local source")
				}
				if test.PublicationFault == "branch" {
					nativeChangeGit(t, provider.workspace, "branch", "-m", "renamed-publication")
				}
				if test.PublicationFault == "missing_remote" {
					nativeChangeGit(t, remote, "update-ref", "-d", "refs/heads/"+oldPublicationBranch)
				}

				oldAttempt, oldLease := owner.CompletionData().AttemptID, owner.claim.lease
				stopPublicationGuard()
				if err := h.native.Release(t.Context(), oldLease, "released"); err != nil {
					t.Fatal(err)
				}
				if genuinePublication {
					currentIssue, readErr := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
					if readErr != nil {
						t.Fatal(readErr)
					}
					for _, state := range []string{"Human Review", "Rework"} {
						currentIssue, readErr = h.admin.Transition(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: state, Reason: "user_requested"})
						if readErr != nil {
							t.Fatalf("human transition state=%s: %T %v", state, readErr, readErr)
						}
					}
				}
				machine := fixtureSchedulerMachine(h.scheduler)
				client := h.scheduler.Transport()
				if test.PublicationFault == "foreign_source" || test.ForeignRunner || genuinePublication {
					client, machine = hubclientEnrolledRunner(t, h.admin.Transport(), h.organization, h.project, h.failChanges)
					h.native, err = client.Native(h.organization, h.project)
					if err != nil {
						t.Fatal(err)
					}
					h.connector.worker, err = NewNativeConnector(h.native)
					if err != nil {
						t.Fatal(err)
					}
				}
				dependencies.Store = api.NewRunnerSessions(h.native)
				agent, err = runner.NewRunner(dependencies)
				if err != nil {
					t.Fatal(err)
				}
				machineID := machine.ID
				h.scheduler, err = newTestScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if test.PublicationFault == "policy" {
					h.repolicy(t)
				}
				if test.StalePolicy || test.PublicationFault == "policy" {
					rules, err := h.admin.ChangeReviewPolicy(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					rules.PolicyID = h.descriptor.ID
					rules.RequireReview = rules.RequireReview || test.StalePolicy
					if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), ExpectedID: rules.ID, Policy: rules}); err != nil {
						t.Fatal(err)
					}
				}
				publicationRetry = true
				reconcileLane := "Rework"
				if !test.Rework {
					reconcileLane = "In Progress"
				}
				candidates := h.candidatesIn(t, reconcileLane)
				if test.PublicationFault == "foreign_source" {
					if len(candidates) != 1 {
						t.Fatalf("foreign runner was not admitted to reconcile an uncertain publication: candidates=%d", len(candidates))
					}
					return
				}
				if len(candidates) != 1 {
					t.Fatalf("unchanged held source not admitted: candidates=%d", len(candidates))
				}
				candidate = candidates[0]
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				owner = execution.(*ownedExecution)
				if test.StalePolicy && (owner.Recovery().ChangeDetail == nil || owner.Recovery().ChangeDetail.Summary.Status != "stale_policy") {
					t.Fatal("retry must reproduce a stale current Change policy")
				}
				if owner.claim.lease.MachineID != machineID || owner.claim.lease.ID == oldLease.ID || owner.claim.lease.FencingToken <= oldLease.FencingToken {
					t.Fatal("retry reused old lease authority")
				}
				if test.PublicationFault == "version" {
					base := expected.Versions[len(expected.Versions)-1].BaseSHA
					capture, captureErr := workspace.CaptureChangeSource(t.Context(), provider.workspace, base, oldPublicationHead)
					if captureErr != nil {
						t.Fatal(captureErr)
					}
					h.publishCaptured(t, tracker.NativeWorkItemID(issue.ID), expected.Change.ID, oldPublicationHead, &capture, expected.Change.CurrentVersion, base)
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					expected = &current
				}
				if test.PublicationFault == "unavailable_source" {
					next := fixtureClient(h.native.Transport()).requests.Load()
					fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") {
							return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"insufficient_scope","message":"source read refused"}`))}, nil
						}
						return next.RoundTrip(request)
					}))
				}
				if test.PublicationFault == "lease" {
					if err := h.native.Release(t.Context(), owner.claim.lease, "released"); err != nil {
						t.Fatal(err)
					}
				}
				remoteBefore, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags").Output()
				if readErr != nil {
					t.Fatal(readErr)
				}
				var publicationFault func(*workspace.LandOptions) func()
				var readFailure, pushMarker string
				if test.PublicationFault == "initial_read" || test.PublicationFault == "post_push_read" {
					realGit, lookupErr := exec.LookPath("git")
					if lookupErr != nil {
						t.Fatal(lookupErr)
					}
					wrapper := t.TempDir()
					pushMarker = filepath.Join(wrapper, "pushed")
					readFailure = filepath.Join(wrapper, "read-failed")
					quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
					condition := "true"
					if test.PublicationFault == "post_push_read" {
						condition = "test -f " + quote(pushMarker)
					}
					for name, script := range map[string]string{
						"uploadpack":  "#!/bin/sh\nif " + condition + "; then : > " + quote(readFailure) + "; exit 1; fi\nexec " + quote(realGit) + " upload-pack \"$@\"\n",
						"receivepack": "#!/bin/sh\n" + quote(realGit) + " receive-pack \"$@\"\nresult=$?\nif [ \"$result\" -ne 0 ]; then exit \"$result\"; fi\n: > " + quote(pushMarker) + "\nexit 1\n",
					} {
						if writeErr := os.WriteFile(filepath.Join(wrapper, name), []byte(script), 0o700); writeErr != nil {
							t.Fatal(writeErr)
						}
					}
					publicationFault = func(opts *workspace.LandOptions) func() {
						opts.TargetBranch = "main"
						for _, name := range []string{"uploadpack", "receivepack"} {
							nativeChangeGit(t, source, "config", "remote.origin."+name, quote(filepath.Join(wrapper, name)))
						}
						return func() {
							for _, name := range []string{"uploadpack", "receivepack"} {
								nativeChangeGit(t, source, "config", "--unset", "remote.origin."+name)
							}
						}
					}
				}
				prBefore := forgePull
				callsBefore := provider.calls
				if test.StalePolicy {
					if err := os.WriteFile(filepath.Join(provider.workspace, "LOCAL.md"), []byte("unpublished local work\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					freshRoot := nativeChangeTempDir(t)
					nativeChangeGit(t, freshRoot, "clone", remote, "source")
					freshSource := filepath.Join(freshRoot, "source")
					nativeChangeGit(t, freshSource, "remote", "set-url", "origin", nativeChangeRepository)
					nativeChangeGit(t, freshSource, "config", "url."+remote+".insteadOf", nativeChangeRepository)
					dependencies.Workspace, err = workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(freshRoot, "workspaces"), SourceRoot: freshSource, AutoBranch: true})
					if err != nil {
						t.Fatal(err)
					}
					agent, err = runner.NewRunner(dependencies)
					if err != nil {
						t.Fatal(err)
					}
				}
				result, err = agent.Run(t.Context(), runner.RunRequest{Execution: &nativePublicationFixtureExecution{ownedExecution: owner, forge: forge, losePush: &losePush, retainedHead: retainedHead, publicationFault: publicationFault}, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
				result.NativeChange = owner.NativeChange()
				if test.PublicationFault == "initial_read" || test.PublicationFault == "post_push_read" {
					if _, statErr := os.Stat(readFailure); statErr != nil {
						t.Fatalf("publication did not reach the injected ref read failure: %v; run error=%v", statErr, err)
					}
					_, pushErr := os.Stat(pushMarker)
					if test.PublicationFault == "post_push_read" && pushErr != nil || test.PublicationFault == "initial_read" && !errors.Is(pushErr, os.ErrNotExist) {
						t.Fatalf("publication ref read failed at the wrong push boundary: %v", pushErr)
					}
					if !nativeTransportUnavailable(err) || !owner.CompletionData().Handoff.UncertainForgeEffect() || owner.Settled() || provider.calls != callsBefore || forgeCreates != wantCreates || forgePull != prBefore {
						t.Fatalf("ref read failure lost infrastructure retry: settled=%t checkpoint=%+v error=%v", owner.Settled(), owner.CompletionData().Handoff, err)
					}
					branchHead, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+oldPublicationBranch).Output()
					if readErr != nil || strings.TrimSpace(string(branchHead)) != oldPublicationHead {
						t.Fatalf("ref read failure changed intended source: %s %v", branchHead, readErr)
					}
					return
				}
				if test.PublicationFault == "advanced_pr" {
					if owner.CompletionData().Handoff.UncertainForgeEffect() || provider.calls != callsBefore || forgeCreates != wantCreates || forgePull != prBefore {
						t.Fatalf("advanced PR did not settle uncertainty without writing: checkpoint=%+v error=%v", owner.CompletionData().Handoff, err)
					}
					remoteAfter, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags").Output()
					if readErr != nil || string(remoteAfter) != string(remoteBefore) {
						t.Fatal("advanced PR reconciliation overwrote third party source")
					}
					if releaseErr := h.native.Release(t.Context(), owner.claim.lease, "released"); releaseErr != nil {
						t.Fatal(releaseErr)
					}
					next := h.candidatesIn(t, "Rework")
					if len(next) != 1 {
						t.Fatalf("settled Rework not dispatchable: %d", len(next))
					}
					if _, claimErr := h.scheduler.AdoptClaim(t.Context(), next[0], time.Now()); claimErr != nil {
						t.Fatal(claimErr)
					}
					nextOwner := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
					_, nextErr := agent.Run(t.Context(), runner.RunRequest{Execution: &nativePublicationFixtureExecution{ownedExecution: nextOwner, forge: forge, retainedHead: retainedHead}, ProjectID: "local", Issue: next[0], Mode: runner.RunModeImplement})
					if provider.calls != callsBefore+1 {
						t.Fatalf("settled Rework skipped provider turn: calls=%d want=%d error=%v", provider.calls, callsBefore+1, nextErr)
					}
					return
				}
				if test.PublicationFault == "missing_pr" || test.PublicationFault == "missing_remote" {
					repeatedCreates := wantCreates
					if test.PublicationFault == "missing_pr" {
						repeatedCreates++
					}
					branchHead, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+oldPublicationBranch).Output()
					if err != nil || readErr != nil || owner.CompletionSnapshot().Publication == nil || owner.CompletionSnapshot().Publication.HeadSHA != oldPublicationHead || forgeCreates != repeatedCreates || strings.TrimSpace(string(branchHead)) != oldPublicationHead || provider.calls != callsBefore {
						t.Fatalf("absent forge write was not repeated exactly once: fault=%s creates=%d publication=%+v error=%v", test.PublicationFault, forgeCreates, owner.CompletionSnapshot().Publication, err)
					}
					return
				}
				if test.PublicationFault != "" && test.PublicationFault != "policy" {
					if err == nil && (result.NativeChange == nil || result.NativeChange.VersionError == "" && result.NativeChange.Error == "") || owner.CompletionSnapshot().Publication != nil || forgeCreates != wantCreates || forgePull != prBefore || provider.calls != callsBefore {
						t.Fatalf("unresolved publication was accepted or rewrote forge/source: fault=%s creates=%d publication=%v provider_calls=%d error=%v", test.PublicationFault, forgeCreates, owner.CompletionSnapshot().Publication != nil, provider.calls-callsBefore, err)
					}
					remoteAfter, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags").Output()
					if readErr != nil || string(remoteBefore) != string(remoteAfter) {
						t.Fatal("refused reconciliation modified bare remote")
					}
				} else {
					wantCalls := callsBefore
					if test.Publication == "confirmed" {
						wantCalls++
					}
					if test.Validator != "" && expected.Summary.Status != "reviewed" {
						wantCalls++
						if result.NativeChange == nil || result.NativeChange.Validator == nil || result.NativeChange.Validator.Verdict != "pass" {
							t.Fatalf("publication recovery skipped the pending native validator: change=%+v error=%v", result.NativeChange, err)
						}
					}
					if provider.calls != wantCalls {
						t.Fatalf("publication recovery provider calls = %d, want %d", provider.calls, wantCalls)
					}
				}

				if (test.PublicationFault == "" || test.PublicationFault == "policy") && (err != nil || result.NativeChange == nil || owner.CompletionSnapshot().Publication == nil || forgeCreates != 1) {
					t.Fatalf("real Runner retry failed exact PR adoption: creates=%d publication=%v error=%v", forgeCreates, owner.CompletionSnapshot().Publication != nil, err)
				}
				item := tracker.NativeWorkItemID(issue.ID)
				path, err := nativeItemPath(item)
				if err != nil {
					t.Fatal(err)
				}
				var oldRecord tracker.NativeAttempt
				err = h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+path+"/attempts/"+oldAttempt, nil, &oldRecord)
				if err != nil || oldRecord.AttemptID != oldAttempt || oldRecord.Checkpoint == nil || !reflect.DeepEqual(*oldRecord.Checkpoint, oldCheckpoint) {
					t.Fatalf("retry changed old attempt checkpoint: %v", err)
				}
				detail, err := h.admin.Change(t.Context(), item, expected.Change.ID)
				if err != nil {
					t.Fatal(err)
				}
				if test.StalePolicy {
					if len(detail.Versions) != len(expected.Versions)+1 || detail.Change.CurrentVersion == expected.Change.CurrentVersion || result.NativeChange.Reviewed || detail.Summary.Status == "stale_policy" || detail.Summary.NativeReview != "pending" {
						t.Fatalf("stale policy recovery did not publish source for current review: change=%+v summary=%+v versions=%d", result.NativeChange, detail.Summary, len(detail.Versions))
					}
					for i, version := range detail.Versions {
						if i < len(expected.Versions) {
							version.External = nil
							prior := expected.Versions[i]
							prior.External = nil
							if !reflect.DeepEqual(version, prior) {
								t.Fatal("policy refresh changed an immutable source version")
							}
							continue
						}
						if version.ID != detail.Change.CurrentVersion || version.HeadSHA != oldPublicationHead || version.PolicyID != h.descriptor.ID || version.Source == nil || !owner.CompletionSnapshot().Publication.Matches(detail.Change.ID, version) {
							t.Fatalf("policy refresh lost verified source or publication identity: %+v", version)
						}
					}
					if !reflect.DeepEqual(detail.Reviews, expected.Reviews) || !reflect.DeepEqual(detail.Checks, expected.Checks) || detail.Change.Landed != nil {
						t.Fatal("policy refresh changed review, checks or landing evidence")
					}
					local, err := os.ReadFile(filepath.Join(provider.workspace, "LOCAL.md"))
					if err != nil || string(local) != "unpublished local work\n" {
						t.Fatal("foreign recovery discarded unpublished work on the original runner")
					}
					return
				}
				for i := range detail.Versions {
					detail.Versions[i].External = nil
					expected.Versions[i].External = nil
				}
				if test.Validator != "" && (test.PublicationFault == "" || test.PublicationFault == "policy") && expected.Summary.Status != "reviewed" {
					if len(detail.Reviews) != len(expected.Reviews)+1 || detail.Reviews[len(detail.Reviews)-1].Validator == nil || detail.Reviews[len(detail.Reviews)-1].VersionID != expected.Change.CurrentVersion {
						t.Fatal("publication recovery lost the fresh current-version validator decision")
					}
					verdict := detail.Reviews[len(detail.Reviews)-1].Validator
					if verdict.HeadSHA != oldPublicationHead || verdict.VersionID != expected.Change.CurrentVersion {
						t.Fatal("validator recovery reused stale source or provider identity")
					}
					for _, previous := range expected.Reviews {
						if previous.Validator != nil && previous.Validator.SessionID == verdict.SessionID {
							t.Fatal("validator recovery reused a previous validator session")
						}
					}
					for _, command := range verdict.Commands {
						if command.ExitCode != 0 || command.HeadSHA != oldPublicationHead || command.TreeSHA != oldPublicationTree {
							t.Fatalf("repaired validation lacks actual current source evidence: %+v", command)
						}
					}
					detail.Reviews = detail.Reviews[:len(expected.Reviews)]
				}
				if detail.Change.CurrentVersion != expected.Change.CurrentVersion || !reflect.DeepEqual(detail.Versions, expected.Versions) || !reflect.DeepEqual(detail.Reviews, expected.Reviews) || !reflect.DeepEqual(detail.Checks, expected.Checks) || detail.Change.Landed != nil {
					t.Fatal("held retry changed version, reviews, checks or landing identity")
				}
				if test.PublicationFault != "" && test.PublicationFault != "policy" {
					return
				}
				var visible []tracker.PullRequestView
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/pull-requests", nil, &visible); err != nil {
					t.Fatal(err)
				}
				if len(visible) != 1 || visible[0].ChangeID != detail.Change.ID || visible[0].URL != nativeChangeRepository+"/pull/7" || visible[0].Number != 7 {
					t.Fatal("reconciled PR absent from the operator publication owner")
				}
				target, err := execution.(runner.LandingExecution).LandingTarget(t.Context())
				if !test.Formal && test.Publication == "confirmed" {
					if err != nil || !result.NativeChange.Reviewed || target.VersionID != expected.Change.CurrentVersion {
						t.Fatalf("fresh applicable native decision did not establish readiness: target=%+v error=%v", target, err)
					}
				} else if !errors.Is(err, runner.ErrLandingNotReviewed) || target.External == nil || target.External.URL != visible[0].URL {
					t.Fatal("publication cleared human hold or lost landing PR identity")
				}
				return
			}
			if test.Expired {
				evidence, readErr := h.latestRuntimeEvidence(t, tracker.NativeWorkItemID(issue.ID))
				if !errors.Is(context.Cause(runCtx), context.DeadlineExceeded) || readErr != nil || evidence.Attempt == nil || evidence.Attempt.Status != "succeeded" {
					t.Fatalf("completed turn lost after parent expiry: attempt=%+v error=%v", evidence.Attempt, readErr)
				}
			}
			if test.SSH && err == nil {
				result.NativeChange = execution.(runner.ChangeExecution).NativeChange()
			}
			if test.Absorbed || test.Delivered != "" {
				change := result.NativeChange
				if test.Delivered != "" {
					head, headErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD").Output()
					status, statusErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "status", "--porcelain").Output()
					if headErr != nil || statusErr != nil || providerStartHead == "" || (test.Delivered == "exact" || test.Delivered == "release") && strings.TrimSpace(string(head)) != providerStartHead || strings.TrimSpace(string(status)) != "" {
						t.Fatalf("provider changed preserved source: before=%s after=%s status=%s errors=%v/%v", providerStartHead, head, status, headErr, statusErr)
					}
				}
				if err != nil || change == nil || provider.calls != 1 || change.ChangeID != expected.Change.ID {
					t.Fatalf("absorbed Rework did not finish once: result=%+v calls=%d error=%v", result, provider.calls, err)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if err != nil || !reflect.DeepEqual(versionsWithoutLanding(detail.Versions), versionsWithoutLanding(expected.Versions)) || detail.Change.CurrentVersion != expected.Change.CurrentVersion {
					t.Fatalf("absorbed Rework changed immutable identity: detail=%+v error=%v", detail, err)
				}
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				version := expected.Versions[len(expected.Versions)-1]
				if stored.BaseSHA != version.BaseSHA || stored.HeadSHA != version.HeadSHA || len(stored.Files) != 1 || stored.Files[0].Path != "PRESERVED.md" || stored.Files[0].Additions != 1 || stored.Files[0].Deletions != 0 || stored.AttemptID != attempt || stored.Producer.LeaseID != execution.Recovery().Lease.ID || stored.Producer.FencingToken != execution.Recovery().Lease.FencingToken {
					t.Fatalf("incorporated source lost its reviewed delta or attempt identity: %+v", stored)
				}
				if test.Formal || test.Hold {
					if change.Landing != nil || detail.Change.Landed != nil || change.VersionID != version.ID || change.VersionError != "" || test.Formal && (change.Reviewed || detail.Summary.NativeReview != "changes_requested") {
						t.Fatalf("human rejection was waived: %+v", change)
					}
					h.complete(t, issue.ID, change)
					evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), attempt)
					if err != nil || evidence.Attempt == nil || evidence.Attempt.Finalization == nil || evidence.Attempt.Finalization.ChangeID != expected.Change.ID || evidence.Attempt.Finalization.VersionID != version.ID || evidence.Attempt.Finalization.HeadSHA != version.HeadSHA || evidence.Attempt.Finalization.VersionError != change.VersionError || evidence.Attempt.ClaimReleasedAt == nil || test.Hold && (evidence.Attempt.Disposition == nil || !evidence.Attempt.Disposition.HumanAction) {
						t.Fatalf("refusal projection lost existing source provenance: %+v, %v", evidence.Attempt, err)
					}
				} else {
					target := "main"
					if test.Delivered == "release" {
						target = "release/v1.2"
					}
					head, err := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+target).Output()
					if err != nil {
						t.Fatal(err)
					}
					merge := deliveredTargetHead
					if test.AdvanceTarget {
						merge = targetHead
					}
					if merge == "" || strings.TrimSpace(string(head)) != merge || change.Landing == nil || !change.Landing.Landed || detail.Change.Landed == nil || detail.Change.Landed.VersionID != expected.Change.CurrentVersion || detail.Change.Landed.MergeSHA != merge || detail.Change.Landed.BaseRef != target || change.VersionError != "" {
						t.Fatalf("absorbed source lacks authentic landing: change=%+v detail=%+v", change, detail)
					}
					if test.Delivered == "" {
						if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
							t.Fatal(err)
						}
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("a second release retained authority: %v", err)
					}
				}
				if h.state(t, issue.ID) != test.WantState || len(h.candidatesIn(t, "Rework")) != 0 {
					t.Fatal("absorbed completion remained dispatchable")
				}
				next := h.createInProgress(t, "Next native work")
				h.claim(t, next.ID)
				return
			}
			if test.LateConflict {
				owner := execution.(*ownedExecution)
				if err != nil || result.FinalState != runner.FinalStateCompleted || result.NativeChange != nil || owner.CompletionData().Outcome != "succeeded" || owner.CompletionSnapshot().WorktreeState != "dirty" || checks.DirtyArtifacts(t, Underlying(owner)) {
					t.Fatalf("late host conflict lost successful dirty continuation: result=%+v outcome=%+v error=%v", result, owner.CompletionData(), err)
				}
				detail, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if readErr != nil || !reflect.DeepEqual(detail, *expected) || h.state(t, issue.ID) != "Rework" {
					t.Fatalf("late conflict changed version, feedback or lane: detail=%+v error=%v", detail, readErr)
				}
				if output, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "diff", "--name-only", "--diff-filter=U").Output(); err != nil || strings.TrimSpace(string(output)) != "CHANGE.md" {
					t.Fatalf("host conflict not preserved: %s, %v", output, err)
				}
				if owner.CompletionSnapshot().Diff != nil {
					for _, file := range owner.CompletionSnapshot().Diff.Files {
						if strings.Contains(file.Patch, "<<<<<<<") {
							t.Fatal("host conflict captured an unresolved index")
						}
					}
				}
				recovery, readErr := h.native.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if readErr != nil || len(recovery.Attempts) != 1 {
					t.Fatalf("late conflict lost durable native attempt: %+v, %v", recovery.Attempts, readErr)
				}
				attempt := recovery.Attempts[0]
				if attempt.Status != "succeeded" || attempt.Sequence != owner.CompletionData().Sequence || attempt.Checkpoint == nil || attempt.Checkpoint.WorktreeState != "dirty" || attempt.Checkpoint.HeadSHA != result.DiffStats.HeadSHA {
					t.Fatalf("late conflict receipt disagrees with authentic outcome: %+v", attempt)
				}
				if attempt.Disposition == nil || attempt.Disposition.Status != "in_progress" || attempt.Disposition.Blockers || attempt.Disposition.HumanAction || attempt.Disposition.ReasonCode != "" {
					t.Fatalf("late conflict did not retain an unfinished disposition: %+v", attempt.Disposition)
				}
				previousLease := execution.Recovery().Lease
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				candidates := h.candidatesIn(t, "Rework")
				if len(candidates) != 1 {
					t.Fatalf("preserved conflict did not offer existing Rework continuation: %+v", candidates)
				}
				candidate = candidates[0]
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				if execution.Recovery().Lease.ID == previousLease.ID || execution.Recovery().Lease.FencingToken == previousLease.FencingToken {
					t.Fatal("conflict continuation reused terminal authority")
				}
				if execution.Recovery().Issue.Revision != recovery.Issue.Revision {
					t.Fatal("conflict continuation fabricated an item edit")
				}
				provider.inProgress = false
				result, err = agent.Run(t.Context(), runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			}
			if !provider.bound && !test.SSH {
				t.Fatalf("native worker did not bind its conversation: run error=%v", err)
			}
			if test.BrokenSigning {
				signature, readErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "log", "-1", "--format=%G?").Output()
				if readErr != nil || strings.TrimSpace(string(signature)) != "N" {
					t.Fatalf("unavailable signer did not produce an unsigned final commit: %s, %v", signature, readErr)
				}
			}
			if test.ImportedVersion && test.Formal {
				detail, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if err == nil || readErr != nil || !reflect.DeepEqual(detail.Versions, expected.Versions) || !reflect.DeepEqual(detail.Reviews, expected.Reviews) || detail.Summary.NativeReview != "changes_requested" || detail.Change.CurrentLanding() != nil {
					t.Fatalf("imported source superseded human rejection: error=%v read=%v summary=%+v", err, readErr, detail.Summary)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v (provider calls: %d; writable calls: %d; final message: %s)", err, provider.calls, provider.writableCalls, result.FinalMessage)
			}
			if result.FinalState != runner.FinalStateCompleted {
				t.Fatalf("final state = %q", result.FinalState)
			}
			if test.Rework {
				marker := "Native Hub recovery context"
				start := strings.Index(provider.prompt, marker)
				if start < 0 {
					t.Fatal("provider prompt omitted native recovery")
				}
				data := provider.prompt[start:]
				data = data[strings.Index(data, "\n")+1:]
				var recovery tracker.NativeRecovery
				if err := json.NewDecoder(strings.NewReader(data)).Decode(&recovery); err != nil {
					t.Fatal(err)
				}
				currentFeedback := *expected
				currentFeedback.Versions = slices.DeleteFunc(slices.Clone(expected.Versions), func(v tracker.ChangeVersion) bool { return v.ID != expected.Change.CurrentVersion })
				currentFeedback.Reviews = slices.DeleteFunc(slices.Clone(expected.Reviews), func(r tracker.ChangeReview) bool { return r.VersionID != expected.Change.CurrentVersion })
				currentFeedback.Checks = slices.DeleteFunc(slices.Clone(expected.Checks), func(c tracker.ChangeCheck) bool { return c.VersionID != expected.Change.CurrentVersion })
				currentFeedback.Discussion = slices.DeleteFunc(slices.Clone(expected.Discussion), func(d tracker.ChangeDiscussion) bool {
					return d.VersionID != "" && d.VersionID != expected.Change.CurrentVersion
				})
				if len(currentFeedback.Reviews) == 0 {
					currentFeedback.Reviews = nil
				}
				if len(currentFeedback.Checks) == 0 {
					currentFeedback.Checks = nil
				}
				if len(currentFeedback.Discussion) == 0 {
					currentFeedback.Discussion = nil
				}
				if test.StaleBase {
					t.Logf("fresh conflict provider request: %d bytes", len(provider.prompt))
				}
				if test.StaleBase && (len(provider.prompt) >= 1048576 || len(recovery.Attempts) > 4 || len(recovery.History) != 0 || !strings.Contains(provider.prompt, "Human hold: preserve the original checkpoint")) {
					t.Fatalf("fresh provider request replayed historic recovery: bytes=%d", len(provider.prompt))
				}
				if recovery.Change == nil || recovery.Change.VersionID != expected.Change.CurrentVersion || !reflect.DeepEqual(recovery.ChangeDetail, &currentFeedback) {
					t.Fatalf("provider prompt lost exact Change feedback: change=%+v, detail=%+v", recovery.Change, recovery.ChangeDetail)
				}
				if !strings.Contains(provider.prompt, "Discussion is not formal approval") || !strings.Contains(provider.prompt, "historical context, not current approval or rejection") {
					t.Fatal("provider prompt omitted feedback authority boundaries")
				}
				if !test.Formal {
					for _, comment := range recovery.Discussion {
						if strings.Contains(comment.Body, "Global config loses concurrent updates") {
							t.Fatal("discussion-only regression used an issue comment")
						}
					}
				}
			}
			attemptID := executionID("attempt", string(execution.Recovery().Lease.ID))
			if !test.SSH {
				var transcript struct {
					Conversation struct {
						Execution struct {
							AttemptID *string `json:"attempt_id"`
						} `json:"execution"`
					} `json:"conversation"`
					Messages []struct {
						Text      string  `json:"text"`
						AttemptID *string `json:"attempt_id"`
						TurnID    *string `json:"turn_id"`
						Actor     struct {
							Kind string `json:"kind"`
						} `json:"actor"`
					} `json:"messages"`
				}
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/conversation", nil, &transcript); err != nil {
					t.Fatal(err)
				}
				if transcript.Conversation.Execution.AttemptID == nil || *transcript.Conversation.Execution.AttemptID != attemptID {
					t.Fatalf("conversation lost attempt identity: %+v", transcript)
				}
				found := false
				for _, message := range transcript.Messages {
					if message.Text == "Finished the native work" {
						found = message.AttemptID != nil && *message.AttemptID == attemptID && message.TurnID != nil && *message.TurnID == "turn-1" && message.Actor.Kind == "runner"
					}
				}
				if !found {
					t.Fatalf("conversation lost authenticated worker events: %+v", transcript.Messages)
				}
			}
			change := result.NativeChange
			if test.SourceFixture != "" && test.SourceFixture != "synthetic" {
				if change == nil || change.Changed || change.VersionID != "" || change.VersionError != "checkpoint could not be safely published: possible credential in selected content" || len(h.changes(t, issue.ID)) != 0 {
					t.Fatal("unsafe source was published or lost its capture refusal")
				}
				return
			}
			if test.AdvanceTarget {
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				owner := execution.(*ownedExecution).CompletionData()
				wantBase := targetHead
				if !test.Staged && !test.Commit {
					wantBase = expected.Versions[len(expected.Versions)-1].BaseSHA
				}
				if targetHead == "" || stored.BaseSHA != wantBase || stored.HeadSHA != execution.(*ownedExecution).CompletionSnapshot().WorktreeHead || stored.AttemptID != owner.AttemptID || stored.Producer.FencingToken != execution.Recovery().Lease.FencingToken {
					t.Fatalf("final diff lost prepared source or fenced attempt identity: %+v, target=%s", stored, targetHead)
				}
				if test.Staged || test.Commit {
					if stored.HeadSHA == stored.BaseSHA || len(stored.Files) != 1 || stored.Files[0].Path != "CHANGE.md" {
						t.Fatalf("final diff did not preserve only the owned issue delta: %+v", stored)
					}
				} else {
					version := expected.Versions[len(expected.Versions)-1]
					if stored.HeadSHA != version.HeadSHA || len(stored.Files) != 1 || stored.Files[0].Path != "PRESERVED.md" || stored.Files[0].Additions != 1 || stored.Files[0].Deletions != 0 {
						t.Fatalf("clean Rework attributed upstream work to the issue: %+v", stored)
					}
					detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if err != nil || !reflect.DeepEqual(detail, *expected) || change == nil || change.VersionID != version.ID || change.Reviewed || change.Landing != nil || change.VersionError != "" {
						t.Fatalf("clean Rework changed historical provenance or waived refusal: change=%+v detail=%+v error=%v", change, detail, err)
					}
					h.complete(t, issue.ID, change)
					if h.state(t, issue.ID) != test.WantState || len(h.changes(t, issue.ID)) != test.WantChanges {
						t.Fatal("clean Rework published upstream work or lost review handoff")
					}
					return
				}
			}
			if test.Validator != "" {
				verdict := test.Validator
				if test.WantVerdict != "" {
					verdict = test.WantVerdict
				}
				if change == nil || change.Validator == nil || change.Validator.Verdict != verdict || change.Validator.SessionID <= 0 || provider.calls != 2 {
					t.Fatalf("validator did not review the native version in a fresh session: change=%+v calls=%d", change, provider.calls)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				if test.ImportedVersion {
					if len(detail.Versions) != len(expected.Versions)+1 || len(detail.Reviews) != len(expected.Reviews)+1 {
						t.Fatal("imported source did not publish exactly one version and fresh review")
					}
					current := detail.Versions[len(detail.Versions)-1]
					owner := execution.(*ownedExecution).CompletionData()
					if current.AttemptID != owner.AttemptID || current.RunID != owner.RunID || current.HeadSHA != expected.Versions[len(expected.Versions)-1].HeadSHA || !reflect.DeepEqual(detail.Versions[:len(expected.Versions)], expected.Versions) || !reflect.DeepEqual(detail.Reviews[:len(expected.Reviews)], expected.Reviews) {
						t.Fatal("imported source lost attempt provenance, unchanged head, or immutable history")
					}
					detail.Reviews = slices.DeleteFunc(slices.Clone(detail.Reviews), func(review tracker.ChangeReview) bool { return review.VersionID != change.VersionID })
				}
				if len(detail.Reviews) != 1 || detail.Reviews[0].Validator == nil || detail.Reviews[0].VersionID != change.VersionID || change.Validator.HeadSHA != change.HeadSHA {
					t.Fatalf("validator verdict was not pinned to the immutable version: %+v, %v", detail, err)
				}
				if test.DependencyOnly {
					receipts := detail.Reviews[0].Validator.Commands
					if len(receipts) != 4 {
						t.Fatalf("command evidence missing from native review: %+v", receipts)
					}
					if receipts[0].TimingStage != "finalization" || receipts[0].Command != receipts[1].Command {
						t.Fatalf("native review lost the finalization gate evidence: %+v", receipts)
					}
					for index, receipt := range receipts {
						wantExit := 0
						if test.FailedCheck && index == 3 {
							wantExit = 7
						}
						if receipt.HeadSHA != change.HeadSHA || receipt.TreeSHA == "" || receipt.TreeSHA != receipts[0].TreeSHA || receipt.ExitCode != wantExit || receipt.Output == "" {
							t.Fatalf("unverified native command receipt: %+v", receipt)
						}
					}
					if receipts[0].OutputTruncated != test.LargeOutput || receipts[1].OutputTruncated != test.LargeOutput {
						t.Fatalf("large output evidence not preserved: finalization=%t validation=%t", receipts[0].OutputTruncated, receipts[1].OutputTruncated)
					}
					if change.Reviewed == test.FailedCheck {
						t.Fatalf("review readiness ignored actual validation: %+v", change)
					}
				}

			}
			if test.FailVersion {
				if change == nil || change.ChangeID != expected.Change.ID || change.VersionID != "" || change.Reviewed || change.Error != "" || change.VersionCode != "invalid_request" || !strings.Contains(change.VersionError, "version publication refused") {
					t.Fatalf("runner lost refused publication result: %+v", change)
				}
				h.complete(t, issue.ID, change)
				evidence, readErr := h.latestRuntimeEvidence(t, tracker.NativeWorkItemID(issue.ID))
				if readErr != nil || evidence.Attempt == nil || evidence.Attempt.Finalization == nil || evidence.Attempt.Finalization.VersionError != change.VersionError || evidence.Attempt.Finalization.VersionCode != change.VersionCode || evidence.Attempt.ClaimReleasedAt == nil {
					t.Fatalf("publication refusal was omitted from the terminal receipt: %+v, %v", evidence.Attempt, readErr)
				}
				if f := evidence.Attempt.TerminalFailure; evidence.Attempt.Status != "failed" || f == nil || f.ErrorClass != "publication" || f.Stage != "publication" || f.Error != change.VersionCode+": "+change.VersionError {
					t.Fatalf("failed finalization ended without its cause: status=%s failure=%+v", evidence.Attempt.Status, f)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
				if err != nil || !reflect.DeepEqual(detail.Versions, expected.Versions) || detail.Change.CurrentVersion != expected.Change.CurrentVersion {
					t.Fatalf("refused publication replaced an immutable version: %+v, %v", detail, err)
				}
				if h.state(t, issue.ID) != test.WantState || len(h.candidatesIn(t, "Rework")) != 0 || provider.calls != 1 {
					t.Fatal("refused publication redispatched coding instead of review")
				}
				return
			}
			if test.WantNone {
				if change != nil {
					t.Fatalf("dirty work reported %#v; it takes the ordinary completion path", change)
				}
				if state := h.state(t, issue.ID); state != test.WantState || len(h.changes(t, issue.ID)) != 0 {
					t.Fatalf("dirty work moved to %s or opened a change", state)
				}
				return
			}
			if change == nil || change.Changed != test.WantChanged || (change.ChangeID != "") != test.WantChanged {
				t.Fatalf("native change = %#v, want changed = %t", change, test.WantChanged)
			}
			if test.Reopened {
				reviewed, err := h.connector.ReadChangeReview(t.Context(), issue.ID, change.ChangeID, change.VersionID, change.HeadSHA)
				if err != nil || !reviewed.Reviewed {
					t.Fatalf("completed Rework lost current review after an earlier landing: reviewed=%t, error=%v", reviewed.Reviewed, err)
				}
			}
			if test.Reconnect {
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
				if err != nil || len(detail.Versions) != 1 || len(h.changes(t, issue.ID)) != 1 || provider.calls != 1 {
					t.Fatalf("worker finalization did not publish once: detail=%+v calls=%d error=%v", detail, provider.calls, err)
				}
				owner := execution.(*ownedExecution).CompletionData()
				var stored tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/attempts/"+owner.AttemptID+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				if stored.AttemptID != owner.AttemptID || stored.Producer.LeaseID != owner.LeaseID || stored.Producer.FencingToken != owner.FencingToken || detail.Versions[0].AttemptID != owner.AttemptID || detail.Versions[0].RunID != owner.RunID {
					t.Fatalf("finalization lost current owner fencing: diff=%+v version=%+v", stored, detail.Versions[0])
				}
				if err := execution.Finish(t.Context(), "succeeded"); err != nil {
					t.Fatal(err)
				}
				replayed, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
				if err != nil || !reflect.DeepEqual(detail.Versions, replayed.Versions) || len(h.changes(t, issue.ID)) != 1 {
					t.Fatalf("repeated finalization republished the Change: %+v, %v", replayed, err)
				}
			}
			h.complete(t, issue.ID, change, test.Land)
			if state := h.state(t, issue.ID); state != test.WantState {
				t.Fatalf("state = %s, want %s", state, test.WantState)
			}
			changes := h.changes(t, issue.ID)
			if len(changes) != test.WantChanges {
				t.Fatalf("changes = %#v, want %d", changes, test.WantChanges)
			}
			if test.WantChanged {
				if changes[0].ID != change.ChangeID || changes[0].Title != "Update the README" || !test.Rework && !strings.Contains(changes[0].Body, change.HeadSHA) {
					t.Fatalf("change = %#v, reported %#v", changes[0], change)
				}
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatalf("read stored diff: %v", err)
				}
				expectedPath := map[bool]string{false: "CHANGE.md", true: "PRESERVED.md"}[test.Land && test.Rework && !test.LateConflict]
				if test.DependencyOnly {
					expectedPath = "frontend/package.json"
				}
				if stored.HeadSHA != change.HeadSHA || !test.Land && stored.BaseSHA != change.BaseSHA || !nativeDiffHas(stored.Files, expectedPath) {
					t.Fatalf("stored diff = %#v, reported %#v", stored, change)
				}
				head, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD").Output()
				if err != nil || strings.TrimSpace(string(head)) != stored.HeadSHA {
					t.Fatalf("published head is not finalized Git HEAD: %s, %v", head, err)
				}
				if !test.Rework || !test.Land || test.LateConflict || test.StaleBase {
					detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
					if err != nil {
						t.Fatal(err)
					}
					version := detail.Versions[len(detail.Versions)-1]
					owner := execution.(*ownedExecution).CompletionData()
					if version.ID != change.VersionID || version.ID != detail.Change.CurrentVersion || version.HeadSHA != stored.HeadSHA || version.BaseSHA != stored.BaseSHA || version.AttemptID != attemptID || version.RunID != owner.RunID || version.PolicyID != owner.PolicyID {
						t.Fatalf("published version lost finalized attempt/head/policy identity: %+v", version)
					}
					if test.SourceFixture == "synthetic" {
						bundle, err := h.admin.ChangeSource(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID, version.ID)
						if err != nil || version.Source == nil {
							t.Fatalf("published synthetic source unavailable: %v", err)
						}
						if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, bundle); err != nil {
							t.Fatal(err)
						}
						var paths []string
						for _, file := range stored.Files {
							paths = append(paths, file.Path)
						}
						slices.Sort(paths)
						if !slices.Equal(paths, []string{"CHANGE.md", "internal/hubserver/health_email_test.go", "internal/hubserver/slack_delivery_test.go"}) || stored.Producer.LeaseID != owner.LeaseID || stored.Producer.FencingToken != owner.FencingToken {
							t.Fatal("synthetic source lost approved paths or runner fencing")
						}
					}
				}
				if test.Rework {
					detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
					if err != nil {
						t.Fatal(err)
					}
					if test.Reopened && (detail.Change.CurrentLanding() != nil || !reflect.DeepEqual(detail.Change.Landed, expected.Change.Landed)) {
						t.Fatal("new publication lost the historical landing or treated it as current")
					}
					if test.Land && !test.LateConflict && !test.StaleBase && !test.Reopened && !test.ImportedVersion {
						if len(detail.Versions) != len(expected.Versions) || change.VersionID != expected.Change.CurrentVersion || change.HeadSHA != expected.Versions[len(expected.Versions)-1].HeadSHA {
							t.Fatalf("unchanged Rework replaced the immutable version: change=%+v, detail=%+v", change, detail)
						}
					} else if len(detail.Versions) != len(expected.Versions)+1 {
						t.Fatalf("rework did not publish one new version: %+v", detail)
					}
					if test.StaleBase {
						if change.HeadSHA == expected.Versions[len(expected.Versions)-1].HeadSHA || !reflect.DeepEqual(detail.Versions[:len(expected.Versions)], expected.Versions) || stored.HeadSHA == stored.BaseSHA || len(stored.Files) != 1 || stored.Files[0].Path != "PRESERVED.md" || !change.Reviewed {
							t.Fatalf("base refresh lost immutable history, exact-head review or issue-owned delta: change=%+v diff=%+v versions=%+v", change, stored, detail.Versions)
						}
					}
					historicalReviews := detail.Reviews
					if test.ImportedVersion {
						historicalReviews = detail.Reviews[:len(expected.Reviews)]
					}
					if !reflect.DeepEqual(historicalReviews, expected.Reviews) || !reflect.DeepEqual(detail.Discussion, expected.Discussion) {
						t.Fatal("new version changed historical feedback or fabricated review")
					}
					current := detail.Versions[len(detail.Versions)-1]
					if change.VersionID == "" || (!test.Land || test.LateConflict || test.StaleBase) && change.VersionID == expected.Change.CurrentVersion || detail.Change.CurrentVersion != change.VersionID || current.ID != change.VersionID || current.HeadSHA != stored.HeadSHA || current.PolicyID != h.descriptor.ID || current.Repository != nativeChangeRepository || current.Code.URI != nativeChangeRepository+"/commit/"+stored.HeadSHA {
						t.Fatalf("rework version lost final head or artifact authority: change=%+v, detail=%+v", change, detail)
					}
				}
			}
			if test.Land {
				candidates := h.candidatesIn(t, "Merging")
				if len(candidates) != 1 {
					t.Fatalf("landing candidates = %#v", candidates)
				}
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
					t.Fatal(err)
				}
				landing := h.scheduler.RunExecution(issue.ID)
				if test.BaseMoved {
					guarded, stop, err := landing.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer stop()
					if err := landing.(runner.LandingRuntimeExecution).StartLanding(guarded, 655, 0); err != nil {
						t.Fatal(err)
					}
					target, err := landing.(runner.LandingExecution).LandingTarget(guarded)
					if err != nil {
						t.Fatal(err)
					}
					info, err := backend.Create(guarded, workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier, Landing: &workspace.LandOptions{HeadSHA: target.HeadSHA, Repository: target.Repository}})
					if err != nil {
						t.Fatal(err)
					}
					mergeRequests := 0
					client, err := github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(issue.ID), DisableConditionalRequests: true, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response := httptest.NewRecorder()
						response.Header().Set("X-RateLimit-Remaining", "4991")
						switch request.Method {
						case http.MethodGet:
							if strings.HasSuffix(request.URL.Path, "/pulls/7") {
								fmt.Fprintf(response, `{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/native-change"}},"base":{"sha":%q,"ref":"main","repo":{"full_name":"example/native-change"}}}`, target.HeadSHA, info.Branch, change.BaseSHA)
							} else {
								response.WriteString("[]")
							}
						case http.MethodPost:
							fmt.Fprintf(response, `{"number":7,"state":"open","head":{"sha":%q,"ref":%q},"base":{"ref":"main"}}`, target.HeadSHA, info.Branch)
						case http.MethodPut:
							var body map[string]string
							if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["sha"] != target.HeadSHA || body["merge_method"] != target.Method || !strings.HasSuffix(request.URL.Path, "/pulls/7/merge") {
								t.Fatalf("merge mutation lost reviewed identity: %s, %v, %v", request.URL.Path, body, err)
							}
							mergeRequests++
							response.WriteHeader(http.StatusMethodNotAllowed)
							response.WriteString(`{"message":"Base branch was modified. Review and try the merge again."}`)
						default:
							t.Fatalf("unexpected fixture request: %s %s", request.Method, request.URL.Path)
						}
						return response.Result(), nil
					})}})
					if err != nil {
						t.Fatal(err)
					}
					_, err = backend.(workspace.GitHubPRLander).LandChangeViaGitHub(guarded, info, workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier}, workspace.LandOptions{HeadSHA: target.HeadSHA, Repository: target.Repository, Method: target.Method, GitHubClient: client})
					var refusal *workspace.LandRefusal
					var status *github.StatusError
					if !errors.As(err, &refusal) || refusal.Kind != workspace.LandRefusalBaseMoved || !errors.As(err, &status) || status.StatusCode != http.StatusMethodNotAllowed || mergeRequests != 1 {
						t.Fatalf("merge HTTP405 did not produce the typed base wait: %v", err)
					}
					if err := landing.(runner.LandingRuntimeExecution).ObserveLanding(guarded, runner.NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, RefusalKind: refusal.Kind, Refusal: refusal.Reason}); err != nil {
						t.Fatal(err)
					}
					if err := landing.Checkpoint(guarded, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: target.HeadSHA, ExternalEffect: "none", EffectState: "none"}); err != nil {
						t.Fatal(err)
					}
					if err := landing.Finish(guarded, "succeeded"); err != nil {
						t.Fatal(err)
					}
					previousLease := landing.Recovery().Lease
					if err := h.scheduler.ReleaseClaim(guarded, issue.ID, "waiting"); err != nil {
						t.Fatal(err)
					}
					stop()
					item := tracker.NativeWorkItemID(issue.ID)
					evidence, err := h.latestRuntimeEvidence(t, item)
					if err != nil || evidence.Attempt == nil || evidence.Attempt.Status != "succeeded" || evidence.Attempt.WorkItemRevision != 3 || evidence.Attempt.DispatchGeneration != 0 || evidence.Attempt.Checkpoint == nil || evidence.Attempt.Checkpoint.WorktreeState != "unpushed" || evidence.Attempt.Checkpoint.HeadSHA != target.HeadSHA || evidence.Attempt.Runtime == nil || evidence.Attempt.Runtime.Landing == nil || evidence.Attempt.Runtime.Landing.Landed || evidence.Attempt.Runtime.Landing.RefusalKind != workspace.LandRefusalBaseMoved || evidence.Change == nil || evidence.Change.Change.CurrentVersion != target.VersionID || evidence.Change.Change.Landed != nil || h.state(t, issue.ID) != "Merging" {
						t.Fatalf("waiting attempt lost truthful revision3 landing evidence: %+v, %v", evidence, err)
					}
					candidates = h.candidatesIn(t, "Merging")
					if len(candidates) != 1 || candidates[0].ID != issue.ID {
						t.Fatalf("successful unlanded wait disappeared from normal claim: %+v", candidates)
					}
					if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
						t.Fatal(err)
					}
					landing = h.scheduler.RunExecution(issue.ID)
					currentLease := landing.Recovery().Lease
					if currentLease.FencingToken <= previousLease.FencingToken {
						t.Fatal("landing continuation reused terminal authority")
					}

				}
				guarded, stop, err := landing.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				target, err := landing.(runner.LandingExecution).LandingTarget(guarded)
				if err != nil {
					t.Fatal(err)
				}
				item := tracker.NativeWorkItemID(issue.ID)
				previousLease := execution.Recovery().Lease
				currentLease := landing.Recovery().Lease
				for _, test := range []struct {
					name    string
					lease   tracker.NativeLease
					version string
				}{
					{name: "released lease", lease: previousLease, version: target.VersionID},
					{name: "stale fencing", lease: tracker.NativeLease{ID: currentLease.ID, FencingToken: currentLease.FencingToken - 1}, version: target.VersionID},
					{name: "foreign lease", lease: tracker.NativeLease{ID: "foreign-lease", FencingToken: currentLease.FencingToken}, version: target.VersionID},
					{name: "stale version", lease: currentLease, version: "version_stale"},
				} {
					if _, err := h.native.LandChangeVersion(t.Context(), item, target.ChangeID, test.version, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name, LeaseID: test.lease.ID, FencingToken: test.lease.FencingToken}, MergeSHA: target.HeadSHA, BaseRef: "main", Method: target.Method}); err == nil {
						t.Fatalf("landing accepted %s", test.name)
					}
				}
				result, err := agent.Run(t.Context(), runner.RunRequest{Execution: landing, ProjectID: "local", Issue: candidates[0], Mode: runner.RunModeMerge})
				if err != nil || result.NativeLanding == nil || !result.NativeLanding.Landed || result.NativeLanding.VersionID != change.VersionID || result.NativeLanding.HeadSHA != change.HeadSHA || h.state(t, issue.ID) != "Done" {
					t.Fatalf("preserved head was not genuinely landed: %+v, %v", result.NativeLanding, err)
				}
				if candidates := h.candidatesIn(t, "Merging"); len(candidates) != 0 {
					t.Fatalf("landed version was reclaimed: %+v", candidates)
				}
				path, expectedContent := "CHANGE.md", "changed\n"
				if test.DependencyOnly {
					path, expectedContent = "frontend/package.json", `{"dependencies":{}}`
				}
				if test.Rework && !test.LateConflict {
					path, expectedContent = "PRESERVED.md", "reviewed source\n"
				}
				calls := 1
				if test.Validator != "" {
					calls++
				}
				if test.LateConflict {
					calls = 2
				}
				content, err := exec.CommandContext(t.Context(), "git", "--git-dir", remote, "show", "main:"+path).Output()
				if err != nil || string(content) != expectedContent || provider.calls != calls {
					t.Fatalf("landing did not publish preserved source without another coding turn: %q, %v, calls=%d", content, err, provider.calls)
				}
				if test.LateConflict {
					head, err := exec.CommandContext(t.Context(), "git", "--git-dir", remote, "rev-parse", "main").Output()
					if err != nil || strings.TrimSpace(string(head)) != result.NativeLanding.MergeSHA {
						t.Fatalf("landing receipt did not identify the actual merge: %s, %v, receipt=%+v", head, err, result.NativeLanding)
					}
					attempts, err := h.native.AttemptsPage(t.Context(), tracker.NativeWorkItemID(issue.ID), "", 10)
					if err != nil || len(attempts.Items) != 3 {
						t.Fatalf("completed conflict journey lost authentic attempts: %+v, %v", attempts.Items, err)
					}
					for _, attempt := range attempts.Items {
						if attempt.Status != "succeeded" || attempt.PolicyID != h.descriptor.ID {
							t.Fatalf("host conflict became a failed or repinned attempt: %+v", attempt)
						}
					}
				}
			}
			if candidates := h.candidates(t); len(candidates) != 0 {
				t.Fatalf("the completed item is offered again: %#v", candidates)
			}
		})
	}
}

// committingAgent is a fake provider: it completes one turn, committing a
// file in the worktree first when commit is set.
type committingAgent struct {
	sourceFeedback   string
	sourceRecord     string
	sourceVerdict    string
	finalMessage     string
	validatorCommand string
	dependencyOnly   bool
	failedCheck      bool
	complete         bool
	inProgress       bool
	hold             bool
	lowScore         bool
	validator        string
	commit           bool
	dirty            bool
	staged           bool
	prompt           string
	workspace        string
	writableCalls    int
	calls            int
	bound            bool
	duringTurn       func()
	duringLiveTurn   func(runner.AgentTurnRequest, runner.AgentUpdateHandler)
	afterTurn        func()
	duringValidation func(runner.AgentTurnRequest)
}

func (t *changeFailingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/controls") {
		if failure := t.controlFailures.Load(); failure != 0 {
			if failure > 0 {
				next := failure - 1
				if next == 0 {
					next = -1
				}
				t.controlFailures.Store(next)
				t.controlsFailed <- int(failure)
			}
			if failure == 2 {
				return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("hub restarting")), Request: request}, nil
			}
			return nil, errors.New("hub disconnected")
		}
	}
	if t.failDetails.Load() && request.Method == http.MethodGet && (strings.Contains(request.URL.Path, "/changes/") || request.URL.Query().Get("view") == "recovery") {
		return nil, errors.New("change detail unavailable")
	}
	if request.Method == http.MethodPost {
		if t.failVersions.Load() && strings.HasSuffix(request.URL.Path, "/versions") {
			return nil, errors.New("version publication unavailable")
		}
		if t.failIntake.Load() && strings.HasSuffix(request.URL.Path, "/source-intake") {
			return nil, errors.New("source persistence unavailable")
		}
		if t.fail.Load() && strings.HasSuffix(request.URL.Path, "/changes") {
			return nil, errors.New("change creation unavailable")
		}
		if t.failDiffs.Load() && strings.HasSuffix(request.URL.Path, "/diff") {
			return nil, errors.New("diff storage unavailable")
		}
		if t.failEvents.Load() && strings.HasSuffix(request.URL.Path, "/events") {
			return nil, errors.New("run event unavailable")
		}
	}
	response, err := t.next.RoundTrip(request)
	if err == nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") && t.dropVersions.Swap(false) {
		response.Body.Close()
		return nil, errors.New("version acknowledgment lost")
	}
	return response, err
}

func nativeChangeSeedGit(ctx context.Context, dir, input string, args ...string) error {
	commandArgs := append([]string{"-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+dir)
	command.Stdin = strings.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	if len(args) > 0 && args[0] == "init" {
		for _, setting := range [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}} {
			if err := nativeChangeSeedGit(ctx, dir, "", "config", "--local", setting[0], setting[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func newNativeChangeHubConfigured(t *testing.T, review string, states []tracker.NativeState, inMemory, requireDependencies bool, config hubserver.Config, constructors Constructors, repositoryBackend ...hubserver.ReconcileBackend) *nativeChangeHub {
	newTestClient := func(t testing.TB, config Config) (*Client, error) { return createTestClient(t, config, constructors) }
	t.Helper()
	if config.Clock == nil {
		config.Clock = time.Now
	}
	config = hubclientPostgresConfig(t, config, states, requireDependencies)

	if config.Conversation != nil && config.Conversation.Enabled && config.Conversation.ArchiveContents == nil {
		conversationConfig := *config.Conversation
		conversationConfig.ArchiveContents = &conversationArchiveFixture{}
		config.Conversation = &conversationConfig
	}
	if config.Content == nil && (config.Hosted == nil || config.Hosted.SharedEntry == nil || config.Hosted.SharedEntry.Content == nil) {
		config.Content = newHubContentStore()
	}
	if len(repositoryBackend) > 0 {
		config.ReconcileBackend = repositoryBackend[0]
		config.ImportBackend, _ = repositoryBackend[0].(hubserver.ImportBackend)
	}
	if config.RunDetails == nil {
		config.RunDetails = &archivedRunDetails{}
	}
	service, err := hubserver.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	var requests sync.RWMutex
	serve := func(w http.ResponseWriter, r *http.Request) {
		requests.RLock()
		defer requests.RUnlock()
		if r.Method == http.MethodGet && (strings.HasSuffix(r.URL.Path, "/work-items") || r.URL.Query().Get("include") == "change") {
			if err := testsupport.NativeChangeRefreshBoard(r.Context(), service, tracker.OrganizationID(config.Hosted.OrganizationID)); err != nil {
				t.Errorf("refresh fixture board: %v", err)
				http.Error(w, "fixture board unavailable", 500)
				return
			}
		}
		service.Handler().ServeHTTP(w, r)
	}
	t.Cleanup(func() {
		requests.Lock()
		defer requests.Unlock()
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	var serverURL string
	var httpClient *http.Client
	if inMemory {
		serverURL = "https://native-hub.test"
		httpClient = &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			serve(recorder, request)
			return recorder.Result(), nil
		})}
	} else {
		server := httptest.NewServer(http.HandlerFunc(serve))
		t.Cleanup(server.Close)
		serverURL, httpClient = server.URL, server.Client()
	}
	admin, err := newTestClient(t, Config{URL: serverURL, TokenSource: func() string { return nativeChangeAdminToken }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	h := &nativeChangeHub{config: config, clock: config.Clock, review: review, organization: tracker.OrganizationID(config.Hosted.OrganizationID), project: "prj_example", descriptor: clientTestPolicy()}
	h.restart = func() {
		t.Helper()
		requests.Lock()
		defer requests.Unlock()
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		service, err = hubserver.Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
	}
	h.seed = func(seed func(tenantstore.Writer) error) {
		t.Helper()
		hubclientSeedPostgres(t, config, h.organization, seed)
	}
	h.prepareEvidence = func(row eventstore.Evidence, raw []byte) (eventstore.Evidence, error) {
		return eventstore.NewContent(config.RunDetails).PrepareEvidence(t.Context(), row, raw)
	}
	h.reopen = func(func(string) error) { t.Fatal("obsolete SQLite fixture seed") }
	if h.admin, err = admin.Native(h.organization, h.project); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	h.otherWorker = func(t *testing.T) *NativeClient {
		t.Helper()
		client, _ := hubclientEnrolledRunner(t, admin, h.organization, h.project, httpClient.Transport)
		native, err := client.Native(h.organization, h.project)
		if err != nil {
			t.Fatal(err)
		}
		return native
	}
	h.failChanges = &changeFailingTransport{next: httpClient.Transport}
	worker, machine := hubclientEnrolledRunner(t, admin, h.organization, h.project, h.failChanges)
	if h.native, err = worker.Native(h.organization, h.project); err != nil {
		t.Fatal(err)
	}
	operator, err := NewNativeConnector(h.admin)
	if err != nil {
		t.Fatal(err)
	}
	workerConnector, err := NewNativeConnector(h.native)
	if err != nil {
		t.Fatal(err)
	}
	h.connector = &hubclientFixtureConnector{NativeConnector: operator, admin: h.admin, worker: workerConnector}
	h.scheduler, err = newTestScheduler(worker, SchedulerConfig{
		OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project},
		Machine:           machine,
		HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second,
		LeaseHold: h.leaseHold,
		Now:       config.Clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const nativeChangeAdminToken = "native-change-admin"

// publish puts a version at the given head on the change as an operator
// would, so a rework run finds a current version to compare its head with.
func (h *nativeChangeHub) publish(t *testing.T, item tracker.NativeWorkItemID, changeID, head string, previous ...string) tracker.ChangeVersion {
	t.Helper()
	return h.publishCaptured(t, item, changeID, head, nil, previous...)
}

// nativeChangeHub is a real hub with a native project whose workflow has a
// review lane, and a worker client and scheduler that claim from it.
type nativeChangeHub struct {
	config          hubserver.Config
	leaseHolds      atomic.Int64
	clock           func() time.Time
	reopen          func(func(string) error)
	restart         func()
	seed            func(func(tenantstore.Writer) error)
	prepareEvidence func(eventstore.Evidence, []byte) (eventstore.Evidence, error)
	sourceDirectory string
	review          string
	organization    tracker.OrganizationID
	project         tracker.ProjectID
	descriptor      policy.Descriptor
	admin           *NativeClient
	native          *NativeClient
	connector       *hubclientFixtureConnector
	scheduler       *Scheduler
	failChanges     *changeFailingTransport
	otherWorker     func(*testing.T) *NativeClient
}

var nativeRuntimeDatabasePath = testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
	return store.Open(ctx, store.Config{Path: path})
})

// repolicy approves a changed repository policy, as an operator editing the
// project's definition between attempts does; the seeded review policy
// follows it, and versions published before it are stale.
func (h *nativeChangeHub) repolicy(t *testing.T) {
	t.Helper()
	next := h.descriptor
	next.ConfigDigest = policy.Digest([]byte("changed between attempts"))
	next = next.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: h.descriptor.ID, Policy: next}); err != nil {
		t.Fatal(err)
	}
	h.descriptor = next
}

func (a *committingAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if strings.HasPrefix(request.Prompt, progresscheck.FailureInstructions) {
		return runner.AgentTurnResult{}, onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: `{"outcome":"unknown","reason":"fake failure decision"}`})
	}

	if a.afterTurn != nil {
		defer a.afterTurn()
	}
	a.calls++
	if strings.Contains(request.Prompt, "independent Detent implementation reviewer") {
		if request.Resume.ThreadID != "" || request.Resume.SessionID != "" || !request.ReadOnly || !strings.Contains(request.Prompt, "Reviewed native version:") {
			return runner.AgentTurnResult{}, errors.New("native source reviewer did not start a fresh read-only version review")
		}
		if a.sourceFeedback != "" && (!strings.Contains(request.Prompt, a.sourceFeedback) || !strings.Contains(request.Prompt, "untrusted") || !strings.Contains(request.Prompt, a.sourceRecord)) {
			return runner.AgentTurnResult{}, errors.New("source reviewer omitted typed current feedback")
		}
		verdict := a.sourceVerdict
		if verdict == "" {
			verdict = "pass"
		}
		findings := "[]"
		if verdict == "rework" {
			findings = `[{"severity":"high","path":"CHANGE.md","body":"The required feedback repair is absent from this immutable source."}]`
		}
		if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: fmt.Sprintf(`{"verdict":%q,"score":1,"summary":"Checked source","findings":%s,"criteria_evidence":[]}`, verdict, findings)}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "thread-source-review", TurnID: "source-review-turn"}, nil
	}
	if strings.Contains(request.Prompt, "Detent validator-agent") {
		if request.Resume.ThreadID != "" || request.Resume.SessionID != "" || !request.ReadOnly || !strings.Contains(request.Prompt, "Reviewed native version:") {
			return runner.AgentTurnResult{}, errors.New("native validator did not start a fresh read-only version review")
		}
		score := .95
		if a.lowScore {
			score = .5
		}
		var evidence []gate.CriterionEvidence
		if match := regexp.MustCompile(`(?m)^receipt_id="([^"]+)" command="(?:\\.|[^"\\])*" head=(\S+) tree=(\S+) exit=0\b`).FindStringSubmatch(request.Prompt); len(match) == 4 {
			evidence = append(evidence, gate.CriterionEvidence{Criterion: "Complete the requested work.", Kind: "receipt", Reference: "Host validation", ReceiptIDs: []string{match[1]}, HeadSHA: match[2], TreeSHA: match[3], Behavior: "The configured fixture validation command succeeds on the published source."})
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			return runner.AgentTurnResult{}, err
		}
		if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: fmt.Sprintf(`{"verdict":%q,"score":%f,"summary":"Checked acceptance","findings":[],"criteria_evidence":%s}`, a.validator, score, encoded)}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "thread-validator", TurnID: "validator-turn"}, nil
	}
	a.writableCalls++
	a.prompt = request.Prompt
	a.workspace = request.Workspace
	a.bound = request.ConversationControl != nil
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: "thread-native", TurnID: "turn-1"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if a.duringTurn != nil {
		a.duringTurn()
	}
	if a.duringLiveTurn != nil {
		a.duringLiveTurn(request, onUpdate)
	}
	if a.dirty {
		if err := os.WriteFile(filepath.Join(request.Workspace, "SCRATCH.md"), []byte("draft\n"), 0o600); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	if a.commit || a.staged {
		if err := os.WriteFile(filepath.Join(request.Workspace, "CHANGE.md"), []byte("changed\n"), 0o600); err != nil {
			return runner.AgentTurnResult{}, err
		}
		commands := [][]string{{"add", "CHANGE.md"}}
		if a.dependencyOnly {
			if err := os.Remove(filepath.Join(request.Workspace, "CHANGE.md")); err != nil {
				return runner.AgentTurnResult{}, err
			}
			if err := os.WriteFile(filepath.Join(request.Workspace, "frontend/package.json"), []byte(`{"dependencies":{}}`), 0o600); err != nil {
				return runner.AgentTurnResult{}, err
			}
			commands = [][]string{{"add", "frontend/package.json"}}
		}

		if a.commit {
			commands = append(commands, []string{"commit", "-m", "change"})
		}
		for _, args := range commands {
			if output, err := exec.CommandContext(ctx, "git", append([]string{"-C", request.Workspace}, args...)...).CombinedOutput(); err != nil {
				return runner.AgentTurnResult{}, errors.Join(err, errors.New(string(output)))
			}
		}
	}
	message := "Finished the native work"
	if a.inProgress {
		message = "Native work remains unfinished\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
	}
	if a.complete {
		action := "null"
		if a.hold {
			action = "Approve the migration"
		}
		message += "\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: " + action + "\n```"
	}
	if a.finalMessage != "" {
		message = a.finalMessage
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ThreadID: "thread-native", TurnID: "turn-1", Delta: message, ItemID: "result"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: "thread-native", TurnID: "turn-1", Status: "completed"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: "thread-native", TurnID: "turn-1"}, nil
}

func isolateNativeChangeGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test User")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test User")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

// createInProgress files an item and moves it to In Progress, where the
// orchestrator's dispatch leaves a native item it runs.
func (h *nativeChangeHub) createInProgress(t *testing.T, title string) connector.Issue {
	t.Helper()
	issue, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: title, Body: "Change the README." + issueContractTestSections, State: "In Progress"})
	if err != nil {
		t.Fatal(err)
	}
	return api.IssueFromNative(issue)
}

func (h *nativeChangeHub) publishCapturedExternal(t *testing.T, item tracker.NativeWorkItemID, changeID, head string, captured *tracker.ChangeSourceCapture, external *tracker.ChangeExternalReference, previous ...string) tracker.ChangeVersion {
	t.Helper()
	digest := policy.Digest([]byte(head))
	var expectedVersionID string
	if len(previous) > 0 {
		expectedVersionID = previous[0]
	}
	base := nativeChangeFixtureHead("a")
	if len(previous) > 1 {
		base = previous[1]
	}
	var source *tracker.ChangeSource
	var bundle []byte
	if captured == nil && base != head {
		capture := nativeChangeSourceFixture(t.Context(), t, base, head, h.sourceDirectory)
		captured = &capture
	}
	if captured != nil {
		source, bundle = &captured.Source, captured.Bundle
	}
	version, err := h.admin.PublishChangeVersion(t.Context(), item, changeID, tracker.PublishChangeVersion{
		SourceBundle:      bundle,
		Mutation:          nativeMutationKey(),
		ExpectedVersionID: expectedVersionID,
		ChangeVersionInput: tracker.ChangeVersionInput{
			External: external,
			BaseSHA:  base, HeadSHA: head, MergeBaseSHA: base, Repository: nativeChangeRepository,
			Code:      tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: digest, Availability: "unverified"},
			Artifacts: []tracker.ChangeArtifact{}, PolicyID: h.descriptor.ID, Source: source,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func newHubContentStore() entryauthority.ContentHandler {
	var contentMu sync.Mutex
	contentObjects := make(map[string][]byte)
	return func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := request.Reference.Validate(request.Organization); err != nil {
			return nil, err
		}
		contentMu.Lock()
		defer contentMu.Unlock()
		if request.Write {
			contentObjects[request.Reference.Key] = bytes.Clone(request.Data)
			return nil, nil
		}
		data, found := contentObjects[request.Reference.Key]
		if !found {
			return nil, errors.New("archive unavailable")
		}
		return bytes.Clone(data), nil
	}
}

// changeFailingTransport refuses Change Request creation when armed, which is
// how a hub that cannot open the change looks to the runner.
type changeFailingTransport struct {
	next            http.RoundTripper
	fail            atomic.Bool
	failDiffs       atomic.Bool
	failEvents      atomic.Bool
	failIntake      atomic.Bool
	failDetails     atomic.Bool
	failVersions    atomic.Bool
	dropVersions    atomic.Bool
	controlFailures atomic.Int32
	controlsFailed  chan int
}

var nativeChangeBareRepo = nativeChangeRepositorySeed("bare", func(ctx context.Context, dir string) error {
	if err := nativeChangeSeedGit(ctx, dir, "", "init", "--bare", "--object-format=sha1", "--template="); err != nil {
		return err
	}
	if err := nativeChangeSeedGit(ctx, dir, "", "hash-object", "-w", "-t", "tree", "--stdin"); err != nil {
		return err
	}
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if err := nativeChangeSeedGit(ctx, dir, nativeChangeFixtureCommit(label), "hash-object", "-w", "-t", "commit", "--stdin"); err != nil {
			return err
		}
	}
	return nil
})

var nativeChangeSourceRepo = nativeChangeRepositorySeed("source", func(ctx context.Context, dir string) error {
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"config", "core.autocrlf", "false"},
		{"config", "commit.gpgsign", "false"}, {"config", "core.hooksPath", os.DevNull},
		{"config", "user.name", "Test User"}, {"config", "user.email", "test@example.com"},
	} {
		if err := nativeChangeSeedGit(ctx, dir, "", args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		return err
	}
	if err := nativeChangeSeedGit(ctx, dir, "", "add", "README.md"); err != nil {
		return err
	}
	return nativeChangeSeedGit(ctx, dir, "", "commit", "-m", "initial")
})

func (h *nativeChangeHub) publicationCI(t *testing.T) *NativeClient {
	t.Helper()
	rules, err := h.admin.ChangeReviewPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	client, err := createTestClient(t, Config{URL: fixtureClient(h.admin.Transport()).baseURL.String(), TokenSource: func() string { return "example-ci" }, HTTPClient: fixtureClient(h.admin.Transport()).httpClient})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	rules.PolicyID = h.descriptor.ID
	rules.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "full-ci", PrincipalID: "example_ci", WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), ExpectedID: rules.ID, Policy: rules}); err != nil {
		t.Fatal(err)
	}
	return ci
}

func (h *nativeChangeHub) publishCaptured(t *testing.T, item tracker.NativeWorkItemID, changeID, head string, captured *tracker.ChangeSourceCapture, previous ...string) tracker.ChangeVersion {
	t.Helper()
	return h.publishCapturedExternal(t, item, changeID, head, captured, nil, previous...)
}

func (h *nativeChangeHub) changes(t *testing.T, issueID string) []tracker.ChangeRequest {
	t.Helper()
	changes, err := h.admin.Changes(t.Context(), tracker.NativeWorkItemID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func (h *nativeChangeHub) leaseHold(context.Context) (func(), error) {
	h.leaseHolds.Add(1)
	return func() {}, nil
}

func (*committingAgent) SupportsLiveControl() bool { return true }

// complete moves the item the way the orchestrator's lane ledger does after a
// native run reports its change: along the hub workflow, through the adapter,
// while the claim is still held, and then releases the claim.
func (h *nativeChangeHub) complete(t *testing.T, issueID string, change *runner.NativeChange, allowLanding ...bool) {
	t.Helper()
	if change == nil {
		t.Fatal("the run reported no native change")
	}
	states, err := h.connector.WorkflowStates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	current, err := h.native.Issue(t.Context(), tracker.NativeWorkItemID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	target, ok := connector.CompletionLane(states, current.State, h.review, change.Changed || change.VersionError != "")
	if landing, allowed := connector.CompletionLane(states, current.State, "Merging", true); len(allowLanding) > 0 && allowLanding[0] && change.Changed && change.Reviewed && allowed {
		for _, state := range states {
			if state.Name == landing && state.Dispatchable {
				target, ok = landing, true
			}
		}
	}
	if !ok {
		t.Fatalf("no completion lane out of %s", current.State)
	}
	if err := h.connector.UpdateIssueState(t.Context(), issueID, target); err != nil {
		t.Fatal(err)
	}
	if err := h.scheduler.ReleaseClaim(t.Context(), issueID, "completed"); err != nil {
		t.Fatal(err)
	}
}

func versionsWithoutLanding(versions []tracker.ChangeVersion) []tracker.ChangeVersion {
	stripped := make([]tracker.ChangeVersion, len(versions))
	for i, version := range versions {
		version.Landing = nil
		stripped[i] = version
	}
	return stripped
}

func nativeDiffHas(files []tracker.AttemptDiffFile, path string) bool {
	for _, file := range files {
		if file.Path == path {
			return true
		}
	}
	return false
}

const nativeChangeRepository = "https://github.com/example/native-change"

func nativeChangeFixtureHead(label string) string {
	commit := nativeChangeFixtureCommit(label)
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("commit %d\x00%s", len(commit), commit))))
}

var nativeChangeFixtureRoot string
var repositoryRootOnce sync.Once

func confirmIssueContract(t *testing.T, admin *NativeClient, id string) {
	t.Helper()
	issue, err := admin.Issue(t.Context(), tracker.NativeWorkItemID(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.UpdateIssue(t.Context(), issue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: issue.Revision, Body: &issue.Body}); err != nil {
		t.Fatal(err)
	}
}

// Windows Git appends metadata beneath worktrees; testing's long subtest
// directory names can exhaust its path budget before Git creates that metadata.
func nativeChangeTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp("", "native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func nativeChangeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	nativeChangeGitWithContext(t.Context(), t, dir, args...)
}

func (h *nativeChangeHub) state(t *testing.T, issueID string) string {
	t.Helper()
	issue, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	return issue.State
}

type nativePublicationFixtureExecution struct {
	*ownedExecution
	keepGuard         func(func())
	forge             *github.Client
	losePush          *bool
	retainedHead      string
	beforePublication func(workspace.LandOptions)
	publicationFault  func(*workspace.LandOptions) func()
}

// claim fetches the only candidate and adopts it, as dispatch does.
func (h *nativeChangeHub) claim(t *testing.T, issueID string) connector.Issue {
	t.Helper()
	candidates := h.candidates(t)
	if len(candidates) != 1 || candidates[0].ID != issueID {
		t.Fatalf("candidates = %#v, want %s", candidates, issueID)
	}
	claim, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return claim.Issue
}

func (h *nativeChangeHub) candidates(t *testing.T) []connector.Issue {
	t.Helper()
	h.scheduler.ExecutionSource("local").NativeClient().ApplyHeartbeatChanges(nil)
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
	if err != nil {
		page, readErr := h.admin.Issues(t.Context(), nil)
		for _, issue := range page.Items {
			t.Logf("candidate refusal issue=%s state=%s contract=%+v sections=%v", issue.WorkItemID, issue.State, issue.IssueContract, config.IssueContractSectionDigests(issue.Body))
		}
		t.Logf("candidate refusal issue read: %v", readErr)
		t.Fatal(err)
	}
	return candidates
}

func nativeChangeFixtureCommit(label string) string {
	return "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Test User <test@example.com> 1 +0000\ncommitter Test User <test@example.com> 1 +0000\n\n" + label + "\n"
}

func nativeChangeSourceFixture(ctx context.Context, t *testing.T, base, head string, repositories ...string) tracker.ChangeSourceCapture {
	t.Helper()
	var directory string
	if len(repositories) > 0 {
		directory = repositories[0]
	}
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if head == nativeChangeFixtureHead(label) {
			directory = ""
			break
		}
	}
	if directory == "" {
		directory = nativeChangeBareRepo(t)
	}
	command := exec.CommandContext(ctx, "git", "-C", directory, "pack-objects", "--stdout", "--revs")
	command.Stdin = strings.NewReader(head + "\n")
	pack, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	bundle := append([]byte("# v2 git bundle\n"+head+" HEAD\n\n"), pack...)
	return tracker.ChangeSourceCapture{Source: tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}, Bundle: bundle}
}

const issueContractTestSections = "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."

func nativeChangeGitWithContext(ctx context.Context, t *testing.T, dir string, args ...string) {
	t.Helper()
	if !filepath.IsAbs(dir) {
		t.Fatalf("git fixture directory must be absolute: %q", dir)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func PublicationFixtureKeepsLeaseUntilHandoff(t *testing.T, factories Constructors) {
	t.Parallel()
	h := newScenarioHub(t, factories, true)
	issue := h.createInProgress(t, "Publication handoff")
	h.claim(t, issue.ID)
	owner := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
	stopGuard := func() {}
	fixture := &nativePublicationFixtureExecution{ownedExecution: owner, keepGuard: func(stop func()) {
		stopGuard = sync.OnceFunc(stop)
		t.Cleanup(stopGuard)
	}}
	guarded, returned, err := fixture.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer returned()
	if err := fixture.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Finish(guarded, "interrupted"); err != nil {
		t.Fatal(err)
	}
	returned()
	SetSchedulerNow(h.scheduler, time.Now().Add(2*h.scheduler.LeaseDuration()))
	if err := fixture.Validate(guarded); err != nil {
		t.Fatalf("publication guard did not retain renewable authority: %v", err)
	}
	if guarded.Err() != nil {
		t.Errorf("runner return stopped the publication guard: %v", context.Cause(guarded))
	}
	stopGuard()
	if !errors.Is(guarded.Err(), context.Canceled) {
		t.Fatalf("handoff left the old guard running: %v", guarded.Err())
	}
	if err := h.native.Release(t.Context(), owner.claim.lease, "released"); err != nil {
		t.Fatalf("publication handoff lost its lease: %v", err)
	}
}

func (e *nativePublicationFixtureExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	guarded, stop, err := e.ownedExecution.Guard(ctx)
	if err != nil || e.keepGuard == nil {
		return guarded, stop, err
	}
	e.keepGuard(stop)
	return guarded, func() {}, nil
}
