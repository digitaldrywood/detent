package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type implementationReviewExecution struct {
	attachmentValidatorExecution
	events            *[]string
	validatorDisabled bool
	auditResult       tracker.SecurityAuditReview
	reviewResult      gate.ValidatorResult
	validatorResult   gate.ValidatorResult
	recordError       error
	commands          []gate.CommandResult
	validation        *gate.CommandResult
	discussion        NativeReviewDiscussion
	head              string
	securityAudit     *NativeSecurityAuditEvidence
	reviewSource      tracker.NativeChangeReference
}

func (e *implementationReviewExecution) RecoverChangeSource(ctx context.Context) (workspace.ChangeSource, error) {
	input, err := e.attachmentValidatorExecution.ValidatorVersion(ctx)
	if e.head != "" {
		input.Version.HeadSHA = e.head
	}
	return workspace.ChangeSource{Version: *input.Version}, err
}

func (e *implementationReviewExecution) PrepareReview(_ context.Context, source tracker.NativeChangeReference) error {
	*e.events = append(*e.events, "prepare review")
	e.reviewSource = source
	return e.recordError
}

func (e *implementationReviewExecution) ReviewVersion(context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "review version")
	input, _ := e.attachmentValidatorExecution.ValidatorVersion(context.Background())
	input.Version.Policy.Gates.AutomatedReview = gate.AutomatedReviewRequired
	input.Version.Policy.Gates.Validator = !e.validatorDisabled
	if e.head != "" {
		input.Version.HeadSHA = e.head
		input.Diff.HeadSHA = e.head
	}
	input.Commands = e.commands
	input.Discussion = e.discussion
	if e.validation != nil {
		input.Version.Validation = e.validation
	}
	return input, nil
}

func (e *implementationReviewExecution) SecurityAuditVersion(ctx context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "audit version")
	return e.attachmentValidatorExecution.ValidatorVersion(ctx)
}

func (e *implementationReviewExecution) RecordSecurityAudit(_ context.Context, result tracker.SecurityAuditReview) error {
	*e.events = append(*e.events, "record audit")
	e.auditResult = result
	return nil
}

func (e *implementationReviewExecution) RecordReview(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record review")
	if result.VersionID != "version_example" {
		return fmt.Errorf("independent review did not identify the expected immutable version: %+v", result)
	}
	e.reviewResult = result
	return e.recordError
}

func (e *implementationReviewExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	*e.events = append(*e.events, "validator version")
	input, err := e.attachmentValidatorExecution.ValidatorVersion(ctx)
	if e.head != "" {
		input.Version.HeadSHA = e.head
		input.Diff.HeadSHA = e.head
	}
	input.Commands = append(input.Commands, e.commands...)
	input.Commands = append(input.Commands, e.reviewResult.Commands...)
	input.SecurityAudit = e.securityAudit
	if e.validation != nil {
		input.Version.Validation = e.validation
	}
	return input, err
}

func (e *implementationReviewExecution) RecordValidator(_ context.Context, result gate.ValidatorResult) error {
	*e.events = append(*e.events, "record validator")
	e.validatorResult = result
	return nil
}

