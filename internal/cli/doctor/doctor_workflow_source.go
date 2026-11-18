package doctor

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

func NormalizedWorkflowPolicy(raw []byte) []byte {
	return bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
}

func WorkflowSourceGit(ctx context.Context, sourceRoot string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := gitexec.Command(commandCtx, sourceRoot, args...)
	output, err := cmd.CombinedOutput()
	if commandCtx.Err() != nil {
		return "", commandCtx.Err()
	}
	if err != nil {
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}
