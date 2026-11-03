package procgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func RunHook(ctx context.Context, cmd *exec.Cmd, started func() error) (resultErr error) {
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
	files := []*os.File{configuration, configurationWriter}
	defer func() { resultErr = errors.Join(resultErr, closeHookFiles(files...)) }()
	control, controlWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	files = append(files, control, controlWriter)
	completion, completionWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	files = append(files, completion, completionWriter)
	request := hookCommand{Path: cmd.Path, Args: cmd.Args, Env: cmd.Environ(), Dir: cmd.Dir}
	cmd.Path = executable
	cmd.Args = []string{executable, hookOwnerArgument, "launch"}
	cmd.ExtraFiles = []*os.File{configuration, control, completionWriter}
	if err := Start(cmd); err != nil {
		return err
	}
	closeErr := closeHookFiles(configuration, control, completionWriter)
	if started != nil {
		if err := started(); err != nil {
			closeErr = errors.Join(closeErr, closeHookFiles(configurationWriter, controlWriter))
			return errors.Join(err, closeErr, TerminateTree(cmd, GroupID(cmd)), Wait(cmd), waitHookOwnerCompletion(completion))
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
	err = errors.Join(err, <-encoded, ctx.Err(), closeErr)
	detached := err == nil
	if detached {
		_, detachErr := controlWriter.Write([]byte{1})
		err = errors.Join(err, detachErr)
		detached = detachErr == nil
	}
	err = errors.Join(err, closeHookFiles(controlWriter))
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

func launchHookOwner() (_ int, resultErr error) {
	for fd := 3; fd <= 5; fd++ {
		unix.CloseOnExec(fd)
	}
	result, resultWriter, err := os.Pipe()
	if err != nil {
		return 125, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeHookFiles(result, resultWriter)) }()
	files := []*os.File{os.Stdin, os.Stdout, os.Stderr,
		os.NewFile(3, "configuration"), os.NewFile(4, "control"), os.NewFile(5, "completion"), resultWriter}
	owner := &exec.Cmd{Path: os.Args[0], Args: []string{os.Args[0], hookOwnerArgument, "own"},
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	owner.ExtraFiles = files[3:]
	owner.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := Start(owner); err != nil {
		return 125, err
	}
	if err := closeHookFiles(files[3:]...); err != nil {
		return 125, err
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

func closeHookFiles(files ...*os.File) error {
	var result error
	for _, file := range files {
		if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	return result
}