func TestNativeImplementationReviewSSHCallbacksRemainSeparate(t *testing.T) {
	t.Parallel()
	var events []string
	execution := &implementationReviewExecution{events: &events, commands: []gate.CommandResult{{ReceiptID: "receipt-original", Command: "make check", HeadSHA: "head", TreeSHA: "tree", DurationNS: 123}}}
	human := tracker.Actor{Kind: "human", PrincipalID: "operator"}
	execution.discussion = NativeReviewDiscussion{Issue: []tracker.NativeComment{{ID: "issue_feedback", Body: "Human edited source feedback", Actor: tracker.Actor{Kind: "runner", PrincipalID: "worker"}, EditedBy: &human, CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(200, 0).UTC(), Provenance: &tracker.Provenance{Provider: "github", ExternalID: "feedback-original", AuthorID: "author"}}}, Change: []tracker.ChangeDiscussion{{ID: "change_feedback", VersionID: "version_example", Body: "Current source discussion", Actor: human, CreatedAt: time.Unix(200, 0).UTC()}}}
	left, right := net.Pipe()
	central := NewSSHPeer(t.Context(), left, left, NewSSHCallbackHandler(RunRequest{Execution: execution}, nil, nil, nil))
	remote := NewSSHPeer(t.Context(), right, right, nil)
	t.Cleanup(func() {
		central.Close()
		remote.Close()
		left.Close()
		right.Close()
	})
	transport := &sshNativeExecution{sshExecution: &sshExecution{peer: remote}}
	source := tracker.NativeChangeReference{ChangeID: "change_example", VersionID: "version_example", HeadSHA: "head"}
	if err := transport.PrepareReview(t.Context(), source); err != nil || execution.reviewSource != source {
		t.Fatalf("SSH lost review continuation binding: %+v, %v", execution.reviewSource, err)
	}
	input, err := transport.ReviewVersion(t.Context())
	if err != nil || input.Version == nil || input.Version.ID != "version_example" {
		t.Fatalf("SSH lost immutable source-review version: %+v, %v", input, err)
	}
	if !reflect.DeepEqual(input.Commands, execution.commands) {
		t.Fatalf("SSH lost original command evidence: %+v", input.Commands)
	}
	if !reflect.DeepEqual(input.Discussion, execution.discussion) {
		t.Fatalf("SSH lost typed discussion actor/editor/version/provenance: %+v", input.Discussion)
	}
	review := gate.ValidatorResult{VersionID: input.Version.ID, HeadSHA: input.Version.HeadSHA, Verdict: gate.ValidatorVerdictWait, SessionID: 123, Submitted: true}
	if err := transport.RecordReview(t.Context(), review); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.ValidatorVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	validator := review
	validator.Verdict, validator.SessionID = gate.ValidatorVerdictPass, 124
	if err := transport.RecordValidator(t.Context(), validator); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.SecurityAuditVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	audit := tracker.SecurityAuditReview{SessionID: 125, Verdict: "pass"}
	if err := transport.RecordSecurityAudit(t.Context(), audit); err != nil {
		t.Fatal(err)
	}
	if execution.auditResult.SessionID != 125 {
		t.Fatal("SSH lost security audit result")
	}
	if !reflect.DeepEqual(execution.reviewResult, review) || !reflect.DeepEqual(execution.validatorResult, validator) {
		t.Fatalf("SSH confused source and acceptance records: source=%+v acceptance=%+v", execution.reviewResult, execution.validatorResult)
	}
	if want := []string{"prepare review", "review version", "record review", "validator version", "record validator", "audit version", "record audit"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("SSH lost separate review callbacks: got %v, want %v", events, want)
	}
	execution.recordError = ErrExecutionAuthorityUnavailable
	if err := transport.PrepareReview(t.Context(), source); !errors.Is(err, ErrExecutionAuthorityUnavailable) {
		t.Fatalf("SSH lost review continuation refusal: %v", err)
	}
}

type implementationReviewBackend struct {
	fakeCodexClient
	t                 *testing.T
	events            *[]string
	response          string
	discussion        *NativeReviewDiscussion
	securityAudit     *NativeSecurityAuditEvidence
	validatorResponse string
}

func TestNativeReviewCompletionSurvivesSlowSessionWrite(t *testing.T) {
	for _, review := range []bool{true, false} {
		name := "acceptance validation"
		if review {
			name = "implementation review"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var events []string
				head := strings.Repeat("a", 40)
				owner := &receiptReviewExecution{implementationReviewExecution: &implementationReviewExecution{events: &events, head: head}}
				input, err := owner.ReviewVersion(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				release := make(chan struct{})
				wait := &slowTurnObservation{observing: make(chan struct{}), release: release}
				sessions := &slowReviewSessionStore{fakeSessionStore: &fakeSessionStore{sessionID: 42}, wait: wait}
				proceed, published := make(chan struct{}), make(chan struct{}, 1)
				tokens := make(chan struct{})
				backend := &reviewDispatchBackend{proceed: proceed, tokens: tokens, read: make(chan bool, 1)}
				source := &implementationReviewWorkspace{attachmentValidatorWorkspace: attachmentValidatorWorkspace{reviewMutationWorkspace: reviewMutationWorkspace{
					fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
					heads:                []string{head},
				}}}
				r, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, Store: sessions, Workspace: source, AgentBackend: backend,
					Workflow: config.Workflow{Config: config.Config{Gate: gate.Config{Validator: gate.ValidatorConfig{Enabled: true}}}}})
				if err != nil {
					t.Fatal(err)
				}
				r.sleepInhibitor = nil
				finished := make(chan error, 1)
				go func() {
					_, err := r.Validate(t.Context(), ValidatorRequest{usageExecution: owner, implementationReview: review,
						NativeVersion: input.Version, Diff: input.Diff, Issue: connector.Issue{ID: "test", Identifier: "TEST-1"},
						complete: func(_ context.Context, result gate.ValidatorResult, finalization *sessionmodel.Finalization) (gate.ValidatorResult, error) {
							if finalization == nil || finalization.Finish.ProviderThreadID != "review-thread" || result.HeadSHA != head {
								return result, errors.New("missing source-bound review finalization")
							}
							published <- struct{}{}
							return result, nil
						},
					})
					finished <- err
				}()
				synctest.Wait()
				close(proceed)
				for range 5 {
					tokens <- struct{}{}
					synctest.Wait()
				}
				var readOK, publishedBeforeRelease bool
				select {
				case readOK = <-backend.read:
				default:
				}
				select {
				case <-published:
					publishedBeforeRelease = true
				default:
				}
				close(release)
				err = <-finished
				if err != nil {
					t.Fatal(err)
				}
				if len(owner.usage) != 1 || owner.usage[0].Input != 5 {
					t.Errorf("5 usage updates produced %d writes, want one final write with 5 tokens", len(owner.usage))
				}
				if !readOK || !publishedBeforeRelease {
					t.Fatalf("pinned-source read=%v review receipt published=%v before releasing telemetry", readOK, publishedBeforeRelease)
				}
			})
		})
	}
}

