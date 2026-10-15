package update

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func CommandPreflight(args ...string) BinaryPreflight {
	commandArgs := append([]string(nil), args...)
	return func(ctx context.Context, path string) error {
		cmd := exec.CommandContext(ctx, path, commandArgs...) // #nosec G204 -- the verified candidate path is executed directly without a shell.
		output, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			return fmt.Errorf("run candidate startup preflight: %w", err)
		}
		return fmt.Errorf("run candidate startup preflight: %w: %s", err, detail)
	}
}
