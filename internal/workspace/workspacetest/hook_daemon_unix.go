//go:build darwin || linux

package workspacetest

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
	"golang.org/x/sys/unix"
)

func RunDetachedHook(t *testing.T) {
	control := os.Getenv("TEST_HOOK_CONTROL")
	if control == "" {
		return
	}
	if os.Getenv("TEST_HOOK_CHILD") == "1" {
		if os.Getenv("TEST_HOOK_IGNORE_TERM") == "1" {
			signal.Ignore(syscall.SIGTERM)
		}
		lock, err := os.OpenFile(control+".lock", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		cwd := os.Getenv("TEST_HOOK_CWD")
		if cwd == "" {
			cwd = "/"
		}
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
		in, err := os.Open(control)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		pid := os.Getpid()
		start, err := procstart.Identity(pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(control+".pid.tmp", []byte(strconv.Itoa(pid)+"\n"+start), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(control+".pid.tmp", control+".pid"); err != nil {
			t.Fatal(err)
		}
		ready := os.NewFile(3, "ready")
		if _, err := ready.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		ready.Close()
		dropDescriptor := os.Getenv("TEST_HOOK_DROP_DESCRIPTOR") == "1"
		var request [1]byte
		for {
			_, err := io.ReadFull(in, request[:])
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if dropDescriptor {
				if err := syscall.Close(4); err != nil {
					t.Fatal(err)
				}
				dropDescriptor = false
			}
			if err := os.WriteFile(control+".reply", []byte("served\n"+os.Getenv("DETENT_WORKER_SCRATCH")), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestDetachedHookHelper$")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: os.Getenv("TEST_HOOK_KEEP_GROUP") != "1"}
	child.Env = append(os.Environ(), "TEST_HOOK_CHILD=1", "GOCOVERDIR="+os.Getenv("TEST_HOOK_CHILD_COVER"))
	child.ExtraFiles = []*os.File{writer}
	if os.Getenv("TEST_HOOK_DROP_DESCRIPTOR") == "1" {
		var stat unix.Stat_t
		flags, err := unix.FcntlInt(4, unix.F_GETFL, 0)
		if err != nil || unix.Fstat(4, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO || flags&unix.O_ACCMODE != unix.O_WRONLY {
			t.Fatal("fixture did not inherit the hook writer")
		}
		lifetime := os.NewFile(4, "hook writer")
		defer lifetime.Close()
		child.ExtraFiles = append(child.ExtraFiles, lifetime)
	}
	if err := procgroup.Start(child); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if _, err := io.ReadFull(reader, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Release(); err != nil {
		t.Fatal(err)
	}
}

type Daemon struct {
	Command string
	control string
}

func DetachedHook(t *testing.T) Daemon {
	t.Helper()
	control := filepath.Join(t.TempDir(), "control")
	if err := syscall.Mkfifo(control, 0600); err != nil {
		t.Fatal(err)
	}
	quit, err := os.OpenFile(control, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		quit.Close()
		lock, err := os.Open(control + ".lock")
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Error(err)
			return
		}
		pid, start, err := daemonIdentity(control)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Error(err)
			return
		}
		testenv.WaitUntil(t, context.WithoutCancel(t.Context()), func() error {
			alive, err := procstart.Matches(pid, start)
			if alive {
				return errors.New("waiting for detached fixture exit")
			}
			return err
		})
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "GOCOVERDIR=" + shellQuote(t.TempDir()) + " TEST_HOOK_CHILD_COVER=" + shellQuote(t.TempDir()) + " TEST_HOOK_CONTROL=" + shellQuote(control) + " " + shellQuote(executable) + " -test.run='^TestDetachedHookHelper$'"
	return Daemon{Command: command, control: control}
}

func daemonIdentity(control string) (int, string, error) {
	data, err := os.ReadFile(control + ".pid")
	if err != nil {
		return 0, "", err
	}
	rawPID, start, ok := strings.Cut(string(data), "\n")
	if !ok || start == "" {
		return 0, "", errors.New("missing detached fixture start identity")
	}
	pid, err := strconv.Atoi(rawPID)
	return pid, start, err
}

func (d Daemon) Alive(t *testing.T) bool {
	t.Helper()
	lock, err := os.Open(d.control + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal(err)
	}
	return err != nil
}

func (d Daemon) Request(t *testing.T) string {
	t.Helper()
	if !d.Alive(t) {
		t.Fatal("service exited before receiving work")
	}
	input, err := os.OpenFile(d.control, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var scratch string
	testenv.WaitUntil(t, t.Context(), func() error {
		data, err := os.ReadFile(d.control + ".reply")
		var served bool
		scratch, served = strings.CutPrefix(string(data), "served\n")
		if err == nil && !served {
			return errors.New("awaiting service response")
		}
		return err
	})
	return scratch
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func UseDaemonCandidates(t *testing.T, daemons ...Daemon) {
	t.Helper()
	previous := hookproc.ReapProcessScanner
	t.Cleanup(func() { hookproc.ReapProcessScanner = previous })
	hookproc.ReapProcessScanner = func(context.Context, string) ([]int, error) {
		var candidates []int
		for _, d := range daemons {
			pid, _, err := daemonIdentity(d.control)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if d.Alive(t) {
				candidates = append(candidates, pid)
			}
		}
		return candidates, nil
	}
}
