package hubclient

import (
	"bufio"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// Wire shapes. The hub's own resource structs are unexported, so this test
// decodes the contract shapes documented in docs/conversation/decisions.md
// section 5. A renamed or dropped field therefore fails here as a contract
// change rather than passing silently.

type wireOwner struct {
	PrincipalID string `json:"principal_id"`
	Subject     string `json:"subject"`
}

type wireExecution struct {
	Status       conversation.ExecutionStatus `json:"status"`
	AttemptID    *string                      `json:"attempt_id"`
	RunID        *string                      `json:"run_id"`
	RunnerID     *string                      `json:"runner_id"`
	ThreadID     *string                      `json:"thread_id"`
	TurnID       *string                      `json:"turn_id"`
	Capabilities conversation.Capabilities    `json:"capabilities"`
	Error        *string                      `json:"error"`
	UpdatedAt    time.Time                    `json:"updated_at"`
}

type wireWorkItem struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Lane        string `json:"lane"`
	RunnerBound bool   `json:"runner_bound"`
}

type wireConversation struct {
	ID             string                  `json:"id"`
	OrganizationID tracker.OrganizationID  `json:"organization_id"`
	ProjectID      tracker.ProjectID       `json:"project_id"`
	Title          string                  `json:"title"`
	Visibility     conversation.Visibility `json:"visibility"`
	Status         conversation.Status     `json:"status"`
	WorkItemID     *string                 `json:"work_item_id"`
	LinkedAt       *time.Time              `json:"linked_at"`
	WorkItem       *wireWorkItem           `json:"work_item"`
	Owner          wireOwner               `json:"owner"`
	Execution      wireExecution           `json:"execution"`
	Revision       int64                   `json:"revision"`
	EventSeq       int64                   `json:"event_seq"`
}

type wireMessage struct {
	ID             string                   `json:"id"`
	ConversationID string                   `json:"conversation_id"`
	Seq            int64                    `json:"seq"`
	Role           conversation.Role        `json:"role"`
	Kind           conversation.MessageKind `json:"kind"`
	Text           string                   `json:"text"`
	Data           json.RawMessage          `json:"data"`
	Delivery       conversation.Delivery    `json:"delivery"`
	AttemptID      *string                  `json:"attempt_id"`
	TurnID         *string                  `json:"turn_id"`
	ProviderItemID *string                  `json:"provider_item_id"`
	Actor          conversation.Actor       `json:"actor"`
	CommandKey     *string                  `json:"command_key"`
}

type wireQuestion struct {
	ID         string                      `json:"id"`
	MessageID  string                      `json:"message_id"`
	Status     conversation.QuestionStatus `json:"status"`
	Owner      conversation.Expected       `json:"owner"`
	Prompts    []conversation.Prompt       `json:"questions"`
	Answers    map[string][]string         `json:"answers"`
	AnsweredBy *string                     `json:"answered_by"`
}

type wireSnapshot struct {
	Conversation wireConversation `json:"conversation"`
	Messages     []wireMessage    `json:"messages"`
	Questions    []wireQuestion   `json:"questions"`
	Cursor       int64            `json:"cursor"`
	HasMore      bool             `json:"has_more"`
}

type wireCreated struct {
	Conversation wireConversation      `json:"conversation"`
	Receipt      *conversation.Receipt `json:"receipt"`
}

type wireLinkResult struct {
	Conversation wireConversation `json:"conversation"`
	Issue        struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		State      string `json:"state"`
	} `json:"issue"`
	Scheduling struct {
		Lane        string `json:"lane"`
		RunnerBound bool   `json:"runner_bound"`
	} `json:"scheduling"`
}

type wireError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// conversationFixture is one hub with a product project (owner, member and
// worker credentials) and a second project only an outsider can read.
type conversationFixture struct {
	service      *hubserver.Service
	server       *httptest.Server
	admin        *Client
	organization tracker.OrganizationID
	project      tracker.NativeProject
	other        tracker.NativeProject
	base         string
	otherBase    string
	owner        string
	ownerID      string
	member       string
	memberID     string
	outsider     string
	worker       string
	descriptor   policy.Descriptor
	coordinator  *scriptedCoordinator
}

const conversationFixtureAdminToken = "conversation-integration-admin"

// newConversationFixture opens the hub with the transitional hub-side
// coordinator backend: unlinked chats are answered inside the hub.
func newConversationFixture(t *testing.T) *conversationFixture {
	t.Helper()
	coordinator := newScriptedCoordinator()
	return newConversationFixtureWith(t, &hubserver.ConversationConfig{Enabled: true, Backend: coordinator, Workspace: t.TempDir()}, coordinator)
}

// newRunnerCoordinatorFixture opens the hub with the conversation product
// enabled and no coordinator backend, so an unlinked chat's message opens a
// coordinator work item for a customer runner (decisions section 9).
func newRunnerCoordinatorFixture(t *testing.T) *conversationFixture {
	t.Helper()
	return newConversationFixtureWith(t, &hubserver.ConversationConfig{Enabled: true}, nil)
}

