package cli

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
func checkDoctorInvariants(ctx context.Context, id, root string, run func(context.Context, string) error) []doctorCheck {
	name := "Project " + id + " invariants"
	if _, err := os.Stat(filepath.Join(root, "invariants", "policy.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []doctorCheck{{Name: name, Status: doctorFail, Detail: err.Error()}}
	}
	check := doctorCheck{Name: name, Status: doctorOK, Detail: "registered invariant behaviors passed"}
	if err := run(ctx, root); err != nil {
		check.Status = doctorFail
		check.Detail = fmt.Sprintf("invariant behavior verification failed: %v", err)
		check.Hint = "Run go run ./tools/invariantcheck in the source repository for diagnostics."
	}
	return []doctorCheck{check}
}

func runDoctorInvariants(ctx context.Context, root string) error {
	cmd := exec.CommandContext(ctx, "go", "run", "./tools/invariantcheck")
	procgroup.Configure(ctx, cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = root
	cmd.Env = doctorCommandEnvironment(os.Environ(), []string{"DETENT_API_TOKEN="})
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}
