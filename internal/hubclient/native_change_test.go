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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	reopen       func(func(string) error)
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

const issueContractTestSections = "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."

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
		config.ImportBackend, _ = repositoryBackend[0].(hubserver.ImportBackend)
	}
	service, err := hubserver.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	var currentService atomic.Pointer[hubserver.Service]
	currentService.Store(service)
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
			currentService.Load().Handler().ServeHTTP(recorder, request)
			return recorder.Result(), nil
		})}
	} else {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			currentService.Load().Handler().ServeHTTP(w, r)
		}))
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
	h.reopen = func(seed func(string) error) {
		t.Helper()
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		if err := seed(config.DatabasePath); err != nil {
			t.Fatal(err)
		}
		service, err = hubserver.Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		currentService.Store(service)
	}
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
	claim, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return claim.Issue
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
	return h.publishCaptured(t, item, changeID, head, nil, previous...)
}

func (h *nativeChangeHub) publishCaptured(t *testing.T, item tracker.NativeWorkItemID, changeID, head string, captured *tracker.ChangeSourceCapture, previous ...string) tracker.ChangeVersion {
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
	var source *tracker.ChangeSource
	var bundle []byte
	if captured == nil && base != head {
		capture := nativeChangeSourceFixture(base, head)
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
			BaseSHA: base, HeadSHA: head, MergeBaseSHA: base, Repository: nativeChangeRepository,
			Code:      tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: digest, Availability: "unverified"},
			Artifacts: []tracker.ChangeArtifact{}, PolicyID: h.descriptor.ID, Source: source,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func nativeChangeSourceFixture(base, head string) tracker.ChangeSourceCapture {
	bundle := []byte("bounded retained source fixture")
	return tracker.ChangeSourceCapture{Source: tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}, Bundle: bundle}
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	base := strings.Repeat("a", 40)
	head := strings.Repeat("c", 40)
	for _, test := range []struct {
		name                    string
		role                    string
		outcome                 string
		worktree                string
		source                  runner.AttemptDiffSource
		loseLease               bool
		validationErr           error
		restart                 bool
		legacyRestart           bool
		restoreRefusal          string
		failCreate              bool
		retainSource            bool
		sourceRefusal           bool
		failDiff                bool
		failDetail              bool
		failVersion             bool
		dropVersion             bool
		versionCode             string
		diffCode                string
		conversation            bool
		land                    bool
		finalMessage            string
		disposition             *tracker.NativeDisposition
		unreadFinal             bool
		checkpointHead          string
		existing                bool
		published               string
		repolicy                bool
		noRemote                bool
		reviewer                bool
		requiredPR              bool
		lostPRResponse          bool
		publicationWriteRefusal string
		publicationFenceLoss    int
		publicationFailure      string
		publicationStaged       bool
		publicationRestore      bool
		publicationUncertain    bool
		wantChange              *runner.NativeChange
		wantDiagnostic          string
		wantChanges             int
		// wantVersions is the versions the item's change carries after the
		// finish; the last one is current and carries the run's head.
		wantVersions int
	}{
		{name: "required PR refuses unrelated ambiguous external effect", requiredPR: true, publicationUncertain: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "required PR no edit Rework preserves human hold", requiredPR: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "required PR lost response adopts one PR under human hold", requiredPR: true, lostPRResponse: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "required PR refuses replaced version at persistence", requiredPR: true, publicationWriteRefusal: "version", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR refuses lost fence at persistence", requiredPR: true, publicationWriteRefusal: "fence", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR refuses ambiguous effect at persistence", requiredPR: true, publicationWriteRefusal: "ambiguous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR refuses moved final head at persistence", requiredPR: true, publicationWriteRefusal: "head", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR refuses substituted PR target at persistence", requiredPR: true, publicationWriteRefusal: "base", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR refuses substituted PR branch at persistence", requiredPR: true, publicationWriteRefusal: "branch", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR refuses substituted PR identity at persistence", requiredPR: true, publicationWriteRefusal: "identity", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR survives completion restoration without duplicate effects", requiredPR: true, publicationRestore: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true}, wantChanges: 1, wantVersions: 1},
		{name: "required PR fences push", requiredPR: true, publicationFenceLoss: 2, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR fences create", requiredPR: true, publicationFenceLoss: 3, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR fences returning the verified receipt", requiredPR: true, publicationFenceLoss: 4, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR records actionable authentication blocker", requiredPR: true, publicationFailure: "auth", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR records actionable push blocker", requiredPR: true, publicationFailure: "push", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "required PR finalizes existing staged repair under human hold", requiredPR: true, publicationStaged: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true}, wantChanges: 1, wantVersions: 1},
		{name: "canceled deferred validation preserves authority", validationErr: context.Canceled, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "timed out deferred validation preserves authority", validationErr: context.DeadlineExceeded, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "excluded checkpoint path is a permanent publication refusal", sourceRefusal: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantDiagnostic: `excluded source path ".detent/skills/split-issue.md"`},
		{name: "deferred publication survives restart", retainSource: true, restart: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "installed deferred publication restores tracker authority", restart: true, legacyRestart: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart refuses released authority", restart: true, restoreRefusal: "released", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart refuses another machine", restart: true, restoreRefusal: "machine", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart refuses a lease pinned to a superseded policy", restart: true, restoreRefusal: "policy", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart refuses stale fencing", restart: true, restoreRefusal: "fence", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "existing valid different fence cannot inherit deferred completion", restart: true, restoreRefusal: "existing", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "existing same lease renewal preserves deferred completion", restart: true, restoreRefusal: "renewed", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "reviewed source lands with its coding lease in the four lane workflow", land: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "blocked reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "Source conflicts remain unresolved.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: merge_conflict\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "merge_conflict", FinalSummary: "Source conflicts remain unresolved."}},
		{name: "human action reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "The operator must approve the migration.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: permission_wait\nblockers: []\nhuman_action: Approve the migration\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait", HumanAction: true, FinalSummary: "The operator must approve the migration."}},
		{name: "instance limitation reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: instance_limitation\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation", FinalSummary: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403."}},
		{name: "unfinished clean source retains normalized disposition", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "in_progress"}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "unfinished dirty source retains normalized disposition without publication", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "in_progress"}},
		{name: "human action retains normalized disposition", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: Approve the rollout\n```", disposition: &tracker.NativeDisposition{Status: "in_progress", HumanAction: true}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "native272 instance report retains typed evidence", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused listener with EPERM\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:worker-loopback", Owner: workpad.BlockerOwnerInstance, Reason: "sandbox refused listener with EPERM", Unverifiable: true}}}},
		{name: "dirty instance report retains typed evidence without publication", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused listener with EPERM\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:worker-loopback", Owner: workpad.BlockerOwnerInstance, Reason: "sandbox refused listener with EPERM", Unverifiable: true}}}},
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
			base, head := base, head
			var publicationSource func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)
			var publicationCalls int
			var original tracker.ChangeDetail
			var ci *NativeClient
			var publicationOwner *nativeExecution
			if test.requiredPR {
				source := nativeChangeSourceRepo(t)
				remote := filepath.Join(nativeChangeTempDir(t), "origin.git")
				nativeChangeGit(t, source, "init", "--bare", "-b", "main", remote)
				nativeChangeGit(t, source, "remote", "add", "origin", nativeChangeRepository)
				nativeChangeGit(t, source, "config", "url."+remote+".insteadOf", nativeChangeRepository)
				nativeChangeGit(t, source, "push", "origin", "main")
				backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
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
				if err := os.WriteFile(filepath.Join(info.Path, "README.md"), []byte("existing repair\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, info.Path, "add", "README.md")
				if test.publicationStaged {
					if _, err := backend.(workspace.NativeWorkFinalizer).FinalizeNativeWork(t.Context(), info, work, func(context.Context) error { return nil }); err != nil {
						t.Fatal(err)
					}
				} else {
					nativeChangeGit(t, info.Path, "commit", "-m", "existing repair")
				}
				if test.publicationFailure == "push" {
					if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\nprintf publication-policy-refusal >&2\nexit 1\n"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				head = readHead(info.Path)
				test.published, test.checkpointHead = head, head
				test.wantChange = &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}
				test.source = func(context.Context) (tracker.AttemptDiffRequest, bool) {
					return tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: head, Files: []tracker.AttemptDiffFile{{Path: "README.md", Status: "modified"}}}, true
				}
				client, err := github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					pull := fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/native-change"}},"base":{"ref":"main","repo":{"full_name":"example/native-change"}}}`, head, info.Branch)
					response := "[]"
					switch request.Method {
					case http.MethodGet:
						if publicationCalls > 0 {
							response = "[" + pull + "]"
							if strings.HasSuffix(request.URL.Path, "/pulls/7") {
								response = pull
							}
						}
					case http.MethodPost:
						publicationCalls++
						if test.publicationFailure == "auth" {
							return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"Resource not accessible by integration"}`))}, nil
						}
						if test.lostPRResponse && publicationCalls == 1 {
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
				publicationSource = func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
					if test.publicationFenceLoss > 0 {
						authorize := opts.Authorize
						calls := 0
						opts.Authorize = func(ctx context.Context) error {
							calls++
							if calls == test.publicationFenceLoss {
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
					if test.publicationUncertain || test.publicationFailure == "push" || test.publicationFenceLoss > 0 && test.publicationFenceLoss < 4 {
						wantCalls = 0
					}
					if readHead(info.Path) != head || readHead(source) != base || publicationCalls != wantCalls {
						t.Errorf("publication changed source, merged or duplicated a PR: calls=%d", publicationCalls)
					}
				})
			}
			if test.requiredPR {
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
			if test.existing {
				earlier, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: "Earlier change"})
				if err != nil {
					t.Fatal(err)
				}
				if test.requiredPR {
					ci = h.publicationCI(t)
				}
				if test.published != "" {
					version := h.publish(t, item, earlier.ID, test.published, "", base)
					if test.requiredPR {
						check := version.Checks[0]
						if _, err := ci.SubmitChangeCheck(t.Context(), item, earlier.ID, version.ID, tracker.SubmitChangeCheck{Mutation: nativeMutationKey(), ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: version.HeadSHA, RunID: version.RunID, PolicyID: version.PolicyID, ConfigDigest: version.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: version.CreatedAt, Evidence: []tracker.ChangeArtifact{{Kind: "test", URI: "s3://fixture/test", SHA256: policy.Digest([]byte("test receipt")), Availability: "available"}}}}); err != nil {
							t.Fatal(err)
						}
						if _, err := h.admin.ReviewChange(t.Context(), item, earlier.ID, version.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), ExpectedVersionID: version.ID, Decision: "changes_requested", Body: "Human hold: prepare the missing PR, do not land this version"}); err != nil {
							t.Fatal(err)
						}
						original, err = h.admin.Change(t.Context(), item, earlier.ID)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if test.repolicy {
					h.repolicy(t)
				}
			}
			if test.reviewer && !test.requiredPR {
				rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
					t.Fatal(err)
				}
			}
			candidate := h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID)
			publicationOwner = execution.(*nativeExecution)
			if publicationSource != nil {
				execution.(runner.PublicationSourceExecution).SetPublicationSource(publicationSource)
			}
			guarded, stop, err := execution.Guard(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if test.source != nil {
				execution.(runner.DiffExecution).SetDiffSource(test.source)
			}
			if test.retainSource {
				execution.(runner.ChangeSourceExecution).SetChangeSource(func(_ context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					bundle := []byte("immutable source before publication outage")
					return tracker.ChangeSourceCapture{Source: tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}, Bundle: bundle}, nil
				})
			}
			if !test.noRemote {
				execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
			}
			if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: test.role, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: test.worktree, HeadSHA: test.checkpointHead, ExternalEffect: "none", EffectState: "none"}
			if test.publicationUncertain {
				checkpoint.ExternalEffect, checkpoint.EffectState, checkpoint.EffectID = "provider_turn", "ambiguous", "effect_"+strings.Repeat("a", 32)
			}
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
			if test.sourceRefusal {
				execution.(runner.ChangeSourceExecution).SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error) {
					return tracker.ChangeSourceCapture{}, fmt.Errorf(`%w: excluded source path ".detent/skills/split-issue.md"`, workspace.ErrCheckpointUnsafe)
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
			if test.publicationRestore {
				before := execution.(*nativeExecution)
				if err := before.PrepareFinish(guarded, test.outcome, "", nil); err != nil || before.publication == nil {
					t.Fatalf("prepare publication for restored completion: %v", err)
				}
				saved := before.CompletionState()
				stop()
				h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: h.scheduler.machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				publicationOwner = execution.(*nativeExecution)
				guarded = t.Context()
			}
			if test.publicationUncertain {
				owner := execution.(*nativeExecution)
				if err := owner.PrepareFinish(guarded, test.outcome, "", nil); err != nil || owner.publication != nil || owner.data.Outcome != "" || owner.data.Handoff.ExternalEffect != "provider_turn" || owner.data.Handoff.EffectState != "ambiguous" || publicationCalls != 0 || !strings.Contains(owner.change.Error, "unresolved external effect") {
					t.Fatalf("publication erased unrelated effect ambiguity: %+v, %v", owner.data, err)
				}
				return
			}
			finish := execution.Finish
			if test.lostPRResponse {
				if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.outcome, "", nil); err != nil {
					t.Fatal(err)
				}
				owner := execution.(*nativeExecution)
				if owner.publication != nil || owner.change.Error == "" || owner.data.Handoff.EffectState != "ambiguous" || publicationCalls != 1 || owner.data.Outcome != "" {
					t.Fatalf("lost response was represented as success: %+v", owner.data)
				}
			}

			if test.publicationWriteRefusal != "" {
				owner := execution.(*nativeExecution)
				if err := owner.PrepareFinish(guarded, test.outcome, "", nil); err != nil || owner.publication == nil {
					t.Fatalf("prepare held publication: %v", err)
				}
				switch test.publicationWriteRefusal {
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
					checkpoint := *owner.data.Handoff
					checkpoint.EffectState = "ambiguous"
					if err := owner.Checkpoint(guarded, checkpoint); err != nil {
						t.Fatal(err)
					}
				case "head":
					owner.SetDiffSource(nativeChangeDiff(strings.Repeat("d", 40), "README.md"))
				case "base":
					owner.publication.BaseRef = "another-target"
				case "branch":
					owner.publication.Branch = "another-branch"
				case "identity":
					owner.publication.External = tracker.ChangeExternalReference{Provider: "github", ID: "8", URL: nativeChangeRepository + "/pull/8"}
				}
				if err := owner.Finish(guarded, test.outcome); err == nil {
					t.Fatal("stale or ambiguous publication was persisted as success")
				}
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
				if err != nil || evidence.Attempt == nil || evidence.Attempt.Finalization != nil {
					t.Fatalf("refused publication leaked into finalization reads: %+v, %v", evidence, err)
				}
				return
			}
			if test.wantDiagnostic != "" || test.finalMessage != "" {
				finish = func(ctx context.Context, outcome string) error {
					return execution.(runner.CompletionExecution).PrepareFinish(ctx, outcome, test.finalMessage, nil)
				}
			}
			finishErr := finish(guarded, test.outcome)
			if test.publicationFailure != "" {
				change := publicationOwner.NativeChange()
				if finishErr != nil || publicationOwner.publication != nil || change == nil || change.VersionError == "" || change.Reviewed || change.VersionID != original.Change.CurrentVersion || !strings.Contains(change.VersionError, "preserve source") {
					t.Fatalf("publication failure lost its actionable blocker or current source: %+v, %v", change, finishErr)
				}
				h.complete(t, issue.ID, change)
				if h.state(t, issue.ID) != h.review {
					t.Fatal("publication blocker bypassed the existing review hold")
				}
				return
			}
			if test.publicationFenceLoss > 0 {
				if !errors.Is(finishErr, runner.ErrExecutionAuthorityUnavailable) || publicationOwner.publication != nil || publicationOwner.data.Outcome != "" {
					t.Fatalf("lost fence fabricated successful publication: %+v, %v", publicationOwner.data, finishErr)
				}
				return
			}
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
				if change.VersionError != "" && change.VersionCode != test.versionCode && change.VersionCode != test.diffCode && (!test.repolicy || change.VersionCode != "policy_mismatch") {
					t.Fatalf("native result lost publication code: %#v", change)
				}
				if execution.(*nativeExecution).settled {
					before := *change
					for range 3 {
						if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.outcome, test.finalMessage, nil); err != nil {
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
					if test.validationErr != nil {
						native := execution.(*nativeExecution)
						saved := native.CompletionState()
						interrupted, cancel := context.WithCancelCause(guarded)
						cancel(test.validationErr)
						_, err := h.scheduler.RestoreCompletion(interrupted, orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved)
						if !errors.Is(err, test.validationErr) || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
							t.Fatalf("operation interruption revoked deferred authority: %v", err)
						}
						if guarded.Err() != nil || string(native.CompletionState()) != string(saved) || h.scheduler.RunExecution(issue.ID) != execution {
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
					if test.restart {
						stop()
						before := execution.(*nativeExecution)
						saved := before.CompletionState()
						if test.legacyRestart {
							saved = nil
						}
						machine := h.scheduler.machine
						if test.restoreRefusal == "machine" {
							machine.ID = "another-machine"
						}
						if test.restoreRefusal == "released" {
							if err := h.native.Release(t.Context(), before.claim.lease, "released"); err != nil {
								t.Fatal(err)
							}
						}
						if test.restoreRefusal == "fence" {
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
						if test.restoreRefusal == "policy" {
							h.repolicy(t)
						}
						h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
						if err != nil {
							t.Fatal(err)
						}
						if test.restoreRefusal == "existing" {
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
							if h.scheduler.RunExecution(issue.ID) != currentExecution || currentExecution.(*nativeExecution).lastDiff != nil || currentExecution.(*nativeExecution).preparedOutcome != "" {
								t.Fatal("refused completion mutated the replacement execution")
							}
							return
						}
						_, err = h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: leasePolicy, Repository: nativeChangeRepository}, candidate, saved)
						if test.restoreRefusal != "" && test.restoreRefusal != "renewed" {
							if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || len(h.scheduler.nativeClaims) != 0 {
								t.Fatalf("invalid authority restored: claims=%v err=%v", h.scheduler.nativeClaims, err)
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						if test.restoreRefusal == "renewed" {
							if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, time.Now()); err != nil {
								t.Fatal(err)
							}
							if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
								t.Fatal(err)
							}
						}
						execution = h.scheduler.RunExecution(issue.ID)
						if got := execution.(*nativeExecution); got.data.RunID != before.data.RunID || got.data.AttemptID != before.data.AttemptID || got.claim.lease.ID != before.claim.lease.ID || got.claim.lease.FencingToken != before.claim.lease.FencingToken || got.diffSource != nil || got.lastDiff == nil || got.storedSeq != before.storedSeq {
							t.Fatalf("restoration changed execution identity or lost stored diff: %+v", got.data)
						}
						guarded = t.Context()
						if err := execution.Validate(guarded); err != nil {
							t.Fatal(err)
						}
					}
					if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.outcome, "", nil); err != nil {
						t.Fatal(err)
					}
					if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.outcome, "", nil); err != nil {
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
					if test.retainSource {
						bundle, err := h.admin.ChangeSource(t.Context(), item, stored[0].ID, republished.VersionID)
						if err != nil || string(bundle) != "immutable source before publication outage" {
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
							if test.requiredPR && (detail.Summary.NativeReview != "changes_requested" || current.ID != got.VersionID || len(detail.Versions) != 1) {
								t.Fatal("publication-only recovery replaced the current version or discarded its human hold")
							}
							if test.requiredPR && current.External == nil {
								t.Fatal("required PR no edit Rework retained native work but published no operator-visible GitHub PR")
							}
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
			if test.requiredPR {
				current, err := h.admin.Change(t.Context(), item, original.Change.ID)
				if err != nil {
					t.Fatal(err)
				}
				version := current.Versions[0]
				version.External = nil
				if !reflect.DeepEqual(version, original.Versions[0]) || !reflect.DeepEqual(current.Reviews, original.Reviews) || !reflect.DeepEqual(current.Checks, original.Checks) {
					t.Fatal("same-head publication changed immutable version, reviews or CI evidence")
				}
				var visible tracker.NativeIssue
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"?include=change", nil, &visible); err != nil {
					t.Fatal(err)
				}
				if visible.Change == nil || visible.Change.URL != nativeChangeRepository+"/pull/7" || visible.PullRequest == nil || visible.PullRequest.Number != 7 || visible.PullRequest.URL != visible.Change.URL {
					t.Fatalf("operator reads omitted the verified PR: %+v", visible)
				}
				target, err := execution.(runner.LandingExecution).LandingTarget(t.Context())
				if !errors.Is(err, runner.ErrLandingNotReviewed) || target.External == nil || target.External.URL != visible.Change.URL {
					t.Fatalf("publication lost the human hold or landing identity: %+v, %v", target, err)
				}
			}
			if again := execution.Finish(guarded, test.outcome); test.loseLease == (again == nil) {
				t.Fatalf("repeated finish error = %v", again)
			}
			checkChange(test.wantChanges, test.wantVersions)
			evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
			if err != nil || evidence.Attempt == nil {
				t.Fatalf("finalizer projection unavailable: %+v, %v", evidence, err)
			}
			if change := changes.NativeChange(); change != nil {
				f := evidence.Attempt.Finalization
				if f == nil || f.ChangeID != change.ChangeID || f.VersionID != change.VersionID || f.HeadSHA != change.HeadSHA || f.BaseSHA != change.BaseSHA || f.Changed != change.Changed || f.Files != change.Files || f.Reviewed != change.Reviewed || f.ObservedAt.IsZero() || !f.Settled {
					t.Fatalf("finalizer omitted recorded source outcome: %+v, change=%+v", f, change)
				}
			} else if evidence.Attempt.Finalization != nil || evidence.Attempt.FinalizationAvailability != "unavailable" {
				t.Fatalf("finalizer fabricated a missing source result: %+v", evidence.Attempt.Finalization)
			}
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
	for _, test := range []struct {
		name           string
		outage         bool
		completeDuring bool
		version        bool
	}{
		{name: "finish event"},
		{name: "version publication", version: true},
		{name: "completion during four minute outage", outage: true, completeDuring: true},
		{name: "completion after four minute outage", outage: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newNativeChangeHub(t, true)
				issue := h.createInProgress(t, "Native change")
				h.claim(t, issue.ID)
				execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				originalTransport := h.native.client.httpClient.Transport
				var offline atomic.Bool
				if test.outage {
					h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if offline.Load() {
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"tenant_unavailable"}`)), Request: request}, nil
						}
						return originalTransport.RoundTrip(request)
					})
				}
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				execution.SetDiffSource(nativeChangeDiff(strings.Repeat("c", 40), "README.md"))
				execution.SetRepository(nativeChangeRepository)
				if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", ExternalEffect: "none", EffectState: "none"}
				if err := execution.Checkpoint(guarded, checkpoint); err != nil {
					t.Fatal(err)
				}
				originalLease := execution.claim.lease
				if test.outage {
					offline.Store(true)
					time.Sleep(2 * time.Minute)
					synctest.Wait()
				} else {
					h.failChanges.failEvents.Store(!test.version)
					h.failChanges.failVersions.Store(test.version)
				}
				if !test.outage || test.completeDuring {
					if err := execution.Finish(guarded, "succeeded"); err == nil || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("unpublished finish = %v", err)
					}
					recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
					if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Outcome != "" {
						t.Fatalf("unpublished finish retired attempt: %+v, %v", recovery.Attempts, err)
					}
				}
				if test.outage {
					time.Sleep(2 * time.Minute)
					synctest.Wait()
					if guarded.Err() != nil {
						t.Fatalf("outage canceled worker: %v", context.Cause(guarded))
					}
					offline.Store(false)
					time.Sleep(30 * time.Second)
					synctest.Wait()
				} else {
					h.failChanges.failEvents.Store(false)
					h.failChanges.failVersions.Store(false)
				}
				if err := execution.Validate(guarded); err != nil {
					t.Fatal(err)
				}
				if err := execution.PrepareFinish(guarded, "succeeded", "", nil); err != nil {
					t.Fatal(err)
				}
				change := execution.NativeChange()
				if change == nil || change.Error != "" || change.VersionError != "" || change.VersionID == "" {
					t.Fatalf("reconnected publication = %+v", change)
				}
				h.complete(t, issue.ID, change)
				recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" || recovery.Attempts[0].LeaseID != originalLease.ID || recovery.Attempts[0].FencingToken != originalLease.FencingToken || h.state(t, issue.ID) != "In Review" || len(h.candidates(t)) != 0 {
					t.Fatalf("reconnect lost outcome or restarted attempt: %+v, %v", recovery.Attempts, err)
				}
				changes := h.changes(t, issue.ID)
				if len(changes) != 1 {
					t.Fatalf("changes = %+v", changes)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), changes[0].ID)
				if err != nil || len(detail.Versions) != 1 {
					t.Fatalf("replay duplicated version: %+v, %v", detail.Versions, err)
				}
			})
		})
	}
}