// newGitHubConnectorConversationFixture opens the same fixture on a hub that
// can attach a GitHub repository, and attaches one to the project, the way the
// hosted browser preview fixture binds one (hubserver hosted_browser_test.go)
// and the way the eighth dogfood run's hub was set up. The project is then a
// native project with a connector: `?include=change` answers
// `connector: "github"` and no pull request is ever opened by the attempt.
func newGitHubConnectorConversationFixture(t *testing.T) *conversationFixture {
	t.Helper()
	coordinator := newScriptedCoordinator()
	f := newConversationFixtureOn(t,
		&hubserver.ConversationConfig{Enabled: true, Backend: coordinator, Workspace: t.TempDir()},
		coordinator,
		&fixedReconcileBackend{owner: "digitaldrywood", name: "detent", nodeID: "R_conversation_connector"},
	)
	f.bindGitHubRepository(t, "digitaldrywood/detent")
	return f
}

// fixedReconcileBackend answers every reconciliation with the one repository
// the fixture binds. The hub reads the binding, never GitHub, so the transport
// only has to exist and agree with itself.
type fixedReconcileBackend struct {
	owner  string
	name   string
	nodeID string
}

func (b *fixedReconcileBackend) Reconcile(_ context.Context, _ hubserver.ReconcileRequest) (hubserver.ReconcileSnapshot, error) {
	return hubserver.ReconcileSnapshot{Repository: hubserver.RepositorySource{
		NodeID: b.nodeID, Owner: b.owner, Name: b.name, UpdatedAt: time.Now().UTC(),
	}}, nil
}

// bindGitHubRepository attaches the repository to the fixture's project and
// enables the integration, which is exactly what the hosted fixture's
// `UPDATE projects SET repository_id = ?, github_repository_enabled = 1` does,
// through the API an operator would use.
func (f *conversationFixture) bindGitHubRepository(t *testing.T, repository string) {
	t.Helper()
	var integration struct {
		Revision          string `json:"revision"`
		Repository        string `json:"repository"`
		RepositoryEnabled bool   `json:"repository_enabled"`
	}
	f.expect(t, conversationFixtureAdminToken, "GET", f.base+"/integration", nil, 200, &integration)
	f.expect(t, conversationFixtureAdminToken, "POST", f.base+"/integration/repository", map[string]any{
		"idempotency_key": "bind-repository", "expected_revision": integration.Revision, "repository": repository,
	}, 200, &integration)
	f.expect(t, conversationFixtureAdminToken, "PUT", f.base+"/integration", map[string]any{
		"idempotency_key": "enable-repository", "expected_revision": integration.Revision,
		"intake": "disabled", "projection": "disabled", "repository_enabled": true,
	}, 200, &integration)
	if integration.Repository != repository || !integration.RepositoryEnabled {
		t.Fatalf("project integration = %#v, want %s enabled", integration, repository)
	}
}

func newConversationFixtureWith(t *testing.T, conversations *hubserver.ConversationConfig, coordinator *scriptedCoordinator) *conversationFixture {
	t.Helper()
	return newConversationFixtureOn(t, conversations, coordinator, nil)
}

func newConversationFixtureOn(
	t *testing.T,
	conversations *hubserver.ConversationConfig,
	coordinator *scriptedCoordinator,
	reconcile hubserver.ReconcileBackend,
) *conversationFixture {
	t.Helper()
	service, err := hubserver.Open(t.Context(), hubserver.Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		// The conversation product is exercised without the hosted shell:
		// operator tokens with project grants are the actors and the
		// /chat* client routes are not part of this slice.
		InitialAdminToken: []byte(conversationFixtureAdminToken),
		Conversation:      conversations,
		ReconcileBackend:  reconcile,
	})
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
	admin, err := New(Config{URL: server.URL, TokenSource: func() string { return conversationFixtureAdminToken }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	if err := admin.request(t.Context(), http.MethodGet, "/api/v2/organizations", nil, &organizations); err != nil {
		t.Fatal(err)
	}
	f := &conversationFixture{service: service, server: server, admin: admin, organization: organizations.Items[0].ID, coordinator: coordinator, descriptor: clientTestPolicy()}
	f.project = f.createProject(t, admin, "product")
	f.other = f.createProject(t, admin, "other")
	f.base = "/api/v2/organizations/" + string(f.organization) + "/projects/" + string(f.project.ID)
	f.otherBase = "/api/v2/organizations/" + string(f.organization) + "/projects/" + string(f.other.ID)
	f.ownerID, f.owner = f.createToken(t, admin, "owner", "operator", f.project.ID)
	f.memberID, f.member = f.createToken(t, admin, "member", "operator", f.project.ID)
	_, f.outsider = f.createToken(t, admin, "outsider", "operator", f.other.ID)
	_, f.worker = f.createToken(t, admin, "worker", "worker", f.project.ID)
	native, err := admin.Native(f.organization, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.ApproveProjectPolicy(t.Context(), policy.Change{Policy: f.descriptor}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *conversationFixture) createProject(t *testing.T, admin *Client, name string) tracker.NativeProject {
	t.Helper()
	states := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}
	var project tracker.NativeProject
	body := map[string]any{"name": name, "idempotency_key": "project-" + name, "states": states, "require_dependencies": false}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(f.organization)+"/projects", body, &project); err != nil {
		t.Fatal(err)
	}
	return project
}

func (f *conversationFixture) createToken(t *testing.T, admin *Client, name, scope string, project tracker.ProjectID) (string, string) {
	t.Helper()
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v1/tokens", map[string]string{"name": name, "scope": scope}, &token); err != nil {
		t.Fatal(err)
	}
	if err := admin.request(t.Context(), http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": f.organization, "project_id": project}, nil); err != nil {
		t.Fatal(err)
	}
	return token.ID, token.Token
}

