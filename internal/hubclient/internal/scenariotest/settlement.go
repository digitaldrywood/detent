package scenariotest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	checkpointstate "github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	"github.com/digitaldrywood/detent/internal/hubserver"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/explain"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type SettlementScenario struct {
	BudgetStage             string
	BudgetChangedSource     bool
	MergeResolution         bool
	Name                    string
	TransitionReason        string
	Role                    string
	Outcome                 string
	Worktree                string
	Source                  runner.AttemptDiffSource
	LoseLease               bool
	ValidationErr           error
	Restart                 bool
	LegacyRestart           bool
	OldCompletionFrame      bool
	RestoreRefusal          string
	FailCreate              bool
	RetainSource            bool
	EmptyRework             string
	SourceRefusal           bool
	FailDiff                bool
	FailDetail              bool
	FailVersion             bool
	DropVersion             bool
	VersionCode             string
	DiffCode                string
	Conversation            bool
	Land                    bool
	Integrate               bool
	IntegrateHead           bool
	IntegrateChanged        bool
	IntegrateAudit          bool
	IntegrationIncorporated bool
	IntegrationConflict     bool
	LandingRestart          bool
	ReviewDispositions      bool
	FinalMessage            string
	Disposition             *tracker.NativeDisposition
	ReleasedBlocked         bool
	UnreadFinal             bool
	RefreshFinal            bool
	CheckpointHead          string
	StaleFinalDiff          bool
	Existing                bool
	Published               string
	Repolicy                bool
	NoRemote                bool
	Reviewer                bool
	RequiredPR              bool
	CheckPRBeforeFinish     bool
	LostPRResponse          bool
	LostPush                string
	PublicationWriteRefusal string
	PublicationFenceLoss    int
	PublicationFailure      string
	PublicationStaged       bool
	PublicationRestore      bool
	PublicationUncertain    bool
	PublicationReference    string
	PublicationRefused      bool
	PublicationLanded       bool
	PublicationHistory      bool
	PublicationRecovery     bool
	ResumedPush             bool
	RecoveredPushFault      string
	WantChange              *runner.NativeChange
	WantDiagnostic          string
	WantChanges             int

	WantVersions int
}

