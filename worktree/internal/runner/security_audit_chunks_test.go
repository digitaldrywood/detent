package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/store"
)

type chunkAuditBackend struct {
	fakeCodexClient
	t               *testing.T
	mode            string
	requests        []AgentTurnRequest
	deadline        time.Time
	cancel          context.CancelFunc
	covered         strings.Builder
	initialCoverage string
	sequenceStart   int
}

func (b *chunkAuditBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.t.Helper()
	b.requests = append(b.requests, req)
	_, raw, ok := strings.Cut(req.Prompt, "\n\nAudit payload:\n")
	var input struct {
		Verification *securityaudit.Result       `json:"verification_of"`
		Chunk        securityaudit.ChunkIdentity `json:"chunk"`
		Diff         string                      `json:"textual_diff"`
	}
	if ok && json.Unmarshal([]byte(raw), &input) == nil && input.Verification != nil && input.Chunk.Index == 0 {
		b.initialCoverage = b.covered.String()
		b.covered.Reset()
		b.sequenceStart = len(b.requests) - 1
	}
	if !ok || json.Unmarshal([]byte(raw), &input) != nil || input.Chunk.Count <= 1 || input.Chunk.Index != len(b.requests)-1-b.sequenceStart || len(req.Prompt) > securityaudit.DefaultMaxDiffBytes {
		b.t.Fatal("audit did not supply an ordered bounded source chunk")
	}
	if input.Chunk.Start != b.covered.Len() {
		b.t.Fatal("source bytes skipped or duplicated")
	}
	b.covered.WriteString(input.Diff)
	if input.Chunk.Index > 0 && req.Resume.ThreadID != "audit-thread" {
		b.t.Fatal("cross-file context was not retained in the provider conversation")
	}
	if _, err := os.Stat(req.Workspace); err != nil {
		b.t.Fatalf("missing per-turn workspace: %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok || (!b.deadline.IsZero() && deadline.After(b.deadline)) {
		b.t.Fatal("chunk reset the overall audit timeout")
	}
	b.deadline = deadline
	if !req.ReadOnly || req.SupplementalTools || len(req.ExtraWritableRoots) != 0 || !strings.Contains(req.ToolInstructions, "Use no tools") {
		b.t.Fatal("chunk weakened auditor isolation")
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", serviceapi.AddressEnvironment, serviceapi.TokenEnvironment, serviceapi.DispositionTokenEnvironment} {
		if req.Environment.Variables[key] != "" {
			b.t.Fatalf("chunk exposed %s", key)
		}
	}
	result := AgentTurnResult{ThreadID: "audit-thread", SessionID: fmt.Sprintf("audit-thread-turn-%d", input.Chunk.Index), AuthenticationMode: "api_key"}
	if b.mode == "reported cost" {
		result.ReportedCostMicros = new(int64(1000 * (input.Chunk.Index + 1)))
		result.CostSource = "provider"
	}
	if b.mode == "missing conversation" {
		result.ThreadID, result.SessionID = "", ""
	}
	if b.mode != "missing process" {
		if err := update(AgentUpdate{Type: AgentUpdateProcessStarted, WorkerProcess: procgroup.Identity{PID: 4242, GroupID: 4242, StartedAt: time.Now().UTC()}}); err != nil {
			return result, err
		}
	}
	tokens := AgentTokenUsage{TotalTokens: 10}
	if b.mode == "reported cost" {
		if err := update(AgentUpdate{Type: AgentUpdateTokenUsage, Tokens: AgentTokenUsage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5}}); err != nil {
			return result, err
		}
		tokens.InputTokens, tokens.OutputTokens = 8, 2
	}
	if b.mode == "cumulative usage" {
		tokens.ThreadTotal = &AgentTokenCounts{TotalTokens: int64(10 * len(b.requests))}
	}
	if err := update(AgentUpdate{Type: AgentUpdateTokenUsage, Tokens: tokens}); err != nil {
		return result, err
	}
	output := `{"verdict":"pass","summary":"reviewed","findings":[]}`
	if strings.HasPrefix(b.mode, "chunked verification") {
		if input.Verification == nil && input.Chunk.Index == 0 {
			output = `{"verdict":"undetermined","summary":"Potential p2","findings":[{"id":"lead","certainty":"undetermined","body":"Potential cross-file bypass","path":"a.go","unresolved_fact":"Whether caller authenticates"}]}`
		}
		if input.Verification != nil {
			if input.Chunk.Index == 0 && (!agentResumeEmpty(req.Resume) || req.Model != "complex-model") {
				b.t.Fatal("chunked verification resumed lower-model conversation")
			}
			result := securityaudit.Result{Verdict: securityaudit.VerdictPass, Summary: "Caller enforces authorization", Findings: append([]securityaudit.Finding(nil), input.Verification.Findings...)}
			for index := range result.Findings {
				result.Findings[index].Certainty = securityaudit.CertaintyDisproved
				result.Findings[index].Status = "rejected"
				result.Findings[index].UnresolvedFact = ""
				result.Findings[index].Body = "Caller enforces authorization"
			}
			decisive := b.mode == "chunked verification" || strings.Contains(b.mode, "late") && input.Chunk.Index == input.Chunk.Count-1 || strings.Contains(b.mode, "early") && input.Chunk.Index == 0
			if !decisive {
				result = *input.Verification
			}
			if strings.Contains(b.mode, "confirmation") && decisive {
				result.Verdict = securityaudit.VerdictFail
				result.Summary = "Caller reaches unchecked helper"
				for index := range result.Findings {
					result.Findings[index].Certainty = securityaudit.CertaintyConfirmed
					result.Findings[index].Severity = "p1"
					result.Findings[index].Status = "unresolved"
					result.Findings[index].Body = "Caller reaches unchecked helper"
				}
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				b.t.Fatal(err)
			}
			output = string(encoded)
		}
	}
	last := input.Chunk.Index == input.Chunk.Count-1
	if b.mode == "early finding" && input.Chunk.Index == 0 || b.mode == "late finding" && last {
		output = `{"verdict":"fail","summary":"Cross-file authorization bypass","findings":[{"id":"auth","certainty":"confirmed","severity":"p1","body":"Caller in a.go reaches unchecked helper in b.go","path":"b.go","line":3}]}`
	}
	if input.Chunk.Index > 0 {
		switch b.mode {
		case "provider failure":
			return result, errors.New("provider unavailable")
		case "malformed":
			output = "invalid"
		case "mismatched conversation":
			result.ThreadID = "other-thread"
		case "tool use":
			if err := update(AgentUpdate{Type: AgentUpdateToolStarted}); err != nil {
				return result, err
			}
		case "timeout":
			<-ctx.Done()
			return result, ctx.Err()
		case "cancelled":
			b.cancel()
			return result, ctx.Err()
		}
	}
	if err := update(AgentUpdate{Type: AgentUpdateMessageDelta, Delta: output}); err != nil {
		return result, err
	}
	return result, nil
}

