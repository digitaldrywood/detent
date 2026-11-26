package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type readOnlyAuditTestBackend struct {
	requests          []AgentTurnRequest
	tools             [][]AgentTool
	preToolCommentary string
	onTools           func(context.Context, AgentToolHandler) error
}

func (b *readOnlyAuditTestBackend) Name() string { return "audit" }

func (b *readOnlyAuditTestBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	return b.RunTurnWithTools(ctx, req, nil, nil, update)
}

func (b *readOnlyAuditTestBackend) RunTurnWithTools(ctx context.Context, req AgentTurnRequest, tools []AgentTool, handler AgentToolHandler, update AgentUpdateHandler) (AgentTurnResult, error) {
	b.requests = append(b.requests, req)
	b.tools = append(b.tools, tools)
	if b.preToolCommentary != "" {
		if err := update(AgentUpdate{Type: AgentUpdateMessageDelta, TurnID: "turn_audit", ItemID: "msg_commentary", Delta: b.preToolCommentary}); err != nil {
			return AgentTurnResult{}, err
		}
	}
	if err := update(AgentUpdate{Type: AgentUpdateToolStarted}); err != nil {
		return AgentTurnResult{}, err
	}
	if handler != nil && b.onTools != nil {
		if err := b.onTools(ctx, handler); err != nil {
			return AgentTurnResult{}, err
		}
	}
	if err := update(AgentUpdate{Type: AgentUpdateToolCompleted}); err != nil {
		return AgentTurnResult{}, err
	}
	if err := update(AgentUpdate{Type: AgentUpdateMessageDelta, TurnID: "turn_audit", ItemID: "msg_final", Delta: `{"verdict":"pass",`}); err != nil {
		return AgentTurnResult{}, err
	}
	if err := update(AgentUpdate{Type: AgentUpdateMessageDelta, TurnID: "turn_audit", Delta: `"summary":"verified middleware in commit tree","findings":[]}`}); err != nil {
		return AgentTurnResult{}, err
	}
	if err := update(AgentUpdate{Type: AgentUpdateToolStarted, TurnID: "turn_aux", AuxiliaryTurn: true}); err != nil {
		return AgentTurnResult{}, err
	}
	if err := update(AgentUpdate{Type: AgentUpdateMessageDelta, TurnID: "turn_aux", ItemID: "msg_aux", AuxiliaryTurn: true, Delta: "auxiliary commentary must not replace verdict"}); err != nil {
		return AgentTurnResult{}, err
	}
	if err := update(AgentUpdate{Type: AgentUpdateToolCompleted, TurnID: "turn_aux", AuxiliaryTurn: true}); err != nil {
		return AgentTurnResult{}, err
	}
	return AgentTurnResult{ThreadID: "thread_audit", SessionID: "session_audit"}, nil
}

