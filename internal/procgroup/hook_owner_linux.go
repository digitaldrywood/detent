package procgroup

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const hookOwnerArgument = "--detent-procgroup-hook-owner"

type hookCommand struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

func init() {
	if len(os.Args) != 3 || os.Args[1] != hookOwnerArgument {
		return
	}
	code := 125
	var err error
	switch os.Args[2] {
	case "launch":
		code, err = launchHookOwner()
	case "own":
		err = runHookOwner()
		if err == nil {
			code = 0
		}
	default:
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hook process owner:", err)
	}
	os.Exit(code)
}

func runHookOwner() error {
	for fd := 3; fd <= 6; fd++ {
		unix.CloseOnExec(fd)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("enable hook subreaper: %w", err)
	}
	probe, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf("check hook process handles: %w", err)
	}
	err = unix.PidfdSendSignal(probe, 0, nil, 0)
	_ = unix.Close(probe)
	if err != nil {
		return fmt.Errorf("check hook process signals: %w", err)
	}
	configuration := os.NewFile(3, "configuration")
	control := os.NewFile(4, "control")
	completion := os.NewFile(5, "completion")
	result := os.NewFile(6, "result")
	defer completion.Close()
	defer result.Close()
	var request hookCommand
	err = json.NewDecoder(configuration).Decode(&request)
	_ = configuration.Close()
	if err != nil {
		return fmt.Errorf("read hook command: %w", err)
	}
	if len(request.Args) == 0 || request.Path == "" {
		return errors.New("hook command is missing")
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	detach := make(chan bool, 1)
	go func() {
		var value [1]byte
		_, err := io.ReadFull(control, value[:])
		detach <- err == nil && value[0] == 1
	}()
	defer func() {
		_ = control.Close()
		if detach != nil {
			<-detach
		}
	}()
	cmd := &exec.Cmd{Path: request.Path, Args: request.Args, Env: request.Env, Dir: request.Dir,
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if err := Start(cmd); err != nil {
		_ = json.NewEncoder(result).Encode(127)
		return fmt.Errorf("start hook command: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- Wait(cmd) }()
	defer func() {
		if wait != nil {
			_ = cmd.Process.Kill()
			<-wait
		}
	}()
	finished := false
	stopping := false
	killing := false
	var deadline <-chan time.Time
	var timer *time.Timer
	var cleanupErr error
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if stopping {
			sig := unix.SIGTERM
			if killing {
				sig = unix.SIGKILL
			}
			cleanupErr = signalHookChildren(sig)
		}
		if finished {
			empty, err := reapHookChildren()
			if err != nil {
				cleanupErr = err
			}
			if empty && cleanupErr == nil && (detach == nil || stopping) {
				return nil
			}
		}
		select {
		case err := <-wait:
			finished = true
			wait = nil
			code := 0
			if err != nil {
				code = 125
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					code = exit.ExitCode()
				}
			}
			_ = json.NewEncoder(result).Encode(code)
			_ = result.Close()
			_ = os.Stdin.Close()
			_ = os.Stdout.Close()
			_ = os.Stderr.Close()
		case detached := <-detach:
			detach = nil
			if !detached {
				stopping = true
			}
		case received := <-signals:
			if received != syscall.SIGCHLD {
				stopping = true
			}
		case <-deadline:
			killing = true
			deadline = nil
		}
		if stopping && timer == nil {
			timer = time.NewTimer(DefaultTerminationGrace)
			deadline = timer.C
		}
	}
}

func reapHookChildren() (bool, error) {
	for {
		pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
		if errors.Is(err, unix.ECHILD) {
			return true, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || pid == 0 {
			return false, err
		}
	}
}

func signalHookChildren(sig unix.Signal) error {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return fmt.Errorf("list hook owner threads: %w", err)
	}
	for _, task := range tasks {
		children, err := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("list hook children: %w", err)
		}
		for _, value := range strings.Fields(string(children)) {
			pid, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("parse hook child: %w", err)
			}
			if err := signalHookChild(pid, sig); err != nil {
				return err
			}
		}
	}
	return nil
}