func TestRunnerChunkedAudit(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"pass", "chunked verification", "chunked verification late confirmation", "chunked verification late rejection", "chunked verification early confirmation", "chunked verification early rejection", "cumulative usage", "reported cost", "early finding", "late finding", "provider failure", "malformed", "mismatched conversation", "missing conversation", "missing process", "tool use", "cancelled", "usage budget", "resume unavailable", "timeout", "process reap failure", "artifact reap failure"} {
		t.Run(mode, func(t *testing.T) {
			run := func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				backend := &chunkAuditBackend{t: t, mode: mode, cancel: cancel}
				if mode == "resume unavailable" {
					backend.verifyErr = ErrAgentResumeUnsupported
				}
				cfg := config.Default()
				cfg.Agents.ModelSelection.Enabled = new(false)
				if strings.HasPrefix(mode, "chunked verification") {
					cfg.Agents.ModelSelection.ComplexModel = new("complex-model")
					backend.models = []AgentModel{{ID: "complex-model", Model: "complex-model", SupportedReasoningEfforts: []string{"low", "medium", "high"}}}
				}
				cfg.Agents.Backends = []config.AgentBackend{{ID: "audit", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "test-audit"}}
				cfg.Agents.Routes = []config.AgentRoute{{Name: "audit", Role: RoleSecurityAudit, Backend: "audit", Model: "audit-model", Default: true}}
				sessionStore := &scratchReapSessionStore{fakeSessionStore: &fakeSessionStore{sessionID: 73}, process: store.WorkerProcess{SessionID: 73}}
				reapFailure := errors.New("audit process reap failed")
				auditRoot := t.TempDir()
				runner, err := NewRunner(Dependencies{ProcessSource: emptyProcessSource, ProjectID: "project", Workflow: config.Workflow{Config: cfg}, Workspace: &fakeWorkspaceBackend{}, AgentBackends: map[string]AgentBackend{"audit": backend}, SecurityAuditRoot: auditRoot, Store: sessionStore, ReapWorkerProcess: func(_ context.Context, identity procgroup.Identity, _ time.Duration) (procgroup.TerminationOutcome, error) {
					request := backend.requests[len(backend.requests)-1]
					if mode != "missing process" && identity.PID != 4242 {
						t.Fatalf("reaped the wrong audit worker: %+v", identity)
					}
					if _, err := os.Stat(request.TempDir); err != nil {
						t.Fatalf("scratch removed before worker reap: %v", err)
					}
					if mode == "process reap failure" && len(backend.requests) > 1 {
						return procgroup.TerminationOutcomeTerminated, reapFailure
					}
					return procgroup.TerminationOutcomeTerminated, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				runner.reapArtifactProcesses = func(_ context.Context, root, path string, _ time.Duration) (int, error) {
					request := backend.requests[len(backend.requests)-1]
					if root != auditRoot || path != request.Workspace {
						t.Fatalf("audit artifact ownership changed: root=%q path=%q", root, path)
					}
					if _, err := os.Stat(request.TempDir); err != nil {
						t.Fatalf("scratch removed before artifact reap: %v", err)
					}
					if mode == "artifact reap failure" && len(backend.requests) > 1 {
						return 0, reapFailure
					}
					return 0, nil
				}
				snapshot := securityaudit.Snapshot{ProjectID: "project", IssueID: "issue", Repository: "owner/repository", BaseSHA: "base", HeadSHA: "head", Diff: "diff --git a/a.go b/a.go\n@@ -0,0 +1 @@\n+caller\n" + strings.Repeat("+record\n", 80000) + "diff --git a/b.go b/b.go\n@@ -0,0 +1 @@\n+unchecked helper\n"}
				var usage []int64
				usageExecution := &recordingUsageExecution{}
				budgetErr := errors.New("usage budget exhausted")
				execution, err := runner.Audit(ctx, SecurityAuditRequest{Issue: connector.Issue{ID: "issue"}, Snapshot: snapshot, usageExecution: usageExecution, OnUsageUpdate: func(update UsageUpdate) error {
					if update.Tokens.TotalTokens > 0 {
						usage = append(usage, update.Tokens.TotalTokens)
					}
					if mode == "usage budget" && update.Tokens.TotalTokens >= 20 {
						return budgetErr
					}
					return nil
				}})
				wantError := !strings.HasPrefix(mode, "chunked verification") && mode != "pass" && mode != "early finding" && mode != "late finding" && mode != "cumulative usage" && mode != "reported cost"
				if (err != nil) != wantError {
					t.Fatalf("audit error = %v", err)
				}
				wantSessions := 1
				if strings.HasPrefix(mode, "chunked verification") {
					wantSessions = 2
				}
				if sessionStore.startCalls != wantSessions || sessionStore.finishCalls != wantSessions {
					t.Fatalf("audit did not retain one session: start=%d finish=%d", sessionStore.startCalls, sessionStore.finishCalls)
				}
				if wantError {
					if execution.Result.Verdict != "" || sessionStore.finished.FinalState == FinalStateCompleted {
						t.Fatalf("incomplete audit emitted success: %+v", execution.Result)
					}
					if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("lost overall audit timeout: %v", err)
					}
					if mode == "usage budget" && !errors.Is(err, budgetErr) {
						t.Fatalf("lost budget owner refusal: %v", err)
					}
					if mode == "process reap failure" || mode == "artifact reap failure" {
						if !errors.Is(err, ErrWorkerProcessReap) || !errors.Is(err, reapFailure) || len(sessionStore.reaps) != 1 {
							t.Fatalf("lost chunk reap failure: error=%v reaps=%d", err, len(sessionStore.reaps))
						}
						request := backend.requests[len(backend.requests)-1]
						if _, err := os.Stat(request.TempDir); err != nil {
							t.Fatalf("failed chunk lost worker scratch: %v", err)
						}
					}
					return
				}
				if backend.covered.String() != snapshot.Diff || sessionStore.finished.Turns != int64(len(backend.requests)-backend.sequenceStart) || execution.SessionID != 73 || execution.InvocationID == "" || execution.ProviderThreadID != "audit-thread" {
					t.Fatal("aggregate lost complete source or runtime attribution")
				}
				if sessionStore.finished.TotalTokens != int64(10*(len(backend.requests)-backend.sequenceStart)) {
					t.Fatalf("usage was reset across chunks: %d", sessionStore.finished.TotalTokens)
				}
				if mode == "reported cost" {
					var input, output, cost, reports int64
					for _, entry := range usageExecution.entries {
						input += entry.Input
						output += entry.Output
						if entry.ReportedCostMicros != nil {
							cost += *entry.ReportedCostMicros
							reports++
						}
					}
					turns := int64(len(backend.requests))
					if input != 8*turns || output != 2*turns || cost != 1000*turns*(turns+1)/2 || reports != turns {
						t.Fatalf("audit usage input=%d output=%d cost=%d cost reports=%d turns=%d", input, output, cost, reports, turns)
					}
				}
				if len(sessionStore.reaps) != len(backend.requests) {
					t.Fatalf("audit workers were not reaped per chunk: %d", len(sessionStore.reaps))
				}
				for _, request := range backend.requests {
					if _, err := os.Stat(request.TempDir); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("completed chunk retained worker scratch: %v", err)
					}
				}
				for index := 1; index < len(usage); index++ {
					if usage[index] < usage[index-1] {
						t.Fatal("usage owner observed decreasing consumption")
					}
				}
				if strings.HasPrefix(mode, "chunked verification") {
					if backend.initialCoverage != snapshot.Diff || execution.Verification == nil || len(execution.Result.Findings) != 1 {
						t.Fatal("chunked verification lost full source or verdicts")
					}
					finding := execution.Result.Findings[0]
					if strings.HasPrefix(mode, "chunked verification") && strings.Contains(mode, "confirmation") {
						if finding.Certainty != securityaudit.CertaintyConfirmed || finding.Severity != "p1" || finding.Status != "unresolved" {
							t.Fatalf("later evidence did not confirm lead: %+v", finding)
						}
					} else if finding.Certainty != securityaudit.CertaintyDisproved || finding.Status != "rejected" || finding.Body != "Caller enforces authorization" {
						t.Fatalf("later evidence did not reject lead: %+v", finding)
					}
				}
				wantFail := mode == "early finding" || mode == "late finding" || strings.HasPrefix(mode, "chunked verification") && strings.Contains(mode, "confirmation")
				if (execution.Result.Verdict == securityaudit.VerdictFail) != wantFail || wantFail && len(execution.Result.Findings) != 1 {
					t.Fatalf("aggregate suppressed findings: %+v", execution.Result)
				}
			}
			if mode == "timeout" {
				synctest.Test(t, run)
			} else {
				run(t)
			}
		})
	}
}