// call performs one operator API request with the given token and returns
// the status and body. Every operator interaction in the integration test
// goes over real HTTP, as a client would.
func (f *conversationFixture) call(t *testing.T, token, method, path string, body any) (int, []byte) {
	t.Helper()
	status, payload, err := f.tryCall(token, method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, payload
}

// tryCall is call without the testing dependency, so concurrent scenarios
// can report their failure from the test goroutine.
func (f *conversationFixture) tryCall(token, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(context.Background(), method, f.server.URL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := f.server.Client().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, payload, nil
}

// expect performs a request, requires the status and decodes the body.
func (f *conversationFixture) expect(t *testing.T, token, method, path string, body any, status int, out any) {
	t.Helper()
	got, payload := f.call(t, token, method, path, body)
	if got != status {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, got, status, payload)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(payload, out); err != nil {
		t.Fatalf("decode %s %s: %v: %s", method, path, err, payload)
	}
}

// failure performs a request that must fail and returns the error body.
func (f *conversationFixture) failure(t *testing.T, token, method, path string, body any, status int) wireError {
	t.Helper()
	var failed wireError
	f.expect(t, token, method, path, body, status, &failed)
	return failed
}

// conversationCreateKeys hands every create call its own idempotency key, so
// two chats in one test are never collapsed into one stored response.
var conversationCreateKeys atomic.Int64

func (f *conversationFixture) createConversation(t *testing.T, token, title, key, text string) wireCreated {
	t.Helper()
	create := key
	if create == "" {
		create = "create-" + strconv.FormatInt(conversationCreateKeys.Add(1), 10)
	}
	body := map[string]any{"title": title, "key": create}
	if key != "" {
		body["first_message"] = map[string]any{"key": key, "text": text}
	}
	var created wireCreated
	f.expect(t, token, http.MethodPost, f.base+"/conversations", body, http.StatusCreated, &created)
	return created
}

func (f *conversationFixture) command(t *testing.T, token, id string, command conversation.Command) conversation.Receipt {
	t.Helper()
	var receipt conversation.Receipt
	f.expect(t, token, http.MethodPost, f.base+"/conversations/"+id+"/commands", command, http.StatusOK, &receipt)
	return receipt
}

func (f *conversationFixture) snapshot(t *testing.T, token, id string) wireSnapshot {
	t.Helper()
	var snapshot wireSnapshot
	f.expect(t, token, http.MethodGet, f.base+"/conversations/"+id, nil, http.StatusOK, &snapshot)
	return snapshot
}

// awaitSnapshot polls the snapshot until want is satisfied. Polling is the
// client's own recovery path: it must agree with the stream at all times.
func (f *conversationFixture) awaitSnapshot(t *testing.T, token, id string, reason string, want func(wireSnapshot) bool) wireSnapshot {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last wireSnapshot
	for time.Now().Before(deadline) {
		last = f.snapshot(t, token, id)
		if want(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	var transcript strings.Builder
	for _, message := range last.Messages {
		text := message.Text
		if len(text) > 160 {
			text = text[:160] + "…"
		}
		fmt.Fprintf(&transcript, "\n  %s/%s [%s] %q", message.Role, message.Kind, message.Delivery, text)
	}
	for _, question := range last.Questions {
		fmt.Fprintf(&transcript, "\n  question %s [%s]", question.ID, question.Status)
	}
	t.Fatalf("snapshot never satisfied %s: execution %s, %d messages%s", reason, last.Conversation.Execution.Status, len(last.Messages), transcript.String())
	return last
}

// SSE reading. Copied in miniature from internal/hubserver's stream test:
// test files cannot be imported across packages.

type sseFrame struct {
	ID    string
	Event string
	Data  string
}

type sseStream struct {
	cancel  context.CancelFunc
	body    io.ReadCloser
	updated chan struct{}
	done    chan struct{}

	mu     sync.Mutex
	frames []sseFrame
	err    error
}

func (f *conversationFixture) stream(t *testing.T, token, id string, after int64) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	url := f.server.URL + f.base + "/conversations/" + id + "/events?after=" + strconv.FormatInt(after, 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := f.server.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		cancel()
		t.Fatalf("open stream = %d: %s", response.StatusCode, payload)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("content type = %q", got)
	}
	stream := &sseStream{cancel: cancel, body: response.Body, updated: make(chan struct{}, 1), done: make(chan struct{})}
	go stream.read()
	t.Cleanup(stream.close)
	return stream
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.body.Close()
	<-s.done
}

func (s *sseStream) read() {
	defer close(s.done)
	reader := bufio.NewReader(s.body)
	for {
		frame, err := readSSEFrame(reader)
		if err != nil {
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			s.signal()
			return
		}
		if frame.Event == string(conversation.EventHeartbeat) {
			continue
		}
		s.mu.Lock()
		s.frames = append(s.frames, frame)
		s.mu.Unlock()
		s.signal()
	}
}

func (s *sseStream) signal() {
	select {
	case s.updated <- struct{}{}:
	default:
	}
}

func readSSEFrame(reader *bufio.Reader) (sseFrame, error) {
	var frame sseFrame
	seen := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return frame, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if seen {
				return frame, nil
			}
			continue
		}
		seen = true
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			frame.ID = value
		case "event":
			frame.Event = value
		case "data":
			frame.Data = value
		}
	}
}

