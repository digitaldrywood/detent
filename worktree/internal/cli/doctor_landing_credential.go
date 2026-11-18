package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func checkDoctorLandingCredential(ctx context.Context, id, source string, cfg workflowconfig.Config, deps doctorDeps) (doctorCheck, bool) {
	if deps.gitLandingCredential == nil || cfg.Tracker.Kind != workflowconfig.TrackerHubNative || cfg.Deliverable.GitHubPullRequest || cfg.Workspace.Kind == "filesystem" {
		return doctorCheck{}, false
	}
	check := doctorCheck{Name: "Project " + id + " landing push credential", Status: doctorOK, Detail: "repository push authentication passed a noninteractive dry run; no remote refs changed"}
	root, err := expandDoctorWorkspacePath(source)
	if err == nil {
		err = deps.gitLandingCredential(ctx, root)
	}
	if err != nil {
		check.Status = doctorFail
		check.Detail = runnerauth.SanitizeProblem(runnerauth.Problem{ErrorOutput: err.Error()}).ErrorOutput
		check.Hint = "Configure the runner's host gh login with repository write access for HTTPS, or its SSH key for an SSH remote; worker.github_token grants agent GitHub access."
	}
	return check, true
}

func defaultDoctorLandingCredential(ctx context.Context, source string) error {
	return probeDoctorLandingCredential(ctx, source, func(cmd *exec.Cmd) error { return cmd.Run() })
}

func probeDoctorLandingCredential(ctx context.Context, source string, run func(*exec.Cmd) error) error {
	ctx, cancel := context.WithTimeout(ctx, doctorCheckTimeout)
	defer cancel()
	command := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git")
		cmd.Args = append(cmd.Args, "-C", source)
		cmd.Args = append(cmd.Args, args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GCM_INTERACTIVE=never", "GIT_SSH_COMMAND="+firstNonBlankString(os.Getenv("GIT_SSH_COMMAND"), "ssh")+" -o BatchMode=yes")
		procgroup.Configure(ctx, cmd)
		cmd.WaitDelay = time.Second
		var output runnerCheckoutOutput
		cmd.Stdout, cmd.Stderr = &output, &output
		err := run(cmd)
		if err != nil {
			return "", fmt.Errorf("landing push credential: %w: %s", err, output.String())
		}
		return strings.TrimSpace(output.String()), nil
	}
	remote, err := command("remote", "get-url", "--push", "origin")
	if err != nil {
		return err
	}
	args := []string{}
	parsed, parseErr := url.Parse(remote)
	switch {
	case parseErr == nil && parsed.Scheme == "https" && parsed.Hostname() == "github.com":
		args = append(args, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential")
	case strings.HasPrefix(remote, "git@"), parseErr == nil && parsed.Scheme == "ssh":
	default:
		return errors.New("landing push credential: origin must use GitHub HTTPS or SSH")
	}
	args = append(args, "push", "--dry-run", "--no-verify", "--", "origin", "HEAD:refs/heads/detent/credential-check")
	_, err = command(args...)
	return err
}