func TestSecurityAuditReadOnlyCommitTreeTools(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-b", "develop")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored_secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "middleware.go"), []byte("package app\nfunc RequireInventoryManage() bool { return true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "long.txt"), []byte("LONG_MATCH_NEEDLE "+strings.Repeat("x", securityAuditMaxSearchBytes+512)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "pkg", "file.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "big.txt"), []byte(strings.Repeat("0123456789abcdef\n", (securityAuditMaxReadBuffer/17)+200)), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".gitignore", "middleware.go", "long.txt", "pkg/file.go", "big.txt")
	runGit("commit", "-m", "add middleware")
	baseSHA := runGit("rev-parse", "HEAD")
	runGit("update-index", "--add", "--cacheinfo", "160000,"+baseSHA+",submod")
	runGit("commit", "-m", "add gitlink")
	headSHA := runGit("rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repo, "untracked_secret.txt"), []byte("UNTRACKED_TOKEN_VALUE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored_secret.txt"), []byte("IGNORED_TOKEN_VALUE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Agents.ModelSelection.Enabled = new(false)
	cfg.Agents.Backends = []config.AgentBackend{{ID: "audit", Kind: config.AgentBackendCodex, Protocol: "app-server", Command: "test-audit"}}
	cfg.Agents.Routes = []config.AgentRoute{{Name: "audit", Role: RoleSecurityAudit, Backend: "audit", Model: "audit-model", Default: true}}
	cfg.Gate.SecurityAudit.Enabled = true

	var checkedTools bool
	backend := &readOnlyAuditTestBackend{
		preToolCommentary: "I will inspect middleware.go at head_sha before rendering the audit verdict.",
		onTools: func(ctx context.Context, handler AgentToolHandler) error {
			checkedTools = true
			res, err := handler(ctx, AgentToolCall{Name: "read_repo_file", Arguments: json.RawMessage(`{"path":"middleware.go"}`)})
			if err != nil || !res.Success || !strings.Contains(res.Content, "RequireInventoryManage") {
				t.Fatalf("read_repo_file(middleware.go) = %+v, %v", res, err)
			}
			for _, bad := range []string{"untracked_secret.txt", "ignored_secret.txt", "../outside.txt", "/etc/passwd", "pkg", "submod"} {
				raw, _ := json.Marshal(map[string]string{"path": bad})
				res, err := handler(ctx, AgentToolCall{Name: "read_repo_file", Arguments: raw})
				if err != nil || res.Success {
					t.Fatalf("read_repo_file(%q) unexpectedly succeeded: %+v, %v", bad, res, err)
				}
			}
			res, err = handler(ctx, AgentToolCall{Name: "read_repo_file", Arguments: json.RawMessage(`{"path":"big.txt","start_line":200000}`)})
			if err != nil || !res.Success || !strings.Contains(res.Content, "read limit") || !strings.Contains(res.Content, "total line count unknown") {
				t.Fatalf("read_repo_file(big.txt past limit) = %+v, %v", res, err)
			}
			res, err = handler(ctx, AgentToolCall{Name: "search_repo", Arguments: json.RawMessage(`{"pattern":"RequireInventoryManage"}`)})
			if err != nil || !res.Success || !strings.Contains(res.Content, "middleware.go:2:") {
				t.Fatalf("search_repo(RequireInventoryManage) = %+v, %v", res, err)
			}
			res, err = handler(ctx, AgentToolCall{Name: "search_repo", Arguments: json.RawMessage(`{"pattern":"UNTRACKED_TOKEN_VALUE"}`)})
			if err != nil || !res.Success || strings.Contains(res.Content, "untracked_secret") {
				t.Fatalf("search_repo leaked untracked file: %+v, %v", res, err)
			}
			res, err = handler(ctx, AgentToolCall{Name: "search_repo", Arguments: json.RawMessage(`{"pattern":"IGNORED_TOKEN_VALUE"}`)})
			if err != nil || !res.Success || strings.Contains(res.Content, "ignored_secret") {
				t.Fatalf("search_repo leaked ignored file: %+v, %v", res, err)
			}
			res, err = handler(ctx, AgentToolCall{Name: "search_repo", Arguments: json.RawMessage(`{"pattern":"LONG_MATCH_NEEDLE"}`)})
			if err != nil || !res.Success || !strings.Contains(res.Content, "long.txt:1:LONG_MATCH_NEEDLE") || !strings.Contains(res.Content, "... (truncated)") {
				t.Fatalf("search_repo on >16KiB line = %+v, %v", res, err)
			}
			return nil
		},
	}
	r, err := NewRunner(Dependencies{
		ProcessSource:     emptyProcessSource,
		ProjectID:         "prj_example",
		Workflow:          config.Workflow{Config: cfg},
		Workspace:         &fakeWorkspaceBackend{},
		AgentBackends:     map[string]AgentBackend{"audit": backend},
		SecurityAuditRoot: t.TempDir(),
		Store:             &fakeSessionStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := securityaudit.Snapshot{
		ProjectID:  "prj_example",
		IssueID:    "wi_example",
		Repository: "owner/repository",
		BaseSHA:    "base",
		HeadSHA:    headSHA,
		Diff:       "diff --git a/handler.go b/handler.go\n+ok\n",
		DiffBytes:  len("diff --git a/handler.go b/handler.go\n+ok\n"),
	}

	exec, err := r.Audit(workspace.WithProcessSource(t.Context(), emptyProcessSource), SecurityAuditRequest{
		Issue:         connector.Issue{ID: "wi_example"},
		Snapshot:      snap,
		WorkspacePath: repo,
	})
	if err != nil {
		t.Fatalf("Audit with checkout error = %v", err)
	}
	if !checkedTools || exec.Result.Verdict != securityaudit.VerdictPass {
		t.Fatalf("checkedTools=%v Verdict=%q, want true and pass", checkedTools, exec.Result.Verdict)
	}
	got := backend.requests[0]
	if got.Workspace == repo || !got.ReadOnly || !got.IsolatedTools || got.ToolBudget == nil {
		t.Fatalf("turn request Workspace=%q ReadOnly=%v IsolatedTools=%v ToolBudget=%v", got.Workspace, got.ReadOnly, got.IsolatedTools, got.ToolBudget)
	}
	if len(backend.tools[0]) != 2 || backend.tools[0][0].Name != "read_repo_file" || backend.tools[0][1].Name != "search_repo" {
		t.Fatalf("tools = %+v", backend.tools[0])
	}

	_, err = r.Audit(workspace.WithProcessSource(t.Context(), emptyProcessSource), SecurityAuditRequest{
		Issue:    connector.Issue{ID: "wi_example"},
		Snapshot: snap,
	})
	if err == nil || !strings.Contains(err.Error(), ErrSecurityAuditToolUse.Error()) {
		t.Fatalf("Audit without checkout error = %v, want %v", err, ErrSecurityAuditToolUse)
	}
	noCheckoutReq := backend.requests[1]
	if !noCheckoutReq.ReadOnly || !noCheckoutReq.IsolatedTools || noCheckoutReq.ToolBudget == nil {
		t.Fatalf("without checkout request ReadOnly=%v IsolatedTools=%v ToolBudget=%v", noCheckoutReq.ReadOnly, noCheckoutReq.IsolatedTools, noCheckoutReq.ToolBudget)
	}
}