func (s *sseStream) collected() []sseFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sseFrame(nil), s.frames...)
}

// await waits until the collected frames satisfy want, or fails.
func (s *sseStream) await(t *testing.T, reason string, want func([]sseFrame) bool) []sseFrame {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		frames := s.collected()
		if want(frames) {
			return frames
		}
		select {
		case <-s.updated:
		case <-deadline:
			t.Fatalf("stream never satisfied %s: %s", reason, describeFrames(s.collected()))
		}
	}
}

func describeFrames(frames []sseFrame) string {
	labels := make([]string, 0, len(frames))
	for _, frame := range frames {
		labels = append(labels, frame.ID+":"+frame.Event)
	}
	return strings.Join(labels, " ")
}

// deltaText concatenates every message.delta payload of the stream.
func deltaText(frames []sseFrame) string {
	var text strings.Builder
	for _, frame := range frames {
		if frame.Event != string(conversation.EventMessageDelta) {
			continue
		}
		var delta struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(frame.Data), &delta); err == nil {
			text.WriteString(delta.Text)
		}
	}
	return text.String()
}

func countFrames(frames []sseFrame, event conversation.EventType) int {
	count := 0
	for _, frame := range frames {
		if frame.Event == string(event) {
			count++
		}
	}
	return count
}

func lastFrame(t *testing.T, frames []sseFrame, event conversation.EventType, out any) sseFrame {
	t.Helper()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Event != string(event) {
			continue
		}
		if out != nil {
			if err := json.Unmarshal([]byte(frames[i].Data), out); err != nil {
				t.Fatalf("decode %s frame: %v", event, err)
			}
		}
		return frames[i]
	}
	t.Fatalf("no %s frame in %s", event, describeFrames(frames))
	return sseFrame{}
}

// executionStatuses lists the execution.updated statuses in stream order.
// executionFrame returns the first execution.updated payload with the given
// status.
func executionFrame(t *testing.T, frames []sseFrame, status conversation.ExecutionStatus) wireExecution {
	t.Helper()
	for _, frame := range frames {
		if frame.Event != string(conversation.EventExecutionUpdated) {
			continue
		}
		var execution wireExecution
		if err := json.Unmarshal([]byte(frame.Data), &execution); err != nil {
			t.Fatalf("decode execution frame: %v", err)
		}
		if execution.Status == status {
			return execution
		}
	}
	t.Fatalf("no execution.updated frame with status %q: %s", status, describeFrames(frames))
	return wireExecution{}
}

func executionStatuses(t *testing.T, frames []sseFrame) []conversation.ExecutionStatus {
	t.Helper()
	var statuses []conversation.ExecutionStatus
	for _, frame := range frames {
		if frame.Event != string(conversation.EventExecutionUpdated) {
			continue
		}
		var execution wireExecution
		if err := json.Unmarshal([]byte(frame.Data), &execution); err != nil {
			t.Fatalf("decode execution frame: %v", err)
		}
		if len(statuses) == 0 || statuses[len(statuses)-1] != execution.Status {
			statuses = append(statuses, execution.Status)
		}
	}
	return statuses
}

// Worker side.

// stubWorkspace is the smallest workspace.Backend the runner accepts. The
// conversation slice never touches a repository, so nothing is created.
type stubWorkspace struct {
	info workspace.Info
}

func (w *stubWorkspace) Create(_ context.Context, issue workspace.Issue) (workspace.Info, error) {
	info := w.info
	info.Branch = issue.BranchName
	return info, nil
}

func (w *stubWorkspace) Cleanup(context.Context, string) error { return nil }

