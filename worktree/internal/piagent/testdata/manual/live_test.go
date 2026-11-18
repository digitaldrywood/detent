package piagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

func TestRealPiSmoke(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Fatal("Pi CLI is unavailable")
	}
	dir := t.TempDir()
	backend, err := NewAgentBackend(Options{Provider: os.Getenv("DETENT_PI_PROVIDER"), SessionDir: filepath.Join(dir, "sessions"), TurnTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var output string
	result, err := backend.RunTurn(t.Context(), runner.AgentTurnRequest{Workspace: dir, TempDir: dir, Model: os.Getenv("DETENT_PI_MODEL"), Prompt: "Reply with exactly PI_SMOKE_OK. Do not use tools.", MaxTurns: 1}, func(u runner.AgentUpdate) error {
		if u.Type == runner.AgentUpdateMessageDelta {
			output += u.Delta
		}
		return nil
	})
	if err != nil || result.SessionID == "" || !strings.Contains(output, "PI_SMOKE_OK") {
		t.Fatalf("smoke = %#v, %q, %v", result, output, err)
	}
}
