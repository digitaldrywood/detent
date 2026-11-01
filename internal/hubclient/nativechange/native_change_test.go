package nativechange

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

var nativeChangeFixtureRoot string

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	scratch, err := os.MkdirTemp("", "detent-hubclient-test-")
	if err != nil {
		panic(err)
	}
	nativeChangeFixtureRoot = scratch
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if err := os.Setenv(key, scratch); err != nil {
			panic(err)
		}
	}
	for key, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "Test User", "GIT_AUTHOR_EMAIL": "test@example.com", "GIT_COMMITTER_NAME": "Test User", "GIT_COMMITTER_EMAIL": "test@example.com"} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(scratch); err != nil {
		panic(err)
	}
	os.Exit(code)
}

const nativeChangeAdminToken = "native-change-admin"

var nativeRuntimeDatabasePath = testenv.DatabaseTemplate(func(ctx context.Context, path string) (io.Closer, error) {
	return store.Open(ctx, store.Config{Path: path})
})

// nativeChangeHub is a real hub with a native project whose workflow has a
// review lane, and a worker client and scheduler that claim from it.
type nativeChangeHub struct {
	leaseHolds      atomic.Int64
	clock           func() time.Time
	reopen          func(func(string) error)
	sourceDirectory string
	review          string
	organization    tracker.OrganizationID
	project         tracker.ProjectID
	descriptor      policy.Descriptor
	admin           *NativeClient
	native          *NativeClient
	connector       *NativeConnector
	scheduler       *Scheduler
	failChanges     *changeFailingTransport
	otherWorker     func(*testing.T) *NativeClient
}

func (h *nativeChangeHub) leaseHold(context.Context) (func(), error) {
	h.leaseHolds.Add(1)
	return func() {}, nil
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
	return newNativeChangeHubConfigured(t, review, states, inMemory, requireDependencies, config, repositoryBackend...)
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

func newNativeChangeHubConfigured(t *testing.T, review string, states []tracker.NativeState, inMemory, requireDependencies bool, config hubserver.Config, repositoryBackend ...hubserver.ReconcileBackend) *nativeChangeHub {
	t.Helper()
	if config.Clock == nil {
		config.Clock = time.Now
	}

	if config.Conversation != nil && config.Conversation.Enabled && config.Conversation.ArchiveContents == nil {
		conversationConfig := *config.Conversation
		conversationConfig.ArchiveContents = scenariotest.NewConversationArchive()
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
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	if err := admin.Request(t.Context(), http.MethodGet, "/api/v2/organizations", nil, &organizations); err != nil {
		t.Fatal(err)
	}
	h := &nativeChangeHub{clock: config.Clock, review: review, organization: organizations.Items[0].ID, descriptor: clientTestPolicy()}
	reopens := 0
	h.reopen = func(seed func(string) error) {
		t.Helper()
		requests.Lock()
		defer requests.Unlock()
		if err := service.Close(); err != nil {
			t.Fatalf("close fixture before seed: %v", err)
		}
		reopens++
		seedPath := filepath.Join(filepath.Dir(config.DatabasePath), fmt.Sprintf("seed-%d.db", reopens))
		if err := hubserver.BackupDatabase(t.Context(), config.DatabasePath, seedPath); err != nil {
			t.Fatalf("snapshot stopped fixture: %v", err)
		}
		if err := seed(seedPath); err != nil {
			t.Fatalf("seed stopped fixture: %v", err)
		}
		config.DatabasePath = seedPath
		service, err = hubserver.Open(t.Context(), config)
		if err != nil {
			t.Fatalf("reopen seeded fixture: %v", err)
		}
	}
	var project tracker.NativeProject
	body := map[string]any{"name": "native-change", "idempotency_key": "project-native-change", "states": states, "require_dependencies": requireDependencies}
	if err := admin.Request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", body, &project); err != nil {
		t.Fatal(err)
	}
	h.project = project.ID
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := admin.Request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "native-change-worker", "scope": "worker"}, &token); err != nil {
		t.Fatal(err)
	}
	if err := admin.Request(t.Context(), http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
		t.Fatal(err)
	}
	if h.admin, err = admin.Native(h.organization, h.project); err != nil {
		t.Fatal(err)
	}
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	h.otherWorker = func(t *testing.T) *NativeClient {
		t.Helper()
		var other struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := admin.Request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "native-change-other-worker", "scope": "worker"}, &other); err != nil {
			t.Fatal(err)
		}
		if err := admin.Request(t.Context(), http.MethodPost, "/api/v2/tokens/"+other.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
			t.Fatal(err)
		}
		client, err := newTestClient(t, Config{URL: serverURL, TokenSource: func() string { return other.Token }, HTTPClient: httpClient})
		if err != nil {
			t.Fatal(err)
		}
		native, err := client.Native(h.organization, h.project)
		if err != nil {
			t.Fatal(err)
		}
		return native
	}
	h.failChanges = &changeFailingTransport{next: httpClient.Transport}
	worker, err := newTestClient(t, Config{URL: serverURL, TokenSource: func() string { return token.Token }, HTTPClient: &http.Client{Transport: h.failChanges}})
	if err != nil {
		t.Fatal(err)
	}
	if h.native, err = worker.Native(h.organization, h.project); err != nil {
		t.Fatal(err)
	}
	if h.connector, err = NewNativeConnector(h.native); err != nil {
		t.Fatal(err)
	}
	h.scheduler, err = newTestScheduler(worker, SchedulerConfig{
		OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project},
		Machine:           Machine{ID: "machine-native-change", Hostname: "host", Capacity: 1, Version: "test"},
		HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second,
		LeaseHold: h.leaseHold,
		Now:       time.Now,
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
	issue, err := h.connector.CreateIssue(t.Context(), connector.IssueDraft{Title: title, Body: "Change the README." + issueContractTestSections})
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
		page, readErr := h.admin.Issues(t.Context(), nil)
		for _, issue := range page.Items {
			t.Logf("candidate refusal issue=%s state=%s contract=%+v sections=%v", issue.WorkItemID, issue.State, issue.IssueContract, config.IssueContractSectionDigests(issue.Body))
		}
		t.Logf("candidate refusal issue read: %v", readErr)
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
	return h.publishCapturedExternal(t, item, changeID, head, captured, nil, previous...)
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

func nativeChangeFixtureCommit(label string) string {
	return "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Test User <test@example.com> 1 +0000\ncommitter Test User <test@example.com> 1 +0000\n\n" + label + "\n"
}

func nativeChangeFixtureHead(label string) string {
	commit := nativeChangeFixtureCommit(label)
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("commit %d\x00%s", len(commit), commit))))
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