func (w *stubWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error { return nil }

func (w *stubWorkspace) AfterRun(context.Context, workspace.Info, workspace.Issue) {}

func (w *stubWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, nil
}

// RecoveryState reports a clean workspace so the native checkpoint the run
// writes is "clean" and a later attempt is a plain fresh checkout rather
// than a manual-recovery refusal.
func (w *stubWorkspace) RecoveryState(context.Context, workspace.Info, workspace.Issue) (workspace.RecoveryState, error) {
	return workspace.RecoveryState{HeadSHA: strings.Repeat("a", 40), WorkspaceFingerprint: strings.Repeat("b", 64), BaseFingerprint: strings.Repeat("c", 64)}, nil
}

// newIssueRunnerInWorkspace builds the production issue runner over a
// caller-supplied worktree, so a test that needs the attempt to post a real
// stored diff (decisions section 18.5) can hand it a real repository and one
// that does not can hand it an empty directory.
func newIssueRunnerInWorkspace(t *testing.T, backend runner.AgentBackend, path string) *runner.Runner {
	t.Helper()
	r, err := runner.NewRunner(runner.Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Complete the linked issue"},
		Workspace:    &stubWorkspace{info: workspace.Info{Path: path, Key: "native", Branch: "native"}},
		AgentBackend: backend,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newDogfoodWorktree builds the seventh dogfood run's repository: one commit
// holding the greeting without its exclamation mark, and the fix applied in
// the worktree but not committed, which is exactly the state the run's
// `main.go | 2 +-` describes. The attempt's diff source reads it, so the
// attempt posts a real diff before run.finished and the hub records a change
// for the item.
func newDogfoodWorktree(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	greeting := filepath.Join(path, "main.go")
	if err := os.WriteFile(greeting, []byte("package main\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "dogfood@example.com"},
		{"config", "user.name", "Dogfood"},
		{"add", "main.go"},
		{"commit", "--no-gpg-sign", "-m", "add the greeting"},
	} {
		command := exec.CommandContext(t.Context(), "git", args...)
		command.Dir = path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	if err := os.WriteFile(greeting, []byte("package main\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name + \"!\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newWorkerScheduler builds the production worker client and scheduler.
func (f *conversationFixture) newWorkerScheduler(t *testing.T, machine string) *Scheduler {
	t.Helper()
	client, err := New(Config{URL: f.server.URL, TokenSource: func() string { return f.worker }, HTTPClient: f.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{
		OrganizationID:    f.organization,
		NativeProjects:    map[string]tracker.ProjectID{"local": f.project.ID},
		Machine:           Machine{ID: tracker.MachineID(machine), Hostname: "host", Capacity: 1, Version: "test"},
		HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

// claimIssue claims the issue for the scheduler and returns the candidate.
func (f *conversationFixture) claimIssue(t *testing.T, scheduler *Scheduler, issueID string) connector.Issue {
	t.Helper()
	candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: f.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != issueID {
		t.Fatalf("candidates = %#v, want the linked issue %s", candidates, issueID)
	}
	if _, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	return candidates[0]
}

type attemptOutcome struct {
	result runner.RunResult
	err    error
}

// startAttempt drives the production runner over one claimed native issue in
// the background, exactly as the orchestrator's dispatch path does.
func (f *conversationFixture) startAttempt(t *testing.T, scheduler *Scheduler, backend runner.AgentBackend, issue connector.Issue) <-chan attemptOutcome {
	t.Helper()
	return f.startAttemptInWorkspace(t, scheduler, backend, issue, t.TempDir())
}

// startAttemptInWorkspace is startAttempt over a caller-supplied worktree, so
// the attempt's stored diff is the worktree's own.
func (f *conversationFixture) startAttemptInWorkspace(
	t *testing.T,
	scheduler *Scheduler,
	backend runner.AgentBackend,
	issue connector.Issue,
	path string,
) <-chan attemptOutcome {
	t.Helper()
	execution := scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("claimed native issue has no execution lifecycle")
	}
	agent := newIssueRunnerInWorkspace(t, backend, path)
	request := runner.RunRequest{Execution: execution, ProjectID: "local", Issue: issue, Mode: runner.RunModePlan}
	ctx := t.Context()
	done := make(chan attemptOutcome, 1)
	go func() {
		result, err := agent.Run(ctx, request)
		done <- attemptOutcome{result: result, err: err}
	}()
	return done
}

// awaitAttempt waits for a background attempt to finish.
func awaitAttempt(t *testing.T, done <-chan attemptOutcome) runner.RunResult {
	t.Helper()
	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("attempt error = %v", outcome.err)
		}
		return outcome.result
	case <-time.After(60 * time.Second):
		t.Fatal("attempt did not finish")
	}
	return runner.RunResult{}
}

// scriptedCoordinator answers unlinked conversations with a short streamed
// reply that echoes the user's own message, so a reply landing in the wrong
// conversation is visible rather than indistinguishable.
type scriptedCoordinator struct {
	mu      sync.Mutex
	prompts []string
	turns   int
}

func newScriptedCoordinator() *scriptedCoordinator { return &scriptedCoordinator{} }

func (c *scriptedCoordinator) RunTurn(_ context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	c.mu.Lock()
	c.prompts = append(c.prompts, request.Prompt)
	turn := c.turns
	c.turns++
	c.mu.Unlock()
	for _, delta := range []string{"Coordinator reply to ", lastPromptLine(request.Prompt)} {
		if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: delta}); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	return runner.AgentTurnResult{ThreadID: "coordinator-thread", TurnID: fmt.Sprintf("coordinator-turn-%d", turn+1)}, nil
}

func (c *scriptedCoordinator) recordedPrompts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.prompts...)
}

// lastPromptLine is the newest user message: coordinatorPrompt appends the
// pending messages after the transcript block.
func lastPromptLine(prompt string) string {
	lines := strings.Split(strings.TrimSpace(prompt), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// scriptedLiveBackend is the worker-side provider: it streams a turn, asks
// one question, waits for the authorized answer, accepts a steer and
// completes. A second run must resume the first run's thread and see the
// queued continuation text in its prompt.
type scriptedLiveBackend struct {
	mu       sync.Mutex
	requests []runner.AgentTurnRequest
	consumed []runner.AgentControl
	choice   string
}

const (
	scriptedThreadID       = "thread-live-1"
	scriptedControlWait    = 30 * time.Second
	scriptedFirstQuestion  = `[{"id":"approach","header":"Approach","question":"Which approach should I take?","options":[{"label":"Incremental","description":"Small steps"},{"label":"Rewrite","description":"Start over"}]}]`
	scriptedSecondQuestion = `[{"id":"ship","header":"Ship","question":"Ship the change?","options":[{"label":"Yes","description":"Ship"},{"label":"No","description":"Hold"}]}]`
)

func (*scriptedLiveBackend) SupportsLiveControl() bool { return true }

func (b *scriptedLiveBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.mu.Lock()
	b.requests = append(b.requests, request)
	attempt := len(b.requests)
	b.mu.Unlock()
	if request.ConversationControl == nil {
		return runner.AgentTurnResult{}, errors.New("turn ran without a conversation control")
	}
	if attempt == 1 {
		return b.firstTurn(ctx, request, onUpdate)
	}
	return b.continuationTurn(ctx, request, onUpdate)
}

func (b *scriptedLiveBackend) firstTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	const turnID = "turn-1"
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: scriptedThreadID, TurnID: turnID}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := b.stream(onUpdate, scriptedThreadID, turnID, "item-1", "Reading the issue. ", "Planning the change. ", "One question first. "); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := request.ConversationControl.InputRequested(runner.AgentInputRequest{ID: "req-1", ThreadID: scriptedThreadID, TurnID: turnID, Questions: json.RawMessage(scriptedFirstQuestion)}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	answer, err := b.consume(ctx, request, runner.AgentControlAnswer)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	choice := "unknown"
	if values := answer.Answers["approach"]; len(values) > 0 {
		choice = values[0]
	}
	b.mu.Lock()
	b.choice = choice
	b.mu.Unlock()
	if err := b.stream(onUpdate, scriptedThreadID, turnID, "item-2", "Taking the "+choice+" approach. ", "Working on it. "); err != nil {
		return runner.AgentTurnResult{}, err
	}
	steer, err := b.consume(ctx, request, runner.AgentControlMessage)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := b.stream(onUpdate, scriptedThreadID, turnID, "item-3", "Noted: "+steer.Text); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: scriptedThreadID, TurnID: turnID, Status: "completed"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: scriptedThreadID, TurnID: turnID}, nil
}

func (b *scriptedLiveBackend) continuationTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	const turnID = "turn-2"
	thread := request.Resume.ThreadID
	if thread == "" {
		thread = scriptedThreadID
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnStarted, ThreadID: thread, TurnID: turnID}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := b.stream(onUpdate, thread, turnID, "item-4", "Resuming the same thread. "); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := request.ConversationControl.InputRequested(runner.AgentInputRequest{ID: "req-2", ThreadID: thread, TurnID: turnID, Questions: json.RawMessage(scriptedSecondQuestion)}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	answer, err := b.consume(ctx, request, runner.AgentControlAnswer)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	choice := "unknown"
	if values := answer.Answers["ship"]; len(values) > 0 {
		choice = values[0]
	}
	if err := b.stream(onUpdate, thread, turnID, "item-5", "Shipping decision: "+choice+". "); err != nil {
		return runner.AgentTurnResult{}, err
	}
	if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateTurnCompleted, ThreadID: thread, TurnID: turnID, Status: "completed"}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{ThreadID: thread, TurnID: turnID}, nil
}

func (b *scriptedLiveBackend) stream(onUpdate runner.AgentUpdateHandler, thread, turn, item string, deltas ...string) error {
	for _, delta := range deltas {
		update := runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ThreadID: thread, TurnID: turn, ItemID: item, Delta: delta}
		if err := onUpdate(update); err != nil {
			return err
		}
	}
	return nil
}

// consume waits for the next control of the wanted kind, runs its ownership
// check exactly as a real transport does immediately before writing to the
// provider, and settles its reply.
func (b *scriptedLiveBackend) consume(ctx context.Context, request runner.AgentTurnRequest, kind runner.AgentControlKind) (runner.AgentControl, error) {
	deadline := time.After(scriptedControlWait)
	for {
		select {
		case command, ok := <-request.ConversationControl.Commands:
			if !ok {
				return runner.AgentControl{}, errors.New("conversation control queue closed")
			}
			if err := command.Validate(); err != nil {
				command.Reply <- err
				return runner.AgentControl{}, err
			}
			err := command.Check(ctx)
			command.Reply <- err
			if err != nil {
				return runner.AgentControl{}, err
			}
			b.mu.Lock()
			b.consumed = append(b.consumed, command)
			b.mu.Unlock()
			if command.Kind == kind {
				return command, nil
			}
		case <-ctx.Done():
			return runner.AgentControl{}, ctx.Err()
		case <-deadline:
			return runner.AgentControl{}, fmt.Errorf("no %s control arrived within %s", kind, scriptedControlWait)
		}
	}
}

func (b *scriptedLiveBackend) recordedRequests() []runner.AgentTurnRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runner.AgentTurnRequest(nil), b.requests...)
}

func (b *scriptedLiveBackend) recordedControls() []runner.AgentControl {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runner.AgentControl(nil), b.consumed...)
}

// Runner-dispatched coordinator (decisions section 9). The helpers below
// enroll real runner identities, claim through the production provider
// capacity path and dispatch runs the way internal/orchestrator/dispatch.go
// does.

// coordinatorItemLabel marks a hub-created coordinator work item. It repeats
// internal/orchestrator/dispatch.go's own unexported constant, which is what
// selects the coordinator run mode in production.
const coordinatorItemLabel = "detent:coordinator"

// harnessDispatchMode mirrors (*Orchestrator).dispatchMode: the coordinator
// label wins over every workflow state. dispatchMode and issueIsCoordinatorItem
// are unexported in internal/orchestrator, so the rule is mirrored here rather
// than called. Ordinary issues run in plan mode, as the O01 slice dispatches
// them.
func harnessDispatchMode(issue connector.Issue) string {
	for _, label := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(label), coordinatorItemLabel) {
			return runner.RunModeCoordinator
		}
	}
	return runner.RunModePlan
}

