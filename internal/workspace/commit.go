package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const signingProbeTimeout = 5 * time.Second

const commitSigningFailureMarker = "detent: commit signing failed"

type CommitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type CommitReport struct {
	Identity CommitIdentity `json:"identity"`
	Signing  string         `json:"signing"`
}

type commitPolicy struct {
	report      CommitReport
	config      []string
	program     string
	format      string
	environment []string
	unavailable *atomic.Bool
}

func probeCommitPolicy(ctx context.Context, source string, fallback CommitIdentity) (_ commitPolicy, err error) {
	ctx, cancel := context.WithTimeout(ctx, signingProbeTimeout)
	defer cancel()
	output, err := runGitAt(ctx, source, "config", "--null", "--list")
	if err != nil {
		return commitPolicy{}, fmt.Errorf("read runner commit configuration: %w", err)
	}
	values := make(map[string]string)
	for _, entry := range strings.Split(output, "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		values[key] = value
	}
	read := func(key string) string { return strings.TrimSpace(values[strings.ToLower(key)]) }
	identity := CommitIdentity{Name: read("user.name"), Email: read("user.email")}
	if identity.Name == "" || identity.Email == "" {
		identity = fallback
	}
	if identity.Name == "" || identity.Email == "" {
		identity = CommitIdentity{Name: "Detent runner", Email: "runner@detent.invalid"}
	}
	if len(identity.Name)+len(identity.Email) > 500 || strings.ContainsAny(identity.Name+identity.Email, "\r\n\x00<>") {
		return commitPolicy{}, errors.New("runner commit identity is invalid")
	}
	p := commitPolicy{report: CommitReport{Identity: identity, Signing: "unsigned"}, format: read("gpg.format"), unavailable: new(atomic.Bool)}
	if p.format == "" {
		p.format = "openpgp"
	}
	p.program = read("gpg." + p.format + ".program")
	if p.program == "" && p.format == "openpgp" {
		p.program = read("gpg.program")
	}
	if p.program == "" {
		switch p.format {
		case "openpgp":
			p.program = "gpg"
		case "ssh":
			p.program = "ssh-keygen"
		case "x509":
			p.program = "gpgsm"
		default:
			return p, nil
		}
	}
	for _, key := range []string{"HOME", "USERPROFILE", "GNUPGHOME", "XDG_CONFIG_HOME", "SSH_AUTH_SOCK", "GPG_AGENT_INFO"} {
		p.environment = append(p.environment, key+"="+os.Getenv(key))
	}
	p.config = []string{"-c", "gpg.format=" + p.format}
	for _, key := range []string{"user.signingkey", "gpg.ssh.defaultKeyCommand", "gpg.ssh.allowedSignersFile", "gpg.ssh.revocationFile"} {
		if value := read(key); value != "" {
			p.config = append(p.config, "-c", key+"="+value)
		}
	}
	enabled, err := runGitAt(ctx, source, "config", "--bool", "--get", "commit.gpgsign")
	if err != nil || strings.TrimSpace(enabled) != "true" {
		return p, nil
	}
	p.report.Signing = p.format
	if attempted, err := p.verifySigning(ctx); err != nil {
		if !attempted {
			return p, err
		}
		p.report.Signing = "unsigned"
	}
	return p, nil
}

func (p commitPolicy) verifySigning(ctx context.Context) (_ bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, signingProbeTimeout)
	defer cancel()
	scratch, err := changeSourceScratch()
	if err != nil {
		return false, fmt.Errorf("prepare signing probe: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
	if _, err := runGitAt(ctx, scratch, "init", "--quiet"); err != nil {
		return false, fmt.Errorf("initialize signing probe: %w", err)
	}
	file := filepath.Join(scratch, "empty")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		return false, err
	}
	tree, err := runGitAt(ctx, scratch, "hash-object", "-w", "-t", "tree", "--", file)
	if err != nil {
		return false, err
	}
	_, err = p.run(ctx, scratch, nil, "commit-tree", strings.TrimSpace(tree), "-m", "Detent signing probe")
	return true, err
}

func ProbeCommitReport(ctx context.Context, source string, fallback CommitIdentity) (_ CommitReport, err error) {
	if source == "" {
		var scratch string
		scratch, err = changeSourceScratch()
		if err != nil {
			return CommitReport{}, err
		}
		defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
		source = scratch
	}
	policy, err := probeCommitPolicy(ctx, source, fallback)
	return policy.report, err
}

func (l *LocalGit) CommitReport() CommitReport {
	report := l.commits.report
	if l.commits.unavailable != nil && l.commits.unavailable.Load() {
		report.Signing = "unsigned"
	}
	return report
}

func (l *LocalGit) Problems() []runnerauth.Problem {
	return l.CommitReport().Problems()
}

func (r CommitReport) Problems() []runnerauth.Problem {
	if r.Signing != "unsigned" {
		return nil
	}
	p := runnerauth.NewProblem("settings_invalid")
	p.Check = "commit_signing"
	p.Message = "Commit signing unavailable; runner commits are unsigned."
	p.FixHint = "Restore a signing key usable without pinentry; startup-unavailable signing requires a runner restart."
	return []runnerauth.Problem{p}
}

