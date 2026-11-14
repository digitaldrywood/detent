package nativesigning_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/internal/gitfixture"
)

func TestLocalGitNativeWorkDisablesTrackedHooks(t *testing.T) {
	if address := os.Getenv("DETENT_WORKSPACE_STALLED_SIGNER"); address != "" {
		connection, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if _, err := io.Copy(io.Discard, connection); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, test := range []struct {
		name          string
		signed        bool
		rework        bool
		noIdentity    bool
		gpg           string
		signerLost    bool
		lossAt        string
		requireSigned bool
		indexLocked   bool
		gpgsign       string
		sshDefaultKey bool
		paused        bool
		landingMethod string
	}{
		{name: "unsigned Code"},
		{name: "signed Code", signed: true},
		{name: "signed Rework", signed: true, rework: true},
		{name: "signed SSH default key command", signed: true, sshDefaultKey: true},
		{name: "no identity or GPG", noIdentity: true},
		{name: "working OpenPGP", signed: true, gpg: "working"},
		{name: "OpenPGP unavailable after startup finalization", signed: true, gpg: "warm", signerLost: true},
		{name: "OpenPGP unavailable after startup checkpoint", signed: true, gpg: "warm", signerLost: true, lossAt: "checkpoint"},
		{name: "OpenPGP unavailable after startup paused rework", signed: true, rework: true, paused: true, gpg: "warm", signerLost: true},
		{name: "required signing refuses unsigned landing", signed: true, gpg: "warm", signerLost: true, requireSigned: true},
		{name: "index storage failure retains signed policy", signed: true, gpg: "working", indexLocked: true},
		{name: "working OpenPGP uppercase boolean", signed: true, gpg: "working", gpgsign: "TRUE"},
		{name: "working OpenPGP mixed-case boolean", signed: true, gpg: "working", gpgsign: "Yes"},
		{name: "unusable OpenPGP key", gpg: "missing"},
		{name: "locked OpenPGP key never prompts", gpg: "locked"},
		{name: "unresponsive signer is bounded", gpg: "stalled"},
		{name: "signed paused Rework", signed: true, rework: true, gpg: "working", paused: true},
		{name: "unusable key paused Rework", rework: true, gpg: "missing", paused: true},
		{name: "no identity paused Rework", rework: true, noIdentity: true, paused: true},
		{name: "signed merge landing", signed: true, gpg: "working", landingMethod: "merge"},
		{name: "signed replay landing", signed: true, gpg: "working", landingMethod: "rebase"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home)
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GNUPGHOME", filepath.Join(home, "gnupg"))
			source := gitfixture.SourceRepo(t)
			remote := gitfixture.BareRemote(t)
			gitfixture.Git(t, source, "remote", "add", "origin", remote)
			gitfixture.Git(t, source, "push", "-u", "origin", "main")
			backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := workspace.Issue{Identifier: "native#156", NativeRework: test.rework, ProgressBaseRef: "main"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			hooks := []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit"}
			markers := t.TempDir()
			if err := os.Mkdir(filepath.Join(info.Path, ".githooks"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, hook := range hooks {
				script := fmt.Sprintf("#!/bin/sh\nprintf 'executed\\n' >> %q\n", filepath.Join(markers, hook))
				if err := os.WriteFile(filepath.Join(info.Path, ".githooks", hook), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			gitfixture.Git(t, source, "config", "core.hooksPath", ".githooks")
			if test.paused {
				gitfixture.WriteFile(t, info.Path, "README.md", "feature conflict\n")
				gitfixture.Git(t, info.Path, "add", "README.md")
			}
			gitfixture.Git(t, info.Path, "add", ".githooks")
			gitfixture.Git(t, info.Path, "commit", "-m", "test: install tracked hooks")
			for _, hook := range hooks {
				if gitfixture.ReadFile(t, filepath.Join(markers, hook)) != "executed\n" {
					t.Fatalf("hook fixture did not execute %s", hook)
				}
				if err := os.Remove(filepath.Join(markers, hook)); err != nil {
					t.Fatal(err)
				}
			}
			gitfixture.Git(t, source, "config", "user.name", "Native Author")
			gitfixture.Git(t, source, "config", "user.email", "native@example.com")
			if test.signed && test.gpg == "" {
				signer := filepath.Join(t.TempDir(), "signing-key")
				if output, err := exec.CommandContext(t.Context(), "ssh-keygen", "-t", "ed25519", "-N", "", "-f", signer).CombinedOutput(); err != nil {
					t.Fatalf("create signing fixture: %v: %s", err, output)
				}
				public, err := os.ReadFile(signer + ".pub")
				if err != nil {
					t.Fatal(err)
				}
				allowed := filepath.Join(t.TempDir(), "allowed-signers")
				if err := os.WriteFile(allowed, append([]byte("native@example.com "), public...), 0o600); err != nil {
					t.Fatal(err)
				}
				gitfixture.Git(t, source, "config", "commit.gpgsign", "true")
				gitfixture.Git(t, source, "config", "gpg.format", "ssh")
				gitfixture.Git(t, source, "config", "user.signingkey", signer)
				gitfixture.Git(t, source, "config", "gpg.ssh.allowedSignersFile", allowed)
				if test.sshDefaultKey {
					gitfixture.Git(t, source, "config", "--unset", "user.signingkey")
					keyCommand := filepath.Join(filepath.Dir(signer), "default-key")
					if err := os.WriteFile(keyCommand, []byte("#!/bin/sh\nprintf 'key::'\ncat "+shellQuote(signer+".pub")+"\n"), 0o700); err != nil {
						t.Fatal(err)
					}
					gitfixture.Git(t, source, "config", "gpg.ssh.defaultKeyCommand", shellQuote(keyCommand))
					program := filepath.Join(filepath.Dir(signer), "agent-signer")
					script := "#!/bin/sh\ncd " + shellQuote(filepath.Dir(signer)) + "\nexec ssh-agent -a \"signing-agent-$$\" sh -c 'ssh-add \"$1\" && shift && exec \"$@\"' sh " + shellQuote(signer) + " ssh-keygen \"$@\"\n"
					if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
						t.Fatal(err)
					}
					gitfixture.Git(t, source, "config", "gpg.ssh.program", program)
					head := strings.TrimSpace(gitfixture.Git(t, source, "commit-tree", "-S", "HEAD^{tree}", "-m", "test: verify SSH default key signing"))
					gitfixture.Git(t, source, "verify-commit", head)
				}
			}
			if test.noIdentity {
				gitfixture.Git(t, source, "config", "--unset", "user.name")
				gitfixture.Git(t, source, "config", "--unset", "user.email")
				gitfixture.Git(t, source, "config", "gpg.program", filepath.Join(home, "unavailable-gpg"))
			}
			pinentry := filepath.Join(home, "pinentry-called")
			var signerReady net.Listener
			if test.gpg != "" {
				signerReady = configureGPGCommitFixture(t, source, test.gpg, pinentry)
			}
			if test.gpgsign != "" {
				gitfixture.Git(t, source, "config", "commit.gpgsign", test.gpgsign)
			}
			options := workspace.LocalGitOptions{Root: filepath.Dir(info.Path), SourceRoot: source, AutoBranch: true, CommitIdentity: workspace.CommitIdentity{Name: "Enrolled runner", Email: "machine_fixture@runners.detent.invalid"}}
			if signerReady == nil {
				backend, err = workspace.NewBackend(workspace.KindLocalGit, options)
			} else {
				release := testenv.WaitContext(t, t.Context())
				if deadline, ok := release.Deadline(); ok {
					if err := signerReady.(*net.TCPListener).SetDeadline(deadline); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				var connection net.Conn
				t.Cleanup(func() {
					cancel()
					_ = signerReady.Close()
					if connection != nil {
						_ = connection.Close()
					}
					<-done
				})
				go func() {
					backend, err = workspace.NewBackendContext(ctx, workspace.KindLocalGit, options)
					_ = signerReady.Close()
					close(done)
				}()
				var readyErr error
				connection, readyErr = signerReady.Accept()
				if readyErr != nil {
					t.Fatalf("signer did not reach readiness: %v", readyErr)
				}
				if deadline, ok := release.Deadline(); ok {
					if err := connection.SetReadDeadline(deadline); err != nil {
						t.Fatal(err)
					}
				}
				cancel()
				select {
				case <-done:
				case <-release.Done():
					_ = connection.Close()
					<-done
					t.Fatal("constructor did not return after caller cancellation")
				}
				if n, readErr := connection.Read(make([]byte, 1)); n != 0 || readErr != io.EOF {
					t.Fatalf("cancelled signer did not exit: bytes=%d error=%v", n, readErr)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			owner := backend.(*workspace.LocalGit)
			report := owner.CommitReport()
			if (report.Signing != "unsigned") != test.signed {
				t.Fatalf("signing report = %+v", report)
			}
			if got := owner.Problems(); (len(got) > 0) == test.signed {
				t.Fatalf("signing problems = %+v", got)
			}
			loseSigner := func() {
				proof := strings.TrimSpace(gitfixture.Git(t, info.Path, "commit-tree", "-S", "HEAD^{tree}", "-p", "HEAD", "-m", "test: startup signing"))
				assertRunnerCommitPolicy(t, info.Path, proof, "Native Author <native@example.com>", true)
				cmd := exec.CommandContext(t.Context(), "gpgconf", "--kill", "gpg-agent")
				cmd.Dir = os.Getenv("GNUPGHOME")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("make isolated signer unavailable: %v: %s", err, output)
				}
				test.signed = false
			}
			if test.signerLost && test.lossAt == "" {
				loseSigner()
			}
			configPath := filepath.Join(source, ".git", "config")
			configBefore := gitfixture.ReadFile(t, configPath)
			if test.paused {
				gitfixture.WriteFile(t, source, "README.md", "upstream conflict\n")
				gitfixture.Git(t, source, "add", "README.md")
				gitfixture.Git(t, source, "-c", "user.name=Upstream", "-c", "user.email=upstream@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "advance upstream")
				gitfixture.Git(t, source, "push", "origin", "main")
				prepared, err := owner.PrepareRework(t.Context(), info, issue, workspace.MergePrepareOptions{TargetBranch: "main"}, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || prepared.Status != workspace.MergePrepareStatusConflict {
					t.Fatalf("prepare paused rework: %+v, %v", prepared, err)
				}
				checkpoint, changed, err := owner.PushCheckpointRef(t.Context(), info, issue, workspace.CheckpointRef{}, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || !changed {
					t.Fatalf("paused rework checkpoint: changed=%v error=%v", changed, err)
				}
				identity := "Native Author <native@example.com>"
				if test.noIdentity {
					identity = "Enrolled runner <machine_fixture@runners.detent.invalid>"
				}
				assertRunnerCommitPolicy(t, info.Path, checkpoint.CommitSHA, identity, test.signed)
				gitfixture.WriteFile(t, info.Path, "README.md", "resolved conflict\n")
				gitfixture.Git(t, info.Path, "add", "README.md")
			}
			before := strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(info.Path, "repair.md"), []byte("repair\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitfixture.Git(t, info.Path, "add", "repair.md")
			intendedTree := strings.TrimSpace(gitfixture.Git(t, info.Path, "write-tree"))
			indexPath := strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "--git-path", "index"))
			indexBefore := gitfixture.ReadFile(t, indexPath)
			if test.indexLocked {
				lock := strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "--git-path", "index.lock"))
				if err := os.WriteFile(lock, []byte("retained lock\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := owner.FinalizeNativeWork(t.Context(), info, issue, func(ctx context.Context) error { return ctx.Err() }); err != nil {
				if !test.indexLocked {
					t.Fatalf("finalize native work: %v: %s", err, gitexec.ErrorOutput(err))
				}
				if strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "HEAD")) != before || gitfixture.ReadFile(t, indexPath) != indexBefore || owner.CommitReport().Signing != "openpgp" || len(owner.Problems()) != 0 {
					t.Fatal("storage failure changed source or downgraded usable signing")
				}
				return
			} else if test.indexLocked {
				t.Fatal("finalization succeeded despite the index lock")
			}
			if !test.paused {
				for _, hook := range hooks {
					if _, err := os.Stat(filepath.Join(markers, hook)); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("host finalization executed %s: %v", hook, err)
					}
				}
			}
			head := strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "HEAD"))
			if head == before || strings.TrimSpace(gitfixture.Git(t, info.Path, "status", "--porcelain")) != "" {
				t.Fatal("finalization did not commit the staged repair")
			}
			if test.signed {
				gitfixture.Git(t, info.Path, "verify-commit", head)
			}
			expected := "Native Author <native@example.com>"
			if test.noIdentity {
				expected = "Enrolled runner <machine_fixture@runners.detent.invalid>"
			}
			author := expected
			if test.paused {
				author = "Test User <test@example.com>"
			}
			if got := strings.TrimSpace(gitfixture.Git(t, info.Path, "show", "-s", "--format=%an <%ae>", head)); got != author {
				t.Fatalf("finalization changed author: %s", got)
			}
			assertRunnerCommitPolicy(t, info.Path, head, expected, test.signed)
			if test.signerLost && !test.paused && (strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", head+"^{tree}")) != intendedTree || strings.TrimSpace(gitfixture.Git(t, info.Path, "show", "-s", "--format=%P", head)) != before) {
				t.Fatal("signer loss changed finalization tree or parent")
			}
			if test.signerLost && test.lossAt == "checkpoint" {
				loseSigner()
			}
			if test.paused {
				if gitfixture.ReadFile(t, configPath) != configBefore {
					t.Fatal("paused rework changed host Git configuration")
				}
				if _, err := os.Stat(pinentry); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("paused rework invoked pinentry: %v", err)
				}
				return
			}
			owned := func(ctx context.Context) error { return ctx.Err() }
			gitfixture.WriteFile(t, info.Path, "checkpoint.md", "checkpoint work\n")
			if test.signerLost {
				gitfixture.Git(t, info.Path, "add", "checkpoint.md")
				intendedTree = strings.TrimSpace(gitfixture.Git(t, info.Path, "write-tree"))
			}
			ref, changed, err := owner.PushCheckpointRef(t.Context(), info, issue, workspace.CheckpointRef{}, owned)
			if err != nil || !changed {
				t.Fatalf("checkpoint ref: changed=%v error=%v", changed, err)
			}
			assertRunnerCommitPolicy(t, info.Path, ref.CommitSHA, expected, test.signed)
			if test.signerLost && (ref.TreeSHA != intendedTree || ref.HeadSHA != head || strings.TrimSpace(gitfixture.Git(t, info.Path, "show", "-s", "--format=%P", ref.CommitSHA)) != head || owner.CommitReport().Signing != "unsigned" || len(owner.Problems()) == 0) {
				t.Fatal("signer loss changed checkpoint identity or omitted signing problem")
			}
			plan, err := owner.PrepareCheckpoint(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := owner.Checkpoint(t.Context(), plan, workspace.CheckpointSelection{HeadSHA: head, Paths: []string{"checkpoint.md", "repair.md", "README.md", ".githooks/pre-commit", ".githooks/prepare-commit-msg", ".githooks/commit-msg", ".githooks/post-commit"}, Reviewed: true}, owned, procgroup.Environment{Variables: map[string]string{"GIT_COMMITTER_NAME": "inherited worker", "GIT_COMMITTER_EMAIL": "inherited@example.com", "GNUPGHOME": filepath.Join(home, "unavailable-worker-keys"), "HOME": filepath.Join(home, "worker-home")}})
			if err != nil {
				t.Fatal(err)
			}
			assertRunnerCommitPolicy(t, info.Path, checkpoint, expected, test.signed)
			if test.signerLost && (strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", checkpoint+"^{tree}")) != intendedTree || strings.TrimSpace(gitfixture.Git(t, info.Path, "show", "-s", "--format=%P", checkpoint)) != head) {
				t.Fatal("signer loss changed checkpoint tree or parent")
			}
			if test.requireSigned {
				hook := "#!/bin/sh\nwhile read old new ref; do\n git verify-commit \"$new\" || exit 1\ndone\n"
				if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			method := test.landingMethod
			if method == "" {
				method = "squash"
			}
			result, err := owner.LandChange(t.Context(), info, issue, workspace.LandOptions{HeadSHA: checkpoint, Method: method, Message: "Land fixture", TargetBranch: "main", Authorize: owned})
			if test.requireSigned {
				var refusal *workspace.LandRefusal
				if !errors.As(err, &refusal) || strings.TrimSpace(gitfixture.Git(t, remote, "rev-parse", "main")) != strings.TrimSpace(gitfixture.Git(t, source, "rev-parse", "main")) || strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "HEAD")) != checkpoint {
					t.Fatalf("required signing did not refuse landing with retained source: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRunnerCommitPolicy(t, source, result.MergeSHA, expected, test.signed)
			if _, err := os.Stat(pinentry); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("signing invoked pinentry: %v", err)
			}
			if test.signerLost && test.lossAt == "checkpoint" {
				warmGPGCommitFixture(t, os.Getenv("GNUPGHOME"))
				gitfixture.WriteFile(t, info.Path, "restored.md", "restored signing\n")
				gitfixture.Git(t, info.Path, "add", "restored.md")
				if _, err := owner.FinalizeNativeWork(t.Context(), info, issue, owned); err != nil {
					t.Fatal(err)
				}
				restored := strings.TrimSpace(gitfixture.Git(t, info.Path, "rev-parse", "HEAD"))
				assertRunnerCommitPolicy(t, info.Path, restored, expected, true)
				if owner.CommitReport().Signing != "openpgp" || len(owner.Problems()) != 0 {
					t.Fatal("restored usable signer remained downgraded")
				}
			}
			if got := strings.TrimSpace(gitfixture.Git(t, source, "config", "core.hooksPath")); got != ".githooks" || gitfixture.ReadFile(t, configPath) != configBefore {
				t.Fatal("finalization changed repository hooks or signing configuration")
			}
		})
	}
}

func assertRunnerCommitPolicy(t *testing.T, path, head, identity string, signed bool) {
	t.Helper()
	if got := strings.TrimSpace(gitfixture.Git(t, path, "show", "-s", "--format=%cn <%ce>", head)); got != identity {
		t.Fatalf("committer = %q, want %q", got, identity)
	}
	commit := gitfixture.Git(t, path, "cat-file", "commit", head)
	if strings.Contains(commit, "\ngpgsig ") != signed {
		t.Fatalf("commit signature present = %v, want %v", strings.Contains(commit, "\ngpgsig "), signed)
	}
	if signed {
		gitfixture.Git(t, path, "verify-commit", head)
	}
}

func configureGPGCommitFixture(t *testing.T, source, mode, pinentry string) net.Listener {
	t.Helper()
	program, err := exec.LookPath("gpg")
	if err != nil {
		t.Fatal("GPG is required for the commit signing regression")
	}
	home := os.Getenv("GNUPGHOME")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"S.gpg-agent", "S.gpg-agent.extra", "S.gpg-agent.browser", "S.gpg-agent.ssh"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte("%Assuan%\nsocket="+name+".socket\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prompt := filepath.Join(home, "pinentry")
	if err := os.WriteFile(prompt, []byte("#!/bin/sh\nprintf called > "+shellQuote(pinentry)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "gpg-agent.conf"), []byte("pinentry-program "+prompt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signer := filepath.Join(home, "signer")
	script := "#!/bin/sh\ncd " + shellQuote(home) + "\nexec " + shellQuote(program) + " \"$@\"\n"
	if err := os.WriteFile(signer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "gpgconf", "--kill", "gpg-agent")
		cmd.Dir = home
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("stop isolated signing agent: %v: %s", err, output)
		}
	}
	var signerReady net.Listener
	if mode == "stalled" {
		signerReady, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = signerReady.Close() })
		script := "#!/bin/sh\nGOCOVERDIR=" + shellQuote(t.TempDir()) + " DETENT_WORKSPACE_STALLED_SIGNER=" + shellQuote(signerReady.Addr().String()) + " exec " + shellQuote(os.Args[0]) + " -test.run='^TestLocalGitNativeWorkDisablesTrackedHooks$'\n"
		if err := os.WriteFile(signer, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "working" || mode == "locked" || mode == "warm" {
		passphrase := ""
		if mode == "locked" || mode == "warm" {
			passphrase = "fixture-passphrase"
		}
		cmd := exec.CommandContext(t.Context(), program, "--batch", "--pinentry-mode", "loopback", "--passphrase", passphrase, "--quick-generate-key", "Native Author <native@example.com>", "ed25519", "sign", "0")
		cmd.Dir = home
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate isolated signing key: %v: %s", err, output)
		}
		stop()
	}
	if mode == "warm" {
		warmGPGCommitFixture(t, home)
	}
	t.Cleanup(stop)
	gitfixture.Git(t, source, "config", "commit.gpgsign", "true")
	gitfixture.Git(t, source, "config", "gpg.format", "openpgp")
	gitfixture.Git(t, source, "config", "gpg.program", signer)
	key := "native@example.com"
	if mode == "missing" {
		key = "unavailable@example.com"
	}
	gitfixture.Git(t, source, "config", "user.signingkey", key)
	return signerReady
}

func warmGPGCommitFixture(t *testing.T, home string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(home, "signer"), "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase", "fixture-passphrase", "--detach-sign", "--output", filepath.Join(home, "warm-signature"))
	cmd.Dir = home
	cmd.Stdin = strings.NewReader("warm isolated signer\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("warm isolated signer: %v: %s", err, output)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
