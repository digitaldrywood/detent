package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const nativeChangeAdminToken = "native-change-admin"

// nativeChangeHub is a real hub with a native project whose workflow has a
// review lane, and a worker client and scheduler that claim from it.
type nativeChangeHub struct {
	review       string
	organization tracker.OrganizationID
	project      tracker.ProjectID
	descriptor   policy.Descriptor
	admin        *NativeClient
	native       *NativeClient
	connector    *NativeConnector
	scheduler    *Scheduler
	failChanges  *changeFailingTransport
}

// changeFailingTransport refuses Change Request creation when armed, which is
// how a hub that cannot open the change looks to the runner.
type changeFailingTransport struct {
	next       http.RoundTripper
	fail       atomic.Bool
	failDiffs  atomic.Bool
	failEvents atomic.Bool
	failIntake atomic.Bool
}

func (t *changeFailingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost {
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
	return t.next.RoundTrip(request)
}

func newNativeChangeHub(t *testing.T) *nativeChangeHub {
	t.Helper()
	return newNativeChangeHubWithStates(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	})
}

// newNativeChangeHubWithStates builds the hub with a given workflow and the
// review lane the orchestrator is configured with.
func newNativeChangeHubWithStates(t *testing.T, review string, states []tracker.NativeState, repositoryBackend ...hubserver.ReconcileBackend) *nativeChangeHub {
	t.Helper()
	config := hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte(nativeChangeAdminToken)}
	if len(repositoryBackend) > 0 {
		config.ReconcileBackend = repositoryBackend[0]
	}
	service, err := hubserver.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	admin, err := New(Config{URL: server.URL, TokenSource: func() string { return nativeChangeAdminToken }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	if err := admin.request(t.Context(), http.MethodGet, "/api/v2/organizations", nil, &organizations); err != nil {
		t.Fatal(err)
	}
	h := &nativeChangeHub{review: review, organization: organizations.Items[0].ID, descriptor: clientTestPolicy()}
	var project tracker.NativeProject
	body := map[string]any{"name": "native-change", "idempotency_key": "project-native-change", "states": states, "require_dependencies": false}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", body, &project); err != nil {
		t.Fatal(err)
	}
	h.project = project.ID
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "native-change-worker", "scope": "worker"}, &token); err != nil {
		t.Fatal(err)
	}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
		t.Fatal(err)
	}
	if h.admin, err = admin.Native(h.organization, h.project); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	h.failChanges = &changeFailingTransport{next: server.Client().Transport}
	worker, err := New(Config{URL: server.URL, TokenSource: func() string { return token.Token }, HTTPClient: &http.Client{Transport: h.failChanges}})
	if err != nil {
		t.Fatal(err)
	}
	if h.native, err = worker.Native(h.organization, h.project); err != nil {
		t.Fatal(err)
	}
	if h.connector, err = NewNativeConnector(h.native); err != nil {
		t.Fatal(err)
	}
	h.scheduler, err = NewScheduler(worker, SchedulerConfig{
		OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project},
		Machine:           Machine{ID: "machine-native-change", Hostname: "host", Capacity: 1, Version: "test"},
		HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// createInProgress files an item and moves it to In Progress, where the
// orchestrator's dispatch leaves a native item it runs.
func (h *nativeChangeHub) createInProgress(t *testing.T, title string) connector.Issue {
	t.Helper()
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: title, Body: "Change the README."})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
		t.Fatal(err)
	}
	return issue
}

// claim fetches the only candidate and adopts it, as dispatch does.
func (h *nativeChangeHub) claim(t *testing.T, issueID string) connector.Issue {
	t.Helper()
	candidates := h.candidates(t)
	if len(candidates) != 1 || candidates[0].ID != issueID {
		t.Fatalf("candidates = %#v, want %s", candidates, issueID)
	}
	if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	return candidates[0]
}

func (h *nativeChangeHub) candidates(t *testing.T) []connector.Issue {
	t.Helper()
	candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
	if err != nil {
		t.Fatal(err)
	}
	return candidates
}