func nativeChangeDiff(head string, files ...string) runner.AttemptDiffSource {
	return func(context.Context) (tracker.AttemptDiffRequest, bool) {
		request := tracker.AttemptDiffRequest{BaseSHA: nativeChangeFixtureHead("a"), HeadSHA: head, Files: []tracker.AttemptDiffFile{}}
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

func TestNativeExecutionResumedVersionPublication(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusal=%t", refusal), func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			var otherProject tracker.NativeProject
			if err := h.admin.Transport().Request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", map[string]any{"name": "another-project", "idempotency_key": "another-project", "states": []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}, &otherProject); err != nil {
				t.Fatal(err)
			}
			foreign, err := h.native.Transport().Native(h.organization, otherProject.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, title := range []string{"First repair", "Second repair"} {
				issue := h.createInProgress(t, title)
				candidate := h.claim(t, issue.ID)
				item := tracker.NativeWorkItemID(issue.ID)
				e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				e.SetRepository(nativeChangeRepository)
				e.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return nativeChangeSourceFixture(ctx, t, base, head), nil
				})
				if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "unpushed", HeadSHA: nativeChangeFixtureHead("c"), ExternalEffect: "none", EffectState: "none"}
				e.SetDiffSource(nativeChangeDiff(checkpoint.HeadSHA, "README.md"))
				if err := e.Checkpoint(t.Context(), checkpoint); err != nil {
					t.Fatal(err)
				}
				diff, _ := nativeChangeDiff(checkpoint.HeadSHA, "README.md")(t.Context())
				changeID, err := e.openChange(t.Context(), diff)
				if err != nil {
					t.Fatal(err)
				}
				transport := fixtureClient(h.native.Transport()).requests.Load()
				var requests []tracker.PublishChangeVersion
				reject := false
				fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/versions") {
						body, err := io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						request.Body = io.NopCloser(bytes.NewReader(body))
						var input tracker.PublishChangeVersion
						if err := json.Unmarshal(body, &input); err != nil {
							return nil, err
						}
						requests = append(requests, input)
						if reject {
							return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"idempotency_conflict","message":"different content"}`)), Request: request}, nil
						}
					}
					return transport.RoundTrip(request)
				}))
				h.failChanges.dropVersions.Store(true)
				if _, _, err := e.publishChangeVersion(t.Context(), changeID, diff); err == nil {
					t.Fatal("lost publication acknowledgment was reported as success")
				}
				first, err := h.native.PublishChangeVersion(t.Context(), item, changeID, requests[0])
				if err != nil {
					t.Fatalf("identical retry after acknowledgment loss: %v", err)
				}
				if id, _, err := e.publishChangeVersion(t.Context(), changeID, diff); err != nil || id != first.ID {
					t.Fatalf("reconcile acknowledged version: %s, %v", id, err)
				}
				if _, err := e.RecoverChangeSource(t.Context()); err != nil {
					t.Fatal(err)
				}
				saved, attempt := e.CompletionState(), e.data.AttemptID
				h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: fixtureSchedulerMachine(h.scheduler), HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
					t.Fatal(err)
				}
				e = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				if e.data.AttemptID != attempt || e.retainedSource == nil || e.retainedSource.Source.HeadSHA != first.HeadSHA {
					t.Fatal("restoration lost the attempt or retained source")
				}
				for _, boundary := range []string{"expected version", "lease", "fence"} {
					invalid := requests[0]
					invalid.IdempotencyKey = title + boundary
					invalid.ExpectedVersionID = first.ID
					switch boundary {
					case "expected version":
						invalid.ExpectedVersionID = ""
					case "lease":
						invalid.LeaseID = "00000000-0000-4000-8000-000000000000"
					case "fence":
						invalid.FencingToken++
					}
					if _, err := h.native.PublishChangeVersion(t.Context(), item, changeID, invalid); err == nil || hubErrorCode(err) == "idempotency_conflict" {
						t.Fatalf("%s did not refuse through its authority boundary: %v", boundary, err)
					}
				}
				e.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return nativeChangeSourceFixture(ctx, t, base, head), nil
				})
				checkpoint.HeadSHA = nativeChangeFixtureHead("d")
				e.SetDiffSource(nativeChangeDiff(checkpoint.HeadSHA, "README.md"))
				if err := e.Checkpoint(t.Context(), checkpoint); err != nil {
					t.Fatal(err)
				}
				reject = refusal
				if err := e.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
					t.Fatal(err)
				}
				reject = false
				if err := e.Finish(t.Context(), "succeeded"); err != nil {
					t.Fatal(err)
				}
				detail, err := h.admin.Change(t.Context(), item, changeID)
				if err != nil {
					t.Fatal(err)
				}
				wantVersions, wantStatus := 2, "succeeded"
				if refusal {
					wantVersions, wantStatus = 1, "failed"
				}
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != wantStatus || len(detail.Versions) != wantVersions {
					t.Fatalf("resumed publication: versions=%+v attempts=%+v error=%v", detail.Versions, recovery.Attempts, err)
				}
				if refusal {
					finalization := recovery.Attempts[0].Finalization
					if finalization == nil || finalization.VersionCode != "idempotency_conflict" || finalization.VersionID != "" || finalization.Reviewed || e.retainedSource.Source.HeadSHA != checkpoint.HeadSHA {
						t.Fatalf("refusal lost incomplete publication or recoverable source: %+v", finalization)
					}
					var retained nativeCompletionState
					if err := json.Unmarshal(e.CompletionState(), &retained); err != nil || retained.RetainedSource == nil {
						t.Fatalf("refusal lost recoverable completion source: %v", err)
					}
					if err := workspace.VerifyChangeSource(t.Context(), retained.RetainedSource.Source, retained.RetainedSource.Bundle); err != nil {
						t.Fatalf("refused source cannot be recovered: %v", err)
					}
				} else {
					if detail.Versions[1].HeadSHA != checkpoint.HeadSHA || detail.Versions[1].AttemptID != attempt || requests[0].IdempotencyKey == requests[len(requests)-1].IdempotencyKey {
						t.Fatal("changed candidate reused earlier publication identity")
					}
					if _, err := h.native.PublishChangeVersion(t.Context(), item, changeID, requests[len(requests)-1]); err != nil {
						t.Fatalf("identical changed-candidate retry: %v", err)
					}
				}
				if _, err := foreign.PublishChangeVersion(t.Context(), item, changeID, requests[0]); err == nil {
					t.Fatal("cross-project publication accepted")
				}
				bundle, err := h.admin.ChangeSource(t.Context(), item, changeID, first.ID)
				if err != nil || !bytes.Equal(bundle, requests[0].SourceBundle) {
					t.Fatalf("publication changed the earlier source bundle: %v", err)
				}
				fixtureClient(h.native.Transport()).requests.Store(transport)
				h.complete(t, issue.ID, e.NativeChange())
			}
		})
	}
}

func TestNativeExecutionSettlesBeforeFinishing(t *testing.T) {
	for _, test := range []struct {
		name           string
		outage         bool
		completeDuring bool
		version        bool
		restart        bool
		ackLost        bool
	}{
		{name: "finish event"},
		{name: "pending finish event survives completion restore", restart: true},
		{name: "acknowledged finish event survives completion restore", restart: true, ackLost: true},
		{name: "version publication", version: true},
		{name: "completion during four minute outage", outage: true, completeDuring: true},
		{name: "completion after four minute outage", outage: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newNativeChangeHub(t, true)
				issue := h.createInProgress(t, "Native change")
				candidate := h.claim(t, issue.ID)
				if got := h.leaseHolds.Load(); got != 1 {
					t.Fatalf("claim acquired %d fixture lease holds, want 1", got)
				}
				execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				originalTransport := fixtureClient(h.native.Transport()).requests.Load()
				var offline atomic.Bool
				if test.outage {
					fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if offline.Load() {
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"tenant_unavailable"}`)), Request: request}, nil
						}
						return originalTransport.RoundTrip(request)
					}))
				}
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				execution.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("c"), "README.md"))
				execution.SetRepository(nativeChangeRepository)
				if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: "unpushed", ExternalEffect: "none", EffectState: "none"}
				if err := execution.Checkpoint(guarded, checkpoint); err != nil {
					t.Fatal(err)
				}
				originalLease := execution.claim.lease
				if test.restart {
					if err := execution.PrepareFinish(guarded, "succeeded", "", nil); err != nil {
						t.Fatal(err)
					}
				}
				if test.outage {
					offline.Store(true)
					time.Sleep(2 * time.Minute)
					synctest.Wait()
				} else if test.ackLost {
					var drop atomic.Bool
					drop.Store(true)
					fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response, err := originalTransport.RoundTrip(request)
						if err == nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") && drop.Swap(false) {
							response.Body.Close()
							return nil, errors.New("finished event acknowledgment lost")
						}
						return response, err
					}))
				} else {
					h.failChanges.failEvents.Store(!test.version)
					h.failChanges.failVersions.Store(test.version)
				}
				if !test.outage || test.completeDuring {
					if err := execution.Finish(guarded, "succeeded"); err == nil || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("unpublished finish = %v", err)
					}
					recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
					wantOutcome := ""
					if test.outage {
						wantOutcome = "cancelled"
					}
					if test.ackLost {
						wantOutcome = "succeeded"
					}
					if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Outcome != wantOutcome {
						t.Fatalf("unpublished finish retired attempt: %+v, %v", recovery.Attempts, err)
					}
				}
				if test.restart {
					if execution.pending == nil || execution.pending.Type != "run.finished" {
						t.Fatalf("finish transport failure did not retain terminal event: %+v", execution.pending)
					}
					pending, err := json.Marshal(execution.pending)
					if err != nil {
						t.Fatal(err)
					}
					saved := execution.CompletionState()
					stop()
					fixtureClient(h.native.Transport()).requests.Store(originalTransport)
					h.failChanges.failEvents.Store(false)
					machine := fixtureSchedulerMachine(h.scheduler)
					h.scheduler, err = newTestScheduler(h.native.Transport(), SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
						t.Fatal(err)
					}
					if got := h.leaseHolds.Load(); got != 2 {
						t.Fatalf("completion restore acquired %d fixture lease holds, want 2", got)
					}
					execution = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
					restoredPending, err := json.Marshal(execution.pending)
					if err != nil || string(restoredPending) != string(pending) || !sameCompletionLease(execution.claim.lease, originalLease) {
						t.Fatalf("restoration changed pending finish or authority: want=%s got=%s error=%v", pending, restoredPending, err)
					}
					guarded = t.Context()
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
				if test.outage {
					if err := execution.Validate(guarded); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("expired execution retained authority: %v", err)
					}
					recovery, err := h.admin.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
					if err != nil || len(recovery.Attempts) != 1 || recovery.Attempts[0].Status != "cancelled" || recovery.Attempts[0].Checkpoint == nil || recovery.Attempts[0].Checkpoint.WorktreeState != "unpushed" {
						t.Fatalf("expired execution lost recovery evidence: %+v, %v", recovery.Attempts, err)
					}
					replacement, err := h.native.Claim(t.Context(), tracker.NativeClaim{PolicyID: h.descriptor.ID, WorkItemID: tracker.NativeWorkItemID(issue.ID), MachineID: originalLease.MachineID, SessionID: "outage-retry", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
					if err != nil || replacement.ID == originalLease.ID || replacement.FencingToken <= originalLease.FencingToken {
						t.Fatalf("expired execution did not return to claim eligibility: %+v, %v", replacement, err)
					}
					return
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
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "uncommitted edits", Dirty: true, WantNone: true, WantState: "In Progress"},
		{Name: "late host conflict continues to resolved publication and landing", Rework: true, Formal: true, Staged: true, LateConflict: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
	}, scenariotest.Constructors{New: NewExecution, Recovered: NewRecoveredExecution}, scenariotest.Checks{DirtyArtifacts: func(t *testing.T, execution runner.Execution) bool {
		owner, ok := execution.(*nativeExecution)
		if !ok {
			t.Fatal("fixture lost native execution ownership")
		}
		return owner.artifacts.finished
	}})
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

func (*committingAgent) SupportsLiveControl() bool { return true }

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

func TestNativeChangeGitIsolation(t *testing.T) {
	if fixture := os.Getenv("DETENT_TEST_GIT_FIXTURE"); fixture != "" {
		isolateNativeChangeGit(t)
		switch fixture {
		case "bare":
			nativeChangeSourceFixture(t.Context(), t, nativeChangeFixtureHead("a"), nativeChangeFixtureHead("b"))
		case "source":
			nativeChangeSourceRepo(t)
		}
		return
	}
	isolateNativeChangeGit(t)
	for _, fixture := range []string{"bare", "source"} {
		t.Run(fixture, func(t *testing.T) {
			enclosing := nativeChangeSourceRepo(t)
			paths := []string{".git/config", ".git/HEAD", ".git/refs/heads/main", ".git/index", "README.md"}
			before := make(map[string]string)
			for _, path := range paths {
				data, err := os.ReadFile(filepath.Join(enclosing, path))
				if err != nil {
					t.Fatal(err)
				}
				before[path] = string(data)
			}
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestNativeChangeGitIsolation$")
			cmd.Dir = enclosing
			cmd.Env = append(os.Environ(), "DETENT_TEST_GIT_FIXTURE="+fixture, "GIT_DIR="+filepath.Join(enclosing, ".git"))
			if fixture == "source" {
				cmd.Env = append(cmd.Env, "GIT_WORK_TREE="+enclosing)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("isolated fixture: %v\n%s", err, output)
			}
			for _, path := range paths {
				data, err := os.ReadFile(filepath.Join(enclosing, path))
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != before[path] {
					t.Errorf("fixture modified enclosing repository %s", path)
				}
			}
		})
	}
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

func nativeChangeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	nativeChangeGitWithContext(t.Context(), t, dir, args...)
}

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
	first := h.publish(t, item, change.ID, nativeChangeFixtureHead("b"))
	head := nativeChangeFixtureHead("c")
	second, err := h.admin.PublishChangeVersion(t.Context(), item, change.ID, tracker.PublishChangeVersion{
		Mutation:          nativeMutationKey(),
		ExpectedVersionID: first.ID,
		ChangeVersionInput: tracker.ChangeVersionInput{
			BaseSHA: nativeChangeFixtureHead("a"), HeadSHA: head, MergeBaseSHA: nativeChangeFixtureHead("a"), Repository: nativeChangeRepository,
			Code:      tracker.ChangeArtifact{Kind: "code", URI: nativeChangeRepository + "/commit/" + head, SHA256: policy.Digest([]byte(head)), Availability: "unverified"},
			Artifacts: []tracker.ChangeArtifact{}, PolicyID: h.descriptor.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reviewed, err := h.connector.ReadChangeReview(t.Context(), issue.ID, change.ID, first.ID, head); err != nil || reviewed.Reviewed {
		t.Fatalf("for a version that is no longer current ReadChangeReview = %+v, %v; want not reviewed", reviewed, err)
	}
	if reviewed, err := h.connector.ReadChangeReview(t.Context(), issue.ID, change.ID, "", head); err != nil || reviewed.Reviewed {
		t.Fatalf("for no version ReadChangeReview = %+v, %v; want not reviewed", reviewed, err)
	}
	if reviewed, err := h.connector.ReadChangeReview(t.Context(), issue.ID, change.ID, second.ID, head); err != nil || !reviewed.Reviewed {
		t.Fatalf("under the default policy ReadChangeReview = %+v, %v; want reviewed", reviewed, err)
	}
	if review, err := h.connector.ReadChangeReview(t.Context(), issue.ID, change.ID, second.ID, strings.Repeat("d", 40)); err != nil || review.Current || review.Reviewed {
		t.Fatalf("mismatched head accepted: %+v %v", review, err)
	}
	rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
	if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
		t.Fatal(err)
	}
	if reviewed, err := h.connector.ReadChangeReview(t.Context(), issue.ID, change.ID, second.ID, head); err != nil || reviewed.Reviewed || reviewed.Current {
		t.Fatalf("after the policy asks for review ReadChangeReview = %+v, %v; want stale policy", reviewed, err)
	}
	if _, err := h.connector.ReadChangeReview(t.Context(), issue.ID, "change_"+strings.Repeat("0", 32), second.ID, head); err == nil {
		t.Fatal("an unknown change read as a review state")
	}
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
	if err := h.admin.Transport().Request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "publication-fixture-ci", "scope": "operator"}, &token); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Transport().Request(t.Context(), http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
		t.Fatal(err)
	}
	client, err := newTestClient(t, Config{URL: fixtureClient(h.admin.Transport()).baseURL.String(), TokenSource: func() string { return token.Token }, HTTPClient: fixtureClient(h.admin.Transport()).httpClient})
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

func TestHumanBlockedHoldOutranksRunnerLease(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		holder string
		want   string
	}{
		{holder: "human", want: "Blocked"},
		{holder: "runner", want: "Todo"},
	} {
		t.Run(test.holder, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHubWithStates(t, "In Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Blocked", "Done", "Todo"}},
				{Name: "Blocked", Transitions: []string{"Todo", "In Progress"}},
				{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
			})
			issue := h.createInProgress(t, "held work")
			h.claim(t, issue.ID)
			if test.holder == "human" {
				current, err := h.admin.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Blocked", Reason: "user_requested", ReasonDetail: "Operator lane owns this item"}); err != nil {
					t.Fatal(err)
				}
			} else if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err != nil {
				t.Fatal(err)
			}
			err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Todo")
			if fenced := errors.Is(err, runner.ErrExecutionAuthorityUnavailable); fenced != (test.want == "Blocked") || err != nil && !fenced {
				t.Fatalf("runner release error = %v", err)
			}
			if got := h.state(t, issue.ID); got != test.want {
				t.Fatalf("state = %s, want %s", got, test.want)
			}
		})
	}
}
