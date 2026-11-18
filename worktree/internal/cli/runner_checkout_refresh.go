package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func refreshRunnerCheckout(ctx context.Context, selected globalconfig.Project) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	run := func(args ...string) (string, error) {
		if _, err := exec.LookPath("gh"); err == nil {
			args = append([]string{"-c", "credential.https://github.com.helper=!gh auth git-credential"}, args...)
		}
		cmd := exec.CommandContext(ctx, "git")
		cmd.Args = append(cmd.Args, args...)
		cmd.Dir = selected.Workdir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_SSH_COMMAND="+firstNonBlankString(os.Getenv("GIT_SSH_COMMAND"), "ssh")+" -o BatchMode=yes")
		procgroup.Configure(ctx, cmd)
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	status, err := run("status", "--porcelain")
	if err != nil {
		return fmt.Errorf("inspect verification checkout: %w", err)
	}
	if status != "" {
		return nil
	}
	if _, err := run("fetch", "origin"); err != nil {
		return fmt.Errorf("fetch verification checkout: %w", err)
	}
	if _, err := run("rev-parse", "--verify", "@{upstream}"); err != nil {
		return nil
	}
	if _, err := run("merge", "--ff-only", "@{upstream}"); err != nil {
		return fmt.Errorf("fast-forward verification checkout: %w", err)
	}
	return nil
}

func createRunnerStarterFiles(selected globalconfig.Project, repository string) error {
	workflow := filepath.Join(selected.Workdir, "WORKFLOW.md")
	if _, err := os.Stat(workflow); err == nil {
		if _, err := workflowconfig.LoadWorkflow(workflow); err == nil {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	files := []struct{ path, content string }{
		{filepath.Join(filepath.Dir(workflow), "detent.yaml"), "schema: 1\ntracker:\n  kind: hub_native\n  repository: " + repository + "\n"},
		{workflow, "Implement the assigned issue. Follow AGENTS.md and the repository's development instructions.\nRun the project's focused checks before handing off your changes.\n"},
	}
	for _, file := range files {
		out, err := os.OpenFile(file.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, writeErr := out.WriteString(file.content)
		if err := errors.Join(writeErr, out.Close()); err != nil {
			return err
		}
	}
	return nil
}
