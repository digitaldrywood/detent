package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

// Repositories opt in by carrying the behavioral manifest and checker.
func CheckInvariants(ctx context.Context, id, root string, run func(context.Context, string) error) []Check {
	name := "Project " + id + " invariants"
	if _, err := os.Stat(filepath.Join(root, "invariants", "policy.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []Check{{Name: name, Status: Fail, Detail: err.Error()}}
	}
	check := Check{Name: name, Status: OK, Detail: "registered invariant behaviors passed"}
	if err := run(ctx, root); err != nil {
		check.Status = Fail
		check.Detail = fmt.Sprintf("invariant behavior verification failed: %v", err)
		check.Hint = "Run go run ./tools/invariantcheck in the source repository for diagnostics."
	}
	return []Check{check}
}

func RunInvariants(ctx context.Context, root string) error {
	cmd := exec.CommandContext(ctx, "go", "run", "./tools/invariantcheck")
	procgroup.Configure(ctx, cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = root
	cmd.Env = CommandEnvironment(os.Environ(), []string{"DETENT_API_TOKEN="})
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}
