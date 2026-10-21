package procstart

import (
	"errors"
	"fmt"
	"strings"
)

var ErrNotRunning = errors.New("process is not running")

func Identity(pid int) (string, error) {
	if pid <= 0 {
		return "", ErrNotRunning
	}
	return identity(pid)
}

func Matches(pid int, recorded string) (bool, error) {
	recorded = strings.TrimSpace(recorded)
	if pid <= 0 || recorded == "" {
		return false, nil
	}
	current, err := Identity(pid)
	if errors.Is(err, ErrNotRunning) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect process %d start identity: %w", pid, err)
	}
	return current == recorded, nil
}
