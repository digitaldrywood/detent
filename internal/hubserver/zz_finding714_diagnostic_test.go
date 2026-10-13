package hubserver

import (
	"context"
	"encoding/json"
	"github.com/digitaldrywood/detent/internal/codex"
	"github.com/digitaldrywood/detent/internal/runner"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type findingEvidenceBackend struct {
	backend *codex.AgentBackend
	mu      sync.Mutex
	file    *os.File
}

func (b *findingEvidenceBackend) record(v any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := json.NewEncoder(b.file).Encode(v); err != nil {
		panic(err)
	}
}
func (b *findingEvidenceBackend) RunTurn(ctx context.Context, r runner.AgentTurnRequest, u runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return b.backend.RunTurn(ctx, r, u)
}
func (b *findingEvidenceBackend) RunTurnWithTools(ctx context.Context, r runner.AgentTurnRequest, ts []runner.AgentTool, h runner.AgentToolHandler, u runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.record(map[string]any{"kind": "request", "at": time.Now().UTC(), "model": r.Model, "effort": r.ReasoningEffort, "read_only": r.ReadOnly, "prompt": r.Prompt, "tools": ts})
	v, err := b.backend.RunTurnWithTools(ctx, r, ts, func(ctx context.Context, c runner.AgentToolCall) (runner.AgentToolResult, error) {
		v, err := h(ctx, c)
		b.record(map[string]any{"kind": "tool", "at": time.Now().UTC(), "call": c, "result": v, "error": err})
		return v, err
	}, func(v runner.AgentUpdate) error {
		b.record(map[string]any{"kind": "update", "at": time.Now().UTC(), "update": v})
		return u(v)
	})
	b.record(map[string]any{"kind": "result", "at": time.Now().UTC(), "result": v, "error": err})
	return v, err
}
func TestFinding714RealCoordinatorPreview(t *testing.T) {
	scratch := filepath.Join(os.Getenv("TMPDIR"), "issue714")
	if scratch == "" {
		t.Fatal("scratch required")
	}
	ws := filepath.Join(scratch, "workspace")
	home := filepath.Join(scratch, "home")
	profile := filepath.Join(scratch, "codex")
	temp := filepath.Join(scratch, "temp")
	for _, p := range []string{ws, home, profile, temp} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	credHome := os.Getenv("CODEX_HOME")
	if credHome == "" {
		v, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		credHome = filepath.Join(v, ".codex")
	}
	cred := filepath.Join(credHome, "auth.json")
	if _, err := os.Stat(cred); err == nil {
		if err := os.Symlink(cred, filepath.Join(profile, "auth.json")); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "CODEX_HOME=" + profile, "TMPDIR=" + temp, "TMP=" + temp, "TEMP=" + temp}
	for _, n := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORGANIZATION", "OPENAI_PROJECT", "CODEX_API_KEY"} {
		if v, ok := os.LookupEnv(n); ok {
			env = append(env, n+"="+v)
		}
	}
	flags := []string{"app-server"}
	for _, v := range []string{"shell_tool", "unified_exec", "shell_snapshot", "view_image", "code_mode", "code_mode_host", "apps", "plugins", "browser_use", "computer_use", "image_generation", "multi_agent", "multi_agent_v2", "goals", "tool_suggest", "skill_search", "hooks", "memories", "sleep_tool"} {
		flags = append(flags, "-c", "features."+v+"=false")
	}
	flags = append(flags, "-c", "web_search=disabled")
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(t.Context(), binary, "features", "list")
	probe.Env = env
	out, err := probe.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"shell_tool", "unified_exec", "view_image", "apps", "plugins", "browser_use", "computer_use"} {
		if !strings.Contains(string(out), v+"\t") && !strings.Contains(string(out), v+" ") {
			t.Fatalf("feature missing: %s", v)
		}
	}
	factory, err := codex.NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, flags...)
		cmd.Env = env
		cmd.Dir = ws
		return cmd
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := codex.NewAppServer(factory)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := codex.NewAgentBackend(client, codex.Options{ApprovalPolicy: "never", ThreadSandbox: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(scratch, "model.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	wrapped := &findingEvidenceBackend{backend: backend, file: file}
	f := newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", func(c *Config) {
		c.Conversation = &ConversationConfig{Enabled: true, Backend: wrapped, Workspace: ws, Model: "gpt-6.1-sol", ReasoningEffort: "high"}
	})
	f.service.stopHealthDetector()
	finding := newHealthFinding("dead_man", "flow", "project", f.project, "No work has landed for thirty minutes despite candidates and a free slot.", "The project operator checks scheduler admission and landing evidence.", []string{f.project}, healthEvidence{Counts: map[string]int{"candidates": 1, "free_slots": 1}})
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeHealthEvaluation(t.Context(), tx, "org_browser_preview", time.Now().UTC(), []healthFinding{finding}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"url": f.server.URL, "owner": f.server.URL + "/__preview/account/owner", "viewer": f.server.URL + "/__preview/account/viewer", "project": f.project, "private_project": f.privateProject, "stop": f.server.URL + "/__preview/stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "preview.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Ready: %s", f.server.URL)
	select {
	case <-f.stop:
	case <-time.After(15 * time.Minute):
	case <-t.Context().Done():
	}
}
