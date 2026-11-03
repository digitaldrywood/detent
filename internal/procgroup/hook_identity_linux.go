package procgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect hook child identity: %w", err)
	}
	fields, err := hookProcessFields(stat)
	if err != nil {
		return err
	}
	if fields[1] != strconv.Itoa(os.Getpid()) {
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
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("read hook owner start: %w", err)
	}
	startFields, err := hookProcessFields(started)
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
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil || !bytes.Equal(arguments, current) {
		return true, err
	}
	identity, err := os.ReadFile(filepath.Join(root, "stat"))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("read hook owner identity: %w", err)
	}
	currentFields, err := hookProcessFields(identity)
	if err != nil {
		return true, err
	}
	if startFields[19] != currentFields[19] {
		return true, nil
	}
	cwd, err := os.Readlink(filepath.Join(root, "cwd"))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return true, nil
	}
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

func hookProcessFields(stat []byte) ([]string, error) {
	end := strings.LastIndex(string(stat), ") ")
	if end < 0 {
		return nil, errors.New("invalid hook process identity")
	}
	fields := strings.Fields(string(stat[end+2:]))
	if len(fields) < 20 {
		return nil, errors.New("incomplete hook process identity")
	}
	return fields, nil
}
