package spritesigning

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

type LocalIdentity struct {
	Directory string
	Config    string
}

func (l LocalIdentity) PublicKey(_ context.Context) (string, error) {
	if !filepath.IsAbs(l.Directory) || !filepath.IsAbs(l.Config) {
		return "", ErrOwnership
	}
	if err := os.MkdirAll(l.Directory, 0700); err != nil {
		return "", fmt.Errorf("create signing directory: %w", err)
	}
	directoryInfo, err := os.Lstat(l.Directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode().Perm()&0077 != 0 {
		return "", ErrOwnership
	}
	path := filepath.Join(l.Directory, "signing_ed25519")
	info, err := os.Lstat(path)
	var raw []byte
	if errors.Is(err, os.ErrNotExist) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return "", fmt.Errorf("generate signing key: %w", err)
		}
		block, err := ssh.MarshalPrivateKey(private, "")
		if err != nil {
			return "", fmt.Errorf("encode signing key: %w", err)
		}
		raw = pem.EncodeToMemory(block)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return "", fmt.Errorf("reserve signing key file: %w", err)
		}
		_, writeErr := file.Write(raw)
		syncErr := file.Sync()
		if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
			return "", fmt.Errorf("persist signing key: %w", err)
		}
	} else {
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return "", ErrOwnership
		}
		raw, err = os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read member signing key: %w", err)
		}
	}
	signer, err := ssh.ParsePrivateKey(raw)
	clear(raw)
	if err != nil {
		return "", errors.New("member signing key is invalid")
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return "", ErrOwnership
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

func (l LocalIdentity) ConfigureAndVerify(ctx context.Context, account Account) error {
	if strings.ContainsAny(account.Name+account.Email, "\r\n\x00<>") || account.Name == "" || account.Email == "" {
		return ErrOwnership
	}
	public, err := l.PublicKey(ctx)
	if err != nil {
		return err
	}
	key := filepath.Join(l.Directory, "signing_ed25519")
	current, err := localCommand(ctx, "", "git", "config", "--file", l.Config, "--get", "user.signingkey")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return fmt.Errorf("read configured signing identity: %w", err)
		}
	} else if strings.TrimSpace(current) != key {
		return ErrOwnership
	}
	repo, err := os.MkdirTemp("", "detent-signing-verify-*")
	if err != nil {
		return fmt.Errorf("create signing verification repository: %w", err)
	}
	defer os.RemoveAll(repo)
	allowed := filepath.Join(repo, "allowed_signers")
	if err := os.WriteFile(allowed, []byte("* "+public+"\n"), 0600); err != nil {
		return fmt.Errorf("write signing verifier: %w", err)
	}
	if _, err := localCommand(ctx, repo, "git", "init", "--quiet"); err != nil {
		return fmt.Errorf("initialize signing verification: %w", err)
	}
	config := []string{
		"user.name", account.Name,
		"user.email", account.Email,
		"gpg.format", "ssh",
		"gpg.ssh.program", "ssh-keygen",
		"user.signingkey", key,
		"commit.gpgsign", "true",
	}
	args := make([]string, 0, len(config)+10)
	for i := 0; i < len(config); i += 2 {
		args = append(args, "-c", config[i]+"="+config[i+1])
	}
	args = append(args, "-c", "core.hooksPath="+repo, "-c", "gpg.ssh.allowedSignersFile="+allowed)
	if _, err := localCommand(ctx, repo, "git", append(append([]string{}, args...), "commit", "--quiet", "--allow-empty", "-m", "Verify member signing")...); err != nil {
		return fmt.Errorf("sign verification commit: %w", err)
	}
	if _, err := localCommand(ctx, repo, "git", append(append([]string{}, args...), "verify-commit", "HEAD")...); err != nil {
		return fmt.Errorf("verify signed commit: %w", err)
	}
	for i := 0; i < len(config); i += 2 {
		if _, err := localCommand(ctx, "", "git", "config", "--file", l.Config, config[i], config[i+1]); err != nil {
			return fmt.Errorf("configure member signing: %w", err)
		}
	}
	return nil
}

func localCommand(ctx context.Context, directory, program string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = directory
	cmd.Stdin = nil
	procgroup.Configure(ctx, cmd)
	output, err := cmd.Output()
	return string(output), err
}