// complete moves the item the way the orchestrator's lane ledger does after a
// native run reports its change: along the hub workflow, through the adapter,
// while the claim is still held, and then releases the claim.
func (h *nativeChangeHub) complete(t *testing.T, issueID string, change *runner.NativeChange) {
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
	target, ok := connector.CompletionLane(states, current.State, h.review, change.Changed)
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

func (h *nativeChangeHub) state(t *testing.T, issueID string) string {
	t.Helper()
	issue, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	return issue.State
}

func (h *nativeChangeHub) changes(t *testing.T, issueID string) []tracker.ChangeRequest {
	t.Helper()
	changes, err := h.admin.Changes(t.Context(), tracker.NativeWorkItemID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

const nativeChangeRepository = "https://github.com/example/native-change"

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

// publish puts a version at the given head on the change as an operator
// would, so a rework run finds a current version to compare its head with.
func (h *nativeChangeHub) publish(t *testing.T, item tracker.NativeWorkItemID, changeID, head string) tracker.ChangeVersion {
	t.Helper()
	digest := policy.Digest([]byte(head))
	version, err := h.admin.PublishChangeVersion(t.Context(), item, changeID, tracker.PublishChangeVersion{
		Mutation: nativeMutationKey(),
		ChangeVersionInput: tracker.ChangeVersionInput{
			BaseSHA: strings.Repeat("a", 40), HeadSHA: head, MergeBaseSHA: strings.Repeat("a", 40), Repository: nativeChangeRepository,
			Code:      tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: digest, Availability: "unverified"},
			Artifacts: []tracker.ChangeArtifact{}, PolicyID: h.descriptor.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func nativeChangeDiff(head string, files ...string) runner.AttemptDiffSource {
	return func(context.Context) (tracker.AttemptDiffRequest, bool) {
		request := tracker.AttemptDiffRequest{BaseSHA: strings.Repeat("a", 40), HeadSHA: head, Files: []tracker.AttemptDiffFile{}}
		for _, file := range files {
			request.Files = append(request.Files, tracker.AttemptDiffFile{Path: file, Status: tracker.DiffStatusModified, Additions: 1, Patch: "@@ -1 +1 @@"})
		}
		return request, true
	}
}

// TestNativeExecutionSettlesFinishedRun drives one claimed execution through
// start, checkpoint and finish against a real hub, and checks what the finish
// decides: whether a Change Request is opened under the lease and what the run
// reports for the orchestrator to move the item on.
func TestNativeExecutionSettlesFinishedRun(t *testing.T) {
	t.Parallel()
	base := strings.Repeat("a", 40)
	head := strings.Repeat("c", 40)
	for _, test := range []struct {
		name        string
		role        string
		outcome     string
		worktree    string
		source      runner.AttemptDiffSource
		loseLease   bool
		failCreate  bool
		failDiff    bool
		existing    bool
		published   string
		repolicy    bool
		noRemote    bool
		reviewer    bool
		wantChange  *runner.NativeChange
		wantError   bool
		wantChanges int
		// wantVersions is the versions the item's change carries after the
		// finish; the last one is current and carries the run's head.
		wantVersions int
	}{
		{name: "commits open a change", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "rework reuses the item's change", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "rework publishes a new head as the next version", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: strings.Repeat("b", 40),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 2},
		{name: "rework keeps the version that already carries its head", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "rework republishes the same head under a changed policy", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, repolicy: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 2},
		{name: "a policy that asks for a reviewer leaves the version waiting", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), reviewer: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "a remote no https URL names opens the change without a version", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), noRemote: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1},
		{name: "a clean worktree with no commits opens nothing", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base),
			wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "a dirty worktree takes the ordinary path", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", source: nativeChangeDiff(base, "scratch.txt")},
		{name: "committed work left dirty takes the ordinary path", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", source: nativeChangeDiff(head, "README.md")},
		{name: "an unread worktree takes the ordinary path", role: runner.RoleCode, outcome: "succeeded", worktree: "unknown", source: nativeChangeDiff(head, "README.md")},
		{name: "an unstored final diff is not reviewable", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), failDiff: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantError: true},
		{name: "a refused create reports the commits without a change", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), failCreate: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantError: true},
		{name: "a lost lease decides nothing", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), loseLease: true},
		{name: "a failed run decides nothing", role: runner.RoleCode, outcome: "failed", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "a plan run decides nothing", role: runner.RolePlan, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "no readable worktree decides nothing", role: runner.RoleCode, outcome: "succeeded", worktree: "clean"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t)
			issue := h.createInProgress(t, "Native change")
			item := tracker.NativeWorkItemID(issue.ID)
			if test.existing {
				earlier, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Earlier change"})
				if err != nil {
					t.Fatal(err)
				}
				if test.published != "" {
					h.publish(t, item, earlier.ID, test.published)
				}
				if test.repolicy {
					h.repolicy(t)
				}
			}
			if test.reviewer {
				rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
					t.Fatal(err)
				}
			}
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
			guarded, stop, err := execution.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if test.source != nil {
				execution.(runner.DiffExecution).SetDiffSource(test.source)
			}
			if !test.noRemote {
				execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
			}
			if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: test.role, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: test.worktree, ExternalEffect: "none", EffectState: "none"}
			if err := execution.Checkpoint(guarded, checkpoint); err != nil {
				t.Fatal(err)
			}
			if test.loseLease {
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "lost"); err != nil {
					t.Fatal(err)
				}
			}
			h.failChanges.fail.Store(test.failCreate)
			h.failChanges.failDiffs.Store(test.failDiff)
			finishErr := execution.Finish(guarded, test.outcome)
			if test.loseLease != (finishErr != nil) {
				t.Fatalf("finish error = %v, lease lost = %t", finishErr, test.loseLease)
			}
			changes := execution.(runner.ChangeExecution)
			checkChange := func(wantError bool, wantChanges, wantVersions int) {
				t.Helper()
				got := changes.NativeChange()
				if test.wantChange == nil {
					if got != nil {
						t.Fatalf("native change = %#v, want none", got)
					}
				} else {
					if got == nil {
						t.Fatal("native change = nil")
					}
					if wantError != (got.Error != "") || wantError == (got.ChangeID != "") && got.Changed {
						t.Fatalf("native change = %#v, want error = %t", got, wantError)
					}
					want := *test.wantChange
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
							runPublished := test.published != got.HeadSHA || test.repolicy
							if got.VersionID != current.ID || got.VersionError != "" || detail.Change.CurrentVersion != current.ID || current.HeadSHA != got.HeadSHA || current.BaseSHA != got.BaseSHA || runPublished && (current.RunID == "" || current.AttemptID == "") || current.Repository != nativeChangeRepository || current.Code.Kind != "code" || detail.Summary.Status != map[bool]string{false: "reviewed", true: "needs_evidence"}[test.reviewer] || got.Reviewed == test.reviewer {
								t.Fatalf("version = %#v for native change %#v (summary %#v)", current, got, detail.Summary)
							}
						}
					}
				}
				if stored := h.changes(t, issue.ID); len(stored) != wantChanges {
					t.Fatalf("changes = %#v, want %d", stored, wantChanges)
				}
			}
			checkChange(test.wantError, test.wantChanges, test.wantVersions)
			if again := execution.Finish(guarded, test.outcome); test.loseLease == (again == nil) {
				t.Fatalf("repeated finish error = %v", again)
			}
			checkChange(test.wantError, test.wantChanges, test.wantVersions)
			if test.failCreate {
				h.failChanges.fail.Store(false)
				if err := execution.Finish(guarded, test.outcome); err != nil {
					t.Fatal(err)
				}
				checkChange(false, 1, 1)
			}
			if state := h.state(t, issue.ID); state != "In Progress" {
				t.Fatalf("the execution moved the item to %s; only the orchestrator moves lanes", state)
			}
		})
	}
}

