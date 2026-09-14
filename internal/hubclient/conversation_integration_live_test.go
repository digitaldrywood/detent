package hubclient

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

// conversationLiveCodeword is what the model is asked to remember across
// the attempt boundary. A continuation that lost the provider thread cannot
// produce it.
const conversationLiveCodeword = "amber-otter"

const conversationLiveInstructions = "You are testing a conversation. Use no shell, filesystem, network, MCP or delegation tools. " +
	"You may use request_user_input. Follow the latest user message. Do not perform repository work. Answer briefly."

// codexLiveBackend runs the production Codex backend with the isolated,
// read-only turn settings of the live test while leaving the prompt, the
// resume state and the conversation control to the production runner.
type codexLiveBackend struct {
	backend *codex.AgentBackend
	base    runner.AgentTurnRequest

	mu       sync.Mutex
	requests []runner.AgentTurnRequest
}

func (b *codexLiveBackend) SupportsLiveControl() bool { return b.backend.SupportsLiveControl() }

func (b *codexLiveBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	live := b.base
	live.Prompt = request.Prompt
	live.Resume = request.Resume
	live.ConversationControl = request.ConversationControl
	b.mu.Lock()
	b.requests = append(b.requests, live)
	b.mu.Unlock()
	return b.backend.RunTurn(ctx, live, onUpdate)
}

func (b *codexLiveBackend) recordedRequests() []runner.AgentTurnRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]runner.AgentTurnRequest(nil), b.requests...)
}

