package hubclient

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
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestMain(m *testing.M) {
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	scratch, err := os.MkdirTemp("", "detent-hubclient-test-")
	if err != nil {
		panic(err)
	}
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
		serverURL = "http://native-hub.test"
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
	h.otherWorker = func(t *testing.T) *NativeClient {
		t.Helper()
		var other struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := admin.request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": "native-change-other-worker", "scope": "worker"}, &other); err != nil {
			t.Fatal(err)
		}
		if err := admin.request(t.Context(), http.MethodPost, "/api/v2/tokens/"+other.ID+"/grants", map[string]any{"organization_id": h.organization, "project_id": h.project}, nil); err != nil {
			t.Fatal(err)
		}
		client, err := New(Config{URL: serverURL, TokenSource: func() string { return other.Token }, HTTPClient: httpClient})
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
		LeaseHold: h.leaseHold,
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
		directory = nativeChangeTempDir(t)
		if output, err := exec.CommandContext(ctx, "git", "-C", directory, "init", "--bare", "--object-format=sha1", "--template=").CombinedOutput(); err != nil {
			t.Fatalf("initialize fixture repository: %v\n%s", err, output)
		}
		for _, object := range []struct{ kind, content string }{
			{"tree", ""},
			{"commit", nativeChangeFixtureCommit("a")},
			{"commit", nativeChangeFixtureCommit("b")},
			{"commit", nativeChangeFixtureCommit("c")},
			{"commit", nativeChangeFixtureCommit("d")},
			{"commit", nativeChangeFixtureCommit("e")},
			{"commit", nativeChangeFixtureCommit("f")},
			{"commit", nativeChangeFixtureCommit("g")},
			{"commit", nativeChangeFixtureCommit("h")},
		} {
			command := exec.CommandContext(ctx, "git", "-C", directory, "hash-object", "-w", "-t", object.kind, "--stdin")
			command.Stdin = strings.NewReader(object.content)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("write fixture object: %v\n%s", err, output)
			}
		}
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
func TestNativeExecutionSettlesFinishedRun(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	base := nativeChangeFixtureHead("a")
	head := nativeChangeFixtureHead("c")
	for _, test := range []struct {
		mergeResolution         bool
		name                    string
		transitionReason        string
		role                    string
		outcome                 string
		worktree                string
		source                  runner.AttemptDiffSource
		loseLease               bool
		validationErr           error
		restart                 bool
		legacyRestart           bool
		oldCompletionFrame      bool
		restoreRefusal          string
		failCreate              bool
		retainSource            bool
		emptyRework             string
		sourceRefusal           bool
		failDiff                bool
		failDetail              bool
		failVersion             bool
		dropVersion             bool
		versionCode             string
		diffCode                string
		conversation            bool
		land                    bool
		integrate               bool
		integrateHead           bool
		integrateChanged        bool
		integrateAudit          bool
		integrationIncorporated bool
		integrationConflict     bool
		landingRestart          bool
		reviewDispositions      bool
		finalMessage            string
		disposition             *tracker.NativeDisposition
		releasedBlocked         bool
		unreadFinal             bool
		refreshFinal            bool
		checkpointHead          string
		staleFinalDiff          bool
		existing                bool
		published               string
		repolicy                bool
		noRemote                bool
		reviewer                bool
		requiredPR              bool
		checkPRBeforeFinish     bool
		lostPRResponse          bool
		lostPush                string
		publicationWriteRefusal string
		publicationFenceLoss    int
		publicationFailure      string
		publicationStaged       bool
		publicationRestore      bool
		publicationUncertain    bool
		publicationReference    string
		publicationRefused      bool
		publicationLanded       bool
		publicationHistory      bool
		publicationRecovery     bool
		resumedPush             bool
		recoveredPushFault      string
		wantChange              *runner.NativeChange
		wantDiagnostic          string
		wantChanges             int
		// wantVersions is the versions the item's change carries after the
		// finish; the last one is current and carries the run's head.
		wantVersions int
	}{
		{name: "NativeChange final checkpoint replaces a stored precommit diff", staleFinalDiff: true, role: runner.RoleCode, outcome: "succeeded", worktree: "clean", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "NativeChange no commit Rework settles the current final head", staleFinalDiff: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "clean", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "NativeChange changed integration head publishes a new version", land: true, integrate: true, integrateChanged: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "NativeChange audit enabled changed integration head publishes a new version", land: true, integrate: true, integrateChanged: true, integrateAudit: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "NativeChange audit enabled published integration head retains its audit", land: true, integrate: true, integrateChanged: true, integrateHead: true, integrateAudit: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "NativeChange genuine integration conflict retains Rework evidence", retainSource: true, land: true, integrate: true, integrationConflict: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "NativeChange audited integration conflict retains corrective landing", retainSource: true, integrateAudit: true, land: true, integrate: true, integrationConflict: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "NativeChange same published head keeps reviewed receipts after incorporation refusal", land: true, integrate: true, integrateHead: true, integrationConflict: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "MergeResolution publishes changed source for independent review", mergeResolution: true, existing: true, published: nativeChangeFixtureHead("d"), reviewer: true, retainSource: true, role: runner.RoleMerge, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{MergeResolution: true, Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 2},
		{name: "fixing Rework binds recorded review dispositions to its published version", reviewDispositions: true, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1, disposition: &tracker.NativeDisposition{Status: "complete"}, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nreview_dispositions: [{thread_id: T1, disposition: fixed, evidence: regression passes}]\nblockers: []\nhuman_action: null\n```"},
		{name: "required PR reopens landed history with a new PR", requiredPR: true, publicationLanded: true, publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", wantChanges: 1, wantVersions: 3},
		{name: "required PR recovers historical confirmed push after stale forge read", requiredPR: true, publicationRecovery: true, publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR recovers historical push recorded by a later resume attempt", requiredPR: true, publicationRecovery: true, resumedPush: true, publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR refuses historical push without checkpoint", requiredPR: true, publicationRecovery: true, recoveredPushFault: "missing", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR refuses historical push with tampered digest", requiredPR: true, publicationRecovery: true, recoveredPushFault: "digest", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR refuses historical push for different source", requiredPR: true, publicationRecovery: true, recoveredPushFault: "source", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR refuses historical push for different policy", requiredPR: true, publicationRecovery: true, recoveredPushFault: "policy", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR refuses historical push for different branch", requiredPR: true, publicationRecovery: true, recoveredPushFault: "branch", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR refuses historical push for different base", requiredPR: true, publicationRecovery: true, recoveredPushFault: "base", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR refuses historical proof for unknown remote head", requiredPR: true, publicationRecovery: true, recoveredPushFault: "remote", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed"},
		{name: "required PR retains its publication receipt on historical versions", requiredPR: true, publicationHistory: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChanges: 1, wantVersions: 1},
		{name: "required PR adopts a retained branch under human hold", requiredPR: true, publicationReference: "current", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", wantChanges: 1, wantVersions: 1},
		{name: "required PR reuses the previous version PR after recommit", requiredPR: true, publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", wantChanges: 1, wantVersions: 2},
		{name: "required PR reuses latest published head after refused landing", requiredPR: true, publicationReference: "previous", publicationRefused: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", wantChanges: 1, wantVersions: 3},
		{name: "required PR refuses an unknown retained branch head", requiredPR: true, publicationReference: "unknown", publicationFailure: "identity", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", wantChanges: 1, wantVersions: 2},
		{name: "required PR refuses unrelated ambiguous external effect", requiredPR: true, publicationUncertain: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "required PR no edit Rework preserves human hold", requiredPR: true, checkPRBeforeFinish: true, reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "required PR reconciles a lost push already on the forge without another push or attempt", requiredPR: true, lostPush: "present", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "required PR retries a lost push absent from the forge", requiredPR: true, lostPush: "absent", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "required PR retries a lost push over its recorded head", requiredPR: true, lostPush: "absent", publicationReference: "previous", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", wantChanges: 1, wantVersions: 2},
		{name: "required PR finishes a lost push over a diverged branch as head moved", requiredPR: true, lostPush: "diverged", reviewer: true, existing: true, published: head, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
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
		{name: "old completion frame restores successful deferred publication", restart: true, oldCompletionFrame: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart refuses released authority", restart: true, restoreRefusal: "released", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart refuses another machine", restart: true, restoreRefusal: "machine", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart retains a lease across configuration changes", restart: true, restoreRefusal: "policy", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "restart refuses stale fencing", restart: true, restoreRefusal: "fence", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "existing valid different fence cannot inherit deferred completion", restart: true, restoreRefusal: "existing", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "existing same lease renewal preserves deferred completion", restart: true, restoreRefusal: "renewed", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), failCreate: true, wantDiagnostic: "change creation unavailable"},
		{name: "reviewed source lands with its coding lease in the four lane workflow", land: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "reviewed source integrates during finalization", land: true, integrate: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "NativeCompletion no-change reviewed head already delivered", land: true, integrate: true, integrateHead: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "prepared completion restart allows inline landing runtime", landingRestart: true, land: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", checkpointHead: head, source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "validator rework reason survives history and explanation", transitionReason: "validator_rework", wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}, role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "Acceptance criterion is unverified.\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "complete", FinalSummary: "Acceptance criterion is unverified."}},
		{name: "blocked reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "Source conflicts remain unresolved.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: merge_conflict\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "merge_conflict", FinalSummary: "Source conflicts remain unresolved."}},
		{name: "human action reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "The operator must approve the migration.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: permission_wait\nblockers: []\nhuman_action: Approve the migration\nblock_kind: missing_authorization\noptions:\n  - answer: Approve the migration\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait", HumanAction: true, FinalSummary: "The operator must approve the migration."}},
		{name: "instance limitation reason and summary survive attempts API", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: instance_limitation\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation", FinalSummary: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403."}},
		{name: "unfinished clean source retains normalized disposition", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "in_progress"}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "unfinished dirty source retains normalized disposition without publication", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "in_progress"}},
		{name: "human action retains normalized disposition", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: Approve the rollout\nblock_kind: missing_authorization\noptions:\n  - answer: Approve the rollout\n```", disposition: &tracker.NativeDisposition{Status: "in_progress", HumanAction: true}, wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "native272 instance report retains typed evidence", releasedBlocked: true, role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused listener with EPERM\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:worker-loopback", Owner: workpad.BlockerOwnerInstance, Reason: "sandbox refused listener with EPERM", Unverifiable: true}}}},
		{name: "dirty instance report retains typed evidence without publication", role: runner.RoleCode, outcome: "succeeded", worktree: "dirty", finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused listener with EPERM\nhuman_action: null\n```", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:worker-loopback", Owner: workpad.BlockerOwnerInstance, Reason: "sandbox refused listener with EPERM", Unverifiable: true}}}},
		{name: "native273 malformed predicate retains rejection", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - reason: browser unavailable\n    predicate: instance_available\nhuman_action: null\n```", wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "invalid report preserves legacy receipt", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), finalMessage: "```detent-status\nschema: 99\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", wantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{name: "commits open a change", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "rework reuses the item's change", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "rework publishes a new head as the next version", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: nativeChangeFixtureHead("b"),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 2},
		{name: "rework keeps the version that already carries its head", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "clean preserved current head retains its deliverable with an empty attempt diff", role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: func(context.Context) (tracker.AttemptDiffRequest, bool) {
			return tracker.AttemptDiffRequest{BaseSHA: head, HeadSHA: head, Files: []tracker.AttemptDiffFile{}}, true
		}, existing: true, published: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head}, wantChanges: 1, wantVersions: 1},
		{name: "verified empty Rework publishes cancellation for independent review", emptyRework: "verified", retainSource: true, reviewer: true, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head), existing: true, published: nativeChangeFixtureHead("b"),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head}, wantChanges: 1, wantVersions: 2},
		{name: "unverified empty Rework preserves the mismatch", emptyRework: "unverified", retainSource: true, role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head), existing: true, published: nativeChangeFixtureHead("b"),
			wantDiagnostic: "the final attempt diff does not identify the current Change Request head", wantChanges: 1, wantVersions: 1},
		{name: "empty rework without recovered source retains the mismatch", role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head), existing: true, published: nativeChangeFixtureHead("b"), retainSource: true,
			wantDiagnostic: "the final attempt diff does not identify the current Change Request head", wantChanges: 1, wantVersions: 1},
		{name: "empty recovered rework without source capture retains the mismatch", emptyRework: "verified", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head), existing: true, published: nativeChangeFixtureHead("b"),
			wantDiagnostic: "the final attempt diff does not identify the current Change Request head", wantChanges: 1, wantVersions: 1},
		{name: "empty unrelated head retains the mismatch for review", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(base), existing: true, published: head,
			wantDiagnostic: "the final attempt diff does not identify the current Change Request head", wantChanges: 1, wantVersions: 1},
		{name: "removed clean worktree reuses its matching final checkpoint diff", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, unreadFinal: true, checkpointHead: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "final checkpoint refreshes the diff after a late commit before cleanup", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(nativeChangeFixtureHead("b"), "README.md"), refreshFinal: true, unreadFinal: true, checkpointHead: head,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "final checkpoint advances past the stored diff before cleanup", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(nativeChangeFixtureHead("b"), "README.md"), refreshFinal: true, unreadFinal: true, checkpointHead: nativeChangeFixtureHead("b"),
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "NativeCompletion differing final head already integrated with a nonempty diff", land: true, integrate: true, integrateChanged: true, integrationIncorporated: true, role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{name: "an unavailable final diff cannot reuse an older checkpoint head", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, unreadFinal: true, checkpointHead: nativeChangeFixtureHead("b"),
			wantDiagnostic: "checkpoint head", wantChanges: 1, wantVersions: 1},
		{name: "rework preserves the same head after configuration changes", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, repolicy: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 1},
		{name: "changed review requirements republish the same source", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: head, repolicy: true, reviewer: true,
			wantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, wantChanges: 1, wantVersions: 2},
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
		{name: "a refused new version cannot succeed", role: runner.RoleRework, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), existing: true, published: nativeChangeFixtureHead("b"), failVersion: true,
			wantDiagnostic: "version publication unavailable", wantChanges: 1, wantVersions: 1},
		{name: "a lost lease decides nothing", role: runner.RoleCode, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md"), loseLease: true},
		{name: "a failed run decides nothing", role: runner.RoleCode, outcome: "failed", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "a plan run decides nothing", role: runner.RolePlan, outcome: "succeeded", worktree: "unpushed", source: nativeChangeDiff(head, "README.md")},
		{name: "no readable worktree cannot succeed", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", wantDiagnostic: "diff source is unavailable"},
		{name: "version acknowledgment loss retains the current immutable version", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: nativeChangeFixtureHead("b"), dropVersion: true, wantDiagnostic: "version acknowledgment lost", wantChanges: 1, wantVersions: 2},
		{name: "version allowance refusal retains the previous immutable version", role: runner.RoleRework, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), existing: true, published: nativeChangeFixtureHead("b"), versionCode: "allowance_exhausted", wantDiagnostic: "allowance_exhausted", wantChanges: 1, wantVersions: 1},
		{name: "diff allowance refusal is a publication diagnostic", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), diffCode: "allowance_exhausted", wantDiagnostic: "allowance_exhausted"},
		{name: "fenced diff refusal retains lost authority", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", source: nativeChangeDiff(head, "README.md"), diffCode: "stale_fencing_token", wantDiagnostic: "stale_fencing_token"},
		{name: "conversation completion remains conversation owned", role: runner.RoleCode, outcome: "succeeded", worktree: "clean", conversation: true, finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var h *nativeChangeHub
			if test.releasedBlocked {
				h = newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
					{Name: "In Progress", Transitions: []string{"Todo", "In Review", "Done"}},
					{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
					{Name: "Done", Terminal: true},
				}, true)
			} else if test.land {
				h = newNativeChangeHubTransport(t, "Human Review", []tracker.NativeState{
					{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
					{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
					{Name: "Human Review", Transitions: []string{"In Progress", "Done"}},
					{Name: "Done", Terminal: true},
				}, true)
			} else {
				h = newNativeChangeHub(t, true)
			}
			if test.integrateAudit {
				previous := h.descriptor.ID
				h.descriptor.Gates.SecurityAudit = true
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
					t.Fatal(err)
				}
			}
			issue := h.createInProgress(t, "Native change")
			if test.releasedBlocked {
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
			if test.publicationLanded {
				publicationNumber = 8
			}
			var original tracker.ChangeDetail
			var ci *NativeClient
			var publicationOwner *nativeExecution
			var publicationPrevious string
			var publicationEarlier string
			var refusedPublication tracker.ChangeVersion
			var advancePublication func()
			var publicationHead func() string
			publicationCleared := false
			var replacePublicationHead func()
			var publicationCapture func(context.Context, string, string) (tracker.ChangeSourceCapture, error)
			stalePublication := test.publicationRecovery
			publicationUnavailable := test.resumedPush
			if test.requiredPR {
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
				if test.publicationLanded {
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
				if test.publicationReference != "" {
					publicationBranch = "manual/recovery"
					publicationPrevious = head
					if test.publicationReference != "current" {
						treeRef := "HEAD^{tree}"
						if test.publicationLanded {
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
					if test.publicationReference == "unknown" {
						publishedHead = base
					}
					nativeChangeGit(t, info.Path, "push", "origin", publishedHead+":refs/heads/"+publicationBranch)
					if test.publicationLanded {
						publicationBranch = info.Branch
					}
				}
				test.published, test.checkpointHead = head, head
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
				test.wantChange = &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}
				test.source = func(context.Context) (tracker.AttemptDiffRequest, bool) {
					return tracker.AttemptDiffRequest{BaseSHA: base, HeadSHA: head, Files: []tracker.AttemptDiffFile{{Path: "README.md", Status: "modified"}}}, true
				}
				client, err := github.NewClient(github.ClientConfig{TokenSource: github.StaticTokenSource(t.Name()), DisableConditionalRequests: true, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if publicationUnavailable {
						return nil, errors.New("forge temporarily unavailable")
					}
					pullHead := head
					if test.publicationReference != "" && !test.publicationLanded {
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
						if test.publicationLanded && strings.HasSuffix(request.URL.Path, "/pulls/7") {
							response = fmt.Sprintf(`{"number":7,"state":"closed","merged":true,"head":{"sha":%q,"ref":"manual/recovery","repo":{"full_name":"example/native-change"}},"base":{"ref":"main","repo":{"full_name":"example/native-change"}}}`, publicationPrevious)
							break
						}
						if test.publicationLanded && strings.HasSuffix(request.URL.Path, "/pulls") && request.URL.Query().Get("head") != "example:"+publicationBranch {
							t.Fatal("reopened publication queried the historical branch")
						}
						if publicationCalls > 0 || test.publicationReference != "" && !test.publicationLanded {
							response = "[" + pull + "]"
							if strings.HasSuffix(request.URL.Path, fmt.Sprintf("/pulls/%d", publicationNumber)) {
								response = pull
							}
						}
					case http.MethodPost:
						publicationCalls++
						if test.publicationLanded {
							var input struct {
								Head string `json:"head"`
								Base string `json:"base"`
							}
							if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.Head != "example:"+publicationBranch || input.Base != "main" {
								t.Fatalf("reopened PR used the wrong publication identity: %+v, %v", input, err)
							}
						}
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
				pushLost := false
				publicationSource = func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
					if test.lostPush != "" {
						record := opts.PublicationEffect
						opts.PublicationEffect = func(ctx context.Context, kind, state string, publication workspace.GitHubPublication) error {
							if kind == "git_push" && state == "pending" {
								publicationPushes++
							}
							if opts.UnresolvedEffect && opts.ExpectedPublicationHead == "" && state == "pending" && test.lostPush == "absent" && !publicationCleared {
								t.Fatal("publication retried before recording the absent external effect")
							}
							if err := record(ctx, kind, state, publication); err != nil {
								return err
							}
							publicationCleared = publicationCleared || state == "none"
							return nil
						}
					}
					if test.lostPush != "" && !pushLost {
						record := opts.PublicationEffect
						opts.PublicationEffect = func(ctx context.Context, kind, state string, publication workspace.GitHubPublication) error {
							if kind != "git_push" || state != "pending" || pushLost {
								return record(ctx, kind, state, publication)
							}
							pushLost = true
							if err := record(ctx, kind, state, publication); err != nil {
								return err
							}
							if test.lostPush == "present" {
								nativeChangeGitWithContext(ctx, t, info.Path, "push", "origin", version.HeadSHA+":refs/heads/"+info.Branch)
							}
							return errors.Join(errors.New("git push response lost before the remote recorded it"), record(ctx, kind, "ambiguous", publication))
						}
					}
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
					if test.publicationReference != "" && !test.publicationLanded || test.publicationUncertain || test.lostPush == "diverged" || test.publicationFailure == "push" || test.publicationFenceLoss > 0 && test.publicationFenceLoss < 4 {
						wantCalls = 0
					}
					if readHead(info.Path) != head || readHead(source) != base || publicationCalls != wantCalls {
						t.Errorf("publication changed source, merged or duplicated a PR: calls=%d", publicationCalls)
					}
					if test.publicationLanded {
						output, err := exec.CommandContext(context.WithoutCancel(t.Context()), "git", "-C", remote, "rev-parse", "refs/heads/manual/recovery").Output()
						if err != nil || strings.TrimSpace(string(output)) != publicationPrevious {
							t.Errorf("reopened publication changed the historical PR branch: %s, %v", output, err)
						}
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
			if test.mergeResolution {
				rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
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
				passPublicationCheck := func(version tracker.ChangeVersion) {
					t.Helper()
					check := version.Checks[0]
					if _, err := ci.SubmitChangeCheck(t.Context(), item, earlier.ID, version.ID, tracker.SubmitChangeCheck{Mutation: nativeMutationKey(), ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: version.HeadSHA, RunID: version.RunID, PolicyID: version.PolicyID, ConfigDigest: version.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: version.CreatedAt, Evidence: []tracker.ChangeArtifact{{Kind: "test", URI: "s3://fixture/test", SHA256: policy.Digest([]byte("test receipt")), Availability: "available"}}}}); err != nil {
						t.Fatal(err)
					}
				}
				if test.published != "" {
					var previous string
					var external *tracker.ChangeExternalReference
					if test.publicationReference != "" {
						external = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: nativeChangeRepository + "/pull/7"}
						if test.publicationReference != "current" {
							if test.publicationLanded {
								older := h.publishCapturedExternal(t, item, earlier.ID, publicationEarlier, nil, external, "", base)
								previous = older.ID
							}
							if test.publicationRefused {
								older := h.publishCapturedExternal(t, item, earlier.ID, nativeChangeFixtureHead("d"), nil, external, "", base)
								previous = older.ID
							}
							old := h.publishCapturedExternal(t, item, earlier.ID, publicationPrevious, nil, external, previous, base)
							if test.publicationLanded {
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
					if !test.publicationRecovery {
						version = h.publishCapturedExternal(t, item, earlier.ID, test.published, nil, external, previous, base)
					}
					if test.requiredPR {
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
				if test.repolicy {
					h.repolicy(t)
				}
			}
			if test.reviewer && !test.requiredPR && !test.mergeResolution {
				rules := tracker.ChangeReviewPolicy{PolicyID: h.descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), Policy: rules}); err != nil {
					t.Fatal(err)
				}
			}
			if test.mergeResolution {
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
			if test.releasedBlocked {
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
					t.Fatal(err)
				}
			}
			execution := h.scheduler.RunExecution(issue.ID)
			publicationOwner = execution.(*nativeExecution)
			if test.publicationRecovery {
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
			if test.source != nil {
				execution.(runner.DiffExecution).SetDiffSource(test.source)
			}
			if test.mergeResolution || test.emptyRework != "" {
				if _, err := execution.(runner.ChangeSourceExecution).RecoverChangeSource(guarded); err != nil {
					t.Fatal(err)
				}
			}
			var retainedCapture tracker.ChangeSourceCapture
			if test.retainSource {
				retainedCapture = nativeChangeSourceFixture(t.Context(), t, base, head)
				if test.emptyRework == "verified" {
					retainedCapture.Source.DiffSHA256 = tracker.ChangeSourceDigest(nil)
				}

				execution.(runner.ChangeSourceExecution).SetChangeSource(func(_ context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
					return retainedCapture, nil
				})
			}
			if !test.noRemote {
				execution.(runner.RepositoryExecution).SetRepository(nativeChangeRepository)
			}
			if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: test.role, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			if test.integrateAudit {
				if err := execution.(runner.RuntimeExecution).ObserveRuntime(guarded, tracker.NativeRuntimeObservation{Phase: "implementation", Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 41}}); err != nil {
					t.Fatal(err)
				}
			}
			if test.mergeResolution {
				if err := execution.(runner.MergeResolutionExecution).PrepareMergeResolution(); err != nil {
					t.Fatal(err)
				}
			}
			if test.mergeResolution {
				owner := execution.(*nativeExecution)
				previous := owner.recoveredSource
				refusal := runner.NativeLanding{ChangeID: previous.ChangeID, VersionID: previous.VersionID, HeadSHA: previous.HeadSHA, RefusalKind: workspace.LandRefusalConflict, Refusal: "conflicting files: feature.txt", ConflictFiles: []string{"feature.txt"}, Path: "merge_resolution_pending"}
				if err := owner.ObserveLanding(guarded, refusal); err != nil {
					t.Fatal(err)
				}
				if err := owner.append(guarded, "run.observed", "", nil, true); err != nil {
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

			if test.publicationRefused {
				if err := execution.(runner.LandingRuntimeExecution).ObserveLanding(guarded, runner.NativeLanding{ChangeID: refusedPublication.ChangeID, VersionID: refusedPublication.ID, HeadSHA: refusedPublication.HeadSHA, RefusalKind: workspace.LandRefusalProtected}); err != nil {
					t.Fatal(err)
				}
				if err := publicationOwner.append(guarded, "run.observed", "", nil, true); err != nil {
					t.Fatal(err)
				}
				original, err = h.admin.Change(t.Context(), item, refusedPublication.ChangeID)
				if err != nil || original.Versions[1].Landing == nil || original.Versions[1].Landing.Landed {
					t.Fatalf("prior publication refusal was not retained: %v", err)
				}
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "local_only", Availability: "unverified", WorktreeState: test.worktree, HeadSHA: test.checkpointHead, ExternalEffect: "none", EffectState: "none"}
			if test.publicationUncertain {
				checkpoint.ExternalEffect, checkpoint.EffectState, checkpoint.EffectID = "provider_turn", "ambiguous", "effect_"+strings.Repeat("a", 32)
			}
			if err := execution.Checkpoint(guarded, checkpoint); err != nil {
				t.Fatal(err)
			}
			if test.refreshFinal {
				execution.(runner.DiffExecution).SetDiffSource(nativeChangeDiff(head, "README.md"))
				checkpoint.HeadSHA = head
				if err := execution.Checkpoint(guarded, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			if test.publicationRecovery {
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
				startPublication := func() *nativeExecution {
					candidates := h.candidatesIn(t, h.state(t, issue.ID))
					if len(candidates) != 1 {
						t.Fatalf("publication recovery candidates=%d", len(candidates))
					}
					if _, err := h.scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
						t.Fatal(err)
					}
					resumed := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
					resumed.SetPublicationSource(publicationSource)
					resumed.SetDiffSource(test.source)
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
				owner := execution.(*nativeExecution)
				if err := owner.PrepareFinish(guarded, "succeeded", "", nil); err != nil {
					t.Fatal(err)
				}
				sourceAttempt := owner.data.AttemptID
				if test.resumedPush {
					if owner.change == nil || owner.change.VersionID == "" || owner.publication != nil || owner.data.Handoff == nil || owner.data.Handoff.ExternalEffect != "none" || publicationHead() != publicationPrevious {
						t.Fatalf("source attempt did not retain an unpushed version: change=%+v checkpoint=%+v", owner.change, owner.data.Handoff)
					}
					sourceVersion := owner.change.VersionID
					if err := h.scheduler.ReleaseClaim(guarded, issue.ID, "failed"); err != nil {
						t.Fatal(err)
					}
					reopenPublication()
					publicationUnavailable = false
					owner = startPublication()
					if err := owner.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
						t.Fatal(err)
					}
					if owner.data.AttemptID == sourceAttempt || owner.change == nil || owner.change.VersionID != sourceVersion {
						t.Fatalf("resume did not retain distinct source and recording attempt provenance: source=%s owner=%+v", sourceAttempt, owner.data)
					}
				}
				if owner.publication != nil || owner.change == nil || !strings.Contains(owner.change.VersionError, "identity mismatch") || owner.data.Handoff == nil || owner.data.Handoff.ExternalEffect != "git_push" || owner.data.Handoff.EffectState != "confirmed" {
					t.Fatalf("stale GET did not retain confirmed push without PR receipt: change=%+v checkpoint=%+v", owner.change, owner.data.Handoff)
				}
				pushedVersion, pushedAttempt := owner.change.VersionID, owner.data.AttemptID
				pushedHead := head
				if publicationHead() != pushedHead {
					t.Fatal("stale forge observation did not follow a successful real push")
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "failed"); err != nil {
					t.Fatal(err)
				}
				if test.resumedPush {
					detail, err := h.admin.Change(t.Context(), item, owner.change.ChangeID)
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
				// The new attempt runs under a newly approved policy. The old push
				// must still be checked against its original immutable policy.
				h.repolicy(t)
				rules, err := h.admin.ChangeReviewPolicy(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				rules.PolicyID = h.descriptor.ID
				if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), ExpectedID: rules.ID, Policy: rules}); err != nil {
					t.Fatal(err)
				}
				// A later version and attempt displace the push owner from the
				// compact recovery read. Its exact scoped history remains available.
				publicationUnavailable = true
				intervening := startPublication()
				if err := intervening.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil || intervening.change == nil || intervening.change.VersionID == "" || intervening.change.VersionID == pushedVersion || intervening.publication != nil {
					t.Fatalf("could not retain later unpublished version: change=%+v error=%v", intervening.change, err)
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
				transport := h.native.client.httpClient.Transport
				historicalReads := 0
				retainedHead := pushedHead
				if test.recoveredPushFault == "remote" {
					replacePublicationHead()
					retainedHead = base
				}
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					response, err := transport.RoundTrip(request)
					if err != nil || request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/changes/"+owner.change.ChangeID) {
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
						switch test.recoveredPushFault {
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
							binding := tracker.NativePRPublication{ChangeID: owner.change.ChangeID, VersionID: pushedVersion, Repository: nativeChangeRepository, HeadSHA: pushedHead, PolicyID: owner.data.PolicyID, BaseRef: "main", Branch: "manual/recovery", SourceVersion: *owner.data.Handoff.Change, SourceAttemptID: pushedAttempt}
							if test.recoveredPushFault == "base" {
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
				})
				if err := resumed.PrepareFinish(t.Context(), "succeeded", "", nil); err != nil {
					t.Fatal(err)
				}
				if historicalReads == 0 {
					t.Fatal("publication did not read the retained historical push authority")
				}
				if test.recoveredPushFault != "" {
					if resumed.publication != nil || resumed.change.VersionError == "" || publicationHead() != retainedHead || publicationCalls != 0 {
						t.Fatalf("invalid historical push changed the PR: fault=%s change=%+v publication=%+v", test.recoveredPushFault, resumed.change, resumed.publication)
					}
					return
				}
				if resumed.publication == nil || resumed.publication.External.ID != "7" || resumed.publication.HeadSHA != head || publicationCalls != 0 || publicationHead() != head {
					t.Fatalf("confirmed historical push could not advance the retained PR: source_attempt=%s recording_attempt=%s recording_attempt_reads=%d previous=%s/%s change=%+v publication=%+v creates=%d", sourceAttempt, pushedAttempt, historicalReads, pushedVersion, pushedHead, resumed.change, resumed.publication, publicationCalls)
				}
				if err := resumed.Finish(t.Context(), "succeeded"); err != nil {
					t.Fatal(err)
				}
				detail, err := h.admin.Change(t.Context(), item, resumed.change.ChangeID)
				if err != nil || len(detail.Versions) != 3 || detail.Versions[1].ID != pushedVersion || detail.Versions[1].External != nil || detail.Versions[2].External == nil || detail.Versions[2].External.ID != "7" || detail.Versions[2].HeadSHA != head || detail.Change.CurrentLanding() != nil || resumed.change.Reviewed {
					t.Fatalf("recovery did not persist only the current PR binding while retaining review gates: detail=%+v error=%v", detail, err)
				}
				return
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
				execution.(*nativeExecution).claim.lease.RunDetailBatching = false
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
				h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: h.scheduler.machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
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
				if err := owner.PrepareFinish(guarded, test.outcome, "", nil); err != nil || owner.publication != nil || owner.data.Outcome != "" || owner.data.Handoff.ExternalEffect != "provider_turn" || owner.data.Handoff.EffectState != "ambiguous" || publicationCalls != 0 || owner.change.Error != "" || owner.change.VersionCode != workspace.LandRefusalHeadMoved || !strings.Contains(owner.change.VersionError, "unresolved provider_turn effect") {
					t.Fatalf("unrelated effect ambiguity was not a recorded head_moved refusal: %+v, %+v, %v", owner.data, owner.change, err)
				}
				return
			}
			if test.landingRestart {
				before := execution.(*nativeExecution)
				if err := before.PrepareFinish(guarded, test.outcome, test.finalMessage, nil); err != nil || before.change == nil || !before.settled || before.data.Outcome != "" {
					t.Fatalf("inline landing was not deferred before Finish: change=%+v outcome=%q error=%v", before.change, before.data.Outcome, err)
				}
				saved := before.CompletionState()
				machine := h.scheduler.machine
				h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, candidate, saved); err != nil {
					t.Fatal(err)
				}
				execution = h.scheduler.RunExecution(issue.ID)
				guarded = t.Context()
				after := execution.(*nativeExecution)
				if after.data.RunID != before.data.RunID || after.data.AttemptID != before.data.AttemptID || !sameCompletionLease(after.claim.lease, before.claim.lease) {
					t.Fatal("inline landing restart changed run authority")
				}
				if err := after.PrepareFinish(guarded, test.outcome, test.finalMessage, nil); err != nil {
					t.Fatal(err)
				}
				if err := execution.(runner.LandingRuntimeExecution).StartLanding(guarded, 42, 7); err != nil {
					t.Fatalf("restored inline landing runtime was refused: pending=%+v error=%v", after.pending, err)
				}
				if after.pending != nil || after.data.Runtime == nil || after.data.Runtime.Phase != "merging" || after.data.Finalization != nil || after.data.Disposition != nil || after.data.TerminalFailure != nil {
					t.Fatalf("inline landing did not record live runtime without terminal fields: %+v pending=%+v", after.data, after.pending)
				}
			}
			finish := execution.Finish
			if test.checkPRBeforeFinish {
				owner := execution.(*nativeExecution)
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
				if err != nil || slices.ContainsFunc(refs, func(ref NativePullRequestReference) bool { return ref.Number > 0 }) {
					t.Fatalf("unconfirmed publication acquired linkage: %+v error=%v", refs, err)
				}
				if err := owner.PrepareFinish(guarded, test.outcome, "", nil); err != nil {
					t.Fatal(err)
				}
				if owner.publication == nil || owner.data.Finalization != nil || owner.data.Outcome != "" {
					t.Fatalf("fixture must confirm real publication before attempt completion: publication=%+v change=%+v data=%+v", owner.publication, owner.change, owner.data)
				}
				detail, err := h.admin.Change(t.Context(), item, owner.change.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, version := range detail.Versions {
					if version.ID == owner.change.VersionID {
						found = true
						if version.External == nil || *version.External != owner.publication.External {
							t.Fatalf("confirmed publication is missing from current version before acceptance: external=%+v expected=%+v summary=%+v", version.External, owner.publication.External, detail.Summary)
						}
					}
				}
				if !found || detail.Change.CurrentVersion != owner.change.VersionID {
					t.Fatal("confirmed publication's current native version is missing")
				}
				result := read(operatortool.GetChange, operatortool.ChangeArguments{ProjectID: string(h.project), ItemID: string(item), ChangeID: owner.change.ChangeID})
				if !strings.Contains(result.Content, owner.publication.External.URL) {
					t.Fatalf("worker read lost confirmed PR before completion: %s", result.Content)
				}
				refs, err = h.native.PullRequestReferences(guarded, item)
				if err != nil || !slices.ContainsFunc(refs, func(ref NativePullRequestReference) bool {
					return ref.Number == 7 && ref.URL == owner.publication.External.URL
				}) {
					t.Fatalf("cached PR references lost confirmed publication: %+v error=%v", refs, err)
				}
			}
			if test.publicationLanded {
				owner := execution.(*nativeExecution)
				if err := owner.PrepareFinish(guarded, test.outcome, "", nil); err != nil || owner.publication == nil {
					t.Fatalf("reopened publication did not produce a new PR: %+v, %v", owner.change, err)
				}
				receipt := *owner.publication
				if receipt.External.ID != "8" || receipt.External.URL != nativeChangeRepository+"/pull/8" || receipt.Repository != nativeChangeRepository || receipt.BaseRef != "main" || receipt.HeadSHA != head || publicationHead() != head {
					t.Fatalf("reopened publication lost its exact new identity: %+v", receipt)
				}
				owner.publication = nil
				if err := owner.preparePRPublication(guarded, owner.change); err != nil || owner.publication == nil || *owner.publication != receipt || publicationCalls != 1 {
					t.Fatalf("reopened publication replay duplicated or changed the new PR: %+v, %v, creates=%d", owner.publication, err, publicationCalls)
				}
			}
			if test.lostPush != "" {
				owner := execution.(*nativeExecution)
				if err := owner.PrepareFinish(guarded, test.outcome, "", nil); err != nil || owner.publication != nil || owner.change.Error == "" || owner.data.Handoff.ExternalEffect != "git_push" || owner.data.Handoff.EffectState != "ambiguous" || owner.data.Outcome != "" {
					t.Fatalf("lost push was not retained as an ambiguous effect: %+v, %v", owner.data, err)
				}
				if test.publicationReference != "" && owner.data.Handoff.ExpectedHeadSHA != publicationPrevious {
					t.Fatalf("lost push omitted expected remote head: got %s, want %s", owner.data.Handoff.ExpectedHeadSHA, publicationPrevious)
				}
				if test.lostPush == "diverged" {
					replacePublicationHead()
					if err := owner.Finish(guarded, test.outcome); err != nil {
						t.Fatalf("diverged publication branch held the lease as unavailable: %v", err)
					}
					if !publicationCleared || owner.data.Handoff.UncertainForgeEffect() || owner.change.Error != "" || owner.change.VersionCode != workspace.LandRefusalHeadMoved || owner.publication != nil || owner.data.Outcome != "failed" || publicationHead() != base {
						t.Fatalf("diverged publication branch was not a recorded head_moved failure: change=%+v outcome=%q", owner.change, owner.data.Outcome)
					}
					return
				}
			}
			if test.lostPRResponse {
				if err := execution.(runner.CompletionExecution).PrepareFinish(guarded, test.outcome, "", nil); err != nil {
					t.Fatal(err)
				}
				owner := execution.(*nativeExecution)
				if owner.publication != nil || owner.change.Error == "" || owner.data.Handoff.EffectState != "ambiguous" || publicationCalls != 1 || owner.data.Outcome != "" {
					t.Fatalf("lost response was represented as success: %+v", owner.data)
				}
				detail, err := h.admin.Change(t.Context(), item, owner.change.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				for _, version := range detail.Versions {
					if version.ID == detail.Change.CurrentVersion && version.External != nil {
						t.Fatal("ambiguous publication acquired current-version linkage")
					}
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
					owner.SetDiffSource(nativeChangeDiff(nativeChangeFixtureHead("d"), "README.md"))
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
			if test.staleFinalDiff {
				owner := execution.(*nativeExecution)
				owner.SetDiffSource(nativeChangeDiff(base, "README.md"))
				if err := owner.postDiff(guarded, owner.data.Sequence+1); err != nil {
					t.Fatal(err)
				}
				owner.SetDiffSource(test.source)
			}
			finishErr := finish(guarded, test.outcome)
			if test.integrateAudit && finishErr == nil {
				owner := execution.(*nativeExecution)
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
			if test.staleFinalDiff {
				owner := execution.(*nativeExecution)
				var stored tracker.AttemptDiff
				path, err := nativeAttemptDiffPath(owner.data.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if err := h.native.client.request(guarded, http.MethodGet, h.native.base()+path, nil, &stored); err != nil || stored.HeadSHA != owner.worktreeHead {
					t.Fatalf("stored final diff differs from checkpoint: head=%s checkpoint=%s error=%v", stored.HeadSHA, owner.worktreeHead, err)
				}
			}
			if test.publicationFailure != "" {
				change := publicationOwner.NativeChange()
				if finishErr != nil || publicationOwner.publication != nil || change == nil || change.VersionError == "" || change.Reviewed || change.VersionID != original.Change.CurrentVersion || !strings.Contains(change.VersionError, "preserve source") {
					t.Fatalf("publication failure lost its actionable blocker or current source: %+v, %v", change, finishErr)
				}
				if (change.VersionCode == workspace.LandRefusalHeadMoved) != (test.publicationFailure == "identity") {
					t.Fatalf("publication failure code = %q for %s refusal", change.VersionCode, test.publicationFailure)
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
				if test.integrate {
					stop()
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
						t.Fatal(err)
					}
					integrationCandidate := h.claim(t, issue.ID)
					owner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
					owner.SetRepository(nativeChangeRepository)
					ctx, release, err := owner.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer release()
					checkpointHead := base
					if test.integrateHead {
						checkpointHead = head
					}
					owner.SetDiffSource(nativeChangeDiff(checkpointHead))
					if test.integrateChanged {
						if !test.integrateHead {
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
						if test.integrationConflict {
							return workspace.LandResult{}, &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, Reason: "the published source delta is not proven incorporated on the current integration base"}
						}
						if test.integrateChanged && !test.integrationIncorporated {
							return workspace.LandResult{}, &workspace.LandRefusal{Kind: workspace.LandRefusalNothing, Reason: "the current integration base does not incorporate the published source delta"}
						}
						return workspace.LandResult{MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}, nil
					})
					if err := owner.RecordUsage(ctx, tracker.NativeUsage{Provider: "openai", Model: "test", Input: 123, Output: 45}); err != nil {
						t.Fatal(err)
					}
					if err := owner.PrepareFinish(ctx, "succeeded", test.finalMessage, nil); err != nil {
						t.Fatal(err)
					}
					if err := owner.PrepareFinish(ctx, "succeeded", test.finalMessage, nil); err != nil {
						t.Fatal(err)
					}
					if test.integrateChanged && !test.integrationIncorporated {
						change := owner.NativeChange()
						wantIntegrations := 1
						if integrations != wantIntegrations || change == nil || change.VersionError != "" || change.Error != "" || change.Landing != nil || change.HeadSHA != checkpointHead || change.VersionID == "" {
							t.Fatalf("changed source did not publish: integrations=%d change=%+v", integrations, change)
						}
						wantVersions := 2
						if test.integrateHead {
							wantVersions = 1
						}
						detail, err := h.admin.Change(ctx, item, change.ChangeID)
						if err != nil || len(detail.Versions) != wantVersions || detail.Change.CurrentVersion != change.VersionID || detail.Change.CurrentLanding() != nil {
							t.Fatalf("changed source did not retain its new version: detail=%+v error=%v", detail, err)
						}
						if test.integrateAudit && !test.integrateHead && (change.Reviewed || detail.Summary.Status == "reviewed") {
							t.Fatal("new source reused the prior head's security audit")
						}
						if test.integrateHead && (!change.Reviewed || detail.Summary.Status != "reviewed") {
							t.Fatal("unchanged published source lost its existing audit")
						}
						if err := owner.Finish(ctx, "succeeded"); err != nil {
							t.Fatal(err)
						}
						if h.state(t, issue.ID) == "Done" || owner.data.Outcome != "succeeded" {
							t.Fatalf("publication did not finish for review: state=%s outcome=%s", h.state(t, issue.ID), owner.data.Outcome)
						}
						return
					}
					if test.integrationConflict {
						change := owner.NativeChange()
						if test.integrateHead {
							if integrations != 1 || change == nil || !change.Reviewed || change.VersionID == "" || change.VersionError != "" || change.Landing != nil || change.HeadSHA != head {
								t.Fatalf("same-head refusal discarded reviewed source: integrations=%d change=%+v", integrations, change)
							}
							return
						}
						if integrations != 1 || change == nil || change.VersionError != "" || change.Landing == nil || !change.Reviewed || change.VersionID == "" || change.HeadSHA != head || checkpointHead == head || owner.lastDiff.BaseSHA != owner.lastDiff.HeadSHA || len(owner.lastDiff.Files) != 0 {
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
							h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: h.scheduler.machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
							if err != nil {
								t.Fatal(err)
							}
							if _, err := h.scheduler.RestoreCompletion(ctx, orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: nativeChangeRepository}, integrationCandidate, saved); err != nil {
								t.Fatal(err)
							}
							owner = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
							if !reflect.DeepEqual(owner.NativeChange().Landing, refusal) || owner.finalizationSource == nil || owner.finalizationSource.VersionID != change.VersionID {
								t.Fatalf("completion restart lost corrective source: %+v", owner.NativeChange())
							}
						}
						if err := owner.ObserveLanding(ctx, *refusal); err != nil {
							t.Fatal(err)
						}
						if err := owner.Finish(ctx, "succeeded"); err != nil {
							t.Fatal(err)
						}
						if owner.data.Outcome != "succeeded" || owner.data.TerminalFailure != nil || h.state(t, issue.ID) == "Done" {
							t.Fatalf("source refusal became a publication failure: outcome=%s failure=%+v state=%s", owner.data.Outcome, owner.data.TerminalFailure, h.state(t, issue.ID))
						}
						if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
							t.Fatal(err)
						}
						h.claim(t, issue.ID)
						next := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
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
					if test.restart {
						stop()
						before := execution.(*nativeExecution)
						saved := before.CompletionState()
						if test.oldCompletionFrame {
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
						h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
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
						if test.restoreRefusal != "" && test.restoreRefusal != "renewed" && test.restoreRefusal != "policy" {
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
							if test.emptyRework == "verified" && test.retainSource {
								if current.Source == nil || current.Source.DiffSHA256 != tracker.ChangeSourceDigest(nil) || detail.Versions[0].HeadSHA != test.published {
									t.Fatalf("empty revision lost captured source or rewrote its predecessor: %+v", detail.Versions)
								}
								bundle, err := h.admin.ChangeSource(t.Context(), item, got.ChangeID, current.ID)
								if err != nil || !bytes.Equal(bundle, retainedCapture.Bundle) {
									t.Fatalf("empty revision lost its retained bundle: %v", err)
								}
							}
							if test.reviewDispositions {
								want := []tracker.ReviewDisposition{{HeadSHA: head, ThreadID: "T1", Disposition: "fixed", Evidence: "regression passes"}}
								if !reflect.DeepEqual(current.ReviewDispositions, want) {
									t.Fatalf("published dispositions=%+v, want %+v", current.ReviewDispositions, want)
								}
								target, err := execution.(runner.LandingExecution).LandingTarget(t.Context())
								if err != nil || !reflect.DeepEqual(target.ReviewDispositions, want) {
									t.Fatalf("landing dispositions=%+v error=%v", target.ReviewDispositions, err)
								}
							}

							if test.requiredPR && (detail.Summary.NativeReview != "changes_requested" || current.ID != got.VersionID || len(detail.Versions) != test.wantVersions) {
								t.Fatal("publication-only recovery replaced the current version or discarded its human hold")
							}
							if test.requiredPR && current.External == nil {
								t.Fatal("required PR no edit Rework retained native work but published no operator-visible GitHub PR")
							}
							runPublished := test.published != got.HeadSHA
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
				for i := range current.Versions {
					if original.Versions[i].External == nil {
						current.Versions[i].External = nil
					}
				}
				if !reflect.DeepEqual(current.Versions, original.Versions) || !reflect.DeepEqual(current.Reviews, original.Reviews) || !reflect.DeepEqual(current.Checks, original.Checks) || !reflect.DeepEqual(current.Change.Landed, original.Change.Landed) {
					t.Fatal("same-head publication changed immutable version, reviews or CI evidence")
				}
				var visible tracker.NativeIssue
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"?include=change", nil, &visible); err != nil {
					t.Fatal(err)
				}
				if visible.Change == nil || visible.Change.URL != fmt.Sprintf("%s/pull/%d", nativeChangeRepository, publicationNumber) || visible.PullRequest == nil || visible.PullRequest.Number != publicationNumber || visible.PullRequest.URL != visible.Change.URL {
					t.Fatalf("operator reads omitted the verified PR: %+v", visible)
				}
				var pulls []tracker.PullRequestView
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/pull-requests", nil, &pulls); err != nil {
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
			if again := execution.Finish(guarded, test.outcome); test.loseLease == (again == nil) {
				t.Fatalf("repeated finish error = %v", again)
			}
			checkChange(test.wantChanges, test.wantVersions)
			if test.lostPush == "present" {
				recovery, err := h.admin.Recovery(t.Context(), item)
				if err != nil || len(recovery.Attempts) != 1 || publicationPushes != 1 || publicationCalls != 1 || publicationHead() != head {
					t.Fatalf("publication replay started new delivery: attempts=%d pushes=%d PR creates=%d error=%v", len(recovery.Attempts), publicationPushes, publicationCalls, err)
				}
			}

			evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
			if err != nil || evidence.Attempt == nil {
				t.Fatalf("finalizer projection unavailable: %+v, %v", evidence, err)
			}
			if test.refreshFinal {
				var stored tracker.AttemptDiff
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+evidence.Attempt.AttemptID+"/diff", nil, &stored); err != nil {
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
			if test.releasedBlocked {
				if evidence.CurrentLease == nil || evidence.Scheduling.Outcome != "claimed" {
					t.Fatalf("workflow exclusion hid the current fenced owner: %+v", evidence.Scheduling)
				}
				if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
					t.Fatal(err)
				}
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
				if err != nil || evidence.CurrentLease != nil || evidence.Attempt == nil || evidence.Attempt.Current || evidence.Attempt.ClaimReleasedAt == nil || evidence.Attempt.Status != "succeeded" || !reflect.DeepEqual(evidence.Attempt.Disposition, test.disposition) {
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
			if test.publicationHistory {
				newVersion := h.publish(t, item, original.Change.ID, head, original.Change.CurrentVersion, base)
				detail, err := h.admin.Change(t.Context(), item, original.Change.ID)
				if err != nil || len(detail.Versions) != 2 || detail.Versions[0].External == nil || detail.Versions[0].External.ID != "7" || detail.Versions[1].External != nil || detail.Change.CurrentVersion != newVersion.ID {
					t.Fatalf("historical publication was lost or represented as current-head publication: %+v, %v", detail, err)
				}
			}
			if test.disposition != nil && test.disposition.FinalSummary != "" {
				transitionReason := test.transitionReason
				if transitionReason == "" {
					transitionReason = "worker_progress"
				}
				transitionDetail := "The completed turn requires review"
				if test.transitionReason != "" {
					transitionDetail = test.disposition.FinalSummary
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
				if test.transitionReason != "" {
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

func TestNativeExecutionResumedVersionPublication(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusal=%t", refusal), func(t *testing.T) {
			h := newNativeChangeHub(t, true)
			var otherProject tracker.NativeProject
			if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", map[string]any{"name": "another-project", "idempotency_key": "another-project", "states": []tracker.NativeState{{Name: "Todo", Dispatchable: true}}}, &otherProject); err != nil {
				t.Fatal(err)
			}
			foreign, err := h.native.client.Native(h.organization, otherProject.ID)
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
				transport := h.native.client.httpClient.Transport
				var requests []tracker.PublishChangeVersion
				reject := false
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
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
				})
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
				h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: h.scheduler.machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
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
				h.native.client.httpClient.Transport = transport
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
					h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						response, err := originalTransport.RoundTrip(request)
						if err == nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") && drop.Swap(false) {
							response.Body.Close()
							return nil, errors.New("finished event acknowledgment lost")
						}
						return response, err
					})
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
					h.native.client.httpClient.Transport = originalTransport
					h.failChanges.failEvents.Store(false)
					machine := h.scheduler.machine
					h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
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
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	isolateNativeChangeGit(t)
	sourceSeed := nativeChangeSourceRepo(t)
	useFastConversationTimings(t)
	dependencyMessage := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: prj_6d4919bebd73446798e6cd807feda10e#750\n    owner: orchestrator\n    reason: prerequisite remains Backlog\n    predicate:\n      type: issue_state\n      ref: prj_6d4919bebd73446798e6cd807feda10e#750\n      states: [Done]\nhuman_action: null\n```"
	instanceMessage := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: effective host gate lacks a passing current-head receipt\nhuman_action: null\n```"
	dependencyDisposition := &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "prj_6d4919bebd73446798e6cd807feda10e#750", Identifier: "prj_6d4919bebd73446798e6cd807feda10e#750", Owner: workpad.BlockerOwnerOrchestrator, Reason: "prerequisite remains Backlog", RecheckInterval: "tick", Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState, Ref: "prj_6d4919bebd73446798e6cd807feda10e#750", Identifier: "prj_6d4919bebd73446798e6cd807feda10e#750", States: []string{"done"}}}}}
	instanceDisposition := &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "instance:tool", Owner: workpad.BlockerOwnerInstance, Reason: "effective host gate lacks a passing current-head receipt", Unverifiable: true}}}
	for _, test := range []struct {
		sourceFixture        string
		importedVersion      bool
		finalMessage         string
		wantDisposition      *tracker.NativeDisposition
		enrolled             bool
		restartCompletion    bool
		legacyReleaseRestart bool
		completedContinue    bool
		continueMessage      bool
		summaryFailure       bool
		feedback             string
		initialSourceWait    bool
		reconnect            bool
		publicationFault     string
		publication          string
		largeOutput          bool
		dependencyOnly       bool
		failedCheck          bool
		absorbed             bool
		delivered            string
		hold                 bool
		lowScore             bool
		wantVerdict          string
		ssh                  bool
		validator            string
		name                 string
		localIntakeOff       bool
		interactive          bool
		hosted               bool
		rework               bool
		foreignRunner        bool
		stalePolicy          bool
		reopened             bool
		formal               bool
		failDetail           bool
		failVersion          bool
		land                 bool
		commit               bool
		staged               bool
		expired              bool
		brokenSigning        bool
		lateConflict         bool
		advanceTarget        bool
		baseMoved            bool
		staleBase            bool
		dirty                bool
		wantNone             bool
		wantChanged          bool
		wantState            string
		wantChanges          int
	}{
		{name: "blocked native729 first unchanged Rework preserves dependency", finalMessage: dependencyMessage, wantDisposition: dependencyDisposition, rework: true},
		{name: "imported unchanged Rework records implementing attempt before validation", importedVersion: true, rework: true, validator: "pass", land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "imported unchanged Rework cannot clear existing human rejection", importedVersion: true, rework: true, formal: true, validator: "pass", land: true},
		{name: "blocked native825 unchanged Rework preserves instance gate blocker", finalMessage: instanceMessage, wantDisposition: instanceDisposition, rework: true},
		{name: "blocked SSH Rework retains staged source", finalMessage: instanceMessage, wantDisposition: instanceDisposition, rework: true, ssh: true, staged: true},
		{name: "held publication reconciles lost create through new Runner lease", publication: "lost_create", rework: true, formal: true},
		{name: "ambiguous retained PR push followed by third party advancement executes Rework", publication: "lost_push", publicationFault: "advanced_pr", rework: true, formal: true},
		{name: "first publication HEAD discovery failure retries infrastructure", publication: "head_read", rework: true, formal: true},
		{name: "uncertain publication initial ref read failure retries infrastructure", publication: "lost_push", publicationFault: "initial_read", rework: true, formal: true},
		{name: "uncertain publication post push ref read failure retries infrastructure", publication: "lost_push", publicationFault: "post_push_read", rework: true, formal: true},
		{name: "held publication reconciles lost push through new Runner lease", publication: "lost_push", rework: true, formal: true},
		{name: "held implementation publication reconciles lost push on another Runner", publication: "lost_push", foreignRunner: true, staged: true},
		{name: "held implementation publication refreshes stale policy on another Runner", publication: "lost_push", publicationFault: "policy", foreignRunner: true, stalePolicy: true, staged: true},
		{name: "held publication records a fresh validator decision on new Runner lease", publication: "lost_create", rework: true, formal: true, validator: "pass"},
		{name: "confirmed publication retries pending validator on unchanged source", publication: "confirmed", rework: true, validator: "wait"},
		{name: "confirmed publication retries validator rework after prerequisite repair", publication: "confirmed", rework: true, validator: "rework"},
		{name: "confirmed publication reuses applicable passing native verdict", publication: "confirmed", rework: true, validator: "pass"},
		{name: "confirmed publication preserves human hold during validator recovery", publication: "confirmed", rework: true, formal: true, validator: "rework"},
		{name: "held publication preserves validator authority across configuration changes", publication: "lost_create", publicationFault: "policy", rework: true, formal: true, validator: "pass"},
		{name: "held publication refuses stale validator head", publication: "lost_create", publicationFault: "changed_head", rework: true, formal: true, validator: "pass"},
		{name: "held publication reconciles staged work through new Runner lease", publication: "lost_create", rework: true, formal: true, staged: true},
		{name: "held publication refuses replaced version on new Runner lease", publication: "lost_create", publicationFault: "version", rework: true, formal: true},
		{name: "held publication admits a foreign runner to reconcile", publication: "lost_create", publicationFault: "foreign_source", rework: true, formal: true},
		{name: "held publication refuses unavailable source on new Runner lease", publication: "lost_create", publicationFault: "unavailable_source", rework: true, formal: true},
		{name: "held publication repeats the write for missing_pr on new Runner lease", publication: "lost_create", publicationFault: "missing_pr", rework: true, formal: true},
		{name: "held publication refuses wrong_base on new Runner lease", publication: "lost_create", publicationFault: "wrong_base", rework: true, formal: true},
		{name: "held publication refuses wrong_repository on new Runner lease", publication: "lost_create", publicationFault: "wrong_repository", rework: true, formal: true},
		{name: "held publication refuses wrong_head on new Runner lease", publication: "lost_create", publicationFault: "wrong_head", rework: true, formal: true},
		{name: "held publication refuses wrong_branch on new Runner lease", publication: "lost_create", publicationFault: "wrong_branch", rework: true, formal: true},
		{name: "held publication refuses multiple_prs on new Runner lease", publication: "lost_create", publicationFault: "multiple_prs", rework: true, formal: true},
		{name: "held publication refuses unavailable_forge on new Runner lease", publication: "lost_create", publicationFault: "unavailable_forge", rework: true, formal: true},
		{name: "held publication repeats the write for missing_remote on new Runner lease", publication: "lost_create", publicationFault: "missing_remote", rework: true, formal: true},
		{name: "held publication refuses dirty_source on new Runner lease", publication: "lost_create", publicationFault: "dirty_source", rework: true, formal: true},
		{name: "held publication refuses changed_head on new Runner lease", publication: "lost_create", publicationFault: "changed_head", rework: true, formal: true},
		{name: "held publication survives configuration change on new Runner lease", publication: "lost_create", publicationFault: "policy", rework: true, formal: true},
		{name: "held publication refuses lease on new Runner lease", publication: "lost_create", publicationFault: "lease", rework: true, formal: true},
		{name: "held publication refuses branch on new Runner lease", publication: "lost_create", publicationFault: "branch", rework: true, formal: true},
		{name: "absorbed Rework settles the original version on its actual base", absorbed: true, rework: true, land: true, wantState: "Done", wantChanges: 1},
		{name: "NativeCompletion no-change source already on target", delivered: "ancestor", advanceTarget: true, rework: true, land: true, wantState: "Done", wantChanges: 1},
		{name: "NativeCompletion no-change reviewed head already delivered", delivered: "exact", rework: true, land: true, wantState: "Done", wantChanges: 1},
		{name: "NativeCompletion no-change release source already on release", delivered: "release", rework: true, land: true, wantState: "Done", wantChanges: 1},
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
		{name: "same worker reconnects during a provider command and publishes once", reconnect: true, staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "inherited synthetic fixtures publish", sourceFixture: "synthetic", staged: true, wantChanged: true, wantState: "In Review", wantChanges: 1},
		{name: "introduced credential refuses publication", sourceFixture: "introduced", staged: true},
		{name: "inherited credential refuses publication", sourceFixture: "inherited", staged: true},
		{name: "unsafe intermediate commit refuses publication", sourceFixture: "history", staged: true},
		{name: "enrolled source evidence hold releases capacity and resumes with fresh authority", enrolled: true, summaryFailure: true, staged: true, validator: "pass"},
		{name: "authenticated Continue delivers after retained source pass summary failure", continueMessage: true, summaryFailure: true, staged: true},
		{name: "authenticated Continue delivers after completed source and acceptance feedback", continueMessage: true, completedContinue: true, summaryFailure: true, feedback: "technical", validator: "pass", staged: true},
		{name: "authenticated Continue delivers after optional completed source feedback", continueMessage: true, completedContinue: true, summaryFailure: true, feedback: "technical", staged: true},
		{name: "authenticated Continue delivers after source wait summary failure", continueMessage: true, initialSourceWait: true, summaryFailure: true, staged: true},
		{name: "deferred interrupted source summary failure survives Scheduler restart", restartCompletion: true, enrolled: true, summaryFailure: true, staged: true},
		{name: "deferred interrupted source summary failure restores separate required acceptance", restartCompletion: true, enrolled: true, summaryFailure: true, staged: true, validator: "pass"},
		{name: "deferred interrupted source wait survives Scheduler restart", restartCompletion: true, enrolled: true, summaryFailure: true, initialSourceWait: true, staged: true},
		{name: "acknowledged interrupted completion survives legacy recapture restart", legacyReleaseRestart: true, enrolled: true, summaryFailure: true, staged: true},
		{name: "deferred source summary failure releases interrupted and resumes without coding", summaryFailure: true, staged: true},
		{name: "current change feedback refreshes retained source pass before acceptance", summaryFailure: true, staged: true, validator: "pass", feedback: "change"},
		{name: "issue feedback refreshes retained source pass before acceptance", summaryFailure: true, staged: true, validator: "pass", feedback: "issue"},
		{name: "human edit refreshes retained source pass before acceptance", summaryFailure: true, staged: true, validator: "pass", feedback: "edit"},
		{name: "current change feedback refreshes source wait before acceptance", summaryFailure: true, staged: true, validator: "pass", feedback: "change", initialSourceWait: true},
		{name: "technical evidence feedback reviews without writable work", summaryFailure: true, staged: true, feedback: "technical"},
		{name: "runner feedback preserves retained source pass reuse", summaryFailure: true, staged: true, feedback: "bot"},
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
		{name: "host signing unavailable preserves requested changes", rework: true, formal: true, staged: true, brokenSigning: true, wantChanged: true, wantState: "Human Review", wantChanges: 1},
		{name: "growing multi-page machine history keeps fresh conflict recovery compact", rework: true, staleBase: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "Rework lands the clean preserved reviewed head without source changes", rework: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "reopened completed Rework lands its reviewed current version", reopened: true, rework: true, staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "reopened Rework accepts current requested changes before repair", reopened: true, rework: true, formal: true, staged: true, land: true, wantChanged: true, wantState: "Merging", wantChanges: 1},
		{name: "Rework forwards a refused version to review", rework: true, commit: true, failVersion: true, wantState: "Human Review"},
		{name: "Rework scoped read failure releases claim before dispatch", rework: true, failDetail: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.publication != "head_read" {
				t.Parallel()
			}
			genuinePublication := test.publication != "" && test.validator != "" && (test.publicationFault == "" || test.publicationFault == "policy")
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
			if test.summaryFailure {
				states[1].Transitions = append(states[1].Transitions, "Blocked")
				states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"In Progress"}})
				if test.feedback != "" {
					states[1].Transitions = append(states[1].Transitions, "Rework")
					states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", review, "Done"}})
				}
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
			if test.summaryFailure {
				previous := h.descriptor.ID
				h.descriptor.Gates.AutomatedReview = gate.AutomatedReviewRequired
				h.descriptor = h.descriptor.WithID()
				if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
					t.Fatal(err)
				}
			}
			identityPath := ""
			if test.enrolled {
				identityPath = filepath.Join(t.TempDir(), "private", "runner.json")
				hubURL := strings.Replace(h.native.client.baseURL.String(), "http://", "https://", 1)
				file, err := runnerauth.Initialize(identityPath, hubURL)
				if err != nil {
					t.Fatal(err)
				}
				enrollmentAdmin, err := New(Config{URL: hubURL, TokenSource: func() string { return nativeChangeAdminToken }, HTTPClient: &http.Client{Transport: h.failChanges}})
				if err != nil {
					t.Fatal(err)
				}
				enrollment, err := enrollmentAdmin.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
				if err != nil {
					t.Fatal(err)
				}
				machine := Machine{ID: file.Identity.MachineID, Hostname: "host", Capacity: 1, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
				var identity runnerauth.Identity
				redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version, OS: runtime.GOOS, Architecture: runtime.GOARCH, IsolationTier: machine.IsolationTier, BackendIsolation: machine.BackendIsolation, SpriteName: runnerSpriteName(machine.Hostname)}
				if err := enrollmentAdmin.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &identity); err != nil {
					t.Fatal(err)
				}
				file.Identity = identity
				if err := runnerauth.Save(identityPath, file); err != nil {
					t.Fatal(err)
				}
				fleet, err := NewFleetClient(enrollmentAdmin, h.organization, map[string]tracker.ProjectID{"local": h.project})
				if err != nil {
					t.Fatal(err)
				}
				if err := fleet.UpdateRunner(t.Context(), file.Identity.RunnerID, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Source reviewer", State: "active", CapacityLimit: 1, ProjectIDs: []tracker.ProjectID{h.project}, IsolationTier: isolation.NativeTrusted, Availability: runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}}}); err != nil {
					t.Fatal(err)
				}
				client, err := New(Config{URL: hubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}})
				if err != nil {
					t.Fatal(err)
				}
				if h.native, err = client.Native(h.organization, h.project); err != nil {
					t.Fatal(err)
				}
				if h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold}); err != nil {
					t.Fatal(err)
				}
				if h.scheduler.client.runner == nil {
					t.Fatal("fixture did not enroll the claim owner")
				}
			}
			if test.localIntakeOff {
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
				if err := h.admin.client.request(t.Context(), http.MethodPost, h.admin.base()+"/conversations/"+created.Conversation.ID+"/link", map[string]any{"key": "link", "share_history": true, "issue": map[string]any{"title": "Update the README", "description": "Interactive work" + issueContractTestSections}}, &linked); err != nil {
					t.Fatal(err)
				}
				issue = connector.Issue{ID: string(linked.Issue.WorkItemID), Identifier: "native#1", Title: linked.Issue.Title}
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "In Progress"); err != nil {
					t.Fatal(err)
				}
			} else {
				issue = h.createInProgress(t, "Update the README")
			}
			if test.summaryFailure {
				confirmIssueContract(t, h.admin, issue.ID)
			}
			source := nativeChangeTempDir(t)
			if err := os.CopyFS(source, os.DirFS(sourceSeed)); err != nil {
				t.Fatal(err)
			}
			fixtures := map[string]string{}
			credential := "ghp_" + strings.Repeat("Z", 36)
			if test.sourceFixture != "" {
				for _, name := range []string{"health_email_test.go", "slack_delivery_test.go"} {
					data, err := os.ReadFile(filepath.Join("..", "hubserver", name))
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join("internal", "hubserver", name)
					fixtures[path] = string(data)
					if err := os.MkdirAll(filepath.Join(source, "internal", "hubserver"), 0o700); err != nil {
						t.Fatal(err)
					}
					content := string(data)
					if test.sourceFixture == "inherited" {
						content += "\n" + credential
					}
					if err := os.WriteFile(filepath.Join(source, path), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				nativeChangeGit(t, source, "add", "internal/hubserver")
				nativeChangeGit(t, source, "commit", "-m", "inherited notification fixtures")
			}
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
			if test.brokenSigning {
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
				nativeChangeGit(t, reviewedPath, "-c", "commit.gpgsign=false", "commit", "-m", "preserved reviewed source")
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
				old := h.publish(t, item, change.ID, nativeChangeFixtureHead("a"))
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
				if test.delivered == "release" {
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

				if test.advanceTarget || test.delivered == "exact" {
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
				if test.absorbed || test.delivered != "" {
					target := "main"
					if test.delivered == "release" {
						target = "release/v1.2"
					}
					head, err := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+target).Output()
					if err != nil {
						t.Fatal(err)
					}
					deliveredTargetHead = strings.TrimSpace(string(head))
				}
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
			if test.delivered == "release" {
				current := expected.Versions[len(expected.Versions)-1]
				execution.(*nativeExecution).claim.recovery.Issue.ReleaseDelivery = &tracker.ReleaseDelivery{ReleasePickInput: tracker.ReleasePickInput{Repository: current.Repository, PRNumber: 7, BaseRef: "release/v1.2", BaseSHA: current.BaseSHA, HeadSHA: current.HeadSHA, HeadRef: deliveryHeadRef, SourceMergeSHA: current.HeadSHA}, ChangeID: expected.Change.ID, VersionID: current.ID}
				next := h.native.client.httpClient.Transport
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/release-pick") {
						status := tracker.ReleaseDeliveryStatus{ReleasePickInput: execution.Recovery().Issue.ReleaseDelivery.ReleasePickInput, ReleaseHeadSHA: current.HeadSHA}
						raw, err := json.Marshal(status)
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw))), Request: request}, err
					}
					return next.RoundTrip(request)
				})
			}
			if test.enrolled {
				h.connector, err = NewNativeConnector(h.native)
				if err != nil {
					t.Fatal(err)
				}
			}
			provider := &committingAgent{finalMessage: test.finalMessage, dependencyOnly: test.dependencyOnly, failedCheck: test.failedCheck, commit: test.commit, dirty: test.dirty, staged: test.staged || genuinePublication, validator: test.validator, lowScore: test.lowScore, complete: test.absorbed || test.delivered != "" || test.summaryFailure, hold: test.hold, inProgress: test.lateConflict}
			if test.sourceFixture != "" {
				provider.duringTurn = func() {
					for path, original := range fixtures {
						file := filepath.Join(provider.workspace, path)
						if test.sourceFixture == "history" {
							if err := os.WriteFile(file, []byte(original+"\n"+credential), 0o600); err != nil {
								t.Fatal(err)
							}
							nativeChangeGit(t, provider.workspace, "add", path)
							nativeChangeGit(t, provider.workspace, "commit", "-m", "unsafe intermediate fixture")
						}
						content := original + "\n"
						if test.sourceFixture == "inherited" || test.sourceFixture == "introduced" {
							content += credential + "\n"
						}
						if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
							t.Fatal(err)
						}
						nativeChangeGit(t, provider.workspace, "add", path)
					}
				}
			}
			if test.publication == "confirmed" {
				provider.duringValidation = func(request runner.AgentTurnRequest) {
					t.Helper()
					owner := execution.(*nativeExecution)
					if !request.ReadOnly || owner.publication == nil || owner.data.Outcome != "" || owner.data.Finalization != nil {
						t.Fatal("acceptance must read confirmed publication before attempt completion")
					}
					validation, err := owner.ValidatorVersion(t.Context())
					if err != nil || validation.Version == nil || validation.Version.External == nil || *validation.Version.External != owner.publication.External || validation.Version.ID != owner.change.VersionID || validation.Version.HeadSHA != owner.publication.HeadSHA {
						t.Fatalf("acceptance lacks the exact confirmed current PR binding: %+v error=%v", validation.Version, err)
					}

				}
			}
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
			var providerStartHead string
			if test.delivered != "" {
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
			if test.reconnect {
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
						if err := h.admin.client.request(ctx, http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/conversation", nil, &before); err != nil {
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
					h.reopen(func(string) error { return nil })
					h.failChanges.controlFailures.Store(0)
					after := before
					if err := h.admin.client.request(ctx, http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/conversation", nil, &after); err != nil {
						t.Fatal(err)
					}
					if after.Conversation.Execution.Owner != before.Conversation.Execution.Owner || after.Conversation.Execution.Status != conversation.ExecutionRunning || after.Conversation.Execution.Capabilities != before.Conversation.Execution.Capabilities {
						t.Fatalf("restart lost current worker: before=%+v after=%+v", before.Conversation.Execution, after.Conversation.Execution)
					}
					var receipt conversation.Receipt
					if err := h.admin.client.request(ctx, http.MethodPost, h.admin.base()+"/conversations/"+after.Conversation.ID+"/commands", conversation.Command{Key: "reconnected-steer", Kind: conversation.CommandMessage, Text: "Keep validating", Expected: conversation.Expected{AttemptID: after.Conversation.Execution.Owner.AttemptID, TurnID: "turn-1"}}, &receipt); err != nil {
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
			var runtimeStore store.Store
			if test.validator != "" || test.summaryFailure {
				runtimeStore, err = store.Open(t.Context(), store.Config{Path: nativeRuntimeDatabasePath(t)})
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
				candidate.Description = "Remove the unused direct @radix-ui/themes dependency. Required validation: frontend pnpm build, frontend pnpm check, root make check-fast." + issueContractTestSections
			}
			gateMarker := filepath.Join(t.TempDir(), "gate-invoked")
			if test.finalMessage != "" {
				validationCommand = "printf gate > '" + strings.ReplaceAll(gateMarker, "'", "'\"'\"'") + "'; exit 2"
			}
			automatedReview := ""
			if test.summaryFailure {
				automatedReview = gate.AutomatedReviewRequired
			}
			var runLogs bytes.Buffer
			t.Cleanup(func() {
				if t.Failed() {
					t.Log(runLogs.String())
				}
			})
			dependencies := runner.Dependencies{ProcessSource: emptyProcessSource,
				Logger:       slog.New(slog.NewTextHandler(&runLogs, nil)),
				Store:        runtimeStore,
				ProjectID:    "local",
				Workflow:     config.Workflow{Config: config.Config{Gate: gate.Config{Run: validationCommand, AutomatedReview: automatedReview, Validator: gate.ValidatorConfig{Enabled: test.validator != ""}}}, Prompt: "Complete the issue"},
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
			transport := h.native.client.httpClient.Transport
			postedSource := false
			if test.summaryFailure {
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if postedSource && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/changes/") {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("summary unavailable")), Request: request}, nil
					}
					response, err := transport.RoundTrip(request)
					if err == nil && response != nil && request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/reviews") && response.StatusCode >= 200 && response.StatusCode < 300 {
						postedSource = true
					}
					return response, err
				})
			}
			var editedComment tracker.NativeComment
			if test.feedback == "edit" {
				var err error
				editedComment, err = h.native.CreateComment(t.Context(), tracker.NativeWorkItemID(issue.ID), tracker.CreateComment{Mutation: nativeMutationKey(), Body: "Earlier runner context."})
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.initialSourceWait {
				provider.sourceVerdict = "wait"
			}
			var publicationRefsBefore []byte
			if test.publication == "head_read" {
				runExecution.(*nativePublicationFixtureExecution).beforePublication = func() {
					if execution.(*nativeExecution).data.Handoff.UncertainForgeEffect() {
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
			request := runner.RunRequest{Execution: runExecution, DeferExecutionFinish: test.failVersion || test.summaryFailure, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement, OnUsageUpdate: func(update runner.UsageUpdate) error {
				if test.summaryFailure && update.DetentSessionID > 0 {
					implementationSession.CompareAndSwap(0, update.DetentSessionID)
				}
				return nil
			}}
			var result runner.RunResult
			if test.delivered != "" {
				result, err = runNativeCompletionOrchestrator(t, h, agent, request)
			} else {
				result, err = agent.Run(runCtx, request)
			}
			if test.publication == "head_read" {
				owner := execution.(*nativeExecution)
				if !nativeTransportUnavailable(err) || owner.settled || owner.data.Outcome != "" || owner.data.Handoff.UncertainForgeEffect() || owner.publication != nil || forgeCreates != 0 || forgePull != "" {
					t.Fatalf("HEAD discovery failure lost infrastructure retry: settled=%t outcome=%q checkpoint=%+v error=%v", owner.settled, owner.data.Outcome, owner.data.Handoff, err)
				}
				if after, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)").Output(); readErr != nil || string(after) != string(publicationRefsBefore) {
					t.Fatalf("HEAD discovery failure wrote remote refs: before=%s after=%s", publicationRefsBefore, after)
				}
				return
			}
			if test.summaryFailure {
				h.native.client.httpClient.Transport = transport
				owner := execution.(*nativeExecution)
				var producerErr *runner.NativeImplementationReviewError
				change := result.NativeChange
				if !postedSource || !errors.As(err, &producerErr) || change == nil || change.VersionID == "" || change.ImplementationReviewError == "" || owner.data.Outcome != "" || provider.calls != 2 {
					t.Fatalf("deferred producer failure lost acknowledged source: result=%+v turns=%d prepared=%q finished=%q error=%v", result, provider.calls, owner.preparedOutcome, owner.data.Outcome, err)
				}
				item := tracker.NativeWorkItemID(issue.ID)
				original, err := h.admin.Change(t.Context(), item, change.ChangeID)
				if err != nil {
					t.Fatal(err)
				}
				wantDecision := "approved"
				if test.initialSourceWait {
					wantDecision = "commented"
				}
				if len(original.Versions) != 1 || len(original.Reviews) != 1 || original.Reviews[0].Decision != wantDecision {
					t.Fatalf("source POST was not durable: %+v", original)
				}
				restorePreparedOwner := func(owner *nativeExecution, savedIssue connector.Issue) *nativeExecution {
					transitionAttempts := 0
					ownerClient := owner.claim.source.client.client
					ownerTransport := ownerClient.httpClient.Transport
					ownerConnector, err := NewNativeConnector(owner.claim.source.client)
					if err != nil {
						t.Fatal(err)
					}
					ownerClient.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/workflow") {
							transitionAttempts++
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("completion unavailable")), Request: request}, nil
						}
						return ownerTransport.RoundTrip(request)
					})
					if err := ownerConnector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err == nil || transitionAttempts == 0 {
						t.Fatalf("completion outage did not reach the tracker transition: requests=%d error=%v", transitionAttempts, err)
					}
					ownerClient.httpClient.Transport = ownerTransport
					saved := owner.CompletionState()
					var decoded nativeCompletionState
					if err := json.Unmarshal(saved, &decoded); err != nil || decoded.PreparedOutcome != "interrupted" || decoded.Data.Outcome != "" || decoded.Pending != nil {
						t.Fatalf("deferred interruption capture was not before first Finish: %+v error=%v", decoded.Data, err)
					}
					wantFailure, err := json.Marshal(owner.preparedFailure)
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
					machine := h.scheduler.machine
					h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: source}, savedIssue, saved); err != nil {
						t.Fatal(err)
					}
					h.connector = ownerConnector
					return h.scheduler.RunExecution(issue.ID).(*nativeExecution)
				}
				if test.restartCompletion {
					owner = restorePreparedOwner(owner, candidate)
				}
				oldLease := owner.Recovery().Lease
				if test.feedback == "bot" {
					if _, err := h.native.CreateComment(t.Context(), item, tracker.CreateComment{Mutation: nativeMutationKey(), Body: "CHANGE.md must contain the repaired feedback value before this source can be accepted."}); err != nil {
						t.Fatal(err)
					}
				}
				if err := h.connector.UpdateIssueState(t.Context(), issue.ID, "Blocked"); err != nil {
					t.Fatal(err)
				}
				if test.legacyReleaseRestart {
					if err := owner.Finish(t.Context(), "interrupted"); err != nil || owner.data.Outcome != "interrupted" || owner.data.Disposition == nil || owner.data.Disposition.Status != "complete" || owner.data.Finalization == nil || !owner.data.Finalization.Settled {
						t.Fatalf("legacy release retry lacks acknowledged complete interruption: %+v error=%v", owner.data, err)
					}
					before := owner
					originalCompletion, err := json.Marshal([]any{owner.data.Finalization, owner.data.Disposition, owner.data.TerminalFailure, owner.data.Outcome})
					if err != nil {
						t.Fatal(err)
					}
					var originalData []byte
					ownerClient := owner.claim.source.client.client
					transport := ownerClient.httpClient.Transport
					releaseAttempts := 0
					ownerClient.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/release") {
							releaseAttempts++
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("release unavailable")), Request: request}, nil
						}
						return transport.RoundTrip(request)
					})
					if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err == nil || releaseAttempts == 0 {
						t.Fatalf("acknowledged interruption did not reach release outage: requests=%d error=%v", releaseAttempts, err)
					}
					ownerClient.httpClient.Transport = transport
					for restart := range 2 {
						var saved json.RawMessage
						if restart > 0 {
							saved = owner.CompletionState()
						}
						machine := h.scheduler.machine
						h.scheduler, err = NewScheduler(h.native.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := h.scheduler.RestoreCompletion(t.Context(), orchestrator.SchedulingRequest{ProjectID: "local", Policy: h.descriptor, Repository: source}, candidate, saved); err != nil {
							t.Fatal(err)
						}
						owner = h.scheduler.RunExecution(issue.ID).(*nativeExecution)
						if owner.data.RunID != before.data.RunID || owner.data.AttemptID != before.data.AttemptID || owner.data.PolicyID != before.data.PolicyID || owner.data.Identity == nil || before.data.Identity == nil || *owner.data.Identity != *before.data.Identity || !sameCompletionLease(owner.claim.lease, before.claim.lease) {
							t.Fatal("acknowledged completion restart changed run authority")
						}
						actualData, err := json.Marshal(owner.data)
						if restart == 0 {
							actualCompletion, completionErr := json.Marshal([]any{owner.data.Finalization, owner.data.Disposition, owner.data.TerminalFailure, owner.data.Outcome})
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
				evidence, err := h.admin.RuntimeEvidence(t.Context(), item, owner.data.AttemptID)
				if err != nil || evidence.Attempt == nil || evidence.Attempt.Finalization == nil {
					t.Fatalf("released claim has no authoritative finalization: %+v error=%v", evidence.Attempt, err)
				}
				attempt, finalization := evidence.Attempt, evidence.Attempt.Finalization
				if attempt.Status != "interrupted" || attempt.Disposition == nil || attempt.Disposition.Status != "complete" || attempt.TerminalFailure == nil || !finalization.Changed || finalization.Reviewed || finalization.ChangeID != change.ChangeID || finalization.VersionID != change.VersionID || finalization.HeadSHA != change.HeadSHA || attempt.PolicyID != h.descriptor.ID {
					t.Errorf("claim release lost pending technical evidence: attempt=%+v finalization=%+v", attempt, finalization)
				}
				if test.enrolled {
					identity, err := h.scheduler.client.RunnerIdentity(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					var routing runnerauth.Runner
					path := "/api/v2/organizations/" + string(h.organization) + "/runners/" + identity.RunnerID + "/routing"
					if err := h.scheduler.client.request(t.Context(), http.MethodGet, path, nil, &routing); err != nil {
						t.Fatal(err)
					}
					if routing.Used != 0 || routing.HostUsed != 0 || len(routing.Leases) != 0 {
						t.Errorf("settled source evidence hold retained runner capacity: used=%d host_used=%d leases=%+v", routing.Used, routing.HostUsed, routing.Leases)
					}
					if _, err := h.native.Renew(t.Context(), oldLease, 90); !nativeLeaseLost(err) {
						t.Errorf("settled source evidence hold retained renewable authority: %v", err)
					}
					client, err := New(Config{URL: h.native.client.baseURL.String(), IdentityFile: identityPath, HTTPClient: &http.Client{Transport: h.failChanges}})
					if err != nil {
						t.Fatal(err)
					}
					machine := h.scheduler.machine
					h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
					if err != nil {
						t.Fatal(err)
					}
				}
				if held := h.candidates(t); len(held) != 0 {
					t.Errorf("reconnect reoffered technical evidence hold with retained authority: candidates=%+v", held)
					return
				}
				if test.completedContinue {
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
					if test.validator != "" {
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
				feedbackRework := test.feedback == "change" || test.feedback == "issue" || test.feedback == "edit"
				freshSourceReview := test.initialSourceWait || test.feedback != "" && test.feedback != "bot"
				if test.feedback == "technical" {
					feedbackBody = "The host evidence is now available; independently check it without changing source."
				}
				var feedbackRecord any
				switch test.feedback {
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
				if test.feedback != "" && test.feedback != "bot" {
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
				if test.continueMessage {
					if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/conversation", nil, &continued); err != nil {
						t.Fatal(err)
					}
					var receipt conversation.Receipt
					if err := h.admin.client.request(t.Context(), http.MethodPost, h.admin.base()+"/conversations/"+continued.Conversation.ID+"/commands", conversation.Command{Key: "continue-evidence", Kind: conversation.CommandContinue, Text: continueText, Expected: conversation.Expected{AttemptID: continued.attempt()}}, &receipt); err != nil || receipt.Status != conversation.DeliverySaved {
						t.Fatalf("authenticated Continue was not saved: %+v error=%v", receipt, err)
					}
				}
				if current.State != "In Progress" {
					if _, err := h.admin.Transition(t.Context(), item, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}); err != nil {
						t.Fatal(err)
					}
				}
				if test.enrolled {
					source := h.scheduler.nativeProject("local")
					if source == nil {
						t.Fatal("reconnected source owner is missing")
					}
					if err := source.client.HeartbeatMachine(t.Context(), h.scheduler.machine); err != nil {
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
				if test.validator != "" && !feedbackRework {
					wantCalls++
				}
				if test.continueMessage {
					provider.duringLiveTurn = func(request runner.AgentTurnRequest, _ runner.AgentUpdateHandler) {
						if !strings.Contains(request.Prompt, continueText) || request.ConversationControl == nil {
							t.Fatal("primary turn omitted bound Continue prompt or controls")
						}
						testenv.WaitUntil(t, t.Context(), func() error {
							var started runnerSnapshot
							if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/conversation", nil, &started); err != nil {
								t.Fatal(err)
							}
							if started.delivery("continue-evidence") == conversation.DeliveryDelivered && started.attempt() == resumedExecution.(*nativeExecution).data.AttemptID && started.Conversation.Execution.Status == conversation.ExecutionRunning {
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
				if test.continueMessage {
					var delivered runnerSnapshot
					if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/work-items/"+issue.ID+"/conversation", nil, &delivered); err != nil {
						t.Fatal(err)
					}
					if resumeErr != nil || resumedSession.Load() == 0 || resumedSession.Load() == implementationSession.Load() || provider.calls != calls+1 || provider.writableCalls != writableCalls+1 || !resumed.TurnStarted || !strings.Contains(provider.prompt, continueText) || delivered.delivery("continue-evidence") != conversation.DeliveryDelivered || !strings.Contains(delivered.assistantText(), "Finished the native work") || delivered.attempt() == continued.attempt() || !resumedExecution.(*nativeExecution).conversationContinuation {
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
				if test.validator != "" && !feedbackRework {
					wantReviews++
				}
				if err != nil || len(latest.Reviews) != wantReviews || !reflect.DeepEqual(original.Versions, latest.Versions) || !reflect.DeepEqual(original.Reviews, latest.Reviews[:min(len(original.Reviews), len(latest.Reviews))]) {
					t.Errorf("resume changed immutable source or recorded review: original=%+v latest=%+v error=%v", original, latest, err)
				}
				if feedbackRework {
					if resumed.NativeChange == nil || resumed.NativeChange.ImplementationReview == nil || resumed.NativeChange.ImplementationReview.Verdict != "rework" || resumed.NativeChange.Reviewed || latest.Reviews[len(latest.Reviews)-1].Validator != nil || latest.Reviews[len(latest.Reviews)-1].Decision != "changes_requested" {
						t.Fatalf("new source feedback bypassed independent rework: change=%+v reviews=%+v", resumed.NativeChange, latest.Reviews)
					}
				} else if test.validator != "" {
					if len(latest.Reviews) != 2 || latest.Reviews[1].Validator == nil || latest.Reviews[1].Validator.SessionID == 0 || original.Reviews[0].ImplementationReview == nil || latest.Reviews[1].Validator.SessionID == original.Reviews[0].ImplementationReview.SessionID || latest.Reviews[1].Validator.SessionID == implementationSession.Load() || !resumed.NativeChange.Reviewed {
						t.Fatalf("resume did not separately record current source and acceptance: reviews=%+v change=%+v", latest.Reviews, resumed.NativeChange)
					}
				}
				freshChanges, err := filepath.Glob(filepath.Join(freshRoot, "*", "CHANGE.md"))
				wantWorkspaces := 1
				if freshSourceReview || test.validator != "" {
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
				resumedOwner := resumedExecution.(*nativeExecution)
				stored := original.Versions[0]
				review := original.Reviews[0].ImplementationReview
				if stored.Source == nil || stored.Validation == nil || stored.RunID != owner.data.RunID || stored.AttemptID != owner.data.AttemptID || resumedOwner.data.RunID != stored.RunID || resumedOwner.data.AttemptID == stored.AttemptID || implementationSession.Load() == 0 || review == nil || review.SessionID == 0 || review.SessionID == implementationSession.Load() {
					t.Fatalf("resume lost source, session, or fresh-workspace identity: stored=%+v review=%+v original=%+v resumed=%+v", stored, review, evidence.Attempt, resumedOwner.data)
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
				} else if err == nil || owner.data.Handoff == nil || owner.data.Handoff.ExternalEffect != wantEffect || owner.data.Handoff.EffectState != wantState || forgeCreates != wantCreates {
					t.Fatalf("first Runner attempt did not preserve lost create: effect=%+v creates=%d error=%v", owner.data.Handoff, forgeCreates, err)
				}
				if expected == nil {
					current, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), owner.change.ChangeID)
					if readErr != nil || len(current.Versions) != 1 {
						t.Fatalf("implementation publication did not retain its version: %+v, %v", current.Versions, readErr)
					}
					expected = &current
				} else if test.staged {
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
				retainedHead := ""
				if test.publicationFault == "advanced_pr" {
					advanced, advanceErr := exec.CommandContext(t.Context(), "git", "-C", remote, "commit-tree", oldPublicationTree, "-p", oldPublicationHead, "-m", "third party advancement").Output()
					if advanceErr != nil {
						t.Fatal(advanceErr)
					}
					advancedHead := strings.TrimSpace(string(advanced))
					nativeChangeGit(t, remote, "update-ref", "refs/heads/"+oldPublicationBranch, advancedHead)
					forgePull = fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":%q,"ref":%q,"repo":{"full_name":"example/native-change"}},"base":{"ref":"main","repo":{"full_name":"example/native-change"}}}`, advancedHead, oldPublicationBranch)
					retainedHead = oldPublicationHead
				}
				if test.publicationFault == "post_push_read" {
					nativeChangeGit(t, remote, "update-ref", "-d", "refs/heads/"+oldPublicationBranch)
				}
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
				if test.publicationFault == "foreign_source" || test.foreignRunner {
					machineID = "foreign-machine"
				}
				h.scheduler, err = NewScheduler(h.scheduler.client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: machineID, Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second, LeaseHold: h.leaseHold})
				if err != nil {
					t.Fatal(err)
				}
				if test.publicationFault == "policy" {
					h.repolicy(t)
				}
				if test.stalePolicy {
					rules, err := h.admin.ChangeReviewPolicy(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					rules.PolicyID = h.descriptor.ID
					rules.RequireReview = true
					if _, err := h.admin.ApproveChangeReviewPolicy(t.Context(), tracker.ApproveChangeReviewPolicy{Mutation: nativeMutationKey(), ExpectedID: rules.ID, Policy: rules}); err != nil {
						t.Fatal(err)
					}
				}
				publicationRetry = true
				reconcileLane := "Rework"
				if !test.rework {
					reconcileLane = "In Progress"
				}
				candidates := h.candidatesIn(t, reconcileLane)
				if test.publicationFault == "foreign_source" {
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
				owner = execution.(*nativeExecution)
				if test.stalePolicy && (owner.Recovery().ChangeDetail == nil || owner.Recovery().ChangeDetail.Summary.Status != "stale_policy") {
					t.Fatal("retry must reproduce a stale current Change policy")
				}
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
				remoteBefore, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags").Output()
				if readErr != nil {
					t.Fatal(readErr)
				}
				var publicationFault func(*workspace.LandOptions) func()
				var readFailure, pushMarker string
				if test.publicationFault == "initial_read" || test.publicationFault == "post_push_read" {
					realGit, lookupErr := exec.LookPath("git")
					if lookupErr != nil {
						t.Fatal(lookupErr)
					}
					wrapper := t.TempDir()
					pushMarker = filepath.Join(wrapper, "pushed")
					readFailure = filepath.Join(wrapper, "read-failed")
					quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
					condition := "true"
					if test.publicationFault == "post_push_read" {
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
				if test.stalePolicy {
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
				result, err = agent.Run(t.Context(), runner.RunRequest{Execution: &nativePublicationFixtureExecution{nativeExecution: owner, forge: forge, losePush: &losePush, retainedHead: retainedHead, publicationFault: publicationFault}, ProjectID: "local", Issue: candidate, Mode: runner.RunModeImplement})
				result.NativeChange = owner.NativeChange()
				if test.publicationFault == "initial_read" || test.publicationFault == "post_push_read" {
					if _, statErr := os.Stat(readFailure); statErr != nil {
						t.Fatalf("publication did not reach the injected ref read failure: %v; run error=%v", statErr, err)
					}
					_, pushErr := os.Stat(pushMarker)
					if test.publicationFault == "post_push_read" && pushErr != nil || test.publicationFault == "initial_read" && !errors.Is(pushErr, os.ErrNotExist) {
						t.Fatalf("publication ref read failed at the wrong push boundary: %v", pushErr)
					}
					if !nativeTransportUnavailable(err) || !owner.data.Handoff.UncertainForgeEffect() || owner.settled || provider.calls != callsBefore || forgeCreates != wantCreates || forgePull != prBefore {
						t.Fatalf("ref read failure lost infrastructure retry: settled=%t checkpoint=%+v error=%v", owner.settled, owner.data.Handoff, err)
					}
					branchHead, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+oldPublicationBranch).Output()
					if readErr != nil || strings.TrimSpace(string(branchHead)) != oldPublicationHead {
						t.Fatalf("ref read failure changed intended source: %s %v", branchHead, readErr)
					}
					return
				}
				if test.publicationFault == "advanced_pr" {
					if owner.data.Handoff.UncertainForgeEffect() || provider.calls != callsBefore || forgeCreates != wantCreates || forgePull != prBefore {
						t.Fatalf("advanced PR did not settle uncertainty without writing: checkpoint=%+v error=%v", owner.data.Handoff, err)
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
					nextOwner := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
					_, nextErr := agent.Run(t.Context(), runner.RunRequest{Execution: &nativePublicationFixtureExecution{nativeExecution: nextOwner, forge: forge, retainedHead: retainedHead}, ProjectID: "local", Issue: next[0], Mode: runner.RunModeImplement})
					if provider.calls != callsBefore+1 {
						t.Fatalf("settled Rework skipped provider turn: calls=%d want=%d error=%v", provider.calls, callsBefore+1, nextErr)
					}
					return
				}
				if test.publicationFault == "missing_pr" || test.publicationFault == "missing_remote" {
					repeatedCreates := wantCreates
					if test.publicationFault == "missing_pr" {
						repeatedCreates++
					}
					branchHead, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+oldPublicationBranch).Output()
					if err != nil || readErr != nil || owner.publication == nil || owner.publication.HeadSHA != oldPublicationHead || forgeCreates != repeatedCreates || strings.TrimSpace(string(branchHead)) != oldPublicationHead || provider.calls != callsBefore {
						t.Fatalf("absent forge write was not repeated exactly once: fault=%s creates=%d publication=%+v error=%v", test.publicationFault, forgeCreates, owner.publication, err)
					}
					return
				}
				if test.publicationFault != "" && test.publicationFault != "policy" {
					if err == nil && (result.NativeChange == nil || result.NativeChange.VersionError == "" && result.NativeChange.Error == "") || owner.publication != nil || forgeCreates != wantCreates || forgePull != prBefore || provider.calls != callsBefore {
						t.Fatalf("unresolved publication was accepted or rewrote forge/source: fault=%s creates=%d publication=%v provider_calls=%d error=%v", test.publicationFault, forgeCreates, owner.publication != nil, provider.calls-callsBefore, err)
					}
					remoteAfter, readErr := exec.CommandContext(t.Context(), "git", "-C", remote, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags").Output()
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

				if (test.publicationFault == "" || test.publicationFault == "policy") && (err != nil || result.NativeChange == nil || owner.publication == nil || forgeCreates != 1) {
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
				if test.stalePolicy {
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
						if version.ID != detail.Change.CurrentVersion || version.HeadSHA != oldPublicationHead || version.PolicyID != h.descriptor.ID || version.Source == nil || !owner.publication.Matches(detail.Change.ID, version) {
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
				if test.validator != "" && (test.publicationFault == "" || test.publicationFault == "policy") && expected.Summary.Status != "reviewed" {
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
				if test.publicationFault != "" && test.publicationFault != "policy" {
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
			if test.absorbed || test.delivered != "" {
				change := result.NativeChange
				if test.delivered != "" {
					head, headErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "rev-parse", "HEAD").Output()
					status, statusErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "status", "--porcelain").Output()
					if headErr != nil || statusErr != nil || providerStartHead == "" || (test.delivered == "exact" || test.delivered == "release") && strings.TrimSpace(string(head)) != providerStartHead || strings.TrimSpace(string(status)) != "" {
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
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				version := expected.Versions[len(expected.Versions)-1]
				if stored.BaseSHA != version.BaseSHA || stored.HeadSHA != version.HeadSHA || len(stored.Files) != 1 || stored.Files[0].Path != "PRESERVED.md" || stored.Files[0].Additions != 1 || stored.Files[0].Deletions != 0 || stored.AttemptID != attempt || stored.Producer.LeaseID != execution.Recovery().Lease.ID || stored.Producer.FencingToken != execution.Recovery().Lease.FencingToken {
					t.Fatalf("incorporated source lost its reviewed delta or attempt identity: %+v", stored)
				}
				if test.formal || test.hold {
					if change.Landing != nil || detail.Change.Landed != nil || change.VersionID != version.ID || change.VersionError != "" || test.formal && (change.Reviewed || detail.Summary.NativeReview != "changes_requested") {
						t.Fatalf("human rejection was waived: %+v", change)
					}
					h.complete(t, issue.ID, change)
					evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), attempt)
					if err != nil || evidence.Attempt == nil || evidence.Attempt.Finalization == nil || evidence.Attempt.Finalization.ChangeID != expected.Change.ID || evidence.Attempt.Finalization.VersionID != version.ID || evidence.Attempt.Finalization.HeadSHA != version.HeadSHA || evidence.Attempt.Finalization.VersionError != change.VersionError || evidence.Attempt.ClaimReleasedAt == nil || test.hold && (evidence.Attempt.Disposition == nil || !evidence.Attempt.Disposition.HumanAction) {
						t.Fatalf("refusal projection lost existing source provenance: %+v, %v", evidence.Attempt, err)
					}
				} else {
					target := "main"
					if test.delivered == "release" {
						target = "release/v1.2"
					}
					head, err := exec.CommandContext(t.Context(), "git", "-C", remote, "rev-parse", "refs/heads/"+target).Output()
					if err != nil {
						t.Fatal(err)
					}
					merge := deliveredTargetHead
					if test.advanceTarget {
						merge = targetHead
					}
					if merge == "" || strings.TrimSpace(string(head)) != merge || change.Landing == nil || !change.Landing.Landed || detail.Change.Landed == nil || detail.Change.Landed.VersionID != expected.Change.CurrentVersion || detail.Change.Landed.MergeSHA != merge || detail.Change.Landed.BaseRef != target || change.VersionError != "" {
						t.Fatalf("absorbed source lacks authentic landing: change=%+v detail=%+v", change, detail)
					}
					if test.delivered == "" {
						if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "completed"); err != nil {
							t.Fatal(err)
						}
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
			if test.brokenSigning {
				signature, readErr := exec.CommandContext(t.Context(), "git", "-C", provider.workspace, "log", "-1", "--format=%G?").Output()
				if readErr != nil || strings.TrimSpace(string(signature)) != "N" {
					t.Fatalf("unavailable signer did not produce an unsigned final commit: %s, %v", signature, readErr)
				}
			}
			if test.importedVersion && test.formal {
				detail, readErr := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), expected.Change.ID)
				if err == nil || readErr != nil || !reflect.DeepEqual(detail.Versions, expected.Versions) || !reflect.DeepEqual(detail.Reviews, expected.Reviews) || detail.Summary.NativeReview != "changes_requested" || detail.Change.CurrentLanding() != nil {
					t.Fatalf("imported source superseded human rejection: error=%v read=%v summary=%+v", err, readErr, detail.Summary)
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
			if test.sourceFixture != "" && test.sourceFixture != "synthetic" {
				if change == nil || change.Changed || change.VersionID != "" || change.VersionError != "checkpoint could not be safely published: possible credential in selected content" || len(h.changes(t, issue.ID)) != 0 {
					t.Fatal("unsafe source was published or lost its capture refusal")
				}
				return
			}
			if test.advanceTarget {
				attempt := executionID("attempt", string(execution.Recovery().Lease.ID))
				var stored tracker.AttemptDiff
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+attempt+"/diff", nil, &stored); err != nil {
					t.Fatal(err)
				}
				owner := execution.(*nativeExecution).data
				wantBase := targetHead
				if !test.staged && !test.commit {
					wantBase = expected.Versions[len(expected.Versions)-1].BaseSHA
				}
				if targetHead == "" || stored.BaseSHA != wantBase || stored.HeadSHA != execution.(*nativeExecution).worktreeHead || stored.AttemptID != owner.AttemptID || stored.Producer.FencingToken != execution.Recovery().Lease.FencingToken {
					t.Fatalf("final diff lost prepared source or fenced attempt identity: %+v, target=%s", stored, targetHead)
				}
				if test.staged || test.commit {
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
				if err != nil {
					t.Fatal(err)
				}
				if test.importedVersion {
					if len(detail.Versions) != len(expected.Versions)+1 || len(detail.Reviews) != len(expected.Reviews)+1 {
						t.Fatal("imported source did not publish exactly one version and fresh review")
					}
					current := detail.Versions[len(detail.Versions)-1]
					owner := execution.(*nativeExecution).data
					if current.AttemptID != owner.AttemptID || current.RunID != owner.RunID || current.HeadSHA != expected.Versions[len(expected.Versions)-1].HeadSHA || !reflect.DeepEqual(detail.Versions[:len(expected.Versions)], expected.Versions) || !reflect.DeepEqual(detail.Reviews[:len(expected.Reviews)], expected.Reviews) {
						t.Fatal("imported source lost attempt provenance, unchanged head, or immutable history")
					}
					detail.Reviews = slices.DeleteFunc(slices.Clone(detail.Reviews), func(review tracker.ChangeReview) bool { return review.VersionID != change.VersionID })
				}
				if len(detail.Reviews) != 1 || detail.Reviews[0].Validator == nil || detail.Reviews[0].VersionID != change.VersionID || change.Validator.HeadSHA != change.HeadSHA {
					t.Fatalf("validator verdict was not pinned to the immutable version: %+v, %v", detail, err)
				}
				if test.dependencyOnly {
					receipts := detail.Reviews[0].Validator.Commands
					if len(receipts) != 4 {
						t.Fatalf("command evidence missing from native review: %+v", receipts)
					}
					if receipts[0].TimingStage != "finalization" || receipts[0].Command != receipts[1].Command {
						t.Fatalf("native review lost the finalization gate evidence: %+v", receipts)
					}
					for index, receipt := range receipts {
						wantExit := 0
						if test.failedCheck && index == 3 {
							wantExit = 7
						}
						if receipt.HeadSHA != change.HeadSHA || receipt.TreeSHA == "" || receipt.TreeSHA != receipts[0].TreeSHA || receipt.ExitCode != wantExit || receipt.Output == "" {
							t.Fatalf("unverified native command receipt: %+v", receipt)
						}
					}
					if receipts[0].OutputTruncated != test.largeOutput || receipts[1].OutputTruncated != test.largeOutput {
						t.Fatalf("large output evidence not preserved: finalization=%t validation=%t", receipts[0].OutputTruncated, receipts[1].OutputTruncated)
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
				if f := evidence.Attempt.TerminalFailure; evidence.Attempt.Status != "failed" || f == nil || f.ErrorClass != "publication" || f.Stage != "publication" || f.Error != change.VersionCode+": "+change.VersionError {
					t.Fatalf("failed finalization ended without its cause: status=%s failure=%+v", evidence.Attempt.Status, f)
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
				reviewed, err := h.connector.ReadChangeReview(t.Context(), issue.ID, change.ChangeID, change.VersionID, change.HeadSHA)
				if err != nil || !reviewed.Reviewed {
					t.Fatalf("completed Rework lost current review after an earlier landing: reviewed=%t, error=%v", reviewed.Reviewed, err)
				}
			}
			if test.reconnect {
				detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
				if err != nil || len(detail.Versions) != 1 || len(h.changes(t, issue.ID)) != 1 || provider.calls != 1 {
					t.Fatalf("worker finalization did not publish once: detail=%+v calls=%d error=%v", detail, provider.calls, err)
				}
				owner := execution.(*nativeExecution).data
				var stored tracker.AttemptDiff
				if err := h.admin.client.request(t.Context(), http.MethodGet, h.admin.base()+"/attempts/"+owner.AttemptID+"/diff", nil, &stored); err != nil {
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
					if test.sourceFixture == "synthetic" {
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
				if test.rework {
					detail, err := h.admin.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), change.ChangeID)
					if err != nil {
						t.Fatal(err)
					}
					if test.reopened && (detail.Change.CurrentLanding() != nil || !reflect.DeepEqual(detail.Change.Landed, expected.Change.Landed)) {
						t.Fatal("new publication lost the historical landing or treated it as current")
					}
					if test.land && !test.lateConflict && !test.staleBase && !test.reopened && !test.importedVersion {
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
					historicalReviews := detail.Reviews
					if test.importedVersion {
						historicalReviews = detail.Reviews[:len(expected.Reviews)]
					}
					if !reflect.DeepEqual(historicalReviews, expected.Reviews) || !reflect.DeepEqual(detail.Discussion, expected.Discussion) {
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
			action = "Approve the migration\nblock_kind: missing_authorization\noptions:\n  - answer: Approve the migration"
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
	forge             *github.Client
	losePush          *bool
	retainedHead      string
	beforePublication func()
	publicationFault  func(*workspace.LandOptions) func()
}

func (e *nativePublicationFixtureExecution) SetPublicationSource(source func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)) {
	pushConfirmationLost := false
	e.nativeExecution.SetPublicationSource(func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
		if e.beforePublication != nil {
			e.beforePublication()
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
