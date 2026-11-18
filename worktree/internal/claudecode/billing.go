package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (b *AgentBackend) BillingBasis(ctx context.Context, process runner.AgentProcessRequest) (string, error) {
	ctx, cancel := context.WithTimeout(contextOrBackground(ctx), 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if b.options.CommandFactoryWithArgs != nil {
		cmd = b.options.CommandFactoryWithArgs(ctx, []string{"auth", "status"})
	} else {
		cmd = b.options.CommandFactory(ctx)
		if cmd != nil {
			cmd.Args = append(cmd.Args, "auth", "status")
		}
	}
	if cmd == nil {
		return "unknown", ErrNilCommand
	}
	cmd.Dir = process.Workspace
	procgroup.SetEnvironment(cmd, process.Environment)
	procgroup.SetTempDir(cmd, process.TempDir)
	procgroup.Configure(ctx, cmd)
	output := newTailBuffer(16 * 1024)
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return "unknown", errors.New("claude authentication observation unavailable")
	}
	var status struct {
		LoggedIn     bool   `json:"loggedIn"`
		AuthMethod   string `json:"authMethod"`
		APIKeySource string `json:"apiKeySource"`
	}
	if err := json.Unmarshal([]byte(output.String()), &status); err != nil {
		return "unknown", errors.New("claude authentication observation invalid")
	}
	if !status.LoggedIn {
		return "unknown", nil
	}
	return claudeBilling(claudeEvent{APIKeySource: status.APIKeySource, AuthSource: status.AuthMethod}), nil
}