// TestNativeExecutionSettlesBeforeFinishing checks the order a crash cannot
// strand: the Change Request exists before run.finished is published, so a
// finish that dies in between leaves the attempt running, where the lease's
// expiry re-offers the item, and a retried finish publishes the outcome
// without opening a second change.
func TestNativeExecutionSettlesBeforeFinishing(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t)
	issue := h.createInProgress(t, "Native change")
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	guarded, stop, err := execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	execution.(runner.DiffExecution).SetDiffSource(nativeChangeDiff(strings.Repeat("c", 40), "README.md"))
	if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", ExternalEffect: "none", EffectState: "none"}
	if err := execution.Checkpoint(guarded, checkpoint); err != nil {
		t.Fatal(err)
	}
	attemptStatus := func() string {
		t.Helper()
		recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
		if err != nil || len(recovery.Attempts) != 1 {
			t.Fatalf("recovery = %#v, error = %v", recovery, err)
		}
		return recovery.Attempts[0].Status
	}
	h.failChanges.failEvents.Store(true)
	if err := execution.Finish(guarded, "succeeded"); err == nil {
		t.Fatal("the run.finished failure was not injected")
	}
	if changes := h.changes(t, issue.ID); len(changes) != 1 {
		t.Fatalf("changes before run.finished = %#v, want the opened change", changes)
	}
	if status := attemptStatus(); status != "running" {
		t.Fatalf("attempt status after an unpublished finish = %q, want running", status)
	}
	h.failChanges.failEvents.Store(false)
	if err := execution.Finish(guarded, "succeeded"); err != nil {
		t.Fatal(err)
	}
	if status := attemptStatus(); status != "succeeded" {
		t.Fatalf("attempt status = %q, want succeeded", status)
	}
	changes := h.changes(t, issue.ID)
	change := execution.(runner.ChangeExecution).NativeChange()
	if len(changes) != 1 || change == nil || change.ChangeID != changes[0].ID || change.Error != "" {
		t.Fatalf("change = %#v, changes = %#v", change, changes)
	}
}

