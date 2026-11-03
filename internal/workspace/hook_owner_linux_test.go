package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestDetachedHookDaemonTeardown(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}
	for _, teardown := range []string{"reap", "git cleanup", "filesystem cleanup", "before remove", "clean recovery", "quarantine", "failed linked", "failed directory", "remove", "hook failure"} {
		t.Run(teardown, func(t *testing.T) {
			root := t.TempDir()
			useRealProcessScan(t, root)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			local, err := NewLocalGit(LocalGitOptions{Root: root, SourceRoot: initSourceRepo(t), AutoBranch: true, Logger: logger})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "EXAMPLE-1"}
			var info Info
			var filesystem *Filesystem
			if teardown == "filesystem cleanup" || teardown == "failed directory" {
				filesystem, err = NewFilesystem(FilesystemOptions{Root: root, Logger: logger})
				if err != nil {
					t.Fatal(err)
				}
				info, err = filesystem.Create(t.Context(), issue)
			} else {
				info, err = local.Create(t.Context(), issue)
			}
			if err != nil {
				t.Fatal(err)
			}
			command, pidPath := detachedHookFixture(t)
			t.Cleanup(func() {
				data, _ := os.ReadFile(pidPath)
				pid, _ := strconv.Atoi(string(data))
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				_, _ = ReapProcesses(context.Background(), info.Path, procgroup.DefaultTerminationGrace)
			})
			neighbor, neighborPath := detachedHookFixture(t)
			neighborInfo := Info{Path: t.TempDir(), Key: "neighbor"}
			useRealProcessScan(t, neighborInfo.Path)
			t.Cleanup(func() {
				_, _ = ReapProcesses(context.Background(), neighborInfo.Path, procgroup.DefaultTerminationGrace)
			})
			if err := local.executeHook(t.Context(), "before_run", neighbor, neighborInfo, issue); err != nil {
				t.Fatal(err)
			}
			if teardown == "before remove" {
				local.hooks.BeforeRemove = command
			} else {
				if teardown == "hook failure" {
					command += "; exit 23"
				}
				err = local.executeHook(t.Context(), "before_run", command, info, issue)
				if teardown == "hook failure" {
					var hook *HookError
					if !errors.As(err, &hook) || hook.ExitCode != 23 {
						t.Fatalf("hook failure = %v, want exit 23", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			switch teardown {
			case "reap":
				_, err = ReapProcesses(t.Context(), info.Path, procgroup.DefaultTerminationGrace)
			case "git cleanup", "before remove":
				_, err = local.CleanupIssue(t.Context(), issue)
			case "filesystem cleanup":
				_, err = filesystem.CleanupIssue(t.Context(), issue)
			case "clean recovery":
				err = local.removeCleanWorktree(t.Context(), info.Path)
			case "quarantine":
				_, err = local.quarantineWorktree(t.Context(), info.Path)
			case "failed linked", "failed directory":
				_, _, err = local.quarantineFailedWorkspace(t.Context(), info.Path)
			case "remove":
				err = local.removePath(t.Context(), info.Path)
			case "hook failure":
				err = nil
			}
			if err != nil {
				t.Fatal(err)
			}
			if detachedFixtureAlive(t, pidPath) {
				t.Fatal("setsid daemon with empty environment and outside cwd survived teardown")
			}
			if !detachedFixtureAlive(t, neighborPath) {
				t.Fatal("teardown stopped a daemon owned by another workspace")
			}
		})
	}
}

func detachedFixtureAlive(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	state := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if _, err := unix.Poll(state, 0); err != nil {
		t.Fatal(err)
	}
	return state[0].Revents&unix.POLLIN == 0
}

func detachedHookFixture(t *testing.T) (string, string) {
	t.Helper()
	outside := t.TempDir()
	pidPath := filepath.Join(outside, "daemon.pid")
	command := "GOCOVERDIR=" + shellQuote(t.TempDir()) + " " + shellQuote(os.Args[0]) + " -test.run=^TestDetachedHookFixture$ -- parent " +
		shellQuote(outside) + " " + shellQuote(pidPath) + " " + shellQuote(t.TempDir()) + " " + shellQuote(t.TempDir())
	return command, pidPath
}

func TestDetachedHookFixture(t *testing.T) {
	args := os.Args
	if len(args) < 7 || args[len(args)-6] != "--" {
		return
	}
	args = args[len(args)-5:]
	if args[0] == "parent" {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		child := exec.Command(os.Args[0], "-test.run=^TestDetachedHookFixture$", "--", "child", args[1], args[2], args[3], args[4])
		child.Env = []string{"TMPDIR=" + os.TempDir(), "GOCOVERDIR=" + args[4]}
		child.ExtraFiles = []*os.File{writer}
		if err := procgroup.Start(child); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(args[2], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			t.Fatal(err)
		}
		_ = writer.Close()
		var ready [1]byte
		if _, err := io.ReadFull(reader, ready[:]); err != nil {
			_ = child.Process.Kill()
			t.Fatal(err)
		}
		_ = child.Process.Release()
		return
	}
	if _, err := syscall.Setsid(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(args[1]); err != nil {
		t.Fatal(err)
	}
	os.Clearenv()
	signal.Ignore(syscall.SIGTERM)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGUSR1)
	ready := os.NewFile(3, "ready")
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ready.Close()
	<-stop
	fmt.Fprintln(os.Stdout, "released")
}