// refusingWorkspace is a workspace.Backend that fails every Create. A
// coordinator run must never reach the git workspace backend: it runs in a
// temporary directory the runner owns (decisions section 9.4).
type refusingWorkspace struct {
	mu      sync.Mutex
	created bool
}

func (w *refusingWorkspace) Create(context.Context, workspace.Issue) (workspace.Info, error) {
	w.mu.Lock()
	w.created = true
	w.mu.Unlock()
	return workspace.Info{}, errors.New("a coordinator run must not create a git workspace")
}

func (w *refusingWorkspace) Cleanup(context.Context, string) error { return nil }

func (w *refusingWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (w *refusingWorkspace) AfterRun(context.Context, workspace.Info, workspace.Issue) {}

func (w *refusingWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, nil
}

func (w *refusingWorkspace) RecoveryState(context.Context, workspace.Info, workspace.Issue) (workspace.RecoveryState, error) {
	return workspace.RecoveryState{}, errors.New("a coordinator run has no git recovery state")
}

func (w *refusingWorkspace) reachedGit() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.created
}

// conversationRunner is one enrolled customer runner: its own identity file,
// hub client, provider report and production scheduler.
type conversationRunner struct {
	fixture   *conversationFixture
	client    *Client
	scheduler *Scheduler
	identity  runnerauth.Identity
	machine   Machine
	alias     string
}