// TestNativeRunnerOpensChangeAndLeavesDispatch runs the production runner with
// a fake agent in a real git worktree against a real hub. A run that commits
// opens a Change Request carrying the stored attempt diff and the item moves
// to review; a run that commits nothing opens none and the item leaves the
// dispatchable set, so the claim offers it no more.
func TestNativeRunnerOpensChangeAndLeavesDispatch(t *testing.T) {
	isolateNativeChangeGit(t)
	for _, test := range []struct {
		name        string
		hosted      bool
		commit      bool
		dirty       bool
		wantNone    bool
		wantChanged bool
		wantState   string
		wantChanges int
	}{
		{name: "commits", commit: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "hosted template commits reach Human Review", hosted: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "hosted template without commits ends", hosted: true, wantState: "Done"},
		{name: "no commits", wantState: "Done"},
		{name: "uncommitted edits", dirty: true, wantNone: true, wantState: "In Progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeChangeHub(t)
			if test.hosted {
				// The default auto_promote.source_state names the review lane.
				h = newNativeChangeHubWithStates(t, "Human Review", hubserver.HostedProjectStates())
			}
			issue := h.createInProgress(t, "Update the README")
			candidate := h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
			if execution == nil {
				t.Fatal("claimed native issue has no execution lifecycle")
			}
			source := nativeChangeSourceRepo(t)
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			agent, err := runner.NewRunner(runner.Dependencies{
				Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Complete the issue"},
				Workspace:    backend,
				AgentBackend: &committingAgent{commit: test.commit, dirty: test.dirty},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := agent.Run(t.Context(), runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.FinalState != runner.FinalStateCompleted {
				t.Fatalf("final state = %q", result.FinalState)
			}
			change := result.NativeChange
			if test.wantNone {
				if change != nil {
					t.Fatalf("dirty work reported %#v; it takes the ordinary completion path", change)
				}
				if state := h.state(t, issue.ID); state != test.wantState || len(h.changes(t, issue.ID)) != 0 {
					t.Fatalf("dirty work moved to %s or opened a change", state)
				}
				return
			}
			if change == nil || change.Changed != test.wantChanged || (change.ChangeID != "") != test.wantChanged {
				t.Fatalf("native change = %#v, want changed = %t", change, test.wantChanged)
			}
			h.complete(t, issue.ID, change)
			if state := h.state(t, issue.ID); state != test.wantState {
				t.Fatalf("state = %s, want %s", state, test.wantState)
			}
			changes := h.changes(t, issue.ID)
			if len(changes) != test.wantChanges {
				t.Fatalf("changes = %#v, want %d", changes, test.wantChanges)
			}
			if test.wantChanged {
				if changes[0].ID != change.ChangeID || changes[0].Title != "Update the README" || !strings.Contains(changes[0].Body, change.HeadSHA) {
					t.Fatalf("change = %#v, reported %#v", changes[0], change)
				}
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatalf("read stored diff: %v", err)
				}
				if stored.HeadSHA != change.HeadSHA || stored.BaseSHA != change.BaseSHA || !nativeDiffHas(stored.Files, "CHANGE.md") {
					t.Fatalf("stored diff = %#v, reported %#v", stored, change)
				}
			}
			if candidates := h.candidates(t); len(candidates) != 0 {
				t.Fatalf("the completed item is offered again: %#v", candidates)
			}
		})
	}
}

func nativeDiffHas(files []tracker.AttemptDiffFile, path string) bool {
	for _, file := range files {
		if file.Path == path {
			return true
		}
	}
	return false
}

// committingAgent is a fake provider: it completes one turn, committing a
// file in the worktree first when commit is set.
type committingAgent struct {
	commit bool
	dirty  bool
}

func (a *committingAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: "thread-native", TurnID: "turn-1"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if a.dirty {
		if err := os.WriteFile(filepath.Join(request.Workspace, "SCRATCH.md"), []byte("draft\n"), 0o600); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	if a.commit {
		if err := os.WriteFile(filepath.Join(request.Workspace, "CHANGE.md"), []byte("changed\n"), 0o600); err != nil {
			return runner.AgentTurnResult{}, err
		}
		for _, args := range [][]string{{"add", "CHANGE.md"}, {"commit", "-m", "change"}} {
			if output, err := exec.CommandContext(ctx, "git", append([]string{"-C", request.Workspace}, args...)...).CombinedOutput(); err != nil {
				return runner.AgentTurnResult{}, errors.Join(err, errors.New(string(output)))
			}
		}
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: "thread-native", TurnID: "turn-1", Status: "completed"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: "thread-native", TurnID: "turn-1"}, nil
}

func nativeChangeSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"config", "core.autocrlf", "false"},
		{"config", "user.name", "Test User"}, {"config", "user.email", "test@example.com"},
	} {
		nativeChangeGit(t, dir, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("source repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nativeChangeGit(t, dir, "add", "README.md")
	nativeChangeGit(t, dir, "commit", "-m", "initial")
	return dir
}

func nativeChangeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
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

// TestNativeConnectorChangeReviewed reads the change's review state the way a
// run's completion does: reviewed under a policy that accepts the current
// version, not once the policy asks for more, and an error for a change the
// item does not have.
func TestNativeConnectorChangeReviewed(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t)
	issue := h.createInProgress(t, "Native change")
	item := tracker.NativeWorkItemID(issue.ID)
	change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Change"})
	if err != nil {
		t.Fatal(err)
	}
	first := h.publish(t, item, change.ID, strings.Repeat("b", 40))
	head := strings.Repeat("c", 40)
	second, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
		Mutation:          nativeMutationKey(),
		ExpectedVersionID: first.ID,
		ChangeVersionInput: tracker.ChangeVersionInput{
			BaseSHA: strings.Repeat("a", 40), HeadSHA: head, MergeBaseSHA: strings.Repeat("a", 40), Repository: nativeChangeRepository,
			Code:      tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: policy.Digest([]byte(head)), Availability: "unverified"},
			Artifacts: []tracker.ChangeArtifact{}, PolicyID: h.descriptor.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reviewed, err := h.connector.ChangeReviewed(t.Context(), issue.ID, change.ID, first.ID); err != nil || reviewed {
		t.Fatalf("for a version that is no longer current ChangeReviewed = %t, %v; want not reviewed", reviewed, err)
	}
	if reviewed, err := h.connector.ChangeReviewed(t.Context(), issue.ID, change.ID, ""); err != nil || reviewed {
		t.Fatalf("for no version ChangeReviewed = %t, %v; want not reviewed", reviewed, err)
	}
	if reviewed, err := h.connector.ChangeReviewed(t.Context(), issue.ID, change.ID, second.ID); err != nil || !reviewed {
		t.Fatalf("under the default policy ChangeReviewed = %t, %v; want reviewed", reviewed, err)
	}
	rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
		t.Fatal(err)
	}
	if reviewed, err := h.connector.ChangeReviewed(t.Context(), issue.ID, change.ID, second.ID); err != nil || reviewed {
		t.Fatalf("after the policy asks for review ChangeReviewed = %t, %v; want not reviewed", reviewed, err)
	}
	if _, err := h.connector.ChangeReviewed(t.Context(), issue.ID, "change_"+strings.Repeat("0", 32), second.ID); err == nil {
		t.Fatal("an unknown change read as a review state")
	}
}
