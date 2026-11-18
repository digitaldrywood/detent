package hookproc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
)

func startHookSignalFixture(t *testing.T, path, owner string) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHookSignalKeepsVerifiedInstance$")
	cmd.Env = append(os.Environ(), HookOwnerKey+"="+owner, "DETENT_WORKSPACE="+path, "TEST_HOOK_SIGNAL_CHILD=1", "GOCOVERDIR="+t.TempDir())
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := procgroup.Start(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		input.Close()
		if cmd.ProcessState == nil {
			cmd.Wait()
		}
	})
	reader := bufio.NewReader(output)
	if line, err := reader.ReadString('\n'); line != "ready\n" || err != nil {
		t.Fatalf("fixture readiness = %q, %v", line, err)
	}
	return cmd, input, reader
}

func TestHookSignalKeepsVerifiedInstance(t *testing.T) {
	if os.Getenv("TEST_HOOK_SIGNAL_CHILD") == "1" {
		fmt.Fprintln(os.Stdout, "ready")
		var change [1]byte
		if _, err := io.ReadFull(os.Stdin, change[:]); err == io.EOF {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		t.Fatal(syscall.Exec("/bin/cat", []string{"cat"}, os.Environ()))
	}
	ctx := context.WithValue(t.Context(), hookOwnerKey{}, "fixture")
	path := t.TempDir()
	cmd, input, reader := startHookSignalFixture(t, path, "fixture")
	read := func(want string) {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil || line != want+"\n" {
			t.Fatalf("child response = %q, %v; want %q", line, err, want)
		}
	}
	write := func(text string) {
		t.Helper()
		if _, err := fmt.Fprintln(input, text); err != nil {
			t.Fatal(err)
		}
	}
	pid := cmd.Process.Pid
	substitute := false
	identity := func(pid int) (string, error) {
		start, err := procstart.Identity(pid)
		if err == nil && substitute {
			substitute = false
			if _, err := input.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			write("replacement ready")
			read("replacement ready")
			replacement, err := procstart.Identity(pid)
			if err != nil || replacement != start {
				t.Fatalf("replacement start = %q, %v; want %q", replacement, err, start)
			}
		}
		return start, err
	}
	scan := func(context.Context, string) ([]int, error) { return []int{pid}, nil }
	verified, send := verifiedProcessTargets(ctx, path, scan, identity, processEnvironment, nil, nil)
	if pids, err := verified(ctx, path); err != nil || len(pids) != 1 || pids[0] != pid {
		t.Fatalf("verified targets = %v, %v; want child %d", pids, err, pid)
	}
	substitute = true
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if err := send(pid, sig); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("signal %s after replacement = %v; want ESRCH", sig, err)
		}
		if substitute {
			t.Fatal("signal skipped the final identity check")
		}
		write(sig.String())
		read(sig.String())
	}
}

func TestHookTokenRequiresVerifiedOwnership(t *testing.T) {
	path := t.TempDir()
	ctx, finish := WithHookProcesses(t.Context())
	var registry Registry
	owner, err := registry.Start(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	cmd, input, _ := startHookSignalFixture(t, path, owner.Token())
	pid := cmd.Process.Pid
	scan := ReapProcessScanner
	t.Cleanup(func() { ReapProcessScanner = scan })
	ReapProcessScanner = func(context.Context, string) ([]int, error) { return []int{pid}, nil }
	owner.descriptor.instances.capture = func(int) (auditToken, error) { return auditToken{}, syscall.EPERM }
	var unverified *HookExitUnverifiedError
	if err := owner.CommandFinished(ctx, nil); !errors.As(err, &unverified) || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("hook completion after token denial = %v", err)
	}
	if err := registry.Stop(ctx, path); !errors.As(err, &unverified) || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("removal after token denial = %v", err)
	}
	ReapProcessScanner = func(context.Context, string) ([]int, error) { return nil, nil }
	if err := finish(); !errors.As(err, &unverified) {
		t.Fatalf("retirement lost the service after discovery stopped: %v", err)
	}
	input.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("service was signaled after token denial: %v", err)
	}
	if err := registry.Stop(ctx, path); err != nil {
		t.Fatalf("verified exit: %v", err)
	}
}
