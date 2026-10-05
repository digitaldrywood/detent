package hubclient

import (
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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
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
	next         http.RoundTripper
	fail         atomic.Bool
	failDiffs    atomic.Bool
	failEvents   atomic.Bool
	failIntake   atomic.Bool
	failDetails  atomic.Bool
	failVersions atomic.Bool
	dropVersions atomic.Bool
}

func (t *changeFailingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.failDetails.Load() && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") {
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

func newNativeChangeHub(t *testing.T, inMemory ...bool) *nativeChangeHub {
	t.Helper()
	return newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}, len(inMemory) > 0 && inMemory[0])
}

// newNativeChangeHubWithStates builds the hub with a given workflow and the
// review lane the orchestrator is configured with.
func newNativeChangeHubWithStates(t *testing.T, review string, states []tracker.NativeState, repositoryBackend ...hubserver.ReconcileBackend) *nativeChangeHub {
	return newNativeChangeHubTransport(t, review, states, false, repositoryBackend...)
}

func newNativeChangeHubTransport(t *testing.T, review string, states []tracker.NativeState, inMemory bool, repositoryBackend ...hubserver.ReconcileBackend) *nativeChangeHub {
	t.Helper()
	return newNativeChangeHubDependencies(t, review, states, inMemory, false, repositoryBackend...)
}