// enrollRunner enrolls a runner identity with the operations the coordinator
// path needs and gives it a live-control (codex) provider report. Enrollment
// goes through the production client: CreateRunnerEnrollment, EnrollRunner and
// a hub client bound to the identity file. The provider report is republished
// on every heartbeat by the scheduler itself, so it is always fresh.
func (f *conversationFixture) enrollRunner(t *testing.T, alias string) *conversationRunner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := f.admin.CreateRunnerEnrollment(t.Context(), f.organization, runnerauth.EnrollmentRequest{
		Binding:    file.Identity.Binding,
		ProjectIDs: []tracker.ProjectID{f.project.ID},
		Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events},
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{ID: file.Identity.MachineID, Hostname: "customer-" + alias, DisplayName: "Runner " + alias, Capacity: 2, Version: "test"}
	identity, err := EnrollRunner(t.Context(), path, f.organization, enrollment.Token, machine)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: f.server.URL, IdentityFile: path, HTTPClient: f.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(client, SchedulerConfig{
		OrganizationID: f.organization,
		NativeProjects: map[string]tracker.ProjectID{"local": f.project.ID},
		Machine:        machine,
		// The report names codex, the only backend the hub accepts for a
		// live coordinator turn, and provider_default, the model a run with
		// no configured route resolves to.
		ProviderReports: func() ([]providercapacity.Report, error) {
			return []providercapacity.Report{{
				Provider: "openai", Backend: "codex", AccountAlias: alias, Models: []string{"provider_default"},
				MaxConcurrent: 4, Availability: "available", ObservedAt: time.Now().UTC(),
			}}, nil
		},
		HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conversationRunner{fixture: f, client: client, scheduler: scheduler, identity: identity, machine: machine, alias: alias}
}

// newCoordinatorRunner builds the production runner for a coordinator turn
// over a workspace backend that refuses to create anything.
func newCoordinatorRunner(t *testing.T, backend runner.AgentBackend) (*runner.Runner, *refusingWorkspace) {
	t.Helper()
	git := &refusingWorkspace{}
	r, err := runner.NewRunner(runner.Dependencies{
		Workflow:     config.Workflow{Config: config.Config{}, Prompt: "Complete the linked issue"},
		Workspace:    git,
		AgentBackend: backend,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, git
}

// coordinatorSchedulingRequest is the scheduling request the orchestrator
// issues for this project. The provider requirement is resolved by the runner
// itself, exactly as internal/orchestrator/tick.go resolves it before a claim,
// and over the same dispatch mode the run will use.
func coordinatorSchedulingRequest(f *conversationFixture, agent *runner.Runner) orchestrator.SchedulingRequest {
	return orchestrator.SchedulingRequest{
		Policy: f.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo"},
		ProviderRequirement: func(ctx context.Context, issue connector.Issue) (providercapacity.Requirement, error) {
			return agent.DispatchCapacity(ctx, runner.RunRequest{Issue: issue, Mode: harnessDispatchMode(issue)})
		},
	}
}

// candidates runs one production claim cycle.
func (r *conversationRunner) candidates(t *testing.T, agent *runner.Runner) []connector.Issue {
	t.Helper()
	issues, err := r.scheduler.FetchCandidateIssues(t.Context(), coordinatorSchedulingRequest(r.fixture, agent))
	if err != nil {
		t.Fatal(err)
	}
	return issues
}

// claim claims the named work item and adopts the claim.
func (r *conversationRunner) claim(t *testing.T, agent *runner.Runner, workItemID string) connector.Issue {
	t.Helper()
	issues := r.candidates(t, agent)
	if len(issues) != 1 || issues[0].ID != workItemID {
		t.Fatalf("claimed %#v, want the single work item %s", issues, workItemID)
	}
	if _, err := r.scheduler.AdoptClaim(t.Context(), issues[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	return issues[0]
}

// release ends the claim the way the orchestrator does after a run.
func (r *conversationRunner) release(t *testing.T, issueID, reason string) {
	t.Helper()
	if err := r.scheduler.ReleaseClaim(t.Context(), issueID, reason); err != nil {
		t.Fatal(err)
	}
}

// start dispatches one claimed work item through the production runner. The
// request is built the way internal/orchestrator/dispatch.go builds it: the
// mode comes from the coordinator label and a coordinator run is handed the
// scheduler's bounded hub reader instead of the writing tools.
func (r *conversationRunner) start(t *testing.T, agent *runner.Runner, issue connector.Issue) <-chan attemptOutcome {
	t.Helper()
	execution := r.scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("claimed native issue has no execution lifecycle")
	}
	request := runner.RunRequest{Execution: execution, ProjectID: "local", Issue: issue, Mode: harnessDispatchMode(issue)}
	if request.Mode == runner.RunModeCoordinator {
		if reader := r.scheduler.CoordinatorReader(issue.ID); reader != nil {
			request.Coordinator = &runner.CoordinatorRequest{Reader: reader, ProjectID: r.scheduler.CoordinatorProject(issue.ID)}
		}
	}
	ctx := t.Context()
	done := make(chan attemptOutcome, 1)
	go func() {
		result, err := agent.Run(ctx, request)
		done <- attemptOutcome{result: result, err: err}
	}()
	return done
}

// awaitOutcome waits for a background attempt and returns its outcome,
// including the error, so a scenario can assert on a refused or interrupted
// run rather than failing on it.
func awaitOutcome(t *testing.T, done <-chan attemptOutcome) attemptOutcome {
	t.Helper()
	select {
	case outcome := <-done:
		return outcome
	case <-time.After(60 * time.Second):
		t.Fatal("attempt did not finish")
	}
	return attemptOutcome{}
}

// coordinatorItems lists the project's coordinator work items through the
// operator API. Without include=coordinator they are hidden.
func (f *conversationFixture) coordinatorItems(t *testing.T, token string, include bool) []tracker.NativeIssue {
	t.Helper()
	path := f.base + "/work-items?limit=50"
	if include {
		path += "&include=coordinator"
	}
	var page tracker.Page[tracker.NativeIssue]
	f.expect(t, token, http.MethodGet, path, nil, http.StatusOK, &page)
	if !include {
		return page.Items
	}
	items := make([]tracker.NativeIssue, 0, len(page.Items))
	for _, item := range page.Items {
		if slices.Contains(item.Labels, coordinatorItemLabel) {
			items = append(items, item)
		}
	}
	return items
}

// awaitCoordinatorItem waits until the conversation has exactly one open
// coordinator item and returns it. A closed item is in a terminal state, so
// "open" is what the operator API can observe.
func (f *conversationFixture) awaitCoordinatorItem(t *testing.T, conversationID string) tracker.NativeIssue {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var open []tracker.NativeIssue
		for _, item := range f.coordinatorItems(t, f.owner, true) {
			if !item.Terminal && strings.Contains(item.Body, "Conversation: "+conversationID) {
				open = append(open, item)
			}
		}
		if len(open) == 1 {
			return open[0]
		}
		if len(open) > 1 {
			t.Fatalf("conversation %s has %d open coordinator items, want exactly one", conversationID, len(open))
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("conversation %s never opened a coordinator item", conversationID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// coordinatorItemState reads one coordinator item's workflow state.
func (f *conversationFixture) coordinatorItemState(t *testing.T, workItemID string) (string, bool) {
	t.Helper()
	for _, item := range f.coordinatorItems(t, f.owner, true) {
		if string(item.WorkItemID) == workItemID {
			return item.State, item.Terminal
		}
	}
	t.Fatalf("coordinator item %s is not listed with include=coordinator", workItemID)
	return "", false
}
