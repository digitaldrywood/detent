package dispatch

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
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	h := &nativeChangeHub{review: review, organization: organizations.Items[0].ID, descriptor: clientTestPolicy()}
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