func newNativeChangeHubDependencies(t *testing.T, review string, states []tracker.NativeState, inMemory, requireDependencies bool, repositoryBackend ...hubserver.ReconcileBackend) *nativeChangeHub {
	t.Helper()
	config := hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken), Conversation: &hubserver.ConversationConfig{Enabled: true}}
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
	var serverURL string
	var httpClient *http.Client
	if inMemory {
		serverURL = "http://native-hub.test"
		httpClient = &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			service.Handler().ServeHTTP(recorder, request)
			return recorder.Result(), nil
		})}
	} else {
		server := httptest.NewServer(service.Handler())
		t.Cleanup(server.Close)
		serverURL, httpClient = server.URL, server.Client()
	}
	admin, err := New(Config{URL: serverURL, TokenSource: func() string { return nativeChangeAdminToken }, HTTPClient: httpClient})
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
	body := map[string]any{"name": "native-change", "idempotency_key": "project-native-change", "states": states, "require_dependencies": requireDependencies}
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
	h.failChanges = &changeFailingTransport{next: httpClient.Transport}
	worker, err := New(Config{URL: serverURL, TokenSource: func() string { return token.Token }, HTTPClient: &http.Client{Transport: h.failChanges}})
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
	target, ok := connector.CompletionLane(states, current.State, h.review, change.Changed)
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
func (h *nativeChangeHub) publish(t *testing.T, item tracker.NativeWorkItemID, changeID, head string, previous ...string) tracker.ChangeVersion {
	t.Helper()
	digest := policy.Digest([]byte(head))
	var expectedVersionID string
	if len(previous) > 0 {
		expectedVersionID = previous[0]
	}
	base := strings.Repeat("a", 40)
	if len(previous) > 1 {
		base = previous[1]
	}
	version, err := h.admin.PublishChangeVersion(t.Context(), item, changeID, tracker.PublishChangeVersion{
		Mutation:          nativeMutationKey(),
		ExpectedVersionID: expectedVersionID,
		ChangeVersionInput: tracker.ChangeVersionInput{
			BaseSHA: base, HeadSHA: head, MergeBaseSHA: base, Repository: nativeChangeRepository,
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
		name           string
		role           string
		outcome        string
		worktree       string
		source         runner.AttemptDiffSource
		loseLease      bool
		failCreate     bool
		failDiff       bool
		failDetail     bool
		failVersion    bool
		dropVersion    bool
		versionCode    string
		diffCode       string
		conversation   bool
		land           bool
		finalMessage   string
		disposition    *tracker.NativeDisposition
		unreadFinal    bool
		checkpointHead string
		existing       bool
		published      string
		repolicy       bool
		noRemote       bool
		reviewer       bool
		wantChange     *runner.NativeChange
		wantDiagnostic string
		wantChanges    int
		// wantVersions is the versions the item's change carries after the
		// finish; the last one is current and carries the run's head.
		wantVersions int
	}{
		{name: "reviewed source lands with its coding lease in the four lane workflow", land: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "blocked reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "Source conflicts remain unresolved.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: merge_conflict\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "merge_conflict", FinalSummary: "Source conflicts remain unresolved."}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "human action reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "The operator must approve the migration.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: permission_wait\nblockers: []\nhuman_action: Approve the migration\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait", HumanAction: true, FinalSummary: "The operator must approve the migration."}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "instance limitation reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: instance_limitation\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation", FinalSummary: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403."}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "unfinished clean source retains normalized disposition", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "in_progress"}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "human action retains normalized disposition", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: Approve the rollout\n```", disposition: &tracker.NativeDisposition{Status: "in_progress", HumanAction: true}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "native272 instance report retains typed evidence", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused listener with EPERM\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:worker-loopback", Owner: workpad.BlockerOwnerInstance, Reason: "sandbox refused listener with EPERM", Unverifiable: true}}}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "native273 malformed predicate retains rejection", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - reason: browser unavailable\n    predicate: instance_available\nhuman_action: null\n```", wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "invalid report preserves legacy receipt", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 99\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "commits open a change", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "rework reuses the item's change", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "rework publishes a new head as the next version", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: strings.Repeat("b", 40),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 2},
		{name: "rework keeps the version that already carries its head", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "clean preserved current head retains its deliverable with an empty attempt diff", role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: func(context.Context) (tracker.AttemptDiffRequest, bool) {
			return tracker.AttemptDiffRequest{BaseSHA: head, HeadSHA: head, Files: []tracker.AttemptDiffFile{}}, true
		}, existing: true, published: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head}, wantChanges: 1, wantVersions: 1},
		{name: "empty unrelated head retains the mismatch for review", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), existing: true, published: head,
			wantDiagnostic: "the final attempt diff does not identify the current Change Request head", wantChanges: 1, wantVersions: 1},
		{name: "removed clean worktree reuses its matching final checkpoint diff", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, unreadFinal: true, checkpointHead: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "an unavailable final diff cannot reuse an older checkpoint head", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, unreadFinal: true, checkpointHead: strings.Repeat("b", 40),
			wantDiagnostic: "checkpoint head", wantChanges: 1, wantVersions: 1},
		{name: "rework refuses the same head under a changed policy", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, repolicy: true,
			wantDiagnostic: "policy_mismatch", wantChanges: 1, wantVersions: 1},
		{name: "a policy that asks for a reviewer leaves the version waiting", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), reviewer: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "a remote no https URL names opens the change without a version", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), noRemote: true,
			wantDiagnostic: "https", wantChanges: 1},
		{name: "a clean worktree with no commits opens nothing", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base),
			wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "a dirty worktree takes the ordinary path", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", source: nativeChangeDiff(base, "scratch.txt")},
		{name: "committed work left dirty takes the ordinary path", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", source: nativeChangeDiff(head, "README.md")},
		{name: "an unread final worktree cannot succeed", role: runner.RoleCode, outcome: "succeeded", worktree: "unknown", source: nativeChangeDiff(head, "README.md"), wantDiagnostic: "checkpoint is unavailable"},
		{name: "an unstored final diff is not reviewable", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), failDiff: true,
			wantDiagnostic: "diff storage unavailable"},
		{name: "a refused create reports the commits without a change", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), failCreate: true,
			wantDiagnostic: "change creation unavailable"},
		{name: "an inaccessible preserved version cannot succeed", role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head), existing: true, published: head, failDetail: true,
			wantDiagnostic: "change detail unavailable", wantChanges: 1, wantVersions: 1},
		{name: "a refused new version cannot succeed", role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), existing: true, published: strings.Repeat("b", 40), failVersion: true,
			wantDiagnostic: "version publication unavailable", wantChanges: 1, wantVersions: 1},
		{name: "a lost lease decides nothing", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), loseLease: true},
		{name: "a failed run decides nothing", role: runner.RoleCode, outcome: "failed", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "a plan run decides nothing", role: runner.RolePlan, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "no readable worktree cannot succeed", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", wantDiagnostic: "diff source is unavailable"},
		{name: "version acknowledgment loss retains the current immutable version", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: strings.Repeat("b", 40), dropVersion: true, wantDiagnostic: "version acknowledgment lost", wantChanges: 1, wantVersions: 2},
		{name: "version allowance refusal retains the previous immutable version", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: strings.Repeat("b", 40), versionCode: "allowance_exhausted", wantDiagnostic: "allowance_exhausted", wantChanges: 1, wantVersions: 1},
		{name: "diff allowance refusal is a publication diagnostic", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), diffCode: "allowance_exhausted", wantDiagnostic: "allowance_exhausted"},
		{name: "fenced diff refusal retains lost authority", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), diffCode: "stale_fencing_token", wantDiagnostic: "stale_fencing_token"},
		{name: "conversation completion remains conversation owned", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", conversation: true, finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var h *nativeChangeHub
			if test.land {
				h = newNativeChangeHubTransport(t, "Human Review", []tracker.NativeState{
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
					{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
					{Name: "Human Review", Transitions: []string{"In Progress", "Done"}},
					{Name: "Done", Terminal: true},
				}, true)
			} else {
				h = newNativeChangeHub(t, true)
			}
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
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: test.worktree, HeadSHA: test.checkpointHead, ExternalEffect: "none", EffectState: "none"}
			if err := execution.Checkpoint(guarded, checkpoint); err != nil {
				t.Fatal(err)
			}
			if test.loseLease {
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "lost"); err != nil {
					t.Fatal(err)
				}
			}
			if test.unreadFinal {
				execution.(runner.DiffExecution).SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
					return tracker.AttemptDiffRequest{}, false
				})
			}
			h.failChanges.fail.Store(test.failCreate)
			h.failChanges.failDiffs.Store(test.failDiff)
			h.failChanges.failDetails.Store(test.failDetail)
			h.failChanges.failVersions.Store(test.failVersion)
			h.failChanges.dropVersions.Store(test.dropVersion)
			if test.versionCode != "" || test.diffCode != "" {
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					code := ""
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") {
						code = test.versionCode
					}
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/diff") {
						code = test.diffCode
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
				})
			}
			if test.conversation {
				owner := execution.(*nativeExecution)
				owner.mu.Lock()
				owner.conversationContinuation = true
				owner.mu.Unlock()
			}
			if test.disposition != nil {
				transport := &executionTransport{next: h.native.client.httpClient.Transport}
				h.native.client.httpClient.Transport = transport
				transport.drop.Store(true)
				if err := execution.(runner.RuntimeExecution).ObserveRuntime(guarded, tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now()}); err != nil {
					t.Fatalf("temporary runtime publication failure escaped observation owner: %v", err)
				}
				if transport.drop.Load() || execution.(*nativeExecution).pending == nil {
					t.Fatal("runtime acknowledgment loss did not retain a pending event")
				}
			}
			finish := execution.Finish
			if test.wantDiagnostic != "" || test.finalMessage != "" {
				finish = func(ctx context.Context, outcome string) error {
					return execution.(runner.CompletionExecution).PrepareFinish(ctx, outcome, test.finalMessage)
				}
			}
			finishErr := finish(guarded, test.outcome)
			if test.finalMessage != "" && finishErr == nil {
				if err := execution.(runner.RuntimeExecution).ObserveRuntime(guarded, tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: time.Now()}); err != nil {
					t.Fatalf("post-preparation runtime observation: %v", err)
				}
			}
			lostAuthority := test.loseLease || test.diffCode == "stale_fencing_token"
			if lostAuthority != (finishErr != nil) {
				t.Fatalf("finish error = %v, lease lost = %t", finishErr, test.loseLease)
			}
			if lostAuthority {
				if !errors.Is(finishErr, runner.ErrExecutionAuthorityUnavailable) || execution.(runner.ChangeExecution).NativeChange() != nil {
					t.Fatalf("lost authority fabricated a native result: %v", finishErr)
				}
				return
			}
			if test.land {
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
			if test.wantDiagnostic != "" {
				change := execution.(runner.ChangeExecution).NativeChange()
				if change == nil || !strings.Contains(change.Error+change.VersionError, test.wantDiagnostic) || change.Reviewed || change.VersionID != "" {
					t.Fatalf("native result lost publication failure: %#v", change)
				}
				if change.VersionError != "" && change.VersionCode != test.versionCode && (!test.repolicy || change.VersionCode != "policy_mismatch") {
					t.Fatalf("native result lost publication code: %#v", change)
				}
				h.failChanges.failDetails.Store(false)
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "running" {
					t.Fatalf("unavailable publication evidence recorded success: %#v, %v", recovery.Attempts, err)
				}
				stored := h.changes(t, issue.ID)
				if len(stored) != test.wantChanges {
					t.Fatalf("changes after failed preparation = %#v", stored)
				}
				if len(stored) != 0 {
					detail, err := h.admin.Change(t.Context(), item, stored[0].ID)
					if err != nil || len(detail.Versions) != test.wantVersions {
						t.Fatalf("failed preparation changed immutable versions: %#v, %v", detail, err)
					}
				}
				if h.state(t, issue.ID) != "In Progress" {
					t.Fatal("failed preparation changed the issue lane")
				}
				if test.failCreate || test.failDiff || test.failDetail || test.failVersion || test.dropVersion {
					h.failChanges.fail.Store(false)
					h.failChanges.failDiffs.Store(false)
					h.failChanges.failDetails.Store(false)
					h.failChanges.failVersions.Store(false)
					if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.outcome, ""); err != nil {
						t.Fatal(err)
					}
					if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.outcome, ""); err != nil {
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
					if test.failVersion || test.dropVersion {
						wantVersions = 2
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(detail.Versions) != wantVersions {
						t.Fatalf("publication retry duplicated versions: %+v", detail.Versions)
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
				if test.wantChange == nil {
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
			checkChange(test.wantChanges, test.wantVersions)
			if again := execution.Finish(guarded, test.outcome); test.loseLease == (again == nil) {
				t.Fatalf("repeated finish error = %v", again)
			}
			checkChange(test.wantChanges, test.wantVersions)
			if test.finalMessage != "" {
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" {
					t.Fatalf("typed receipt lost provider outcome: %#v, %v", recovery.Attempts, err)
				}
				got := recovery.Attempts[0].Disposition
				if !reflect.DeepEqual(got, test.disposition) {
					t.Fatalf("disposition=%#v, want %#v", got, test.disposition)
				}
				page, err := h.admin.Attempts(t.Context(), item, "")
				if err != nil || len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0].Disposition, test.disposition) {
					t.Fatalf("attempts API disposition = %#v, error = %v", page.Items, err)
				}
			}
			if state := h.state(t, issue.ID); state != "In Progress" {
				t.Fatalf("the execution moved the item to %s; only the orchestrator moves lanes", state)
			}
			if test.disposition != nil && test.disposition.FinalSummary != "" {
				transitionContext := connector.WithLaneTransitionReason(guarded, "The completed turn requires review")
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
						found = event.Data.Reason == "worker_progress" && event.Data.ReasonDetail == "The completed turn requires review"
					}
				}
				if !found {
					t.Fatalf("history lost the transition reason: %#v", history.Items)
				}
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
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Native change")
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("claimed issue has no native execution")
	}
	guarded, stop, err := execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	execution.(runner.DiffExecution).SetDiffSource(nativeChangeDiff(strings.Repeat("c", 40), "README.md"))
	execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
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
		absorbed       bool
		hold           bool
		lowScore       bool
		wantVerdict    string
		ssh            bool
		validator      string
		name           string
		localIntakeOff bool
		interactive    bool
		hosted         bool
		rework         bool
		formal         bool
		failDetail     bool
		failVersion    bool
		land           bool
		commit         bool
		staged         bool
		signingFail    bool
		lateConflict   bool
		baseMoved      bool
		dirty          bool
		wantNone       bool
		wantChanged    bool
		wantState      string
		wantChanges    int
	}{
		{name: "absorbed Rework settles the original version on its actual base", absorbed: true, rework: true, land: true, wantState: "Done", wantChanges: 1},
		{name: "absorbed SSH Rework settles without a second provider session", absorbed: true, ssh: true, rework: true, land: true, wantState: "Done", wantChanges: 1},
		{name: "absorbed Rework preserves formal human rejection", absorbed: true, rework: true, formal: true, land: true, wantState: "Human Review", wantChanges: 1},
		{name: "absorbed Rework preserves a reported human hold", absorbed: true, hold: true, rework: true, land: true, wantState: "Human Review", wantChanges: 1},
		{name: "SSH native validator pass after publication", ssh: true, validator: "pass", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "native validator pass after publication", validator: "pass", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "native validator low score requests rework", lowScore: true, validator: "pass", wantVerdict: "rework", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "native validator rework retains version", validator: "rework", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "enrolled runner executes beside intake-off local runtime", localIntakeOff: true, staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "commits", commit: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "initial interactive code stays conversation owned", interactive: true, staged: true, wantNone: true, wantState: "In Progress"},
		{name: "host commits staged code", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "ordinary staged code reaches landing", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "successful base moved wait reclaims the reviewed unlanded version", staged: true, land: true, baseMoved: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "hosted template commits reach Human Review", hosted: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "hosted template without commits ends", hosted: true, wantState: "Done"},
		{name: "no commits", wantState: "Done"},
		{name: "uncommitted edits", dirty: true, wantNone: true, wantState: "In Progress"},
		{name: "Rework receives current Change discussion", rework: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "Rework receives formal requested changes", rework: true, formal: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "host commits staged Rework", rework: true, formal: true, staged: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "late host conflict continues to resolved publication and landing", rework: true, formal: true, staged: true, lateConflict: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "host signing unavailable preserves requested changes", rework: true, formal: true, staged: true, signingFail: true},
		{name: "Rework lands the clean preserved reviewed head without source changes", rework: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "Rework forwards a refused version to review", rework: true, commit: true, failVersion: true, wantState: "Human Review"},
		{name: "Rework scoped read failure releases claim before dispatch", rework: true, failDetail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			review := "In Review"
			states := []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done", "Todo"}},
				{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
			}
			if test.hosted {
				// The default auto_promote.source_state names the review lane.
				review, states = "Human Review", hubserver.HostedProjectStates()
			}
			if test.rework {
				review, states = "Human Review", []tracker.NativeState{
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
					{Name: "In Progress", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
					{Name: "Human Review", Transitions: []string{"Rework", "Done"}},
					{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
					{Name: "Done", Terminal: true},
				}
			}
			if test.land {
				state := 1
				if test.rework {
					state = 3
				}
				states[state].Transitions = append(states[state].Transitions, "Merging")
				transitions := []string{"Done", review}
				if test.rework {
					transitions = append(transitions, "Rework")
				}
				states = append(states, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: transitions})
			}
			h := newNativeChangeHubTransport(t, review, states, true)
			if test.validator != "" {
				previous := h.descriptor.ID
				h.descriptor.Gates.Validator = true
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
					t.Fatal(err)
				}
			}
			if test.localIntakeOff {
				local, err := orchestrator.New(orchestrator.Config{LocalIntakeDisabled: true, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}}, orchestrator.Dependencies{Connector: memory.New(memory.Config{Issues: []connector.Issue{{ID: "local-queued", State: "Todo"}}}), Runner: orchestrator.FakeRunner{}})
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
			var issue connector.Issue
			if test.interactive {
				var created struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
				}
				if err := h.admin.client.request(t.Context(), http.MethodPost, h.admin.base()+"/conversations", map[string]any{"key": "interactive", "title": "Interactive"}, &created); err != nil {
					t.Fatal(err)
				}
				var linked struct {
					Issue tracker.NativeIssue `json:"issue"`
				}
				if err := h.admin.client.request(t.Context(), http.MethodPost, h.admin.base()+"/conversations/"+created.Conversation.ID+"/link", map[string]any{"key": "link", "share_history": true, "issue": map[string]any{"title": "Update the README", "description": "Interactive work"}}, &linked); err != nil {
					t.Fatal(err)
				}
				issue = connector.Issue{ID: string(linked.Issue.WorkItemID), Identifier: "native#1", Title: linked.Issue.Title}
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
					t.Fatal(err)
				}
			} else {
				issue = h.createInProgress(t, "Update the README")
			}
			source := nativeChangeSourceRepo(t)
			remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
			nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
			nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
			nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
			nativeChangeGit(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			reviewedPath := source
			var expected *tracker.ChangeDetail
			var candidate connector.Issue
			if test.rework {
				if test.land && !test.lateConflict {
					info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
					if err != nil {
						t.Fatal(err)
					}
					reviewedPath = info.Path
					if err := os.WriteFile(filepath.Join(reviewedPath, "PRESERVED.md"), []byte("reviewed source\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					nativeChangeGit(t, reviewedPath, "add", "PRESERVED.md")
					nativeChangeGit(t, reviewedPath, "commit", "-m", "preserved reviewed source")
				}
				item := tracker.NativeWorkItemID(issue.ID)
				change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
				if err != nil {
					t.Fatal(err)
				}
				old := h.publish(t, item, change.ID, strings.Repeat("a", 40))
				if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: old.ID, Body: "Historical discussion"}); err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, old.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: "changes_requested", Body: "Historical requested changes"}); err != nil {
					t.Fatal(err)
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
				current := h.publish(t, item, change.ID, strings.TrimSpace(string(head)), old.ID, base)
				if test.absorbed {
					nativeChangeGit(t, source, "merge", "--squash", current.HeadSHA)
					nativeChangeGit(t, source, "commit", "-m", "absorb reviewed source")
					nativeChangeGit(t, source, "push", "origin", "main")
				}
				if test.formal {
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
				for _, state := range []string{"Human Review", "Rework"} {
					currentIssue, err = h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: state, Reason: "user_requested"})
					if err != nil {
						t.Fatal(err)
					}
				}
				detail, err := h.admin.Change(t.Context(), item, change.ID)
				if err != nil {
					t.Fatal(err)
				}
				expected = &detail
				if test.failDetail {
					next := h.native.client.httpClient.Transport
					h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/changes/"+change.ID) {
							response := httptest.NewRecorder()
							response.WriteHeader(http.StatusForbidden)
							return response.Result(), nil
						}
						return next.RoundTrip(request)
					})
				}
				candidates, err := h.scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Rework"}})
				if test.failDetail {
					var failure *APIError
					if !errors.As(err, &failure) || failure.Status != http.StatusForbidden || len(candidates) != 0 || len(h.scheduler.nativeClaims) != 0 {
						t.Fatalf("failed hydration dispatched work: candidates=%v, error=%v", candidates, err)
					}
					if state := h.state(t, issue.ID); state != "Rework" {
						t.Fatalf("hydration failure changed lane to %s", state)
					}
					h.native.client.httpClient.Transport = h.failChanges
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
			if test.lateConflict {
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
			if test.signingFail {
				nativeChangeGit(t, source, "config", "commit.gpgsign", "true")
				nativeChangeGit(t, source, "config", "gpg.program", filepath.Join(t.TempDir(), "unavailable-signer"))
			}
			provider := &committingAgent{commit: test.commit, dirty: test.dirty, staged: test.staged, validator: test.validator, lowScore: test.lowScore, complete: test.absorbed, hold: test.hold}
			var runtimeStore store.Store
			if test.validator != "" {
				runtimeStore, err = store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runtimeStore.Close() })
			}
			agent, err := runner.NewRunner(runner.Dependencies{
				Store:        runtimeStore,
				ProjectID:    "local",
				Workflow:     config.Workflow{Config: config.Config{Gate: gate.Config{Validator: gate.ValidatorConfig{Enabled: test.validator != ""}}}, Prompt: "Complete the issue"},
				Workspace:    backend,
				AgentBackend: provider,
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.failVersion {
				// A transport outage is deferred, not a definitive refusal. This
				// case exercises the existing review handoff for a refused version.
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") {
						return &http.Response{StatusCode: http.StatusUnprocessableEntity, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"invalid_request","message":"version publication refused"}`)), Request: request}, nil
					}
					return h.failChanges.RoundTrip(request)
				})
			}
			runExecution := execution
			if test.ssh {
				remote, closePeers := nativeSSHExecution(t, t.Context(), execution, t.TempDir())
				defer closePeers()
				runExecution = remote
			}
			result, err := agent.Run(t.Context(), runner.RunRequest{Execution: runExecution, DeferExecutionFinish: test.failVersion, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			if test.ssh && err == nil {
				result.NativeChange = execution.(runner.ChangeExecution).NativeChange()
			}
			if test.absorbed {
				change := result.NativeChange
				if err != nil || change == nil || provider.calls != 1 || change.ChangeID != expected.Change.ID {
					t.Fatalf("absorbed Rework did not finish once: result=%+v calls=%d error=%v", result, provider.calls, err)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if err != nil || !reflect.DeepEqual(detail.Versions, expected.Versions) || detail.Change.CurrentVersion != expected.Change.CurrentVersion {
					t.Fatalf("absorbed Rework changed immutable identity: detail=%+v error=%v", detail, err)
				}
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				if stored.BaseSHA != stored.HeadSHA || len(stored.Files) != 0 {
					t.Fatalf("absorbed fixture did not finish empty: %+v", stored)
				}
				if test.formal || test.hold {
					if change.Landing != nil || detail.Change.Landed != nil || !strings.Contains(change.VersionError, "the final attempt diff does not identify the current Change Request head") {
						t.Fatalf("human rejection was waived: %+v", change)
					}
					h.complete(t, issue.ID, change)
				} else {
					if change.Landing == nil || !change.Landing.Landed || detail.Change.Landed == nil || detail.Change.Landed.VersionID != expected.Change.CurrentVersion || detail.Change.Landed.MergeSHA != stored.BaseSHA || change.VersionError != "" {
						t.Fatalf("absorbed source lacks authentic landing: change=%+v detail=%+v", change, detail)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("a second release retained authority: %v", err)
					}
				}
				if h.state(t, issue.ID) != test.wantState || len(h.candidatesIn(t, "Rework")) != 0 {
					t.Fatal("absorbed completion remained dispatchable")
				}
				next := h.createInProgress(t, "Next native work")
				h.claim(t, next.ID)
				return
			}
			if test.lateConflict {
				owner := execution.(*nativeExecution)
				if err != nil || result.FinalState != runner.FinalStateCompleted || result.NativeChange != nil || owner.data.Outcome != "succeeded" || owner.worktreeState != "dirty" || owner.artifacts.finished {
					t.Fatalf("late host conflict lost successful dirty continuation: result=%+v outcome=%+v error=%v", result, owner.data, err)
				}
				detail, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if readErr != nil || !reflect.DeepEqual(detail, *expected) || h.state(t, issue.ID) != "Rework" {
					t.Fatalf("late conflict changed version, feedback or lane: detail=%+v error=%v", detail, readErr)
				}
				if output, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "diff", "--name-only", "--diff-filter=U").Output(); err != nil || strings.TrimSpace(string(output)) != "CHANGE.md" {
					t.Fatalf("host conflict not preserved: %s, %v", output, err)
				}
				if owner.lastDiff != nil {
					for _, file := range owner.lastDiff.Files {
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
				if attempt.Status != "succeeded" || attempt.Sequence != owner.data.Sequence || attempt.Checkpoint == nil || attempt.Checkpoint.WorktreeState != "dirty" || attempt.Checkpoint.HeadSHA != result.DiffStats.HeadSHA {
					t.Fatalf("late conflict receipt disagrees with authentic outcome: %+v", attempt)
				}
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
				result, err = agent.Run(t.Context(), runner.RunRequest{Execution: execution, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			}
			if !provider.bound && !test.ssh {
				t.Fatal("native worker did not bind its conversation")
			}
			if test.signingFail {
				if !errors.Is(err, runner.ErrWorkspacePreparation) || errors.Is(err, workspace.ErrMergeResolutionInvalid) || result.NativeChange != nil {
					t.Fatalf("host signing failure lost identity or published: result=%+v, error=%v", result.NativeChange, err)
				}
				detail, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if readErr != nil || !reflect.DeepEqual(detail, *expected) || h.state(t, issue.ID) != "Rework" {
					t.Fatalf("signing failure changed current version, feedback or lane: detail=%+v, error=%v", detail, readErr)
				}
				staged, readErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "diff", "--cached", "--name-only").Output()
				if readErr != nil || strings.TrimSpace(string(staged)) != "CHANGE.md" {
					t.Fatalf("host failure lost staged work: %s, %v", staged, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if result.FinalState != runner.FinalStateCompleted {
				t.Fatalf("final state = %q", result.FinalState)
			}
			if test.rework {
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
				if recovery.Change == nil || recovery.Change.VersionID != expected.Change.CurrentVersion || !reflect.DeepEqual(recovery.ChangeDetail, expected) {
					t.Fatalf("provider prompt lost exact Change feedback: change=%+v, detail=%+v", recovery.Change, recovery.ChangeDetail)
				}
				if !strings.Contains(provider.prompt, "Discussion is not formal approval") || !strings.Contains(provider.prompt, "historical context, not current approval or rejection") {
					t.Fatal("provider prompt omitted feedback authority boundaries")
				}
				if !test.formal {
					for _, comment := range recovery.Discussion {
						if strings.Contains(comment.Body, "Global config loses concurrent updates") {
							t.Fatal("discussion-only regression used an issue comment")
						}
					}
				}
			}
			attemptID := executionID("attempt", string(execution.Recovery().Lease.ID))
			if !test.ssh {
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
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/conversation", nil, &transcript); err != nil {
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
			if test.validator != "" {
				verdict := test.validator
				if test.wantVerdict != "" {
					verdict = test.wantVerdict
				}
				if change == nil || change.Validator == nil || change.Validator.Verdict != verdict || change.Validator.SessionID <= 0 || provider.calls != 2 {
					t.Fatalf("validator did not review the native version in a fresh session: change=%+v calls=%d", change, provider.calls)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
				if err != nil || len(detail.Reviews) != 1 || detail.Reviews[0].Validator == nil || detail.Reviews[0].VersionID != change.VersionID || change.Validator.HeadSHA != change.HeadSHA {
					t.Fatalf("validator verdict was not pinned to the immutable version: %+v, %v", detail, err)
				}
			}
			if test.failVersion {
				if change == nil || change.ChangeID != expected.Change.ID || change.VersionID != "" || change.Reviewed || change.Error != "" || change.VersionCode != "invalid_request" || !strings.Contains(change.VersionError, "version publication refused") {
					t.Fatalf("runner lost refused publication result: %+v", change)
				}
				h.complete(t, issue.ID, change)
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
				if err != nil || !reflect.DeepEqual(detail.Versions, expected.Versions) || detail.Change.CurrentVersion != expected.Change.CurrentVersion {
					t.Fatalf("refused publication replaced an immutable version: %+v, %v", detail, err)
				}
				if h.state(t, issue.ID) != test.wantState || len(h.candidatesIn(t, "Rework")) != 0 || provider.calls != 1 {
					t.Fatal("refused publication redispatched coding instead of review")
				}
				return
			}
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
			h.complete(t, issue.ID, change, test.land)
			if state := h.state(t, issue.ID); state != test.wantState {
				t.Fatalf("state = %s, want %s", state, test.wantState)
			}
			changes := h.changes(t, issue.ID)
			if len(changes) != test.wantChanges {
				t.Fatalf("changes = %#v, want %d", changes, test.wantChanges)
			}
			if test.wantChanged {
				if changes[0].ID != change.ChangeID || changes[0].Title != "Update the README" || !test.rework && !strings.Contains(changes[0].Body, change.HeadSHA) {
					t.Fatalf("change = %#v, reported %#v", changes[0], change)
				}
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatalf("read stored diff: %v", err)
				}
				if stored.HeadSHA != change.HeadSHA || !test.land && stored.BaseSHA != change.BaseSHA || !nativeDiffHas(stored.Files, map[bool]string{false: "CHANGE.md", true: "PRESERVED.md"}[test.land && test.rework && !test.lateConflict]) {
					t.Fatalf("stored diff = %#v, reported %#v", stored, change)
				}
				head, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD").Output()
				if err != nil || strings.TrimSpace(string(head)) != stored.HeadSHA {
					t.Fatalf("published head is not finalized Git HEAD: %s, %v", head, err)
				}
				if !test.rework || !test.land || test.lateConflict {
					detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
					if err != nil {
						t.Fatal(err)
					}
					version := detail.Versions[len(detail.Versions)-1]
					owner := execution.(*nativeExecution).data
					if version.ID != change.VersionID || version.ID != detail.Change.CurrentVersion || version.HeadSHA != stored.HeadSHA || version.BaseSHA != stored.BaseSHA || version.AttemptID != attemptID || version.RunID != owner.RunID || version.PolicyID != owner.PolicyID {
						t.Fatalf("published version lost finalized attempt/head/policy identity: %+v", version)
					}
				}
				if test.rework {
					detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
					if err != nil {
						t.Fatal(err)
					}
					if test.land && !test.lateConflict {
						if len(detail.Versions) != len(expected.Versions) || change.VersionID != expected.Change.CurrentVersion || change.HeadSHA != expected.Versions[len(expected.Versions)-1].HeadSHA {
							t.Fatalf("unchanged Rework replaced the immutable version: change=%+v, detail=%+v", change, detail)
						}
					} else if len(detail.Versions) != len(expected.Versions)+1 {
						t.Fatalf("rework did not publish one new version: %+v", detail)
					}
					if !reflect.DeepEqual(detail.Reviews, expected.Reviews) || !reflect.DeepEqual(detail.Discussion, expected.Discussion) {
						t.Fatal("new version changed historical feedback or fabricated review")
					}
					current := detail.Versions[len(detail.Versions)-1]
					if change.VersionID == "" || (!test.land || test.lateConflict) && change.VersionID == expected.Change.CurrentVersion || detail.Change.CurrentVersion != change.VersionID || current.ID != change.VersionID || current.HeadSHA != stored.HeadSHA || current.PolicyID != h.descriptor.ID || current.Repository != nativeChangeRepository || current.Code.URI != nativeChangeRepository+"/commit/"+stored.HeadSHA {
						t.Fatalf("rework version lost final head or artifact authority: change=%+v, detail=%+v", change, detail)
					}
				}
			}
			if test.land {
				candidates := h.candidatesIn(t, "Merging")
				if len(candidates) != 1 {
					t.Fatalf("landing candidates = %#v", candidates)
				}
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
					t.Fatal(err)
				}
				landing := h.scheduler.RunExecution(issue.ID)
				if test.baseMoved {
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
							response.WriteString("[]")
						case http.MethodPost:
							response.WriteString(fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":%q,"ref":%q},"base":{"ref":"main"}}`, target.HeadSHA, info.Branch))
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
					evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
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
					for _, test := range []struct {
						name    string
						lease   tracker.NativeLease
						version string
					}{
						{name: "released lease", lease: previousLease, version: target.VersionID},
						{name: "stale fencing", lease: tracker.NativeLease{ID: currentLease.ID, FencingToken: previousLease.FencingToken}, version: target.VersionID},
						{name: "foreign lease", lease: tracker.NativeLease{ID: "foreign-lease", FencingToken: currentLease.FencingToken}, version: target.VersionID},
						{name: "stale version", lease: currentLease, version: "version_stale"},
					} {
						if _, err := h.native.LandChangeVersion(t.Context(), item, target.ChangeID, test.version, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name, LeaseID: test.lease.ID, FencingToken: test.lease.FencingToken}, MergeSHA: target.HeadSHA, BaseRef: "main", Method: target.Method}); err == nil {
							t.Fatalf("landing accepted %s", test.name)
						}
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
				if test.rework && !test.lateConflict {
					path, expectedContent = "PRESERVED.md", "reviewed source\n"
				}
				calls := 1
				if test.validator != "" {
					calls++
				}
				if test.lateConflict {
					calls = 2
				}
				content, err := exec.CommandContext(t.Context(), "git", "--git-dir", remote, "show", "main:"+path).Output()
				if err != nil || string(content) != expectedContent || provider.calls != calls {
					t.Fatalf("landing did not publish preserved source without another coding turn: %q, %v, calls=%d", content, err, provider.calls)
				}
				if test.lateConflict {
					head, err := exec.CommandContext(t.Context(), "git", "--git-dir", remote, "rev-parse", "main").Output()
					if err != nil || strings.TrimSpace(string(head)) != result.NativeLanding.MergeSHA {
						t.Fatalf("landing receipt did not identify the actual merge: %s, %v, receipt=%+v", head, err, result.NativeLanding)
					}
					recovery, err := h.native.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
					if err != nil || len(recovery.Attempts) != 3 {
						t.Fatalf("completed conflict journey lost authentic attempts: %+v, %v", recovery.Attempts, err)
					}
					for _, attempt := range recovery.Attempts {
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
	complete  bool
	hold      bool
	lowScore  bool
	validator string
	commit    bool
	dirty     bool
	staged    bool
	prompt    string
	workspace string
	calls     int
	bound     bool
}

func (*committingAgent) SupportsLiveControl() bool { return true }

func (a *committingAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	a.calls++
	if strings.Contains(request.Prompt, "Detent validator-agent") {
		if request.Resume.ThreadID != "" || request.Resume.SessionID != "" || !request.ReadOnly || !strings.Contains(request.Prompt, "Reviewed native version:") {
			return runner.AgentTurnResult{}, errors.New("native validator did not start a fresh read-only version review")
		}
		score := .95
		if a.lowScore {
			score = .5
		}
		if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: fmt.Sprintf(`{"verdict":%q,"score":%f,"summary":"Checked acceptance","findings":[]}`, a.validator, score)}); err != nil {
			return runner.AgentTurnResult{}, err
		}
		return runner.AgentTurnResult{ThreadID: "thread-validator", TurnID: "validator-turn"}, nil
	}
	a.prompt = request.Prompt
	a.workspace = request.Workspace
	a.bound = request.ConversationControl != nil
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: "thread-native", TurnID: "turn-1"}); err != nil {
		return runner.AgentTurnResult{}, err
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
	if a.complete {
		action := "null"
		if a.hold {
			action = "Approve the migration"
		}
		message += "\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: " + action + "\n```"
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ThreadID: "thread-native", TurnID: "turn-1", Delta: message, ItemID: "result"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: "thread-native", TurnID: "turn-1", Status: "completed"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: "thread-native", TurnID: "turn-1"}, nil
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

func nativeChangeSourceRepo(t *testing.T) string {
	t.Helper()
	dir := nativeChangeTempDir(t)
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
