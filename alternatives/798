package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type runnerCheckoutError struct {
	message string
	summary string
	fix     string
	cause   error
}

func (e *runnerCheckoutError) Error() string { return e.message }

func (e *runnerCheckoutError) Unwrap() error { return e.cause }

func (e *runnerCheckoutError) guidance() string {
	value := firstNonBlankString(e.summary, e.message)
	value = runnerauth.SanitizeProblem(runnerauth.Problem{Message: value}).Message
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 1000 {
		value = strings.ToValidUTF8(value[:997], "") + "..."
	}
	return value
}

type runnerCheckoutOutput struct {
	data      [8192]byte
	size      int
	truncated bool
}

func (b *runnerCheckoutOutput) Write(p []byte) (int, error) {
	n := copy(b.data[b.size:], p)
	b.size += n
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}

func (b *runnerCheckoutOutput) String() string {
	output := string(b.data[:b.size])
	if b.truncated {
		return "[stderr truncated]\n" + output
	}
	return output
}

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
	return cloneRunnerRepositoryWithCommandContext(ctx, cloneURL, staging, context.WithTimeout)
}

func cloneRunnerRepositoryWithCommandContext(ctx context.Context, cloneURL, staging string, commandContext runtimeCommandContextFactory) error {
	cloneCtx, cancel := commandContext(ctx, 2*time.Minute)
	defer cancel()
	var diagnostics []string
	run := func(remote string, args []string, ssh bool) error {
		args = append(args, "clone", "--filter=blob:none", "--", remote, staging)
		cmd := exec.CommandContext(cloneCtx, "git")
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
		if ssh {
			cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+firstNonBlankString(os.Getenv("GIT_SSH_COMMAND"), "ssh")+" -o BatchMode=yes")
		}
		procgroup.Configure(cloneCtx, cmd)
		cmd.WaitDelay = time.Second
		var stderr runnerCheckoutOutput
		cmd.Stderr = &stderr
		err := cmd.Run()
		if stderr.size > 0 {
			diagnostics = append(diagnostics, stderr.String())
		}
		return err
	}
	args := []string{}
	if _, err := exec.LookPath("gh"); err == nil {
		args = append(args, "-c", "credential.https://github.com.helper=!gh auth git-credential")
	}
	cloneErr := run(cloneURL, args, false)
	if cloneErr == nil {
		return nil
	}
	parsed, err := url.Parse(cloneURL)
	if err == nil && cloneCtx.Err() == nil {
		entries, err := os.ReadDir(staging)
		if err == nil && len(entries) == 0 {
			sshErr := run("git@github.com:"+strings.TrimPrefix(parsed.Path, "/"), nil, true)
			if sshErr == nil {
				return nil
			}
			cloneErr = errors.Join(cloneErr, sshErr)
		}
	}
	return newRunnerCheckoutError(cloneURL, cloneErr, cloneCtx.Err(), strings.Join(diagnostics, "\n"))
}

var runnerCheckoutURLCredentials = regexp.MustCompile(`https?://[^\s/@]+@`)

func newRunnerCheckoutError(cloneURL string, cause, contextErr error, stderr string) *runnerCheckoutError {
	cause = errors.Join(cause, contextErr)
	output := runnerauth.SanitizeProblem(runnerauth.Problem{ErrorOutput: runnerCheckoutURLCredentials.ReplaceAllString(stderr, "https://[redacted]@")}).ErrorOutput
	message := "Cannot clone " + cloneURL + ": " + cause.Error()
	summary := message
	if output != "" {
		message += ": " + output
	}
	fix := "Check repository connectivity and the reported git error, then let the runner retry"
	reason := "git checkout failed"
	diagnostic := strings.ToLower(stderr)
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		reason = "checkout timed out"
		fix = "Repository checkout timed out; check repository size, network throughput and git server availability"
	case errors.Is(cause, context.Canceled):
		reason = "checkout canceled"
		fix = "Repository checkout was canceled; let the runner retry if the project is still assigned"
	case strings.Contains(diagnostic, "repository not found"), strings.Contains(diagnostic, "not found"):
		reason = "repository not found"
		fix = "Verify the repository binding exists and the runner has access to it"
	case strings.Contains(diagnostic, "authentication failed"), strings.Contains(diagnostic, "could not read username"), strings.Contains(diagnostic, "permission denied (publickey)"), strings.Contains(diagnostic, "invalid username or token"), strings.Contains(diagnostic, "http 401"), strings.Contains(diagnostic, "error: 401"):
		reason = "authentication failed"
		fix = "gh auth login --hostname github.com --git-protocol https"
	}
	return &runnerCheckoutError{message: message, summary: reason + ": " + summary, fix: fix, cause: cause}
}