// TestConversationIntegrationLiveCodex is the opt-in variant of the vertical
// slice against a real Codex provider. It asserts the same contract as the
// scripted test where the model is deterministic — a real question reaches
// the conversation, an authorized answer resumes the turn, and a second
// attempt resumes the same provider thread — and asserts the model's own
// output only through the codeword it was asked to memorize.
//
// It never touches the repository: Codex runs with its own CODEX_HOME, a
// read-only sandbox, approvals disabled and tool instructions that forbid
// filesystem, shell and network work.
func TestConversationIntegrationLiveCodex(t *testing.T) {
	if os.Getenv("DETENT_CONVERSATION_LIVE") != "1" {
		t.Skip("live Codex conversation test is opt-in: set DETENT_CONVERSATION_LIVE=1 and log in with the codex CLI")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("the live conversation test requires the codex CLI on PATH: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatalf("Codex login required: %v", err)
	}
	isolated := t.TempDir()
	codexHome := filepath.Join(isolated, "codex")
	provider := filepath.Join(isolated, "workspace")
	for _, directory := range []string{codexHome, provider} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		// The runner appends the same feature (internal/cli/runner.go): Codex
		// lists request_user_input in its default mode only behind it.
		return exec.CommandContext(ctx, binary, "app-server", "--stdio", "--enable", "default_mode_request_user_input")
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := codex.NewAppServer(factory, codex.WithReadTimeout(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := codex.NewAgentBackend(app, codex.Options{ApprovalPolicy: "never", ThreadSandbox: "read-only", StallTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	backend := &codexLiveBackend{backend: agent, base: runner.AgentTurnRequest{
		Workspace: provider, TempDir: isolated, ReadOnly: true,
		Model: "gpt-6-astra", ReasoningEffort: "low", ToolInstructions: conversationLiveInstructions,
		Environment: procgroup.Environment{Variables: map[string]string{"CODEX_HOME": codexHome}},
		TurnTimeout: 3 * time.Minute, MaxDuration: 5 * time.Minute,
	}}

	f := newConversationFixture(t)
	created := f.createConversation(t, f.owner, "Live conversation slice", "", "")
	id := created.Conversation.ID
	var link wireLinkResult
	f.expect(t, f.owner, http.MethodPost, f.base+"/conversations/"+id+"/link", map[string]any{
		"key": "live-link", "share_history": true,
		"issue": map[string]any{"title": "Live conversation slice", "description": "A conversation smoke test. Do no repository work."},
	}, http.StatusOK, &link)

	// The instruction is queued as a conversation message before any runner
	// exists, so the production prompt path carries it into the first turn.
	f.command(t, f.owner, id, conversation.Command{Key: "live-brief", Kind: conversation.CommandMessage, Text: "" +
		"Remember the codeword " + conversationLiveCodeword + ". " +
		"Use request_user_input now to ask whether to take approach Blue or Green, and wait for my answer. " +
		"After I answer, reply with one short sentence naming the colour I chose. Do not use any other tool."})

	stream := f.stream(t, f.owner, id, 0)
	// An enrolled runner identity, as every production runner has: the thread
	// the first attempt creates is handed back to the second attempt only
	// because the same runner produced it (decisions section 9.3). A legacy
	// machine credential would get a transcript instead, which the scripted
	// integration test covers.
	live := f.enrollRunner(t, "live")
	dispatcher, _ := newCoordinatorRunner(t, backend)
	issue := live.claim(t, dispatcher, link.Issue.ID)
	first := f.startAttempt(t, live.scheduler, backend, issue)

	waiting := f.awaitSnapshot(t, f.owner, id, "the live question", func(s wireSnapshot) bool {
		for _, question := range s.Questions {
			if question.Status == conversation.QuestionPending {
				return true
			}
		}
		return false
	})
	question := waiting.Questions[len(waiting.Questions)-1]
	firstAttempt := *waiting.Conversation.Execution.AttemptID
	answers := map[string][]string{}
	for _, prompt := range question.Prompts {
		answers[prompt.ID] = []string{"Blue"}
	}
	// The pending question outlives the provider's ordinary stall timeout:
	// a human wait is not a stall.
	time.Sleep(16 * time.Second)
	if receipt := f.command(t, f.member, id, conversation.Command{
		Key: "live-answer", Kind: conversation.CommandAnswer, QuestionID: question.ID, Answers: answers,
	}); receipt.QuestionID != question.ID {
		t.Fatalf("live answer receipt = %#v", receipt)
	}
	stream.await(t, "the reply that names the chosen colour", func(frames []sseFrame) bool {
		return strings.Contains(strings.ToLower(deltaText(frames)), "blue")
	})
	if result := awaitAttempt(t, first); result.FinalState != runner.FinalStateCompleted {
		t.Fatalf("live attempt final state = %q", result.FinalState)
	}
	live.release(t, link.Issue.ID, "completed")
	completed := f.awaitSnapshot(t, f.owner, id, "the completed live execution", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionCompleted
	})
	if completed.Conversation.Execution.ThreadID == nil || *completed.Conversation.Execution.ThreadID == "" {
		t.Fatalf("live execution kept no provider thread: %#v", completed.Conversation.Execution)
	}
	thread := *completed.Conversation.Execution.ThreadID

	// Continuation on a new attempt: the same provider thread, and the
	// model still remembers what it was told before the attempt ended.
	f.command(t, f.owner, id, conversation.Command{Key: "live-continue", Kind: conversation.CommandContinue,
		Text:     "What codeword did I ask you to remember? Reply only that word.",
		Expected: conversation.Expected{AttemptID: firstAttempt}})
	nextIssue := live.claim(t, dispatcher, link.Issue.ID)
	second := f.startAttempt(t, live.scheduler, backend, nextIssue)
	if result := awaitAttempt(t, second); result.FinalState != runner.FinalStateCompleted {
		t.Fatalf("live continuation final state = %q", result.FinalState)
	}
	live.release(t, link.Issue.ID, "completed")
	final := f.awaitSnapshot(t, f.owner, id, "the completed live continuation", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionCompleted &&
			s.Conversation.Execution.AttemptID != nil && *s.Conversation.Execution.AttemptID != firstAttempt
	})
	requests := backend.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("live provider turns = %d, want one per attempt", len(requests))
	}
	if requests[1].Resume.ThreadID != thread {
		t.Fatalf("live continuation resume = %q, want the first attempt's thread %q", requests[1].Resume.ThreadID, thread)
	}
	if !strings.Contains(requests[1].Prompt, "What codeword did I ask you to remember?") {
		t.Fatalf("live continuation prompt lost the continue text: %q", requests[1].Prompt)
	}
	secondAttempt := *final.Conversation.Execution.AttemptID
	var recalled strings.Builder
	for _, message := range final.Messages {
		if message.Role == conversation.RoleAssistant && message.AttemptID != nil && *message.AttemptID == secondAttempt {
			recalled.WriteString(message.Text)
		}
	}
	if !strings.Contains(strings.ToLower(recalled.String()), conversationLiveCodeword) {
		t.Fatalf("live continuation lost the provider context: %q", recalled.String())
	}
}
