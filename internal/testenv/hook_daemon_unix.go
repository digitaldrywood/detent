//go:build unix

package testenv

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func HookDaemonCommand(t *testing.T) (string, func() procgroup.Identity) {
	t.Helper()
	root := t.TempDir()
	pidPath := filepath.Join(root, "pid")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	command := "DETENT_HOOK_DAEMON=parent DETENT_HOOK_DAEMON_PID=" + quote(pidPath) + " GOCOVERDIR=" + quote(t.TempDir()) + " " + quote(os.Args[0]) + " -test.run='^TestHookDaemonHelper$'"
	identity := func() procgroup.Identity {
		t.Helper()
		data, err := os.ReadFile(pidPath)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		defer process.Release()
		value, err := procgroup.Inspect(&exec.Cmd{Process: process})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		process, err := os.FindProcess(pid)
		if err == nil {
			_ = process.Kill()
			_ = process.Release()
		}
	})
	return command, identity
}

func RunHookDaemonHelper(t *testing.T) {
	t.Helper()
	switch os.Getenv("DETENT_HOOK_DAEMON") {
	case "parent":
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		child := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestHookDaemonHelper$")
		child.Env = append(os.Environ(), "DETENT_HOOK_DAEMON=child")
		child.Dir = "/"
		child.ExtraFiles = []*os.File{writer}
		procgroup.Configure(t.Context(), child)
		child.SysProcAttr.Setpgid = false
		child.SysProcAttr.Setsid = true
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		_ = writer.Close()
		ready, err := bufio.NewReader(reader).ReadString('\n')
		if err != nil {
			_ = child.Process.Kill()
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("DETENT_HOOK_DAEMON_PID"), []byte(ready), 0o600); err != nil {
			_ = child.Process.Kill()
			t.Fatal(err)
		}
		os.Exit(0)
	case "child":
		stopped := make(chan os.Signal, 1)
		signal.Notify(stopped, syscall.SIGTERM)
		ready := os.NewFile(3, "ready")
		_, _ = fmt.Fprintln(ready, os.Getpid())
		_ = ready.Close()
		for range stopped {
			if os.Getenv("DETENT_HOOK_DAEMON_RESIST") != "1" {
				os.Exit(0)
			}
		}
	}
}
