package gotestreceipt

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func Install(dir, executable string) (string, error) {
	if dir == "" || executable == "" {
		return "", errors.New("wrapper directory and executable are required")
	}
	name := WrapperName
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, sum([]byte(executable))[:16], name)
	if strings.ContainsAny(path, " \t\r\n'\"") {
		return "", errors.New("wrapper path cannot be passed through GOFLAGS")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create go test wrapper directory: %w", err)
	}
	if current, err := os.Readlink(path); err == nil && current == executable {
		return path, nil
	}
	staged := path + "." + rand.Text()
	if err := os.Symlink(executable, staged); err != nil {
		return "", fmt.Errorf("link go test wrapper: %w", err)
	}
	if err := os.Rename(staged, path); err != nil {
		return "", errors.Join(fmt.Errorf("install go test wrapper: %w", err), os.Remove(staged))
	}
	return path, nil
}

func Goflags(inherited, wrapper string) string {
	flags := strings.Fields(inherited)
	if !hasFlag(flags, "trimpath") {
		flags = append(flags, "-trimpath")
	}
	if !hasFlag(flags, "exec") {
		flags = append(flags, "-exec="+wrapper)
	}
	return strings.Join(flags, " ")
}

func hasFlag(flags []string, name string) bool {
	for _, flag := range flags {
		flag = strings.TrimLeft(flag, "-")
		if flag == name || strings.HasPrefix(flag, name+"=") {
			return true
		}
	}
	return false
}