// TestNativeRunnerOpensChangeAndLeavesDispatch runs the production runner with
// a fake agent in a real git worktree against a real hub. A run that commits
// opens a Change Request carrying the stored attempt diff and the item moves
// to review; a run that commits nothing opens none and the item leaves the
// dispatchable set, so the claim offers it no more.
func TestNativeRunnerOpensChangeAndLeavesDispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	isolateNativeChangeGit(t)
	dependencyMessage := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: prj_6d4919bebd73446798e6cd807feda10e#750\n    owner: orchestrator\n    reason: prerequisite remains Backlog\n    predicate:\n      type: issue_state\n      ref: prj_6d4919bebd73446798e6cd807feda10e#750\n      states: [Done]\nhuman_action: null\n```"
	instanceMessage := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: effective host gate lacks a passing current-head receipt\nhuman_action: null\n```"
	dependencyDisposition := &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "prj_6d4919bebd73446798e6cd807feda10e#750", Identifier: "prj_6d4919bebd73446798e6cd807feda10e#750", Owner: workpad.BlockerOwnerOrchestrator, Reason: "prerequisite remains Backlog", RecheckInterval: "tick", Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Ref: "prj_6d4919bebd73446798e6cd807feda10e#750", Identifier: "prj_6d4919bebd73446798e6cd807feda10e#750", States: []string{"done"}}}}}
	instanceDisposition := &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:tool", Owner: workpad.BlockerOwnerInstance, Reason: "effective host gate lacks a passing current-head receipt", Unverifiable: true}}}
	for _, test := range []struct {
		finalMessage     string
		wantDisposition  *tracker.NativeDisposition
		publicationFault string
		publication      string
		largeOutput      bool
		dependencyOnly   bool
		failedCheck      bool
		absorbed         bool
		hold             bool
		lowScore         bool
		wantVerdict      string
		ssh              bool
		validator        string
		name             string
		localIntakeOff   bool
		interactive      bool
		hosted           bool
		rework           bool
		reopened         bool
		formal           bool
		failDetail       bool
		failVersion      bool
		land             bool
		commit           bool
		staged           bool
		expired          bool
		signingFail      bool
		lateConflict     bool
		advanceTarget    bool
		baseMoved        bool
		staleBase        bool
		dirty            bool
		wantNone         bool
		wantChanged      bool
		wantState        string
		wantChanges      int
	}{
		{name: "blocked native729 first unchanged Rework preserves dependency", finalMessage: dependencyMessage, wantDisposition: dependencyDisposition, rework: true},
		{name: "blocked native825 unchanged Rework preserves instance gate blocker", finalMessage: instanceMessage, wantDisposition: instanceDisposition, rework: true},
		{name: "blocked SSH Rework retains staged source", finalMessage: instanceMessage, wantDisposition: instanceDisposition, rework: true, ssh: true, staged: true},
		{name: "held publication reconciles lost create through new Runner lease", publication: "lost_create", rework: true, formal: true},
		{name: "held publication reconciles lost push through new Runner lease", publication: "lost_push", rework: true, formal: true},
		{name: "held publication requires validator session authority on new Runner lease", publication: "lost_create", rework: true, formal: true, validator: "pass"},
		{name: "confirmed publication retries pending validator on unchanged source", publication: "confirmed", rework: true, validator: "wait"},
		{name: "confirmed publication retries validator rework after prerequisite repair", publication: "confirmed", rework: true, validator: "rework"},
		{name: "confirmed publication reuses applicable passing native verdict", publication: "confirmed", rework: true, validator: "pass"},
		{name: "confirmed publication preserves human hold during validator recovery", publication: "confirmed", rework: true, formal: true, validator: "rework"},
		{name: "held publication refuses stale validator policy", publication: "lost_create", publicationFault: "policy", rework: true, formal: true, validator: "pass"},
		{name: "held publication refuses stale validator head", publication: "lost_create", publicationFault: "changed_head", rework: true, formal: true, validator: "pass"},
		{name: "held publication reconciles staged work through new Runner lease", publication: "lost_create", rework: true, formal: true, staged: true},
		{name: "held publication refuses replaced version on new Runner lease", publication: "lost_create", publicationFault: "version", rework: true, formal: true},
		{name: "held publication refuses foreign source admission", publication: "lost_create", publicationFault: "foreign_source", rework: true, formal: true},
		{name: "held publication refuses unavailable source on new Runner lease", publication: "lost_create", publicationFault: "unavailable_source", rework: true, formal: true},
		{name: "held publication refuses missing_pr on new Runner lease", publication: "lost_create", publicationFault: "missing_pr", rework: true, formal: true},
		{name: "held publication refuses wrong_base on new Runner lease", publication: "lost_create", publicationFault: "wrong_base", rework: true, formal: true},
		{name: "held publication refuses wrong_repository on new Runner lease", publication: "lost_create", publicationFault: "wrong_repository", rework: true, formal: true},
		{name: "held publication refuses wrong_head on new Runner lease", publication: "lost_create", publicationFault: "wrong_head", rework: true, formal: true},
		{name: "held publication refuses wrong_branch on new Runner lease", publication: "lost_create", publicationFault: "wrong_branch", rework: true, formal: true},
		{name: "held publication refuses multiple_prs on new Runner lease", publication: "lost_create", publicationFault: "multiple_prs", rework: true, formal: true},
		{name: "held publication refuses unavailable_forge on new Runner lease", publication: "lost_create", publicationFault: "unavailable_forge", rework: true, formal: true},
		{name: "held publication refuses missing_remote on new Runner lease", publication: "lost_create", publicationFault: "missing_remote", rework: true, formal: true},
		{name: "held publication refuses dirty_source on new Runner lease", publication: "lost_create", publicationFault: "dirty_source", rework: true, formal: true},
		{name: "held publication refuses changed_head on new Runner lease", publication: "lost_create", publicationFault: "changed_head", rework: true, formal: true},
		{name: "held publication refuses policy on new Runner lease", publication: "lost_create", publicationFault: "policy", rework: true, formal: true},
		{name: "held publication refuses lease on new Runner lease", publication: "lost_create", publicationFault: "lease", rework: true, formal: true},
		{name: "held publication refuses branch on new Runner lease", publication: "lost_create", publicationFault: "branch", rework: true, formal: true},
		{name: "absorbed Rework settles the original version on its actual base", absorbed: true, rework: true, land: true, wantState: "Done", wantChanges: 1},
		{name: "absorbed SSH Rework settles without a second provider session", absorbed: true, ssh: true, rework: true, land: true, wantState: "Done", wantChanges: 1},
		{name: "absorbed Rework preserves formal human rejection", absorbed: true, rework: true, formal: true, land: true, wantState: "Human Review", wantChanges: 1},
		{name: "absorbed Rework preserves a reported human hold", absorbed: true, hold: true, rework: true, land: true, wantState: "Human Review", wantChanges: 1},
		{name: "SSH native validator pass after publication", ssh: true, validator: "pass", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "dependency validation with large output", dependencyOnly: true, largeOutput: true, validator: "pass", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "dependency-only native validation evidence", dependencyOnly: true, validator: "pass", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "SSH dependency-only native validation evidence", ssh: true, dependencyOnly: true, validator: "pass", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "failed dependency check cannot pass review", dependencyOnly: true, failedCheck: true, validator: "pass", wantVerdict: "wait", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "native validator pass after publication", validator: "pass", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "native validator low score requests rework", lowScore: true, validator: "pass", wantVerdict: "rework", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "native validator rework retains version", validator: "rework", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "enrolled runner executes beside intake-off local runtime", localIntakeOff: true, staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "commits", commit: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "initial interactive code stays conversation owned", interactive: true, staged: true, wantNone: true, wantState: "In Progress"},
		{name: "host commits staged code", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "expired parent publishes completed staged code", expired: true, staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "expired parent publishes with native validation", expired: true, staged: true, validator: "pass", land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "expired parent publishes completed Rework commits", expired: true, rework: true, formal: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "ordinary staged code reaches landing", staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "successful base moved wait reclaims the reviewed unlanded version", staged: true, land: true, baseMoved: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "hosted template commits reach Human Review", hosted: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "hosted template without commits ends", hosted: true, wantState: "Done"},
		{name: "no commits", wantState: "Done"},
		{name: "uncommitted edits", dirty: true, wantNone: true, wantState: "In Progress"},
		{name: "Rework receives current Change discussion", rework: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "Rework receives formal requested changes", rework: true, formal: true, commit: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "host commits staged Rework", rework: true, formal: true, staged: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "clean Rework excludes target advancement", rework: true, formal: true, advanceTarget: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "staged Rework excludes target advancement", rework: true, formal: true, staged: true, advanceTarget: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "committed Rework excludes target advancement", rework: true, formal: true, commit: true, advanceTarget: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "late host conflict continues to resolved publication and landing", rework: true, formal: true, staged: true, lateConflict: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "host signing unavailable preserves requested changes", rework: true, formal: true, staged: true, signingFail: true},
		{name: "growing multi-page machine history keeps fresh conflict recovery compact", rework: true, staleBase: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "Rework lands the clean preserved reviewed head without source changes", rework: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "reopened completed Rework lands its reviewed current version", reopened: true, rework: true, staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "reopened Rework accepts current requested changes before repair", reopened: true, rework: true, formal: true, staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "Rework forwards a refused version to review", rework: true, commit: true, failVersion: true, wantState: "Human Review"},
		{name: "Rework scoped read failure releases claim before dispatch", rework: true, failDetail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			genuinePublication := test.publication != "" && test.validator != "" && test.publicationFault == ""
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
					states[2].Transitions = append(states[2].Transitions, "Merging")
					transitions = append(transitions, "Rework")
				}
				states = append(states, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: transitions})
			}
			if test.reopened {
				states[4].Transitions = []string{"Rework"}
			}
			if test.finalMessage != "" {
				states[3].Transitions = append(states[3].Transitions, "Blocked")
				states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Rework"}})
			}
			h := newNativeChangeHubTransport(t, review, states, true)
			var publicationCI *NativeClient
			if test.publication != "" {
				previous := h.descriptor.ID
				h.descriptor.Gates.GitHubPullRequest = true
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{Policy: h.descriptor, ExpectedID: previous}); err != nil {
					t.Fatal(err)
				}
			}
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
			if test.publication != "" {
				publicationCI = h.publicationCI(t)
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
			if test.dependencyOnly {
				for name, content := range map[string]string{
					"frontend/package.json": `{"dependencies":{"@radix-ui/themes":"1"}}`,
					".gitignore":            "frontend/.generated/\n.generated/\n",
					".test-bin/pnpm":        "#!/bin/sh\ntest \"$(cat package.json)\" = '{\"dependencies\":{}}' || exit 2\nmkdir -p .generated\nprintf 'pnpm %s passed\\n' \"$1\" | tee .generated/result\n",
					".test-bin/make":        "#!/bin/sh\ntest \"$(cat frontend/package.json)\" = '{\"dependencies\":{}}' || exit 2\nmkdir -p .generated\nprintf 'make %s passed\\n' \"$1\" | tee .generated/result\n",
				} {
					if test.largeOutput && name == ".test-bin/make" {
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
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(nativeChangeTempDir(t), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			var expected *tracker.ChangeDetail
			var candidate connector.Issue
			if test.rework && !genuinePublication {
				info, err := backend.Create(t.Context(), workspace.Issue{ProjectID: "local", ID: issue.ID, Identifier: issue.Identifier})
				if err != nil {
					t.Fatal(err)
				}
				reviewedPath := info.Path
				if err := os.WriteFile(filepath.Join(reviewedPath, "PRESERVED.md"), []byte("reviewed source\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				nativeChangeGit(t, reviewedPath, "add", "PRESERVED.md")
				nativeChangeGit(t, reviewedPath, "commit", "-m", "preserved reviewed source")
				item := tracker.NativeWorkItemID(issue.ID)
				if test.staleBase {
					if _, err := h.admin.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: "Human hold: preserve the original checkpoint and staged source"}); err != nil {
						t.Fatal(err)
					}
				}
				change, err := h.admin.CreateChange(t.Context(), item, tracker.CreateChange{Mutation: nativeMutationKey(), Title: issue.Title})
				if err != nil {
					t.Fatal(err)
				}
				old := h.publish(t, item, change.ID, strings.Repeat("a", 40))
				if _, err := h.admin.DiscussChange(t.Context(), item, change.ID, tracker.DiscussChange{Mutation: nativeMutationKey(), VersionID: old.ID, Body: "Historical discussion"}); err != nil {
					t.Fatal(err)
				}
				historicalDecision := "changes_requested"
				if test.reopened || test.publication != "" {
					historicalDecision = "approved"
				}
				if _, err := h.admin.ReviewChange(t.Context(), item, change.ID, old.ID, tracker.ReviewChange{Mutation: nativeMutationKey(), Decision: historicalDecision, Body: "Historical review"}); err != nil {
					t.Fatal(err)
				}
				if test.reopened {
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
				if publicationCI != nil {
					check := current.Checks[0]
					if _, err := publicationCI.SubmitChangeCheck(t.Context(), item, change.ID, current.ID, tracker.SubmitChangeCheck{Mutation: nativeMutationKey(), ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: current.HeadSHA, RunID: current.RunID, PolicyID: current.PolicyID, ConfigDigest: current.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: current.CreatedAt, Evidence: []tracker.ChangeArtifact{{Kind: "test", URI: "s3://fixture/test", SHA256: policy.Digest([]byte("test receipt")), Availability: "available"}}}}); err != nil {
						t.Fatal(err)
					}
				}

				if test.advanceTarget {
					nativeChangeGit(t, source, "merge", "--ff-only", current.HeadSHA)
					nativeChangeGit(t, source, "push", "origin", "main")
				}
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
				if test.staleBase {
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
				if test.failDetail {
					next := h.native.client.httpClient.Transport
					h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodGet && (strings.HasSuffix(request.URL.Path, "/changes/"+change.ID) || request.URL.Query().Get("view") == "recovery") {
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
			provider := &committingAgent{finalMessage: test.finalMessage, dependencyOnly: test.dependencyOnly, failedCheck: test.failedCheck, commit: test.commit, dirty: test.dirty, staged: test.staged || genuinePublication, validator: test.validator, lowScore: test.lowScore, complete: test.absorbed, hold: test.hold, inProgress: test.lateConflict}
			var targetHead string
			if test.advanceTarget {
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
			var runtimeStore store.Store
			if test.validator != "" {
				runtimeStore, err = store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runtimeStore.Close() })
			}
			validationCommand := "true"
			prerequisite := ""
			if test.publication == "confirmed" {
				validationCommand = "test -f CHANGE.md"
				if test.validator == "rework" {
					prerequisite = filepath.Join(t.TempDir(), "validator-ready")
					provider.validatorCommand = "test -f '" + strings.ReplaceAll(prerequisite, "'", "'\\''") + "'"
				}
			}
			if test.dependencyOnly {
				validationCommand = `PATH="$PWD/.test-bin:$PATH" make check-fast`
				candidate.Description = "Remove the unused direct @radix-ui/themes dependency. Required validation: frontend pnpm build, frontend pnpm check, root make check-fast."
			}
			gateMarker := filepath.Join(t.TempDir(), "gate-invoked")
			if test.finalMessage != "" {
				validationCommand = "printf gate > '" + strings.ReplaceAll(gateMarker, "'", "'\"'\"'") + "'; exit 2"
			}
			agent, err := runner.NewRunner(runner.Dependencies{
				Store:        runtimeStore,
				ProjectID:    "local",
				Workflow:     config.Workflow{Config: config.Config{Gate: gate.Config{Run: validationCommand, Validator: gate.ValidatorConfig{Enabled: test.validator != ""}}}, Prompt: "Complete the issue"},
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
			var oldPublicationHead, oldPublicationBranch string
			var forge *github.Client
			var forgeCreates int
			var forgePull string
			var publicationRetry bool
			losePush := test.publication == "lost_push"
			if test.publication != "" {
				forge, err = github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body := "[]"
					switch request.Method {
					case http.MethodGet:
						if test.publication != "confirmed" && !publicationRetry && (forgeCreates > 0 || test.publication == "lost_push" && !losePush) {
							return nil, fmt.Errorf("%w: forge observation unavailable after lost reply", ErrUnavailable)
						}
						if publicationRetry && test.publicationFault == "unavailable_forge" {
							return nil, fmt.Errorf("%w: forge unavailable", ErrUnavailable)
						}
						observedPull := forgePull
						if publicationRetry {
							switch test.publicationFault {
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
							if publicationRetry && test.publicationFault == "multiple_prs" {
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
						if forgeCreates == 1 && test.publication == "lost_create" {
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
				runExecution = &nativePublicationFixtureExecution{nativeExecution: execution.(*nativeExecution), forge: forge, losePush: &losePush}
			}
			runCtx, cancelRun := context.WithCancelCause(t.Context())
			defer cancelRun(context.Canceled)
			if test.expired {
				provider.afterTurn = func() { cancelRun(context.DeadlineExceeded) }
			}
			result, err := agent.Run(runCtx, runner.RunRequest{Execution: runExecution, DeferExecutionFinish: test.failVersion, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
			if test.finalMessage != "" {
				if err != nil || result.FinalState != runner.FinalStateCompleted || result.NativeChange != nil || provider.calls != 1 {
					t.Fatalf("blocked Rework ran source completion: result=%+v turns=%d error=%v", result, provider.calls, err)
				}
				if _, err := os.Stat(gateMarker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("blocked Rework consumed the failing gate: %v", err)
				}
				owner := execution.(*nativeExecution)
				checkpoint := owner.data.Handoff
				if checkpoint == nil || checkpoint.HeadSHA != expected.Versions[len(expected.Versions)-1].HeadSHA || checkpoint.WorkspaceDigest == "" || checkpoint.Availability != "available" || checkpoint.ExternalEffect != "none" || checkpoint.EffectState != "none" {
					t.Fatalf("blocked Rework lost its source checkpoint: %+v", checkpoint)
				}
				if test.staged {
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
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "succeeded" || recovery.Attempts[0].TerminalFailure != nil || !reflect.DeepEqual(recovery.Attempts[0].Disposition, test.wantDisposition) || !reflect.DeepEqual(recovery.Attempts[0].Checkpoint, checkpoint) {
					t.Fatalf("blocked Rework lost its durable handoff: %+v, %v", recovery.Attempts, err)
				}
				if len(h.candidatesIn(t, "Rework")) != 0 || h.state(t, issue.ID) != "Blocked" {
					t.Fatal("blocked unchanged source was immediately offered again")
				}
				return
			}
			if test.publication != "" {
				owner := execution.(*nativeExecution)
				if genuinePublication {
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), owner.change.ChangeID)
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
					if test.formal {
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
				if test.publication == "lost_push" {
					wantEffect, wantState, wantCreates = "git_push", "pending", 0
				}
				if test.publication == "confirmed" {
					if err != nil || owner.publication == nil || owner.data.Handoff == nil || forgeCreates != 1 || result.NativeChange == nil || result.NativeChange.Validator == nil {
						t.Fatalf("initial confirmed publication lost native validation: change=%+v error=%v", result.NativeChange, err)
					}
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if readErr != nil || current.Change.CurrentVersion != expected.Change.CurrentVersion || result.NativeChange.Validator.Verdict != test.validator {
						t.Fatalf("initial validator did not retain immutable source: detail=%+v error=%v", current, readErr)
					}
					if test.validator != "pass" && (current.Summary.Status != "needs_evidence" || result.NativeChange.Reviewed) {
						t.Fatal("confirmed publication alone yielded readiness")
					}
					if prerequisite != "" {
						commands := result.NativeChange.Validator.Commands
						if len(commands) != 2 || commands[1].ExitCode == 0 {
							t.Fatalf("validator rework lacks failed prerequisite command: %+v", commands)
						}
						if err := os.WriteFile(prerequisite, []byte("ready"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					expected = &current
					provider.validator = "pass"
				} else if err == nil || owner.data.Handoff == nil || owner.data.Handoff.ExternalEffect != wantEffect || owner.data.Handoff.EffectState != wantState || forgeCreates != wantCreates {
					t.Fatalf("first Runner attempt did not preserve lost create: effect=%+v creates=%d error=%v", owner.data.Handoff, forgeCreates, err)
				}
				if test.staged {
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if readErr != nil || len(current.Versions) != len(expected.Versions)+1 || !reflect.DeepEqual(current.Reviews, expected.Reviews) || !reflect.DeepEqual(current.Checks, expected.Checks) {
						t.Fatal("staged publication changed prior immutable evidence")
					}
					expected = &current
				}
				oldCheckpoint := *owner.data.Handoff
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
				if test.publicationFault == "dirty_source" {
					if err := os.WriteFile(filepath.Join(provider.workspace, "UNFINISHED.md"), []byte("preserve unfinished source"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if test.publicationFault == "changed_head" {
					nativeChangeGit(t, provider.workspace, "commit", "--allow-empty", "-m", "new local source")
				}
				if test.publicationFault == "branch" {
					nativeChangeGit(t, provider.workspace, "branch", "-m", "renamed-publication")
				}
				if test.publicationFault == "missing_remote" {
					nativeChangeGit(t, remote, "update-ref", "-d", "refs/heads/"+oldPublicationBranch)
				}

				oldAttempt, oldLease := owner.data.AttemptID, owner.claim.lease
				if genuinePublication {
					currentIssue, readErr := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
					if readErr != nil {
						t.Fatal(readErr)
					}
					for _, state := range []string{"Human Review", "Rework"} {
						currentIssue, readErr = h.admin.Transition(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: currentIssue.Revision, State: state, Reason: "user_requested"})
						if readErr != nil {
							t.Fatal(readErr)
						}
					}
				}
				if err := h.native.Release(t.Context(), oldLease, "released"); err != nil {
					t.Fatal(err)
				}
				machineID := tracker.MachineID("machine-native-change")
				if test.publicationFault == "foreign_source" {
					machineID = "foreign-machine"
				}
				h.scheduler, err = NewScheduler(h.scheduler.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: machineID, Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				if test.publicationFault == "policy" {
					h.repolicy(t)
				}
				publicationRetry = true
				candidates := h.candidatesIn(t, "Rework")

				if len(candidates) != 1 {
					t.Fatalf("unchanged held source not admitted: candidates=%d", len(candidates))
				}
				candidate = candidates[0]
				if _, err := h.scheduler.AdoptClaim(t.Context(), candidate, time.Now()); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				owner = execution.(*nativeExecution)
				if owner.claim.lease.MachineID != machineID || owner.claim.lease.ID == oldLease.ID || owner.claim.lease.FencingToken <= oldLease.FencingToken {
					t.Fatal("retry reused old lease authority")
				}
				if test.publicationFault == "version" {
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
				if test.publicationFault == "unavailable_source" {
					next := h.native.client.httpClient.Transport
					h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") {
							return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"insufficient_scope","message":"source read refused"}`))}, nil
						}
						return next.RoundTrip(request)
					})
				}
				if test.publicationFault == "lease" {
					if err := h.native.Release(t.Context(), owner.claim.lease, "released"); err != nil {
						t.Fatal(err)
					}
				}
				remoteBefore, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "show-ref").Output()
				if readErr != nil {
					t.Fatal(readErr)
				}
				prBefore := forgePull
				callsBefore := provider.calls
				result, err = agent.Run(t.Context(), runner.RunRequest{Execution: &nativePublicationFixtureExecution{nativeExecution: owner, forge: forge, losePush: &losePush}, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
				result.NativeChange = owner.NativeChange()
				if genuinePublication && test.publication != "confirmed" {
					var refusal *APIError
					if !errors.As(err, &refusal) || refusal.Status != http.StatusUnprocessableEntity || !strings.Contains(refusal.Message, "session separate from the implementing session") || provider.calls != callsBefore+1 || owner.publication == nil || forgeCreates != 1 {
						t.Fatalf("publication-only retry bypassed validator session authority: calls=%d error=%v", provider.calls-callsBefore, err)
					}
					detail, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if readErr != nil || !reflect.DeepEqual(detail.Reviews, expected.Reviews) || detail.Change.CurrentVersion != expected.Change.CurrentVersion || detail.Summary.Status == "reviewed" {
						t.Fatalf("publication-only retry manufactured validator readiness: detail=%+v error=%v", detail, readErr)
					}
					return
				}
				if test.publicationFault != "" {
					if err == nil && (result.NativeChange == nil || result.NativeChange.VersionError == "" && result.NativeChange.Error == "") || owner.publication != nil || forgeCreates != wantCreates || forgePull != prBefore || provider.calls != callsBefore {
						t.Fatalf("unresolved publication was accepted or rewrote forge/source: fault=%s creates=%d publication=%v provider_calls=%d error=%v", test.publicationFault, forgeCreates, owner.publication != nil, provider.calls-callsBefore, err)
					}
					remoteAfter, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "show-ref").Output()
					if readErr != nil || string(remoteBefore) != string(remoteAfter) {
						t.Fatal("refused reconciliation modified bare remote")
					}
				} else {
					wantCalls := callsBefore
					if test.publication == "confirmed" {
						wantCalls++
					}
					if test.validator != "" && expected.Summary.Status != "reviewed" {
						wantCalls++
						if result.NativeChange == nil || result.NativeChange.Validator == nil || result.NativeChange.Validator.Verdict != "pass" {
							t.Fatalf("publication recovery skipped the pending native validator: change=%+v error=%v", result.NativeChange, err)
						}
					}
					if provider.calls != wantCalls {
						t.Fatalf("publication recovery provider calls = %d, want %d", provider.calls, wantCalls)
					}
				}

				if test.publicationFault == "" && (err != nil || result.NativeChange == nil || owner.publication == nil || forgeCreates != 1) {
					t.Fatalf("real Runner retry failed exact PR adoption: creates=%d publication=%v error=%v", forgeCreates, owner.publication != nil, err)
				}
				item := tracker.NativeWorkItemID(issue.ID)
				oldEvidence, err := h.admin.RuntimeEvidence(t.Context(), item, oldAttempt)
				if err != nil || oldEvidence.Attempt == nil || oldEvidence.Attempt.Checkpoint == nil || !reflect.DeepEqual(*oldEvidence.Attempt.Checkpoint, oldCheckpoint) {
					t.Fatalf("retry changed old attempt checkpoint: %v", err)
				}
				detail, err := h.admin.Change(t.Context(), item, expected.Change.ID)
				if err != nil {
					t.Fatal(err)
				}
				for i := range detail.Versions {
					detail.Versions[i].External = nil
					expected.Versions[i].External = nil
				}
				if test.validator != "" && test.publicationFault == "" && expected.Summary.Status != "reviewed" {
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
				if test.publicationFault != "" {
					return
				}
				var visible tracker.NativeIssue
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"?include=change", nil, &visible); err != nil {
					t.Fatal(err)
				}
				if visible.Change == nil || visible.Change.URL != nativeChangeRepository+"/pull/7" || visible.PullRequest == nil || visible.PullRequest.Number != 7 {
					t.Fatal("reconciled PR absent from existing issue/PR reads")
				}
				target, err := execution.(runner.LandingExecution).LandingTarget(t.Context())
				if !test.formal && test.publication == "confirmed" {
					if err != nil || !result.NativeChange.Reviewed || target.VersionID != expected.Change.CurrentVersion {
						t.Fatalf("fresh applicable native decision did not establish readiness: target=%+v error=%v", target, err)
					}
				} else if !errors.Is(err, runner.ErrLandingNotReviewed) || target.External == nil || target.External.URL != visible.Change.URL {
					t.Fatal("publication cleared human hold or lost landing PR identity")
				}
				return
			}
			if test.expired {
				evidence, readErr := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
				if !errors.Is(context.Cause(runCtx), context.DeadlineExceeded) || readErr != nil || evidence.Attempt == nil || evidence.Attempt.Status != "succeeded" {
					t.Fatalf("completed turn lost after parent expiry: attempt=%+v error=%v", evidence.Attempt, readErr)
				}
			}
			if test.ssh && err == nil {
				result.NativeChange = execution.(runner.ChangeExecution).NativeChange()
			}
			if test.absorbed {
				change := result.NativeChange
				if err != nil || change == nil || provider.calls != 1 || change.ChangeID != expected.Change.ID {
					t.Fatalf("absorbed Rework did not finish once: result=%+v calls=%d error=%v", result, provider.calls, err)
				}
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if err != nil || !reflect.DeepEqual(versionsWithoutLanding(detail.Versions), versionsWithoutLanding(expected.Versions)) || detail.Change.CurrentVersion != expected.Change.CurrentVersion {
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
					evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), attempt)
					if err != nil || evidence.Attempt == nil || evidence.Attempt.Finalization == nil || evidence.Attempt.Finalization.SourceVersion == nil || evidence.Attempt.Finalization.SourceVersion.VersionID != expected.Change.CurrentVersion || evidence.Attempt.Finalization.VersionError != change.VersionError || evidence.Attempt.ClaimReleasedAt == nil {
						t.Fatalf("refusal projection lost existing source provenance: %+v, %v", evidence.Attempt, err)
					}
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
				currentFeedback := *expected
				currentFeedback.Versions = slices.DeleteFunc(slices.Clone(expected.Versions), func(v tracker.ChangeVersion) bool { return v.ID != expected.Change.CurrentVersion })
				currentFeedback.Reviews = slices.DeleteFunc(slices.Clone(expected.Reviews), func(r tracker.ChangeReview) bool { return r.VersionID != expected.Change.CurrentVersion })
				currentFeedback.Checks = slices.DeleteFunc(slices.Clone(expected.Checks), func(c tracker.ChangeCheck) bool { return c.VersionID != expected.Change.CurrentVersion })
				currentFeedback.Discussion = slices.DeleteFunc(slices.Clone(expected.Discussion), func(d tracker.ChangeDiscussion) bool {
					return d.VersionID != "" && d.VersionID != expected.Change.CurrentVersion
				})
				if test.staleBase {
					t.Logf("fresh conflict provider request: %d bytes", len(provider.prompt))
				}
				if test.staleBase && (len(provider.prompt) >= 1048576 || len(recovery.Attempts) > 4 || len(recovery.History) != 0 || !strings.Contains(provider.prompt, "Human hold: preserve the original checkpoint")) {
					t.Fatalf("fresh provider request replayed historic recovery: bytes=%d", len(provider.prompt))
				}
				if recovery.Change == nil || recovery.Change.VersionID != expected.Change.CurrentVersion || !reflect.DeepEqual(recovery.ChangeDetail, &currentFeedback) {
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
			if test.advanceTarget {
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				owner := execution.(*nativeExecution).data
				if targetHead == "" || stored.BaseSHA != targetHead || stored.HeadSHA != execution.(*nativeExecution).worktreeHead || stored.AttemptID != owner.AttemptID || stored.Producer.FencingToken != execution.Recovery().Lease.FencingToken {
					t.Fatalf("final diff lost prepared source or fenced attempt identity: %+v, target=%s", stored, targetHead)
				}
				if test.staged || test.commit {
					if stored.HeadSHA == stored.BaseSHA || len(stored.Files) != 1 || stored.Files[0].Path != "CHANGE.md" {
						t.Fatalf("final diff did not preserve only the owned issue delta: %+v", stored)
					}
				} else {
					if stored.HeadSHA != stored.BaseSHA || len(stored.Files) != 0 {
						t.Fatalf("clean Rework attributed upstream work to the issue: %+v", stored)
					}
					detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
					if err != nil || !reflect.DeepEqual(detail, *expected) || change == nil || change.VersionID != "" || change.Landing != nil || change.VersionError == "" {
						t.Fatalf("clean Rework changed historical provenance or waived refusal: change=%+v detail=%+v error=%v", change, detail, err)
					}
					h.complete(t, issue.ID, change)
					if h.state(t, issue.ID) != test.wantState || len(h.changes(t, issue.ID)) != test.wantChanges {
						t.Fatal("clean Rework published upstream work or lost review handoff")
					}
					return
				}
			}
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
				if test.dependencyOnly {
					receipts := detail.Reviews[0].Validator.Commands
					if len(receipts) != 3 {
						t.Fatalf("command evidence missing from native review: %+v", receipts)
					}
					for index, receipt := range receipts {
						wantExit := 0
						if test.failedCheck && index == 2 {
							wantExit = 7
						}
						if receipt.HeadSHA != change.HeadSHA || receipt.TreeSHA == "" || receipt.TreeSHA != receipts[0].TreeSHA || receipt.ExitCode != wantExit || receipt.Output == "" {
							t.Fatalf("unverified native command receipt: %+v", receipt)
						}
					}
					if receipts[0].OutputTruncated != test.largeOutput {
						t.Fatalf("large output evidence not preserved: truncated=%t", receipts[0].OutputTruncated)
					}
					if change.Reviewed == test.failedCheck {
						t.Fatalf("review readiness ignored actual validation: %+v", change)
					}
				}

			}
			if test.failVersion {
				if change == nil || change.ChangeID != expected.Change.ID || change.VersionID != "" || change.Reviewed || change.Error != "" || change.VersionCode != "invalid_request" || !strings.Contains(change.VersionError, "version publication refused") {
					t.Fatalf("runner lost refused publication result: %+v", change)
				}
				h.complete(t, issue.ID, change)
				evidence, readErr := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
				if readErr != nil || evidence.Attempt == nil || evidence.Attempt.Finalization == nil || evidence.Attempt.Finalization.VersionError != change.VersionError || evidence.Attempt.Finalization.VersionCode != change.VersionCode || evidence.Attempt.ClaimReleasedAt == nil {
					t.Fatalf("publication refusal was omitted from the terminal receipt: %+v, %v", evidence.Attempt, readErr)
				}
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
			if test.reopened {
				reviewed, err := h.connector.ChangeReviewed(t.Context(), issue.ID, change.ChangeID, change.VersionID)
				if err != nil || !reviewed {
					t.Fatalf("completed Rework lost current review after an earlier landing: reviewed=%t, error=%v", reviewed, err)
				}
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
				expectedPath := map[bool]string{false: "CHANGE.md", true: "PRESERVED.md"}[test.land && test.rework && !test.lateConflict]
				if test.dependencyOnly {
					expectedPath = "frontend/package.json"
				}
				if stored.HeadSHA != change.HeadSHA || !test.land && stored.BaseSHA != change.BaseSHA || !nativeDiffHas(stored.Files, expectedPath) {
					t.Fatalf("stored diff = %#v, reported %#v", stored, change)
				}
				head, err := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD").Output()
				if err != nil || strings.TrimSpace(string(head)) != stored.HeadSHA {
					t.Fatalf("published head is not finalized Git HEAD: %s, %v", head, err)
				}
				if !test.rework || !test.land || test.lateConflict || test.staleBase {
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
					if test.reopened && (detail.Change.CurrentLanding() != nil || !reflect.DeepEqual(detail.Change.Landed, expected.Change.Landed)) {
						t.Fatal("new publication lost the historical landing or treated it as current")
					}
					if test.land && !test.lateConflict && !test.staleBase && !test.reopened {
						if len(detail.Versions) != len(expected.Versions) || change.VersionID != expected.Change.CurrentVersion || change.HeadSHA != expected.Versions[len(expected.Versions)-1].HeadSHA {
							t.Fatalf("unchanged Rework replaced the immutable version: change=%+v, detail=%+v", change, detail)
						}
					} else if len(detail.Versions) != len(expected.Versions)+1 {
						t.Fatalf("rework did not publish one new version: %+v", detail)
					}
					if test.staleBase {
						if change.HeadSHA == expected.Versions[len(expected.Versions)-1].HeadSHA || !reflect.DeepEqual(detail.Versions[:len(expected.Versions)], expected.Versions) || stored.HeadSHA == stored.BaseSHA || len(stored.Files) != 1 || stored.Files[0].Path != "PRESERVED.md" || !change.Reviewed {
							t.Fatalf("base refresh lost immutable history, exact-head review or issue-owned delta: change=%+v diff=%+v versions=%+v", change, stored, detail.Versions)
						}
					}
					if !reflect.DeepEqual(detail.Reviews, expected.Reviews) || !reflect.DeepEqual(detail.Discussion, expected.Discussion) {
						t.Fatal("new version changed historical feedback or fabricated review")
					}
					current := detail.Versions[len(detail.Versions)-1]
					if change.VersionID == "" || (!test.land || test.lateConflict || test.staleBase) && change.VersionID == expected.Change.CurrentVersion || detail.Change.CurrentVersion != change.VersionID || current.ID != change.VersionID || current.HeadSHA != stored.HeadSHA || current.PolicyID != h.descriptor.ID || current.Repository != nativeChangeRepository || current.Code.URI != nativeChangeRepository+"/commit/"+stored.HeadSHA {
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
				if test.dependencyOnly {
					path, expectedContent = "frontend/package.json", `{"dependencies":{}}`
				}
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
	calls            int
	bound            bool
	duringTurn       func()
	afterTurn        func()
}

func (a *committingAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handler runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
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

func (*committingAgent) SupportsLiveControl() bool { return true }

func (a *committingAgent) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if a.afterTurn != nil {
		defer a.afterTurn()
	}
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
	if a.duringTurn != nil {
		a.duringTurn()
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
		{"config", "commit.gpgsign", "false"}, {"config", "core.hooksPath", os.DevNull},
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
	scratch := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, scratch)
	}
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
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

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

func versionsWithoutLanding(versions []tracker.ChangeVersion) []tracker.ChangeVersion {
	stripped := make([]tracker.ChangeVersion, len(versions))
	for i, version := range versions {
		version.Landing = nil
		stripped[i] = version
	}
	return stripped
}

type nativePublicationFixtureExecution struct {
	*nativeExecution
	forge    *github.Client
	losePush *bool
}

func (e *nativePublicationFixtureExecution) SetPublicationSource(source func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)) {
	e.nativeExecution.SetPublicationSource(func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
		opts.GitHubClient = e.forge
		if e.losePush != nil {
			effect := opts.PublicationEffect
			opts.PublicationEffect = func(ctx context.Context, kind, state string, publication workspace.GitHubPublication) error {
				if *e.losePush && kind == "git_push" && state == "confirmed" {
					*e.losePush = false
					return fmt.Errorf("%w: push confirmation lost after remote accepted head", ErrUnavailable)
				}
				return effect(ctx, kind, state, publication)
			}
		}
		return source(ctx, version, opts)
	})
}

func (h *nativeChangeHub) publicationCI(t *testing.T) *NativeClient {
	t.Helper()
	rules, err := h.admin.ChangeReviewPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "publication-fixture-ci", "scope": "operator"}, &token); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: h.admin.client.baseURL.String(), TokenSource: func() string { return token.Token }, HTTPClient: h.admin.client.httpClient})
	if err != nil {
		t.Fatal(err)
	}
	ci, err := client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	rules.PolicyID = h.descriptor.ID
	rules.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "full-ci", PrincipalID: token.ID, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), ExpectedID: rules.ID, Policy: rules}); err != nil {
		t.Fatal(err)
	}
	return ci
}