func RunSettlement(t *testing.T, scenarios []SettlementScenario, factories Constructors, checks SettlementChecks) {
	newNativeChangeHub := func(t *testing.T, memory ...bool) *nativeChangeHub { return newScenarioHub(t, factories, memory...) }
	newNativeChangeHubTransport := func(t *testing.T, review string, states []tracker.NativeState, memory bool) *nativeChangeHub {
		return newNativeChangeHubConfigured(t, review, states, memory, false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken)}, factories)
	}

	repositoryRootOnce.Do(func() { nativeChangeFixtureRoot = os.Getenv("TMPDIR") })
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	t.Parallel()
	base := nativeChangeFixtureHead("a")
	head := nativeChangeFixtureHead("c")
	for _, test := range scenarios {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			var h *nativeChangeHub
			if test.ReleasedBlocked {
				h = newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
					{Name: "In Progress", Transitions: []string{"Todo", "In Review", "Done"}},
					{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
					{Name: "Done", Terminal: true},
				}, true)
			} else if test.Land {
				h = newNativeChangeHubTransport(t, "Human Review", []tracker.NativeState{
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
					{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
					{Name: "Human Review", Transitions: []string{"In Progress", "Done"}},
					{Name: "Done", Terminal: true},
				}, true)
			} else {
				h = newNativeChangeHub(t, true)
			}
			if test.IntegrateAudit {
				previous := h.descriptor.ID
				h.descriptor.Gates.SecurityAudit = true
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
					t.Fatal(err)
				}
			}
			issue := h.createInProgress(t, "Native change")
			if test.ReleasedBlocked {
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Todo"); err != nil {
					t.Fatal(err)
				}
				confirmIssueContract(t, h.admin, issue.ID)
			}
			item := tracker.NativeWorkItemID(issue.ID)
			base, head := base, head
			var publicationSource func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)
			var publicationCalls int
			var publicationPushes int
			publicationNumber := 7
			if test.PublicationLanded {
				publicationNumber = 8
			}
			var original tracker.ChangeDetail
			var ci *NativeClient
			var publicationOwner *ownedExecution
			var publicationPrevious string
			var publicationEarlier string
			var refusedPublication tracker.ChangeVersion
			var advancePublication func()
			var publicationHead func() string
			publicationCleared := false
			var replacePublicationHead func()
			var budgetCheckpoint *tracker.NativeCheckpoint
			var publicationCapture func(context.Context, string, string) (tracker.ChangeSourceCapture, error)
			stalePublication := test.PublicationRecovery
			publicationUnavailable := test.ResumedPush
			if test.RequiredPR {
				source := nativeChangeSourceRepo(t)
				remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
				nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
				nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
				nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
				nativeChangeGit(t, source, "push", "origin", "main")
				backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{ProcessSource: emptyProcessSource, Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				work := workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier}
				info, err := backend.Create(t.Context(), work)
				if err != nil {
					t.Fatal(err)
				}
				readHead := func(path string) string {
					output, err := exec.CommandContext(context.WithoutCancel(t.Context()), "git", "-C", path, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					return strings.TrimSpace(string(output))
				}
				base = readHead(info.Path)
				h.sourceDirectory = info.Path
				publicationCapture = func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return workspace.CaptureChangeSource(ctx, info.Path, base, head)
				}
				if test.PublicationLanded {
					if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("landed repair\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, info.Path, "add", "README.md")
					nativeChangeGit(t, info.Path, "commit", "-m", "landed repair")
					publicationEarlier = readHead(info.Path)
				}
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("existing repair\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, info.Path, "add", "README.md")
				if test.PublicationStaged {
					if _, err := backend.(workspace.NativeWorkFinalizer).FinalizeNativeWork(t.Context(), info, work, func(context.Context) error { return nil }); err != nil {
						t.Fatal(err)
					}
				} else {
					nativeChangeGit(t, info.Path, "commit", "-m", "existing repair")
				}
				if test.PublicationFailure == "push" {
					if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\nprintf publication-policy-refusal >&2\nexit 1\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				head = readHead(info.Path)
				advancePublication = func() {
					if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("further repair\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, info.Path, "add", "README.md")
					nativeChangeGit(t, info.Path, "commit", "-m", "further repair")
					head = readHead(info.Path)
					stalePublication = false
				}
				publicationBranch := info.Branch
				if test.PublicationReference != "" {
					publicationBranch = "manual/recovery"
					publicationPrevious = head
					if test.PublicationReference != "current" {
						treeRef := "HEAD^{tree}"
						if test.PublicationLanded {
							treeRef = "HEAD^1^{tree}"
						}
						tree, err := exec.CommandContext(t.Context(), "git", "-C", info.Path, "rev-parse", treeRef).Output()
						if err != nil {
							t.Fatal(err)
						}
						commit, err := exec.CommandContext(t.Context(), "git", "-C", info.Path, "commit-tree", strings.TrimSpace(string(tree)), "-p", base, "-m", "Retained recovery").Output()
						if err != nil {
							t.Fatal(err)
						}
						publicationPrevious = strings.TrimSpace(string(commit))
					}
					publishedHead := publicationPrevious
					if test.PublicationReference == "unknown" {
						publishedHead = base
					}
					nativeChangeGit(t, info.Path, "push", "origin", publishedHead+":refs/heads/"+publicationBranch)
					if test.PublicationLanded {
						publicationBranch = info.Branch
					}
				}
				test.Published, test.CheckpointHead = head, head
				if test.BudgetStage != "" {
					ref := tracker.CheckpointRefPrefix + issue.ID
					nativeChangeGit(t, info.Path, "update-ref", ref, head)
					tree, err := exec.CommandContext(t.Context(), "git", "-C", info.Path, "rev-parse", head+"^{tree}").Output()
					if err != nil {
						t.Fatal(err)
					}
					budgetCheckpoint = &tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "git_ref", WorktreeState: "unpushed", HeadSHA: head, CommitSHA: head, BaseSHA: base, TreeSHA: strings.TrimSpace(string(tree)), Ref: ref, ExternalEffect: "none", EffectState: "none"}
				}
				publicationHead = func() string {
					output, err := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+publicationBranch).Output()
					if err != nil {
						t.Fatal(err)
					}
					return strings.TrimSpace(string(output))
				}
				replacePublicationHead = func() {
					nativeChangeGit(t, remote, "update-ref", "refs/heads/"+publicationBranch, base)
				}
				test.WantChange = &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}
				test.Source = func(context.Context) (tracker.AttemptDiffRequest, bool) {
					return tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: head, Files: []tracker.AttemptDiffFile{{Path: "README.md", Status: "modified"}}}, true
				}
				client, err := github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if publicationUnavailable {
						return nil, errors.New("forge temporarily unavailable")
					}
					pullHead := head
					if test.PublicationReference != "" && !test.PublicationLanded {
						output, err := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+publicationBranch).Output()
						if err != nil {
							t.Fatal(err)
						}
						pullHead = strings.TrimSpace(string(output))
						if stalePublication {
							pullHead = publicationPrevious
						}
					}
					pull := fmt.Sprintf(`{"number":%d,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/native-change"}},"base":{"ref":"main","repo":{"full_name":"example/native-change"}}}`, publicationNumber, pullHead, publicationBranch)
					response := "[]"
					switch request.Method {
					case http.MethodGet:
						if test.PublicationLanded && strings.HasSuffix(request.URL.Path, "/pulls/7") {
							response = fmt.Sprintf(`{"number":7,"state":"closed","merged":true,"head":{"sha":%q,"ref":"manual/recovery","repo":{"full_name":"example/native-change"}},"base":{"ref":"main","repo":{"full_name":"example/native-change"}}}`, publicationPrevious)
							break
						}
						if test.PublicationLanded && strings.HasSuffix(request.URL.Path, "/pulls") && request.URL.Query().Get("head") != "example:"+publicationBranch {
							t.Fatal("reopened publication queried the historical branch")
						}
						if publicationCalls > 0 || test.PublicationReference != "" && !test.PublicationLanded {
							response = "[" + pull + "]"
							if strings.HasSuffix(request.URL.Path, fmt.Sprintf("/pulls/%d", publicationNumber)) {
								response = pull
							}
						}
					case http.MethodPost:
						publicationCalls++
						if test.PublicationLanded {
							var input struct {
								Head string `json:"head"`
								Base string `json:"base"`
							}
							if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Head != "example:"+publicationBranch || input.Base != "main" {
								t.Fatalf("reopened PR used the wrong publication identity: %+v, %v", input, err)
							}
						}
						if test.PublicationFailure == "auth" {
							return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"Resource not accessible by integration"}`))}, nil
						}
						if (test.LostPRResponse || test.BudgetStage == "ambiguous") && publicationCalls == 1 {
							return nil, errors.New("GitHub create response lost after publication")
						}
						response = pull
					default:
						t.Fatalf("held publication attempted %s %s", request.Method, request.URL)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				pushLost := false
				publicationSource = func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
					if test.LostPush != "" {
						record := opts.PublicationEffect
						opts.PublicationEffect = func(ctx context.Context, kind, state string, publication workspace.GitHubPublication) error {
							if kind == "git_push" && state == "pending" {
								publicationPushes++
							}
							if opts.UnresolvedEffect && opts.ExpectedPublicationHead == "" && state == "pending" && test.LostPush == "absent" && !publicationCleared {
								t.Fatal("publication retried before recording the absent external effect")
							}
							if err := record(ctx, kind, state, publication); err != nil {
								return err
							}
							publicationCleared = publicationCleared || state == "none"
							return nil
						}
					}
					if test.LostPush != "" && !pushLost {
						record := opts.PublicationEffect
						opts.PublicationEffect = func(ctx context.Context, kind, state string, publication workspace.GitHubPublication) error {
							if kind != "git_push" || state != "pending" || pushLost {
								return record(ctx, kind, state, publication)
							}
							pushLost = true
							if err := record(ctx, kind, state, publication); err != nil {
								return err
							}
							if test.LostPush == "present" {
								nativeChangeGitWithContext(ctx, t, info.Path, "push", "origin", version.HeadSHA+":refs/heads/"+info.Branch)
							}
							return errors.Join(errors.New("git push response lost before the remote recorded it"), record(ctx, kind, "ambiguous", publication))
						}
					}
					if test.PublicationFenceLoss > 0 {
						authorize := opts.Authorize
						calls := 0
						opts.Authorize = func(ctx context.Context) error {
							calls++
							if calls == test.PublicationFenceLoss {
								if err := h.native.Release(ctx, publicationOwner.claim.lease, "released"); err != nil {
									return err
								}
							}
							return authorize(ctx)
						}
					}
					opts.HeadSHA, opts.Repository, opts.TargetBranch, opts.Message, opts.GitHubClient = version.HeadSHA, version.Repository, "main", "Publish existing repair", client
					publisher := backend.(interface {
						PrepareGitHubPublication(context.Context, workspace.Info, workspace.Issue, workspace.LandOptions) (workspace.GitHubPublication, error)
					})
					return publisher.PrepareGitHubPublication(ctx, info, work, opts)
				}
				t.Cleanup(func() {
					wantCalls := 1
					if test.PublicationReference != "" && !test.PublicationLanded || test.PublicationUncertain || test.LostPush == "diverged" || test.PublicationFailure == "push" || test.BudgetChangedSource || test.PublicationFenceLoss > 0 && test.PublicationFenceLoss < 4 {
						wantCalls = 0
					}
					if readHead(info.Path) != head || readHead(source) != base || publicationCalls != wantCalls {
						t.Errorf("publication changed source, merged or duplicated a PR: calls=%d", publicationCalls)
					}
					if test.PublicationLanded {
						output, err := exec.CommandContext(context.WithoutCancel(t.Context()), "git", "-C", remote, "rev-parse", "refs/heads/manual/recovery").Output()
						if err != nil || strings.TrimSpace(string(output)) != publicationPrevious {
							t.Errorf("reopened publication changed the historical PR branch: %s, %v", output, err)
						}
					}
				})
			}
			if test.RequiredPR {
				previousPolicy := h.descriptor.ID
				h.descriptor.Gates.GitHubPullRequest = true
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{Policy: h.descriptor, ExpectedID: previousPolicy}); err != nil {
					t.Fatal(err)
				}
				rules, err := h.admin.ChangeReviewPolicy(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), ExpectedID: rules.ID, Policy: tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}}); err != nil {
					t.Fatal(err)
				}
			}
			if test.MergeResolution {
				rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
					t.Fatal(err)
				}
			}
			if test.Existing {
				earlier, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Earlier change"})
				if err != nil {
					t.Fatal(err)
				}
				if test.RequiredPR {
					ci = h.publicationCI(t)
				}
				passPublicationCheck := func(version tracker.ChangeVersion) {
					t.Helper()
					check := version.Checks[0]
					if _, err := ci.SubmitChangeCheck(t.Context(), item, earlier.ID, version.ID, tracker.SubmitChangeCheck{Mutation: nativeMutationKey(), ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: version.HeadSHA, RunID: version.RunID, PolicyID: version.PolicyID, ConfigDigest: version.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: version.CreatedAt, Evidence: []tracker.ChangeArtifact{{Kind: "test", URI: "s3://fixture/test", SHA256: policy.Digest([]byte("test receipt")), Availability: "available"}}}}); err != nil {
						t.Fatal(err)
					}
				}
				if test.Published != "" {
					var previous string
					var external *tracker.ChangeExternalReference
					if test.PublicationReference != "" {
						external = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: nativeChangeRepository + "/pull/7"}
						if test.PublicationReference != "current" {
							if test.PublicationLanded {
								older := h.publishCapturedExternal(t, item, earlier.ID, publicationEarlier, nil, external, "", base)
								previous = older.ID
							}
							if test.PublicationRefused {
								older := h.publishCapturedExternal(t, item, earlier.ID, nativeChangeFixtureHead("d"), nil, external, "", base)
								previous = older.ID
							}
							old := h.publishCapturedExternal(t, item, earlier.ID, publicationPrevious, nil, external, previous, base)
							if test.PublicationLanded {
								passPublicationCheck(old)
								if _, err := h.admin.ReviewChange(t.Context(), item, earlier.ID, old.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), ExpectedVersionID: old.ID, Decision: "approved", Body: "Land the reviewed source"}); err != nil {
									t.Fatal(err)
								}
								if _, err := h.admin.LandChangeVersion(t.Context(), item, earlier.ID, old.ID, tracker.LandChangeVersion{Mutation: nativeMutationKey(), MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}); err != nil {
									t.Fatal(err)
								}
								if h.state(t, issue.ID) != "Done" {
									t.Fatal("historical publication did not land")
								}
								currentIssue, err := h.admin.Issue(t.Context(), item)
								if err != nil {
									t.Fatal(err)
								}
								if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: "Todo", Reason: "user_requested"}); err != nil {
									t.Fatal(err)
								}
								confirmIssueContract(t, h.admin, issue.ID)
								if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
									t.Fatal(err)
								}
							}
							refusedPublication = old
							previous, external = old.ID, nil
						}
					}
					version := refusedPublication
					if !test.PublicationRecovery {
						version = h.publishCapturedExternal(t, item, earlier.ID, test.Published, nil, external, previous, base)
					}
					if test.RequiredPR {
						passPublicationCheck(version)
						if _, err := h.admin.ReviewChange(t.Context(), item, earlier.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), ExpectedVersionID: version.ID, Decision: "changes_requested", Body: "Human hold: prepare the missing PR, do not land this version"}); err != nil {
							t.Fatal(err)
						}
						original, err = h.admin.Change(t.Context(), item, earlier.ID)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if test.Repolicy {
					h.repolicy(t)
				}
			}
			if test.Reviewer && !test.RequiredPR && !test.MergeResolution {
				rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
					t.Fatal(err)
				}
			}
			if test.MergeResolution {
				stored := h.changes(t, issue.ID)
				detail, err := h.admin.Change(t.Context(), item, stored[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.ReviewChange(t.Context(), item, detail.Change.ID, detail.Change.CurrentVersion, tracker.ReviewChange{Mutation: nativeMutationKey(), ExpectedVersionID: detail.Change.CurrentVersion, Decision: "approved", Body: "Approved original source"}); err != nil {
					t.Fatal(err)
				}
			}
			candidate := h.claim(t, issue.ID)
			if test.ReleasedBlocked {
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
					t.Fatal(err)
				}
			}
			execution := h.scheduler.RunExecution(issue.ID)
			publicationOwner = execution.(*ownedExecution)
			if test.PublicationRecovery {
				publicationOwner.SetChangeSource(publicationCapture)
			}
			if publicationSource != nil {
				execution.(runner.PublicationSourceExecution).SetPublicationSource(publicationSource)
			}
			guarded, stop, err := execution.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if test.Source != nil {
				execution.(runner.DiffExecution).SetDiffSource(test.Source)
			}
			if test.MergeResolution || test.EmptyRework != "" {
				if _, err := execution.(runner.ChangeSourceExecution).RecoverChangeSource(guarded); err != nil {
					t.Fatal(err)
				}
			}
			var retainedCapture tracker.ChangeSourceCapture
			if test.RetainSource {
				retainedCapture = nativeChangeSourceFixture(t.Context(), t, base, head)
				if test.EmptyRework == "verified" {
					retainedCapture.Source.DiffSHA256 = tracker.ChangeSourceDigest(nil)
				}

				execution.(runner.ChangeSourceExecution).SetChangeSource(func(_ context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return retainedCapture, nil
				})
			}
			if !test.NoRemote {
				execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
			}
			if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: test.Role, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if test.IntegrateAudit {
				if err := execution.(runner.RuntimeExecution).ObserveRuntime(guarded, tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 41}}); err != nil {
					t.Fatal(err)
				}
			}
			if test.MergeResolution {
				if err := execution.(runner.MergeResolutionExecution).PrepareMergeResolution(); err != nil {
					t.Fatal(err)
				}
			}
			if test.MergeResolution {
				owner := execution.(*ownedExecution)
				previous := owner.CompletionSnapshot().RecoveredSource
				refusal := runner.NativeLanding{ChangeID: previous.ChangeID, VersionID: previous.VersionID, HeadSHA: previous.HeadSHA, RefusalKind: workspace.LandRefusalConflict, Refusal: "conflicting files: feature.txt", ConflictFiles: []string{"feature.txt"}, Path: "merge_resolution_pending"}
				if err := owner.ObserveLanding(guarded, refusal); err != nil {
					t.Fatal(err)
				}
				if err := checks.Observe(guarded, owner); err != nil {
					t.Fatal(err)
				}
				target, err := owner.LandingTarget(guarded)
				if err != nil || !target.MergeResolution || target.HeadSHA != previous.HeadSHA {
					t.Fatalf("conflict retry lost reviewed target: %#v, %v", target, err)
				}
				detail, err := h.admin.Change(t.Context(), item, previous.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				for _, version := range detail.Versions {
					if version.ID == previous.VersionID && (version.Landing == nil || !reflect.DeepEqual(version.Landing.ConflictFiles, refusal.ConflictFiles)) {
						t.Fatalf("conflict files were not retained: %#v", version.Landing)
					}
				}
			}

			if test.PublicationRefused {
				if err := execution.(runner.LandingRuntimeExecution).ObserveLanding(guarded, runner.NativeLanding{ChangeID: refusedPublication.ChangeID, VersionID: refusedPublication.ID, HeadSHA: refusedPublication.HeadSHA, RefusalKind: workspace.LandRefusalProtected}); err != nil {
					t.Fatal(err)
				}
				if err := checks.Observe(guarded, publicationOwner); err != nil {
					t.Fatal(err)
				}
				original, err = h.admin.Change(t.Context(), item, refusedPublication.ChangeID)
				if err != nil || original.Versions[1].Landing == nil || original.Versions[1].Landing.Landed {
					t.Fatalf("prior publication refusal was not retained: %v", err)
				}
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: test.Worktree, HeadSHA: test.CheckpointHead, ExternalEffect: "none", EffectState: "none"}
			if budgetCheckpoint != nil {
				checkpoint = *budgetCheckpoint
			}
			if test.PublicationUncertain {
				checkpoint.ExternalEffect, checkpoint.EffectState, checkpoint.EffectID = "provider_turn", "ambiguous", "effect_"+strings.Repeat("a", 32)
			}
			if err := execution.Checkpoint(guarded, checkpoint); err != nil {
				t.Fatal(err)
			}
			if test.RefreshFinal {
				execution.(runner.DiffExecution).SetDiffSource(nativeChangeDiff(head, "README.md"))
				checkpoint.HeadSHA = head
				if err := execution.Checkpoint(guarded, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			if test.PublicationRecovery {
				reopenPublication := func() {
					currentIssue, err := h.admin.Issue(t.Context(), item)
					if err != nil {
						t.Fatal(err)
					}
					for _, state := range []string{"In Review", "In Progress"} {
						currentIssue, err = h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: state, Reason: "user_requested"})
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				startPublication := func() *ownedExecution {
					candidates := h.candidatesIn(t, h.state(t, issue.ID))
					if len(candidates) != 1 {
						t.Fatalf("publication recovery candidates=%d", len(candidates))
					}
					if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
						t.Fatal(err)
					}
					resumed := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
					resumed.SetPublicationSource(publicationSource)
					resumed.SetDiffSource(test.Source)
					resumed.SetRepository(nativeChangeRepository)
					resumed.SetChangeSource(publicationCapture)
					if err := resumed.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleRework, Backend: "codex", Model: "test"}); err != nil {
						t.Fatal(err)
					}
					checkpoint.HeadSHA = head
					if err := resumed.Checkpoint(t.Context(), checkpoint); err != nil {
						t.Fatal(err)
					}
					return resumed
				}
				owner := execution.(*ownedExecution)
				if err := owner.PrepareFinish(guarded, "succeeded", "", nil); err != nil {
					t.Fatal(err)
				}
				sourceAttempt := owner.CompletionData().AttemptID
				if test.ResumedPush {
					if owner.NativeChange() == nil || owner.NativeChange().VersionID == "" || owner.CompletionSnapshot().Publication != nil || owner.CompletionData().Handoff == nil || owner.CompletionData().Handoff.ExternalEffect != "none" || publicationHead() != publicationPrevious {
						t.Fatalf("source attempt did not retain an unpushed version: change=%+v checkpoint=%+v", owner.NativeChange(), owner.CompletionData().Handoff)
					}
					sourceVersion := owner.NativeChange().VersionID
					if err := h.scheduler.ReleaseClaim(guarded, issue.ID, "failed"); err != nil {
						t.Fatal(err)
					}
					reopenPublication()
					publicationUnavailable = false
					owner = startPublication()
					if err := owner.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
						t.Fatal(err)
					}
					if owner.CompletionData().AttemptID == sourceAttempt || owner.NativeChange() == nil || owner.NativeChange().VersionID != sourceVersion {
						t.Fatalf("resume did not retain distinct source and recording attempt provenance: source=%s owner=%+v", sourceAttempt, owner.CompletionData())
					}
				}
				if owner.CompletionSnapshot().Publication != nil || owner.NativeChange() == nil || !strings.Contains(owner.NativeChange().VersionError, "identity mismatch") || owner.CompletionData().Handoff == nil || owner.CompletionData().Handoff.ExternalEffect != "git_push" || owner.CompletionData().Handoff.EffectState != "confirmed" {
					t.Fatalf("stale GET did not retain confirmed push without PR receipt: change=%+v checkpoint=%+v", owner.NativeChange(), owner.CompletionData().Handoff)
				}
				pushedVersion, pushedAttempt := owner.NativeChange().VersionID, owner.CompletionData().AttemptID
				pushedHead := head
				if publicationHead() != pushedHead {
					t.Fatal("stale forge observation did not follow a successful real push")
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
					t.Fatal(err)
				}
				if test.ResumedPush {
					detail, err := h.admin.Change(t.Context(), item, owner.NativeChange().ChangeID)
					if err != nil || len(detail.Versions) != 2 || detail.Versions[1].ID != pushedVersion || detail.Versions[1].AttemptID != sourceAttempt || detail.Versions[1].External != nil {
						t.Fatalf("resume changed immutable source provenance: detail=%+v error=%v", detail, err)
					}
					version := detail.Versions[1]
					recording, err := h.admin.NativeAttempt(t.Context(), item, pushedAttempt)
					if err != nil || recording.Checkpoint == nil || recording.Checkpoint.Change == nil || recording.Checkpoint.ExternalEffect != "git_push" || recording.Checkpoint.EffectState != "confirmed" {
						t.Fatalf("resume push has no durable confirmed checkpoint: recording=%+v error=%v", recording, err)
					}
					binding := tracker.NativePRPublication{ChangeID: version.ChangeID, VersionID: version.ID, Repository: version.Repository, HeadSHA: version.HeadSHA, PolicyID: version.PolicyID, BaseRef: "main", Branch: "manual/recovery", SourceVersion: tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}, SourceAttemptID: sourceAttempt}
					if *recording.Checkpoint.Change != binding.SourceVersion || recording.Checkpoint.HeadSHA != pushedHead || recording.Checkpoint.EffectID != binding.EffectID("git_push") {
						t.Fatalf("recorded push did not bind its distinct immutable source owner: checkpoint=%+v source=%+v", recording.Checkpoint, version)
					}
					t.Logf("source attempt %s created version %s; resume attempt %s durably confirmed push %s for source attempt %s; real remote head %s, no PR receipt", sourceAttempt, pushedVersion, pushedAttempt, recording.Checkpoint.EffectID, binding.SourceAttemptID, publicationHead())
				}
				advancePublication()

				h.repolicy(t)
				rules, err := h.admin.ChangeReviewPolicy(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				rules.PolicyID = h.descriptor.ID
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), ExpectedID: rules.ID, Policy: rules}); err != nil {
					t.Fatal(err)
				}

				publicationUnavailable = true
				intervening := startPublication()
				if err := intervening.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil || intervening.NativeChange() == nil || intervening.NativeChange().VersionID == "" || intervening.NativeChange().VersionID == pushedVersion || intervening.CompletionSnapshot().Publication != nil {
					t.Fatalf("could not retain later unpublished version: change=%+v error=%v", intervening.NativeChange(), err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
					t.Fatal(err)
				}
				publicationUnavailable = false
				reopenPublication()
				resumed := startPublication()
				for _, attempt := range resumed.Recovery().Attempts {
					if attempt.AttemptID == pushedAttempt {
						t.Fatal("fixture did not displace historical push from compact recovery")
					}
				}
				transport := fixtureClient(h.native.Transport()).requests.Load()
				historicalReads := 0
				historicalChangeID := owner.NativeChange().ChangeID
				historicalData := owner.CompletionData()
				retainedHead := pushedHead
				if test.RecoveredPushFault == "remote" {
					replacePublicationHead()
					retainedHead = base
				}
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					response, err := transport.RoundTrip(request)
					if err != nil || request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/changes/"+historicalChangeID) {
						return response, err
					}
					var result operatortool.ChangeResult
					if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					detail := result.Detail
					if detail == nil {
						t.Fatal("runtime Change read omitted detail")
					}
					var proof *tracker.ChangePublicationPush
					for i := range detail.PublicationPushes {
						if detail.PublicationPushes[i].Change.VersionID == pushedVersion {
							proof = &detail.PublicationPushes[i]
						}
					}
					if proof != nil && proof.RecordingAttemptID != pushedAttempt {
						t.Fatalf("historical push recorder was not projected: pushes=%+v", detail.PublicationPushes)
					}
					if proof != nil {
						historicalReads++
						switch test.RecoveredPushFault {
						case "missing":
							detail.PublicationPushes = nil
						case "digest":
							proof.EffectID = "effect_" + strings.Repeat("0", 32)
						case "source":
							proof.Change.VersionID = refusedPublication.ID
						case "policy":
							for i := range detail.Versions {
								if detail.Versions[i].ID == pushedVersion {
									detail.Versions[i].PolicyID = h.descriptor.ID
								}
							}
						case "base", "branch":
							binding := tracker.NativePRPublication{ChangeID: historicalChangeID, VersionID: pushedVersion, Repository: nativeChangeRepository, HeadSHA: pushedHead, PolicyID: historicalData.PolicyID, BaseRef: "main", Branch: "manual/recovery", SourceVersion: *historicalData.Handoff.Change, SourceAttemptID: pushedAttempt}
							if test.RecoveredPushFault == "base" {
								binding.BaseRef = "different-base"
							} else {
								binding.Branch = "different-branch"
							}
							proof.EffectID = binding.EffectID("git_push")
						}
					}
					body, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					response.Body = io.NopCloser(strings.NewReader(string(body)))
					return response, nil
				}))
				if err := resumed.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
					t.Fatal(err)
				}
				if historicalReads == 0 {
					t.Fatal("publication did not read the retained historical push authority")
				}
				if test.RecoveredPushFault != "" {
					if resumed.CompletionSnapshot().Publication != nil || resumed.NativeChange().VersionError == "" || publicationHead() != retainedHead || publicationCalls != 0 {
						t.Fatalf("invalid historical push changed the PR: fault=%s change=%+v publication=%+v", test.RecoveredPushFault, resumed.NativeChange(), resumed.CompletionSnapshot().Publication)
					}
					return
				}
				if resumed.CompletionSnapshot().Publication == nil || resumed.CompletionSnapshot().Publication.External.ID != "7" || resumed.CompletionSnapshot().Publication.HeadSHA != head || publicationCalls != 0 || publicationHead() != head {
					t.Fatalf("confirmed historical push could not advance the retained PR: source_attempt=%s recording_attempt=%s recording_attempt_reads=%d previous=%s/%s change=%+v publication=%+v creates=%d", sourceAttempt, pushedAttempt, historicalReads, pushedVersion, pushedHead, resumed.NativeChange(), resumed.CompletionSnapshot().Publication, publicationCalls)
				}
				if err := resumed.Finish(t.Context(), "succeeded"); err != nil {
					t.Fatal(err)
				}
				detail, err := h.admin.Change(t.Context(), item, resumed.NativeChange().ChangeID)
				if err != nil || len(detail.Versions) != 3 || detail.Versions[1].ID != pushedVersion || detail.Versions[1].External != nil || detail.Versions[2].External == nil || detail.Versions[2].External.ID != "7" || detail.Versions[2].HeadSHA != head || detail.Change.CurrentLanding() != nil || resumed.NativeChange().Reviewed {
					t.Fatalf("recovery did not persist only the current PR binding while retaining review gates: detail=%+v error=%v", detail, err)
				}
				return
			}
			if test.LoseLease {
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "lost"); err != nil {
					t.Fatal(err)
				}
			}
			if test.UnreadFinal {
				execution.(runner.DiffExecution).SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
					return tracker.AttemptDiffRequest{}, false
				})
			}
			if test.SourceRefusal {
				execution.(runner.ChangeSourceExecution).SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error) {
					return tracker.ChangeSourceCapture{}, fmt.Errorf(`%w: excluded source path ".detent/skills/split-issue.md"`, workspace.ErrCheckpointUnsafe)
				})
			}
			h.failChanges.fail.Store(test.FailCreate)
			h.failChanges.failDiffs.Store(test.FailDiff)
			h.failChanges.failDetails.Store(test.FailDetail)
			h.failChanges.failVersions.Store(test.FailVersion)
			h.failChanges.dropVersions.Store(test.DropVersion)
			if test.VersionCode != "" || test.DiffCode != "" {
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					code := ""
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") {
						code = test.VersionCode
					}
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/diff") {
						code = test.DiffCode
					}
					if code != "" {
						response := httptest.NewRecorder()
						response.Header().Set("Content-Type", "application/json")
						status := http.StatusTooManyRequests
						if code == "stale_fencing_token" {
							status = http.StatusConflict
						}
						response.WriteHeader(status)
						if err := json.NewEncoder(response).Encode(map[string]string{"code": code, "message": "producer refused publication"}); err != nil {
							return nil, err
						}
						return response.Result(), nil
					}
					return h.failChanges.RoundTrip(request)
				}))
			}
			if test.Conversation {
				owner := execution.(*ownedExecution)
				checks.SetConversation(owner)
			}
			if test.Disposition != nil {
				checks.DisableBatching(execution)
				transport := &executionTransport{next: fixtureClient(h.native.Transport()).requests.Load()}
				fixtureClient(h.native.Transport()).requests.Store(transport)
				transport.drop.Store(true)
				if err := execution.(runner.RuntimeExecution).ObserveRuntime(guarded, tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now()}); err != nil {
					t.Fatalf("temporary runtime publication failure escaped observation owner: %v", err)
				}
				if transport.drop.Load() || execution.(*ownedExecution).CompletionSnapshot().Pending == nil {
					t.Fatal("runtime acknowledgment loss did not retain a pending event")
				}
			}
			if test.PublicationRestore {
				before := execution.(*ownedExecution)
				if err := before.PrepareFinish(guarded, test.Outcome, "", nil); err != nil || before.CompletionSnapshot().Publication == nil {
					t.Fatalf("prepare publication for restored completion: %v", err)
				}
				saved := before.CompletionState()
				stop()
				h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: fixtureSchedulerMachine(h.scheduler), HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				publicationOwner = execution.(*ownedExecution)
				guarded = t.Context()
			}
			if test.PublicationUncertain {
				owner := execution.(*ownedExecution)
				if err := owner.PrepareFinish(guarded, test.Outcome, "", nil); err != nil || owner.CompletionSnapshot().Publication != nil || owner.CompletionData().Outcome != "" || owner.CompletionData().Handoff.ExternalEffect != "provider_turn" || owner.CompletionData().Handoff.EffectState != "ambiguous" || publicationCalls != 0 || owner.NativeChange().Error != "" || owner.NativeChange().VersionCode != workspace.LandRefusalHeadMoved || !strings.Contains(owner.NativeChange().VersionError, "unresolved provider_turn effect") {
					t.Fatalf("unrelated effect ambiguity was not a recorded head_moved refusal: %+v, %+v, %v", owner.CompletionData(), owner.NativeChange(), err)
				}
				return
			}
			if test.LandingRestart {
				before := execution.(*ownedExecution)
				if err := before.PrepareFinish(guarded, test.Outcome, test.FinalMessage, nil); err != nil || before.NativeChange() == nil || !before.Settled() || before.CompletionData().Outcome != "" {
					t.Fatalf("inline landing was not deferred before Finish: change=%+v outcome=%q error=%v", before.NativeChange(), before.CompletionData().Outcome, err)
				}
				saved := before.CompletionState()
				machine := fixtureSchedulerMachine(h.scheduler)
				h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				guarded = t.Context()
				after := execution.(*ownedExecution)
				if after.CompletionData().RunID != before.CompletionData().RunID || after.CompletionData().AttemptID != before.CompletionData().AttemptID || !sameCompletionLease(after.claim.lease, before.claim.lease) {
					t.Fatal("inline landing restart changed run authority")
				}
				if err := after.PrepareFinish(guarded, test.Outcome, test.FinalMessage, nil); err != nil {
					t.Fatal(err)
				}
				if err := execution.(runner.LandingRuntimeExecution).StartLanding(guarded, 42, 7); err != nil {
					t.Fatalf("restored inline landing runtime was refused: pending=%+v error=%v", after.CompletionSnapshot().Pending, err)
				}
				if after.CompletionSnapshot().Pending != nil || after.CompletionData().Runtime == nil || after.CompletionData().Runtime.Phase != "merging" || after.CompletionData().Finalization != nil || after.CompletionData().Disposition != nil || after.CompletionData().TerminalFailure != nil {
					t.Fatalf("inline landing did not record live runtime without terminal fields: %+v pending=%+v", after.CompletionData(), after.CompletionSnapshot().Pending)
				}
			}
			finish := execution.Finish
			if test.CheckPRBeforeFinish {
				owner := execution.(*ownedExecution)
				_, handler := owner.AgentTools()
				read := func(name string, arguments any) runner.AgentToolResult {
					t.Helper()
					raw, err := json.Marshal(arguments)
					if err != nil {
						t.Fatal(err)
					}
					result, err := handler(guarded, runner.AgentToolCall{Name: name, Arguments: raw})
					if err != nil {
						t.Fatal(err)
					}
					return result
				}
				refs, err := h.native.PullRequestReferences(guarded, item)
				if err != nil || slices.ContainsFunc(refs, func(ref api.NativePullRequestReference) bool { return ref.Number > 0 }) {
					t.Fatalf("unconfirmed publication acquired linkage: %+v error=%v", refs, err)
				}
				if err := owner.PrepareFinish(guarded, test.Outcome, "", nil); err != nil {
					t.Fatal(err)
				}
				if owner.CompletionSnapshot().Publication == nil || owner.CompletionData().Finalization != nil || owner.CompletionData().Outcome != "" {
					t.Fatalf("fixture must confirm real publication before attempt completion: publication=%+v change=%+v data=%+v", owner.CompletionSnapshot().Publication, owner.NativeChange(), owner.CompletionData())
				}
				detail, err := h.admin.Change(t.Context(), item, owner.NativeChange().ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, version := range detail.Versions {
					if version.ID == owner.NativeChange().VersionID {
						found = true
						if version.External == nil || *version.External != owner.CompletionSnapshot().Publication.External {
							t.Fatalf("confirmed publication is missing from current version before acceptance: external=%+v expected=%+v summary=%+v", version.External, owner.CompletionSnapshot().Publication.External, detail.Summary)
						}
					}
				}
				if !found || detail.Change.CurrentVersion != owner.NativeChange().VersionID {
					t.Fatal("confirmed publication's current native version is missing")
				}
				result := read(operatortool.GetChange, operatortool.ChangeArguments{ProjectID: string(h.project), ItemID: string(item), ChangeID: owner.NativeChange().ChangeID})
				if !strings.Contains(result.Content, owner.CompletionSnapshot().Publication.External.URL) {
					t.Fatalf("worker read lost confirmed PR before completion: %s", result.Content)
				}
				refs, err = h.native.PullRequestReferences(guarded, item)
				if err != nil || !slices.ContainsFunc(refs, func(ref api.NativePullRequestReference) bool {
					return ref.Number == 7 && ref.URL == owner.CompletionSnapshot().Publication.External.URL
				}) {
					t.Fatalf("cached PR references lost confirmed publication: %+v error=%v", refs, err)
				}
			}
			if test.PublicationLanded {
				owner := execution.(*ownedExecution)
				if err := owner.PrepareFinish(guarded, test.Outcome, "", nil); err != nil || owner.CompletionSnapshot().Publication == nil {
					t.Fatalf("reopened publication did not produce a new PR: %+v, %v", owner.NativeChange(), err)
				}
				receipt := *owner.CompletionSnapshot().Publication
				if receipt.External.ID != "8" || receipt.External.URL != nativeChangeRepository+"/pull/8" || receipt.Repository != nativeChangeRepository || receipt.BaseRef != "main" || receipt.HeadSHA != head || publicationHead() != head {
					t.Fatalf("reopened publication lost its exact new identity: %+v", receipt)
				}
				if err := checks.ReplayPublication(guarded, owner); err != nil || owner.CompletionSnapshot().Publication == nil || *owner.CompletionSnapshot().Publication != receipt || publicationCalls != 1 {
					t.Fatalf("reopened publication replay duplicated or changed the new PR: %+v, %v, creates=%d", owner.CompletionSnapshot().Publication, err, publicationCalls)
				}
			}
			if test.LostPush != "" {
				owner := execution.(*ownedExecution)
				if err := owner.PrepareFinish(guarded, test.Outcome, "", nil); err != nil || owner.CompletionSnapshot().Publication != nil || owner.NativeChange().Error == "" || owner.CompletionData().Handoff.ExternalEffect != "git_push" || owner.CompletionData().Handoff.EffectState != "ambiguous" || owner.CompletionData().Outcome != "" {
					t.Fatalf("lost push was not retained as an ambiguous effect: %+v, %v", owner.CompletionData(), err)
				}
				if test.PublicationReference != "" && owner.CompletionData().Handoff.ExpectedHeadSHA != publicationPrevious {
					t.Fatalf("lost push omitted expected remote head: got %s, want %s", owner.CompletionData().Handoff.ExpectedHeadSHA, publicationPrevious)
				}
				if test.LostPush == "diverged" {
					replacePublicationHead()
					if err := owner.Finish(guarded, test.Outcome); err != nil {
						t.Fatalf("diverged publication branch held the lease as unavailable: %v", err)
					}
					if !publicationCleared || owner.CompletionData().Handoff.UncertainForgeEffect() || owner.NativeChange().Error != "" || owner.NativeChange().VersionCode != workspace.LandRefusalHeadMoved || owner.CompletionSnapshot().Publication != nil || owner.CompletionData().Outcome != "failed" || publicationHead() != base {
						t.Fatalf("diverged publication branch was not a recorded head_moved failure: change=%+v outcome=%q", owner.NativeChange(), owner.CompletionData().Outcome)
					}
					return
				}
			}
			if test.BudgetStage != "" {
				owner := execution.(*ownedExecution)
				transport := fixtureClient(h.native.Transport()).requests.Load()
				refused, replayed := false, false
				var rejected tracker.NativeRunEvent
				var clockMu sync.Mutex
				clockNow := time.Now()
				budgetClient := h.native.Transport()
				budgetFixture := fixtureClient(budgetClient)
				budgetFixture.budgetNow = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clockNow }
				waits, maintenanceCalls, renewals, readRefusals := 0, 0, 0, 0
				budgetFixture.budgetTimer = func(delay time.Duration) (<-chan time.Time, func()) {
					if delay != time.Minute {
						t.Fatalf("budget timer did not honor Retry-After: %s", delay)
					}
					clockMu.Lock()
					clockNow = clockNow.Add(delay)
					now := clockNow
					clockMu.Unlock()
					waits++
					if err := budgetClient.Request(guarded, http.MethodPost, "/projects/example/leases/lease/renew", nil, nil); err != nil {
						t.Fatal(err)
					}
					if err := budgetClient.Request(guarded, http.MethodGet, "/budget-maintenance", nil, nil); err != nil {
						t.Fatal(err)
					}
					fired := make(chan time.Time, 1)
					fired <- now
					return fired, func() {}
				}
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.URL.Path == "/projects/example/leases/lease/renew" {
						renewals++
						return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
					}
					if request.URL.Path == "/budget-maintenance" {
						maintenanceCalls++
						if maintenanceCalls == 1 {
							return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"code":"runner_request_budget"}`)), Request: request}, nil
						}
						return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
					}
					if test.BudgetStage == "read" && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") && readRefusals <= 3 {
						readRefusals++
						refused = true
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"code":"runner_request_budget"}`)), Request: request}, nil
					}
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") {
						payload, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						request.Body = io.NopCloser(bytes.NewReader(payload))
						var event tracker.NativeRunEvent
						if err := json.Unmarshal(payload, &event); err != nil {
							return nil, err
						}
						checkpoint := event.Data.Handoff
						matches := test.BudgetStage == "completion" && event.Type == "run.finished"
						if checkpoint != nil && event.Type == "run.checkpointed" {
							matches = matches || test.BudgetStage == "receipt" && checkpoint.Publication != nil || checkpoint.ExternalEffect == "pr_create" && checkpoint.EffectState == test.BudgetStage
						}
						if matches && !refused {
							refused, rejected = true, event
							return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"code":"runner_request_budget"}`)), Request: request}, nil
						}
						if refused && !replayed && test.BudgetStage != "read" {
							if event.Data.Sequence != rejected.Data.Sequence || event.IdempotencyKey != rejected.IdempotencyKey || !reflect.DeepEqual(event, rejected) {
								t.Fatal("replay changed event sequence, idempotency or payload")
							}
							replayed = true
						}
					}
					return transport.RoundTrip(request)
				}))
				if err := owner.PrepareFinish(guarded, test.Outcome, "", nil); err != nil {
					t.Fatal(err)
				}
				var finishErr error
				if test.BudgetStage == "completion" {
					finishErr = owner.Finish(guarded, test.Outcome)
				} else if owner.NativeChange().Error != "" {
					finishErr = fmt.Errorf("%w: %s", api.ErrUnavailable, owner.NativeChange().Error)
				}
				if !refused || !nativeTransportUnavailable(finishErr) || owner.CompletionData().Outcome == "failed" || owner.NativeChange().VersionError != "" {
					t.Fatalf("budget refusal became terminal: change=%+v error=%v refused=%v", owner.NativeChange(), finishErr, refused)
				}
				saved := owner.CompletionState()
				if test.BudgetChangedSource {
					advancePublication()
				}
				var before checkpointstate.State
				if err := json.Unmarshal(saved, &before); err != nil {
					t.Fatal(err)
				}
				machine := fixtureSchedulerMachine(h.scheduler)
				h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				guarded = t.Context()
				var restored checkpointstate.State
				if err := json.Unmarshal(execution.(*ownedExecution).CompletionState(), &restored); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(restored.Data.Handoff, before.Data.Handoff) || !reflect.DeepEqual(restored.Pending, before.Pending) || !reflect.DeepEqual(restored.Diff, before.Diff) || restored.WorktreeHead != before.WorktreeHead || restored.Repository != before.Repository {
					t.Fatal("restart changed source, effect, pending event or publication binding")
				}
				if after := execution.(*ownedExecution); after.CompletionData().AttemptID != owner.CompletionData().AttemptID || after.CompletionData().RunID != owner.CompletionData().RunID {
					t.Fatal("budget restart changed source attempt")
				}
				if test.BudgetStage == "ambiguous" {
					if _, err := execution.(runner.ChangeSourceExecution).RecoverChangeSource(guarded); err != nil {
						t.Fatal(err)
					}
				}
				execution.(runner.PublicationSourceExecution).SetPublicationSource(publicationSource)
				finish = execution.Finish
				if test.BudgetChangedSource {
					if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.Outcome, "", nil); err != nil {
						t.Fatal(err)
					}
					after := execution.(*ownedExecution)
					if after.CompletionSnapshot().Publication != nil || after.NativeChange().VersionError == "" || publicationCalls != 0 {
						t.Fatalf("changed source authorized publication: change=%+v publication=%+v creates=%d", after.NativeChange(), after.CompletionSnapshot().Publication, publicationCalls)
					}
					return
				}
				if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.Outcome, "", nil); err != nil || execution.(*ownedExecution).NativeChange().Error != "" || execution.(*ownedExecution).NativeChange().VersionError != "" {
					t.Fatalf("resume failed: change=%+v checkpoint=%+v recovered=%+v diff=%+v error=%v", execution.(*ownedExecution).NativeChange(), execution.(*ownedExecution).CompletionData().Handoff, execution.(*ownedExecution).CompletionSnapshot().RecoveredSource, execution.(*ownedExecution).CompletionSnapshot().Diff, err)
				}
				if waits < 2 || maintenanceCalls < 2 || renewals != waits || test.BudgetStage != "read" && !replayed {
					t.Fatalf("cooldown/maintenance/replay missing: waits=%d maintenance=%d renewals=%d replayed=%t", waits, maintenanceCalls, renewals, replayed)
				}
			}

			if test.LostPRResponse {
				if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.Outcome, "", nil); err != nil {
					t.Fatal(err)
				}
				owner := execution.(*ownedExecution)
				if owner.CompletionSnapshot().Publication != nil || owner.NativeChange().Error == "" || owner.CompletionData().Handoff.EffectState != "ambiguous" || publicationCalls != 1 || owner.CompletionData().Outcome != "" {
					t.Fatalf("lost response was represented as success: %+v", owner.CompletionData())
				}
				detail, err := h.admin.Change(t.Context(), item, owner.NativeChange().ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				for _, version := range detail.Versions {
					if version.ID == detail.Change.CurrentVersion && version.External != nil {
						t.Fatal("ambiguous publication acquired current-version linkage")
					}
				}
			}

			if test.PublicationWriteRefusal != "" {
				owner := execution.(*ownedExecution)
				if err := owner.PrepareFinish(guarded, test.Outcome, "", nil); err != nil || owner.CompletionSnapshot().Publication == nil {
					t.Fatalf("prepare held publication: %v", err)
				}
				switch test.PublicationWriteRefusal {
				case "version":
					bundle, err := h.admin.ChangeSource(t.Context(), item, original.Change.ID, original.Change.CurrentVersion)
					if err != nil {
						t.Fatal(err)
					}
					input := original.Versions[0].ChangeVersionInput
					if _, err := h.admin.PublishChangeVersion(t.Context(), item, original.Change.ID, tracker.PublishChangeVersion{SourceBundle: bundle, Mutation: tracker.Mutation{IdempotencyKey: t.Name(), LeaseID: owner.claim.lease.ID, FencingToken: owner.claim.lease.FencingToken}, ExpectedVersionID: original.Change.CurrentVersion, ChangeVersionInput: input}); err != nil {
						t.Fatal(err)
					}
				case "fence":
					if err := h.native.Release(t.Context(), owner.claim.lease, "released"); err != nil {
						t.Fatal(err)
					}
				case "ambiguous":
					checkpoint := *owner.CompletionData().Handoff
					checkpoint.EffectState = "ambiguous"
					if err := owner.Checkpoint(guarded, checkpoint); err != nil {
						t.Fatal(err)
					}
				case "head":
					owner.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("d"), "README.md"))
				case "base":
					checks.TamperPublication(owner, "base")
				case "branch":
					checks.TamperPublication(owner, "branch")
				case "identity":
					checks.TamperPublication(owner, "identity")
				}
				if err := owner.Finish(guarded, test.Outcome); err == nil {
					t.Fatal("stale or ambiguous publication was persisted as success")
				}
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
				if err != nil || evidence.Attempt == nil || evidence.Attempt.Finalization != nil {
					t.Fatalf("refused publication leaked into finalization reads: %+v, %v", evidence, err)
				}
				return
			}
			if test.WantDiagnostic != "" || test.FinalMessage != "" {
				finish = func(ctx context.Context, outcome string) error {
					return execution.(runner.CompletionExecution).PrepareFinish(ctx, outcome, test.FinalMessage, nil)
				}
			}
			if test.StaleFinalDiff {
				owner := execution.(*ownedExecution)
				owner.SetDiffSource(nativeChangeDiff(base, "README.md"))
				if err := checks.StoreDiff(guarded, owner, owner.CompletionData().Sequence+1); err != nil {
					t.Fatal(err)
				}
				owner.SetDiffSource(test.Source)
			}
			finishErr := finish(guarded, test.Outcome)
			if test.IntegrateAudit && finishErr == nil {
				owner := execution.(*ownedExecution)
				change := owner.NativeChange()
				now := time.Now().UTC()
				audit := tracker.SecurityAuditReview{VersionID: change.VersionID, SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "integration-audit", ProjectID: string(h.project), IssueID: string(item), Repository: nativeChangeRepository, BaseSHA: change.BaseSHA, HeadSHA: change.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now, ExitStatus: securityaudit.ExitStatusSuccess, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
				if err := owner.RecordSecurityAudit(guarded, audit); err != nil {
					t.Fatal(err)
				}
				if !owner.NativeChange().Reviewed {
					t.Fatal("source audit did not approve the initial version")
				}
			}
			if test.StaleFinalDiff {
				owner := execution.(*ownedExecution)
				var stored tracker.AttemptDiff
				path, err := api.NativeAttemptDiffPath(owner.CompletionData().AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if err := h.native.Transport().Request(guarded, http.MethodGet, h.native.Scope()+path, nil, &stored); err != nil || stored.HeadSHA != owner.CompletionSnapshot().WorktreeHead {
					t.Fatalf("stored final diff differs from checkpoint: head=%s checkpoint=%s error=%v", stored.HeadSHA, owner.CompletionSnapshot().WorktreeHead, err)
				}
			}
			if test.PublicationFailure != "" {
				change := publicationOwner.NativeChange()
				if finishErr != nil || publicationOwner.CompletionSnapshot().Publication != nil || change == nil || change.VersionError == "" || change.Reviewed || change.VersionID != original.Change.CurrentVersion || !strings.Contains(change.VersionError, "preserve source") {
					t.Fatalf("publication failure lost its actionable blocker or current source: %+v, %v", change, finishErr)
				}
				if (change.VersionCode == workspace.LandRefusalHeadMoved) != (test.PublicationFailure == "identity") {
					t.Fatalf("publication failure code = %q for %s refusal", change.VersionCode, test.PublicationFailure)
				}
				detail, err := h.admin.Change(t.Context(), item, change.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				for _, version := range detail.Versions {
					if version.ID == detail.Change.CurrentVersion && version.External != nil {
						t.Fatal("failed publication acquired current-version linkage")
					}
				}
				h.complete(t, issue.ID, change)
				if h.state(t, issue.ID) != h.review {
					t.Fatal("publication blocker bypassed the existing review hold")
				}
				return
			}
			if test.PublicationFenceLoss > 0 {
				if !errors.Is(finishErr, runner.ErrExecutionAuthorityUnavailable) || publicationOwner.CompletionSnapshot().Publication != nil || publicationOwner.CompletionData().Outcome != "" {
					t.Fatalf("lost fence fabricated successful publication: %+v, %v", publicationOwner.CompletionData(), finishErr)
				}
				return
			}
			if test.FinalMessage != "" && finishErr == nil {
				if err := execution.(runner.RuntimeExecution).ObserveRuntime(guarded, tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: time.Now()}); err != nil {
					t.Fatalf("post-preparation runtime observation: %v", err)
				}
			}
			lostAuthority := test.LoseLease || test.DiffCode == "stale_fencing_token"
			if lostAuthority != (finishErr != nil) {
				t.Fatalf("finish error = %v, lease lost = %t", finishErr, test.LoseLease)
			}
			if lostAuthority {
				if !errors.Is(finishErr, runner.ErrExecutionAuthorityUnavailable) || execution.(runner.ChangeExecution).NativeChange() != nil {
					t.Fatalf("lost authority fabricated a native result: %v", finishErr)
				}
				return
			}
			if test.Land {
				if test.Integrate {
					stop()
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					integrationCandidate := h.claim(t, issue.ID)
					owner := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
					owner.SetRepository(nativeChangeRepository)
					ctx, release, err := owner.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					checkpointHead := base
					if test.IntegrateHead {
						checkpointHead = head
					}
					owner.SetDiffSource(nativeChangeDiff(checkpointHead))
					if test.IntegrateChanged {
						if !test.IntegrateHead {
							checkpointHead = nativeChangeFixtureHead("e")
						}
						owner.SetDiffSource(nativeChangeDiff(checkpointHead, "README.md"))
					}
					if err := owner.Start(ctx, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
						t.Fatal(err)
					}
					if err := owner.Checkpoint(ctx, tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "available", WorktreeState: "clean", HeadSHA: checkpointHead, ExternalEffect: "none", EffectState: "none"}); err != nil {
						t.Fatal(err)
					}
					integrations := 0
					owner.SetIntegrationSource(func(_ context.Context, version tracker.ChangeVersion, currentBase string) (workspace.LandResult, error) {
						if version.HeadSHA != head || currentBase != checkpointHead {
							t.Fatalf("integration changed source identity: version=%+v base=%s", version, currentBase)
						}
						integrations++
						if test.IntegrationConflict {
							return workspace.LandResult{}, &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, Reason: "the published source delta is not proven incorporated on the current integration base"}
						}
						if test.IntegrateChanged && !test.IntegrationIncorporated {
							return workspace.LandResult{}, &workspace.LandRefusal{Kind: workspace.LandRefusalNothing, Reason: "the current integration base does not incorporate the published source delta"}
						}
						return workspace.LandResult{MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}, nil
					})
					if err := owner.RecordUsage(ctx, tracker.NativeUsage{Provider: "openai", Model: "test", Input: 123, Output: 45}); err != nil {
						t.Fatal(err)
					}
					if err := owner.PrepareFinish(ctx, "succeeded", test.FinalMessage, nil); err != nil {
						t.Fatal(err)
					}
					if err := owner.PrepareFinish(ctx, "succeeded", test.FinalMessage, nil); err != nil {
						t.Fatal(err)
					}
					if test.IntegrateChanged && !test.IntegrationIncorporated {
						change := owner.NativeChange()
						wantIntegrations := 1
						if integrations != wantIntegrations || change == nil || change.VersionError != "" || change.Error != "" || change.Landing != nil || change.HeadSHA != checkpointHead || change.VersionID == "" {
							t.Fatalf("changed source did not publish: integrations=%d change=%+v", integrations, change)
						}
						wantVersions := 2
						if test.IntegrateHead {
							wantVersions = 1
						}
						detail, err := h.admin.Change(ctx, item, change.ChangeID)
						if err != nil || len(detail.Versions) != wantVersions || detail.Change.CurrentVersion != change.VersionID || detail.Change.CurrentLanding() != nil {
							t.Fatalf("changed source did not retain its new version: detail=%+v error=%v", detail, err)
						}
						if test.IntegrateAudit && !test.IntegrateHead && (change.Reviewed || detail.Summary.Status == "reviewed") {
							t.Fatal("new source reused the prior head's security audit")
						}
						if test.IntegrateHead && (!change.Reviewed || detail.Summary.Status != "reviewed") {
							t.Fatal("unchanged published source lost its existing audit")
						}
						if err := owner.Finish(ctx, "succeeded"); err != nil {
							t.Fatal(err)
						}
						if h.state(t, issue.ID) == "Done" || owner.CompletionData().Outcome != "succeeded" {
							t.Fatalf("publication did not finish for review: state=%s outcome=%s", h.state(t, issue.ID), owner.CompletionData().Outcome)
						}
						return
					}
					if test.IntegrationConflict {
						change := owner.NativeChange()
						if test.IntegrateHead {
							if integrations != 1 || change == nil || !change.Reviewed || change.VersionID == "" || change.VersionError != "" || change.Landing != nil || change.HeadSHA != head {
								t.Fatalf("same-head refusal discarded reviewed source: integrations=%d change=%+v", integrations, change)
							}
							return
						}
						if integrations != 1 || change == nil || change.VersionError != "" || change.Landing == nil || !change.Reviewed || change.VersionID == "" || change.HeadSHA != head || checkpointHead == head || owner.CompletionSnapshot().Diff.BaseSHA != owner.CompletionSnapshot().Diff.HeadSHA || len(owner.CompletionSnapshot().Diff.Files) != 0 {
							t.Fatalf("empty differing-head checkpoint did not enter corrective landing: integrations=%d change=%+v", integrations, change)
						}
						refusal := change.Landing
						if refusal.ChangeID != change.ChangeID || refusal.VersionID != change.VersionID || refusal.HeadSHA != head || refusal.Landed || refusal.RefusalKind != workspace.LandRefusalConflict || refusal.Path != "merge_resolution_pending" || !strings.Contains(refusal.Refusal, "not proven incorporated") {
							t.Fatalf("corrective landing lost the typed refusal or source: %+v", refusal)
						}
						release()
						ctx = t.Context()
						for _, observed := range []bool{false, true} {
							if observed {
								if err := owner.ObserveLanding(ctx, *refusal); err != nil {
									t.Fatal(err)
								}
								if err := owner.FlushRuntime(ctx); err != nil {
									t.Fatal(err)
								}
							}
							saved := owner.CompletionState()
							h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: fixtureSchedulerMachine(h.scheduler), HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
							if err != nil {
								t.Fatal(err)
							}
							if _, err := h.scheduler.RestoreCompletion(ctx, orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, integrationCandidate, saved); err != nil {
								t.Fatal(err)
							}
							owner = h.scheduler.RunExecution(issue.ID).(*ownedExecution)
							if !reflect.DeepEqual(owner.NativeChange().Landing, refusal) || owner.finalizationSource() == nil || owner.finalizationSource().VersionID != change.VersionID {
								t.Fatalf("completion restart lost corrective source: %+v", owner.NativeChange())
							}
						}
						if err := owner.ObserveLanding(ctx, *refusal); err != nil {
							t.Fatal(err)
						}
						if err := owner.Finish(ctx, "succeeded"); err != nil {
							t.Fatal(err)
						}
						if owner.CompletionData().Outcome != "succeeded" || owner.CompletionData().TerminalFailure != nil || h.state(t, issue.ID) == "Done" {
							t.Fatalf("source refusal became a publication failure: outcome=%s failure=%+v state=%s", owner.CompletionData().Outcome, owner.CompletionData().TerminalFailure, h.state(t, issue.ID))
						}
						if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
							t.Fatal(err)
						}
						h.claim(t, issue.ID)
						next := h.scheduler.RunExecution(issue.ID).(*ownedExecution)
						source, err := next.RecoverChangeSource(t.Context())
						if err != nil || source.Version.ID != change.VersionID || source.Version.HeadSHA != head || len(source.Bundle) == 0 {
							t.Fatalf("corrective attempt did not recover published source: source=%+v error=%v", source.Version, err)
						}
						target, err := next.LandingTarget(t.Context())
						if err != nil || !target.MergeResolution || !target.RebaseRequired || target.HeadSHA != head || target.VersionID != change.VersionID {
							t.Fatalf("corrective attempt did not enter bounded merge resolution: target=%+v error=%v", target, err)
						}
						return
					}
					if integrations != 1 || h.state(t, issue.ID) != "Done" {
						t.Fatalf("finalization did not integrate once: integrations=%d state=%s", integrations, h.state(t, issue.ID))
					}
					change := owner.NativeChange()
					if change == nil || change.Landing == nil || !change.Landing.Landed {
						t.Fatalf("integration lost its landing result: %+v", change)
					}
					if err := owner.ObserveLanding(ctx, *change.Landing); err != nil {
						t.Fatal(err)
					}
					if _, err := h.scheduler.RenewClaim(ctx, issue.ID, time.Now()); err != nil || ctx.Err() != nil {
						t.Fatalf("integrated finalization lost its lease: %v cause=%v", err, context.Cause(ctx))
					}
					if err := owner.Finish(ctx, "succeeded"); err != nil {
						t.Fatal(err)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
					if err != nil || evidence.Attempt == nil {
						t.Fatalf("reading integrated finalization: %v", err)
					}
					attempt := evidence.Attempt
					if attempt.Status != "succeeded" || attempt.Outcome != "succeeded" || attempt.ClaimReleasedAt == nil || evidence.CurrentLease != nil || evidence.Change == nil || evidence.Change.Change.CurrentLanding() == nil {
						t.Fatalf("integration did not finish and release: attempt=%+v lease=%+v", attempt, evidence.CurrentLease)
					}
					recovery, err := h.admin.Recovery(t.Context(), item)
					if err != nil || len(recovery.Attempts) != 2 || len(h.candidatesIn(t, "In Progress")) != 0 || len(h.candidatesIn(t, "Rework")) != 0 {
						t.Fatalf("repeated finalization scheduled another provider attempt: attempts=%d error=%v", len(recovery.Attempts), err)
					}
					if f := attempt.Finalization; f == nil || !f.Settled || !f.Reviewed || f.VersionID == "" || f.HeadSHA != head || f.Error != "" || f.VersionError != "" {
						t.Fatalf("integration lost host finalization: %+v", f)
					}
					if attempt.Runtime == nil || attempt.Runtime.Landing == nil || !attempt.Runtime.Landing.Landed || attempt.Runtime.Landing.MergeSHA != strings.Repeat("e", 40) || integrations != 1 {
						t.Fatalf("integration lost its receipt or ran again: runtime=%+v integrations=%d", attempt.Runtime, integrations)
					}
					if len(attempt.Usage) != 1 || attempt.Usage[0].Input != 123 || attempt.Usage[0].Output != 45 {
						t.Fatalf("integration lost usage: %+v", attempt.Usage)
					}
					return
				}
				if err := execution.(runner.LandingRuntimeExecution).StartLanding(guarded, 42, 7); err != nil {
					t.Fatal(err)
				}
				landing := execution.(runner.LandingExecution)
				target, err := landing.LandingTarget(guarded)
				if err != nil || target.HeadSHA != head || target.VersionID == "" || h.state(t, issue.ID) != "In Progress" {
					t.Fatalf("coding lease lost reviewed source: %+v, %v", target, err)
				}
				if err := landing.RecordLanding(guarded, runner.NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, Landed: true, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: target.Method}); err != nil {
					t.Fatal(err)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" || recovery.Attempts[0].Identity.Role != runner.RoleCode || h.state(t, issue.ID) != "Done" || len(h.candidates(t)) != 0 {
					t.Fatalf("direct landing restarted work or lost source identity: %+v, %v", recovery.Attempts, err)
				}
				return
			}
			if test.WantDiagnostic != "" {
				change := execution.(runner.ChangeExecution).NativeChange()
				if change == nil || !strings.Contains(change.Error+change.VersionError, test.WantDiagnostic) || change.Reviewed || change.VersionID != "" {
					t.Fatalf("native result lost publication failure: %#v", change)
				}
				if change.VersionError != "" && change.VersionCode != test.VersionCode && change.VersionCode != test.DiffCode && (!test.Repolicy || change.VersionCode != "policy_mismatch") {
					t.Fatalf("native result lost publication code: %#v", change)
				}
				if execution.(*ownedExecution).Settled() {
					before := *change
					for range 3 {
						if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.Outcome, test.FinalMessage, nil); err != nil {
							t.Fatal(err)
						}
						if got := execution.(runner.ChangeExecution).NativeChange(); !reflect.DeepEqual(got, &before) {
							t.Fatalf("settled publication changed refusal evidence: got %+v, want %+v", got, before)
						}
					}
				}
				h.failChanges.failDetails.Store(false)
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "running" {
					t.Fatalf("unavailable publication evidence recorded success: %#v, %v", recovery.Attempts, err)
				}
				stored := h.changes(t, issue.ID)
				if len(stored) != test.WantChanges {
					t.Fatalf("changes after failed preparation = %#v", stored)
				}
				if len(stored) != 0 {
					detail, err := h.admin.Change(t.Context(), item, stored[0].ID)
					if err != nil || len(detail.Versions) != test.WantVersions {
						t.Fatalf("failed preparation changed immutable versions: %#v, %v", detail, err)
					}
				}
				if h.state(t, issue.ID) != "In Progress" {
					t.Fatal("failed preparation changed the issue lane")
				}
				if test.FailCreate || test.FailDiff || test.FailDetail || test.FailVersion || test.DropVersion {
					if test.ValidationErr != nil {
						native := execution.(*ownedExecution)
						saved := native.CompletionState()
						interrupted, cancel := context.WithCancelCause(guarded)
						cancel(test.ValidationErr)
						_, err := h.scheduler.RestoreCompletion(interrupted, orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved)
						if !errors.Is(err, test.ValidationErr) || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
							t.Fatalf("operation interruption revoked deferred authority: %v", err)
						}
						var before, after nativeCompletionState
						if err := json.Unmarshal(saved, &before); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(native.CompletionState(), &after); err != nil {
							t.Fatal(err)
						}
						if before.PreparedFinalization == nil || after.PreparedFinalization == nil || before.PreparedFinalization.ObservedAt.IsZero() || after.PreparedFinalization.ObservedAt.IsZero() {
							t.Fatal("deferred completion lost its prepared finalization observation")
						}
						after.PreparedFinalization.ObservedAt = before.PreparedFinalization.ObservedAt
						if guarded.Err() != nil || !reflect.DeepEqual(before, after) || h.scheduler.RunExecution(issue.ID) != execution {
							t.Fatal("interrupted validation replaced authority or lost deferred completion")
						}
						if _, err := h.native.ValidateLease(t.Context(), native.claim.lease); err != nil {
							t.Fatalf("interrupted validation released original lease: %v", err)
						}
					}
					h.failChanges.fail.Store(false)
					h.failChanges.failDiffs.Store(false)
					h.failChanges.failDetails.Store(false)
					h.failChanges.failVersions.Store(false)
					if test.Restart {
						stop()
						before := execution.(*ownedExecution)
						saved := before.CompletionState()
						if test.OldCompletionFrame {
							var state nativeCompletionState
							if err := json.Unmarshal(saved, &state); err != nil {
								t.Fatal(err)
							}
							state.PreparedFinalization, state.PreparedFailure = nil, nil
							saved, err = json.Marshal(state)
							if err != nil {
								t.Fatal(err)
							}
						}
						if test.LegacyRestart {
							saved = nil
						}
						machine := fixtureSchedulerMachine(h.scheduler)
						if test.RestoreRefusal == "machine" {
							machine.ID = "another-machine"
						}
						if test.RestoreRefusal == "released" {
							if err := h.native.Release(t.Context(), before.claim.lease, "released"); err != nil {
								t.Fatal(err)
							}
						}
						if test.RestoreRefusal == "fence" {
							var state nativeCompletionState
							if err := json.Unmarshal(saved, &state); err != nil {
								t.Fatal(err)
							}
							state.Lease.FencingToken++
							saved, err = json.Marshal(state)
							if err != nil {
								t.Fatal(err)
							}
						}
						leasePolicy := h.descriptor
						if test.RestoreRefusal == "policy" {
							h.repolicy(t)
						}
						h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
						if err != nil {
							t.Fatal(err)
						}
						if test.RestoreRefusal == "existing" {
							if err := h.native.Release(t.Context(), before.claim.lease, "released"); err != nil {
								t.Fatal(err)
							}
							current := h.claim(t, issue.ID)
							if current.Metadata["hub_fencing_token"] == candidate.Metadata["hub_fencing_token"] {
								t.Fatal("replacement did not advance fencing")
							}
							currentExecution := h.scheduler.RunExecution(issue.ID)
							if err := currentExecution.Validate(t.Context()); err != nil {
								t.Fatal(err)
							}
							if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
								t.Fatalf("different valid claim inherited completed result: %v", err)
							}
							if h.scheduler.RunExecution(issue.ID) != currentExecution || currentExecution.(*ownedExecution).CompletionSnapshot().Diff != nil || currentExecution.(*ownedExecution).CompletionSnapshot().PreparedOutcome != "" {
								t.Fatal("refused completion mutated the replacement execution")
							}
							return
						}
						_, err = h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: leasePolicy, Repository: nativeChangeRepository}, candidate, saved)
						if test.RestoreRefusal != "" && test.RestoreRefusal != "renewed" && test.RestoreRefusal != "policy" {
							if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || len(h.scheduler.ActiveExecutions()) != 0 {
								t.Fatalf("invalid authority restored: claims=%v err=%v", h.scheduler.ActiveExecutions(), err)
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						if test.RestoreRefusal == "renewed" {
							if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, time.Now()); err != nil {
								t.Fatal(err)
							}
							if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
								t.Fatal(err)
							}
						}
						execution = h.scheduler.RunExecution(issue.ID)
						if got := execution.(*ownedExecution); got.CompletionData().RunID != before.CompletionData().RunID || got.CompletionData().AttemptID != before.CompletionData().AttemptID || got.claim.lease.ID != before.claim.lease.ID || got.claim.lease.FencingToken != before.claim.lease.FencingToken || checks.HasDiffSource(got) || got.CompletionSnapshot().Diff == nil || got.CompletionSnapshot().StoredSeq != before.CompletionSnapshot().StoredSeq {
							t.Fatalf("restoration changed execution identity or lost stored diff: %+v", got.CompletionData())
						}
						guarded = t.Context()
						if err := execution.Validate(guarded); err != nil {
							t.Fatal(err)
						}
					}
					if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.Outcome, "", nil); err != nil {
						t.Fatal(err)
					}
					if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.Outcome, "", nil); err != nil {
						t.Fatal(err)
					}
					republished := execution.(runner.ChangeExecution).NativeChange()
					if republished == nil || republished.Error != "" || republished.VersionError != "" || republished.VersionID == "" || republished.HeadSHA != head {
						t.Fatalf("publisher did not recover same source: %+v", republished)
					}
					stored := h.changes(t, issue.ID)
					if len(stored) != 1 {
						t.Fatalf("publication retry duplicated change: %+v", stored)
					}
					detail, err := h.admin.Change(t.Context(), item, stored[0].ID)
					wantVersions := 1
					if test.FailVersion || test.DropVersion {
						wantVersions = 2
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(detail.Versions) != wantVersions {
						t.Fatalf("publication retry duplicated versions: %+v", detail.Versions)
					}
					if test.RetainSource {
						bundle, err := h.admin.ChangeSource(t.Context(), item, stored[0].ID, republished.VersionID)
						if err != nil || !bytes.Equal(bundle, retainedCapture.Bundle) {
							t.Fatalf("deferred publication lost finalized source: %q, %v", bundle, err)
						}
					}
					h.complete(t, issue.ID, republished)
					recovery, err := h.admin.Recovery(t.Context(), item)
					if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" {
						t.Fatalf("publication recovery restarted attempt: %+v error=%v", recovery.Attempts, err)
					}
					return
				}
				if change.VersionError != "" {
					h.complete(t, issue.ID, change)
					if h.state(t, issue.ID) != h.review || len(h.candidates(t)) != 0 {
						t.Fatal("publication refusal returned to coding instead of review")
					}
				}
				return
			}
			changes := execution.(runner.ChangeExecution)
			checkChange := func(wantChanges, wantVersions int) {
				t.Helper()
				got := changes.NativeChange()
				if test.WantChange == nil {
					if got != nil {
						t.Fatalf("native change = %#v, want none", got)
					}
				} else {
					if got == nil {
						t.Fatal("native change = nil")
					}
					if got.Error != "" || got.Changed && got.ChangeID == "" {
						t.Fatalf("native change = %#v", got)
					}
					want := *test.WantChange
					want.ChangeID, want.Error, want.VersionID, want.VersionError, want.VersionCode, want.Reviewed = got.ChangeID, got.Error, got.VersionID, got.VersionError, got.VersionCode, got.Reviewed
					if *got != want {
						t.Fatalf("native change = %#v, want %#v", *got, want)
					}
					if got.ChangeID != "" {
						stored := h.changes(t, issue.ID)
						if stored[len(stored)-1].ID != got.ChangeID {
							t.Fatalf("reported change %s is not the item's change %#v", got.ChangeID, stored)
						}
						detail, err := h.admin.Change(t.Context(), item, got.ChangeID)
						if err != nil {
							t.Fatal(err)
						}
						if len(detail.Versions) != wantVersions {
							t.Fatalf("versions = %#v, want %d", detail.Versions, wantVersions)
						}
						if wantVersions == 0 {
							if got.VersionID != "" || got.Reviewed || !strings.Contains(got.VersionError, "https") {
								t.Fatalf("native change without a version = %#v", got)
							}
						} else {
							current := detail.Versions[len(detail.Versions)-1]
							if test.EmptyRework == "verified" && test.RetainSource {
								if current.Source == nil || current.Source.DiffSHA256 != tracker.ChangeSourceDigest(nil) || detail.Versions[0].HeadSHA != test.Published {
									t.Fatalf("empty revision lost captured source or rewrote its predecessor: %+v", detail.Versions)
								}
								bundle, err := h.admin.ChangeSource(t.Context(), item, got.ChangeID, current.ID)
								if err != nil || !bytes.Equal(bundle, retainedCapture.Bundle) {
									t.Fatalf("empty revision lost its retained bundle: %v", err)
								}
							}
							if test.ReviewDispositions {
								want := []tracker.ReviewDisposition{{HeadSHA: head, ThreadID: "T1", Disposition: "fixed", Evidence: "regression passes"}}
								if !reflect.DeepEqual(current.ReviewDispositions, want) {
									t.Fatalf("published dispositions=%+v, want %+v", current.ReviewDispositions, want)
								}
								target, err := execution.(runner.LandingExecution).LandingTarget(t.Context())
								if err != nil || !reflect.DeepEqual(target.ReviewDispositions, want) {
									t.Fatalf("landing dispositions=%+v error=%v", target.ReviewDispositions, err)
								}
							}

							if test.RequiredPR && (detail.Summary.NativeReview != "changes_requested" || current.ID != got.VersionID || len(detail.Versions) != test.WantVersions) {
								t.Fatal("publication-only recovery replaced the current version or discarded its human hold")
							}
							if test.RequiredPR && current.External == nil {
								t.Fatal("required PR no edit Rework retained native work but published no operator-visible GitHub PR")
							}
							runPublished := test.Published != got.HeadSHA
							if got.VersionID != current.ID || got.VersionError != "" || detail.Change.CurrentVersion != current.ID || current.HeadSHA != got.HeadSHA || current.BaseSHA != got.BaseSHA || runPublished && (current.RunID == "" || current.AttemptID == "") || current.Repository != nativeChangeRepository || current.Code.Kind != "code" || detail.Summary.Status != map[bool]string{false: "reviewed", true: "needs_evidence"}[test.Reviewer] || got.Reviewed == test.Reviewer {
								t.Fatalf("version = %#v for native change %#v (summary %#v)", current, got, detail.Summary)
							}
						}
					}
				}
				if stored := h.changes(t, issue.ID); len(stored) != wantChanges {
					t.Fatalf("changes = %#v, want %d", stored, wantChanges)
				}
			}
			checkChange(test.WantChanges, test.WantVersions)
			if test.RequiredPR {
				current, err := h.admin.Change(t.Context(), item, original.Change.ID)
				if err != nil {
					t.Fatal(err)
				}
				for i := range current.Versions {
					if original.Versions[i].External == nil {
						current.Versions[i].External = nil
					}
				}
				if !reflect.DeepEqual(current.Versions, original.Versions) || !reflect.DeepEqual(current.Reviews, original.Reviews) || !reflect.DeepEqual(current.Checks, original.Checks) || !reflect.DeepEqual(current.Change.Landed, original.Change.Landed) {
					t.Fatal("same-head publication changed immutable version, reviews or CI evidence")
				}
				var visible tracker.NativeIssue
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"?include=change", nil, &visible); err != nil {
					t.Fatal(err)
				}
				if visible.Change == nil || visible.Change.URL != fmt.Sprintf("%s/pull/%d", nativeChangeRepository, publicationNumber) || visible.PullRequest == nil || visible.PullRequest.Number != publicationNumber || visible.PullRequest.URL != visible.Change.URL {
					t.Fatalf("operator reads omitted the verified PR: %+v", visible)
				}
				var pulls []tracker.PullRequestView
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/work-items/"+issue.ID+"/pull-requests", nil, &pulls); err != nil {
					t.Fatal(err)
				}
				if len(pulls) != 1 || pulls[0].Number != publicationNumber || pulls[0].URL != visible.Change.URL {
					t.Fatalf("operator PR list omitted the verified publication: %+v", pulls)
				}
				target, err := execution.(runner.LandingExecution).LandingTarget(t.Context())
				if !errors.Is(err, runner.ErrLandingNotReviewed) || target.External == nil || target.External.URL != visible.Change.URL {
					t.Fatalf("publication lost the human hold or landing identity: %+v, %v", target, err)
				}
			}
			if again := execution.Finish(guarded, test.Outcome); test.LoseLease == (again == nil) {
				t.Fatalf("repeated finish error = %v", again)
			}
			checkChange(test.WantChanges, test.WantVersions)
			if test.LostPush == "present" {
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || publicationPushes != 1 || publicationCalls != 1 || publicationHead() != head {
					t.Fatalf("publication replay started new delivery: attempts=%d pushes=%d PR creates=%d error=%v", len(recovery.Attempts), publicationPushes, publicationCalls, err)
				}
			}

			evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
			if err != nil || evidence.Attempt == nil {
				t.Fatalf("finalizer projection unavailable: %+v, %v", evidence, err)
			}
			if test.RefreshFinal {
				var stored tracker.AttemptDiff
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, h.admin.Scope()+"/attempts/"+evidence.Attempt.AttemptID+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" || recovery.Attempts[0].TerminalFailure != nil || stored.HeadSHA != checkpoint.HeadSHA || stored.Generation.Seq != evidence.Attempt.Sequence {
					t.Fatalf("final checkpoint did not store and settle the final head once: diff=%+v attempts=%+v error=%v", stored, recovery.Attempts, err)
				}
			}
			if change := changes.NativeChange(); change != nil {
				f := evidence.Attempt.Finalization
				if f == nil || f.ChangeID != change.ChangeID || f.VersionID != change.VersionID || f.HeadSHA != change.HeadSHA || f.BaseSHA != change.BaseSHA || f.Changed != change.Changed || f.Files != change.Files || f.Reviewed != change.Reviewed || f.ObservedAt.IsZero() || !f.Settled {
					t.Fatalf("finalizer omitted recorded source outcome: %+v, change=%+v", f, change)
				}
			} else if evidence.Attempt.Finalization != nil || evidence.Attempt.FinalizationAvailability != "unavailable" {
				t.Fatalf("finalizer fabricated a missing source result: %+v", evidence.Attempt.Finalization)
			}
			if test.FinalMessage != "" {
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" {
					t.Fatalf("typed receipt lost provider outcome: %#v, %v", recovery.Attempts, err)
				}
				got := recovery.Attempts[0].Disposition
				if !reflect.DeepEqual(got, test.Disposition) {
					t.Fatalf("disposition=%#v, want %#v", got, test.Disposition)
				}
				page, err := h.admin.Attempts(t.Context(), item, "")
				if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].Disposition, test.Disposition) {
					t.Fatalf("attempts API disposition = %#v, error = %v", page.Items, err)
				}
			}
			if state := h.state(t, issue.ID); state != "In Progress" {
				t.Fatalf("the execution moved the item to %s; only the orchestrator moves lanes", state)
			}
			if test.ReleasedBlocked {
				if evidence.CurrentLease == nil || evidence.Scheduling.Outcome != "claimed" {
					t.Fatalf("workflow exclusion hid the current fenced owner: %+v", evidence.Scheduling)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
				if err != nil || evidence.CurrentLease != nil || evidence.Attempt == nil || evidence.Attempt.Current || evidence.Attempt.ClaimReleasedAt == nil || evidence.Attempt.Status != "succeeded" || !reflect.DeepEqual(evidence.Attempt.Disposition, test.Disposition) {
					t.Fatalf("released blocked attempt represented as active: %+v, %v", evidence, err)
				}
				if len(h.candidates(t)) != 0 {
					t.Fatal("nondispatchable In Progress unexpectedly became a candidate")
				}
				explanation := explain.FromNativeEvidence(evidence)
				if evidence.Scheduling.Outcome != "skipped" || !strings.Contains(evidence.Scheduling.Reason, "In Progress is not dispatchable") || explanation.Eligibility.Current == nil || !strings.Contains(explanation.Eligibility.Current.Reason, "In Progress is not dispatchable") {
					t.Fatalf("released blocked settlement exclusion is not actionable: scheduling=%+v eligibility=%+v", evidence.Scheduling, explanation.Eligibility)
				}
			}
			if test.PublicationHistory {
				newVersion := h.publish(t, item, original.Change.ID, head, original.Change.CurrentVersion, base)
				detail, err := h.admin.Change(t.Context(), item, original.Change.ID)
				if err != nil || len(detail.Versions) != 2 || detail.Versions[0].External == nil || detail.Versions[0].External.ID != "7" || detail.Versions[1].External != nil || detail.Change.CurrentVersion != newVersion.ID {
					t.Fatalf("historical publication was lost or represented as current-head publication: %+v, %v", detail, err)
				}
			}
			if test.Disposition != nil && test.Disposition.FinalSummary != "" {
				transitionReason := test.TransitionReason
				if transitionReason == "" {
					transitionReason = "worker_progress"
				}
				transitionDetail := "The completed turn requires review"
				if test.TransitionReason != "" {
					transitionDetail = test.Disposition.FinalSummary
				}
				transitionContext := connector.WithLaneTransitionReason(guarded, transitionReason, transitionDetail)
				if err := h.connector.UpdateIssueState(transitionContext, issue.ID, "In Review"); err != nil {
					t.Fatal(err)
				}
				history, err := h.admin.History(t.Context(), item, "")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, event := range history.Items {
					if event.Type == "workflow.transitioned" && event.Data.ToState == "In Review" {
						found = event.Data.Reason == transitionReason && event.Data.ReasonDetail == transitionDetail
					}
				}
				if !found {
					t.Fatalf("history lost the transition reason: %#v", history.Items)
				}
				if test.TransitionReason != "" {
					evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
					if err != nil {
						t.Fatal(err)
					}
					explanation := explain.FromNativeEvidence(evidence)
					if explanation.LatestTransition == nil || explanation.LatestTransition.Reason != transitionReason || explanation.LatestTransition.ReasonDetail != transitionDetail {
						t.Fatalf("explanation lost the validator transition: %+v", explanation.LatestTransition)
					}
				}
			}
		})
	}
}
