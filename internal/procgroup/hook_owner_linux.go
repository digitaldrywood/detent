package procgroup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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

func RunHook(ctx context.Context, cmd *exec.Cmd, started func() error) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	if len(cmd.ExtraFiles) != 0 {
		return errors.New("hook owner requires private control descriptors")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve hook owner: %w", err)
	}
	configuration, configurationWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer configuration.Close()
	defer configurationWriter.Close()
	control, controlWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer control.Close()
	defer controlWriter.Close()
	completion, completionWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer completion.Close()
	defer completionWriter.Close()
	request := hookCommand{Path: cmd.Path, Args: cmd.Args, Env: cmd.Environ(), Dir: cmd.Dir}
	cmd.Path = executable
	cmd.Args = []string{executable, hookOwnerArgument, "launch"}
	cmd.ExtraFiles = []*os.File{configuration, control, completionWriter}
	if err := Start(cmd); err != nil {
		return err
	}
	_ = configuration.Close()
	_ = control.Close()
	_ = completionWriter.Close()
	if started != nil {
		if err := started(); err != nil {
			_ = configurationWriter.Close()
			_ = controlWriter.Close()
			return errors.Join(err, TerminateTree(cmd, GroupID(cmd)), Wait(cmd), waitHookOwnerCompletion(completion))
		}
	}
	encoded := make(chan error, 1)
	go func() {
		err := json.NewEncoder(configurationWriter).Encode(request)
		encoded <- errors.Join(err, configurationWriter.Close())
	}()
	err = Wait(cmd)
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
		err = nil
	}
	err = errors.Join(err, <-encoded, ctx.Err())
	detached := err == nil
	if detached {
		_, detachErr := controlWriter.Write([]byte{1})
		err = errors.Join(err, detachErr)
		detached = detachErr == nil
	}
	_ = controlWriter.Close()
	if !detached {
		err = errors.Join(err, waitHookOwnerCompletion(completion))
	}
	return err
}

func waitHookOwnerCompletion(completion *os.File) error {
	if err := completion.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Errorf("bound hook owner cleanup: %w", err)
	}
	_, err := io.Copy(io.Discard, completion)
	return err
}

func launchHookOwner() (int, error) {
	result, resultWriter, err := os.Pipe()
	if err != nil {
		return 125, err
	}
	defer result.Close()
	defer resultWriter.Close()
	files := []*os.File{os.Stdin, os.Stdout, os.Stderr,
		os.NewFile(3, "configuration"), os.NewFile(4, "control"), os.NewFile(5, "completion"), resultWriter}
	owner := exec.Command(os.Args[0], hookOwnerArgument, "own")
	owner.Stdin, owner.Stdout, owner.Stderr = os.Stdin, os.Stdout, os.Stderr
	owner.ExtraFiles = files[3:]
	owner.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := Start(owner); err != nil {
		return 125, err
	}
	for _, file := range files[3:] {
		_ = file.Close()
	}
	if err := owner.Process.Release(); err != nil {
		return 125, err
	}
	var code int
	if err := json.NewDecoder(result).Decode(&code); err != nil {
		return 125, fmt.Errorf("read hook exit: %w", err)
	}
	if code < 0 {
		return 125, unix.Kill(os.Getpid(), unix.SIGKILL)
	}
	return code, nil
}

func runHookOwner() error {
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
	cmd := exec.CommandContext(context.Background(), request.Path, request.Args[1:]...)
	cmd.Args, cmd.Env, cmd.Dir = request.Args, request.Env, request.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	Configure(context.Background(), cmd)
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

func signalHookChild(pid int, sig unix.Signal) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open hook child identity: %w", err)
	}
	defer unix.Close(fd)
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect hook child identity: %w", err)
	}
	end := strings.LastIndex(string(stat), ") ")
	if end < 0 {
		return errors.New("invalid hook child identity")
	}
	fields := strings.Fields(string(stat[end+2:]))
	if len(fields) < 2 || fields[1] != strconv.Itoa(os.Getpid()) {
		return nil
	}
	if err := unix.PidfdSendSignal(fd, sig, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("signal hook child: %w", err)
	}
	return nil
}

func TerminateHookOwner(ctx context.Context, pid int, workspace string) (bool, error) {
	root := filepath.Join("/proc", strconv.Itoa(pid))
	arguments, err := os.ReadFile(filepath.Join(root, "cmdline"))
	fields := bytes.Split(arguments, []byte{0})
	if err != nil || len(fields) != 4 || string(fields[1]) != hookOwnerArgument || string(fields[2]) != "own" || len(fields[3]) != 0 {
		return false, nil
	}
	started, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return true, err
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("open hook owner identity: %w", err)
	}
	defer unix.Close(fd)
	current, err := os.ReadFile(filepath.Join(root, "cmdline"))
	if err != nil || !bytes.Equal(arguments, current) {
		return true, err
	}
	identity, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return true, err
	}
	startFields := strings.Fields(string(started[strings.LastIndex(string(started), ") ")+2:]))
	currentFields := strings.Fields(string(identity[strings.LastIndex(string(identity), ") ")+2:]))
	if len(startFields) < 20 || len(currentFields) < 20 || startFields[19] != currentFields[19] {
		return true, nil
	}
	cwd, err := os.Readlink(filepath.Join(root, "cwd"))
	if err != nil {
		return true, err
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return true, err
	}
	if filepath.Clean(cwd) != canonical {
		return true, nil
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return true, fmt.Errorf("stop hook owner: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, 0); err != nil && !errors.Is(err, unix.EINTR) {
			return true, fmt.Errorf("wait hook owner: %w", err)
		}
		if poll[0].Revents&unix.POLLIN != 0 {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, fmt.Errorf("wait hook owner: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
