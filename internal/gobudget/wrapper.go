package gobudget

import (
	"context"
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
)

var errSlotBusy = errors.New("go build budget slot busy")

const (
	minSlotWait = 10 * time.Millisecond
	maxSlotWait = 250 * time.Millisecond
)

func IsWrapperInvocation(arg0 string) bool {
	return strings.TrimSuffix(filepath.Base(arg0), ".exe") == WrapperName
}

func RunWrapper(args []string, getenv func(string) string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "%s: missing tool command\n", WrapperName)
		return 2
	}
	release := releaseNothing
	if gated(args) {
		release = acquire(getenv(DirEnvironment), getenv(SlotsEnvironment), time.Sleep)
	}
	defer func() {
		if err := release(); err != nil {
			fmt.Fprintf(stderr, "%s: release slot: %v\n", WrapperName, err)
		}
	}()

	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...) // #nosec G204 -- the go command supplies the tool path and arguments via -toolexec.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", WrapperName, err)
		return 1
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		for sig := range signals {
			if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				fmt.Fprintf(stderr, "%s: forward %v: %v\n", WrapperName, sig, err)
			}
		}
	}()
	return exitCode(cmd.Wait())
}

func gated(args []string) bool {
	switch strings.TrimSuffix(filepath.Base(args[0]), ".exe") {
	case "compile", "link", "asm", "cgo", "vet":
	default:
		return false
	}
	for _, arg := range args[1:] {
		if arg == "-V" || strings.HasPrefix(arg, "-V=") {
			return false
		}
	}
	return true
}

func releaseNothing() error { return nil }

func acquire(dir string, slotsValue string, sleep func(time.Duration)) func() error {
	slots, err := strconv.Atoi(strings.TrimSpace(slotsValue))
	if strings.TrimSpace(dir) == "" || err != nil || slots <= 0 {
		return releaseNothing
	}
	first := os.Getpid() % slots
	wait := minSlotWait
	for {
		busy := false
		for offset := range slots {
			release, err := tryLock(slotPath(dir, (first+offset)%slots))
			if err == nil {
				return release
			}
			if errors.Is(err, errSlotBusy) {
				busy = true
			}
		}
		if !busy {
			return releaseNothing
		}
		sleep(wait)
		wait = min(wait*2, maxSlotWait)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		return exitErr.ExitCode()
	}
	return 1
}
