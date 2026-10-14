package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

type runnerCheckoutError struct {
	message string
	fix     string
}

func (e *runnerCheckoutError) Error() string { return e.message }

func prepareRunnerCheckout(ctx context.Context, selected globalconfig.Project, cloneURL string) error {
	return prepareRunnerCheckoutWithClone(ctx, selected, cloneURL, cloneRunnerRepository)
}

func prepareRunnerCheckoutWithClone(ctx context.Context, selected globalconfig.Project, cloneURL string, clone func(context.Context, string, string) error) (resultErr error) {
	if _, err := os.Stat(filepath.Join(selected.Workdir, ".git")); err == nil {
		return nil
	}
	empty := false
	if _, err := os.Lstat(selected.Workdir); err == nil {
		entries, readErr := os.ReadDir(selected.Workdir)
		if readErr != nil || len(entries) != 0 {
			return &runnerCheckoutError{message: "Checkout path is already occupied: " + selected.Workdir, fix: "Choose an unused workdir for this project in global.yaml"}
		}
		empty = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect project checkout: %w", err)
	}
	parsed, err := url.Parse(cloneURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(strings.Split(strings.Trim(parsed.Path, "/"), "/")) != 2 {
		return &runnerCheckoutError{message: "Project has no bound repository", fix: "Bind this project's repository in the Hub"}
	}
	parent := filepath.Dir(selected.Workdir)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return fmt.Errorf("create checkout parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".runner-checkout-")
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(staging)) }()
	if err := clone(ctx, cloneURL, staging); err != nil {
		return err
	}
	if empty {
		if err := os.Remove(selected.Workdir); err != nil {
			return fmt.Errorf("checkout path is no longer empty: %w", err)
		}
	}
	if err := os.Rename(staging, selected.Workdir); err != nil {
		return fmt.Errorf("install project checkout without overwriting existing files: %w", err)
	}
	return nil
}

func cloneRunnerRepository(ctx context.Context, cloneURL, staging string) error {
	cloneCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	run := func(remote string, args []string, ssh bool) error {
		args = append(args, "clone", "--", remote, staging)
		cmd := exec.CommandContext(cloneCtx, "git")
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
		if ssh {
			cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+firstNonBlankString(os.Getenv("GIT_SSH_COMMAND"), "ssh")+" -o BatchMode=yes")
		}
		procgroup.Configure(cloneCtx, cmd)
		cmd.WaitDelay = time.Second
		return cmd.Run()
	}
	args := []string{}
	if _, err := exec.LookPath("gh"); err == nil {
		args = append(args, "-c", "credential.https://github.com.helper=!gh auth git-credential")
	}
	if err := run(cloneURL, args, false); err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	parsed, err := url.Parse(cloneURL)
	if err == nil && cloneCtx.Err() == nil {
		entries, err := os.ReadDir(staging)
		if err == nil && len(entries) == 0 {
			if err := run("git@github.com:"+strings.TrimPrefix(parsed.Path, "/"), nil, true); err == nil {
				return nil
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &runnerCheckoutError{message: "Cannot clone " + cloneURL, fix: "gh auth login --hostname github.com --git-protocol https"}
}