func (l *LocalGit) runCommit(ctx context.Context, path string, env []string, args ...string) (string, error) {
	policy := l.commits
	if policy.report.Signing != "unsigned" {
		if attempted, err := policy.verifySigning(ctx); err != nil {
			if !attempted || ctx.Err() != nil || !signerUnavailable(err) {
				return "", err
			}
			policy.report.Signing = "unsigned"
			if policy.unavailable != nil {
				policy.unavailable.Store(true)
			}
			l.logger.Warn("commit signing unavailable", "signing", "unsigned", "error", commandErrorOutput(err))
		} else if policy.unavailable != nil {
			policy.unavailable.Store(false)
		}
	}
	return policy.run(ctx, path, env, args...)
}

func signerUnavailable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var command *CommandError
	if !errors.As(err, &command) || command.ExitCode <= 0 {
		return false
	}
	for _, line := range strings.Split(command.Output, "\n") {
		switch strings.TrimSpace(line) {
		case "gpg: signing failed: No pinentry", "gpg: signing failed: No secret key", "gpg: signing failed: Bad passphrase", "gpg: signing failed: Inappropriate ioctl for device", "gpg: signing failed: No agent running":
			return true
		}
	}
	return false
}

func (p commitPolicy) run(ctx context.Context, path string, env []string, args ...string) (_ string, err error) {
	identity := p.report.Identity
	env = append(env, p.environment...)
	env = append(env, "GIT_COMMITTER_NAME="+identity.Name, "GIT_COMMITTER_EMAIL="+identity.Email, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
	command := 0
	for command < len(args) && args[command] == "-c" {
		command += 2
	}
	if command >= len(args) {
		return "", errors.New("runner commit command is missing")
	}
	if args[command] == "commit" || args[command] == "commit-tree" {
		env = append(env, "GIT_AUTHOR_NAME="+identity.Name, "GIT_AUTHOR_EMAIL="+identity.Email)
	}
	config := []string{"-c", "user.name=" + identity.Name, "-c", "user.email=" + identity.Email, "-c", "commit.gpgsign=false"}
	signed := p.report.Signing != "unsigned"
	if signed {
		scratch, createErr := changeSourceScratch()
		if createErr != nil {
			return "", createErr
		}
		defer func() { err = errors.Join(err, os.RemoveAll(scratch)) }()
		program := p.program
		if !filepath.IsAbs(program) {
			program, err = exec.LookPath(program)
			if err != nil {
				return "", err
			}
		}
		askpass := filepath.Join(scratch, "askpass")
		if err := os.WriteFile(askpass, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			return "", err
		}
		wrapper := filepath.Join(scratch, "signer")
		script := "#!/bin/sh\nif '" + strings.ReplaceAll(filepath.ToSlash(program), "'", "'\"'\"'") + "'"
		if p.format != "ssh" {
			script += " --batch --no-tty --pinentry-mode error"
		}
		script += " \"$@\"; then\n exit 0\nelse\n result=$?\n printf '\\n" + commitSigningFailureMarker + "\\n' >&2\n exit \"$result\"\nfi\n"
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
			return "", err
		}
		env = append(env, "SSH_ASKPASS_REQUIRE=force", "SSH_ASKPASS="+askpass, "DISPLAY=detent-signing", "GPG_TTY=")
		config = append(config, p.config...)
		config = append(config, "-c", "commit.gpgsign=true", "-c", "gpg."+p.format+".program="+wrapper)
	}
	flag := "--no-gpg-sign"
	if signed {
		flag = "--gpg-sign"
	}
	if args[command] == "commit-tree" {
		if signed {
			flag = "-S"
		} else {
			flag = ""
		}
	}
	if args[command] == "rebase" && slices.Contains(args[command+1:], "--continue") {
		flag = ""
		for _, kind := range []string{"rebase-merge", "rebase-apply"} {
			state, err := gitPathFor(ctx, path, kind+"/gpg_sign_opt")
			if err != nil {
				return "", err
			}
			if signed {
				if _, err := os.Stat(filepath.Dir(state)); errors.Is(err, os.ErrNotExist) {
					continue
				} else if err != nil {
					return "", err
				}
				if err := os.WriteFile(state, []byte("-S"), 0o600); err != nil {
					return "", err
				}
			} else if err := os.Remove(state); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
	}
	invocation := append(config, args[:command+1]...)
	if flag != "" {
		invocation = append(invocation, flag)
	}
	invocation = append(invocation, args[command+1:]...)
	cmd := exec.CommandContext(ctx, "git")
	cmd.Args = append([]string{"git", "-C", path}, invocation...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = workspaceCommandWaitDelay
	procgroup.Configure(ctx, cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return "", &CommandError{Command: "git", Args: cmd.Args[1:], ExitCode: exitCode, Output: string(output), Err: err}
	}
	return string(output), nil
}