type slowReviewSessionStore struct {
	*fakeSessionStore
	wait *slowTurnObservation
}

func (s *slowReviewSessionStore) UpdateSessionProviderIdentity(_ context.Context, _ int64, _ store.SessionProviderIdentity) error {
	<-s.wait.release
	return nil
}

type reviewDispatchBackend struct {
	fakeCodexClient
	proceed <-chan struct{}
	tokens  <-chan struct{}
	read    chan bool
}

func (b *reviewDispatchBackend) RunTurnWithTools(ctx context.Context, _ AgentTurnRequest, _ []AgentTool, handle AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	if err := update(AgentUpdate{Type: AgentUpdateTurnStarted, ThreadID: "review-thread", TurnID: "review-turn"}); err != nil {
		return AgentTurnResult{}, err
	}
	<-b.proceed
	reply, err := handle(ctx, AgentToolCall{Name: "detent_read_source", Arguments: json.RawMessage(`{"operation":"list","revision":"head","path":"."}`)})
	b.read <- err == nil && reply.Success
	if err != nil {
		return AgentTurnResult{}, err
	}
	for i := range 5 {
		<-b.tokens
		tokens := AgentTokenUsage{InputTokens: int64(i + 1), TotalTokens: int64(i + 1)}
		if err := update(AgentUpdate{Type: AgentUpdateTokenUsage, Model: "example-model", Tokens: tokens}); err != nil {
			return AgentTurnResult{}, err
		}
	}
	err = update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: `{"verdict":"pass","score":1,"summary":"Reviewed the immutable source.","criteria_evidence":[]}`})
	return AgentTurnResult{ThreadID: "review-thread"}, err
}

type implementationReviewWorkspace struct {
	attachmentValidatorWorkspace
	commands []string
	local    *workspace.LocalGit
}

func (w *implementationReviewWorkspace) ReviewCommandEnvironment(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) gate.CheckEnvironment {
	if w.local != nil {
		return w.local.ReviewCommandEnvironment(ctx, info, issue, command)
	}
	if command != "go test ./..." {
		return gate.CheckEnvironment{}
	}
	return gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.9"}
}

func (w *implementationReviewWorkspace) ReviewCommandEvidence(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) *gate.CommandEvidence {
	if w.local != nil {
		return w.local.ReviewCommandEvidence(ctx, info, issue, command)
	}
	return &gate.CommandEvidence{Environment: w.ReviewCommandEnvironment(ctx, info, issue, command), InputsSHA: strings.Repeat("a", 64)}
}

func (w *implementationReviewWorkspace) RunReviewCommand(ctx context.Context, info workspace.Info, issue workspace.Issue, command string) (gate.CommandResult, error) {
	w.commands = append(w.commands, command)
	if w.local != nil {
		return w.local.RunReviewCommand(ctx, info, issue, command)
	}
	return gate.CommandResult{ReceiptID: fmt.Sprintf("receipt-new-%d", len(w.commands)), Command: command, HeadSHA: issue.PullRequestHeadSHA, TreeSHA: "tree", DurationNS: int64(time.Second), StartedAt: time.Now(), Evidence: w.ReviewCommandEvidence(ctx, info, issue, "go test ./...")}, nil
}

func (b *implementationReviewBackend) RunTurn(_ context.Context, request AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	if !request.ReadOnly {
		b.t.Fatal("native review received a writable turn")
	}
	*b.events = append(*b.events, request.Model)
	if request.Model == "review-model" && b.discussion != nil {
		raw, err := json.Marshal(*b.discussion)
		if err != nil || !strings.Contains(request.Prompt, string(raw)) || !strings.Contains(request.Prompt, "untrusted") {
			b.t.Fatal("independent source review did not consume typed untrusted discussion")
		}
	}
	response := b.response
	if request.Model == "validator-model" || response == "" {
		response = `{"verdict":"pass","score":1,"summary":"Current immutable source has no defects","criteria_evidence":[]}`
	}
	if request.Model == "validator-model" && b.securityAudit != nil {
		for _, identity := range []string{b.securityAudit.ReviewID, b.securityAudit.VersionID, b.securityAudit.DiffDigest, b.securityAudit.ReviewerDigest, "does not approve this validator's decision"} {
			if !strings.Contains(request.Prompt, identity) {
				b.t.Fatalf("validator lost accepted security audit context %q", identity)
			}
		}
	}
	if request.Model == "validator-model" && b.validatorResponse != "" {
		response = b.validatorResponse
	}
	return AgentTurnResult{}, update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: response})
}
